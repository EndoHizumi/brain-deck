package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUDC は、configfs のガジェットと UDC の sysfs を一時ディレクトリに作る。
func fakeUDC(t *testing.T, state string) (dir string) {
	t.Helper()
	root := t.TempDir()
	oldG, oldU, oldD, oldI := configfsGadgets, udcClass, wakeCheckDelay, wakeMinInterval
	t.Cleanup(func() { configfsGadgets, udcClass, wakeCheckDelay, wakeMinInterval = oldG, oldU, oldD, oldI })
	configfsGadgets = filepath.Join(root, "gadget")
	udcClass = filepath.Join(root, "udc")
	wakeCheckDelay, wakeMinInterval = 200*time.Millisecond, 0
	os.MkdirAll(filepath.Join(configfsGadgets, "eth"), 0o755)
	os.WriteFile(filepath.Join(configfsGadgets, "eth", "UDC"), []byte("fake.0\n"), 0o644)
	dir = filepath.Join(udcClass, "fake.0")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "state"), []byte(state+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "srp"), nil, 0o644)
	return dir
}

func srpWritten(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "srp"))
	return strings.TrimSpace(string(b))
}

func TestWakerOnlyWhenSuspended(t *testing.T) {
	dir := fakeUDC(t, "configured")
	w := &Waker{}
	w.try()
	if srpWritten(dir) != "" {
		t.Error("asked to wake while configured")
	}
	if USBState() != "configured" {
		t.Errorf("USBState = %q", USBState())
	}
	// ガジェットが UDC に付いていなければ、何もしない
	os.WriteFile(filepath.Join(configfsGadgets, "eth", "UDC"), []byte("\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "state"), []byte("suspended\n"), 0o644)
	w.try()
	if srpWritten(dir) != "" || USBState() != "" {
		t.Error("asked to wake without a bound UDC")
	}
}

func TestWakerWakes(t *testing.T) {
	dir := fakeUDC(t, "suspended")
	// PC が起きたことにする：srp に書かれたら configured にする
	go func() {
		for i := 0; i < 200 && srpWritten(dir) == ""; i++ {
			time.Sleep(time.Millisecond)
		}
		os.WriteFile(filepath.Join(dir, "state"), []byte("configured\n"), 0o644)
	}()
	w := &Waker{}
	w.try()
	if srpWritten(dir) != "1" {
		t.Fatalf("srp = %q", srpWritten(dir))
	}
	if w.warned {
		t.Error("warned although the PC woke up")
	}
}

func TestWakerNotAllowed(t *testing.T) {
	dir := fakeUDC(t, "suspended") // srp に書いても起きない（PC が許していない）
	w := &Waker{}
	w.try()
	if srpWritten(dir) != "1" || !w.warned {
		t.Fatalf("srp = %q, warned = %v", srpWritten(dir), w.warned)
	}
	// 続けて押しても、間をあける
	wakeMinInterval = time.Hour
	os.WriteFile(filepath.Join(dir, "srp"), nil, 0o644)
	w.try()
	if srpWritten(dir) != "" {
		t.Error("asked again within wakeMinInterval")
	}
	// PC が起きたら、次のサスペンドでまたログに出す
	os.WriteFile(filepath.Join(dir, "state"), []byte("configured\n"), 0o644)
	w.try()
	if w.warned {
		t.Error("warned is not reset after resume")
	}
}

// TestWakerRequestFromPress は、キーを押したときに起こし、離したときには起こさないことを確かめる。
func TestWakerRequestFromPress(t *testing.T) {
	dir := fakeUDC(t, "suspended")
	old := hostWaker
	defer func() { hostWaker = old }()
	w := &Waker{req: make(chan struct{}, 1)} // loop は動かさず、頼まれたかを見る
	hostWaker = w
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, mouse: NewMouse(h, hidMouseID), media: NewMedia(h, hidMediaID)}
	s.release("k") // 離すだけでは起こさない
	s.releaseAll()
	s.mouse.retry()
	if len(w.req) != 0 {
		t.Fatal("asked to wake the PC on a release")
	}
	for _, press := range []func(){
		func() { s.press("k", Combo{Keys: []byte{0x04}}) },
		func() { s.mouse.Press("m", mouseLeft) },
		func() { s.mouse.Move(3, 0) },
		func() { s.media.Press("v", mediaActions["mute"]) },
	} {
		press()
		select {
		case <-w.req:
		default:
			t.Error("a press did not ask to wake the PC")
		}
	}
	s.press("k2", Combo{Keys: []byte{0x05}})
	w.try()
	if srpWritten(dir) != "1" {
		t.Error("did not wake the PC")
	}
	if reps := h.take(); len(reps) == 0 || reps[len(reps)-1][2] != 0x04 || reps[len(reps)-1][3] != 0x05 {
		t.Errorf("the key report must still be written: % x", reps)
	}
}

func TestWakerNotAttached(t *testing.T) {
	dir := fakeUDC(t, "not attached")
	w := &Waker{}
	w.try()
	if srpWritten(dir) != "" || !w.gone {
		t.Fatalf("srp = %q, gone = %v", srpWritten(dir), w.gone)
	}
	os.WriteFile(filepath.Join(dir, "state"), []byte("configured\n"), 0o644)
	w.try()
	if w.gone {
		t.Error("gone is not reset after the USB came back")
	}
}
