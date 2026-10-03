package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCombo(t *testing.T) {
	c, err := parseCombo("LCTRL+lshift+Z")
	if err != nil {
		t.Fatal(err)
	}
	if c.Mods != 0x03 || len(c.Keys) != 1 || c.Keys[0] != 0x1d {
		t.Fatalf("got %+v", c)
	}
	if _, err := parseCombo("LCTRL+NOPE"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestCellOf(t *testing.T) {
	for _, tc := range []struct {
		v, min, max int32
		n, want     int
	}{
		{0, 0, 4095, 4, 0},
		{4095, 0, 4095, 4, 3},
		{2048, 0, 4095, 4, 2},
		{-50, 0, 4095, 4, 0},  // 範囲外は端に寄せる
		{5000, 0, 4095, 4, 3}, // 同上
		{4000, 4095, 0, 4, 0}, // min>max で反転
		{100, 4095, 0, 4, 3},
	} {
		if got := cellOf(tc.v, tc.min, tc.max, tc.n); got != tc.want {
			t.Errorf("cellOf(%d,%d,%d,%d)=%d want %d", tc.v, tc.min, tc.max, tc.n, got, tc.want)
		}
	}
}

func TestReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidg")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &State{hid: NewHIDWriter(path), active: map[string]Combo{}}
	z, _ := parseCombo("LCTRL+Z")
	shift, _ := parseCombo("LSHIFT")
	s.press("k:30", z)
	s.press("k:57", shift)
	s.release("k:30")
	s.release("k:99") // 押されていないものは送らない
	s.releaseAll()

	got, _ := os.ReadFile(path)
	want := [][]byte{
		{0x01, 0, 0x1d, 0, 0, 0, 0, 0},
		{0x03, 0, 0x1d, 0, 0, 0, 0, 0},
		{0x02, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0},
	}
	if !bytes.Equal(got, bytes.Join(want, nil)) {
		t.Fatalf("reports:\n got % x\nwant % x", got, bytes.Join(want, nil))
	}
}

func TestHIDWriterMissingDeviceDoesNotPanic(t *testing.T) {
	s := &State{hid: NewHIDWriter("/nonexistent/hidg0"), active: map[string]Combo{}}
	c, _ := parseCombo("A")
	s.press("x", c)
	if !s.dirty {
		t.Fatal("failed write should mark state dirty for retry")
	}
}

func TestKeypadKeys(t *testing.T) {
	c, err := parseCombo("LCTRL+KPPLUS")
	if err != nil || c.Mods != 0x01 || c.Keys[0] != 0x57 {
		t.Fatalf("got %+v %v", c, err)
	}
	for name, want := range map[string]byte{"KP1": 0x59, "KP9": 0x61, "KP0": 0x62, "KPMINUS": 0x56, "KPDOT": 0x63} {
		if got := hidUsage[name]; got != want {
			t.Errorf("%s = %#x want %#x", name, got, want)
		}
	}
	if got := prettyCombo("LCTRL+KPPLUS"); got != "Ctrl++" {
		t.Errorf("pretty = %q", got)
	}
}
