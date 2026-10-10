package main

import (
	"fmt"
	"image"
	"math/big"
	"sync"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 電卓のウィジェット（widget: calculator） ----------
//
// 見出しの下の上 2/7 に表示（基数、知らせか計算した式、今の式か結果）、下に 6 列 × 5 行のボタンを並べる。
// ボタンは押した瞬間に働き、離すまで黄色にする（押した位置で働くウィジェット。セル全体は光らせない）。
// Brain の本体キーでも入力できる。キーが電卓に行くのは、電卓のセルが今の画面（重ねたレイヤー）にあるあいだだけで、
// そのあいだも、電卓が使わないキー（戻る、Tab、修飾キー、← → など）はふだんどおり働く。表は docs/config.md。
// 描き方は gui/src/calc.ts と同じにする（プレビューを -render-png と画素単位で比べる）。

const (
	defaultCalcID = "calculator"
	calcCols      = 6
	calcRows      = 5
	calcMinSpan   = 2
	calcPad       = 6
)

// calcButtons は、ボタンの並び（入力の名前）。
var calcButtons = [calcRows][calcCols]string{
	{calcBase, calcHist, "(", ")", calcBS, calcAC},
	{"A", "B", "7", "8", "9", "/"},
	{"C", "D", "4", "5", "6", "*"},
	{"E", "F", "1", "2", "3", "-"},
	{calcType, calcNeg, "0", ".", calcEq, "+"},
}

var calcLabels = map[string]string{calcHist: "履歴", calcBS: "BS", calcAC: "AC", "/": "÷", "*": "×", "-": "−",
	calcType: "入力", calcNeg: "±"}

var (
	colCalcDisp  = RGB{0x0c, 0x10, 0x16}
	colCalcKey   = RGB{0x26, 0x32, 0x44}
	colCalcOp    = RGB{0x22, 0x3c, 0x5e}
	colCalcEq    = RGB{0x2e, 0x9e, 0x5b}
	colCalcType  = RGB{0x7a, 0x56, 0x18}
	colCalcClear = RGB{0x5e, 0x2a, 0x32}
	colCalcFn    = RGB{0x34, 0x34, 0x48}
	colCalcBase  = colGraphLine
	colCalcOK    = colInkOK
)

// calcCell は、電卓のセル 1 つの、押しているボタン。
type calcCell struct {
	mu      sync.Mutex
	pressed int // 押しているボタンの番号（行 × 6 + 列）。-1 なら押していない
}

func (c *calcCell) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pressed
}

// compileCalc は widget: calculator の書き方を確かめる。
func compileCalc(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: calculator handles taps itself (tap the buttons); remove key and layer_*")
	}
	w := &WidgetDef{Kind: widgetCalc, ID: s.ID, calc: &calcCell{pressed: -1}, Prefix: s.Prefix}
	if w.ID == "" {
		w.ID = defaultCalcID
	}
	if !validTextID(w.ID) {
		return nil, fmt.Errorf("id must be 1-32 characters of A-Z a-z 0-9 _ . - (omit it to use %q), got %q", defaultCalcID, s.ID)
	}
	return w, nil
}

// calcGeom は、電卓のセルの中の配置。
type calcGeom struct {
	disp    image.Rectangle
	buttons [calcRows * calcCols]image.Rectangle
}

func newCalcGeom(area image.Rectangle) calcGeom {
	bh := area.Dy() / 7
	top := area.Max.Y - bh*calcRows
	g := calcGeom{disp: image.Rect(area.Min.X, area.Min.Y, area.Max.X, top-2)}
	for r := 0; r < calcRows; r++ {
		for c := 0; c < calcCols; c++ {
			x0, x1 := area.Min.X+area.Dx()*c/calcCols, area.Min.X+area.Dx()*(c+1)/calcCols
			g.buttons[r*calcCols+c] = image.Rect(x0, top+bh*r, x1, top+bh*(r+1)).Inset(2)
		}
	}
	return g
}

// calcButtonLabel は、ボタンに出す文字。基数のボタンは今の基数。
func calcButtonLabel(in string, base int) string {
	if in == calcBase {
		return baseName(base)
	}
	if l, ok := calcLabels[in]; ok {
		return l
	}
	return in
}

// calcButtonColor は、ボタンの塗り。
func calcButtonColor(in string) RGB {
	switch in {
	case "+", "-", "*", "/", "(", ")":
		return colCalcOp
	case calcEq:
		return colCalcEq
	case calcType:
		return colCalcType
	case calcBS, calcAC:
		return colCalcClear
	case calcBase, calcHist, calcNeg:
		return colCalcFn
	}
	return colCalcKey
}

