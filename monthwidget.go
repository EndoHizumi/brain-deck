package main

import (
	"fmt"
	"image"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 月の表示のウィジェット（widget: month） ----------
//
// 卓上カレンダーのように、見出し（年と月）、曜日、7 列の日付を並べる。前後の月の日付は薄く出す。
// 今日は反転し、選んだ日は黄色の枠で囲む。土曜は青、日曜と祝日は赤。
// 予定のある日は、数字の下にカレンダーの色の点を出す（1 日にカレンダー 1 つにつき 1 つ。monthMaxDots まで）。
// 祝日のカレンダー（calendars.yaml の holiday: true）は、日付を赤くするのに使い、点は付けない。
// いちばん下の帯は、▲（前の月）、選んだ日の説明（日付、祝日の名前、予定の数）、▼（次の月）。
// 日付をタップすると、その日を選ぶ。同じレイヤーの、follow でこのセルの id を指すカレンダーのセルに、その日の予定が出る。
// 帯の中央をタップすると、今月と今日に戻る。page_reset のあいだ触らなければ、同じく今月と今日に戻る。
// 日付は Brain のタイムゾーンで決める。gui/src/monthwidget.ts の monthLayout、描き方と同じ結果にする。

const (
	defaultMonthID = "month"
	monthMaxOffset = 24 // 今月から前後に送れる月の数
	monthMaxDots   = 3  // 1 日に出す点の数。カレンダーがこれより多い日は、monthMaxDots-1 個の色と、白っぽい点（ほかにもある）
	monthMinSpan   = 2  // これより小さな span（列か行）では、-check で警告する
	monthNumText   = "30"
	monthNoData    = "予定未受信"
	monthOutRange  = "範囲外"
	monthPartRange = "一部範囲外"
	monthNoEvents  = "予定なし"
)

var (
	colMonthSat     = RGB{0x60, 0xa8, 0xff} // 土曜
	colMonthSun     = RGB{0xff, 0x60, 0x60} // 日曜と祝日
	colMonthDim     = RGB{0x50, 0x5a, 0x66} // 前後の月の日付
	colMonthDimSat  = RGB{0x30, 0x50, 0x78}
	colMonthDimSun  = RGB{0x78, 0x38, 0x3c}
	colMonthToday   = colText               // 今日の塗り
	colMonthTodayIn = RGB{0x10, 0x14, 0x1a} // 今日の数字
	colMonthTodaySa = RGB{0x10, 0x50, 0xc0}
	colMonthTodaySu = RGB{0xd0, 0x20, 0x20}
	colMonthMore    = colSub // 点に入りきらないカレンダーがある
)

// monthState は、月の表示のセル 1 つの、画面での状態（出している月、選んだ日、押している矢印）。
// 入力の goroutine と描画の goroutine から触るので、ロックで守る。
type monthState struct {
	mu      sync.Mutex
	offset  int       // 今月からいくつ送ったか（touched から PageReset のあいだだけ使う）
	sel     string    // 選んだ日（YYYY-MM-DD）。空なら今日
	touched time.Time // 最後に触った時刻。ゼロなら一度も触っていない
	nav     int       // 押している ▲（-1）か ▼（+1）
}

func (s *monthState) live(now time.Time, reset time.Duration) bool {
	return !s.touched.IsZero() && (reset <= 0 || now.Before(s.touched.Add(reset)))
}

// view は、now の時点の月（今月からのずれ）、選んだ日、押している矢印。触ってから reset が過ぎていれば、今月と今日。
func (s *monthState) view(now time.Time, reset time.Duration) (offset int, sel string, nav int) {
	if s == nil {
		return 0, "", 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live(now, reset) {
		return 0, "", s.nav
	}
	return s.offset, s.sel, s.nav
}

// resetAt は、今月と今日に戻る時刻（描き直す時刻）。戻らないならゼロ。
func (s *monthState) resetAt(reset time.Duration, now time.Time) time.Time {
	if s == nil || reset <= 0 {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.touched.Add(reset); !s.touched.IsZero() && now.Before(t) {
		return t
	}
	return time.Time{}
}

// set は、月と選んだ日を、触ったことにして決める（-render-month-* で使う）。
func (s *monthState) set(offset int, sel string, now time.Time) {
	s.mu.Lock()
	s.offset, s.sel, s.touched = offset, sel, now
	s.mu.Unlock()
}

// compileMonth は widget: month の書き方を確かめて組み立てる。
func compileMonth(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: month handles taps itself (▲▼ to change the month, tap a day to select it); remove key and layer_*")
	}
	w := &WidgetDef{Kind: widgetMonth, ID: s.ID, month: &monthState{}}
	if w.ID == "" {
		w.ID = defaultMonthID
	}
	if !validTextID(w.ID) {
		return nil, fmt.Errorf("id must be 1-32 characters of A-Z a-z 0-9 _ . - (omit it to use %q), got %q", defaultMonthID, s.ID)
	}
	switch s.WeekStart {
	case "", "sunday":
		w.WeekStart = time.Sunday
	case "monday":
		w.WeekStart = time.Monday
	default:
		return nil, fmt.Errorf("week_start must be sunday or monday, got %q", s.WeekStart)
	}
	var err error
	if w.PageReset, err = parsePageReset(s.PageReset); err != nil {
		return nil, err
	}
	if err := checkCalNames(s.Calendars); err != nil {
		return nil, err
	}
	w.Calendars = s.Calendars
	return w, nil
}

// linkFollows は、レイヤー 1 つの格子の中で、カレンダーの follow を月の表示に結ぶ。
// 月の表示の id が重なるとき、follow の id の月の表示がないときは誤り。小さすぎる月の表示は警告。
func linkFollows(g *Grid, tp, layer string, fail func(path, format string, args ...any)) (warns []string) {
	months := map[string]*WidgetDef{}
	for p, a := range g.Cells {
		if a.Widget == nil || a.Widget.Kind != widgetMonth {
			continue
		}
		k := fmt.Sprintf("%d,%d", p.Col, p.Row)
		if _, dup := months[a.Widget.ID]; dup {
			fail(tp+"/cells/"+k, "layer %q touch cell %q: another widget: month in this layer has id %q (give each one its own id)", layer, k, a.Widget.ID)
		}
		months[a.Widget.ID] = a.Widget
		if a.SpanW < monthMinSpan || a.SpanH < monthMinSpan {
			warns = append(warns, fmt.Sprintf("layer %q touch cell %q: widget: month is meant for span [%d, %d] or larger (now [%d, %d]); the days may be too small to read or tap",
				layer, k, monthMinSpan, monthMinSpan, a.SpanW, a.SpanH))
		}
	}
	for p, a := range g.Cells {
		if a.Widget == nil || a.Widget.Kind != widgetCal || a.Widget.Follow == "" {
			continue
		}
		m, ok := months[a.Widget.Follow]
		if !ok {
			k := fmt.Sprintf("%d,%d", p.Col, p.Row)
			fail(tp+"/cells/"+k, "layer %q touch cell %q: follow: no widget: month with id %q in this layer", layer, k, a.Widget.Follow)
			continue
		}
		a.Widget.follow = m
	}
	sort.Strings(warns)
	return warns
}

// monthDay は、格子の日付 1 つ。
type monthDay struct {
	Day     string   `json:"day"` // YYYY-MM-DD
	In      bool     `json:"in"`  // 出している月の日
	Today   bool     `json:"today,omitempty"`
	Sel     bool     `json:"sel,omitempty"` // 選んだ日（今日でないとき）
	Red     bool     `json:"red,omitempty"` // 日曜か祝日
	Sat     bool     `json:"sat,omitempty"`
	Holiday string   `json:"holiday,omitempty"` // 祝日の名前
	Dots    []string `json:"dots,omitempty"`    // 点の色（#rrggbb）。"more" は、ほかにもある
	Rect    [4]int   `json:"rect"`
	num     int
	rect    image.Rectangle
}

// monthLay は、月の表示のセルの中の配置。JSON は、GUI と比べる表（monthlayout.json）に使う。
type monthLay struct {
	Title      string     `json:"title"` // 2026年10月
	TitleS     int        `json:"title_scale"`
	TitleWarn  bool       `json:"title_warn"` // 時刻を合わせていない
	Status     string     `json:"status,omitempty"`
	StatusWarn bool       `json:"status_warn,omitempty"`
	WdayY      int        `json:"wday_y"`
	Wdays      []int      `json:"wdays"` // 列ごとの曜日（0 が日曜）
	Weeks      int        `json:"weeks"`
	NumS       int        `json:"num_scale"`
	Dot        int        `json:"dot"` // 点の大きさ
	Days       []monthDay `json:"days"`
	Info       string     `json:"info"`
	Offset     int        `json:"offset"`
	Nav        [4]int     `json:"nav"`
	Up         [4]int     `json:"up"`
	Mid        [4]int     `json:"mid"`
	Down       [4]int     `json:"down"`
	nav        image.Rectangle
	up, mid    image.Rectangle
	down       image.Rectangle
	navPressed int
}

func monthTitle(t time.Time) string { return fmt.Sprintf("%d年%d月", t.Year(), int(t.Month())) }

// monthCals は、印を付けるカレンダー（calendars で選んだもの。祝日のカレンダーを除く）と、
// 祝日のカレンダー（calendars によらず、すべて）。
func monthCals(w *WidgetDef, d *CalendarData) (marks, holidays []*Calendar) {
	for _, c := range calShown(w, d) {
		if !c.Holiday {
			marks = append(marks, c)
		}
	}
	if d != nil {
		for i := range d.Calendars {
			if d.Calendars[i].Holiday {
				holidays = append(holidays, &d.Calendars[i])
			}
		}
	}
	return marks, holidays
}

// eachDay は、予定 ev が重なる日（0 時）のうち、[from, to) に入るものについて f を呼ぶ。
func eachDay(ev *CalEvent, from, to time.Time, f func(day time.Time)) {
	d := dayStart(ev.s.In(time.Local))
	if d.Before(from) {
		d = from
	}
	for ; d.Before(to); d = d.AddDate(0, 0, 1) {
		if !(ev.e.After(d) || (ev.e.Equal(ev.s) && d.Equal(dayStart(ev.s.In(time.Local))))) {
			break
		}
		f(d)
	}
}

// monthGeom は、月の表示の格子の配置（予定によらない）。タップした位置を解くのにも使う。
type monthGeom struct {
	first, next, start, end time.Time // 月の 1 日、次の月の 1 日、格子の最初の日、格子の次の日
	weeks, dim, cellH       int
	gridTop                 int
}

func newMonthGeom(area image.Rectangle, w *WidgetDef, today time.Time, offset int, titleS, navH int) monthGeom {
	var g monthGeom
	g.first = time.Date(today.Year(), today.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.Local)
	g.next = g.first.AddDate(0, 1, 0)
	lead := (int(g.first.Weekday()) - int(w.WeekStart) + 7) % 7
	g.start = g.first.AddDate(0, 0, -lead)
	g.dim = g.next.AddDate(0, 0, -1).Day()
	g.weeks = (lead + g.dim + 6) / 7
	g.end = g.start.AddDate(0, 0, g.weeks*7)
	g.gridTop = area.Min.Y + fontH*titleS + widgetLineGap + fontH + 2
	g.cellH = max((area.Max.Y-navH-g.gridTop)/g.weeks, 1)
	return g
}

// dayRect は、格子の i 番目（0 から）の日付の範囲。
func (g monthGeom) dayRect(area image.Rectangle, i int) image.Rectangle {
	col, row := i%7, i/7
	y0 := g.gridTop + row*g.cellH
	return image.Rect(area.Min.X+col*area.Dx()/7, y0, area.Min.X+(col+1)*area.Dx()/7, y0+g.cellH)
}

// monthBands は、見出しの倍率と、帯の高さ、帯（全体、▲、中央、▼）。
func monthBands(area image.Rectangle, title string) (titleS, navH int, nav, up, mid, down image.Rectangle) {
	aw, ah := area.Dx(), area.Dy()
	titleS = 1
	if aw >= 300 && ah >= 220 && font.textWidth(title)*2 <= aw/2 {
		titleS = 2
	}
	navH = min(max(ah/7, fontH+8), todoNavH)
	nav = image.Rect(area.Min.X, area.Max.Y-navH, area.Max.X, area.Max.Y)
	a, b := area.Min.X+aw/5, area.Max.X-aw/5
	up = image.Rect(area.Min.X, nav.Min.Y, a, nav.Max.Y)
	mid = image.Rect(a, nav.Min.Y, b, nav.Max.Y)
	down = image.Rect(b, nav.Min.Y, area.Max.X, nav.Max.Y)
	return
}

// monthLayout は、見出しの下の範囲 area の中の配置と、日付ごとの色と印を決める。
func monthLayout(area image.Rectangle, w *WidgetDef, env WidgetEnv) monthLay {
	now := env.Now.In(time.Local)
	today := dayStart(now)
	offset, sel, nav := w.month.view(env.Now, w.PageReset)
	firstDay := time.Date(today.Year(), today.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.Local)
	l := monthLay{Title: monthTitle(firstDay), Offset: offset, navPressed: nav, TitleWarn: !env.TimeSynced}
	var navH int
	l.TitleS, navH, l.nav, l.up, l.mid, l.down = monthBands(area, l.Title)
	g := newMonthGeom(area, w, today, offset, l.TitleS, navH)
	first, next, start, end, dim, weeks := g.first, g.next, g.start, g.end, g.dim, g.weeks
	l.Weeks = weeks
	aw := area.Dx()
	l.WdayY = area.Min.Y + fontH*l.TitleS + widgetLineGap
	cellH := g.cellH
	for i := 0; i < 7; i++ {
		l.Wdays = append(l.Wdays, (int(w.WeekStart)+i)%7)
	}

	// 数字の倍率と点の大きさ。数字と点が縦に入り、2 桁の数字が横に入る、いちばん大きな倍率
	cellW := aw / 7
	l.NumS = 1
	for s := 3; s > 1; s-- {
		if fontH*s+1+2*s+2 <= cellH && font.textWidth(monthNumText)*s+4 <= cellW {
			l.NumS = s
			break
		}
	}
	l.Dot = 2 * l.NumS

	// 状態の表示（見出しの右）
	d := env.Calendar
	hasData := d != nil && len(d.Calendars) > 0
	switch {
	case !env.TimeSynced:
		l.Status, l.StatusWarn = unsyncedText, true
	case hasData:
		in := 0
		for t := first; t.Before(next); t = t.AddDate(0, 0, 1) {
			if d.covers(t) {
				in++
			}
		}
		switch {
		case in == 0:
			l.Status = monthOutRange
		case in < dim:
			l.Status = monthPartRange
		}
	}

	// 祝日と予定の印
	marks, hols := monthCals(w, d)
	holiday := map[time.Time]string{}
	for _, c := range hols {
		for i := range c.Events {
			ev := &c.Events[i]
			eachDay(ev, start, end, func(day time.Time) {
				if _, ok := holiday[day]; !ok {
					t := ev.Title
					if t == "" {
						t = "祝日"
					}
					holiday[day] = t
				}
			})
		}
	}
	marked := map[time.Time][]int{} // 日ごとの、予定のあるカレンダー（順）
	for ci, c := range marks {
		for i := range c.Events {
			eachDay(&c.Events[i], start, end, func(day time.Time) {
				if m := marked[day]; len(m) == 0 || m[len(m)-1] != ci {
					marked[day] = append(m, ci)
				}
			})
		}
	}

	selDay := today
	if sel != "" {
		if t, err := parseCalDay(sel); err == nil {
			selDay = t
		}
	}
	for i := 0; i < weeks*7; i++ {
		day := start.AddDate(0, 0, i)
		md := monthDay{Day: day.Format(calDayLayout), In: !day.Before(first) && day.Before(next), num: day.Day(),
			Today: day.Equal(today), Sat: day.Weekday() == time.Saturday, Red: day.Weekday() == time.Sunday}
		md.Sel = day.Equal(selDay) && !md.Today
		if h, ok := holiday[day]; ok {
			md.Holiday, md.Red = h, true
		}
		if m := marked[day]; m != nil {
			n := len(m)
			for k, ci := range m {
				if n > monthMaxDots && k == monthMaxDots-1 {
					md.Dots = append(md.Dots, "more")
					break
				}
				md.Dots = append(md.Dots, marks[ci].Color)
			}
		}
		md.rect = g.dayRect(area, i)
		md.Rect = rect4(md.rect)
		l.Days = append(l.Days, md)
	}

	// 帯の中央の説明（選んだ日か今日）
	l.Info = calDate(selDay)
	switch {
	case !hasData:
		l.Info += " " + monthNoData
	case !d.covers(selDay):
		l.Info += " " + monthOutRange
	default:
		// 選んだ日は、月を送ると格子の外になることもあるので、印とは別に数える
		n, h := 0, ""
		for _, c := range marks {
			for i := range c.Events {
				eachDay(&c.Events[i], selDay, selDay.AddDate(0, 0, 1), func(time.Time) { n++ })
			}
		}
		for _, c := range hols {
			for i := range c.Events {
				eachDay(&c.Events[i], selDay, selDay.AddDate(0, 0, 1), func(time.Time) {
					if h == "" {
						h = c.Events[i].Title
					}
				})
			}
		}
		if h != "" {
			l.Info += " " + h
		}
		switch {
		case n > 0:
			l.Info += " 予定" + strconv.Itoa(n)
		case h == "":
			l.Info += " " + monthNoEvents
		}
	}

	l.Nav, l.Up, l.Mid, l.Down = rect4(l.nav), rect4(l.up), rect4(l.mid), rect4(l.down)
	return l
}

func rect4(r image.Rectangle) [4]int { return [4]int{r.Min.X, r.Min.Y, r.Max.X, r.Max.Y} }

// monthKey は、描き直すかどうかを決める中身。
func monthKey(w *WidgetDef, env WidgetEnv) string {
	offset, sel, nav := w.month.view(env.Now, w.PageReset)
	rev := uint64(0)
	if env.Calendar != nil {
		rev = env.Calendar.Rev
	}
	return fmt.Sprintf("%d\x00%s\x00%d\x00%d\x00%v\x00%s", offset, sel, nav, rev, env.TimeSynced,
		dayStart(env.Now.In(time.Local)).Format(calDayLayout))
}

// monthNext は、月の表示の中身が次に変わる時刻（日付の変わり目、今月と今日に戻る時刻）。
func monthNext(w *WidgetDef, env WidgetEnv) time.Time {
	next := dayStart(env.Now.In(time.Local)).AddDate(0, 0, 1)
	if t := w.month.resetAt(w.PageReset, env.Now); !t.IsZero() && t.Before(next) {
		next = t
	}
	return next
}

// monthDayInk は、日付の数字の色。
func monthDayInk(d monthDay) RGB {
	switch {
	case d.Today && d.Red:
		return colMonthTodaySu
	case d.Today && d.Sat:
		return colMonthTodaySa
	case d.Today:
		return colMonthTodayIn
	case !d.In && d.Red:
		return colMonthDimSun
	case !d.In && d.Sat:
		return colMonthDimSat
	case !d.In:
		return colMonthDim
	case d.Red:
		return colMonthSun
	case d.Sat:
		return colMonthSat
	}
	return colText
}

// drawMonth は、月の表示のセルの中身を描く。area は見出しの下の範囲。
func drawMonth(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv) {
	l := monthLayout(area, w, env)
	tc := colText
	if l.TitleWarn {
		tc = colCalWarn
	}
	titleH := fontH * l.TitleS
	cv.text(area.Min.X+2, area.Min.Y, l.Title, l.TitleS, tc, area)
	if l.Status != "" {
		sc := colSub
		if l.StatusWarn {
			sc = colCalWarn
		}
		sx := area.Max.X - font.textWidth(l.Status) - 2
		cv.text(max(sx, area.Min.X), area.Min.Y+(titleH-fontH)/2, l.Status, 1, sc, area)
	}
	aw := area.Dx()
	for i, wd := range l.Wdays {
		x0, x1 := area.Min.X+i*aw/7, area.Min.X+(i+1)*aw/7
		c := colSub
		switch wd {
		case 0:
			c = colMonthSun
		case 6:
			c = colMonthSat
		}
		drawCentered(cv, image.Rect(x0, area.Min.Y, x1, area.Max.Y), l.WdayY, jaWeekdays[wd], 1, c)
	}
	for _, d := range l.Days {
		drawMonthDay(cv, d, l.NumS, l.Dot)
	}
	// 帯：▲ 説明 ▼
	cv.fill(image.Rect(l.nav.Min.X, l.nav.Min.Y, l.nav.Max.X, l.nav.Min.Y+1), colTodoRule)
	arrow := func(zone image.Rectangle, s string, ok, pressed bool) {
		c := colText
		if !ok {
			c = colTodoOff
		} else if pressed {
			cv.fill(zone.Inset(2), colPressed)
			c = colPressedText
		}
		sc := min(fitScale([]string{s}, zone.Dx(), zone.Dy()-4), 3)
		drawCentered(cv, zone, zone.Min.Y+(zone.Dy()-fontH*sc)/2, s, sc, c)
	}
	arrow(l.up, "▲", l.Offset > -monthMaxOffset, l.navPressed < 0)
	arrow(l.down, "▼", l.Offset < monthMaxOffset, l.navPressed > 0)
	is := 1
	if font.textWidth(l.Info)*2 <= l.mid.Dx() && fontH*2 <= l.mid.Dy()-4 {
		is = 2
	}
	info := todoEllipsis(l.Info, l.mid.Dx()/is)
	drawCentered(cv, l.mid, l.mid.Min.Y+(l.mid.Dy()-fontH*is)/2, info, is, colSub)
}

// drawMonthDay は、日付 1 つを描く。数字の下に点を並べる。
func drawMonthDay(cv *Canvas, d monthDay, s, dot int) {
	r := d.rect
	if d.Today {
		cv.fill(r.Inset(1), colMonthToday)
	}
	if d.Sel {
		cv.frame(r.Inset(1), 2, colPressed)
	}
	num := strconv.Itoa(d.num)
	blockH := fontH*s + 1 + dot
	y := r.Min.Y + (r.Dy()-blockH)/2
	drawCentered(cv, r, y, num, s, monthDayInk(d))
	if len(d.Dots) == 0 {
		return
	}
	gap := max(dot/2, 1)
	n := len(d.Dots)
	x := r.Min.X + (r.Dx()-(n*dot+(n-1)*gap))/2
	dy := y + fontH*s + 1
	for _, c := range d.Dots {
		ink := colMonthMore
		if c != "more" {
			ink = parseHexColor(c)
		}
		cv.fill(image.Rect(x, dy, x+dot, dy+dot).Intersect(r), ink)
		x += dot + gap
	}
}

// monthTouch は、月の表示のセルを押したとき。入力の goroutine から呼ばれるので、待たずに返る。
// ▲▼ で月を送り、日付で選び、帯の中央で今月と今日に戻る。どこを押しても「触った」ことにする。
// 予定は見ずに、格子の配置だけで押した位置を解く（入力の goroutine を待たせない）。
func (w *WidgetDef) monthTouch(rt *WidgetRT, t widgetTouch, label string) {
	now := rt.touchEnv().Now // エンジンのロックを持っているので、軽いほう
	area := widgetArea(t.cell, label)
	s := w.month
	s.mu.Lock()
	if !s.live(now, w.PageReset) {
		s.offset, s.sel = 0, ""
	}
	s.touched = now
	today := dayStart(now.In(time.Local))
	first := time.Date(today.Year(), today.Month()+time.Month(s.offset), 1, 0, 0, 0, 0, time.Local)
	titleS, navH, _, up, mid, down := monthBands(area, monthTitle(first))
	switch {
	case t.pt.In(up):
		if s.offset > -monthMaxOffset {
			s.offset--
			s.nav = -1
		}
	case t.pt.In(down):
		if s.offset < monthMaxOffset {
			s.offset++
			s.nav = 1
		}
	case t.pt.In(mid):
		s.offset, s.sel = 0, ""
	default:
		g := newMonthGeom(area, w, today, s.offset, titleS, navH)
		for i := 0; i < g.weeks*7; i++ {
			if t.pt.In(g.dayRect(area, i)) {
				day := g.start.AddDate(0, 0, i)
				s.sel = day.Format(calDayLayout)
				if day.Equal(today) {
					s.sel = ""
				}
			}
		}
	}
	offset, sel := s.offset, s.sel
	s.mu.Unlock()
	rt.redraw()
	vlogf("month: %s offset %d selected %q", w.ID, offset, sel)
}

// monthRelease は、離したとき。押している矢印を戻す。
func (w *WidgetDef) monthRelease(rt *WidgetRT) {
	s := w.month
	s.mu.Lock()
	changed := s.nav != 0
	s.nav = 0
	s.mu.Unlock()
	if changed && rt != nil {
		rt.redraw()
	}
}

// monthRenderState は、-render-month-offset と -render-month-select の値で、設定の月の表示をすべてその状態にする。
func monthRenderState(km *Keymap, offset int, sel string, now time.Time) error {
	if sel != "" {
		if _, err := parseCalDay(sel); err != nil {
			return fmt.Errorf("-render-month-select %q: write YYYY-MM-DD", sel)
		}
	}
	if offset < -monthMaxOffset || offset > monthMaxOffset {
		return fmt.Errorf("-render-month-offset must be %d..%d", -monthMaxOffset, monthMaxOffset)
	}
	for _, l := range km.Layers {
		if l.Grid == nil {
			continue
		}
		for _, a := range l.Grid.Cells {
			if a.Widget != nil && a.Widget.Kind == widgetMonth {
				a.Widget.month.set(offset, strings.TrimSpace(sel), now)
			}
		}
	}
	return nil
}
