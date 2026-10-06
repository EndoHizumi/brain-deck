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

var widgetKinds = []string{widgetClock}

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
	// tap は、key や layer_* を書いていないセルをタップしたときの動き。nil なら何もしない（光らせない）。
	// 入力の goroutine から呼ばれるので、待たずに返ること
	tap func(*WidgetDef)
}

// WidgetEnv は、ウィジェットを描くときの外の状態。描画の goroutine が描き直すたびに作る。
type WidgetEnv struct {
	Now        time.Time
	TimeSynced bool // この起動のあいだに時刻を合わせた（set_time か NTP）
}

// compileWidget は、ウィジェットのセルの書き方を検証して組み立てる。
func compileWidget(s ActionSpec) (*WidgetDef, error) {
	switch s.Widget {
	case widgetClock:
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
	}
	return ""
}

// widgetNext は、セルの中身が次に変わる時刻を返す。
func widgetNext(v *CellView, now time.Time) time.Time {
	switch v.Widget.Kind {
	case widgetClock:
		if v.Widget.Seconds {
			return now.Truncate(time.Second).Add(time.Second)
		}
		// 分の切り替わり。どのタイムゾーンも分単位でずれているので、UTC で切ってよい
		return now.Truncate(time.Minute).Add(time.Minute)
	}
	return time.Time{}
}

// drawWidget は、セルの内側 inner にウィジェットを描く。label があれば上に小さく出す。
// ink、subInk は、押したとき（fill）に色を変えるためのもの。
func drawWidget(cv *Canvas, inner image.Rectangle, v *CellView, env WidgetEnv, ink, subInk RGB) {
	area := inner
	if v.Label != "" {
		caption := strings.ReplaceAll(v.Label, "\n", " ")
		s := min(fitScale([]string{caption}, area.Dx(), fontH*captionScale), captionScale)
		drawCentered(cv, area, area.Min.Y, caption, s, subInk)
		area.Min.Y += fontH*s + widgetLineGap
	}
	switch v.Widget.Kind {
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
	}
}

// drawCentered は、1 行を area の中で左右の中央に描く。
func drawCentered(cv *Canvas, area image.Rectangle, y int, s string, scale int, c RGB) {
	x := area.Min.X + (area.Dx()-font.textWidth(s)*scale)/2
	cv.text(max(x, area.Min.X), y, s, scale, c, area)
}
