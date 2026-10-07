package main

import (
	"fmt"
	"image"
	"strings"
	"time"
	_ "time/tzdata" // Brain と PC で、同じタイムゾーンの表を使う
)

// ---------- ウィジェット ----------
//
// タッチのセルに、キーの代わりに情報を表示する（`{ widget: clock }`）。
// 中身は、描画の goroutine が widgetKey で求め、前に描いたものと違うセルだけを描き直す。
// 次に中身が変わる時刻は widgetNext で求め、それまで描画の goroutine は眠る。
// 入力の処理は、ここを一切待たない。

const (
	widgetClock = "clock"
	widgetText  = "text"
	widgetTodo  = "todo"
	widgetCal   = "calendar"

	defaultClockFormat = "15:04"
	defaultDateFormat  = "1月2日({wday})"
	noDate             = "none"
	wdayToken          = "{wday}" // 曜日（日本語 1 文字）。Go の書式にはないので、自前で置き換える

	clockMaxScale = 10 // 時刻の行の最大の倍率（ふつうのラベルより大きく描く）
	dateMaxScale  = 3
	captionScale  = 2
	widgetLineGap = 4

	unsyncedText = "時刻未設定"
)

var widgetKinds = []string{widgetClock, widgetText, widgetTodo, widgetCal}

// ページを送るウィジェット（Todo、カレンダー）が、触らなければ最初のページに戻るまでの時間
const (
	defaultPageReset = time.Minute
	minPageReset     = 5 * time.Second
	maxPageReset     = 24 * time.Hour
	pageResetOff     = "off"
)

var jaWeekdays = [...]string{"日", "月", "火", "水", "木", "金", "土"}

// colUnsynced は、時刻が一度も合わされていないときの時計の色。
var colUnsynced = RGB{0xff, 0x80, 0x20}

// WidgetDef は組み立て済みのウィジェット 1 つ。
type WidgetDef struct {
	Kind       string
	Format     string         // clock：時刻の行
	DateFormat string         // clock：日付の行。空なら出さない
	Loc        *time.Location // clock：nil なら Brain のタイムゾーン
	Seconds    bool           // clock：秒を出す（1 秒ごとに描き直す）
	ID         string         // text：中身の名前
	Rows       int            // todo、calendar：1 ページの行数。0 なら高さで決める
	PageReset  time.Duration  // todo、calendar：最後に触ってから、最初のページに戻るまでの時間。0 なら戻らない
	Stale      time.Duration  // calendar：最終更新がこれより古ければ、古いと出す
	Calendars  []string       // calendar：出すカレンダーの名前。空ならすべて
	pager      *pagerState    // todo、calendar：ページと、押している行
}

// WidgetEnv は、ウィジェットを描くときの外の状態。描画の goroutine が描き直すたびに作る。
type WidgetEnv struct {
	Now        time.Time
	TimeSynced bool                 // この起動のあいだに時刻を合わせた（set_time か NTP）
	Texts      map[string]TextEntry // text：id ごとの中身（写し）
	Todo       TodoList             // todo：一覧（写し）
	Calendar   *CalendarData        // calendar：予定（写し。書き換えない）。nil なら一度も受け取っていない
}

