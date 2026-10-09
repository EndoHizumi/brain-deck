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

const monthGrid = `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
`

func monthWidget(t *testing.T, spec string) *WidgetDef {
	t.Helper()
	_, km := compileText(t, monthGrid+`        "0,0": `+spec)
	return km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget
}

// monthArea は、4×3 の格子の 2×2 のセル（見出しなし）の、見出しの下の範囲。
var monthCell = image.Rect(0, 0, 400, 320)

func monthDays(l monthLay) string {
	var s []string
	for _, d := range l.Days {
		mark := ""
		if !d.In {
			mark = "-"
		}
		s = append(s, mark+strings.TrimLeft(d.Day[8:], "0"))
	}
	return strings.Join(s, " ")
}

func center(r image.Rectangle) image.Point { return image.Pt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2) }

func TestMonthGrid(t *testing.T) {
	inTokyo(t)
	area := widgetArea(monthCell, "")
	for _, c := range []struct {
		now, spec string
		weeks     int
		first     string // 格子の最初の日
		last      string
		wdays     string
	}{
		// 2026 年 10 月 1 日は木曜。日曜から：9/27〜10/31 の 5 週
		{"2026-10-06T09:00:00", `{ widget: month, span: [2, 2] }`, 5, "2026-09-27", "2026-10-31", "[0 1 2 3 4 5 6]"},
		// 月曜から：9/28〜11/1 の 5 週
		{"2026-10-06T09:00:00", `{ widget: month, span: [2, 2], week_start: monday }`, 5, "2026-09-28", "2026-11-01", "[1 2 3 4 5 6 0]"},
		// 2026 年 2 月は日曜から始まる 28 日。日曜からならちょうど 4 週、月曜からなら 5 週
		{"2026-02-10T09:00:00", `{ widget: month, span: [2, 2] }`, 4, "2026-02-01", "2026-02-28", "[0 1 2 3 4 5 6]"},
		{"2026-02-10T09:00:00", `{ widget: month, span: [2, 2], week_start: monday }`, 5, "2026-01-26", "2026-03-01", "[1 2 3 4 5 6 0]"},
		// うるう年：2028 年 2 月は 29 日（火曜から）
		{"2028-02-29T23:59:59", `{ widget: month, span: [2, 2] }`, 5, "2028-01-30", "2028-03-04", "[0 1 2 3 4 5 6]"},
		// 6 週になる月：2026 年 8 月（土曜から 31 日）
		{"2026-08-01T00:00:00", `{ widget: month, span: [2, 2] }`, 6, "2026-07-26", "2026-09-05", "[0 1 2 3 4 5 6]"},
		// 年をまたぐ週：2026 年 12 月
		{"2026-12-31T12:00:00", `{ widget: month, span: [2, 2], week_start: monday }`, 5, "2026-11-30", "2027-01-03", "[1 2 3 4 5 6 0]"},
	} {
		w := monthWidget(t, c.spec)
		l := monthLayout(area, w, WidgetEnv{Now: jst(c.now), TimeSynced: true})
		if l.Weeks != c.weeks || len(l.Days) != c.weeks*7 || l.Days[0].Day != c.first || l.Days[len(l.Days)-1].Day != c.last || fmt.Sprint(l.Wdays) != c.wdays {
			t.Errorf("%s %s: weeks %d %s..%s wdays %v\n%s", c.now, c.spec, l.Weeks, l.Days[0].Day, l.Days[len(l.Days)-1].Day, l.Wdays, monthDays(l))
			continue
		}
		in, today := 0, ""
		for i, d := range l.Days {
			if d.In {
				in++
			}
			if d.Today {
				today = d.Day
			}
			// 曜日の列と、日付の曜日が合う
			day, _ := parseCalDay(d.Day)
			if int(day.Weekday()) != l.Wdays[i%7] {
				t.Errorf("%s: %s is in the %d column", c.now, d.Day, l.Wdays[i%7])
			}
			if d.Sat != (day.Weekday() == time.Saturday) || (d.Red && day.Weekday() != time.Sunday && d.Holiday == "") {
				t.Errorf("%s: colors of %s: %+v", c.now, d.Day, d)
			}
		}
		if want := jst(c.now).AddDate(0, 1, -jst(c.now).Day()).Day(); in != want {
			t.Errorf("%s: %d days in the month, want %d", c.now, in, want)
		}
		if today != c.now[:10] {
			t.Errorf("%s: today = %q", c.now, today)
		}
	}
	// 年と月の見出し
	w := monthWidget(t, `{ widget: month, span: [2, 2] }`)
	if l := monthLayout(area, w, WidgetEnv{Now: jst("2026-10-06T09:00:00"), TimeSynced: true}); l.Title != "2026年10月" || l.TitleS != 2 || l.NumS != 2 {
		t.Errorf("title %q scale %d num %d", l.Title, l.TitleS, l.NumS)
	}
}

