package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// gadgetDesc は、gadget-setup.sh の NAME='...' （'...' が続けて書いてあれば、つなげたもの）を、bash の printf で展開する。
func gadgetDesc(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("gadget-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + name + `=((?:'[^']*'\\\n)*'[^']*')`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s not found in gadget-setup.sh", name)
	}
	lit := strings.ReplaceAll(string(m[1]), "\\\n", "")
	out, err := exec.Command("bash", "-c", "printf "+lit).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGadgetSetupDescriptorsMatchGo(t *testing.T) {
	if got := gadgetDesc(t, "KBD_DESC"); !bytes.Equal(got, hidDescKeyboard) {
		t.Errorf("KBD_DESC differs from hidDescKeyboard:\n% x\n% x", got, hidDescKeyboard)
	}
	if got := gadgetDesc(t, "COMBO_DESC"); !bytes.Equal(got, hidDescCombo) {
		t.Errorf("COMBO_DESC differs from hidDescCombo:\n% x\n% x", got, hidDescCombo)
	}
}

// TestComboDescriptorShape は、ディスクリプタの項目を順に読み、レポートの大きさが送るものと同じか確かめる。
func TestComboDescriptorShape(t *testing.T) {
	type rep struct{ in, out int } // ビット数
	sizes := map[byte]*rep{}
	var id byte
	var size, count int
	d := hidDescCombo
	depth := 0
	for i := 0; i < len(d); {
		pre := d[i]
		n := int(pre & 3)
		if n == 3 {
			n = 4
		}
		var v int
		for k := 0; k < n; k++ {
			v |= int(d[i+1+k]) << (8 * k)
		}
		switch pre &^ 3 {
		case 0x84: // Report ID
			id = byte(v)
			sizes[id] = &rep{}
		case 0x74:
			size = v
		case 0x94:
			count = v
		case 0x80: // Input
			sizes[id].in += size * count
		case 0x90: // Output
			sizes[id].out += size * count
		case 0xa0:
			depth++
		case 0xc0:
			depth--
		}
		i += 1 + n
	}
	if depth != 0 {
		t.Fatalf("collections not closed: depth %d", depth)
	}
	if r := sizes[hidKeyboardID]; r == nil || r.in != 64 || r.out != 8 {
		t.Errorf("keyboard report: %+v, want 64 bits in, 8 bits out", r)
	}
	if r := sizes[hidMouseID]; r == nil || r.in != 40 {
		t.Errorf("mouse report: %+v, want 40 bits (buttons, X, Y, wheel, pan)", r)
	}
}

func TestDetectHID(t *testing.T) {
	root := t.TempDir()
	old := configfsGadgets
	configfsGadgets = root
	defer func() { configfsGadgets = old }()
	fn := filepath.Join(root, "eth", "functions", "hid.usb0")
	os.MkdirAll(fn, 0o755)
	// /dev/null（1:3）を HID のデバイスに見立てる
	os.WriteFile(filepath.Join(fn, "dev"), []byte("1:3\n"), 0o644)
	for _, c := range []struct {
		desc      []byte
		kbd, mous byte
	}{
		{hidDescCombo, hidKeyboardID, hidMouseID},
		{hidDescKeyboard, 0, 0},
		{[]byte{1, 2, 3}, 0, 0},
	} {
		os.WriteFile(filepath.Join(fn, "report_desc"), c.desc, 0o644)
		l := detectHID("/dev/null")
		if l.KeyboardID != c.kbd || l.MouseID != c.mous {
			t.Errorf("% x: got %+v", c.desc[:3], l)
		}
	}
	if l := detectHID(filepath.Join(root, "nope")); l.KeyboardID != 0 || l.MouseID != 0 {
		t.Errorf("missing device: %+v", l)
	}
	if l := detectHID("/dev/zero"); l.MouseID != 0 { // 1:5 は configfs にない
		t.Errorf("unknown device: %+v", l)
	}
}

// hidRec は、書いたレポートを覚える hidOut。fail のあいだは失敗する。
type hidRec struct {
	mu   sync.Mutex
	reps [][]byte
	fail bool
}

