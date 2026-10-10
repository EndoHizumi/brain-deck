package main

import (
	"fmt"
	"image"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------- 電池、ストップウォッチ、ポモドーロ、カウンター、QR コード、在席ランプのウィジェット ----------
//
// ストップウォッチ、ポモドーロ、カウンター、在席ランプは、タップを自分で処理する（tapWidget）。
// 離したときに働き、0.5 秒（Todo の完了の切り替えと同じ todoHold）押し続けると、長押しの働きをする。
// 押しているあいだは、ふつうのセルと同じく枠を光らせる。状態は GadgetService（gadgets.go）が持つ。
// 描き方は gui/src/gadgetwidget.ts と同じにする（プレビューを -render-png と画素単位で比べる）。

// 色
var (
	colInkOK     = RGB{0x50, 0xd8, 0x80} // textColors の ok と同じ
	colInkWarn   = RGB{0xff, 0xc0, 0x30} // textColors の warn と同じ
	colInkError  = RGB{0xff, 0x58, 0x58} // textColors の error と同じ
	colPomoWork  = RGB{0xff, 0x70, 0x60} // ポモドーロの作業
	colPomoBreak = colInkOK              // ポモドーロの休憩
	colUnknown   = RGB{0x40, 0x48, 0x54} // 在席ランプの、セルの状態にない名前
	colQRDark    = RGB{0, 0, 0}
	colQRLight   = RGB{0xff, 0xff, 0xff}
	colQRDarkOld = RGB{0x30, 0x36, 0x3c} // 期限が切れた QR コード
	colQRLitOld  = RGB{0x8a, 0x92, 0x9c}
	colGauge     = RGB{0x96, 0xa4, 0xb4}
)

// 在席ランプの既定の状態
var defaultPresence = []PresenceSpec{{Name: "在席", Color: "#40c070"}, {Name: "集中", Color: "#ff5858"},
	{Name: "会議中", Color: "#ff8020"}, {Name: "休憩", Color: "#4f9dff"}}

// 色を書かなかった状態に、順に使う色
var presencePalette = []string{"#40c070", "#ff5858", "#ff8020", "#4f9dff", "#c070ff", "#ffd040", "#20c0c0", "#a0a8b0"}

const (
	presenceMaxStates = 12
	presenceMaxRunes  = 16
	qrMaxBytes        = 400 // QR コードにする文字列の長さの上限（UTF-8 のバイト数）
	qrNoneText        = "未設定"
	qrTooBigText      = "QR が入りません"
	stopwatchIdle     = "タップで開始"
	gaugeH            = 8 // 電池の残りの帯の高さ
)

// PresenceSpec は、設定の在席ランプの状態 1 つ（`{ name: 集中, color: "#ff5858" }`）。
type PresenceSpec struct {
	Name  string `yaml:"name" json:"name"`
	Color string `yaml:"color,omitempty" json:"color,omitempty"`
}

// PresenceDef は組み立て済みの状態。
type PresenceDef struct {
	Name  string
	Color RGB
}

func (c RGB) hex() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

func parseColor(s string) (RGB, bool) {
	var c RGB
	if len(s) != 7 || s[0] != '#' {
		return c, false
	}
	if _, err := fmt.Sscanf(s[1:], "%02x%02x%02x", &c.R, &c.G, &c.B); err != nil {
		return c, false
	}
	return c, strings.ToLower(s) == c.hex()
}

// compilePresenceStates は、在席ランプの状態を検証する。書かなければ既定の 4 つ。
func compilePresenceStates(specs []PresenceSpec) ([]PresenceDef, error) {
	if specs == nil {
		specs = defaultPresence
	}
	if len(specs) == 0 || len(specs) > presenceMaxStates {
		return nil, fmt.Errorf("states must have 1..%d entries such as { name: 集中, color: \"#ff5858\" }", presenceMaxStates)
	}
	seen := map[string]bool{}
	out := make([]PresenceDef, len(specs))
	for i, st := range specs {
		if err := validPresenceName(st.Name); err != nil {
			return nil, fmt.Errorf("states[%d]: %v", i, err)
		}
		if seen[st.Name] {
			return nil, fmt.Errorf("states[%d]: %q appears twice", i, st.Name)
		}
		seen[st.Name] = true
		col := st.Color
		if col == "" {
			col = presencePalette[i%len(presencePalette)]
		}
		c, ok := parseColor(col)
		if !ok {
			return nil, fmt.Errorf("states[%d]: color must be #rrggbb, got %q", i, st.Color)
		}
		out[i] = PresenceDef{Name: st.Name, Color: c}
	}
	return out, nil
}

// validPresenceName は、状態の名前が使えるか（1〜16 文字、制御文字なし）。
func validPresenceName(s string) error {
	n := utf8.RuneCountInString(s)
	if n == 0 || n > presenceMaxRunes || strings.TrimSpace(s) != s {
		return fmt.Errorf("name must be 1..%d characters without leading or trailing spaces, got %q", presenceMaxRunes, s)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("name must not contain control characters, got %q", s)
		}
	}
	return nil
}

