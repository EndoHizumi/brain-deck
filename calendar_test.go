package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"strings"
	"testing"
	"time"
)

// inTokyo は、テストのあいだ Brain のタイムゾーンを Asia/Tokyo にする（fixtures は Asia/Tokyo で作った）。
func inTokyo(t *testing.T) {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func loadCalFixture(t *testing.T, name string) *CalendarData {
	t.Helper()
	b, err := os.ReadFile("gui/test/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var d CalendarData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if err := d.prepare(); err != nil {
		t.Fatal(err)
	}
	return &d
}

func jst(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s+"+09:00")
	if err != nil {
		panic(err)
	}
	return t
}

func calWidget(t *testing.T, spec string) *WidgetDef {
	t.Helper()
	_, km := compileText(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": `+spec)
	return km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget
}

func rowsText(rows []calRow) string {
	var s []string
	for _, r := range rows {
		s = append(s, fmt.Sprintf("%d %s %s", r.State, r.Label, r.Title))
	}
	return strings.Join(s, " | ")
}

func TestCalendarRows(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar.json")
	w := calWidget(t, `{ widget: calendar, span: [2, 3] }`)
	cases := []struct{ now, want string }{
		{"2026-10-06T07:00:00", "3 終日 社内イベント週間 | 2 08:30 朝会 | 2 09:30 設計レビュー（ウィジェット） | 2 13:00 1on1 | 2 15:00 歯医者 | 2 16:00 リリース判定会議とその後の打ち上げの段取り | 2 19:00 ジム | 4 明日 10:00 定例"},
		{"2026-10-06T09:41:27", "3 終日 社内イベント週間 | 0 08:30 朝会 | 1 〜10:30 設計レビュー（ウィジェット） | 2 13:00 1on1 | 2 15:00 歯医者 | 2 16:00 リリース判定会議とその後の打ち上げの段取り | 2 19:00 ジム | 4 明日 10:00 定例"},
		{"2026-10-06T10:30:00", "3 終日 社内イベント週間 | 0 08:30 朝会 | 0 09:30 設計レビュー（ウィジェット） | 2 13:00 1on1 | 2 15:00 歯医者 | 2 16:00 リリース判定会議とその後の打ち上げの段取り | 2 19:00 ジム | 4 明日 10:00 定例"},
		{"2026-10-07T10:15:00", "3 終日 社内イベント週間 | 1 〜11:00 定例 | 4 明日 燃えないごみ"},
		{"2026-10-09T12:00:00", "3 終日 社内イベント週間"},
		{"2026-10-10T08:00:00", "5  今日の予定はありません"},
	}
	for _, c := range cases {
		got := rowsText(calRows(calShown(w, d), jst(c.now)))
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.now, got, c.want)
		}
	}
	// 次の予定は、明日でなければ日付
	w2 := calWidget(t, `{ widget: calendar, calendars: [家] }`)
	if got := rowsText(calRows(calShown(w2, d), jst("2026-10-06T21:00:00"))); got != "0 15:00 歯医者 | 0 19:00 ジム | 4 10/8(木) 燃えないごみ" {
		t.Errorf("家 at 21:00: %s", got)
	}
}