// compileWidget は、ウィジェットのセルの書き方を検証して組み立てる。
func compileWidget(s ActionSpec) (*WidgetDef, error) {
	paged := s.Widget == widgetTodo || s.Widget == widgetCal
	if s.Rows != 0 && !paged {
		return nil, fmt.Errorf("rows is for widget: todo and calendar")
	}
	if s.PageReset != "" && !paged {
		return nil, fmt.Errorf("page_reset is for widget: todo and calendar")
	}
	if (s.Stale != "" || s.Calendars != nil) && s.Widget != widgetCal {
		return nil, fmt.Errorf("stale and calendars are for widget: calendar")
	}
	switch s.Widget {
	case widgetClock:
		if s.ID != "" {
			return nil, fmt.Errorf("id is for widget: text")
		}
	case widgetTodo, widgetCal:
		if s.Format != "" || s.DateFormat != "" || s.TZ != "" || s.ID != "" {
			return nil, fmt.Errorf("format, date_format, tz and id cannot be used with widget: %s", s.Widget)
		}
		if s.count() > 0 {
			if s.Widget == widgetTodo {
				return nil, fmt.Errorf("widget: todo handles taps itself (long press an item to check it, ▲▼ to turn pages); remove key and layer_*")
			}
			return nil, fmt.Errorf("widget: calendar handles taps itself (▲▼ to turn pages); remove key and layer_*")
		}
		if s.Rows < 0 || s.Rows > todoMaxRows {
			return nil, fmt.Errorf("rows must be 1..%d (omit it to fit the cell height)", todoMaxRows)
		}
		w := &WidgetDef{Kind: s.Widget, Rows: s.Rows, PageReset: defaultPageReset, pager: &pagerState{}}
		switch s.PageReset {
		case "":
		case pageResetOff:
			w.PageReset = 0
		default:
			d, err := time.ParseDuration(s.PageReset)
			if err != nil || d < minPageReset || d > maxPageReset {
				return nil, fmt.Errorf("page_reset must be a duration from %v to %v (such as 30s or 2m), or off", minPageReset, maxPageReset)
			}
			w.PageReset = d
		}
		if s.Widget == widgetCal {
			w.Stale = defaultCalStale
			if s.Stale != "" {
				d, err := time.ParseDuration(s.Stale)
				if err != nil || d < time.Minute || d > 30*24*time.Hour {
					return nil, fmt.Errorf("stale must be a duration from 1m to 720h (such as 3h)")
				}
				w.Stale = d
			}
			seen := map[string]bool{}
			for _, n := range s.Calendars {
				if n == "" || seen[n] {
					return nil, fmt.Errorf("calendars must be a list of distinct calendar names, got %q", s.Calendars)
				}
				seen[n] = true
			}
			w.Calendars = s.Calendars
		}
		return w, nil
	case widgetText:
		if s.Format != "" || s.DateFormat != "" || s.TZ != "" {
			return nil, fmt.Errorf("format, date_format and tz are for widget: clock")
		}
		if !validTextID(s.ID) {
			return nil, fmt.Errorf("widget: text needs id (1-32 characters of A-Z a-z 0-9 _ . -), got %q", s.ID)
		}
		return &WidgetDef{Kind: widgetText, ID: s.ID}, nil
	case "":
		if s.ID != "" {
			return nil, fmt.Errorf("id needs widget: text")
		}
		return nil, fmt.Errorf("format, date_format and tz need widget: clock")
	default:
		return nil, fmt.Errorf("unknown widget %q (%s)", s.Widget, strings.Join(widgetKinds, ", "))
	}
	w := &WidgetDef{Kind: s.Widget, Format: s.Format, DateFormat: s.DateFormat}
	if w.Format == "" {
		w.Format = defaultClockFormat
	}
	switch w.DateFormat {
	case "":
		w.DateFormat = defaultDateFormat
	case noDate:
		w.DateFormat = ""
	}
	if s.TZ != "" {
		loc, err := time.LoadLocation(s.TZ)
		if err != nil {
			return nil, fmt.Errorf("unknown tz %q (use an IANA name such as Asia/Tokyo)", s.TZ)
		}
		w.Loc = loc
	}
	if strings.TrimSpace(formatClock(time.Date(2026, 1, 5, 10, 20, 30, 0, time.UTC), w.Format)) == "" {
		return nil, fmt.Errorf("format %q shows nothing", s.Format)
	}
	w.Seconds = changesWithin(w.Format) || changesWithin(w.DateFormat)
	return w, nil
}

// changesWithin は、書式の結果が 1 分の中で変わるか（秒を出すか）を調べる。
// "05"、"5"、".000" など、秒の書き方によらず分かる。
func changesWithin(layout string) bool {
	if layout == "" {
		return false
	}
	t := time.Date(2026, 1, 5, 10, 20, 0, 0, time.UTC)
	a := formatClock(t, layout)
	for _, d := range []time.Duration{time.Second, 30 * time.Second, 500 * time.Millisecond} {
		if formatClock(t.Add(d), layout) != a {
			return true
		}
	}
	return false
}

// formatClock は Go の書式で時刻を書く。{wday} は日本語の曜日 1 文字になる。
func formatClock(t time.Time, layout string) string {
	parts := strings.Split(layout, wdayToken)
	for i, p := range parts {
		parts[i] = t.Format(p)
	}
	return strings.Join(parts, jaWeekdays[t.Weekday()])
}