func pomoDuration(name, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < minPomoPhase || d > maxPomoPhase || d%time.Second != 0 {
		return 0, fmt.Errorf("%s must be a whole number of seconds from %v to %v (such as 25m or 5m)", name, minPomoPhase, maxPomoPhase)
	}
	return d, nil
}

// isTapWidget は、タップと長押しを自分で処理するウィジェットか。
func isTapWidget(kind string) bool {
	return kind == widgetStopwatch || kind == widgetPomodoro || kind == widgetCounter || kind == widgetPresence
}

// hasStateID は、id を書けるウィジェットか。
func hasStateID(kind string) bool {
	return kind == widgetText || kind == widgetQR || kind == widgetGraph || isTapWidget(kind)
}

var tapHelp = map[string]string{
	widgetStopwatch: "tap to start or stop, long press to reset",
	widgetPomodoro:  "tap to start or pause, long press to reset",
	widgetCounter:   "tap to add 1, long press to reset",
	widgetPresence:  "tap for the next state, long press for the previous one",
}

func (w *WidgetDef) tapWidget() bool { return w != nil && w.tap != nil }

// handlesTouch は、ウィジェットがタップを受け取るか（押した位置で働くものと、タップと長押しを自分で処理するもの）。
func (w *WidgetDef) handlesTouch() bool { return w.ownsTouch() || w.tapWidget() }

// tapState は、タップで操作するセル 1 つの、押しているあいだの状態。
type tapState struct {
	mu    sync.Mutex
	token uint64
	timer *time.Timer
	down  bool
	long  bool // 長押しの働きをした（離しても何もしない）
}

// tapDown は、押したとき。0.5 秒押し続けたら長押しの働きをする。待たずに返る。
func (w *WidgetDef) tapDown(rt *WidgetRT) {
	s := w.tap
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token++
	tok := s.token
	s.down, s.long = true, false
	s.timer = time.AfterFunc(todoHold, func() {
		s.mu.Lock()
		if s.token != tok || !s.down {
			s.mu.Unlock()
			return
		}
		s.long = true
		s.mu.Unlock()
		rt.tapAction(w, true)
	})
}

// tapUp は、離したとき（cancel なら、設定の差し替えなどで押していたものを捨てたとき）。
func (w *WidgetDef) tapUp(rt *WidgetRT, cancel bool) {
	s := w.tap
	s.mu.Lock()
	s.token++
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	act := s.down && !s.long && !cancel
	s.down = false
	s.mu.Unlock()
	if act {
		rt.tapAction(w, false)
	}
}

