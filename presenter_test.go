package main

import (
	"strings"
	"testing"
	"time"
)

func TestCompilePresenter(t *testing.T) {
	w, err := compileWidget(ActionSpec{Widget: widgetPresenter})
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != "presenter" || w.Duration != 10*time.Minute || len(w.Colors) != 2 || w.Colors[0].Remaining != 5*time.Minute ||
		w.Colors[1].Color != (RGB{0xff, 0x58, 0x58}) {
		t.Fatalf("defaults %+v", w)
	}
	// 区切りは書いた順によらず、残りの長い順に並べる。colors: [] なら色を変えない
	w, err = compileWidget(ActionSpec{Widget: widgetPresenter, ID: "lt", Duration: "5m",
		Colors: []PresenterColorSpec{{Remaining: "30s", Color: "#ff0000"}, {Remaining: "1m", Color: "#ffff00"}}})
	if err != nil || w.Colors[0].Remaining != time.Minute {
		t.Fatalf("%+v %v", w, err)
	}
	if w, err := compileWidget(ActionSpec{Widget: widgetPresenter, Colors: []PresenterColorSpec{}}); err != nil || len(w.Colors) != 0 {
		t.Fatalf("empty colors %+v %v", w, err)
	}
	bad := map[string]ActionSpec{
		"duration must be":     {Widget: widgetPresenter, Duration: "5s"},
		"duration must be ":    {Widget: widgetPresenter, Duration: "1m30.5s"},
		"remaining must be":    {Widget: widgetPresenter, Colors: []PresenterColorSpec{{Remaining: "0s", Color: "#ffffff"}}},
		"appears twice":        {Widget: widgetPresenter, Colors: []PresenterColorSpec{{Remaining: "1m", Color: "#ffffff"}, {Remaining: "60s", Color: "#000000"}}},
		"color must be":        {Widget: widgetPresenter, Colors: []PresenterColorSpec{{Remaining: "1m", Color: "red"}}},
		"handles taps itself":  {Widget: widgetPresenter, Key: "F5"},
		"are for widget: pres": {Widget: widgetClock, Duration: "5m"},
	}
	for want, s := range bad {
		if _, err := compileWidget(s); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%+v: %v (want %q)", s, err, want)
		}
	}
}

func TestPresenterView(t *testing.T) {
	w, _ := compileWidget(ActionSpec{Widget: widgetPresenter, ID: "talk", Duration: "10m"})
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	env := func(st StopwatchState) WidgetEnv {
		return WidgetEnv{Now: now, Gadgets: gadgetFile{Presenter: map[string]StopwatchState{"talk": st}}}
	}
	started := func(ago time.Duration) StopwatchState { t := now.Add(-ago); return StopwatchState{StartedAt: &t} }
	v := presenterView(w, env(StopwatchState{}))
	if v.Main != "10:00" || v.Sub != presenterIdle || v.HasColor || v.Running || v.Bar != 0 || !v.Next.IsZero() {
		t.Fatalf("idle %+v", v)
	}
	v = presenterView(w, env(started(3*time.Minute+200*time.Millisecond)))
	if v.Main != "7:00" || v.Sub != "経過 3:00" || v.HasColor || !v.Next.Equal(now.Add(800*time.Millisecond)) || v.Bar != 300 {
		t.Fatalf("running %+v", v)
	}
	// 残り 5 分ちょうどで黄、1 分で赤
	if v = presenterView(w, env(started(5*time.Minute))); !v.HasColor || v.Color != colInkWarn || v.Main != "5:00" {
		t.Fatalf("5m left %+v", v)
	}
	if v = presenterView(w, env(started(5*time.Minute-time.Millisecond))); v.HasColor {
		t.Fatalf("just over 5m left %+v", v)
	}
	if v = presenterView(w, env(started(9*time.Minute+30*time.Second))); v.Color != colInkError || v.Main != "0:30" {
		t.Fatalf("30s left %+v", v)
	}
	// 過ぎたら、超えた時間と、0.5 秒ごとの点滅
	v = presenterView(w, env(started(10*time.Minute+12*time.Second+300*time.Millisecond)))
	if !v.Over || v.Main != "+0:12" || v.Sub != "時間切れ 経過 10:12" || !v.BlinkOn || v.Bar != 1000 || !v.Next.Equal(now.Add(200*time.Millisecond)) {
		t.Fatalf("over %+v", v)
	}
	if v = presenterView(w, env(started(10*time.Minute+12*time.Second+700*time.Millisecond))); v.BlinkOn {
		t.Fatalf("blink off %+v", v)
	}
	// 一時停止：点滅しない、灰色
	v = presenterView(w, env(StopwatchState{ElapsedMS: 11 * 60000}))
	if !v.Over || v.BlinkOn || v.Running || v.Sub != "一時停止 経過 11:00" || !v.Next.IsZero() {
		t.Fatalf("paused over %+v", v)
	}
	cv := &CellView{Widget: w}
	if c, ok := widgetFill(cv, env(started(10*time.Minute+100*time.Millisecond))); !ok || c != colInkError {
		t.Fatal("no blink fill")
	}
}

// プレゼン用タイマーが動いているあいだは、自動減光をしない
func TestPresenterKeepsBacklight(t *testing.T) {
	fb := newFakeBacklight(t, 5)
	p := dimParamsFor(t, `{dim_after: 5s, dim_level: 1}`)
	p.dimAfter = 80 * time.Millisecond
	d := NewDimmer(p)
	defer d.Close()
	g := NewGadgetService(OpenStore(t.TempDir()))
	var busy []bool
	g.SetOnBusy(func(on bool) { busy = append(busy, on); d.SetBusy(on) })
	if len(busy) != 1 || busy[0] {
		t.Fatalf("initial busy %v", busy)
	}
	fb.waitLevel(1)
	g.PresenterToggle("talk") // 始める：明るくして、暗くしない
	fb.waitLevel(5)
	time.Sleep(200 * time.Millisecond)
	if fb.level() != 5 {
		t.Fatal("dimmed while the presenter timer runs")
	}
	g.StopwatchToggle("other") // ほかのタイマーは関係ない
	g.PresenterToggle("talk")  // 止める：そこから数え直す
	fb.waitLevel(1)
	if len(busy) != 3 || !busy[1] || busy[2] {
		t.Fatalf("busy calls %v", busy)
	}
	g.PresenterToggle("talk")
	g.PresenterReset("talk")
	if len(busy) != 5 || busy[4] {
		t.Fatalf("reset should stop: %v", busy)
	}
}

func TestPresenterStopsOnReboot(t *testing.T) {
	dir := t.TempDir()
	g := NewGadgetService(OpenStore(dir))
	g.saved = make(chan struct{}, 4)
	g.PresenterToggle("talk")
	<-g.saved
	g.Shutdown()
	g2 := NewGadgetService(OpenStore(dir))
	g2.bootID = "other"
	var f gadgetFile
	OpenStore(dir).Load(gadgetStoreName, &f)
	f.BootID = "previous-boot"
	g3 := &GadgetService{bootID: "this-boot", f: gadgetFile{Stopwatch: map[string]StopwatchState{}, Pomodoro: map[string]PomodoroState{},
		Counter: map[string]CounterState{}, Presence: map[string]PresenceState{}, Presenter: map[string]StopwatchState{}}}
	g3.restore(f)
	if s := g3.f.Presenter["talk"]; s.StartedAt != nil {
		t.Fatalf("still running after a reboot: %+v", s)
	}
	if s := g2.Snapshot().Presenter["talk"]; s.StartedAt == nil {
		t.Fatalf("the daemon restart should keep it running: %+v", s)
	}
}
