package main

import (
	"fmt"
	"image"
	"sort"
	"time"
)

// ---------- プレゼン用タイマー（widget: presenter） ----------
//
// 持ち時間（duration）から、残り時間を大きく、経過時間を小さく出し、下に進み具合の帯を出す。
// タップで開始と一時停止、0.5 秒の長押しで 0 に戻す（ストップウォッチと同じ。状態も同じ形で widgets.json に残す）。
// 残り時間が colors の区切りを下回ると、文字と帯の色を変える（既定は残り 5 分で黄、1 分で赤）。
// 持ち時間を過ぎたら「+0:12」のように超えた時間を赤で出し、動いているあいだはセルを 0.5 秒ごとに赤く点滅させる。
// 動いているあいだは、自動減光をしない（backlight.go の Dimmer.SetBusy）。
// 描き方は gui/src/presenter.ts と同じにする（プレビューを -render-png と画素単位で比べる）。

const (
	defaultPresenterDuration = 10 * time.Minute
	minPresenterDuration     = 10 * time.Second
	maxPresenterDuration     = 24 * time.Hour
	presenterMaxColors       = 6
	presenterBlink           = 500 * time.Millisecond
	presenterIdle            = "タップで開始"
)

// 既定の色の区切り（フェーズ E のあとで決めたこと：残り 5 分で黄、1 分で赤）
var defaultPresenterColors = []PresenterColorSpec{{Remaining: "5m", Color: "#ffc030"}, {Remaining: "1m", Color: "#ff5858"}}

// PresenterColorSpec は、設定の色の区切り 1 つ（`{ remaining: 5m, color: "#ffc030" }`）。
type PresenterColorSpec struct {
	Remaining string `yaml:"remaining" json:"remaining"`
	Color     string `yaml:"color" json:"color"`
}

// PresenterColor は組み立て済みの区切り。残りが Remaining 以下になったら Color にする。
type PresenterColor struct {
	Remaining time.Duration
	Color     RGB
}

// compilePresenter は duration と colors を確かめる。
func compilePresenter(w *WidgetDef, s ActionSpec) error {
	w.Duration = defaultPresenterDuration
	if s.Duration != "" {
		d, err := time.ParseDuration(s.Duration)
		if err != nil || d < minPresenterDuration || d > maxPresenterDuration || d%time.Second != 0 {
			return fmt.Errorf("duration must be a whole number of seconds from %v to %v (such as 20m or 5m)", minPresenterDuration, maxPresenterDuration)
		}
		w.Duration = d
	}
	specs := s.Colors
	if specs == nil {
		specs = defaultPresenterColors
	}
	if len(specs) > presenterMaxColors {
		return fmt.Errorf("colors must have at most %d entries such as { remaining: 5m, color: \"#ffc030\" }", presenterMaxColors)
	}
	seen := map[time.Duration]bool{}
	for i, c := range specs {
		d, err := time.ParseDuration(c.Remaining)
		if err != nil || d <= 0 || d > maxPresenterDuration || d%time.Second != 0 {
			return fmt.Errorf("colors[%d]: remaining must be a whole number of seconds such as 5m or 30s, got %q", i, c.Remaining)
		}
		if seen[d] {
			return fmt.Errorf("colors[%d]: remaining %v appears twice", i, d)
		}
		seen[d] = true
		rgb, ok := parseColor(c.Color)
		if !ok {
			return fmt.Errorf("colors[%d]: color must be #rrggbb, got %q", i, c.Color)
		}
		w.Colors = append(w.Colors, PresenterColor{Remaining: d, Color: rgb})
	}
	sort.Slice(w.Colors, func(i, j int) bool { return w.Colors[i].Remaining > w.Colors[j].Remaining })
	return nil
}

// presView は、プレゼン用タイマーの now の時点の様子。
type presView struct {
	Main     string // 残り（「12:34」）か、超えた時間（「+0:12」）
	Sub      string // 状態と経過時間
	Color    RGB    // Main と帯の色
	HasColor bool   // false なら、ふだんの文字の色
	Over     bool
	Running  bool
	BlinkOn  bool // 点滅の、塗る側
	Bar      int  // 帯の長さ（千分率）
	Next     time.Time
}

// presenterView は、セルの設定と状態から、今の様子を求める。
func presenterView(w *WidgetDef, env WidgetEnv) presView {
	st := env.Gadgets.Presenter[w.ID]
	now := env.Now
	el := st.Elapsed(now)
	rem := w.Duration - el
	v := presView{Running: st.StartedAt != nil, Bar: int(min(el*1000/w.Duration, 1000))}
	elapsed := "経過 " + stopwatchText(el)
	if rem > 0 {
		v.Main = stopwatchText((rem + time.Second - 1) / time.Second * time.Second) // 秒を切り上げる（0:00 になった瞬間に超える）
		for _, c := range w.Colors {
			if rem <= c.Remaining {
				v.Color, v.HasColor = c.Color, true
			}
		}
	} else {
		v.Over = true
		v.Main = "+" + stopwatchText(-rem)
		v.Color, v.HasColor = colInkError, true
	}
	switch {
	case !v.Running && el == 0:
		v.Sub = presenterIdle
	case !v.Running:
		v.Sub = "一時停止 " + elapsed
	case v.Over:
		v.Sub = "時間切れ " + elapsed
	default:
		v.Sub = elapsed
	}
	if v.Running {
		frac := el % time.Second // 表示は秒ごとに変わる（持ち時間と区切りは秒の単位）
		v.Next = now.Add(time.Second - frac)
		if v.Over {
			over := -rem
			v.BlinkOn = (over/presenterBlink)%2 == 0
			v.Next = now.Add(presenterBlink - over%presenterBlink)
		}
	}
	return v
}

// drawPresenter は、プレゼン用タイマーを見出しの下の範囲 area に描く。
func drawPresenter(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv, ink, subInk RGB) {
	pv := presenterView(w, env)
	pressed := ink != colText
	c := ink
	if pv.HasColor && !pressed {
		c = pv.Color
	}
	if !pv.Running && !pv.Over && !pressed {
		c = subInk // 止めているあいだは灰色
	}
	if pv.BlinkOn && !pressed {
		c = contrastInk(colInkError)
		subInk = c
	}
	if area.Dy() > gaugeH*3 {
		g := image.Rect(area.Min.X, area.Max.Y-gaugeH, area.Max.X, area.Max.Y)
		cv.frame(g, 1, subInk)
		in := g.Inset(2)
		if fw := in.Dx() * pv.Bar / 1000; fw > 0 {
			gc := colGauge
			if pv.HasColor {
				gc = pv.Color
			}
			if pressed || pv.BlinkOn {
				gc = subInk
			}
			cv.fill(image.Rect(in.Min.X, in.Min.Y, in.Min.X+fw, in.Max.Y), gc)
		}
		area.Max.Y -= gaugeH + widgetLineGap
	}
	drawTwoLines(cv, area, pv.Main, pv.Sub, c, subInk)
}