func dayOf(l monthLay, day string) monthDay {
	for _, d := range l.Days {
		if d.Day == day {
			return d
		}
	}
	return monthDay{Day: "missing " + day}
}

func TestMonthHolidaysAndDots(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar-month.json")
	area := widgetArea(monthCell, "")
	env := WidgetEnv{Now: jst("2026-10-06T09:41:27"), TimeSynced: true, Calendar: d}
	w := monthWidget(t, `{ widget: month, span: [2, 2] }`)
	l := monthLayout(area, w, env)
	for _, c := range []struct {
		day  string
		want string // red sat holiday dots
	}{
		{"2026-10-12", "true false スポーツの日 []"},                   // 祝日（月曜）：赤、名前、点なし（祝日のカレンダーには付けない）
		{"2026-10-11", "true false  []"},                         // 日曜
		{"2026-10-10", "false true  [#50d880]"},                  // 土曜：家（運動会）。社内イベント週間は end_day の前の日（10/9）まで
		{"2026-10-06", "false false  [#4f9dff #50d880]"},         // 今日
		{"2026-10-17", "false true  [#4f9dff #50d880 more]"},     // 4 つのカレンダー：2 つの色と「ほかにもある」
		{"2026-10-21", "false false  [#4f9dff #ff8a50 #c080ff]"}, // 仕事、趣味、学校（3 つまでは色）
		{"2026-10-07", "false false  [#4f9dff #ff8a50]"},         // 仕事と趣味（並びはカレンダーの順）
		{"2026-10-22", "false false  [#4f9dff]"},                 // 23 時から 3 時間：次の日にもかかる
		{"2026-10-23", "false false  [#4f9dff]"},                 //
		{"2026-10-31", "false true  [#50d880]"},                  // 旅行（10/31〜11/2 の終日）
		{"2026-09-30", "false false  [#4f9dff]"},                 // 先月
		{"2026-10-08", "false false  [#4f9dff]"},                 // 社内イベント週間（10/5〜10/9）
		{"2026-10-13", "false false  []"},                        //
		{"2026-10-16", "false false  [#4f9dff]"},                 // リリース
	} {
		md := dayOf(l, c.day)
		if got := fmt.Sprintf("%v %v %s %v", md.Red, md.Sat, md.Holiday, md.Dots); got != c.want {
			t.Errorf("%s: %s, want %s", c.day, got, c.want)
		}
	}
	if md := dayOf(l, "2026-10-06"); !md.Today || md.Sel {
		t.Errorf("today: %+v", md)
	}
	if l.Info != "10/6(火) 予定5" || l.Status != "" {
		t.Errorf("info %q status %q", l.Info, l.Status)
	}

	// calendars で選ぶ。祝日の赤は calendars によらず出す
	w = monthWidget(t, `{ widget: month, span: [2, 2], calendars: [家] }`)
	l = monthLayout(area, w, env)
	if md := dayOf(l, "2026-10-17"); fmt.Sprint(md.Dots) != "[#50d880]" {
		t.Errorf("calendars: [家]: %v", md.Dots)
	}
	if md := dayOf(l, "2026-10-12"); !md.Red || md.Holiday != "スポーツの日" {
		t.Errorf("holiday with calendars: %+v", md)
	}
	if l.Info != "10/6(火) 予定1" {
		t.Errorf("info %q", l.Info)
	}
	// 祝日のカレンダーを calendars に書いても、点は付けない
	w = monthWidget(t, `{ widget: month, span: [2, 2], calendars: [日本の祝日] }`)
	if md := dayOf(monthLayout(area, w, env), "2026-10-12"); len(md.Dots) != 0 || !md.Red {
		t.Errorf("holiday calendar in calendars: %+v", md)
	}

	// 先月：敬老の日、国民の休日、秋分の日
	w = monthWidget(t, `{ widget: month, span: [2, 2] }`)
	w.month.set(-1, "2026-09-22", env.Now)
	l = monthLayout(area, w, env)
	if l.Title != "2026年9月" || l.Info != "9/22(火) 国民の休日" || !dayOf(l, "2026-09-21").Red || !dayOf(l, "2026-09-22").Sel {
		t.Errorf("september: %q %q %+v", l.Title, l.Info, dayOf(l, "2026-09-22"))
	}
	// 送られてきた範囲（9/1〜11/30）の外
	w.month.set(2, "", env.Now)
	if l = monthLayout(area, w, env); l.Title != "2026年12月" || l.Status != monthOutRange || l.Info != "10/6(火) 予定5" {
		t.Errorf("december: %q %q %q", l.Title, l.Status, l.Info)
	}
	w.month.set(2, "2026-12-01", env.Now)
	if l = monthLayout(area, w, env); l.Info != "12/1(火) 範囲外" {
		t.Errorf("december 1: %q", l.Info)
	}
	w.month.set(-2, "", env.Now)
	if l = monthLayout(area, w, env); l.Status != monthOutRange {
		t.Errorf("august: %q", l.Status)
	}
	// 一部だけ入る：今の版の前の送り方（今日から 7 日先まで）
	old := *d
	old.From, old.Days = "2026-10-06", 7
	w.month.set(0, "2026-10-14", env.Now)
	if l = monthLayout(area, w, WidgetEnv{Now: env.Now, TimeSynced: true, Calendar: &old}); l.Status != monthPartRange || l.Info != "10/14(水) 範囲外" {
		t.Errorf("old range: %q %q", l.Status, l.Info)
	}
	w.month.set(0, "2026-10-13", env.Now)
	if l = monthLayout(area, w, WidgetEnv{Now: env.Now, TimeSynced: true, Calendar: &old}); l.Info != "10/13(火) 予定なし" {
		t.Errorf("old range, in: %q", l.Info)
	}
	// 予定を受け取っていない、時刻を合わせていない
	w.month.set(0, "", env.Now)
	if l = monthLayout(area, w, WidgetEnv{Now: env.Now, TimeSynced: false}); l.Info != "10/6(火) "+monthNoData || l.Status != unsyncedText || !l.StatusWarn || !l.TitleWarn {
		t.Errorf("no data, unsynced: %q %q %v %v", l.Info, l.Status, l.StatusWarn, l.TitleWarn)
	}
}