// clockLines は、時計に出す 2 行と、時刻を合わせていないかどうかを返す。
func clockLines(w *WidgetDef, env WidgetEnv) (timeLine, dateLine string, unsynced bool) {
	t := env.Now
	if w.Loc != nil {
		t = t.In(w.Loc)
	}
	timeLine = formatClock(t, w.Format)
	if w.DateFormat != "" {
		dateLine = formatClock(t, w.DateFormat)
	}
	if !env.TimeSynced {
		dateLine = unsyncedText
	}
	return timeLine, dateLine, !env.TimeSynced
}

// widgetKey は、セルに描く中身を表す文字列。前に描いたものと違えば描き直す。
func widgetKey(v *CellView, env WidgetEnv) string {
	w := v.Widget
	switch w.Kind {
	case widgetClock:
		a, b, u := clockLines(w, env)
		return fmt.Sprintf("%s\x00%s\x00%v", a, b, u)
	case widgetText:
		e, ok := env.Texts[w.ID]
		if !ok {
			return "\x00none"
		}
		return fmt.Sprintf("%s\x00%s\x00%v", e.Text, e.Style, e.Expired(env.Now))
	case widgetTodo:
		return todoKey(w, env)
	case widgetCal:
		return calKey(w, env)
	}
	return ""
}

// widgetNext は、セルの中身が次に変わる時刻を返す。ゼロなら、時間では変わらない。
func widgetNext(v *CellView, env WidgetEnv) time.Time {
	now := env.Now
	switch v.Widget.Kind {
	case widgetClock:
		if v.Widget.Seconds {
			return now.Truncate(time.Second).Add(time.Second)
		}
		// 分の切り替わり。どのタイムゾーンも分単位でずれているので、UTC で切ってよい
		return now.Truncate(time.Minute).Add(time.Minute)
	case widgetText:
		if e, ok := env.Texts[v.Widget.ID]; ok && e.ExpiresAt != nil && now.Before(*e.ExpiresAt) {
			return *e.ExpiresAt // 期限が切れたら薄く描き直す
		}
	case widgetTodo:
		return v.Widget.pager.resetAt(v.Widget.PageReset, now) // 最初のページに戻す
	case widgetCal:
		return calNext(v.Widget, env)
	}
	return time.Time{}
}

// drawWidget は、セルの内側 inner にウィジェットを描く。label があれば上に小さく出す。
// ink、subInk は、押したとき（fill）に色を変えるためのもの。
func drawWidget(cv *Canvas, inner image.Rectangle, v *CellView, env WidgetEnv, ink, subInk RGB) {
	caption, s, area := widgetCaption(inner, widgetLabel(v.Widget, v.Label, env))
	if caption != "" {
		drawCentered(cv, inner, inner.Min.Y, caption, s, subInk)
	}
	switch v.Widget.Kind {
	case widgetTodo:
		drawTodo(cv, area, v.Widget, env, subInk)
	case widgetCal:
		drawCalendar(cv, area, v.Widget, env, subInk)
	case widgetClock:
		a, b, unsynced := clockLines(v.Widget, env)
		if unsynced && ink == colText {
			ink, subInk = colUnsynced, colUnsynced
		}
		bh, ds := 0, 0
		if b != "" {
			ds = min(fitScaleMax([]string{b}, area.Dx(), fontH*dateMaxScale, dateMaxScale), dateMaxScale)
			bh = fontH*ds + widgetLineGap
		}
		ts := fitScaleMax([]string{a}, area.Dx(), area.Dy()-bh, clockMaxScale)
		y := area.Min.Y + (area.Dy()-fontH*ts-bh)/2
		drawCentered(cv, area, y, a, ts, ink)
		if b != "" {
			drawCentered(cv, area, y+fontH*ts+widgetLineGap, b, ds, subInk)
		}
	case widgetText:
		e, ok := env.Texts[v.Widget.ID]
		body, c := textNoneText, subInk
		if ok {
			body, c = e.Text, textInk(e.Style, e.Expired(env.Now))
			if ink != colText { // 押したとき（fill）は、押したときの色
				c = ink
			}
		}
		lines, s := textLayout(body, area.Dx(), area.Dy())
		y := area.Min.Y + (area.Dy()-len(lines)*fontH*s)/2
		for _, ln := range lines {
			drawCentered(cv, area, y, ln, s, c)
			y += fontH * s
		}
	}
}

