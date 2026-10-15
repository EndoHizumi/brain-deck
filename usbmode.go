package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------- USB の形の切り替え（キーボードだけ ⇔ キーボードとマウス） ----------
//
// 起動したときは、ブートキーボードの形（キーボードだけ）にする（gadget-setup.sh の既定 HID_MOUSE=0）。
// BIOS や UEFI の画面でも使えるようにするため。マウスを使うときだけ、キーボードとマウスの形に切り替える。
// 切り替えは USB の付け直しなので、2〜3 秒、PC へのキー入力、SSH（usb0）、シリアル（設定 GUI、コンソール）が切れる。
//
// デーモンは、/dev/hidg0 を閉じてから gadget-setup.sh を実行する（このカーネルの f_hid は、開いたまま付け直すと
// ENXIO で開けなくなるため）。gadget-setup.sh には LEFTHAND_SELF=1 を渡し、lefthand.service を止めさせない。
// 付け直したあとに HID の形を読み直し、キーボードのレポートの書き方とマウスの有無を変える。

const (
	usbKeyboard = "keyboard" // キーボードだけ（ブートキーボード）
	usbMouse    = "mouse"    // キーボードとマウス
	usbToggle   = "toggle"
)

var defaultGadgetSetup = "/usr/local/sbin/lefthand-gadget-setup"

// usbSwitchDelay は、切り替えを頼まれてから付け直すまでの時間（設定 GUI への返事を送り終えるため）。
var usbSwitchDelay = 300 * time.Millisecond

// USBMode は USB の形を切り替える。
type USBMode struct {
	mu        sync.Mutex
	s         *State
	hid       *HIDWriter
	dev       string
	script    string
	switching bool
	preparing bool                                                                             // 起動のときの USB の準備を待っている（usbboot.go）
	terminal  bool                                                                             // 端末モード（構成の名前に terminal を付ける。termmode.go）
	netWant   string                                                                           // 次の付け直しで変えるネットワークの方式（netmode.go）。空なら今のまま
	onChange  []func()                                                                         // 切り替えたあと（画面の描き直し、トラックパッドの取り消し）
	run       func(ctx context.Context, script string, mouse, terminal bool, net string) error // テストで差し替える
	unbind    func(ctx context.Context, script string) error                                   // 切り離すだけ（テストで差し替える）
	detect    func(dev string) HIDLayout
	netMode   func() string // 今のネットワークの方式（テストで差し替える）
	waitUnit  func()        // gadget-setup.sh を動かす前に、ethernet_gadget の終わりを待つ（usbboot.go）
}

func NewUSBMode(s *State, hid *HIDWriter, dev, script string) *USBMode {
	return &USBMode{s: s, hid: hid, dev: dev, script: script, run: runGadgetSetup, unbind: runGadgetUnbind, detect: detectHID,
		netMode: detectNetMode, waitUnit: func() { waitGadgetUnitFor(script) }}
}

// OnChange は、切り替えたあとに呼ぶ関数を足す。
func (u *USBMode) OnChange(f func()) {
	u.mu.Lock()
	u.onChange = append(u.onChange, f)
	u.mu.Unlock()
}

// Mouse は、今マウスがあるか。
func (u *USBMode) Mouse() bool { return u != nil && u.s.mouse.Available() }

// Switching は、切り替えの途中か。
func (u *USBMode) Switching() bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.switching
}

var errUSBBusy = errors.New("switching the USB mode is already in progress")

// Request は、mode（keyboard、mouse、toggle）への切り替えを始め、切り替え先がマウスありかを返す。待たずに返る。
// すでにその形なら、何もしない（changed が false）。
func (u *USBMode) Request(mode string) (mouse, changed bool, err error) {
	if u == nil {
		return false, false, errors.New("USB mode switching is not available")
	}
	cur := u.Mouse()
	switch mode {
	case usbKeyboard:
		mouse = false
	case usbMouse:
		mouse = true
	case usbToggle:
		mouse = !cur
	default:
		return cur, false, fmt.Errorf("mode must be %s, %s or %s", usbKeyboard, usbMouse, usbToggle)
	}
	u.mu.Lock()
	if u.switching {
		u.mu.Unlock()
		return mouse, false, errUSBBusy
	}
	if mouse == cur {
		u.mu.Unlock()
		return mouse, false, nil
	}
	u.switching = true
	u.mu.Unlock()
	go func() {
		time.Sleep(usbSwitchDelay)
		u.apply(mouse)
	}()
	return mouse, true, nil
}

