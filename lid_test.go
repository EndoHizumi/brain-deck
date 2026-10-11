package main

import (
	"bytes"
	"fmt"
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
	l := NewLid(d, nil)
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
	l := NewLid(d, nil)
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
	l := NewLid(d, nil)
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
		"lid: {close: off}":        "",
		"lid: {close: none}":       "",
		"lid: {close: sleep}":      "lid.close must be off",
		"lid: {}":                  "",
		"lid: {tablet_keys: on}":   "",
		"lid: {tablet_keys: off}":  "",
		"lid: {tablet_keys: some}": "lid.tablet_keys must be off",
	} {
		_, err := checkText(src)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	var c *LidConfig
	if c.action() != lidOff || c.tabletKeys() != tabletKeysOff {
		t.Error("default must be off")
	}
}

// ---------- フェーズ K3 の前に：タブレットの形と、閉じているあいだの描き直し ----------

// タブレットの形のあいだは本体のキーを捨てる。捨て始める前に押していたキーの離しは通し、捨てているあいだに押したキーは
// 戻したあとの離しも捨てる。lid.tablet_keys: on なら捨てない。
func TestTabletModeDropsKeys(t *testing.T) {
	l := NewLid(nil, nil)
	f := &keyFilter{lid: l}
	k := func(code evdev.EvCode, v int32) *evdev.InputEvent {
		return &evdev.InputEvent{Type: evdev.EV_KEY, Code: code, Value: v}
	}
	if f.drop(k(evdev.KEY_A, 1)) {
		t.Fatal("a key in the normal form must pass")
	}
	l.Switch(evdev.SW_TABLET_MODE, 1)
	if !l.KeysBlocked() || !l.Info().KeysOff {
		t.Fatalf("keys must be blocked in tablet mode: %+v", l.Info())
	}
	if f.drop(k(evdev.KEY_A, 0)) {
		t.Error("the release of a key pressed before tablet mode must pass")
	}
	for _, ev := range []*evdev.InputEvent{k(evdev.KEY_B, 1), k(evdev.KEY_B, 2), k(evdev.KEY_LEFTALT, 1)} {
		if !f.drop(ev) {
			t.Errorf("%v must be dropped in tablet mode", ev)
		}
	}
	if f.drop(&evdev.InputEvent{Type: evdev.EV_SW, Code: evdev.SW_LID, Value: 1}) {
		t.Error("switch events must pass")
	}
	if !f.drop(k(evdev.KEY_B, 0)) {
		t.Error("the release of a dropped key must be dropped")
	}
	l.Switch(evdev.SW_TABLET_MODE, 0)
	if !f.drop(k(evdev.KEY_LEFTALT, 0)) {
		t.Error("the release (after leaving tablet mode) of a key pressed in tablet mode must be dropped")
	}
	if f.drop(k(evdev.KEY_LEFTALT, 1)) || f.drop(k(evdev.KEY_LEFTALT, 0)) {
		t.Error("keys must pass again after leaving tablet mode")
	}
	// lid.tablet_keys: on
	l.SetConfig(&LidConfig{TabletKeys: tabletKeysOn})
	l.Switch(evdev.SW_TABLET_MODE, 1)
	if l.KeysBlocked() || f.drop(k(evdev.KEY_C, 1)) {
		t.Error("lid.tablet_keys: on must keep the keys")
	}
	// 上書き lid.tablet_keys と lid.close は、もう一方を消さない
	c := mustCfg(t, "lid: {close: none, tablet_keys: on}")
	for _, kv := range []struct {
		key string
		v   any
	}{{"lid.tablet_keys", tabletKeysOff}, {"lid.close", lidOff}} {
		overrideKeyOf(kv.key).apply(c, c, kv.v)
	}
	if c.Lid.action() != lidOff || c.Lid.tabletKeys() != tabletKeysOff {
		t.Errorf("lid = %+v", c.Lid)
	}
	c = mustCfg(t, "lid: {close: none, tablet_keys: on}")
	overrideKeyOf("lid.close").apply(c, c, lidOff)
	if c.Lid.tabletKeys() != tabletKeysOn {
		t.Errorf("the lid.close override dropped tablet_keys: %+v", c.Lid)
	}
}

type fakePauser struct{ log []bool }

func (p *fakePauser) SetPaused(on bool) { p.log = append(p.log, on) }

// ふたを閉じたら（lid.close: off）描き直しを止め、開けたら始める。none なら止めない。
func TestLidPausesDisplay(t *testing.T) {
	newFakeBacklight(t, 7)
	d := NewDimmer(nil)
	defer d.Close()
	l := NewLid(d, nil)
	p := &fakePauser{}
	l.SetDisplay(p)
	l.Switch(evdev.SW_LID, 1)
	l.Switch(evdev.SW_LID, 0)
	l.SetAction(lidNone)
	l.Switch(evdev.SW_LID, 1)
	l.SetAction(lidOff) // 閉じたまま off にした
	// SetDisplay、閉じる、開ける、none にする、閉じる（none）、off にする
	if want := []bool{false, true, false, false, false, true}; fmt.Sprint(p.log) != fmt.Sprint(want) {
		t.Errorf("SetPaused calls = %v, want %v", p.log, want)
	}
}

// 描き直しを止めているあいだは、時計が進んでも裏画面を描かない。始めたら全体を描き直して、今の時刻を出す。
func TestDisplayPausedWhileLidClosed(t *testing.T) {
	_, km := compileText(t, widgetConfig)
	tm := time.Date(2026, 10, 6, 9, 5, 7, 0, time.Local)
	env := WidgetEnv{Now: tm, TimeSynced: true}
	d := &Display{cv: NewCanvas(800, 480, 1600, rgb565, 0), env: func() WidgetEnv { return env }}
	l := buildLayout(km, km.view([]int{0}))
	l.W, l.H = 800, 480
	d.layout = l
	d.drawn = make([]bool, len(l.Cells))
	d.want = make([]bool, len(l.Cells))
	d.layoutCols = l.Cols
	d.drawAllLocked(l)
	d.SetPaused(true)
	before := append([]byte(nil), d.cv.pix...)
	env.Now = tm.Add(3 * time.Minute)
	d.SetPressed(0, 3, 1, true) // 押した印も描かない
	if !d.redraw() {
		t.Fatal("redraw stopped")
	}
	if !bytes.Equal(before, d.cv.pix) {
		t.Fatal("drew while paused")
	}
	if !d.wakeAt.IsZero() {
		t.Errorf("wakeAt = %v while paused (must sleep)", d.wakeAt)
	}
	d.SetPressed(0, 3, 1, false)
	d.SetPaused(false)
	d.redraw()
	ref := NewCanvas(800, 480, 1600, rgb565, 0)
	rl := *l
	rl.Env = env
	drawAll(ref, &rl, make([]bool, len(l.Cells)))
	if !bytes.Equal(ref.pix, d.cv.pix) {
		t.Fatal("after resuming, the screen differs from a full redraw at the current time")
	}
	if d.wakeAt.IsZero() {
		t.Error("the clock must be scheduled again after resuming")
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