func TestCalendarRowsEdges(t *testing.T) {
	inTokyo(t)
	ts := func(s string) *time.Time { x := jst(s); return &x }
	d := &CalendarData{Calendars: []Calendar{{Name: "a", Color: "#ffffff", Events: []CalEvent{
		{Title: "夜勤", Start: ts("2026-10-05T22:00:00"), End: ts("2026-10-06T06:00:00")},   // 前の日から続く
		{Title: "合宿", Start: ts("2026-10-05T12:00:00"), End: ts("2026-10-07T12:00:00")},   // 今日をすべて含む
		{Title: "締め切り", Start: ts("2026-10-06T17:00:00"), End: ts("2026-10-06T17:00:00")}, // 長さ 0
		{Title: "", Start: ts("2026-10-06T20:00:00"), End: ts("2026-10-07T01:00:00")},     // 名前なし、明日まで
	}}}}
	if err := d.prepare(); err != nil {
		t.Fatal(err)
	}
	w := calWidget(t, `{ widget: calendar }`)
	for _, c := range []struct{ now, want string }{
		{"2026-10-06T05:00:00", "3 終日 合宿 | 1 〜06:00 夜勤 | 2 17:00 締め切り | 2 20:00 （名前なし）"},
		{"2026-10-06T17:00:00", "3 終日 合宿 | 0 〜06:00 夜勤 | 2 17:00 締め切り | 2 20:00 （名前なし）"},
		{"2026-10-06T17:00:01", "3 終日 合宿 | 0 〜06:00 夜勤 | 0 17:00 締め切り | 2 20:00 （名前なし）"},
		{"2026-10-06T21:00:00", "3 終日 合宿 | 0 〜06:00 夜勤 | 0 17:00 締め切り | 1 今 （名前なし）"},
	} {
		if got := rowsText(calRows(calShown(w, d), jst(c.now))); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.now, got, c.want)
		}
	}
}

func TestCalendarFooterAndNext(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar.json")
	w := calWidget(t, `{ widget: calendar, stale: 2h }`)
	env := WidgetEnv{Now: jst("2026-10-06T09:41:27"), TimeSynced: true, Calendar: d}
	cals := calShown(w, d)
	if f, warn := calFooter(w, cals, env); f != "更新 09:35" || warn {
		t.Errorf("footer = %q %v", f, warn)
	}
	env.Now = jst("2026-10-06T11:35:00")
	if f, warn := calFooter(w, cals, env); f != "更新 09:35 古い" || !warn {
		t.Errorf("stale footer = %q %v", f, warn)
	}
	env.Now = jst("2026-10-07T08:00:00")
	if f, _ := calFooter(w, cals, env); f != "更新 10/6 09:35 古い" {
		t.Errorf("yesterday footer = %q", f)
	}
	env.TimeSynced = false
	if f, warn := calFooter(w, cals, env); f != unsyncedText || !warn {
		t.Errorf("unsynced footer = %q %v", f, warn)
	}
	stale := loadCalFixture(t, "calendar-stale.json")
	env = WidgetEnv{Now: jst("2026-10-06T09:41:27"), TimeSynced: true, Calendar: stale}
	if f, warn := calFooter(calWidget(t, `{ widget: calendar }`), calShown(w, stale), env); f != "更新 10/5 22:10 古い 失敗 1" || !warn {
		t.Errorf("stale fixture footer = %q %v", f, warn)
	}

	// 次に描き直す時刻：今の予定の終わり、次の始まり、古くなる時刻、日付の変わり目
	env = WidgetEnv{Now: jst("2026-10-06T09:41:27"), TimeSynced: true, Calendar: d}
	for _, c := range []struct{ now, want string }{
		{"2026-10-06T09:41:27", "2026-10-06T10:30:00"},
		{"2026-10-06T10:30:00", "2026-10-06T11:35:00"}, // 09:35 の 2 時間後に古くなる
		{"2026-10-06T19:30:00", "2026-10-06T20:30:00"},
		{"2026-10-06T22:00:00", "2026-10-07T00:00:00"},
	} {
		env.Now = jst(c.now)
		if got := calNext(w, env); !got.Equal(jst(c.want)) {
			t.Errorf("next after %s = %v, want %s", c.now, got, c.want)
		}
	}
}

