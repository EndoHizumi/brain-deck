package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const gadgetConfig = `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    keys: { KEY_ESC: { layer_to: base } }
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: stopwatch }
        "1,0": { widget: pomodoro, work: 2m, break: 1m }
        "2,0": { widget: counter }
        "3,0": { widget: presence }
        "0,1": { widget: counter, id: cups }
        "1,1": { widget: presence, id: door, states: [{name: 在室, color: "#40c070"}, {name: 不在}] }
        "2,1": { widget: qr, id: share, label: 共有 }
        "3,1": { widget: battery, key: F5 }
`

func TestGadgetWidgetConfig(t *testing.T) {
	_, km := compileText(t, gadgetConfig)
	cells := km.Layers[0].Grid.Cells
	if w := cells[cellPos{0, 0}].Widget; w.Kind != widgetStopwatch || w.ID != "stopwatch" || !w.tapWidget() {
		t.Errorf("stopwatch = %+v", w)
	}
	if w := cells[cellPos{1, 0}].Widget; w.Work != 2*time.Minute || w.Break != time.Minute {
		t.Errorf("pomodoro = %+v", w)
	}
	if w := cells[cellPos{3, 0}].Widget; len(w.States) != 4 || w.States[1].Name != "集中" || w.States[1].Color.hex() != "#ff5858" {
		t.Errorf("default presence states = %v", w.States)
	}
	if w := cells[cellPos{1, 1}].Widget; len(w.States) != 2 || w.States[1].Color.hex() != presencePalette[1] {
		t.Errorf("door states = %v", w.States)
	}
	if a := cells[cellPos{3, 1}]; a.Kind != actKey || a.Widget.Kind != widgetBattery {
		t.Errorf("battery with key = %+v", a)
	}
	if a := cells[cellPos{2, 1}]; a.Kind != actWidget || a.tappable() {
		t.Errorf("qr without key must not be tappable: %+v", a)
	}
	if a := cells[cellPos{2, 0}]; !a.tappable() {
		t.Errorf("counter must be tappable")
	}

	bad := map[string]string{
		`{ widget: counter, key: B }`:                                 "handles taps itself",
		`{ widget: presence, layer_to: base }`:                        "handles taps itself",
		`{ widget: stopwatch, id: "a b" }`:                            "id must be",
		`{ widget: qr }`:                                              "widget: qr needs id",
		`{ widget: clock, id: x }`:                                    "id is for widget",
		`{ widget: counter, work: 25m }`:                              "work and break are for widget: pomodoro",
		`{ widget: pomodoro, work: 10s }`:                             "work must be",
		`{ widget: pomodoro, break: 5h }`:                             "break must be",
		`{ widget: text, id: a, states: [{name: x}] }`:                "states is for widget: presence",
		`{ widget: presence, states: [] }`:                            "states must have",
		`{ widget: presence, states: [{name: a}, {name: a}] }`:        "appears twice",
		`{ widget: presence, states: [{name: a, color: red}] }`:       "color must be #rrggbb",
		`{ widget: presence, states: [{name: ""}] }`:                  "name must be",
		`{ widget: battery, format: "15:04" }`:                        "format, date_format and tz are for widget: clock",
		`{ widget: stopwatch, rows: 3 }`:                              "rows is for widget",
		`{ key: B, states: [{name: a}] }`:                             "states is for widget: presence",
		`{ widget: trackpad, work: 1m }`:                              "cannot be used with widget: trackpad",
		`{ widget: nope }`:                                            "unknown widget",
		`{ widget: presence, states: [{name: "改行\n"}] }`:              "name must",
		`{ widget: pomodoro, work: 25m, break: 5m, label: x, id: p }`: "",
	}
	for cell, want := range bad {
		src := fmt.Sprintf("touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers:\n  - name: base\n    touch:\n      cols: 2\n      rows: 2\n      cells:\n        \"0,0\": %s\n", cell)
		cfg, err := parseConfig([]byte(src))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", cell, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", cell, err, want)
		}
	}
}