// widgetLabel は、セルの上に出す見出し。Todo は、設定の label に残りの件数を足す。
func widgetLabel(w *WidgetDef, label string, env WidgetEnv) string {
	if w.Kind == widgetTodo {
		return todoCaption(label, env.Todo.Items)
	}
	return label
}

// widgetCaption は、見出し（label）の 1 行と倍率、見出しの下に残る範囲を返す。label が空なら inner をそのまま返す。
func widgetCaption(inner image.Rectangle, label string) (string, int, image.Rectangle) {
	if label == "" {
		return "", 0, inner
	}
	caption := strings.ReplaceAll(label, "\n", " ")
	s := min(fitScale([]string{caption}, inner.Dx(), fontH*captionScale), captionScale)
	area := inner
	area.Min.Y += fontH*s + widgetLineGap
	return caption, s, area
}

// ---------- text ----------

const (
	textMaxScale = 6
	textNoneText = "未設定" // まだ一度も set_text していない
	textEllipsis = "…"
)

// テキストの色。種類ごとに、ふつうのときと、有効期限が切れたとき（薄く）の色
var (
	textColors = map[string][2]RGB{
		textNormal: {colText, {0x6a, 0x74, 0x80}},
		textOK:     {{0x50, 0xd8, 0x80}, {0x2e, 0x5a, 0x44}},
		textError:  {{0xff, 0x58, 0x58}, {0x6a, 0x34, 0x3a}},
		textWarn:   {{0xff, 0xc0, 0x30}, {0x6a, 0x58, 0x2c}},
	}
)

func textInk(style string, expired bool) RGB {
	c, ok := textColors[style]
	if !ok {
		c = textColors[textNormal]
	}
	if expired {
		return c[1]
	}
	return c[0]
}

// textLayout は、テキストを w×h に収まるように折り返し、行と倍率を返す。
// いちばん大きく描ける倍率（textMaxScale まで）を選ぶ。等倍でも収まらなければ、入るところまでで切り、最後に … を付ける。
// gui/src/textwidget.ts の textLayout と同じ結果にする。
func textLayout(s string, w, h int) ([]string, int) {
	paras := strings.Split(s, "\n")
	for sc := textMaxScale; sc >= 1; sc-- {
		lines := wrapText(paras, w/sc)
		if len(lines)*fontH*sc <= h {
			return lines, sc
		}
	}
	lines := wrapText(paras, w)
	n := max(h/fontH, 1)
	if len(lines) <= n {
		return lines, 1
	}
	lines = lines[:n]
	last := []rune(lines[n-1])
	for len(last) > 0 && font.textWidth(string(last)+textEllipsis) > w {
		last = last[:len(last)-1]
	}
	lines[n-1] = string(last) + textEllipsis
	return lines, 1
}

// wrapText は、段落を幅 w（等倍のドット）で折り返す。
// 空白があれば最後の空白で折り返し（空白は捨てる）、なければ文字の境目で折り返す。
func wrapText(paras []string, w int) []string {
	var out []string
	for _, p := range paras {
		line := []rune{}
		lw := 0
		for _, r := range p {
			gw, _ := font.glyphOrBox(r)
			if lw+gw > w && len(line) > 0 {
				if sp := lastSpace(line); sp > 0 {
					out = append(out, string(line[:sp]))
					line = append([]rune{}, line[sp+1:]...)
				} else {
					out = append(out, string(line))
					line = line[:0]
				}
				lw = font.textWidth(string(line))
				if r == ' ' && len(line) == 0 {
					continue // 行の頭の空白は描かない
				}
			}
			line = append(line, r)
			lw += gw
		}
		out = append(out, string(line))
	}
	return out
}

func lastSpace(line []rune) int {
	for i := len(line) - 1; i > 0; i-- {
		if line[i] == ' ' {
			return i
		}
	}
	return -1
}

// drawCentered は、1 行を area の中で左右の中央に描く。
func drawCentered(cv *Canvas, area image.Rectangle, y int, s string, scale int, c RGB) {
	x := area.Min.X + (area.Dx()-font.textWidth(s)*scale)/2
	cv.text(max(x, area.Min.X), y, s, scale, c, area)
}
