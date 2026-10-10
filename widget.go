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
	widgetClock     = "clock"
	widgetText      = "text"
	widgetTodo      = "todo"
	widgetCal       = "calendar"
	widgetBattery   = "battery"
	widgetStopwatch = "stopwatch"
	widgetPomodoro  = "pomodoro"
	widgetCounter   = "counter"
	widgetQR        = "qr"
	widgetPresence  = "presence"
	widgetMonth     = "month"
	widgetWeather   = "weather"
	widgetGraph     = "graph"
	widgetPresenter = "presenter"
	widgetCalc      = "calculator"
	// widgetPenTab（pentablet）は pen.go、widgetMemo（memo）は memo.go

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

var widgetKinds = []string{widgetClock, widgetText, widgetTodo, widgetCal, widgetPad, widgetBattery, widgetStopwatch, widgetPomodoro,
	widgetCounter, widgetQR, widgetPresence, widgetMonth, widgetWeather, widgetGraph, widgetPresenter, widgetCalc, widgetPenTab, widgetMemo, widgetReader}

// ページを送るウィジェット（Todo、カレンダー、月の表示）が、触らなければ最初のページ（今月）に戻るまでの時間
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
	Format     string           // clock：時刻の行
	DateFormat string           // clock：日付の行。空なら出さない
	Loc        *time.Location   // clock：nil なら Brain のタイムゾーン
	Seconds    bool             // clock：秒を出す（1 秒ごとに描き直す）
	ID         string           // text、qr：中身の名前。stopwatch、pomodoro、counter、presence：状態の名前（省略すると種類の名前）。month：follow で指す名前
	Rows       int              // todo、calendar：1 ページの行数。0 なら高さで決める
	PageReset  time.Duration    // todo、calendar、month：最後に触ってから、最初のページ（今月と今日）に戻るまでの時間。0 なら戻らない
	Stale      time.Duration    // calendar、weather：最終更新がこれより古ければ、古いと出す
	Calendars  []string         // calendar：出すカレンダーの名前。month：印を付けるカレンダー。空ならすべて
	Follow     string           // calendar：選んだ日を見る月の表示の id。空なら今日
	follow     *WidgetDef       // calendar：Follow の月の表示（compileKeymap が同じレイヤーから探して結ぶ）
	WeekStart  time.Weekday     // month：週の始まり（日曜か月曜）
	month      *monthState      // month：出している月と、選んだ日
	pager      *pagerState      // todo、calendar：ページと、押している行
	Pad        *PadParams       // trackpad：判定の設定
	Work       time.Duration    // pomodoro：作業の時間
	Break      time.Duration    // pomodoro：休憩の時間
	States     []PresenceDef    // presence：状態（タップで順に切り替える）
	tap        *tapState        // stopwatch、pomodoro、counter、presence：タップと長押し
	Graph      *GraphDef        // graph：単位、範囲、描き方
	Duration   time.Duration    // presenter：持ち時間
	Colors     []PresenterColor // presenter：残り時間に応じた色（残りの長い順）
	Prefix     bool             // calculator：16 進と 2 進の結果を、0x と 0b を付けて打ち込む
	calc       *calcCell        // calculator：押しているボタン
	home       *HomeDef         // home：ホーム画面（設定の home から作る）
	PenTab     *PenTabDef       // pentablet：当てはめる範囲と筆圧
	Memo       *MemoDef         // memo：線の読み取りの設定
}