func TestGadgetConfigRoundTrip(t *testing.T) {
	cfg, _ := compileText(t, gadgetConfig)
	b, err := marshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := parseConfig(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if !reflect.DeepEqual(cfg.Layers, cfg2.Layers) {
		t.Errorf("round trip changed the layers:\n%s", b)
	}
	if !strings.Contains(string(b), `states: [{name: 在室, color: '#40c070'}, {name: 不在}]`) {
		t.Errorf("states are not written on one line:\n%s", b)
	}
}

func newGadgets(t *testing.T) (*GadgetService, string) {
	t.Helper()
	dir := dataDir(t)
	g := NewGadgetService(OpenStore(dir))
	g.saved = make(chan struct{}, 256)
	return g, dir
}

func waitGadgetsSaved(t *testing.T, g *GadgetService) {
	t.Helper()
	select {
	case <-g.saved:
	case <-time.After(5 * time.Second):
		t.Fatal("widgets.json was not saved")
	}
	for len(g.dirty) > 0 || len(g.saved) > 0 { // まとめて書いたぶんも待つ
		select {
		case <-g.saved:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestStopwatchAndCounter(t *testing.T) {
	g, dir := newGadgets(t)
	g.StopwatchToggle("sw")
	time.Sleep(30 * time.Millisecond)
	s := g.Snapshot().Stopwatch["sw"]
	if s.StartedAt == nil || s.Elapsed(time.Now()) < 30*time.Millisecond {
		t.Fatalf("running stopwatch = %+v", s)
	}
	g.StopwatchToggle("sw")
	s = g.Snapshot().Stopwatch["sw"]
	if s.StartedAt != nil || s.ElapsedMS < 30 {
		t.Fatalf("stopped stopwatch = %+v", s)
	}
	el := s.Elapsed(time.Now())
	time.Sleep(20 * time.Millisecond)
	if g.Snapshot().Stopwatch["sw"].Elapsed(time.Now()) != el {
		t.Error("a stopped stopwatch must not count")
	}
	g.StopwatchReset("sw")
	if s := g.Snapshot().Stopwatch["sw"]; s.ElapsedMS != 0 || s.StartedAt != nil {
		t.Errorf("reset = %+v", s)
	}

	for i := 0; i < 3; i++ {
		g.CounterAdd("c", 1)
	}
	g.CounterAdd("c", -10)
	if v := g.Snapshot().Counter["c"].Value; v != 0 {
		t.Errorf("counter must not go below 0: %d", v)
	}
	g.CounterAdd("c", counterMax+5)
	if v := g.Snapshot().Counter["c"].Value; v != counterMax {
		t.Errorf("counter = %d, want %d", v, counterMax)
	}
	g.CounterReset("c")
	g.CounterAdd("c", 7)
	waitGadgetsSaved(t, g)

	// 保存したものを読み直す（デーモンの再起動）。saved は前の保存の知らせのこともあるので、最後の値が書かれるまで待つ
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var f gadgetFile
		if OpenStore(dir).Load(gadgetStoreName, &f) == nil && f.Counter["c"].Value == 7 {
			break
		}
	}
	g2 := NewGadgetService(OpenStore(dir))
	if v := g2.Snapshot().Counter["c"].Value; v != 7 {
		t.Errorf("counter after restart = %d", v)
	}
}

func TestPomodoroView(t *testing.T) {
	w, b := 25*time.Minute, 5*time.Minute
	t0 := time.Date(2026, 10, 6, 0, 20, 0, 0, time.UTC)
	p := PomodoroState{Running: true, Anchor: &t0, WorkMS: w.Milliseconds(), BreakMS: b.Milliseconds(), Done: 1}
	cases := []struct {
		at     time.Duration
		phase  string
		remain string
		count  int
		alert  bool
		blink  bool
	}{
		{0, pomoPhaseWork, "25:00", 1, false, false},
		{21*time.Minute + 27*time.Second, pomoPhaseWork, "3:33", 1, false, false},
		{25 * time.Minute, pomoPhaseBreak, "5:00", 2, true, true},
		{25*time.Minute + 600*time.Millisecond, pomoPhaseBreak, "5:00", 2, true, false},
		{25*time.Minute + 2*time.Second, pomoPhaseBreak, "4:58", 2, true, true},
		{25*time.Minute + pomoAlert, pomoPhaseBreak, "4:45", 2, false, false},
		{30 * time.Minute, pomoPhaseWork, "25:00", 2, true, true},
		{2*time.Hour + 5*time.Minute, pomoPhaseWork, "20:00", 5, false, false},
	}
	for _, c := range cases {
		v := p.view(t0.Add(c.at), time.Minute)
		if v.Phase != c.phase || pomoText(v.Remaining) != c.remain || v.Count != c.count || v.Alert != c.alert || v.BlinkOn != c.blink || !v.Running {
			t.Errorf("at %v: %+v (remain %s)", c.at, v, pomoText(v.Remaining))
		}
		if !v.Next.After(t0.Add(c.at)) || v.Next.Sub(t0.Add(c.at)) > time.Second {
			t.Errorf("at %v: next %v", c.at, v.Next.Sub(t0.Add(c.at)))
		}
	}
	// 点滅をタップで止めたあと
	ack := t0.Add(25*time.Minute + time.Second)
	p.AckAt = &ack
	if v := p.view(t0.Add(25*time.Minute+2*time.Second), 0); v.Alert {
		t.Error("an acknowledged alert must not blink")
	}
	// 始めていないとき
	if v := (PomodoroState{}).view(t0, 25*time.Minute); v.Started || v.Running || pomoText(v.Remaining) != "25:00" || v.Phase != pomoPhaseWork {
		t.Errorf("idle = %+v", v)
	}
}

func TestPomodoroTapPauseResume(t *testing.T) {
	g, _ := newGadgets(t)
	w, b := 2*time.Minute, time.Minute
	g.PomodoroTap("p", w, b) // 始める
	v := g.Snapshot().Pomodoro["p"].view(time.Now(), w)
	if !v.Running || v.Phase != pomoPhaseWork || v.Remaining > w || v.Remaining < w-time.Second {
		t.Fatalf("started = %+v", v)
	}
	time.Sleep(20 * time.Millisecond)
	g.PomodoroTap("p", w, b) // 一時停止
	p := g.Snapshot().Pomodoro["p"]
	v = p.view(time.Now(), w)
	if v.Running || !v.Started || v.Phase != pomoPhaseWork || p.RemainingMS >= w.Milliseconds() {
		t.Fatalf("paused = %+v %+v", v, p)
	}
	rem := p.RemainingMS
	time.Sleep(20 * time.Millisecond)
	g.PomodoroTap("p", 10*time.Minute, b) // 続ける（セルの時間が変わっても、始めたときの時間のまま）
	p = g.Snapshot().Pomodoro["p"]
	v = p.view(time.Now(), w)
	if !v.Running || p.WorkMS != w.Milliseconds() || v.Remaining.Milliseconds() > rem || v.Remaining.Milliseconds() < rem-200 {
		t.Fatalf("resumed = %+v %+v (rem %d)", v, p, rem)
	}
	// 休憩の途中で止めて、続ける
	a := time.Now().Add(-(w + 10*time.Second)).UTC()
	g.update(func(time.Time) bool {
		g.f.Pomodoro["p"] = PomodoroState{Running: true, Anchor: &a, WorkMS: w.Milliseconds(), BreakMS: b.Milliseconds(), Done: 3}
		return true
	})
	ackNow := time.Now()
	g.PomodoroTap("p", w, b) // 点滅中（切り替わってから 10 秒）：止めるだけ
	p = g.Snapshot().Pomodoro["p"]
	if !p.Running || p.AckAt == nil || p.AckAt.Before(ackNow.Add(-time.Second)) {
		t.Fatalf("tap during the alert must only acknowledge it: %+v", p)
	}
	g.PomodoroTap("p", w, b) // 一時停止
	p = g.Snapshot().Pomodoro["p"]
	if p.Running || p.Phase != pomoPhaseBreak || p.Done != 4 {
		t.Fatalf("paused in break = %+v", p)
	}
	g.PomodoroTap("p", w, b) // 続ける
	v = g.Snapshot().Pomodoro["p"].view(time.Now(), w)
	if !v.Running || v.Phase != pomoPhaseBreak || v.Count != 4 || v.Alert {
		t.Fatalf("resumed in break = %+v", v)
	}
	g.PomodoroReset("p")
	if v := g.Snapshot().Pomodoro["p"].view(time.Now(), w); v.Started || v.Count != 0 {
		t.Errorf("reset = %+v", v)
	}
}

func TestPomodoroAlertWakes(t *testing.T) {
	g, _ := newGadgets(t)
	woke := make(chan struct{}, 4)
	g.SetOnAlert(func() { woke <- struct{}{} })
	a := time.Now().Add(-(2*time.Minute - 50*time.Millisecond)).UTC() // 50 ms で休憩に切り替わる
	g.update(func(time.Time) bool {
		g.f.Pomodoro["p"] = PomodoroState{Running: true, Anchor: &a, WorkMS: (2 * time.Minute).Milliseconds(), BreakMS: time.Minute.Milliseconds()}
		return true
	})
	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("the switch to the break did not call onAlert")
	}
}

func TestGadgetsAfterReboot(t *testing.T) {
	dir := dataDir(t)
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	seen := start.Add(90 * time.Second)
	anchor := start
	f := gadgetFile{BootID: "old-boot", LastSeen: &seen,
		Stopwatch: map[string]StopwatchState{"a": {ElapsedMS: 1000, StartedAt: &start}, "b": {ElapsedMS: 5000}},
		Pomodoro:  map[string]PomodoroState{"p": {Running: true, Anchor: &anchor, WorkMS: 60000, BreakMS: 60000, Done: 2}},
		Counter:   map[string]CounterState{"c": {Value: 4}}}
	if err := OpenStore(dir).Save(gadgetStoreName, f); err != nil {
		t.Fatal(err)
	}
	g := NewGadgetService(OpenStore(dir)) // readBootID は test-boot
	s := g.Snapshot()
	if a := s.Stopwatch["a"]; a.StartedAt != nil || a.ElapsedMS != 91000 {
		t.Errorf("stopwatch after reboot = %+v (want stopped at 91 s)", a)
	}
	if b := s.Stopwatch["b"]; b.ElapsedMS != 5000 {
		t.Errorf("stopped stopwatch = %+v", b)
	}
	if p := s.Pomodoro["p"]; p.Running || p.Phase != pomoPhaseBreak || p.RemainingMS != 30000 || p.Done != 3 {
		t.Errorf("pomodoro after reboot = %+v", p)
	}
	if s.Counter["c"].Value != 4 {
		t.Error("counter lost")
	}

	// 同じ起動のあいだ（デーモンの再起動）は数え続ける
	f.BootID = "test-boot"
	OpenStore(dir).Save(gadgetStoreName, f)
	g = NewGadgetService(OpenStore(dir))
	if a := g.Snapshot().Stopwatch["a"]; a.StartedAt == nil {
		t.Errorf("stopwatch after a daemon restart must keep running: %+v", a)
	}
}

func TestGadgetsClockStepped(t *testing.T) {
	g, _ := newGadgets(t)
	g.StopwatchToggle("sw")
	g.PomodoroTap("p", 2*time.Minute, time.Minute)
	before := g.Snapshot()
	g.ClockStepped(time.Hour)
	after := g.Snapshot()
	if d := after.Stopwatch["sw"].StartedAt.Sub(*before.Stopwatch["sw"].StartedAt); d != time.Hour {
		t.Errorf("stopwatch start moved %v", d)
	}
	if d := after.Pomodoro["p"].Anchor.Sub(*before.Pomodoro["p"].Anchor); d != time.Hour {
		t.Errorf("pomodoro anchor moved %v", d)
	}
}

func TestShutdownWritesLastSeen(t *testing.T) {
	g, dir := newGadgets(t)
	g.StopwatchToggle("sw")
	waitGadgetsSaved(t, g)
	g.Shutdown()
	var f gadgetFile
	if err := OpenStore(dir).Load(gadgetStoreName, &f); err != nil {
		t.Fatal(err)
	}
	if f.LastSeen == nil || f.BootID != "test-boot" || f.Stopwatch["sw"].StartedAt == nil {
		t.Errorf("widgets.json after shutdown = %+v", f)
	}
}

func TestPresenceStep(t *testing.T) {
	g, _ := newGadgets(t)
	st, _ := compilePresenceStates(nil)
	if got := g.PresenceStep("p", st, 1, "test"); got != "在席" {
		t.Errorf("first step = %q", got)
	}
	g.PresenceStep("p", st, 1, "test")
	if got := g.PresenceStep("p", st, -1, "test"); got != "在席" {
		t.Errorf("back = %q", got)
	}
	if got := g.PresenceStep("p", st, -1, "test"); got != "休憩" {
		t.Errorf("wrap backwards = %q", got)
	}
	g.PresenceSet("p", "外出", "test")
	if got := g.PresenceStep("p", st, 1, "test"); got != "在席" {
		t.Errorf("unknown state, next = %q", got)
	}
}

// タップで操作するウィジェットを、エンジンを通して押す（短いタップと長押し）。
func TestTapWidgetsThroughEngine(t *testing.T) {
	old := todoHold
	todoHold = 60 * time.Millisecond
	t.Cleanup(func() { todoHold = old })
	_, km := compileText(t, gadgetConfig)
	g, _ := newGadgets(t)
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	rt.gadgets = g
	e.SetWidgets(rt)
	tap := func(col, row int, hold time.Duration) TouchHit {
		h := e.PressTouch(int32(col*200+100), int32(row*160+80))
		time.Sleep(hold)
		e.Release("t")
		return h
	}
	for i := 0; i < 3; i++ {
		if h := tap(2, 0, 0); !h.Mapped || h.Own {
			t.Fatalf("counter hit = %+v (must light the cell like a key)", h)
		}
	}
	if v := g.Snapshot().Counter["counter"].Value; v != 3 {
		t.Fatalf("counter = %d after 3 taps", v)
	}
	tap(2, 0, todoHold*3)
	if v := g.Snapshot().Counter["counter"].Value; v != 0 {
		t.Fatalf("long press must reset the counter, got %d", v)
	}
	tap(0, 1, 0)
	if v := g.Snapshot().Counter["cups"].Value; v != 1 {
		t.Errorf("cups = %d", v)
	}
	tap(0, 0, 0)
	if s := g.Snapshot().Stopwatch["stopwatch"]; s.StartedAt == nil {
		t.Error("tap must start the stopwatch")
	}
	tap(0, 0, todoHold*3)
	if s := g.Snapshot().Stopwatch["stopwatch"]; s.StartedAt != nil || s.ElapsedMS != 0 {
		t.Errorf("long press must reset the stopwatch: %+v", s)
	}
	tap(1, 1, 0)
	tap(1, 1, 0)
	if s := g.Snapshot().Presence["door"].State; s != "不在" {
		t.Errorf("door = %q", s)
	}
	tap(1, 1, todoHold*3)
	if s := g.Snapshot().Presence["door"].State; s != "在室" {
		t.Errorf("door after long press = %q", s)
	}
	// 押したまま設定を差し替えたら、何もしない
	e.PressTouch(2*200+100, 80)
	e.Reload(km)
	e.Release("t")
	time.Sleep(todoHold * 2)
	if v := g.Snapshot().Counter["counter"].Value; v != 0 {
		t.Errorf("a cancelled tap changed the counter to %d", v)
	}
	waitGadgetsSaved(t, g)
}

func TestGadgetRedrawKeys(t *testing.T) {
	_, km := compileText(t, gadgetConfig)
	now := time.Date(2026, 10, 6, 0, 41, 27, 300e6, time.UTC)
	start := now.Add(-90*time.Second - 300*time.Millisecond)
	env := WidgetEnv{Now: now, TimeSynced: true, Gadgets: gadgetFile{
		Stopwatch: map[string]StopwatchState{"stopwatch": {StartedAt: &start}}}}
	l := buildLayout(km, NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}}).View())
	sw := &l.Cells[0]
	if k := widgetKey(sw, env); !strings.HasPrefix(k, "90\x00true") {
		t.Errorf("stopwatch key = %q", k)
	}
	if n := widgetNext(sw, env); !n.Equal(now.Add(700 * time.Millisecond)) { // 経過 90.3 秒 → 91 秒になる時刻
		t.Errorf("stopwatch next = %v", n.Sub(now))
	}
	if n := widgetNext(&l.Cells[2], env); !n.IsZero() {
		t.Error("a counter never redraws by itself")
	}
	if n := widgetNext(&l.Cells[1], env); !n.IsZero() {
		t.Error("an idle pomodoro never redraws by itself")
	}
	exp := now.Add(time.Minute)
	env.QR = map[string]TextEntry{"share": {Text: "x", ExpiresAt: &exp}}
	if n := widgetNext(&l.Cells[6], env); !n.Equal(exp) {
		t.Errorf("qr next = %v", n)
	}
	fill, ok := widgetFill(&l.Cells[3], env)
	if !ok || fill.hex() != "#40c070" {
		t.Errorf("presence fill = %v %v", fill, ok)
	}
	if contrastInk(fill) != (RGB{}) || contrastInk(RGB{0x30, 0x30, 0x30}) != colText {
		t.Error("contrastInk")
	}
}