// SetTerminal は、USB の構成の名前を端末モードのもの（on）か、ふだんのものにして、付け直す。終わるまで待つ。
// PC の udev は、端末モードの名前のときだけ、1 つ目のシリアルで getty を起動する（contrib/udev/71-brain-terminal.rules）。
// HID の形（マウスのあり・なし）は変えない。unbound は、UDC から切り離しているあいだに呼ぶ（nil でもよい）。
// 端末モードは、ここで Brain の getty を止めたり、ttyGS0 を閉じたりする（termmode.go）。
func (u *USBMode) SetTerminal(on bool, unbound func()) error {
	if u == nil {
		return errors.New("USB mode switching is not available")
	}
	deadline := time.Now().Add(40 * time.Second)
	for {
		u.mu.Lock()
		if !u.switching {
			break
		}
		u.mu.Unlock()
		if time.Now().After(deadline) {
			return errUSBBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
	u.switching = true
	u.terminal = on
	u.mu.Unlock()
	return u.applyHook(u.Mouse(), unbound)
}

// apply は、USB を付け直して形を変える。キーとボタンをすべて離してから行う。
func (u *USBMode) apply(mouse bool) error { return u.applyHook(mouse, nil) }

// applyHook は apply と同じ。unbound があれば、先に UDC から切り離して unbound を呼んでから付け直す。
func (u *USBMode) applyHook(mouse bool, unbound func()) error {
	start := time.Now()
	u.s.releaseAll()
	// 付け直しのあいだは、キーボードとマウスのレポートを書かせない（入力は、終わってから届く）
	u.lockReports()
	u.hid.Close()
	if u.waitUnit != nil {
		u.waitUnit()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	u.mu.Lock()
	terminal, net := u.terminal, u.netWant
	u.netWant = ""
	u.mu.Unlock()
	if unbound != nil {
		if err := u.unbind(ctx, u.script); err != nil {
			log.Printf("usb: %v", err)
		}
		unbound()
	}
	err := u.run(ctx, u.script, mouse, terminal, net)
	cancel()
	l := u.reloadLayout() // 付け直したあとに、何も押していない状態を送る
	u.unlockReports()
	if err != nil {
		log.Printf("usb: switching failed: %v", err)
	}
	log.Printf("usb: %s (%v)", l.Source, time.Since(start).Round(time.Millisecond))
	u.mu.Lock()
	u.switching = false
	fs := append([]func(){}, u.onChange...)
	u.mu.Unlock()
	for _, f := range fs {
		f()
	}
	return err
}

// runGadgetSetup は gadget-setup.sh を実行する。mouse は HID の形、terminal は構成の名前（端末モード）、
// net はネットワークの方式（空なら渡さない。スクリプトは今の方式のままにする）。
func runGadgetSetup(ctx context.Context, script string, mouse, terminal bool, net string) error {
	b := map[bool]string{false: "0", true: "1"}
	cmd := exec.CommandContext(ctx, script)
	cmd.Env = append(os.Environ(), "HID_MOUSE="+b[mouse], "GADGET_TERMINAL="+b[terminal], "LEFTHAND_SELF=1")
	if net != "" {
		cmd.Env = append(cmd.Env, "NET_MODE="+net)
	}
	out, err := cmd.CombinedOutput()
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			vlogf("usb: gadget-setup: %s", l)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %v: %s", script, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runGadgetUnbind は、gadget-setup.sh で UDC から切り離すだけ行う。
func runGadgetUnbind(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, script)
	cmd.Env = append(os.Environ(), "GADGET_UNBIND_ONLY=1", "LEFTHAND_SELF=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (unbind): %v: %s", script, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// usbLabels は、usb_mode のセルに出す名前。
var usbLabels = map[string]string{usbKeyboard: "マウスオフ", usbMouse: "マウスオン", usbToggle: "マウス切替"}
