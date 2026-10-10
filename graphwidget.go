package main

import (
	"fmt"
	"image"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------- グラフのウィジェット（widget: graph） ----------
//
// 見出し（label）の下に、いちばん新しい値を大きく出し（単位を付ける）、その下（横長のセルでは右）に、
// 新しい points 個の値を折れ線（chart: line）か棒（chart: bar）で描く。新しい値が右端。
// 値の時刻は見ず、届いた順に等しい間隔で並べる（送る側の間隔がまちまちでも、形は崩れない）。
// 縦の範囲は min、max（書かなければ、出している値の最小と最大。棒では 0 も含める）。範囲の外の値は端に描く。
// いちばん新しい値が stale（既定 5 分）より古ければ、値と線を薄くし、「古い 15:04」と出す。
// 描き方は gui/src/graphwidget.ts と同じにする（プレビューを -render-png と画素単位で比べる）。

const (
	graphChartLine     = "line"
	graphChartBar      = "bar"
	defaultGraphPoints = 60
	defaultGraphStale  = 5 * time.Minute
	graphLineW         = 2 // 折れ線の太さ
	graphValueMax      = 6 // 値の文字の最大の倍率
	graphGap           = 6 // 値と図のあいだ
	graphMinPlotH      = 16
	graphNoneText      = "未受信"
)

var (
	colGraphLine = RGB{0x4f, 0x9d, 0xff}
	colGraphOld  = RGB{0x34, 0x4c, 0x68} // 古いときの線
	colGraphGrid = colTodoRule
)

// GraphDef は、グラフのセルの設定。
type GraphDef struct {
	Unit     string
	Min, Max *float64
	Chart    string
	Points   int
	Decimals int // -1 なら値の大きさで決める
}

// compileGraph は widget: graph の書き方を確かめて組み立てる。
func compileGraph(s ActionSpec) (*WidgetDef, error) {
	if !validTextID(s.ID) {
		return nil, fmt.Errorf("widget: graph needs id (1-32 characters of A-Z a-z 0-9 _ . -; brain-deck graph <id> <value> adds a value), got %q", s.ID)
	}
	g := &GraphDef{Unit: s.Unit, Min: s.Min, Max: s.Max, Chart: s.Chart, Points: defaultGraphPoints, Decimals: -1}
	if utf8.RuneCountInString(g.Unit) > graphUnitRunes || strings.ContainsFunc(g.Unit, unicode.IsControl) {
		return nil, fmt.Errorf("unit must be at most %d characters without control characters, got %q", graphUnitRunes, s.Unit)
	}
	switch g.Chart {
	case "":
		g.Chart = graphChartLine
	case graphChartLine, graphChartBar:
	default:
		return nil, fmt.Errorf("chart must be line or bar, got %q", s.Chart)
	}
	for _, x := range []struct {
		name string
		v    *float64
	}{{"min", s.Min}, {"max", s.Max}} {
		if x.v != nil && !validGraphValue(*x.v) {
			return nil, fmt.Errorf("%s must be a number from -1e12 to 1e12", x.name)
		}
	}
	if s.Min != nil && s.Max != nil && *s.Min >= *s.Max {
		return nil, fmt.Errorf("min (%v) must be less than max (%v)", *s.Min, *s.Max)
	}
	if s.Points != nil {
		if *s.Points < 2 || *s.Points > graphMaxPoints {
			return nil, fmt.Errorf("points must be 2..%d (the number of values to draw), got %d", graphMaxPoints, *s.Points)
		}
		g.Points = *s.Points
	}
	if s.Decimals != nil {
		if *s.Decimals < 0 || *s.Decimals > graphMaxDecimal {
			return nil, fmt.Errorf("decimals must be 0..%d (omit it to choose by the size of the value), got %d", graphMaxDecimal, *s.Decimals)
		}
		g.Decimals = *s.Decimals
	}
	w := &WidgetDef{Kind: widgetGraph, ID: s.ID, Stale: defaultGraphStale, Graph: g}
	if s.Stale != "" {
		d, err := parseStale(s.Stale)
		if err != nil {
			return nil, err
		}
		w.Stale = d
	}
	return w, nil
}

// graphDecimals は、値を書くときの小数点以下の桁数と、末尾の 0 を省くか。
func graphDecimals(v float64, fixed int) (int, bool) {
	if fixed >= 0 {
		return fixed, false
	}
	switch a := math.Abs(v); {
	case a >= 100:
		return 0, true
	case a >= 10:
		return 1, true
	}
	return 2, true
}

// formatGraph は値を小数点以下 d 桁で書く（四捨五入）。trim なら末尾の 0 と小数点を省く。
// 浮動小数点数の文字列にする関数は、言語によって丸め方が違う（ちょうど半分のとき）。GUI と同じ結果にするため、
// 10^d 倍して 0.5 を足し、切り捨てた整数から書く（IEEE 754 の掛け算と足し算は、Go と JavaScript で同じ結果になる）。
func formatGraph(v float64, d int, trim bool) string {
	p := math.Pow10(d)
	n := int64(math.Floor(math.Abs(v)*p + 0.5))
	pi := int64(p)
	s := strconv.FormatInt(n/pi, 10)
	if d > 0 {
		frac := strconv.FormatInt(n%pi, 10)
		s += "." + strings.Repeat("0", d-len(frac)) + frac
		if trim {
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
	}
	if v < 0 && n != 0 {
		s = "-" + s
	}
	return s
}

// graphRange は、縦の範囲 [lo, hi]。
func graphRange(g *GraphDef, pts []GraphPoint) (lo, hi float64) {
	dmin, dmax := pts[0].V, pts[0].V
	for _, p := range pts[1:] {
		dmin, dmax = min(dmin, p.V), max(dmax, p.V)
	}
	autoLo, autoHi := g.Min == nil, g.Max == nil
	lo, hi = dmin, dmax
	if g.Chart == graphChartBar && autoLo {
		lo = min(lo, 0)
	}
	if g.Chart == graphChartBar && autoHi {
		hi = max(hi, 0)
	}
	if !autoLo {
		lo = *g.Min
	}
	if !autoHi {
		hi = *g.Max
	}
	if hi <= lo {
		switch {
		case autoLo && autoHi:
			lo, hi = lo-1, hi+1
		case autoHi:
			hi = lo + 1
		default:
			lo = hi - 1
		}
	}
	return lo, hi
}

// graphLay は、グラフのセルの中の配置。
type graphLay struct {
	None     bool // 値がまだない
	Value    string
	ValueBox image.Rectangle
	ValueS   int
	Sub      string // 古いときの「古い 15:04」
	SubS     int
	Stale    bool
	Plot     image.Rectangle // 線を描く範囲（目盛りの文字は含まない）。空なら図を描かない
	HiLabel  string
	LoLabel  string
	LabelX   int
	XS, YS   []int // 折れ線：点の左上。棒：棒の左右（XS は左、YS は上）
	BarR     []int // 棒の右端
	BarBase  int   // 棒の根元の y
}

// graphPoints は、描く値（新しい Points 個）。
func graphPoints(w *WidgetDef, env WidgetEnv) []GraphPoint {
	s := env.Graph[w.ID]
	pts := s.Points
	if n := w.Graph.Points; len(pts) > n {
		pts = pts[len(pts)-n:]
	}
	return pts
}

func graphStale(w *WidgetDef, last GraphPoint, now time.Time) bool {
	return w.Stale > 0 && now.Sub(last.T) >= w.Stale
}

// graphStaleText は、古いときに出す文（「古い 15:04」、日付が違えば「古い 10/9 15:04」）。
func graphStaleText(t, now time.Time) string {
	t, now = t.In(time.Local), now.In(time.Local)
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return "古い " + t.Format("15:04")
	}
	return "古い " + t.Format("1/2 15:04")
}

// graphLayout は、見出しの下の範囲 area の中の配置を決める。
func graphLayout(area image.Rectangle, w *WidgetDef, env WidgetEnv) graphLay {
	g := w.Graph
	pts := graphPoints(w, env)
	if len(pts) == 0 {
		return graphLay{None: true}
	}
	last := pts[len(pts)-1]
	d, trim := graphDecimals(last.V, g.Decimals)
	l := graphLay{Value: formatGraph(last.V, d, trim) + g.Unit, Stale: graphStale(w, last, env.Now)}
	if l.Stale {
		l.Sub = graphStaleText(last.T, env.Now)
	}
	wide := area.Dx() >= area.Dy()*2
	var plot image.Rectangle
	if wide {
		vb := area
		vb.Max.X = area.Min.X + area.Dx()*2/5
		l.ValueBox = vb
		plot = area
		plot.Min.X = vb.Max.X + graphGap
	} else {
		vb := area
		vb.Max.Y = area.Min.Y + area.Dy()/2
		l.ValueBox = vb
		plot = area
	}
	subH := 0
	if l.Sub != "" {
		l.SubS = min(fitScaleMax([]string{l.Sub}, l.ValueBox.Dx(), fontH*2, 2), 2)
		subH = fontH*l.SubS + widgetLineGap
	}
	l.ValueS = fitScaleMax([]string{l.Value}, l.ValueBox.Dx(), l.ValueBox.Dy()-subH, graphValueMax)
	if !wide {
		// 値の行の高さだけを取り、残りを図にする
		l.ValueBox.Max.Y = l.ValueBox.Min.Y + fontH*l.ValueS + subH
		plot.Min.Y = l.ValueBox.Max.Y + graphGap
	}
	if plot.Dy() < graphMinPlotH || plot.Dx() < graphMinPlotH {
		// 図を描く場所がない：値だけを、範囲の全体に出す
		l.ValueBox = area
		l.ValueS = fitScaleMax([]string{l.Value}, area.Dx(), area.Dy()-subH, graphValueMax)
		return l
	}
	lo, hi := graphRange(g, pts)
	if plot.Dy() >= fontH*3 {
		hd, ht := graphDecimals(max(math.Abs(hi), math.Abs(lo)), g.Decimals)
		l.HiLabel, l.LoLabel = formatGraph(hi, hd, ht), formatGraph(lo, hd, ht)
		gw := max(font.textWidth(l.HiLabel), font.textWidth(l.LoLabel)) + 4
		if plot.Dx()-gw >= graphMinPlotH*2 {
			l.LabelX = plot.Min.X
			plot.Min.X += gw
		} else {
			l.HiLabel, l.LoLabel = "", ""
		}
	}
	l.Plot = plot
	P, n := g.Points, len(pts)
	pw, ph := plot.Dx(), plot.Dy()
	clamp := func(v float64) float64 { return min(max(v, lo), hi) }
	if g.Chart == graphChartBar {
		yb := func(v float64) int { return plot.Min.Y + int(math.Floor((hi-clamp(v))/(hi-lo)*float64(ph)+0.5)) }
		l.BarBase = yb(0)
		for i, p := range pts {
			k := P - n + i
			x0, x1 := plot.Min.X+pw*k/P, plot.Min.X+pw*(k+1)/P
			if x1-x0 >= 3 {
				x1--
			}
			l.XS, l.BarR, l.YS = append(l.XS, x0), append(l.BarR, x1), append(l.YS, yb(p.V))
		}
		return l
	}
	yl := func(v float64) int {
		return plot.Min.Y + int(math.Floor((hi-clamp(v))/(hi-lo)*float64(ph-graphLineW)+0.5))
	}
	for i, p := range pts {
		k := P - n + i
		l.XS = append(l.XS, plot.Min.X+(pw-graphLineW)*k/(P-1))
		l.YS = append(l.YS, yl(p.V))
	}
	return l
}

// drawGraph は、グラフのセルの中身を描く。area は見出しの下の範囲。
func drawGraph(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv, ink, subInk RGB) {
	l := graphLayout(area, w, env)
	if l.None {
		s := min(fitScale([]string{graphNoneText}, area.Dx(), area.Dy()), 2)
		drawCentered(cv, area, area.Min.Y+(area.Dy()-fontH*s)/2, graphNoneText, s, subInk)
		return
	}
	valC, lineC := ink, colGraphLine
	if l.Stale {
		valC, lineC = colTodoDone, colGraphOld
		if ink != colText { // 押したとき（fill）
			valC = ink
		}
	}
	vb := l.ValueBox
	subH := 0
	if l.Sub != "" {
		subH = fontH*l.SubS + widgetLineGap
	}
	y := vb.Min.Y + (vb.Dy()-fontH*l.ValueS-subH)/2
	drawCentered(cv, vb, y, l.Value, l.ValueS, valC)
	if l.Sub != "" {
		drawCentered(cv, vb, y+fontH*l.ValueS+widgetLineGap, l.Sub, l.SubS, colUnsynced)
	}
	if l.Plot.Empty() {
		return
	}
	p := l.Plot
	cv.fill(image.Rect(p.Min.X, p.Min.Y, p.Max.X, p.Min.Y+1), colGraphGrid)
	cv.fill(image.Rect(p.Min.X, p.Max.Y-1, p.Max.X, p.Max.Y), colGraphGrid)
	if l.HiLabel != "" {
		cv.text(l.LabelX, p.Min.Y, l.HiLabel, 1, subInk, area)
		cv.text(l.LabelX, p.Max.Y-fontH, l.LoLabel, 1, subInk, area)
	}
	if w.Graph.Chart == graphChartBar {
		for i := range l.XS {
			y0, y1 := min(l.YS[i], l.BarBase), max(l.YS[i], l.BarBase)
			if y1 == y0 {
				y1 = y0 + 1
			}
			cv.fill(image.Rect(l.XS[i], y0, l.BarR[i], y1).Intersect(p), lineC)
		}
		return
	}
	dot := func(x, y int) { cv.fill(image.Rect(x, y, x+graphLineW, y+graphLineW), lineC) }
	dot(l.XS[0], l.YS[0])
	for i := 1; i < len(l.XS); i++ {
		graphSegment(l.XS[i-1], l.YS[i-1], l.XS[i], l.YS[i], dot)
	}
}

// graphSegment は、(x0, y0) から (x1, y1) までの点を、ブレゼンハムの方法で順に f に渡す（始点は渡さない）。
func graphSegment(x0, y0, x1, y1 int, f func(x, y int)) {
	dx, dy := x1-x0, y1-y0
	sx, sy := 1, 1
	if dx < 0 {
		dx, sx = -dx, -1
	}
	if dy < 0 {
		dy, sy = -dy, -1
	}
	err := dx - dy
	for x0 != x1 || y0 != y1 {
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
		f(x0, y0)
	}
}

// graphKey は、描き直すかどうかを決める中身。
func graphKey(w *WidgetDef, env WidgetEnv) string {
	s := env.Graph[w.ID]
	p, ok := s.Last()
	stale := ok && graphStale(w, p, env.Now)
	day := 0
	if stale { // 「古い 10/9 15:04」は、日付が変わると変わる
		day = env.Now.In(time.Local).YearDay()
	}
	return fmt.Sprintf("%d\x00%d\x00%v\x00%v\x00%d", s.Rev, len(s.Points), stale, p.T.Unix(), day)
}

// graphNext は、いちばん新しい値が古くなる時刻（薄く描き直す）。
func graphNext(w *WidgetDef, env WidgetEnv) time.Time {
	if p, ok := env.Graph[w.ID].Last(); ok && w.Stale > 0 {
		if t := p.T.Add(w.Stale); env.Now.Before(t) {
			return t
		}
	}
	return time.Time{}
}