func TestCalendarWidgetConfig(t *testing.T) {
	w := calWidget(t, `{ widget: calendar, rows: 3, page_reset: 30s, stale: 1h, calendars: [仕事, 家] }`)
	if w.Rows != 3 || w.PageReset != 30*time.Second || w.Stale != time.Hour || strings.Join(w.Calendars, ",") != "仕事,家" || !w.ownsTouch() {
		t.Errorf("widget = %+v", w)
	}
	if w := calWidget(t, `{ widget: calendar }`); w.PageReset != defaultPageReset || w.Stale != defaultCalStale {
		t.Errorf("defaults = %+v", w)
	}
	if w := calWidget(t, `{ widget: todo, page_reset: "off" }`); w.PageReset != 0 {
		t.Errorf("page_reset off = %v", w.PageReset)
	}
	for _, bad := range []string{
		`{ widget: calendar, key: A }`,
		`{ widget: calendar, id: x }`,
		`{ widget: calendar, page_reset: 1s }`,
		`{ widget: calendar, page_reset: soon }`,
		`{ widget: calendar, stale: 10s }`,
		`{ widget: calendar, calendars: [a, a] }`,
		`{ widget: todo, stale: 1h }`,
		`{ widget: text, id: x, page_reset: 1m }`,
		`{ widget: clock, calendars: [a] }`,
		`{ widget: calendar, rows: 21 }`,
	} {
		cfg, err := parseConfig([]byte(`
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": ` + bad))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestCalendarSetKeepsFailed(t *testing.T) {
	cs := NewCalendarService(OpenStore(dataDir(t)))
	cs.saved = make(chan struct{}, 16)
	ts := func(s string) *time.Time { x := jst(s); return &x }
	at := ts("2026-10-06T09:00:00")
	ev := func(title string) CalEvent {
		return CalEvent{Title: title, Start: ts("2026-10-06T10:00:00"), End: ts("2026-10-06T11:00:00")}
	}
	_, d, err := cs.Set(CalendarRequest{Calendars: []Calendar{
		{Name: "仕事", FetchedAt: at, Events: []CalEvent{ev("a"), ev("b")}},
		{Name: "家", Color: "#ABCDEF", FetchedAt: at, Events: []CalEvent{{Title: "休み", Day: "2026-10-07"}}},
		{Name: "古い", FetchedAt: at, Events: []CalEvent{ev("c")}},
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d.Rev != 1 || d.Calendars[0].Color != calPalette[0] || d.Calendars[1].Color != "#abcdef" {
		t.Errorf("first set: %+v", d)
	}
	// 家の取得に失敗：前の予定を残して、失敗を記録する。送られなかった「古い」は消す
	res, d, err := cs.Set(CalendarRequest{Calendars: []Calendar{
		{Name: "仕事", FetchedAt: ts("2026-10-06T09:30:00"), Events: []CalEvent{ev("a2")}},
		{Name: "家", Error: "HTTP 404"},
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Calendars) != 2 || !res[1].Kept || res[1].Events != 1 || d.Calendars[1].Error != "HTTP 404" ||
		d.Calendars[1].FetchedAt == nil || !d.Calendars[1].FetchedAt.Equal(*at) || d.Calendars[0].Events[0].Title != "a2" {
		t.Errorf("second set: res %+v data %+v", res, d)
	}
	// 再起動しても残る
	for f := (CalendarData{}); cs.store.Load(calStoreName, &f) != nil || f.Rev != 2; {
		<-cs.saved
	}
	cs2 := NewCalendarService(cs.store)
	if d2 := cs2.Snapshot(); d2 == nil || d2.Rev != 2 || d2.Calendars[1].Events[0].Title != "休み" || d2.Calendars[1].Events[0].s.IsZero() {
		t.Errorf("after restart: %+v", d2)
	}
	for _, bad := range []CalendarRequest{
		{Calendars: []Calendar{{Name: ""}}},
		{Calendars: []Calendar{{Name: "a"}, {Name: "a"}}},
		{Calendars: []Calendar{{Name: "a", Color: "blue"}}},
		{Calendars: []Calendar{{Name: "a", Events: []CalEvent{{Title: "x"}}}}},
		{Calendars: []Calendar{{Name: "a", Events: []CalEvent{{Title: "x", Day: "2026-13-01"}}}}},
		{Calendars: []Calendar{{Name: "a", Events: []CalEvent{{Title: "x", Day: "2026-10-07", EndDay: "2026-10-07"}}}}},
		{Calendars: []Calendar{{Name: "a", Events: []CalEvent{{Title: "x", Start: ts("2026-10-06T10:00:00"), End: ts("2026-10-06T09:00:00")}}}}},
		{Calendars: []Calendar{{Name: "a", Events: []CalEvent{{Title: "x\x07", Day: "2026-10-07"}}}}},
		{From: "10/6"},
	} {
		if _, _, err := cs.Set(bad, time.Now()); err == nil {
			t.Errorf("%+v should be rejected", bad)
		}
	}
	if cs.Snapshot().Rev != 2 {
		t.Error("a rejected set must not change anything")
	}
	for f := (CalendarData{}); cs.store.Load(calStoreName, &f) != nil || f.Rev != 2; {
		<-cs.saved
	}
}

func TestProtocolCalendar(t *testing.T) {
	d := newTestDaemon(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: calendar, span: [2, 3] }
`)
	hello := result(t, d.call("hello", nil))
	for _, c := range []string{"set_calendar", "get_calendar"} {
		if !strings.Contains(fmt.Sprint(hello["commands"]), c) {
			t.Errorf("hello does not list %s", c)
		}
	}
	if r := result(t, d.call("get_calendar", nil)); r["rev"] != 0.0 || r["shown"] != true || len(r["calendars"].([]any)) != 0 {
		t.Errorf("empty get_calendar = %v", r)
	}
	cals := []map[string]any{{"name": "仕事", "color": "#4f9dff", "fetched_at": "2026-10-06T00:35:00Z", "events": []map[string]any{
		{"title": "朝会", "start": "2026-10-05T23:30:00Z", "end": "2026-10-06T00:00:00Z"},
		{"title": "休み", "day": "2026-10-07"},
	}}}
	r := result(t, d.call("set_calendar", map[string]any{"calendars": cals, "from": "2026-10-06", "days": 7, "source": "test"}))
	if r["rev"] != 1.0 || r["shown"] != true || r["calendars"].([]any)[0].(map[string]any)["events"] != 2.0 {
		t.Errorf("set_calendar = %v", r)
	}
	g := result(t, d.call("get_calendar", nil))
	c0 := g["calendars"].([]any)[0].(map[string]any)
	if g["rev"] != 1.0 || g["days"] != 7.0 || c0["name"] != "仕事" || len(c0["events"].([]any)) != 2 {
		t.Errorf("get_calendar = %v", g)
	}
	for _, p := range []map[string]any{{}, {"calendars": "x"}, {"calendars": []map[string]any{{"name": "a", "color": "red"}}}} {
		if m := d.call("set_calendar", p); m["ok"] != false || m["error"].(map[string]any)["code"] != errBadRequest {
			t.Errorf("set_calendar %v = %v", p, m)
		}
	}
	for f := (CalendarData{}); d.cals.store.Load(calStoreName, &f) != nil || f.Rev != 1; {
		time.Sleep(10 * time.Millisecond)
	}
}

// ページ送りと、しばらく触らなければ戻ること（Todo とカレンダー）。
func TestPageReset(t *testing.T) {
	tt := newTodoTest(t, 10)
	tt.w.PageReset = 150 * time.Millisecond
	g := todoGeometry(tt.area(), 10, 0)
	center := func(r image.Rectangle) image.Point { return image.Pt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2) }
	tt.tap(center(g.down), 0)
	if p, _, _ := tt.view(); p != 1 {
		t.Fatalf("▼: page %d", p)
	}
	if at := tt.w.pager.resetAt(tt.w.PageReset, time.Now()); at.IsZero() || time.Until(at) > tt.w.PageReset {
		t.Fatalf("resetAt = %v", at)
	}
	time.Sleep(100 * time.Millisecond)
	tt.tap(tt.row(0), 0) // 触るたびに延びる
	time.Sleep(100 * time.Millisecond)
	if p, _, _ := tt.view(); p != 1 {
		t.Fatalf("touching should keep the page: %d", p)
	}
	time.Sleep(100 * time.Millisecond)
	if p, _, _ := tt.view(); p != 0 {
		t.Fatalf("after page_reset: page %d, want 0", p)
	}
	if at := tt.w.pager.resetAt(tt.w.PageReset, time.Now()); !at.IsZero() {
		t.Errorf("resetAt after reset = %v", at)
	}
}

func TestCalendarTouchPages(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar.json")
	w := calWidget(t, `{ widget: calendar, span: [2, 3], label: 予定 }`)
	now := jst("2026-10-06T19:30:00") // 19:00 のジムが今。触っていなければ、それがあるページ
	rt := NewWidgetRT(nil)
	rt.SetEnv(func() WidgetEnv { return WidgetEnv{Now: now, TimeSynced: true, Calendar: d} })
	cell := image.Rect(0, 0, 400, 480)
	l := calLayout(widgetArea(cell, "予定"), w, rt.env())
	if l.geom.pages != 2 || l.AutoPage != 1 {
		t.Fatalf("pages %d auto %d", l.geom.pages, l.AutoPage)
	}
	if p, _, _ := w.pager.view(now, w.PageReset, l.AutoPage); p != 1 {
		t.Fatalf("untouched page = %d", p)
	}
	center := func(r image.Rectangle) image.Point { return image.Pt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2) }
	w.touchDown(rt, widgetTouch{cell: cell, pt: center(l.geom.up)}, "予定")
	if p, _, nav := w.pager.view(now, w.PageReset, l.AutoPage); p != 0 || nav != -1 {
		t.Fatalf("▲: page %d nav %d", p, nav)
	}
	w.touchUp(rt)
	// 1 分たつと、自動のページに戻る
	if p, _, _ := w.pager.view(now.Add(defaultPageReset), w.PageReset, l.AutoPage); p != 1 {
		t.Fatalf("after page_reset: %d", p)
	}
	// 予定の行を押しても何もしない（長押しの切り替えはない）
	w.touchDown(rt, widgetTouch{cell: cell, pt: center(l.geom.rows[0])}, "予定")
	if _, held, _ := w.pager.view(now, w.PageReset, l.AutoPage); held != "" {
		t.Errorf("held = %q", held)
	}
	w.touchUp(rt)
}

// カレンダーのセルを描き直す範囲が、全体を描いたものと一致すること。
func TestCalendarWidgetRedraw(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar.json")
	_, km := compileText(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: calendar, span: [2, 3], label: 予定 }
        "2,0": { widget: calendar, span: [2, 1], calendars: [家] }
        "3,2": B
`)
	env := WidgetEnv{Now: jst("2026-10-06T09:41:27"), TimeSynced: true, Calendar: d}
	disp := &Display{cv: NewCanvas(800, 480, 1600, rgb565, 0), env: func() WidgetEnv { return env }}
	l := buildLayout(km, km.view([]int{0}))
	l.W, l.H = 800, 480
	disp.layout, disp.drawn = l, make([]bool, len(l.Cells))
	disp.drawAllLocked(l)
	if !disp.wakeAt.Equal(jst("2026-10-06T10:30:00")) {
		t.Errorf("wakeAt = %v, want the end of the current event", disp.wakeAt)
	}
	check := func(what string, want ...int) {
		t.Helper()
		before := append([]string(nil), disp.wkeys...)
		disp.redrawWidgets()
		ref := NewCanvas(800, 480, 1600, rgb565, 0)
		rl := *l
		rl.Env = env
		drawAll(ref, &rl, disp.drawn)
		if !bytes.Equal(ref.pix, disp.cv.pix) {
			t.Fatalf("%s: differs from a full redraw", what)
		}
		var got []int
		for i := range before {
			if before[i] != disp.wkeys[i] {
				got = append(got, i)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: redrew %v, want %v", what, got, want)
		}
	}
	check("nothing changed")
	env.Now = jst("2026-10-06T09:59:00")
	check("a minute later")
	env.Now = jst("2026-10-06T10:30:00")
	check("review ended", 0) // 家のセルは変わらない
	env.Now = jst("2026-10-06T15:00:00")
	check("dentist", 0, 2)
}

// GUI（gui/src/calwidget.ts）が Go と同じ配置にすることを確かめるための表。
// 配置を変えたら LEFTHAND_UPDATE_CALLAYOUT=1 go test -run CalLayoutTable で書き直す。
const guiCalLayoutJSON = "gui/test/fixtures/callayout.json"

type calLayoutCase struct {
	Fixture   string   `json:"fixture"`
	Now       string   `json:"now"`
	Synced    bool     `json:"synced"`
	Area      [4]int   `json:"area"`
	Rows      int      `json:"rows"`
	Calendars []string `json:"calendars,omitempty"`
	Stale     string   `json:"stale,omitempty"`
	Layout    calLay   `json:"layout"`
	Per       int      `json:"per"`
	Pages     int      `json:"pages"`
	FooterY   int      `json:"footer_y"`
	Next      string   `json:"next"`
}

func TestCalLayoutTable(t *testing.T) {
	inTokyo(t)
	var cases []calLayoutCase
	areas := []image.Rectangle{image.Rect(12, 40, 388, 468), image.Rect(212, 28, 588, 148), image.Rect(212, 12, 388, 148), image.Rect(0, 0, 100, 30)}
	for _, fx := range []string{"calendar.json", "calendar-stale.json", "none"} {
		var d *CalendarData
		if fx != "none" {
			d = loadCalFixture(t, fx)
		}
		for _, now := range []string{"2026-10-06T07:00:00", "2026-10-06T09:41:27", "2026-10-06T19:30:00", "2026-10-06T23:59:59",
			"2026-10-07T10:15:00", "2026-10-10T08:00:00"} {
			for _, a := range areas {
				for _, v := range []struct {
					rows   int
					cals   []string
					stale  string
					synced bool
				}{{0, nil, "", true}, {3, nil, "", true}, {0, []string{"家"}, "", true}, {0, nil, "1h", true}, {0, nil, "", false}} {
					spec := fmt.Sprintf(`{ widget: calendar, rows: %d }`, v.rows)
					if v.rows == 0 {
						spec = `{ widget: calendar }`
					}
					w := calWidget(t, spec)
					w.Calendars = v.cals
					if v.stale != "" {
						w.Stale, _ = time.ParseDuration(v.stale)
					}
					env := WidgetEnv{Now: jst(now), TimeSynced: v.synced, Calendar: d}
					l := calLayout(a, w, env)
					cases = append(cases, calLayoutCase{Fixture: fx, Now: now + "+09:00", Synced: v.synced, Area: rect4(a), Rows: v.rows,
						Calendars: v.cals, Stale: v.stale, Layout: l, Per: l.geom.per, Pages: l.geom.pages, FooterY: l.footerY,
						Next: calNext(w, env).Format(time.RFC3339)})
				}
			}
		}
	}
	var buf bytes.Buffer // 1 行に 1 つ
	buf.WriteString("[\n")
	for i, c := range cases {
		b, _ := json.Marshal(c)
		buf.Write(b)
		if i < len(cases)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("]\n")
	want := buf.Bytes()
	if os.Getenv("LEFTHAND_UPDATE_CALLAYOUT") == "1" {
		if err := os.WriteFile(guiCalLayoutJSON, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(guiCalLayoutJSON); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date (%v); run LEFTHAND_UPDATE_CALLAYOUT=1 go test -run CalLayoutTable", guiCalLayoutJSON, err)
	}
}

func TestTodoCaption(t *testing.T) {
	items := []TodoItem{{ID: "t1", Done: true}, {ID: "t2"}, {ID: "t3"}}
	for _, c := range []struct {
		label string
		items []TodoItem
		want  string
	}{{"Todo", items, "Todo 残り 2"}, {"", items, "残り 2"}, {"Todo", nil, "Todo"}, {"Todo", items[:1], "Todo すべて完了"}} {
		if got := todoCaption(c.label, c.items); got != c.want {
			t.Errorf("todoCaption(%q, %d items) = %q, want %q", c.label, len(c.items), got, c.want)
		}
	}
}