// 予定の印を付ける日：時刻の予定が 0 時ちょうどに終わるなら次の日には付けない、長さ 0 の予定はその日、何日もの予定は毎日。
func TestMonthEventDays(t *testing.T) {
	inTokyo(t)
	ev := func(s, e string) *CalEvent {
		a, b := jst(s), jst(e)
		return &CalEvent{Start: &a, End: &b, s: a, e: b}
	}
	day := func(a, b string) *CalEvent {
		x, _ := parseCalDay(a)
		y, _ := parseCalDay(b)
		return &CalEvent{Day: a, EndDay: b, s: x, e: y}
	}
	from, _ := parseCalDay("2026-10-01")
	to, _ := parseCalDay("2026-10-11")
	for _, c := range []struct {
		ev   *CalEvent
		want string
	}{
		{ev("2026-10-05T22:00:00", "2026-10-06T00:00:00"), "5"},
		{ev("2026-10-05T22:00:00", "2026-10-06T00:00:01"), "5 6"},
		{ev("2026-10-05T00:00:00", "2026-10-05T00:00:00"), "5"},
		{ev("2026-09-29T10:00:00", "2026-10-02T10:00:00"), "1 2"},  // 範囲の前から
		{ev("2026-10-09T10:00:00", "2026-10-20T10:00:00"), "9 10"}, // 範囲の後まで
		{day("2026-10-03", "2026-10-06"), "3 4 5"},
		{day("2026-10-11", "2026-10-12"), ""},
	} {
		var got []string
		eachDay(c.ev, from, to, func(d time.Time) { got = append(got, fmt.Sprint(d.Day())) })
		if strings.Join(got, " ") != c.want {
			t.Errorf("%v..%v: %v, want %s", c.ev.s, c.ev.e, got, c.want)
		}
	}
}