// calcLines は、表示の 2 行（上の小さな行と色、下の大きな行の元）。
type calcDisp struct {
	small     string
	smallC    RGB
	big       string // 式（結果でないとき）
	result    *big.Rat
	resultDim bool // 式が空（0 を薄く出す）
}

func calcDisplay(s CalcState) calcDisp {
	d := calcDisp{smallC: colSub}
	switch {
	case s.Msg != "":
		d.small, d.smallC = s.Msg, colCalcOK
		if s.MsgErr {
			d.smallC = colUnsynced
		}
	case s.Done != "":
		d.small = s.Done
	case len(s.History) > 0:
		h := s.History[0]
		if r, ok := parseRatString(h.Value); ok {
			v, exact := formatRat(r, s.Base, 24, 10)
			if !exact {
				v += "…"
			}
			d.small = h.Expr + " = " + v
		}
	}
	if s.Result != "" {
		d.result, _ = parseRatString(s.Result)
	} else {
		d.big = calcExprText(s.Tokens, s.Base)
		if d.big == "" {
			d.big, d.resultDim = "0", true
		}
	}
	return d
}

// calcTail は、s が幅 w（等倍のドット）に入らなければ、先頭を「…」にして後ろを残す。
func calcTail(s string, w int) string {
	if font.textWidth(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && font.textWidth(textEllipsis+string(r)) > w {
		r = r[1:]
	}
	return textEllipsis + string(r)
}

// calcBigText は、表示の大きな行の文字と倍率。結果は、入る桁で丸め、丸めたときは「約」を付ける。
func calcBigText(d calcDisp, base, w, h int) (string, int) {
	for sc := 4; sc >= 1; sc-- {
		if fontH*sc > h && sc > 1 {
			continue
		}
		if d.result == nil {
			if font.textWidth(d.big)*sc <= w {
				return d.big, sc
			}
			continue
		}
		n := w / (font.textWidth("0") * sc)
		t, exact := formatRat(d.result, base, n, 20)
		if !exact {
			t2, _ := formatRat(d.result, base, n-2, 20)
			t = "約" + t2
		}
		if font.textWidth(t)*sc <= w {
			return t, sc
		}
	}
	if d.result == nil {
		return calcTail(d.big, w), 1
	}
	t, exact := formatRat(d.result, base, w/font.textWidth("0"), 20)
	if !exact {
		t = "約" + t
	}
	return calcTail(t, w), 1
}

// drawCalc は、電卓を見出しの下の範囲 area に描く。
func drawCalc(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv) {
	s, ok := env.Calc[w.ID]
	if !ok {
		s = *newCalcState()
	}
	g := newCalcGeom(area)
	pressed := w.calc.get()
	// 表示
	cv.fill(g.disp, colCalcDisp)
	in := g.disp.Inset(calcPad)
	d := calcDisplay(s)
	ss := 1
	if in.Dy() >= fontH*6 {
		ss = 2
	}
	cv.text(in.Min.X, in.Min.Y, baseName(s.Base), ss, colCalcBase, in)
	if d.small != "" {
		x0 := in.Min.X + (font.textWidth(baseName(s.Base))+4)*ss
		t := calcTail(d.small, (in.Max.X-x0)/ss)
		cv.text(in.Max.X-font.textWidth(t)*ss, in.Min.Y, t, ss, d.smallC, in)
	}
	bigArea := image.Rect(in.Min.X, in.Min.Y+fontH*ss+2, in.Max.X, in.Max.Y)
	t, sc := calcBigText(d, s.Base, bigArea.Dx(), bigArea.Dy())
	c := colText
	if d.resultDim {
		c = colTodoOff
	}
	cv.text(bigArea.Max.X-font.textWidth(t)*sc, bigArea.Max.Y-fontH*sc, t, sc, c, bigArea)
	// ボタン
	for i, r := range g.buttons {
		btn := calcButtons[i/calcCols][i%calcCols]
		label := calcButtonLabel(btn, s.Base)
		fill, ink := calcButtonColor(btn), colText
		if len(btn) == 1 && validDigit(btn[0], 16) && !validDigit(btn[0], s.Base) {
			ink = colTodoOff
		}
		if i == pressed {
			fill, ink = colPressed, colPressedText
		}
		cv.fill(r, fill)
		bs := min(fitScaleMax([]string{label}, r.Dx()-4, r.Dy()-4, 3), 3)
		drawCentered(cv, r, r.Min.Y+(r.Dy()-fontH*bs)/2, label, bs, ink)
	}
}

// calcKey は、描き直すかどうかを決める中身。
func calcKey(w *WidgetDef, env WidgetEnv) string {
	s := env.Calc[w.ID]
	return fmt.Sprintf("%d\x00%d\x00%d", s.Rev, s.Base, w.calc.get())
}

// calcTouch は、電卓のセルを押したとき。押したボタンを働かせ、離すまで黄色にする。入力の goroutine から呼ばれる。
func (w *WidgetDef) calcTouch(rt *WidgetRT, t widgetTouch, label string) {
	g := newCalcGeom(widgetArea(t.cell, label))
	idx := -1
	for i, r := range g.buttons {
		if t.pt.In(r) {
			idx = i
		}
	}
	w.calc.mu.Lock()
	w.calc.pressed = idx
	w.calc.mu.Unlock()
	if idx >= 0 {
		rt.calc.Input(w.ID, calcButtons[idx/calcCols][idx%calcCols], w.Prefix)
	}
	rt.redraw()
}

// calcRelease は、離したとき。
func (w *WidgetDef) calcRelease(rt *WidgetRT) {
	w.calc.mu.Lock()
	changed := w.calc.pressed >= 0
	w.calc.pressed = -1
	w.calc.mu.Unlock()
	if changed && rt != nil {
		rt.redraw()
	}
}

// calcKeyInput は、本体キーを電卓の入力にする。電卓が使わないキーなら false。
// Brain の文字キーの上の段（Q〜P）は、記号キーを押さなくても数字 1〜0 にする。16 進の A〜F は、A、B、C、D、F は
// そのまま、E は 3 と同じキーなので Shift を押しながら（どれも Shift を押しながらでもよい）。
func calcKeyInput(code evdev.EvCode, shift bool) (string, bool) {
	digits := map[evdev.EvCode]string{
		evdev.KEY_Q: "1", evdev.KEY_W: "2", evdev.KEY_E: "3", evdev.KEY_R: "4", evdev.KEY_T: "5",
		evdev.KEY_Y: "6", evdev.KEY_U: "7", evdev.KEY_I: "8", evdev.KEY_O: "9", evdev.KEY_P: "0",
		evdev.KEY_1: "1", evdev.KEY_2: "2", evdev.KEY_3: "3", evdev.KEY_4: "4", evdev.KEY_5: "5",
		evdev.KEY_6: "6", evdev.KEY_7: "7", evdev.KEY_8: "8", evdev.KEY_9: "9", evdev.KEY_0: "0",
	}
	hex := map[evdev.EvCode]string{evdev.KEY_A: "A", evdev.KEY_B: "B", evdev.KEY_C: "C", evdev.KEY_D: "D", evdev.KEY_E: "E", evdev.KEY_F: "F"}
	if h, ok := hex[code]; ok && (shift || code != evdev.KEY_E) {
		return h, true
	}
	if d, ok := digits[code]; ok {
		return d, true
	}
	switch code {
	case evdev.KEY_L:
		return "+", true
	case evdev.KEY_MINUS:
		return "-", true
	case evdev.KEY_X:
		return "*", true
	case evdev.KEY_K, evdev.KEY_SLASH:
		return "/", true
	case evdev.KEY_H:
		return "(", true
	case evdev.KEY_J:
		return ")", true
	case evdev.KEY_N, evdev.KEY_DOT:
		return ".", true
	case evdev.KEY_ENTER:
		if shift {
			return calcType, true
		}
		return calcEq, true
	case evdev.KEY_SPACE, evdev.KEY_EQUAL:
		return calcEq, true
	case evdev.KEY_Z:
		return calcType, true
	case evdev.KEY_BACKSPACE:
		if shift {
			return calcAC, true
		}
		return calcBS, true
	case evdev.KEY_DELETE:
		return calcAC, true
	case evdev.KEY_G:
		return calcBase, true
	case evdev.KEY_S:
		return calcNeg, true
	case evdev.KEY_UP:
		return calcHist, true
	case evdev.KEY_DOWN:
		return calcHistNew, true
	}
	return "", false
}

// calcWarns は、小さすぎる電卓のセルの警告。
func calcWarns(g *Grid, layer string) (warns []string) {
	for p, a := range g.Cells {
		if a.Widget != nil && a.Widget.Kind == widgetCalc && (a.SpanW < calcMinSpan || a.SpanH < calcMinSpan) {
			warns = append(warns, fmt.Sprintf("layer %q touch cell \"%d,%d\": widget: calculator is meant for span [%d, %d] or larger (now [%d, %d]); the buttons may be too small to tap",
				layer, p.Col, p.Row, calcMinSpan, calcMinSpan, a.SpanW, a.SpanH))
		}
	}
	return warns
}