func TestQRLimits(t *testing.T) {
	if _, err := EncodeQR(strings.Repeat("a", 3000)); err == nil {
		t.Error("3000 bytes must not fit")
	}
	q, err := EncodeQR(strings.Repeat("あ", qrMaxBytes/3))
	if err != nil || q.Version > 20 {
		t.Errorf("the longest allowed text: %v, version %d", err, q.Version)
	}
	if px, quiet := qrFit(21, 100, 100); px != 3 || quiet != 4 {
		t.Errorf("qrFit small = %d %d", px, quiet)
	}
	if px, quiet := qrFit(97, 200, 200); px != 0 || quiet != 0 {
		t.Errorf("qrFit too big = %d %d", px, quiet)
	}
	if px, quiet := qrFit(57, 125, 125); px != 2 || quiet != 2 {
		t.Errorf("qrFit narrow quiet zone = %d %d", px, quiet)
	}
	if _, err := textLimitsQR.normalize(strings.Repeat("a", qrMaxBytes+1)); err == nil {
		t.Error("too long QR text")
	}
	if s, err := textLimitsQR.normalize("a\tb\r\nc"); err != nil || s != "a\tb\nc" {
		t.Errorf("QR keeps tabs: %q %v", s, err)
	}
}

func TestBatteryLines(t *testing.T) {
	cases := []struct {
		b         *BatteryInfo
		main, sub string
		c         RGB
	}{
		{nil, "--%", "電池を読めません", colSub},
		{&BatteryInfo{Percent: 87, Plugged: true, Charging: true}, "87%", "充電中", colInkOK},
		{&BatteryInfo{Percent: 100, Plugged: true}, "100%", "USB 給電", colText},
		{&BatteryInfo{Percent: 25}, "25%", "電池", colInkWarn},
		{&BatteryInfo{Percent: 10}, "10%", "電池", colInkError},
	}
	for _, c := range cases {
		m, s, col, _ := batteryLines(c.b, colText)
		if m != c.main || s != c.sub || col != c.c {
			t.Errorf("%+v: %q %q %v", c.b, m, s, col)
		}
	}
}