func TestMonthTouch(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar-month.json")
	w := monthWidget(t, `{ widget: month, span: [2, 2], page_reset: 30s }`)
	now := jst("2026-10-06T09:41:27")
	rt := NewWidgetRT(nil)
	rt.SetEnv(func() WidgetEnv { return WidgetEnv{Now: now, TimeSynced: true, Calendar: d} })
	l := monthLayout(widgetArea(monthCell, ""), w, rt.env())
	tap := func(p image.Point) {
		w.touchDown(rt, widgetTouch{cell: monthCell, pt: p}, "")
	}
	view := func() string {
		o, s, n := w.month.view(now, w.PageReset)
		return fmt.Sprintf("%d %q %d", o, s, n)
	}
	tap(center(l.down))
	if v := view(); v != `1 "" 1` {
		t.Fatalf("▼: %s", v)
	}
	w.touchUp(rt, false)
	if l2 := monthLayout(widgetArea(monthCell, ""), w, rt.env()); l2.Title != "2026年11月" || l2.Info != "10/6(火) 予定5" {
		t.Fatalf("november: %q %q", l2.Title, l2.Info)
	}
	tap(center(l.up))
	tap(center(l.up))
	if v := view(); v != `-1 "" -1` {
		t.Fatalf("▲▲: %s", v)
	}
	w.touchUp(rt, false)
	// 9 月の格子で日付を押す（格子は 9/27 から始まる 10 月のものと違う）
	l = monthLayout(widgetArea(monthCell, ""), w, rt.env())
	tap(center(dayOf(l, "2026-09-23").rect))
	if v := view(); v != `-1 "2026-09-23" 0` {
		t.Fatalf("select: %s", v)
	}
	w.touchUp(rt, false)
	// 30 秒たつと、今月と今日に戻る。それまでに触れば延びる
	if at := w.month.resetAt(w.PageReset, now); !at.Equal(now.Add(30 * time.Second)) {
		t.Errorf("resetAt = %v", at)
	}
	if o, s, _ := w.month.view(now.Add(30*time.Second), w.PageReset); o != 0 || s != "" {
		t.Errorf("after page_reset: %d %q", o, s)
	}
	if at := monthNext(w, WidgetEnv{Now: now}); !at.Equal(now.Add(30 * time.Second)) {
		t.Errorf("monthNext = %v", at)
	}
	// 戻ったあとに触ると、今月から
	now = now.Add(31 * time.Second)
	l = monthLayout(widgetArea(monthCell, ""), w, rt.env())
	tap(center(l.down))
	if v := view(); v != `1 "" 1` {
		t.Fatalf("▼ after reset: %s", v)
	}
	w.touchUp(rt, false)
	// 帯の中央で、すぐに今月と今日へ
	tap(center(l.mid))
	if v := view(); v != `0 "" 0` {
		t.Fatalf("mid: %s", v)
	}
	// 今日を押すと、選んでいない（今日）と同じ
	l = monthLayout(widgetArea(monthCell, ""), w, rt.env())
	tap(center(dayOf(l, "2026-10-08").rect))
	tap(center(dayOf(l, "2026-10-06").rect))
	if v := view(); v != `0 "" 0` {
		t.Fatalf("today: %s", v)
	}
	// 送れるのは前後 24 か月まで
	for i := 0; i < 30; i++ {
		tap(center(l.down))
	}
	if o, _, _ := w.month.view(now, w.PageReset); o != monthMaxOffset {
		t.Errorf("offset = %d", o)
	}
	w.touchUp(rt, false)
	// page_reset: off なら戻らない
	w = monthWidget(t, `{ widget: month, span: [2, 2], page_reset: off }`)
	w.touchDown(rt, widgetTouch{cell: monthCell, pt: center(l.down)}, "")
	if o, _, _ := w.month.view(now.Add(24*time.Hour), w.PageReset); o != 1 {
		t.Errorf("page_reset off: %d", o)
	}
	if at := w.month.resetAt(w.PageReset, now); !at.IsZero() {
		t.Errorf("resetAt with off = %v", at)
	}
	// 日付が変わると描き直す
	if at := monthNext(w, WidgetEnv{Now: now}); !at.Equal(jst("2026-10-07T00:00:00")) {
		t.Errorf("monthNext = %v", at)
	}
}