// WidgetEnv は、ウィジェットを描くときの外の状態。描画の goroutine が描き直すたびに作る。
type WidgetEnv struct {
	Now        time.Time
	TimeSynced bool                 // この起動のあいだに時刻を合わせた（set_time か NTP）
	Texts      map[string]TextEntry // text：id ごとの中身（写し）
	Todo       TodoList             // todo：一覧（写し）
	Calendar   *CalendarData        // calendar：予定（写し。書き換えない）。nil なら一度も受け取っていない
	MouseOff   bool                 // trackpad：USB にマウスがない（キーボードだけの形）
	Gadgets    gadgetFile           // stopwatch、pomodoro、counter、presence：状態（写し）
	QR         map[string]TextEntry // qr：id ごとの中身（写し）
	Battery    *BatteryInfo         // battery：電池の状態。nil なら読めない
	Weather    *WeatherData         // weather：予報（写し。書き換えない）。nil なら一度も受け取っていない
	// WeatherIcons は、天気アイコンの読み込み先。nil ならアイコンを描かない
	WeatherIcons *IconStore
	Graph        map[string]GraphSeries // graph：id ごとの値（写し。書き換えない）
	Calc         map[string]CalcState   // calculator：id ごとの状態（写し）
	Link         string                 // 状態の帯：PC とのつながり（UDC の state。分からなければ空）
	LastSync     *time.Time             // 状態の帯：最後に PC から時刻を合わせた時刻。nil なら一度もない
	Memo         *MemoView              // memo：今のページ（写し）。nil ならメモがない
	Reader       *ReaderView            // reader：読み物のアプリの状態（写し）
	Syncing      bool                   // 状態の帯：PC の brain-deck sync が同期している
	SyncFailed   bool                   // 状態の帯：最後の同期に失敗したものがあった
}

