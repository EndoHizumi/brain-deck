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
	onChange  []func()                                                   // 切り替えたあと（画面の描き直し、トラックパッドの取り消し）
	run       func(ctx context.Context, script string, mouse bool) error // テストで差し替える
	detect    func(dev string) HIDLayout
}

func NewUSBMode(s *State, hid *HIDWriter, dev, script string) *USBMode {
	return &USBMode{s: s, hid: hid, dev: dev, script: script, run: runGadgetSetup, detect: detectHID}
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

// apply は、USB を付け直して形を変える。キーとボタンをすべて離してから行う。
func (u *USBMode) apply(mouse bool) {
	start := time.Now()
	u.s.releaseAll()
	// 付け直しのあいだは、キーボードとマウスのレポートを書かせない（入力は、終わってから届く）
	u.s.mu.Lock()
	u.s.mouse.mu.Lock()
	u.hid.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := u.run(ctx, u.script, mouse)
	cancel()
	l := u.detect(u.dev)
	u.s.kbdID = l.KeyboardID
	u.s.mouse.id = l.MouseID
	u.s.mouse.on.Store(l.MouseID != 0)
	u.s.mouse.held = map[string]byte{}
	u.s.mouse.dirty = false
	u.s.active = map[string]Combo{}
	u.s.dirty = true // 付け直したあとに、何も押していない状態を送る
	u.s.mouse.mu.Unlock()
	u.s.mu.Unlock()
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
}

func runGadgetSetup(ctx context.Context, script string, mouse bool) error {
	m := "0"
	if mouse {
		m = "1"
	}
	cmd := exec.CommandContext(ctx, script)
	cmd.Env = append(os.Environ(), "HID_MOUSE="+m, "LEFTHAND_SELF=1")
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

// usbLabels は、usb_mode のセルに出す名前。
var usbLabels = map[string]string{usbKeyboard: "マウスオフ", usbMouse: "マウスオン", usbToggle: "マウス切替"}
