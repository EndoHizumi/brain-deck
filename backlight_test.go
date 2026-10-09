package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 自動減光 ----------

type fakeBacklight struct {
	t   *testing.T
	dir string
}

func newFakeBacklight(t *testing.T, cur int) *fakeBacklight {
	t.Helper()
	root := t.TempDir()
	oldClass, oldState := backlightClass, backlightStateFile
	backlightClass = filepath.Join(root, "class")
	backlightStateFile = filepath.Join(root, "run-state")
	t.Cleanup(func() { backlightClass, backlightStateFile = oldClass, oldState })
	dir := filepath.Join(backlightClass, "backlight@0")
	writeSys(t, root, map[string]string{"class/backlight@0/max_brightness": "7", "class/backlight@0/brightness": fmt.Sprint(cur)})
	return &fakeBacklight{t: t, dir: dir}
}

// level は今の明るさ。ふつうのファイルなので、書いている途中（空）に読んだら読み直す（sysfs ではそうならない）。
func (f *fakeBacklight) level() int {
	var err error
	for i := 0; i < 50; i++ {
		var v int
		if v, err = readSysInt(filepath.Join(f.dir, "brightness")); err == nil {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	f.t.Fatal(err)
	return 0
}

// waitLevel は、明るさが want になるまで待つ（書くのは別の goroutine）。
func (f *fakeBacklight) waitLevel(want int) {
	f.t.Helper()
	for i := 0; i < 200; i++ {
		if f.level() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("brightness = %d, want %d", f.level(), want)
}

func dimParamsFor(t *testing.T, src string) *dimParams {
	t.Helper()
	cfg, err := parseConfig([]byte("touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers: [{name: base, touch: {cols: 1, rows: 1}}]\nbacklight: " + src + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := compileKeymap(cfg); err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Backlight.compile()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBacklightConfig(t *testing.T) {
	for src, want := range map[string]string{
		`{dim_after: 1m, off_after: 30s}`: "must be longer than dim_after",
		`{dim_after: 1s}`:                 "dim_after must be a duration",
		`{off_after: forever}`:            "off_after must be a duration",
		`{level: 0}`:                      "level must be 1 or more",
		`{dim_level: -1}`:                 "dim_level must be",
		`{device: /dev/fb0}`:              "device must be",
		`{dim_after: 1m, nope: 1}`:        "nope",
	} {
		cfg, err := parseConfig([]byte("touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers: [{name: base, touch: {cols: 1, rows: 1}}]\nbacklight: " + src + "\n"))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", src, err, want)
		}
	}
	p := dimParamsFor(t, `{dim_after: 1m, off_after: 5m, dim_level: 2, wake_on_key: false, in_terminal: true}`)
	if p.dimAfter != time.Minute || p.offAfter != 5*time.Minute || p.dimLevel != 2 || p.wakeOnKey || !p.inTerminal || p.level != -1 {
		t.Errorf("params = %+v", p)
	}
	if p, _ := (*BacklightConfig)(nil).compile(); p != nil {
		t.Error("no backlight section must mean no dimming")
	}
}

func TestDimmer(t *testing.T) {
	fb := newFakeBacklight(t, 5)
	p := dimParamsFor(t, `{dim_after: 5s, off_after: 10s, dim_level: 1}`)
	p.dimAfter, p.offAfter = 60*time.Millisecond, 160*time.Millisecond // テストでは短く
	d := NewDimmer(p)
	defer d.Close()
	if in := d.Info(); !in.Enabled || in.State != "bright" || in.Level != 5 || in.Max != 7 {
		t.Fatalf("info = %+v", in)
	}
	fb.waitLevel(1) // 暗くする
	if b, err := os.ReadFile(backlightStateFile); err != nil || !strings.HasPrefix(string(b), "5 ") {
		t.Errorf("state file = %q %v", b, err)
	}
	fb.waitLevel(0) // 消す
	if d.Info().State != "off" {
		t.Errorf("state = %+v", d.Info())
	}
	// 消えているときのタッチは、明るくするだけ
	if !d.TouchDown() {
		t.Error("a touch while off must be swallowed")
	}
	fb.waitLevel(5)
	time.Sleep(200 * time.Millisecond) // 触れているあいだは暗くしない
	if fb.level() != 5 {
		t.Error("dimmed while the finger is down")
	}
	d.TouchUp()
	if d.TouchDown() {
		t.Error("a touch while bright must not be swallowed")
	}
	d.TouchUp()
	fb.waitLevel(1)
	d.Key() // wake_on_key（既定）
	fb.waitLevel(5)
	// 状態のファイルは、明るさを書いたあとに消すので、少し待つ
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(backlightStateFile); err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(backlightStateFile); err == nil {
		t.Error("the state file must be removed when bright")
	}

	// 端末モードのあいだは暗くしない
	d.SetTerminal(true)
	time.Sleep(200 * time.Millisecond)
	if fb.level() != 5 {
		t.Error("dimmed in terminal mode")
	}
	d.SetTerminal(false)
	fb.waitLevel(1)

	// 設定の差し替え：キーで明るくしない
	p2 := *p
	p2.wakeOnKey = false
	d.Apply(&p2)
	fb.waitLevel(5)
	fb.waitLevel(1)
	d.Key()
	time.Sleep(30 * time.Millisecond)
	if fb.level() != 1 {
		t.Error("wake_on_key: false must not wake on keys")
	}
	d.Wake() // ポモドーロの切り替わり
	fb.waitLevel(5)

	// 自動減光をやめたら明るくし、終わるときは起動したときの明るさに戻す
	d.Apply(nil)
	fb.waitLevel(5)
	if d.TouchDown() {
		t.Error("no dimming: touches must pass")
	}
}

func TestDimmerRestore(t *testing.T) {
	fb := newFakeBacklight(t, 6)
	p := dimParamsFor(t, `{dim_after: 5s, dim_level: 2, level: 7}`)
	p.dimAfter = 30 * time.Millisecond
	d := NewDimmer(p)
	fb.waitLevel(7)
	fb.waitLevel(2)
	d.Close()
	if fb.level() != 6 {
		t.Errorf("Close must restore the brightness at start, got %d", fb.level())
	}

	// 暗くしたまま落ちた：-restore-console が戻す。次のデーモンは、そのときの明るさを使う
	os.WriteFile(filepath.Join(fb.dir, "brightness"), []byte("2\n"), 0o644)
	os.WriteFile(backlightStateFile, []byte("6 "+fb.dir+"\n"), 0o644)
	bl, err := openBacklight("")
	if err != nil || bl.orig != 6 {
		t.Errorf("openBacklight after a crash = %+v %v", bl, err)
	}
	restoreBacklight()
	if fb.level() != 6 {
		t.Errorf("restoreBacklight = %d", fb.level())
	}
	if _, err := os.Stat(backlightStateFile); err == nil {
		t.Error("restoreBacklight must remove the state file")
	}
}

// 暗いときのタッチは、セルを押さず、離したときも何も送らない（touchProc）。
func TestDimSwallowsTouch(t *testing.T) {
	fb := newFakeBacklight(t, 7)
	p := dimParamsFor(t, `{dim_after: 5s}`)
	p.dimAfter = 30 * time.Millisecond
	d := NewDimmer(p)
	defer d.Close()
	fb.waitLevel(1)
	_, km := compileText(t, gadgetConfig)
	g, _ := newGadgets(t)
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	rt.gadgets = g
	e.SetWidgets(rt)
	tp := &touchProc{e: e, dim: d}
	touch := func() {
		tp.event(absEv(evdev.ABS_X, 2*200+100), time.Now()) // カウンターのセル
		tp.event(absEv(evdev.ABS_Y, 80), time.Now())
		tp.event(keyEv(evdev.BTN_TOUCH, 1), time.Now())
		tp.event(synEv(), time.Now())
		tp.event(keyEv(evdev.BTN_TOUCH, 0), time.Now())
		tp.event(synEv(), time.Now())
	}
	touch()
	fb.waitLevel(7)
	if v := g.Snapshot().Counter["counter"].Value; v != 0 {
		t.Fatalf("the waking touch counted: %d", v)
	}
	touch()
	time.Sleep(20 * time.Millisecond)
	if v := g.Snapshot().Counter["counter"].Value; v != 1 {
		t.Errorf("the next touch must count: %d", v)
	}
}

func absEv(code evdev.EvCode, v int32) *evdev.InputEvent {
	return &evdev.InputEvent{Type: evdev.EV_ABS, Code: code, Value: v}
}

func keyEv(code evdev.EvCode, v int32) *evdev.InputEvent {
	return &evdev.InputEvent{Type: evdev.EV_KEY, Code: code, Value: v}
}

func synEv() *evdev.InputEvent {
	return &evdev.InputEvent{Type: evdev.EV_SYN, Code: evdev.SYN_REPORT}
}