// compileWidget は、ウィジェットのセルの書き方を検証して組み立てる。
func compileWidget(s ActionSpec) (*WidgetDef, error) {
	switch s.Widget {
	case widgetPenTab:
		return compilePenTab(s)
	case widgetMemo:
		return compileMemo(s)
	case widgetReader:
		if s.Map != "" || s.PCScreen != "" || s.Region != nil || s.Pressure != nil || s.PressureRange != nil || s.hasPadFields() {
			return nil, fmt.Errorf("widget: reader has no settings (the size and the writing direction are chosen on the Brain)")
		}
		return compileReader(s)
	}
	if s.Map != "" || s.PCScreen != "" || s.Region != nil || s.Pressure != nil || s.PressureRange != nil {
		return nil, fmt.Errorf("map, pc_screen, region, pressure and pressure_range are for widget: pentablet")
	}
	if s.Widget == widgetPad {
		if s.Work != "" || s.Break != "" || s.States != nil {
			return nil, fmt.Errorf("work, break and states cannot be used with widget: trackpad")
		}
		return compilePad(s)
	}
	if s.hasPadFields() {
		return nil, fmt.Errorf("speed, accel, scroll_*, settle_ms, smooth, deadzone, min_pressure, tap_*, drag_ms and long_press are for widget: trackpad")
	}
	paged := s.Widget == widgetTodo || s.Widget == widgetCal
	if s.Rows != 0 && !paged {
		return nil, fmt.Errorf("rows is for widget: todo and calendar")
	}
	if s.PageReset != "" && !paged && s.Widget != widgetMonth {
		return nil, fmt.Errorf("page_reset is for widget: todo, calendar and month")
	}
	if s.Stale != "" && s.Widget != widgetCal && s.Widget != widgetWeather && s.Widget != widgetGraph {
		return nil, fmt.Errorf("stale is for widget: calendar, weather and graph")
	}
	if s.hasGraphFields() && s.Widget != widgetGraph {
		return nil, fmt.Errorf("unit, min, max, chart, points and decimals are for widget: graph")
	}
	if s.Calendars != nil && s.Widget != widgetCal && s.Widget != widgetMonth {
		return nil, fmt.Errorf("calendars is for widget: calendar and month")
	}
	if s.Follow != "" && s.Widget != widgetCal {
		return nil, fmt.Errorf("follow is for widget: calendar (the id of a widget: month cell in the same layer)")
	}
	if s.WeekStart != "" && s.Widget != widgetMonth {
		return nil, fmt.Errorf("week_start is for widget: month")
	}
	if s.Prefix && s.Widget != widgetCalc {
		return nil, fmt.Errorf("prefix is for widget: calculator")
	}
	if (s.Duration != "" || s.Colors != nil) && s.Widget != widgetPresenter {
		return nil, fmt.Errorf("duration and colors are for widget: presenter")
	}
	if (s.Work != "" || s.Break != "") && s.Widget != widgetPomodoro {
		return nil, fmt.Errorf("work and break are for widget: pomodoro")
	}
	if s.States != nil && s.Widget != widgetPresence {
		return nil, fmt.Errorf("states is for widget: presence")
	}
	if isTapWidget(s.Widget) && s.count() > 0 {
		return nil, fmt.Errorf("widget: %s handles taps itself (%s); remove key, layer_*, mouse, usb_mode and terminal", s.Widget, tapHelp[s.Widget])
	}
	if s.Widget != widgetClock && s.Widget != "" && (s.Format != "" || s.DateFormat != "" || s.TZ != "") {
		if paged {
			return nil, fmt.Errorf("format, date_format, tz and id cannot be used with widget: %s", s.Widget)
		}
		return nil, fmt.Errorf("format, date_format and tz are for widget: clock")
	}
	if s.ID != "" && !hasStateID(s.Widget) && s.Widget != widgetMonth && s.Widget != widgetCalc {
		switch {
		case paged:
			return nil, fmt.Errorf("format, date_format, tz and id cannot be used with widget: %s", s.Widget)
		case s.Widget == "":
			return nil, fmt.Errorf("id needs widget: text, qr, graph, stopwatch, pomodoro, counter, presence, presenter, calculator or month")
		}
		return nil, fmt.Errorf("id is for widget: text, qr, graph, stopwatch, pomodoro, counter, presence, presenter, calculator and month")
	}
	switch s.Widget {
	case widgetClock:
	case widgetMonth:
		return compileMonth(s)
	case widgetTodo, widgetCal:
		if s.count() > 0 {
			if s.Widget == widgetTodo {
				return nil, fmt.Errorf("widget: todo handles taps itself (long press an item to check it, ▲▼ to turn pages); remove key and layer_*")
			}
			return nil, fmt.Errorf("widget: calendar handles taps itself (▲▼ to turn pages); remove key and layer_*")
		}
		if s.Rows < 0 || s.Rows > todoMaxRows {
			return nil, fmt.Errorf("rows must be 1..%d (omit it to fit the cell height)", todoMaxRows)
		}
		w := &WidgetDef{Kind: s.Widget, Rows: s.Rows, pager: &pagerState{}}
		var err error
		if w.PageReset, err = parsePageReset(s.PageReset); err != nil {
			return nil, err
		}
		if s.Widget == widgetCal {
			w.Stale = defaultCalStale
			if s.Stale != "" {
				d, err := parseStale(s.Stale)
				if err != nil {
					return nil, err
				}
				w.Stale = d
			}
			if err := checkCalNames(s.Calendars); err != nil {
				return nil, err
			}
			w.Calendars = s.Calendars
			if s.Follow != "" && !validTextID(s.Follow) {
				return nil, fmt.Errorf("follow must be the id of a widget: month cell (1-32 characters of A-Z a-z 0-9 _ . -), got %q", s.Follow)
			}
			w.Follow = s.Follow
		}
		return w, nil
	case widgetText, widgetQR:
		if !validTextID(s.ID) {
			return nil, fmt.Errorf("widget: %s needs id (1-32 characters of A-Z a-z 0-9 _ . -), got %q", s.Widget, s.ID)
		}
		return &WidgetDef{Kind: s.Widget, ID: s.ID}, nil
	case widgetBattery:
		return &WidgetDef{Kind: s.Widget}, nil
	case widgetGraph:
		return compileGraph(s)
	case widgetCalc:
		return compileCalc(s)
	case widgetWeather:
		w := &WidgetDef{Kind: s.Widget, Stale: defaultWthStale}
		if s.Stale != "" {
			d, err := parseStale(s.Stale)
			if err != nil {
				return nil, err
			}
			w.Stale = d
		}
		return w, nil
	case widgetStopwatch, widgetCounter, widgetPomodoro, widgetPresence, widgetPresenter:
		id := widgetStateID(s)
		if !validTextID(id) {
			return nil, fmt.Errorf("id must be 1-32 characters of A-Z a-z 0-9 _ . - (omit it to use %q), got %q", s.Widget, s.ID)
		}
		w := &WidgetDef{Kind: s.Widget, ID: id, tap: &tapState{}}
		switch s.Widget {
		case widgetPomodoro:
			var err error
			if w.Work, err = pomoDuration("work", s.Work, defaultWork); err != nil {
				return nil, err
			}
			if w.Break, err = pomoDuration("break", s.Break, defaultBreak); err != nil {
				return nil, err
			}
		case widgetPresence:
			states, err := compilePresenceStates(s.States)
			if err != nil {
				return nil, err
			}
			w.States = states
		case widgetPresenter:
			if err := compilePresenter(w, s); err != nil {
				return nil, err
			}
		}
		return w, nil
	case "":
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

// parsePageReset は page_reset を読む。空なら既定の 1 分、off なら 0（戻らない）。
func parsePageReset(s string) (time.Duration, error) {
	switch s {
	case "":
		return defaultPageReset, nil
	case pageResetOff:
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < minPageReset || d > maxPageReset {
		return 0, fmt.Errorf("page_reset must be a duration from %v to %v (such as 30s or 2m), or off", minPageReset, maxPageReset)
	}
	return d, nil
}

// parseStale は stale（1 分〜30 日）を読む。
func parseStale(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute || d > 30*24*time.Hour {
		return 0, fmt.Errorf("stale must be a duration from 1m to 720h (such as 3h)")
	}
	return d, nil
}

// checkCalNames は、calendars に同じ名前や空の名前がないことを確かめる。
func checkCalNames(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || seen[n] {
			return fmt.Errorf("calendars must be a list of distinct calendar names, got %q", names)
		}
		seen[n] = true
	}
	return nil
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
	case widgetMonth:
		return monthKey(w, env)
	case widgetWeather:
		return wthKey(w, env)
	case widgetGraph:
		return graphKey(w, env)
	case widgetCalc:
		return calcKey(w, env)
	case widgetPad:
		return fmt.Sprintf("trackpad\x00%v", env.MouseOff) // マウスのオンとオフで描き直す
	case widgetHome:
		return fmt.Sprintf("home\x00%d", w.home.Page())
	case widgetPenTab:
		return penTabKey(w.PenTab, env)
	case widgetMemo:
		return memoKey(w, env)
	case widgetReader:
		return readerKey(env)
	}
	return gadgetKey(v, env)
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
	case widgetMonth:
		return monthNext(v.Widget, env)
	case widgetWeather:
		return wthNext(v.Widget, env)
	case widgetGraph:
		return graphNext(v.Widget, env)
	case widgetHome, widgetPenTab, widgetMemo, widgetReader:
		return time.Time{}
	}
	return gadgetNext(v, env)
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
	case widgetMonth:
		drawMonth(cv, area, v.Widget, env)
	case widgetWeather:
		drawWeather(cv, area, v.Widget, env)
	case widgetGraph:
		drawGraph(cv, area, v.Widget, env, ink, subInk)
	case widgetCalc:
		drawCalc(cv, area, v.Widget, env)
	case widgetPenTab:
		drawPenTab(cv, area, v.Widget.PenTab, env)
	case widgetMemo:
		drawMemo(cv, area, env)
	case widgetReader:
		drawReader(cv, area, env)
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
	case widgetBattery, widgetStopwatch, widgetPomodoro, widgetCounter, widgetPresence, widgetQR, widgetPresenter:
		drawGadget(cv, area, v, env, ink, subInk)
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
// 月の表示で選んだ日の予定を出しているカレンダーは、label にその日付を足す。
func widgetLabel(w *WidgetDef, label string, env WidgetEnv) string {
	switch w.Kind {
	case widgetTodo:
		return todoCaption(label, env.Todo.Items)
	case widgetCal:
		if d, ok := calFollowDay(w, env); ok {
			return strings.TrimSpace(label + " " + calDate(d))
		}
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
