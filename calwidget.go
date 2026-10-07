package main

import (
	"fmt"
	"image"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- カレンダーのウィジェット（widget: calendar） ----------
//
// 今日の予定を 1 行ずつ（終日の予定、時刻の順）並べ、そのあとに今日より先の「次の予定」を 1 つ出す。
// 今の予定は行を塗って目立たせ、終わった予定は薄く描く。いちばん下に、最終更新の時刻を小さく出す
// （古くなった、取得に失敗した、Brain の時刻を合わせていないときは橙色）。
// 入りきらなければ Todo と同じくページに分け、セルの下の ▲▼ で送る。触っていないときは、
// 今か、これからの予定がある最初のページを出す（しばらく触らなければ、そのページに戻る）。
// 時刻と日付は Brain のタイムゾーンで決める。
// gui/src/calwidget.ts の calLayout、描き方と同じ結果にする。

// 行の種類
const (
	calPast    = iota // 今日の、終わった予定
	calNow            // 今の予定
	calFuture         // 今日の、これからの予定
	calAllDay         // 今日の終日の予定（今日をすべて含む予定も）
	calNextRow        // 今日より先の、次の予定
	calInfo           // 「今日の予定はありません」
)

const (
	calNoData      = "予定を受け取っていません"
	calNoneToday   = "今日の予定はありません"
	calNoTitle     = "（名前なし）"
	calFooterBig   = 240 // 見出しの下の範囲の幅と高さがこれ以上なら、最終更新を 2 倍で描く
	calScaleWidth  = 88  // 文字の倍率 s には、幅が calScaleWidth*s 以上要る
	calBarW        = 3   // 左の色の帯の幅（倍率あたり）
	calTimeColText = "00:00"
)

var (
	colCalNow  = RGB{0x1e, 0x4a, 0x7c} // 今の予定の行
	colCalWarn = colUnsynced           // 最終更新が古い、取得に失敗した、時刻を合わせていない
)

// calRow は 1 行。
type calRow struct {
	Label string `json:"label"` // 時刻の欄
	Title string `json:"title"`
	Color string `json:"color"` // カレンダーの色（#rrggbb）。calInfo では空
	State int    `json:"state"`
}

// calLay は、カレンダーのセルの中の配置。
type calLay struct {
	Rows       []calRow `json:"rows"`
	Message    string   `json:"message,omitempty"` // 予定の代わりに中央に出す文（一度も受け取っていない）
	AutoPage   int      `json:"auto_page"`         // 触っていないときに出すページ
	Scale      int      `json:"scale"`
	Footer     string   `json:"footer"`
	FooterWarn bool     `json:"footer_warn"`
	FooterS    int      `json:"footer_scale"`
	geom       todoGeom
	footerY    int
}

// calWeekday などは、日付の書き方。
func calDate(t time.Time) string {
	return fmt.Sprintf("%d/%d(%s)", int(t.Month()), t.Day(), jaWeekdays[t.Weekday()])
}

func calHM(t time.Time) string { return t.Format("15:04") }

// dayStart は、t の日の 0 時（Brain のタイムゾーン）。
func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// calShown は、ウィジェットに出すカレンダー（calendars で選んだもの）。
func calShown(w *WidgetDef, d *CalendarData) []*Calendar {
	var out []*Calendar
	if d == nil {
		return nil
	}
	for i := range d.Calendars {
		c := &d.Calendars[i]
		if len(w.Calendars) == 0 {
			out = append(out, c)
			continue
		}
		for _, n := range w.Calendars {
			if n == c.Name {
				out = append(out, c)
			}
		}
	}
	return out
}

type calItem struct {
	ev    *CalEvent
	cal   *Calendar
	order int // カレンダーの順
}

// calRows は、今日の予定と次の予定の行を作る。
func calRows(cals []*Calendar, now time.Time) []calRow {
	now = now.In(time.Local)
	t0 := dayStart(now)
	t1 := t0.AddDate(0, 0, 1)
	var today, later []calItem
	for ci, c := range cals {
		for i := range c.Events {
			ev := &c.Events[i]
			it := calItem{ev, c, ci}
			switch {
			case ev.s.Before(t1) && (ev.e.After(t0) || (ev.e.Equal(ev.s) && !ev.s.Before(t0))):
				today = append(today, it)
			case !ev.s.Before(t1):
				later = append(later, it)
			}
		}
	}
	whole := func(ev *CalEvent) bool { return ev.AllDay() || (!ev.s.After(t0) && !ev.e.Before(t1)) }
	less := func(a, b calItem) bool {
		if wa, wb := whole(a.ev), whole(b.ev); wa != wb {
			return wa
		}
		if !a.ev.s.Equal(b.ev.s) {
			return a.ev.s.Before(b.ev.s)
		}
		if !a.ev.e.Equal(b.ev.e) {
			return a.ev.e.Before(b.ev.e)
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.ev.Title < b.ev.Title
	}
	sort.SliceStable(today, func(i, j int) bool { return less(today[i], today[j]) })
	title := func(ev *CalEvent) string {
		if ev.Title == "" {
			return calNoTitle
		}
		return ev.Title
	}
	var rows []calRow
	for _, it := range today {
		ev := it.ev
		r := calRow{Title: title(ev), Color: it.cal.Color}
		switch {
		case whole(ev):
			r.Label, r.State = "終日", calAllDay
		case !now.Before(ev.s) && now.Before(ev.e):
			r.State = calNow
			r.Label = "今"
			if ev.e.Before(t1) {
				r.Label = "〜" + calHM(ev.e.In(time.Local))
			}
		case ev.s.Before(t0): // 前の日から続き、今日終わった
			r.Label, r.State = "〜"+calHM(ev.e.In(time.Local)), calPast
		case !ev.e.After(now) && (ev.e.After(ev.s) || ev.s.Before(now)):
			r.Label, r.State = calHM(ev.s.In(time.Local)), calPast
		default:
			r.Label, r.State = calHM(ev.s.In(time.Local)), calFuture
		}
		rows = append(rows, r)
	}
	if len(today) == 0 {
		rows = append(rows, calRow{Title: calNoneToday, State: calInfo})
	}
	if len(later) > 0 {
		sort.SliceStable(later, func(i, j int) bool {
			a, b := later[i], later[j]
			if !a.ev.s.Equal(b.ev.s) {
				return a.ev.s.Before(b.ev.s)
			}
			return less(a, b)
		})
		it := later[0]
		s := it.ev.s.In(time.Local)
		label := calDate(s)
		if dayStart(s).Equal(t1) {
			label = "明日"
		}
		if !it.ev.AllDay() {
			label += " " + calHM(s)
		}
		rows = append(rows, calRow{Label: label, Title: title(it.ev), Color: it.cal.Color, State: calNextRow})
	}
	return rows
}

// calFooter は、最終更新の行の文と、橙色で出すかどうか。
func calFooter(w *WidgetDef, cals []*Calendar, env WidgetEnv) (string, bool) {
	if !env.TimeSynced {
		return unsyncedText, true
	}
	var oldest *time.Time
	failed := 0
	for _, c := range cals {
		if c.Error != "" {
			failed++
		}
		if c.FetchedAt != nil && (oldest == nil || c.FetchedAt.Before(*oldest)) {
			oldest = c.FetchedAt
		}
	}
	s, warn := "更新 なし", true
	if oldest != nil {
		t := oldest.In(time.Local)
		now := env.Now.In(time.Local)
		if dayStart(t).Equal(dayStart(now)) {
			s = "更新 " + calHM(t)
		} else {
			s = fmt.Sprintf("更新 %d/%d %s", int(t.Month()), t.Day(), calHM(t))
		}
		warn = false
		if w.Stale > 0 && !env.Now.Before(oldest.Add(w.Stale)) {
			s += " 古い"
			warn = true
		}
	}
	if failed > 0 {
		s += " 失敗 " + strconv.Itoa(failed)
		warn = true
	}
	return s, warn
}

// calLayout は、見出しの下の範囲 area の中の配置を決める。
func calLayout(area image.Rectangle, w *WidgetDef, env WidgetEnv) calLay {
	cals := calShown(w, env.Calendar)
	if len(cals) == 0 {
		return calLay{Message: calNoData, geom: todoGeometry(area, 0, w.Rows), Scale: 1}
	}
	var l calLay
	l.Footer, l.FooterWarn = calFooter(w, cals, env)
	l.FooterS = 1
	if area.Dx() >= calFooterBig && area.Dy() >= calFooterBig {
		l.FooterS = 2
	}
	body := area
	body.Max.Y -= fontH*l.FooterS + widgetLineGap
	l.footerY = body.Max.Y + widgetLineGap
	l.Rows = calRows(cals, env.Now)
	l.geom = todoGeometry(body, len(l.Rows), w.Rows)
	l.Scale = l.geom.scale
	for l.Scale > 1 && calScaleWidth*l.Scale > area.Dx() {
		l.Scale--
	}
	// 触っていないときは、今かこれからの予定（なければ次の予定）がある最初のページ
	first := 0
	for i, r := range l.Rows {
		if r.State != calPast && r.State != calAllDay {
			first = i
			break
		}
	}
	l.AutoPage = first / l.geom.per
	return l
}

// calKey は、描き直すかどうかを決める中身。
func calKey(w *WidgetDef, env WidgetEnv) string {
	page, _, nav := w.pager.view(env.Now, w.PageReset, -1)
	cals := calShown(w, env.Calendar)
	var b strings.Builder
	rev := uint64(0)
	if env.Calendar != nil {
		rev = env.Calendar.Rev
	}
	f, warn := calFooter(w, cals, env)
	fmt.Fprintf(&b, "%d\x00%d\x00%d\x00%s\x00%v\x00%s", rev, page, nav, f, warn, dayStart(env.Now.In(time.Local)).Format(calDayLayout))
	if len(cals) > 0 {
		for _, r := range calRows(cals, env.Now) {
			fmt.Fprintf(&b, "\x00%d%s", r.State, r.Label)
		}
	}
	return b.String()
}

// calNext は、カレンダーのセルの中身が次に変わる時刻（予定の始まりと終わり、日付の変わり目、古くなる時刻、ページが戻る時刻）。
func calNext(w *WidgetDef, env WidgetEnv) time.Time {
	now := env.Now
	next := dayStart(now.In(time.Local)).AddDate(0, 0, 1)
	consider := func(t time.Time) {
		if t.After(now) && t.Before(next) {
			next = t
		}
	}
	for _, c := range calShown(w, env.Calendar) {
		for i := range c.Events {
			consider(c.Events[i].s)
			consider(c.Events[i].e)
		}
		if c.FetchedAt != nil && w.Stale > 0 {
			consider(c.FetchedAt.Add(w.Stale))
		}
	}
	if t := w.pager.resetAt(w.PageReset, now); !t.IsZero() {
		consider(t)
	}
	return next
}

func parseHexColor(s string) RGB {
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	if err != nil || len(s) != 7 {
		return colSub
	}
	return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}
}

// drawCalendar は、カレンダーのセルの中身を描く。area は見出しの下の範囲。
func drawCalendar(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv, subInk RGB) {
	l := calLayout(area, w, env)
	if l.Message != "" {
		s := min(fitScale([]string{l.Message}, area.Dx(), area.Dy()), 2)
		drawCentered(cv, area, area.Min.Y+(area.Dy()-fontH*s)/2, l.Message, s, subInk)
		return
	}
	page, _, nav := w.pager.view(env.Now, w.PageReset, l.AutoPage)
	g := l.geom
	page = min(page, g.pages-1)
	for i, r := range g.rows {
		k := page*g.per + i
		if k >= len(l.Rows) {
			break
		}
		if i > 0 {
			cv.fill(image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+1), colTodoRule)
		}
		drawCalRow(cv, r, l.Rows[k], l.Scale)
	}
	if !g.nav.Empty() {
		drawPageNav(cv, g, page, nav)
	}
	fc := colSub
	if l.FooterWarn {
		fc = colCalWarn
	}
	f := todoEllipsis(l.Footer, area.Dx()/l.FooterS)
	cv.text(area.Min.X, l.footerY, f, l.FooterS, fc, area)
}

