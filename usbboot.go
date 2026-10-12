package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 起動のときの USB の準備（フェーズ K6） ----------
//
// lefthand.service は ethernet_gadget.service（gadget-setup.sh で USB の構成を作る）を待たずに起動する。
// 起動したときに /dev/hidg0 がまだなければ、USB の準備中とし、PC から外れているときと同じに動く
// （キーは送れず、送れなかった状態はあとで送り直す。つながりは link.go が UDC の state から決める）。
// /dev/hidg0 ができたら、HID の形を読み直して、何も押していない状態を送る。
//
// 起動のときの gadget-setup.sh は、ガジェットを作ってから最後に 1 回だけ UDC に付ける（付け直さない）ので、
// /dev/hidg0 ができたあとに開いても、開いたまま付け直して使えなくなることはない。
// 準備中は USB の形の切り替えと端末モードを待たせる（switching）。lefthand が gadget-setup.sh を動かす前には、
// ethernet_gadget.service の gadget-setup.sh が終わるのを待つ（同時に動いて configfs を取り合わないように）。

const (
	gadgetUnit     = "ethernet_gadget.service"
	usbBootPoll    = 250 * time.Millisecond // /dev/hidg0 ができたかを見る間隔（stat だけなので軽い）
	usbBootLimit   = 3 * time.Minute        // これを過ぎたら、待つのをやめる（キーボードだけの形のまま）
	gadgetUnitWait = 60 * time.Second       // gadget-setup.sh を動かす前に、ethernet_gadget の終わりを待つ上限
	inputBootWait  = 30 * time.Second       // 入力のデバイスとフレームバッファが現れるのを待つ上限
)

// isCharDevice は、path が文字デバイスか。
func isCharDevice(path string) bool {
	var st syscall.Stat_t
	return syscall.Stat(path, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

// gadgetUnitState は、ethernet_gadget.service の ActiveState（active、activating、inactive、failed など）。分からなければ空。
var gadgetUnitState = func() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", gadgetUnit).Output() // 動いていなければ 0 でない値を返す
	return strings.TrimSpace(string(out))
}

// waitGadgetUnit は、ethernet_gadget.service が gadget-setup.sh を動かしているあいだ（activating）待つ。
// 起動のときに、lefthand が gadget-setup.sh を同時に動かさないため。ふだんは 1 回見るだけ。
func waitGadgetUnit(limit time.Duration) {
	deadline := time.Now().Add(limit)
	logged := false
	for {
		st := gadgetUnitState()
		if st != "activating" && st != "reloading" {
			return
		}
		if !logged {
			log.Printf("usb: waiting for %s to finish setting up the gadget", gadgetUnit)
			logged = true
		}
		if time.Now().After(deadline) {
			log.Printf("usb: %s is still %s after %v; going ahead", gadgetUnit, st, limit)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitGadgetUnitFor は、script が ethernet_gadget.service と同じ gadget-setup.sh のときだけ、waitGadgetUnit で待つ
// （試験のデーモンやテストの差し替えたスクリプトでは待たない）。
func waitGadgetUnitFor(script string) {
	if script == defaultGadgetSetup {
		waitGadgetUnit(gadgetUnitWait)
	}
}

// StartBoot は、起動したときに HID のデバイスがまだなければ、準備中にして、できるのを待つ goroutine を始める。
// 準備中は Switching が true（USB の形の切り替えを断り、端末モードを待たせる）。デバイスがあれば何もしない。
func (u *USBMode) StartBoot(exists func(string) bool, poll, limit time.Duration) bool {
	if exists(u.dev) {
		return false
	}
	u.mu.Lock()
	u.switching = true
	u.preparing = true
	u.mu.Unlock()
	log.Printf("usb: %s is not there yet; running without USB until the gadget is set up", u.dev)
	start := time.Now()
	go func() {
		for !exists(u.dev) {
			if time.Since(start) > limit {
				log.Printf("usb: %s did not appear in %v (keyboard only; check %s)", u.dev, limit, gadgetUnit)
				break
			}
			time.Sleep(poll)
		}
		u.finishBoot(time.Since(start))
	}()
	return true
}

// Preparing は、起動のときの USB の準備を待っているか。
func (u *USBMode) Preparing() bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.preparing
}

// finishBoot は、HID のデバイスができたあとに、HID の形を読み直し、何も押していない状態を送らせる。
func (u *USBMode) finishBoot(waited time.Duration) {
	u.lockReports()
	u.hid.Close() // 開けなかったあとの待ち時間を消し、次に書くときにすぐ開く
	l := u.reloadLayout()
	u.unlockReports()
	log.Printf("usb: ready after %v: %s", waited.Round(time.Millisecond), l.Source)
	u.mu.Lock()
	u.switching = false
	u.preparing = false
	fs := append([]func(){}, u.onChange...)
	u.mu.Unlock()
	for _, f := range fs {
		f()
	}
}

// lockReports は、キーボード、マウス、メディアキー、ペンのレポートを書かせないようにする（付け直しと、起動のときの準備）。
func (u *USBMode) lockReports() {
	u.s.mu.Lock()
	u.s.mouse.mu.Lock()
	u.s.media.mu.Lock()
	if u.s.pen == nil {
		u.s.pen = NewPen(u.hid, 0)
	}
	u.s.pen.mu.Lock()
}

// reloadLayout は、HID の形を読み直し、レポートの書き方を合わせる。押していたものは忘れ、何も押していない状態を送らせる。
// lockReports のあいだに呼ぶ。
func (u *USBMode) reloadLayout() HIDLayout {
	l := u.detect(u.dev)
	u.s.kbdID = l.KeyboardID
	u.s.mouse.id = l.MouseID
	u.s.mouse.on.Store(l.MouseID != 0)
	u.s.mouse.held = map[string]byte{}
	u.s.mouse.dirty = false
	u.s.media.reset(l.MediaID)
	u.s.pen.reset(l.PenID)
	u.s.active = map[string]Combo{}
	u.s.dirty = true // 何も押していない状態を送る
	return l
}

func (u *USBMode) unlockReports() {
	u.s.pen.mu.Unlock()
	u.s.media.mu.Unlock()
	u.s.mouse.mu.Unlock()
	u.s.mu.Unlock()
}

// waitDevice は、path ができるまで、limit まで待つ（起動のとき、ドライバの読み込みが遅れても止まらないように）。
// できていれば待たない。待ったら、その時間をログに出す。
func waitDevice(path string, limit time.Duration) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	start := time.Now()
	for time.Since(start) < limit {
		time.Sleep(200 * time.Millisecond)
		if _, err := os.Stat(path); err == nil {
			log.Printf("%s appeared after %v", path, time.Since(start).Round(time.Millisecond))
			return true
		}
	}
	return false
}

// openInputWait は、openInput と同じだが、見つからなければ limit まで待って開き直す（起動のとき、ドライバの読み込みを待つ）。
func openInputWait(spec string, limit time.Duration) (dev *evdev.InputDevice, err error) {
	start := time.Now()
	for {
		dev, err = openInput(spec)
		if err == nil || time.Since(start) > limit {
			if err == nil && time.Since(start) > time.Second {
				log.Printf("input %q appeared after %v", spec, time.Since(start).Round(time.Millisecond))
			}
			return dev, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}