const followCfg = monthGrid + `        "0,0": { widget: month, id: cal, span: [2, 2] }
        "2,0": { widget: calendar, follow: cal, span: [2, 2], label: 予定 }
        "0,2": { widget: calendar, span: [2, 1] }
        "3,2": B
`

func TestMonthFollow(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar-month.json")
	_, km := compileText(t, followCfg)
	g := km.Layers[0].Grid
	m, cal, plain := g.Cells[cellPos{0, 0}].Widget, g.Cells[cellPos{2, 0}].Widget, g.Cells[cellPos{0, 2}].Widget
	if cal.follow != m || plain.follow != nil {
		t.Fatalf("follow not linked")
	}
	now := jst("2026-10-06T09:41:27")
	env := WidgetEnv{Now: now, TimeSynced: true, Calendar: d}
	area := image.Rect(412, 40, 788, 308)
	today := rowsText(calLayout(area, cal, env).Rows)
	if widgetLabel(cal, "予定", env) != "予定" {
		t.Errorf("label without selection")
	}
	m.month.set(1, "2026-10-17", now)
	l := calLayout(area, cal, env)
	if got := rowsText(l.Rows); got != "2 10:00 保護者会 | 2 11:00 買い物 | 2 13:00 休日出勤 | 2 18:00 ライブ" {
		t.Errorf("selected day rows: %s", got)
	}
	if l.AutoPage != 0 {
		t.Errorf("auto page %d", l.AutoPage)
	}
	if got := widgetLabel(cal, "予定", env); got != "予定 10/17(土)" {
		t.Errorf("label %q", got)
	}
	if got := widgetLabel(cal, "", env); got != "10/17(土)" {
		t.Errorf("label without label %q", got)
	}
	// follow のないセルは今日のまま
	if rowsText(calLayout(area, plain, env).Rows) != today {
		t.Errorf("plain calendar changed")
	}
	// 過ぎた日は薄く、終日を先に。祝日も行に出る
	m.month.set(0, "2026-10-05", now)
	if got := rowsText(calLayout(area, cal, env).Rows); got != "0 終日 社内イベント週間" {
		t.Errorf("past day: %s", got)
	}
	m.month.set(0, "2026-10-12", now)
	if got := rowsText(calLayout(area, cal, env).Rows); got != "3 終日 スポーツの日" {
		t.Errorf("holiday: %s", got)
	}
	m.month.set(0, "2026-10-22", now) // 23 時から次の日まで
	if got := rowsText(calLayout(area, cal, env).Rows); got != "2 23:00 夜間メンテナンス" {
		t.Errorf("overnight: %s", got)
	}
	m.month.set(0, "2026-10-23", now)
	if got := rowsText(calLayout(area, cal, env).Rows); got != "2 〜02:00 夜間メンテナンス" {
		t.Errorf("overnight next day: %s", got)
	}
	m.month.set(0, "2026-10-13", now)
	if got := rowsText(calLayout(area, cal, env).Rows); got != "5  "+calNoneDay {
		t.Errorf("no events: %s", got)
	}
	// 今日を選んでいるのは、選んでいないのと同じ
	m.month.set(0, "2026-10-06", now)
	if rowsText(calLayout(area, cal, env).Rows) != today || widgetLabel(cal, "予定", env) != "予定" {
		t.Errorf("today selected")
	}
	// 月の表示が戻る時刻に、カレンダーも描き直す
	m.month.set(0, "2026-10-17", now)
	if at := calNext(cal, env); !at.Equal(now.Add(defaultPageReset)) {
		t.Errorf("calNext = %v", at)
	}
	if rowsText(calLayout(area, cal, WidgetEnv{Now: now.Add(defaultPageReset), TimeSynced: true, Calendar: d}).Rows) != today {
		t.Errorf("after reset: not today's rows")
	}
	if got := widgetLabel(cal, "予定", WidgetEnv{Now: now.Add(defaultPageReset)}); got != "予定" {
		t.Errorf("after reset: %q", got)
	}
}

