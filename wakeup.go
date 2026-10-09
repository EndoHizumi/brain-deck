package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- スリープ中の PC を起こす（USB のリモートウェイクアップ） ----------
//
// PC がスリープすると、USB のバスがサスペンドになり、UDC（ci_hdrc）の state が "suspended" になる。
// そのあいだに Brain のキーを押したら、UDC の srp に 1 を書く。udc-core の srp_store は usb_gadget_wakeup() を呼び、
// ChipIdea のドライバ（ci_udc_wakeup）がポートにレジュームを送らせる（PORTSC の FPR）。
//
// 起こせるのは、PC が許したときだけ：
//   - ガジェットの構成が、リモートウェイクアップできると知らせている（bmAttributes の 0x20。gadget-setup.sh の USB_WAKEUP）
//   - PC がサスペンドの前に SET_FEATURE(DEVICE_REMOTE_WAKEUP) を送った（Linux なら power/wakeup が enabled）
// 許していなければ、ci_udc_wakeup は何もしない（srp_store は結果を返さない）。そのため、書いたあと state を読み直し、
// 起きなかったことをログに出す。
//
// 押したキーのレポートは、いつものとおり書く（f_hid が USB の転送に積み、PC が起きて読みに来たら届く）。
// 起こすのは、押したとき（キー、マウスのボタンと移動、メディアキー）だけ。離したときや送り直しでは起こさない
// （デーモンを起動し直したときの空のレポートで、PC を起こさないため）。

var (
	udcClass        = "/sys/class/udc" // テストで差し替える
	wakeCheckDelay  = time.Second      // 起こしてから state を読み直すまで
	wakeMinInterval = time.Second      // 続けて起こすときの間隔
)

// Waker は、スリープ中の PC を起こす。nil でもよい（何もしない）。
// last と warned は loop の goroutine だけが使う。
type Waker struct {
	req    chan struct{}
	last   time.Time // 最後に起こそうとしたとき
	warned bool      // 起こせなかったことを、サスペンドのたびに一度だけログに出す
}

func NewWaker() *Waker {
	w := &Waker{req: make(chan struct{}, 1)}
	go w.loop()
	return w
}

// Request は、PC がスリープしていれば起こす。入力の処理を待たせない（sysfs は別の goroutine で読み書きする）。
func (w *Waker) Request() {
	if w == nil {
		return
	}
	select {
	case w.req <- struct{}{}:
	default: // すでに頼んである
	}
}

func (w *Waker) loop() {
	for range w.req {
		w.try()
	}
}

// try は、サスペンド中なら起こし、起きたかを確かめる。
func (w *Waker) try() {
	dir := udcDir()
	if dir == "" {
		return
	}
	if readUDCState(dir) != "suspended" {
		w.warned = false
		return
	}
	if time.Since(w.last) < wakeMinInterval {
		return
	}
	w.last = time.Now()
	if err := os.WriteFile(filepath.Join(dir, "srp"), []byte("1\n"), 0); err != nil {
		log.Printf("wakeup: %v", err)
		return
	}
	vlogf("wakeup: the USB is suspended; asked %s to wake the PC", filepath.Base(dir))
	// PC が起きると state が configured に戻る（レジュームは 20 ms ほど。PC の OS が起きるのはそのあと）
	for time.Since(w.last) < wakeCheckDelay {
		time.Sleep(10 * time.Millisecond)
		if s := readUDCState(dir); s != "suspended" && s != "" {
			log.Printf("wakeup: woke the PC (USB %s after %v)", s, time.Since(w.last).Round(time.Millisecond))
			return
		}
	}
	if !w.warned {
		w.warned = true
		log.Printf("wakeup: the PC did not wake up; the PC has to allow remote wakeup (Linux: power/wakeup of the USB device; README \"スリープ解除\"), and the gadget has to advertise it (USB_WAKEUP in /etc/lefthand/gadget.env)")
	}
}

// udcDir は、UDC の sysfs のディレクトリ。ガジェットが付いている UDC を選ぶ。なければ空。
func udcDir() string {
	if b, err := filepath.Glob(filepath.Join(configfsGadgets, "*", "UDC")); err == nil {
		for _, f := range b {
			if name, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(name)) != "" {
				return filepath.Join(udcClass, strings.TrimSpace(string(name)))
			}
		}
	}
	return ""
}

func readUDCState(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "state"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// USBState は、UDC の状態（configured、suspended、not attached など）。分からなければ空。
func USBState() string {
	if dir := udcDir(); dir != "" {
		return readUDCState(dir)
	}
	return ""
}

// hostWaker は、入力を PC に送るときに呼ぶ（main で作る。テストでは nil）。
var hostWaker *Waker