func (h *hidRec) Write(r []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail {
		return os.ErrDeadlineExceeded
	}
	h.reps = append(h.reps, append([]byte(nil), r...))
	return nil
}

func (h *hidRec) take() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.reps
	h.reps = nil
	return r
}

func TestKeyboardReportGetsID(t *testing.T) {
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, kbdID: hidKeyboardID}
	s.press("k", Combo{Mods: 0x01, Keys: []byte{0x1d}})
	r := h.take()
	if len(r) != 1 || !bytes.Equal(r[0], []byte{1, 0x01, 0, 0x1d, 0, 0, 0, 0, 0}) {
		t.Fatalf("got % x", r)
	}
	s2 := &State{hid: h, active: map[string]Combo{}}
	s2.press("k", Combo{Keys: []byte{0x04}})
	if r := h.take(); len(r[0]) != 8 {
		t.Fatalf("keyboard only: got % x", r)
	}
}

func TestMouseReports(t *testing.T) {
	h := &hidRec{}
	m := NewMouse(h, hidMouseID)
	m.Press("a", mouseLeft)
	m.Move(300, -5) // 127 + 127 + 46 に分ける
	m.Press("b", mouseRight)
	m.Release("a")
	m.Wheel(-2, 1)
	want := [][]byte{
		{2, 1, 0, 0, 0, 0},
		{2, 1, 127, 0xfb, 0, 0},
		{2, 1, 127, 0, 0, 0},
		{2, 1, 46, 0, 0, 0},
		{2, 3, 0, 0, 0, 0},
		{2, 2, 0, 0, 0, 0},
		{2, 2, 0, 0, 0xfe, 1},
	}
	got := h.take()
	if len(got) != len(want) {
		t.Fatalf("got %d reports % x", len(got), got)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("#%d: got % x, want % x", i, got[i], want[i])
		}
	}
	// ガジェットにマウスがなければ、何も送らない
	none := NewMouse(h, 0)
	none.Press("a", mouseLeft)
	none.Move(1, 1)
	if r := h.take(); len(r) != 0 || none.Available() {
		t.Fatalf("no mouse: got % x", r)
	}
	var nilMouse *Mouse
	nilMouse.Press("a", 1)
	nilMouse.ReleaseAll()
}

func TestMouseRetryAfterFailure(t *testing.T) {
	h := &hidRec{}
	m := NewMouse(h, hidMouseID)
	m.Press("a", mouseLeft)
	h.fail = true
	m.Release("a") // 届かない
	m.Move(5, 5)   // 捨てる
	h.fail = false
	h.take()
	m.retry()
	if r := h.take(); len(r) != 1 || !bytes.Equal(r[0], []byte{2, 0, 0, 0, 0, 0}) {
		t.Fatalf("retry: got % x", r)
	}
	m.retry()
	if r := h.take(); len(r) != 0 {
		t.Fatalf("second retry should send nothing: % x", r)
	}
}

func TestMouseReleaseExceptAndScrollRepeat(t *testing.T) {
	oldD, oldE := scrollRepeatDelay, scrollRepeatEvery
	scrollRepeatDelay, scrollRepeatEvery = 20*time.Millisecond, 10*time.Millisecond
	defer func() { scrollRepeatDelay, scrollRepeatEvery = oldD, oldE }()
	h := &hidRec{}
	m := NewMouse(h, hidMouseID)
	m.Press("a", mouseLeft)
	m.Press("b", mouseMiddle)
	m.StartScroll("c", -1, 0)
	time.Sleep(60 * time.Millisecond)
	m.ReleaseExcept("b")
	if m.Held() != mouseMiddle {
		t.Fatalf("held %d, want middle", m.Held())
	}
	n := 0
	for _, r := range h.take() {
		if r[4] == 0xff {
			n++
		}
	}
	if n < 3 {
		t.Errorf("scroll repeated %d times, want >= 3", n)
	}
	time.Sleep(40 * time.Millisecond)
	for _, r := range h.take() {
		if r[4] != 0 {
			t.Fatalf("scroll kept repeating after ReleaseExcept: % x", r)
		}
	}
	m.ReleaseAll()
	if m.Held() != 0 {
		t.Fatal("ReleaseAll left buttons")
	}
}