// 月の表示で日付を選ぶと、月の表示と follow のカレンダーだけを描き直し、全体を描いたものと一致すること。
func TestMonthWidgetRedraw(t *testing.T) {
	inTokyo(t)
	d := loadCalFixture(t, "calendar-month.json")
	_, km := compileText(t, followCfg)
	now := jst("2026-10-06T09:41:27")
	env := WidgetEnv{Now: now, TimeSynced: true, Calendar: d}
	disp := &Display{cv: NewCanvas(800, 480, 1600, rgb565, 0), env: func() WidgetEnv { return env }}
	l := buildLayout(km, km.view([]int{0}))
	l.W, l.H = 800, 480
	disp.layout, disp.drawn = l, make([]bool, len(l.Cells))
	disp.drawAllLocked(l)
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
	m := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget
	m.month.set(0, "2026-10-17", now)
	check("select", 0, 2)
	m.month.set(1, "2026-10-17", now)
	check("next month", 0)
	env.Now = now.Add(defaultPageReset)
	check("reset", 0, 2)
	env.Now = jst("2026-10-07T00:00:00")
	check("midnight", 0, 2, 8)
}

func TestMonthConfig(t *testing.T) {
	bad := func(cells, want string) {
		t.Helper()
		cfg, err := parseConfig([]byte(monthGrid + cells))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", strings.TrimSpace(cells), err, want)
		}
	}
	bad(`        "0,0": { widget: month, span: [2, 2], week_start: tuesday }`, "week_start must be sunday or monday")
	bad(`        "0,0": { widget: month, span: [2, 2], key: A }`, "widget: month handles taps itself")
	bad(`        "0,0": { widget: month, span: [2, 2], rows: 3 }`, "rows is for widget: todo and calendar")
	bad(`        "0,0": { widget: month, span: [2, 2], stale: 1h }`, "stale is for widget: calendar")
	bad(`        "0,0": { widget: month, span: [2, 2], follow: x }`, "follow is for widget: calendar")
	bad(`        "0,0": { widget: calendar, week_start: monday }`, "week_start is for widget: month")
	bad(`        "0,0": { widget: clock, calendars: [a] }`, "calendars is for widget: calendar and month")
	bad(`        "0,0": { widget: month, span: [2, 2], id: "a b" }`, "id must be")
	bad(`        "0,0": { widget: month, span: [2, 2], page_reset: 1s }`, "page_reset must be")
	bad(`        "0,0": { widget: month, span: [2, 2], calendars: [a, a] }`, "distinct")
	bad(`        "0,0": { widget: calendar, follow: nope }
        "1,0": { widget: month, span: [2, 2] }`, `follow: no widget: month with id "nope" in this layer`)
	bad(`        "0,0": { widget: month, span: [2, 1] }
        "0,1": { widget: month, span: [2, 1] }`, `another widget: month in this layer has id "month"`)
	// 別のレイヤーの月の表示は指せない
	cfg, err := parseConfig([]byte(monthGrid + `        "0,0": { widget: month, span: [2, 2] }
        "3,2": { layer_toggle: two }
  - name: two
    touch:
      cells:
        "0,0": { widget: calendar, follow: month }
        "3,2": { layer_toggle: two }
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := compileKeymap(cfg); err == nil || !strings.Contains(err.Error(), `layer "two" touch cell "0,0": follow: no widget: month`) {
		t.Errorf("other layer: %v", err)
	}

	// 2×2 より小さければ警告（誤りではない）
	cfg, err = parseConfig([]byte(monthGrid + `        "0,0": { widget: month, span: [2, 1] }
        "2,0": { widget: month, id: m2 }
        "0,1": { widget: month, id: m3, span: [3, 2] }
`))
	if err != nil {
		t.Fatal(err)
	}
	_, warns, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 2 || !strings.Contains(warns[0], `cell "0,0": widget: month is meant for span [2, 2] or larger (now [2, 1])`) ||
		!strings.Contains(warns[1], `cell "2,0"`) {
		t.Errorf("warns = %q", warns)
	}
	w := monthWidget(t, `{ widget: month }`)
	if w.ID != "month" || w.WeekStart != time.Sunday || w.PageReset != defaultPageReset || !w.handlesTouch() {
		t.Errorf("defaults: %+v", w)
	}
	// 月の表示があれば、set_calendar の shown は true
	cfg, _ = parseConfig([]byte(monthGrid + `        "0,0": { widget: month, span: [2, 2] }`))
	if !calendarShown(cfg) {
		t.Errorf("calendarShown with month")
	}
}

func TestCalendarCovers(t *testing.T) {
	inTokyo(t)
	day := func(s string) time.Time { d, _ := parseCalDay(s); return d }
	d := &CalendarData{From: "2026-10-06", Days: 7}
	for _, c := range []struct {
		day  string
		want bool
	}{{"2026-10-05", false}, {"2026-10-06", true}, {"2026-10-13", true}, {"2026-10-14", false}} {
		if got := d.covers(day(c.day)); got != c.want {
			t.Errorf("covers(%s) = %v", c.day, got)
		}
	}
	if !(&CalendarData{}).covers(day("2000-01-01")) {
		t.Errorf("no from: should cover")
	}
}

// GUI（gui/src/monthwidget.ts）が Go と同じ配置にすることを確かめるための表。
// 配置を変えたら LEFTHAND_UPDATE_MONTHLAYOUT=1 go test -run MonthLayoutTable で書き直す。
const guiMonthLayoutJSON = "gui/test/fixtures/monthlayout.json"

type monthLayoutCase struct {
	Fixture   string   `json:"fixture"`
	Now       string   `json:"now"`
	Synced    bool     `json:"synced"`
	Area      [4]int   `json:"area"`
	WeekStart string   `json:"week_start,omitempty"`
	Calendars []string `json:"calendars,omitempty"`
	Offset    int      `json:"offset"`
	Select    string   `json:"select,omitempty"`
	Layout    monthLay `json:"layout"`
	Next      string   `json:"next"`
}

func TestMonthLayoutTable(t *testing.T) {
	inTokyo(t)
	var cases []monthLayoutCase
	areas := []image.Rectangle{
		image.Rect(12, 12, 388, 308), // 2×2（見出しなし）
		image.Rect(12, 40, 788, 308), // 4×2（見出しあり）
		image.Rect(12, 12, 188, 148), // 1×1（小さすぎる）
	}
	for _, fx := range []string{"calendar-month.json", "calendar.json", "none"} {
		var d *CalendarData
		if fx != "none" {
			d = loadCalFixture(t, fx)
		}
		nows := []string{"2026-10-06T09:41:27", "2026-10-31T23:59:59", "2026-02-01T00:00:00", "2028-02-29T12:00:00"}
		if fx != "calendar-month.json" {
			nows = nows[:1]
		}
		for _, now := range nows {
			for _, a := range areas {
				for _, v := range []struct {
					ws     string
					cals   []string
					offset int
					sel    string
					synced bool
				}{{"", nil, 0, "", true}, {"monday", nil, 0, "", true}, {"", []string{"家", "趣味"}, 0, "", true},
					{"", nil, -1, "2026-09-22", true}, {"monday", nil, 1, "2026-10-17", true}, {"", nil, 2, "", false}} {
					spec := `{ widget: month }`
					if v.ws != "" {
						spec = `{ widget: month, week_start: ` + v.ws + ` }`
					}
					w := monthWidget(t, spec)
					w.Calendars = v.cals
					n := jst(now)
					if v.offset != 0 || v.sel != "" {
						w.month.set(v.offset, v.sel, n)
					}
					env := WidgetEnv{Now: n, TimeSynced: v.synced, Calendar: d}
					cases = append(cases, monthLayoutCase{Fixture: fx, Now: now + "+09:00", Synced: v.synced, Area: rect4(a), WeekStart: v.ws,
						Calendars: v.cals, Offset: v.offset, Select: v.sel, Layout: monthLayout(a, w, env),
						Next: monthNext(w, env).Format(time.RFC3339)})
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
	if os.Getenv("LEFTHAND_UPDATE_MONTHLAYOUT") == "1" {
		if err := os.WriteFile(guiMonthLayoutJSON, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(guiMonthLayoutJSON); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date (%v); run LEFTHAND_UPDATE_MONTHLAYOUT=1 go test -run MonthLayoutTable", guiMonthLayoutJSON, err)
	}
}
