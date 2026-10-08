package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// fakeGadget は、gadget-setup.sh の代わりに HID の形を覚える。
type fakeGadget struct {
	mu    sync.Mutex
	mouse bool
	runs  []bool
	terms []bool        // 付け直すたびの構成の名前（端末モードか）
	block chan struct{} // 閉じるまで付け直しを終えない
}

func (g *fakeGadget) run(ctx context.Context, script string, mouse, terminal bool) error {
	if g.block != nil {
		<-g.block
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mouse = mouse
	g.runs = append(g.runs, mouse)
	g.terms = append(g.terms, terminal)
	return nil
}

func (g *fakeGadget) detect(string) HIDLayout {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mouse {
		return HIDLayout{KeyboardID: hidKeyboardID, MouseID: hidMouseID, Source: "fake: keyboard + mouse"}
	}
	return HIDLayout{Source: "fake: keyboard only"}
}

func TestUSBModeSwitch(t *testing.T) {
	old := usbSwitchDelay
	usbSwitchDelay = 0
	defer func() { usbSwitchDelay = old }()
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, 0)}
	g := &fakeGadget{}
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/null", "gadget-setup")
	u.run, u.detect = g.run, g.detect
	changed := make(chan struct{}, 4)
	u.OnChange(func() { changed <- struct{}{} })

	// 押しているキーは、付け直す前に離す
	s.press("k", Combo{Keys: []byte{0x04}})
	h.take()
	if mouse, ch, err := u.Request(usbToggle); err != nil || !mouse || !ch {
		t.Fatalf("toggle: %v %v %v", mouse, ch, err)
	}
	<-changed
	if !s.mouse.Available() || s.kbdID != hidKeyboardID || !u.Mouse() || u.Switching() {
		t.Fatalf("after switching to mouse: kbdID %d, mouse %v, switching %v", s.kbdID, s.mouse.Available(), u.Switching())
	}
	reps := h.take()
	if len(reps) == 0 || len(reps[0]) != 8 || reps[0][2] != 0 {
		t.Fatalf("keys must be released before switching: % x", reps)
	}
	// 付け直したあとは、ID 付きのキーボードのレポートで送る
	s.press("k", Combo{Keys: []byte{0x05}})
	if r := h.take(); len(r) != 1 || len(r[0]) != 9 || r[0][0] != hidKeyboardID {
		t.Fatalf("after switch: % x", r)
	}
	s.release("k")
	// すでにその形なら何もしない
	if _, ch, err := u.Request(usbMouse); err != nil || ch {
		t.Fatalf("same mode: %v %v", ch, err)
	}
	// 切り替えの途中は、もう一度頼んでも断る
	g.block = make(chan struct{})
	if _, ch, err := u.Request(usbKeyboard); err != nil || !ch {
		t.Fatal(err)
	}
	if _, _, err := u.Request(usbMouse); err != errUSBBusy {
		t.Fatalf("busy: %v", err)
	}
	close(g.block)
	<-changed
	if s.mouse.Available() || s.kbdID != 0 {
		t.Fatal("should be keyboard only")
	}
	if _, _, err := u.Request("nope"); err == nil {
		t.Fatal("bad mode should fail")
	}
	if len(g.runs) != 2 || !g.runs[0] || g.runs[1] {
		t.Fatalf("runs %v", g.runs)
	}
}

// TestUSBModeKey は、usb_mode のキーで切り替わり、押していたマウスのボタンも離すことを確かめる。
func TestUSBModeKey(t *testing.T) {
	old := usbSwitchDelay
	usbSwitchDelay = 0
	defer func() { usbSwitchDelay = old }()
	cfg, err := parseConfig([]byte(`
layers:
  - name: base
    keys:
      KEY_A: { mouse: left }
      KEY_B: { usb_mode: toggle, label: マウス }
`))
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, kbdID: hidKeyboardID, mouse: NewMouse(h, hidMouseID)}
	e := NewEngine(km, s)
	g := &fakeGadget{mouse: true}
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/null", "gadget-setup")
	u.run, u.detect = g.run, g.detect
	done := make(chan struct{}, 1)
	u.OnChange(func() { done <- struct{}{} })
	e.SetUSB(u)
	e.PressKey(evdev.KEY_A)
	e.PressKey(evdev.KEY_B)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not switch")
	}
	if s.mouse.Available() || s.mouse.Held() != 0 {
		t.Fatalf("mouse %v held %d", s.mouse.Available(), s.mouse.Held())
	}
	e.ReleaseKey(evdev.KEY_A)
	e.ReleaseKey(evdev.KEY_B)
	if v := cellView(km, km.Layers[0].Keys[evdev.KEY_B]); v.Label != "マウス" || v.Sub != "マウス切替" {
		t.Errorf("cell view %+v", v)
	}
	for _, bad := range []string{`{ usb_mode: on }`, `{ usb_mode: mouse, key: A }`} {
		cfg, err := parseConfig([]byte("layers:\n  - name: base\n    keys:\n      KEY_A: " + bad + "\n"))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !(strings.Contains(err.Error(), "usb_mode must be") || strings.Contains(err.Error(), "exactly one")) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// 端末モードの構成の名前は、マウスの切り替えで付け直しても残る。端末モードの切り替えでは HID の形を変えない
func TestUSBModeTerminalName(t *testing.T) {
	old := usbSwitchDelay
	usbSwitchDelay = 0
	defer func() { usbSwitchDelay = old }()
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, 0)}
	g := &fakeGadget{}
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/null", "gadget-setup")
	u.run, u.detect = g.run, g.detect
	changed := make(chan struct{}, 4)
	u.OnChange(func() { changed <- struct{}{} })
	u.unbind = func(context.Context, string) error { return nil }
	hooked := 0
	if err := u.SetTerminal(true, func() { hooked++ }); err != nil {
		t.Fatal(err)
	}
	<-changed
	if _, ch, err := u.Request(usbMouse); err != nil || !ch {
		t.Fatalf("mouse: %v %v", ch, err)
	}
	<-changed
	if err := u.SetTerminal(false, nil); err != nil {
		t.Fatal(err)
	}
	<-changed
	if hooked != 1 {
		t.Errorf("unbound hook called %d times", hooked)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := []bool{true, true, false}; !equalBools(g.terms, want) {
		t.Errorf("terminal names = %v, want %v", g.terms, want)
	}
	if want := []bool{false, true, true}; !equalBools(g.runs, want) {
		t.Errorf("mouse = %v, want %v", g.runs, want)
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
