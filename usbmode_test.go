package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
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

func (g *fakeGadget) run(ctx context.Context, script string, mouse, terminal bool, net netChoice) error {
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
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, 0), media: NewMedia(h, 0)}
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

// TestNetModeSwitch は、ネットワークの方式の切り替えで、NET_MODE を一度だけ渡し、HID の形と端末モードを変えないことを確かめる。
func TestNetModeSwitch(t *testing.T) {
	old := usbSwitchDelay
	usbSwitchDelay = 0
	defer func() { usbSwitchDelay = old }()
	oldCfg := procConfig
	defer func() { procConfig = oldCfg }()
	procConfig = filepath.Join(t.TempDir(), "config.gz")
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	z.Write([]byte("CONFIG_USB_CONFIGFS_NCM=y\n# CONFIG_USB_CONFIGFS_ECM is not set\nCONFIG_USB_CONFIGFS_RNDIS=y\n"))
	z.Close()
	os.WriteFile(procConfig, buf.Bytes(), 0o644)

	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, kbdID: hidKeyboardID, mouse: NewMouse(h, hidMouseID), media: NewMedia(h, hidMediaID)}
	g := &fakeGadget{mouse: true}
	var mu sync.Mutex
	cur := netChoice{Mode: "ncm", MAC: "pc"}
	var nets []netChoice
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/null", "gadget-setup")
	u.detect = g.detect
	u.netMode = func() netChoice { mu.Lock(); defer mu.Unlock(); return cur }
	u.run = func(ctx context.Context, script string, mouse, terminal bool, net netChoice) error {
		mu.Lock()
		nets = append(nets, net)
		if net.Mode != "" {
			cur = net
		}
		mu.Unlock()
		return g.run(ctx, script, mouse, terminal, net)
	}
	changed := make(chan struct{}, 4)
	u.OnChange(func() { changed <- struct{}{} })

	if ch, err := u.RequestNet("ecm", ""); err == nil || ch || !strings.Contains(err.Error(), "CONFIG_USB_CONFIGFS_ECM") {
		t.Fatalf("ecm must be refused when the kernel lacks it: %v %v", ch, err)
	}
	if _, err := u.RequestNet("eem", ""); err == nil {
		t.Fatal("bad mode should fail")
	}
	if _, err := u.RequestNet("ncm", "mine"); err == nil {
		t.Fatal("bad mac should fail")
	}
	if ch, err := u.RequestNet("ncm", ""); err != nil || ch {
		t.Fatalf("same mode: %v %v", ch, err)
	}
	// 方式が同じでも、MAC が違えば付け直す
	if ch, err := u.RequestNet("ncm", "phone"); err != nil || !ch {
		t.Fatalf("ncm phone: %v %v", ch, err)
	}
	<-changed
	if ch, err := u.RequestNet("rndis", ""); err != nil || !ch {
		t.Fatalf("rndis: %v %v", ch, err)
	}
	<-changed
	if (u.NetMode() != netChoice{Mode: "rndis", MAC: "pc"}) || !u.Mouse() {
		t.Fatalf("after switching: net %q, mouse %v", u.NetMode(), u.Mouse())
	}
	// マウスの切り替えでは、方式を渡さない（スクリプトが今の方式のままにする）
	if _, ch, err := u.Request(usbKeyboard); err != nil || !ch {
		t.Fatal(err)
	}
	<-changed
	mu.Lock()
	defer mu.Unlock()
	if want := []netChoice{{"ncm", "phone"}, {"rndis", "pc"}, {}}; fmt.Sprint(nets) != fmt.Sprint(want) {
		t.Errorf("NET_MODE and NET_MAC passed = %v, want %v", nets, want)
	}
	if want := []bool{true, true, false}; !equalBools(g.runs, want) {
		t.Errorf("mouse = %v, want %v", g.runs, want)
	}
	// カーネルの設定が読めなければ、作れるとする
	procConfig = filepath.Join(t.TempDir(), "none.gz")
	if err := netModeSupported("ecm"); err != nil {
		t.Errorf("unreadable config: %v", err)
	}
}

// TestDetectNetMode は、構成にリンクしているネットワークのファンクションから方式を読むことを確かめる。
func TestDetectNetMode(t *testing.T) {
	old := configfsGadgets
	defer func() { configfsGadgets = old }()
	configfsGadgets = t.TempDir()
	c := filepath.Join(configfsGadgets, "eth", "configs", "c.1")
	os.MkdirAll(c, 0o755)
	if m := detectNetMode(); (m != netChoice{}) {
		t.Fatalf("no function: %v", m)
	}
	os.WriteFile(filepath.Join(c, "hid.usb0"), nil, 0o644)
	os.MkdirAll(filepath.Join(c, "rndis.usb0"), 0o755)
	// configfs は host_addr の最後に NUL を付けて返す
	os.WriteFile(filepath.Join(c, "rndis.usb0", "host_addr"), []byte("88:15:8b:44:3a:01\n\x00"), 0o644)
	if m := detectNetMode(); (m != netChoice{Mode: "rndis", MAC: "phone"}) {
		t.Fatalf("rndis phone: %v", m)
	}
	os.WriteFile(filepath.Join(c, "rndis.usb0", "host_addr"), []byte("02:00:00:00:00:01\n"), 0o644)
	if m := detectNetMode(); (m != netChoice{Mode: "rndis", MAC: "02:00:00:00:00:01"}) {
		t.Fatalf("other mac: %v", m)
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
	s := &State{hid: h, active: map[string]Combo{}, kbdID: hidKeyboardID, mouse: NewMouse(h, hidMouseID), media: NewMedia(h, hidMediaID)}
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
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, 0), media: NewMedia(h, 0)}
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
