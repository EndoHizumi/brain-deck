package main

import (
	"os"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- ふたの開け閉め（フェーズ K1） ----------

// ふたを閉じたら画面を消し、タッチを捨て、キーや知らせでも点けない。開けたら明るくして、時間を数え直す。
func TestLidTurnsScreenOff(t *testing.T) {
	fb := newFakeBacklight(t, 6)
	p := dimParamsFor(t, `{dim_after: 5s, off_after: 10s}`)
	p.dimAfter, p.offAfter = 80*time.Millisecond, 300*time.Millisecond
	d := NewDimmer(p)
	defer d.Close()
	l := NewLid(d, lidOff)
	fb.waitLevel(6)
	l.Switch(evdev.SW_LID, 1)
	fb.waitLevel(0)
	if !l.Info().Closed || l.Info().Screen != "off" || !d.Info().Lid {
		t.Errorf("info = %+v %+v", l.Info(), d.Info())
	}
	if !d.TouchDown() {
		t.Error("a touch with the lid closed must be swallowed")
	}
	d.TouchUp()
	d.Key()
	d.Wake()
	d.SetTerminal(true)
	d.SetTerminal(false)
	d.Apply(p)
	time.Sleep(40 * time.Millisecond)
	if fb.level() != 0 {
		t.Fatalf("the screen came on with the lid closed: %d", fb.level())
	}
	// 開ける：明るくし、時間を数え直す（暗くなるまで）
	l.Switch(evdev.SW_LID, 0)
	fb.waitLevel(6)
	if d.TouchDown() {
		t.Error("a touch after opening must pass")
	}
	d.TouchUp()
	fb.waitLevel(1)

	// lid.close: none なら何もしない
	l.SetAction(lidNone)
	d.Wake()
	fb.waitLevel(6)
	l.Switch(evdev.SW_LID, 1)
	time.Sleep(30 * time.Millisecond)
	if fb.level() != 6 || d.Info().Lid {
		t.Errorf("lid.close: none turned the screen off: %d", fb.level())
	}
	// 閉じたまま off にしたら、すぐ消す。none に戻したら点ける
	l.SetAction(lidOff)
	fb.waitLevel(0)
	l.SetAction(lidNone)
	fb.waitLevel(6)
	// タブレットの形は覚えるだけ
	l.Switch(evdev.SW_TABLET_MODE, 1)
	if !l.Info().Tablet {
		t.Error("tablet mode not recorded")
	}
}

// 自動減光を使っていなくても（backlight がない）、ふたを閉じたら消し、開けたら元の明るさに戻す。
func TestLidWithoutBacklightConfig(t *testing.T) {
	fb := newFakeBacklight(t, 4)
	d := NewDimmer(nil)
	defer d.Close()
	l := NewLid(d, lidOff)
	l.Switch(evdev.SW_LID, 1)
	fb.waitLevel(0)
	if !d.TouchDown() {
		t.Error("touch must be swallowed")
	}
	l.Switch(evdev.SW_LID, 0)
	fb.waitLevel(4)
	if d.TouchDown() {
		t.Error("no dimming: touches must pass")
	}
	// 自動減光をやめても、閉じているあいだは消したまま
	p := dimParamsFor(t, `{dim_after: 5s}`)
	d.Apply(p)
	l.Switch(evdev.SW_LID, 1)
	fb.waitLevel(0)
	d.Apply(nil)
	time.Sleep(30 * time.Millisecond)
	if fb.level() != 0 {
		t.Errorf("Apply(nil) with the lid closed turned it on: %d", fb.level())
	}
}

// ふたが閉じているあいだのタッチは、セルを押さない（touchProc）。
func TestLidSwallowsTouchProc(t *testing.T) {
	newFakeBacklight(t, 7)
	d := NewDimmer(nil)
	defer d.Close()
	l := NewLid(d, lidOff)
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
	l.Switch(evdev.SW_LID, 1)
	touch()
	touch()
	time.Sleep(20 * time.Millisecond)
	if v := g.Snapshot().Counter["counter"].Value; v != 0 {
		t.Fatalf("touches with the lid closed counted: %d", v)
	}
	l.Switch(evdev.SW_LID, 0)
	touch()
	time.Sleep(20 * time.Millisecond)
	if v := g.Snapshot().Counter["counter"].Value; v != 1 {
		t.Errorf("a touch after opening must count: %d", v)
	}
}

func TestLidConfig(t *testing.T) {
	for src, want := range map[string]string{
		"lid: {close: off}":   "",
		"lid: {close: none}":  "",
		"lid: {close: sleep}": "lid.close must be off",
		"lid: {}":             "",
	} {
		_, err := checkText(src)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	var c *LidConfig
	if c.action() != lidOff {
		t.Error("default must be off")
	}
}

// checkText は、最小の設定に src を足して -check と同じ検証をする。
func checkText(src string) (*Config, error) {
	cfg, _, _, err := checkConfig([]byte("touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers: [{name: base, touch: {cols: 1, rows: 1}}]\n" + src + "\n"))
	return cfg, err
}

func mustCfg(t *testing.T, src string) *Config {
	t.Helper()
	c, err := checkText(src)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
