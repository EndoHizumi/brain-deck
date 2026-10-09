package main

import (
	"encoding/json"
	"fmt"
	"image"
	"os"
	"testing"
	"time"
)

// TestHWMonth は、月の表示と follow のカレンダーを実機の画面で押す。▼ で月を送る、日付を選ぶ、帯の中央で今月に戻る、
// page_reset で描画の goroutine が自分で戻す、の描き直しの時間を測り、画面が全体を描いたものと同じになることを確かめる。
// 最後に、1000 件の予定を受け取って SD カードに保存する時間と、calendar.json の大きさを測る
// （保存先は LEFTHAND_HW_SD_DIR。既定 /var/tmp。/tmp は tmpfs なので SD カードではない）。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 90 ./lefthand.test -test.run HWMonth -test.v
func TestHWMonth(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	verbose = true
	cfg, km := compileText(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: month, id: cal, span: [2, 2], page_reset: 5s }
        "2,0": { widget: calendar, follow: cal, span: [2, 2], label: 予定 }
        "0,2": { widget: clock, span: [2, 1] }
        "3,2": B
`)
	cs := NewCalendarService(OpenStore(t.TempDir()))
	now := time.Now()
	today := dayStart(now)
	at := func(day, h int) *time.Time {
		x := today.AddDate(0, 0, day).Add(time.Duration(h) * time.Hour)
		return &x
	}
	ev := func(title string, day, h int) CalEvent {
		return CalEvent{Title: title, Start: at(day, h), End: at(day, h+1)}
	}
	var work, home []CalEvent
	for d := -35; d <= 60; d += 3 {
		work = append(work, ev(fmt.Sprintf("仕事 %d", d), d, 10))
	}
	for d := -30; d <= 60; d += 4 {
		home = append(home, ev(fmt.Sprintf("家 %d", d), d, 19))
	}
	hol := []CalEvent{{Title: "祝日のテスト", Day: today.AddDate(0, 0, 2).Format(calDayLayout)}}
	fetched := now.Add(-time.Minute)
	first := time.Date(today.Year(), today.Month()-1, 1, 0, 0, 0, 0, time.Local)
	if _, _, err := cs.Set(CalendarRequest{From: first.Format(calDayLayout), Days: 90, Calendars: []Calendar{
		{Name: "仕事", FetchedAt: &fetched, Events: work}, {Name: "家", FetchedAt: &fetched, Events: home},
		{Name: "祝日", Holiday: true, FetchedAt: &fetched, Events: hol}}}, now); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	e.SetWidgets(rt)
	env := func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true, Calendar: cs.Snapshot()} }
	rt.SetEnv(env)
	l := buildLayout(km, e.View())
	d, err := StartDisplay(cfg.Display, l, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cs.SetOnChange(d.Poke)
	rt.SetScreen(l.W, l.H, d.Poke)
	g := km.Layers[0].Grid
	m, cal := g.Cells[cellPos{0, 0}].Widget, g.Cells[cellPos{2, 0}].Widget
	// wait は、セル i の描いた中身が want() になるまで待つ
	wait := func(what string, t0 time.Time, i int, want func() string) time.Duration {
		t.Helper()
		for time.Since(t0) < 8*time.Second {
			d.drawMu.Lock()
			ok := d.wkeys[i] == want()
			d.drawMu.Unlock()
			if ok {
				return time.Since(t0)
			}
			time.Sleep(500 * time.Microsecond)
		}
		t.Fatalf("%s: not redrawn", what)
		return 0
	}
	monthDrawn := func(what string, t0 time.Time) time.Duration {
		return wait(what, t0, 0, func() string { return monthKey(m, env()) })
	}
	calDrawn := func(what string, t0 time.Time) time.Duration {
		return wait(what, t0, 2, func() string { return calKey(cal, env()) })
	}
	hwSameAsFull(t, d, env, "month-idle")
	cell := cellRect(0, 0, 2, 2, 4, 3, l.W, l.H)
	pt := func(r image.Rectangle) (int32, int32) { return int32(r.Min.X + r.Dx()/2), int32(r.Min.Y + r.Dy()/2) }
	press := func(what string, r image.Rectangle) time.Time {
		t.Helper()
		x, y := pt(r)
		t0 := time.Now()
		e.PressTouch(x, y)
		// 月の表示を押すと、描画のスレッドがすぐに起きて CPU を取るので、10 ms を超えることがある。
		// 押しても PC には何も送らないので、ここでは記録だけする（キーを待たせないことは、下で確かめる）
		t.Logf("%s: PressTouch returned in %v", what, time.Since(t0))
		return t0
	}
	ml := monthLayout(widgetArea(cell, ""), m, env())
	t0 := press("▼", ml.down)
	t.Logf("next month: drawn in %v", monthDrawn("next", t0))
	e.Release("t")
	t.Logf("▼ released: drawn in %v", monthDrawn("released", time.Now()))
	hwSameAsFull(t, d, env, "month-next")

	// 来月の 15 日を選ぶ。follow のカレンダーにその日の予定が出る
	ml = monthLayout(widgetArea(cell, ""), m, env())
	var target monthDay
	for _, md := range ml.Days {
		if md.In && md.num == 15 {
			target = md
		}
	}
	t0 = press("day", target.rect)
	t.Logf("select %s: month drawn in %v, calendar drawn in %v", target.Day, monthDrawn("select", t0), calDrawn("select", t0))
	e.Release("t")
	if day, ok := calFollowDay(cal, env()); !ok || day.Format(calDayLayout) != target.Day {
		t.Fatalf("follow day = %v %v, want %s", day, ok, target.Day)
	}
	hwSameAsFull(t, d, env, "month-select")

	// 帯の中央で今月と今日へ
	t0 = press("mid", ml.mid)
	t.Logf("back to this month: month drawn in %v, calendar drawn in %v", monthDrawn("mid", t0), calDrawn("mid", t0))
	e.Release("t")
	hwSameAsFull(t, d, env, "month-mid")

	// もう一度送ってから触らずに待つと、5 秒で描画の goroutine が自分で今月に戻す
	t0 = press("▼ again", ml.down)
	e.Release("t")
	monthDrawn("next again", t0)
	reset := m.month.resetAt(m.PageReset, time.Now())
	if reset.IsZero() {
		t.Fatal("no reset time")
	}
	time.Sleep(time.Until(reset))
	if o, _, _ := m.month.view(time.Now(), m.PageReset); o != 0 {
		t.Fatalf("offset after page_reset = %d", o)
	}
	t.Logf("page_reset: redrawn %v after the time", monthDrawn("reset", reset))
	hwSameAsFull(t, d, env, "month-reset")

	// 押したときに入力の goroutine が待つ時間を、キーのセル（3,2）と比べる。押すたびに描き終わるのを待つ
	bcell := cellRect(3, 2, 1, 1, 4, 3, l.W, l.H)
	var keyT, dayT []time.Duration
	for i := 0; i < 8; i++ {
		x, y := pt(bcell)
		t0 := time.Now()
		e.PressTouch(x, y)
		keyT = append(keyT, time.Since(t0))
		e.Release("t")
		time.Sleep(150 * time.Millisecond)
		ml = monthLayout(widgetArea(cell, ""), m, env())
		x, y = pt(ml.Days[10+i].rect)
		t0 = time.Now()
		e.PressTouch(x, y)
		dayT = append(dayT, time.Since(t0))
		monthDrawn("day loop", t0)
		e.Release("t")
		time.Sleep(150 * time.Millisecond)
	}
	t.Logf("PressTouch: key cell %v, month day %v", keyT, dayT)
	// 日付を押した直後（月の表示と follow のカレンダーを描いているあいだ）に、キーのセルを押す。キーは描画を待たない
	var during []time.Duration
	for i := 0; i < 8; i++ {
		ml = monthLayout(widgetArea(cell, ""), m, env())
		x, y := pt(ml.Days[12+i].rect)
		t0 := time.Now()
		e.PressTouch(x, y)
		e.Release("t")
		time.Sleep(5 * time.Millisecond)
		bx, by := pt(bcell)
		t1 := time.Now()
		e.PressTouch(bx, by)
		during = append(during, time.Since(t1))
		e.Release("t")
		monthDrawn("during", t0)
		time.Sleep(150 * time.Millisecond)
	}
	t.Logf("PressTouch of the key cell while the month redraws: %v", during)
	for _, el := range during {
		if el > hwInputLimit {
			t.Errorf("a key press waited %v while the month was redrawn", el)
		}
	}

	// 1000 件の予定を受け取り、SD カードに保存する（set_calendar と同じ）
	dir := os.Getenv("LEFTHAND_HW_SD_DIR")
	if dir == "" {
		dir = "/var/tmp"
	}
	sd, err := os.MkdirTemp(dir, "lefthand-hw-month-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sd)
	big := NewCalendarService(OpenStore(sd))
	big.saved = make(chan struct{}, 1)
	var many [4][]CalEvent
	for i := 0; i < calMaxEvents; i++ {
		day := i%92 - 31
		many[i%4] = append(many[i%4], CalEvent{Title: fmt.Sprintf("予定の名前（会議室と参加者の名前を少し長めに） %d", i), Start: at(day, 8+i%10), End: at(day, 9+i%10),
			Location: "本社 3F 会議室"})
	}
	var cals []Calendar
	for i, evs := range many {
		cals = append(cals, Calendar{Name: fmt.Sprintf("カレンダー%d", i+1), FetchedAt: &fetched, Events: evs})
	}
	req, _ := json.Marshal(map[string]any{"id": 1, "cmd": "set_calendar", "from": first.Format(calDayLayout), "days": 90, "calendars": cals})
	t0 = time.Now()
	if _, _, err := big.Set(CalendarRequest{From: first.Format(calDayLayout), Days: 90, Calendars: cals}, time.Now()); err != nil {
		t.Fatal(err)
	}
	setEl := time.Since(t0)
	select {
	case <-big.saved:
	case <-time.After(30 * time.Second):
		t.Fatal("not saved in 30 s")
	}
	saveEl := time.Since(t0)
	st, err := os.Stat(sd + "/" + calStoreName + ".json")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("1000 events: request %d bytes (limit %d), set %v, saved to %s in %v (%d bytes)", len(req), maxLineBytes, setEl, dir, saveEl, st.Size())
	if len(req) > maxLineBytes {
		t.Errorf("a 1000-event request does not fit in a line")
	}
	// 1000 件のときの月の表示の描き直し
	cs.Set(CalendarRequest{From: first.Format(calDayLayout), Days: 90, Calendars: cals}, time.Now())
	t0 = time.Now()
	t.Logf("month with 1000 events: drawn in %v", monthDrawn("big", t0))
	hwSameAsFull(t, d, env, "month-big")
}

// BenchmarkMonthTouch は、月の表示を押したときに入力の goroutine で行う計算（実機で -test.bench MonthTouch）。
func BenchmarkMonthTouch(b *testing.B) {
	w := &WidgetDef{Kind: widgetMonth, ID: "m", month: &monthState{}, PageReset: time.Minute}
	rt := NewWidgetRT(nil)
	rt.SetEnv(func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true} })
	cell := image.Rect(0, 0, 400, 320)
	for i := 0; i < b.N; i++ {
		w.monthTouch(rt, widgetTouch{cell: cell, pt: image.Pt(200, 150)}, "")
	}
}

// BenchmarkMonthLayout は、1000 件の予定での月の表示の配置（描画の goroutine で行う）。
func BenchmarkMonthLayout(b *testing.B) {
	now := time.Now()
	var evs []CalEvent
	for i := 0; i < calMaxEvents; i++ {
		s := dayStart(now).AddDate(0, 0, i%92-31).Add(time.Duration(8+i%10) * time.Hour)
		e := s.Add(time.Hour)
		evs = append(evs, CalEvent{Title: "x", Start: &s, End: &e})
	}
	d := &CalendarData{Calendars: []Calendar{{Name: "a", Color: "#4f9dff", Events: evs}}}
	d.prepare()
	w := &WidgetDef{Kind: widgetMonth, ID: "m", month: &monthState{}, PageReset: time.Minute}
	env := WidgetEnv{Now: now, TimeSynced: true, Calendar: d}
	for i := 0; i < b.N; i++ {
		monthLayout(image.Rect(12, 12, 388, 308), w, env)
	}
}