// tapAction は、タップ（long なら長押し）の働き。
func (rt *WidgetRT) tapAction(w *WidgetDef, long bool) {
	g := rt.gadgets
	if g == nil {
		return
	}
	switch w.Kind {
	case widgetStopwatch:
		if long {
			g.StopwatchReset(w.ID)
		} else {
			g.StopwatchToggle(w.ID)
		}
	case widgetPomodoro:
		if long {
			g.PomodoroReset(w.ID)
		} else {
			g.PomodoroTap(w.ID, w.Work, w.Break)
		}
	case widgetCounter:
		if long {
			g.CounterReset(w.ID)
		} else {
			g.CounterAdd(w.ID, 1)
		}
	case widgetPresence:
		dir := 1
		if long {
			dir = -1
		}
		g.PresenceStep(w.ID, w.States, dir, "brain")
	}
	vlogf("widget: %s %s %s", w.Kind, w.ID, map[bool]string{false: "tap", true: "long press"}[long])
}

// ---------- 中身 ----------

// stopwatchText は経過時間を書く。1 時間までは「分:秒」、それより長ければ「時:分:秒」。
func stopwatchText(d time.Duration) string {
	s := int64(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// pomoText は残りの時間を「分:秒」で書く。秒は切り上げる（0:00 になった瞬間に切り替わる）。
func pomoText(d time.Duration) string {
	s := int64((d + time.Second - 1) / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// presenceView は、在席ランプに出す名前と色。状態がまだなければ、最初の状態。
// 状態がセルの状態にないとき（ほかのセルの状態にした）は、名前を灰色で出す。
func presenceView(w *WidgetDef, env WidgetEnv) (string, RGB) {
	st, ok := env.Gadgets.Presence[w.ID]
	if !ok || st.State == "" {
		return w.States[0].Name, w.States[0].Color
	}
	if i := presenceIndex(w.States, st.State); i >= 0 {
		return st.State, w.States[i].Color
	}
	return st.State, colUnknown
}

// contrastInk は、色 c の上に描く文字の色（黒か白）。
func contrastInk(c RGB) RGB {
	if 299*int(c.R)+587*int(c.G)+114*int(c.B) >= 140*1000 {
		return RGB{0, 0, 0}
	}
	return colText
}

// widgetFill は、セルの塗りをウィジェットが決めるとき、その色を返す（在席ランプ、点滅しているポモドーロ）。
func widgetFill(v *CellView, env WidgetEnv) (RGB, bool) {
	w := v.Widget
	if w == nil {
		return RGB{}, false
	}
	switch w.Kind {
	case widgetPresence:
		_, c := presenceView(w, env)
		return c, true
	case widgetPomodoro:
		if pv := env.Gadgets.Pomodoro[w.ID].view(env.Now, w.Work); pv.Alert && pv.BlinkOn {
			return pomoInk(pv.Phase), true
		}
	}
	return RGB{}, false
}

func pomoInk(phase string) RGB {
	if phase == pomoPhaseBreak {
		return colPomoBreak
	}
	return colPomoWork
}

// batteryLines は、電池のウィジェットに出す 2 行と、数字の色、帯の割合（-1 なら帯を出さない）。
func batteryLines(b *BatteryInfo, ink RGB) (main, sub string, c RGB, pct int) {
	if b == nil || b.Percent < 0 {
		return "--%", "電池を読めません", colSub, -1
	}
	main = fmt.Sprintf("%d%%", b.Percent)
	c = ink
	switch {
	case b.Charging:
		sub, c = "充電中", colInkOK
	case b.Plugged:
		sub = "USB 給電"
	default:
		sub = "電池"
	}
	if !b.Charging {
		switch {
		case b.Percent <= 15:
			c = colInkError
		case b.Percent <= 30:
			c = colInkWarn
		}
	}
	return main, sub, c, b.Percent
}

// gadgetKey は、新しいウィジェットの描き直しを決める中身（widgetKey から呼ぶ）。
func gadgetKey(v *CellView, env WidgetEnv) string {
	w := v.Widget
	switch w.Kind {
	case widgetBattery:
		m, s, _, _ := batteryLines(env.Battery, colText)
		return m + "\x00" + s
	case widgetStopwatch:
		st := env.Gadgets.Stopwatch[w.ID]
		return fmt.Sprintf("%d\x00%v", st.Elapsed(env.Now)/time.Second, st.StartedAt != nil)
	case widgetPomodoro:
		pv := env.Gadgets.Pomodoro[w.ID].view(env.Now, w.Work)
		return fmt.Sprintf("%s\x00%s\x00%v\x00%v\x00%d\x00%v\x00%v", pomoText(pv.Remaining), pv.Phase, pv.Running, pv.Started, pv.Count, pv.Alert, pv.BlinkOn)
	case widgetCounter:
		return fmt.Sprint(env.Gadgets.Counter[w.ID].Value)
	case widgetPresence:
		n, c := presenceView(w, env)
		return n + "\x00" + c.hex()
	case widgetQR:
		e, ok := env.QR[w.ID]
		if !ok {
			return "\x00none"
		}
		return fmt.Sprintf("%s\x00%v", e.Text, e.Expired(env.Now))
	}
	return ""
}

// gadgetNext は、新しいウィジェットの中身が次に変わる時刻（widgetNext から呼ぶ）。ゼロなら、時間では変わらない。
func gadgetNext(v *CellView, env WidgetEnv) time.Time {
	w := v.Widget
	now := env.Now
	switch w.Kind {
	case widgetStopwatch:
		st := env.Gadgets.Stopwatch[w.ID]
		if st.StartedAt != nil {
			el := st.Elapsed(now)
			return now.Add(time.Second - el%time.Second)
		}
	case widgetPomodoro:
		if pv := env.Gadgets.Pomodoro[w.ID].view(now, w.Work); pv.Running {
			return pv.Next
		}
	case widgetQR:
		if e, ok := env.QR[w.ID]; ok && e.ExpiresAt != nil && now.Before(*e.ExpiresAt) {
			return *e.ExpiresAt
		}
	}
	return time.Time{}
}

// drawTwoLines は、大きな 1 行（最大 clockMaxScale 倍）と、その下の小さな 1 行（最大 dateMaxScale 倍）を、
// area の上下の中央に描く。b が空なら 1 行だけ。時計と同じ配置。
func drawTwoLines(cv *Canvas, area image.Rectangle, a, b string, inkA, inkB RGB) {
	bh, ds := 0, 0
	if b != "" {
		ds = min(fitScaleMax([]string{b}, area.Dx(), fontH*dateMaxScale, dateMaxScale), dateMaxScale)
		bh = fontH*ds + widgetLineGap
	}
	ts := fitScaleMax([]string{a}, area.Dx(), area.Dy()-bh, clockMaxScale)
	y := area.Min.Y + (area.Dy()-fontH*ts-bh)/2
	drawCentered(cv, area, y, a, ts, inkA)
	if b != "" {
		drawCentered(cv, area, y+fontH*ts+widgetLineGap, b, ds, inkB)
	}
}

// drawGadget は、新しいウィジェットの中身を、見出しの下の範囲 area に描く。
// ink、subInk はふだんの文字の色（押したとき fill なら黒）。filled なら、セルの塗りをウィジェットが決めている。
func drawGadget(cv *Canvas, area image.Rectangle, v *CellView, env WidgetEnv, ink, subInk RGB) {
	w := v.Widget
	pressed := ink != colText
	switch w.Kind {
	case widgetBattery:
		m, s, c, pct := batteryLines(env.Battery, ink)
		if pressed {
			c = ink
		}
		if pct >= 0 && area.Dy() > gaugeH*3 {
			g := image.Rect(area.Min.X, area.Max.Y-gaugeH, area.Max.X, area.Max.Y)
			cv.frame(g, 1, subInk)
			in := g.Inset(2)
			if fw := in.Dx() * pct / 100; fw > 0 {
				gc := c
				if pressed {
					gc = subInk
				}
				cv.fill(image.Rect(in.Min.X, in.Min.Y, in.Min.X+fw, in.Max.Y), gc)
			}
			area.Max.Y -= gaugeH + widgetLineGap
		}
		drawTwoLines(cv, area, m, s, c, subInk)
	case widgetStopwatch:
		st := env.Gadgets.Stopwatch[w.ID]
		el := st.Elapsed(env.Now)
		c, sub := ink, "計測中"
		switch {
		case st.StartedAt == nil && el < time.Second:
			c, sub = subInk, stopwatchIdle
		case st.StartedAt == nil:
			c, sub = colInkWarn, "停止中"
		}
		if pressed {
			c = ink
		}
		drawTwoLines(cv, area, stopwatchText(el), sub, c, subInk)
	case widgetPomodoro:
		pv := env.Gadgets.Pomodoro[w.ID].view(env.Now, w.Work)
		phase := "作業"
		if pv.Phase == pomoPhaseBreak {
			phase = "休憩"
		}
		var c RGB
		var sub string
		switch {
		case !pv.Started:
			c, sub = ink, stopwatchIdle
		case pv.Running:
			c, sub = pomoInk(pv.Phase), phase+"中"
		default:
			c, sub = subInk, "一時停止 "+phase
		}
		if pv.Count > 0 {
			sub += fmt.Sprintf(" %d回", pv.Count)
		}
		if pressed {
			c = ink
		}
		if pv.Alert && pv.BlinkOn && !pressed {
			c = contrastInk(pomoInk(pv.Phase))
			subInk = c
		}
		drawTwoLines(cv, area, pomoText(pv.Remaining), sub, c, subInk)
	case widgetCounter:
		drawTwoLines(cv, area, fmt.Sprint(env.Gadgets.Counter[w.ID].Value), "", ink, subInk)
	case widgetPresence:
		name, _ := presenceView(w, env)
		s := min(fitScaleMax([]string{name}, area.Dx(), area.Dy(), textMaxScale), textMaxScale)
		drawCentered(cv, area, area.Min.Y+(area.Dy()-fontH*s)/2, name, s, ink)
	case widgetQR:
		e, ok := env.QR[w.ID]
		if !ok {
			s := min(fitScale([]string{qrNoneText}, area.Dx(), area.Dy()), 2)
			drawCentered(cv, area, area.Min.Y+(area.Dy()-fontH*s)/2, qrNoneText, s, subInk)
			return
		}
		dark, light := colQRDark, colQRLight
		if e.Expired(env.Now) {
			dark, light = colQRDarkOld, colQRLitOld
		}
		if !drawQR(cv, area, e.Text, dark, light) {
			s := min(fitScale([]string{qrTooBigText}, area.Dx(), area.Dy()), 2)
			drawCentered(cv, area, area.Min.Y+(area.Dy()-fontH*s)/2, qrTooBigText, s, colInkWarn)
		}
	}
}

// qrFit は、size 升の QR コードを area に描くときの、1 升のドット数と白い縁の升の数。入らなければ 0。
// 縁は 4 升（規格）が入れば 4、入らなければ 2。1 升は 2 ドット以上（1 ドットでは読み取りにくい）。
func qrFit(size, aw, ah int) (px, quiet int) {
	for _, q := range []int{4, 2} {
		if p := min(aw, ah) / (size + 2*q); p >= 2 {
			return p, q
		}
	}
	return 0, 0
}

// drawQR は text の QR コードを area の中央に描く。入らなければ false。
func drawQR(cv *Canvas, area image.Rectangle, text string, dark, light RGB) bool {
	q, err := EncodeQR(text)
	if err != nil {
		return false
	}
	px, quiet := qrFit(q.Size, area.Dx(), area.Dy())
	if px == 0 {
		return false
	}
	total := (q.Size + 2*quiet) * px
	x0 := area.Min.X + (area.Dx()-total)/2
	y0 := area.Min.Y + (area.Dy()-total)/2
	cv.fill(image.Rect(x0, y0, x0+total, y0+total), light)
	ox, oy := x0+quiet*px, y0+quiet*px
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if q.Dark(x, y) {
				cv.fill(image.Rect(ox+x*px, oy+y*px, ox+(x+1)*px, oy+(y+1)*px), dark)
			}
		}
	}
	return true
}