// drawCalRow は 1 行を描く。左にカレンダーの色の帯、時刻、予定の名前。
func drawCalRow(cv *Canvas, r image.Rectangle, row calRow, s int) {
	labelC, titleC := colSub, colText
	switch row.State {
	case calNow:
		cv.fill(r.Inset(1), colCalNow)
		labelC = colText
	case calPast:
		labelC, titleC = colTodoDone, colTodoDone
	case calInfo:
		titleC = colSub
	}
	x := r.Min.X + todoPad
	if row.Color != "" {
		c := parseHexColor(row.Color)
		if row.State == calPast {
			c = colTodoDone
		}
		bh := fontH * s
		y := r.Min.Y + (r.Dy()-bh)/2
		cv.fill(image.Rect(x, y, x+calBarW*s, y+bh), c)
		x += calBarW*s + todoPad
	}
	ty := r.Min.Y + (r.Dy()-fontH*s)/2
	if row.Label != "" {
		cv.text(x, ty, row.Label, s, labelC, r)
		x += max(font.textWidth(row.Label), font.textWidth(calTimeColText))*s + 4*s
	}
	if w := (r.Max.X - todoPad - x) / s; w > 0 {
		cv.text(x, ty, todoEllipsis(row.Title, w), s, titleC, r)
	}
}
