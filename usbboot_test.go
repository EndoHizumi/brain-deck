package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// 起動したときに /dev/hidg0 がまだなければ、準備中にして、USB の形の切り替えを断る。
// できたら HID の形を読み直し、準備中の終わりを知らせ、何も押していない状態を送らせる。
func TestUSBBootWaitsForHID(t *testing.T) {
	old := usbSwitchDelay
	usbSwitchDelay = 0
	defer func() { usbSwitchDelay = old }()
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, 0), media: NewMedia(h, 0)}
	g := &fakeGadget{mouse: true} // gadget.env の HID_MOUSE=1 で、キーボードとマウスの形で作られる
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/hidg0", "gadget-setup")
	u.run, u.detect = g.run, g.detect
	changed := make(chan struct{}, 4)
	u.OnChange(func() { changed <- struct{}{} })

	var there atomic.Bool
	if !u.StartBoot(func(string) bool { return there.Load() }, time.Millisecond, time.Minute) {
		t.Fatal("StartBoot should wait when the device is missing")
	}
	if !u.Preparing() || !u.Switching() {
		t.Fatalf("preparing %v switching %v", u.Preparing(), u.Switching())
	}
	if _, _, err := u.Request(usbKeyboard); err != errUSBBusy {
		t.Fatalf("switching while preparing: %v", err)
	}
	// 準備中に押したキーは、PC に届かないが、押した状態は覚える（外れているときと同じ）
	s.press("k", Combo{Keys: []byte{0x04}})
	h.take()

	there.Store(true)
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("did not finish")
	}
	if u.Preparing() || u.Switching() {
		t.Fatal("still preparing")
	}
	if !s.mouse.Available() || s.kbdID != hidKeyboardID {
		t.Fatalf("layout not reloaded: kbdID %d mouse %v", s.kbdID, s.mouse.Available())
	}
	s.mu.Lock()
	dirty, n := s.dirty, len(s.active)
	s.mu.Unlock()
	if !dirty || n != 0 {
		t.Fatalf("must send an empty report after USB is ready: dirty %v active %d", dirty, n)
	}
	if len(g.runs) != 0 {
		t.Fatalf("must not run gadget-setup while booting: %v", g.runs)
	}
	// 準備ができたら、切り替えられる
	if _, ch, err := u.Request(usbKeyboard); err != nil || !ch {
		t.Fatalf("after boot: %v %v", ch, err)
	}
	<-changed
}

// デバイスがあれば待たない（lefthand を起動し直したとき）。
func TestUSBBootDeviceThere(t *testing.T) {
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, 0), media: NewMedia(h, 0)}
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/hidg0", "gadget-setup")
	if u.StartBoot(func(string) bool { return true }, time.Millisecond, time.Minute) || u.Preparing() || u.Switching() {
		t.Fatal("must not wait when the device exists")
	}
}

// 現れないまま上限を過ぎたら、待つのをやめる（キーボードだけ）。
func TestUSBBootGivesUp(t *testing.T) {
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, 0), media: NewMedia(h, 0)}
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/hidg0", "gadget-setup")
	u.detect = func(string) HIDLayout { return HIDLayout{Source: "missing"} }
	done := make(chan struct{}, 1)
	u.OnChange(func() { done <- struct{}{} })
	u.StartBoot(func(string) bool { return false }, time.Millisecond, 20*time.Millisecond)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not give up")
	}
	if u.Preparing() || u.Switching() {
		t.Fatal("still preparing after the limit")
	}
}

// lefthand が gadget-setup.sh を動かす前に、ethernet_gadget.service の gadget-setup.sh が終わるのを待つ。
func TestWaitGadgetUnit(t *testing.T) {
	old := gadgetUnitState
	defer func() { gadgetUnitState = old }()
	var calls atomic.Int32
	gadgetUnitState = func() string {
		if calls.Add(1) < 3 {
			return "activating"
		}
		return "active"
	}
	waitGadgetUnit(10 * time.Second)
	if calls.Load() != 3 {
		t.Fatalf("calls %d", calls.Load())
	}
	// 終わらなければ、上限で先に進む
	calls.Store(0)
	gadgetUnitState = func() string { calls.Add(1); return "activating" }
	start := time.Now()
	waitGadgetUnit(10 * time.Millisecond)
	if time.Since(start) > 3*time.Second {
		t.Fatal("did not stop at the limit")
	}
	// 試験のデーモンやテストのスクリプトでは、見もしない
	calls.Store(0)
	waitGadgetUnitFor("/tmp/lhu/gadget-setup.sh")
	if calls.Load() != 0 {
		t.Fatal("must not wait for a non-default script")
	}
}

func TestWaitDevice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fb0")
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(p, nil, 0o644)
	}()
	if !waitDevice(p, 5*time.Second) {
		t.Fatal("did not see the device")
	}
	if waitDevice(filepath.Join(t.TempDir(), "none"), 300*time.Millisecond) {
		t.Fatal("missing device")
	}
}