func TestProtocolWidgets(t *testing.T) {
	d := newTestDaemon(t, gadgetConfig)
	cmds := result(t, d.call("hello", nil))["commands"].([]any)
	for _, c := range []string{"set_qr", "get_qr", "get_widgets", "set_presence"} {
		found := false
		for _, x := range cmds {
			found = found || x == c
		}
		if !found {
			t.Errorf("hello does not list %s", c)
		}
	}

	// QR コード：テキストと同じ形
	r := result(t, d.call("set_qr", map[string]any{"name": "share", "text": "https://example.com/", "ttl_sec": 60, "source": "test"}))
	if r["shown"] != true || r["cleared"] != false {
		t.Errorf("set_qr = %v", r)
	}
	r = result(t, d.call("set_qr", map[string]any{"name": "nocell", "text": "x"}))
	if r["shown"] != false {
		t.Errorf("set_qr to an id without a cell = %v", r)
	}
	if m := d.call("set_qr", map[string]any{"name": "share", "text": strings.Repeat("あ", 200)}); errCode(m) != errBadRequest {
		t.Errorf("too long QR text = %v", m)
	}
	r = result(t, d.call("get_qr", nil))
	texts := r["texts"].(map[string]any)
	if texts["share"].(map[string]any)["text"] != "https://example.com/" || fmt.Sprint(r["ids"]) != "[share]" {
		t.Errorf("get_qr = %v", r)
	}
	if r := result(t, d.call("get_text", nil)); len(r["texts"].(map[string]any)) != 0 {
		t.Errorf("QR codes must not be mixed with texts: %v", r)
	}

	// 在席ランプ
	result(t, d.call("subscribe_data", map[string]any{"enable": true}))
	r = result(t, d.call("set_presence", map[string]any{"state": "会議中", "source": "test"}))
	if r["name"] != "presence" || r["state"] != "会議中" || r["color"] != "#ff8020" {
		t.Errorf("set_presence = %v", r)
	}
	// step と、変わったことの通知（返事の前後どちらにも来うる）
	d.write(`{"id":"door","cmd":"set_presence","name":"door","step":1}` + "\n")
	var ev map[string]any
	r = nil
	for i := 0; i < 10 && (ev == nil || r == nil); i++ {
		m := d.readMsgTimeout(2 * time.Second)
		switch {
		case m == nil:
		case m["id"] == "door":
			r = result(t, m)
		case m["event"] == notifyWidgets:
			if p, _ := m["presence"].(map[string]any)["door"].(map[string]any); p != nil {
				ev = m
			}
		}
	}
	if r == nil || r["state"] != "在室" {
		t.Errorf("step = %v", r)
	}
	if ev == nil || ev["presence"].(map[string]any)["door"].(map[string]any)["state"] != "在室" {
		t.Errorf("widgets event = %v", ev)
	}
	if m := d.call("set_presence", map[string]any{"state": "外出"}); errCode(m) != errBadRequest || !strings.Contains(fmt.Sprint(m), "在席, 集中, 会議中, 休憩") {
		t.Errorf("unknown state = %v", m)
	}
	if m := d.call("set_presence", map[string]any{"name": "nope", "state": "在席"}); errCode(m) != errNotFound {
		t.Errorf("unknown id = %v", m)
	}
	if m := d.call("set_presence", map[string]any{"state": "在席", "step": 1}); errCode(m) != errBadRequest {
		t.Errorf("both state and step = %v", m)
	}
	r = result(t, d.call("get_widgets", nil))
	w := r["widgets"].(map[string]any)
	if w["presence"].(map[string]any)["presence"].(map[string]any)["state"] != "会議中" {
		t.Errorf("get_widgets = %v", r)
	}
	ids := r["ids"].(map[string]any)
	if fmt.Sprint(ids["counter"]) != "[counter cups]" || fmt.Sprint(ids["presence"]) != "[door presence]" || fmt.Sprint(ids["qr"]) != "[share]" {
		t.Errorf("ids = %v", ids)
	}
	ps := r["presence_states"].(map[string]any)["door"].([]any)
	if len(ps) != 2 || ps[1].(map[string]any)["name"] != "不在" {
		t.Errorf("presence_states = %v", ps)
	}
	if _, ok := r["battery"]; !ok {
		t.Errorf("get_widgets has no battery: %v", r)
	}

	// 時刻合わせで、QR コードの有効期限も動く
	before := result(t, d.call("get_qr", nil))["texts"].(map[string]any)["share"].(map[string]any)["expires_at"].(string)
	result(t, d.call("set_time", map[string]any{"unix_ms": time.Now().Add(time.Hour).UnixMilli()}))
	after := result(t, d.call("get_qr", nil))["texts"].(map[string]any)["share"].(map[string]any)["expires_at"].(string)
	if before == after {
		t.Error("set_time must shift the expiry of QR codes written before the clock was set")
	}
	st := result(t, d.call("get_status", nil))
	if _, ok := st["battery"]; !ok {
		t.Errorf("get_status has no battery: %v", st)
	}
	if bl, _ := st["backlight"].(map[string]any); bl == nil || bl["enabled"] != false {
		t.Errorf("get_status backlight = %v", st["backlight"])
	}
}

// readMsgTimeout は次の 1 行を読む。d 以内に来なければ nil。
func (d *testDaemon) readMsgTimeout(timeout time.Duration) map[string]any {
	d.t.Helper()
	d.conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := d.rd.ReadBytes('\n')
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		d.t.Fatalf("daemon sent a broken line %q: %v", line, err)
	}
	return m
}
