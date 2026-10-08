package main

import (
	"image"
	"strings"
)

// ---------- 端末モードの画面 ----------
//
// 上から、状態の帯（端末モードであること、PC との接続、大きさ、修飾キー）、文字の格子、
// タッチのキー（12 列 × 2 段）を描く。帯とタッチのキーは、変わったときだけ描き直す。
// 文字の格子は、端末（term.go）が変わったと記録した行だけを描き直す。画面全体が上に流れたときは、
// 裏画面の点を行単位でずらしてから、新しく出た行だけを描く（回転していないとき）。

const (
	termStatusH  = 14 // 状態の帯の高さ
	termPadRowH  = 44 // タッチのキーの 1 段の高さ
	termPadRows  = 2
	termPadCols  = 12
	termBandSkip = 48 // 右端の、印刷されたソフトキーの帯に重なる幅（タッチのキーを置かない）
	termCellH    = fontH
)

// termGeom は端末モードの画面の配置。
type termGeom struct {
	W, H       int
	CW         int // 半角 1 桁の幅（ドット）
	Cols, Rows int
	Status     image.Rectangle
	Text       image.Rectangle // 文字の格子（Cols×CW、Rows×12 ちょうど）
	Pad        image.Rectangle // タッチのキー
}

// termFontWidths は、terminal.font の名前 → 半角 1 桁の幅。
// k8x12 の半角は 4×12 ドット。wide は横に 2 倍に伸ばす（800 ドットで 100 桁）。
var termFontWidths = map[string]int{"wide": 8, "narrow": 4}

const defaultTermFont = "wide"

func termLayout(W, H int, fontName string) termGeom {
	cw := termFontWidths[fontName]
	if cw == 0 {
		cw = termFontWidths[defaultTermFont]
	}
	g := termGeom{W: W, H: H, CW: cw}
	g.Status = image.Rect(0, 0, W, termStatusH)
	padW := W
	if W > H { // 横向きの画面では、右端の帯の下にキーを置かない
		padW = W - termBandSkip
	}
	g.Pad = image.Rect(0, H-termPadRowH*termPadRows, padW, H)
	g.Cols = W / cw
	g.Rows = (g.Pad.Min.Y - g.Status.Max.Y) / termCellH
	top := g.Status.Max.Y + (g.Pad.Min.Y-g.Status.Max.Y-g.Rows*termCellH)/2
	g.Text = image.Rect(0, top, g.Cols*cw, top+g.Rows*termCellH)
	return g
}

// padKeyRect は、タッチのキー (col, row) の矩形。
func (g termGeom) padKeyRect(col, row int) image.Rectangle {
	x0, x1 := cellSpan(col, termPadCols, g.Pad.Dx())
	return image.Rect(g.Pad.Min.X+x0, g.Pad.Min.Y+row*termPadRowH, g.Pad.Min.X+x1, g.Pad.Min.Y+(row+1)*termPadRowH)
}

// padKeyAt は、画面の点 p にあるタッチのキー。なければ ok=false。
func (g termGeom) padKeyAt(p image.Point) (col, row int, ok bool) {
	if !p.In(g.Pad) {
		return 0, 0, false
	}
	row = (p.Y - g.Pad.Min.Y) / termPadRowH
	for c := 0; c < termPadCols; c++ {
		if p.In(g.padKeyRect(c, row)) {
			return c, row, true
		}
	}
	return 0, 0, false
}

// termStatus は、状態の帯とタッチのキーに出すもの。
type termStatus struct {
	State      string // 「PC のログイン画面」など
	Level      int    // 0 ふつう、1 待ち（黄）、2 うまくいっていない（赤）、3 つながっている（緑）
	Transport  string // 「シリアル」「ssh」など
	Ctrl, Alt  bool   // 次の 1 キーに効く Ctrl、Alt
	Page       int    // タッチのキーのページ
	Pressed    int    // 押しているタッチのキー（row*12+col）。-1 なら押していない
	Exitable   bool   // HOME で抜けられる（ソフトキーの帯がある）
	ExitingMsg string // 抜けている途中の知らせ
}

// termRenderer は端末モードの画面を描く。描画の goroutine だけが触る。
type termRenderer struct {
	g          termGeom
	frame      termFrame
	lastStatus string
	lastPad    string
}

var termLevelColors = [...]RGB{{0x3a, 0x48, 0x5c}, {0xd0, 0xa0, 0x20}, {0xd0, 0x40, 0x40}, {0x2e, 0x9e, 0x5b}}

// render は端末モードの画面を cv に描き、書き換えた論理矩形を返す。full なら全体を描き直す。
// take は、端末の変わった行を f に写し、状態を返す（端末のロックを持つのは、この中だけ）。
func (r *termRenderer) render(cv *Canvas, full bool, take func(f *termFrame) termStatus) []image.Rectangle {
	if full {
		cv.fill(image.Rect(0, 0, cv.W, cv.H), colBG)
		r.lastStatus, r.lastPad = "", ""
	}
	st := take(&r.frame)
	f := &r.frame
	if full {
		f.All = true
		for i := range f.Dirty {
			f.Dirty[i] = true
		}
	}
	var rs []image.Rectangle
	if s := r.statusKey(st, f); s != r.lastStatus {
		r.lastStatus = s
		r.drawStatus(cv, st, f)
		rs = append(rs, r.g.Status)
	}
	if p := r.padKey(st); p != r.lastPad {
		r.lastPad = p
		r.drawPad(cv, st)
		rs = append(rs, image.Rect(0, r.g.Pad.Min.Y, r.g.W, r.g.H)) // 右の、ページの名前も
	}
	rs = append(rs, r.drawText(cv, f)...)
	if full {
		// 帯、格子、キーのあいだの隙間にも、前の画面が残らないよう、全体を写す
		return []image.Rectangle{image.Rect(0, 0, cv.W, cv.H)}
	}
	return rs
}

func (r *termRenderer) statusKey(st termStatus, f *termFrame) string {
	return strings.Join([]string{st.State, st.Transport, st.ExitingMsg, itoaB(st.Level), itoaB(f.View), itoaB(f.History),
		boolS(st.Ctrl), boolS(st.Alt), boolS(st.Exitable)}, "\x00")
}

func (r *termRenderer) padKey(st termStatus) string {
	return itoaB(st.Page) + "/" + itoaB(st.Pressed) + boolS(st.Ctrl) + boolS(st.Alt)
}

func itoaB(n int) string {
	if n < 0 {
		return "-" + itoaB(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoaB(n/10) + string(rune('0'+n%10))
}

func boolS(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (r *termRenderer) drawStatus(cv *Canvas, st termStatus, f *termFrame) {
	b := r.g.Status
	cv.fill(b, RGB{0x10, 0x18, 0x24})
	y := b.Min.Y + 1
	// 左：端末モードの札と、接続の状態
	tag := "端末モード"
	tw := font.textWidth(tag) + 8
	cv.fill(image.Rect(b.Min.X, b.Min.Y, b.Min.X+tw, b.Max.Y), RGB{0x2e, 0x6e, 0xc8})
	cv.text(b.Min.X+4, y, tag, 1, colText, b)
	x := b.Min.X + tw + 4
	state := st.State
	if st.ExitingMsg != "" {
		state = st.ExitingMsg
	}
	if st.Transport != "" {
		state = st.Transport + "：" + state
	}
	sw := font.textWidth(state) + 8
	cv.fill(image.Rect(x, b.Min.Y, x+sw, b.Max.Y), termLevelColors[st.Level])
	tc := colText
	if st.Level == 1 {
		tc = RGB{0, 0, 0}
	}
	cv.text(x+4, y, state, 1, tc, b)
	x += sw + 8
	// 右：大きさ、修飾キー、履歴、抜け方
	var parts []string
	if f.View > 0 {
		parts = append(parts, "履歴 -"+itoaB(f.View)+"/"+itoaB(f.History))
	}
	if st.Ctrl {
		parts = append(parts, "[Ctrl]")
	}
	if st.Alt {
		parts = append(parts, "[Alt]")
	}
	parts = append(parts, itoaB(f.Cols)+"×"+itoaB(f.Rows))
	if st.Exitable {
		parts = append(parts, "HOME で抜ける")
	} else {
		parts = append(parts, "文字切替+戻る で抜ける")
	}
	right := strings.Join(parts, "  ")
	rw := font.textWidth(right)
	rx := max(b.Max.X-rw-4-termBandSkip, x)
	cv.text(rx, y, right, 1, colSub, b)
}

var (
	colPadKey    = RGB{0x1c, 0x28, 0x38}
	colPadBorder = RGB{0x50, 0x64, 0x80}
	colPadMod    = RGB{0x2a, 0x22, 0x3c}
)

func (r *termRenderer) drawPad(cv *Canvas, st termStatus) {
	cv.fill(r.g.Pad, colBG)
	page := termPadPages[st.Page%len(termPadPages)]
	for row := 0; row < termPadRows; row++ {
		for col := 0; col < termPadCols; col++ {
			key := page.Keys[row][col]
			box := r.g.padKeyRect(col, row).Inset(2)
			fill, tc := colPadKey, colText
			if key.Mod != "" {
				fill = colPadMod
				if (key.Mod == "ctrl" && st.Ctrl) || (key.Mod == "alt" && st.Alt) {
					fill, tc = RGB{0x2e, 0x9e, 0x5b}, colText
				}
			}
			if st.Pressed == row*termPadCols+col {
				fill, tc = colPressed, colPressedText
			}
			cv.fill(box, fill)
			cv.frame(box, 1, colPadBorder)
			inner := box.Inset(3)
			label := wideLabel(key.Label)
			s := fitScaleMax([]string{label}, inner.Dx(), inner.Dy(), 3)
			tx := inner.Min.X + (inner.Dx()-font.textWidth(label)*s)/2
			ty := inner.Min.Y + (inner.Dy()-fontH*s)/2
			cv.text(max(tx, inner.Min.X), ty, label, s, tc, inner)
		}
	}
	// ページの名前を右端の帯の手前に小さく出す
	if r.g.Pad.Max.X < r.g.W {
		side := image.Rect(r.g.Pad.Max.X, r.g.Pad.Min.Y, r.g.W, r.g.H)
		cv.fill(side, colBG)
		name := page.Name
		cv.text(side.Min.X+2, side.Min.Y+4, name, 1, colSub, side)
		cv.text(side.Min.X+2, side.Min.Y+4+fontH+2, itoaB(st.Page%len(termPadPages)+1)+"/"+itoaB(len(termPadPages)), 1, colSub, side)
		cv.text(side.Min.X+2, side.Min.Y+4+2*(fontH+2), "←→", 1, colSub, side)
	}
}

// drawText は、変わった行を描き、写す矩形を返す。
func (r *termRenderer) drawText(cv *Canvas, f *termFrame) []image.Rectangle {
	g := r.g
	all := f.All
	if f.Scrolled > 0 && !all {
		if cv.rot == 0 {
			// 裏画面の点を上にずらす（流れた行は描き直さない）
			dy := f.Scrolled * termCellH
			bpp := cv.pf.Bpp
			x0, x1 := g.Text.Min.X*bpp, g.Text.Max.X*bpp
			for y := g.Text.Min.Y; y < g.Text.Max.Y-dy; y++ {
				copy(cv.pix[y*cv.stride+x0:y*cv.stride+x1], cv.pix[(y+dy)*cv.stride+x0:(y+dy)*cv.stride+x1])
			}
		} else {
			all = true
			for i := range f.Dirty {
				f.Dirty[i] = true
			}
		}
	}
	var rs []image.Rectangle
	start := -1
	for y := 0; y <= f.Rows; y++ {
		d := y < f.Rows && f.Dirty[y]
		if d {
			r.drawLine(cv, f, y)
			if start < 0 {
				start = y
			}
			continue
		}
		if start >= 0 {
			rs = append(rs, image.Rect(g.Text.Min.X, g.Text.Min.Y+start*termCellH, g.Text.Max.X, g.Text.Min.Y+y*termCellH))
			start = -1
		}
	}
	if f.Scrolled > 0 || all {
		return []image.Rectangle{g.Text}
	}
	return rs
}

// termPalette16 は 16 色（xterm の既定に近い色）。
var termPalette16 = [16]RGB{
	{0x00, 0x00, 0x00}, {0xcd, 0x31, 0x31}, {0x0d, 0xbc, 0x79}, {0xe5, 0xe5, 0x10},
	{0x44, 0x72, 0xe8}, {0xbc, 0x3f, 0xbc}, {0x11, 0xa8, 0xcd}, {0xd0, 0xd0, 0xd0},
	{0x76, 0x76, 0x76}, {0xf1, 0x4c, 0x4c}, {0x23, 0xd1, 0x8b}, {0xf5, 0xf5, 0x43},
	{0x6b, 0x95, 0xff}, {0xd6, 0x70, 0xd6}, {0x29, 0xb8, 0xdb}, {0xff, 0xff, 0xff},
}

var (
	termDefFG = RGB{0xd0, 0xd0, 0xd0}
	termDefBG = RGB{0, 0, 0}
)

// termRGB は色の値を RGB にする。bold なら 0〜7 の色を明るい色にする。
func termRGB(c tcolor, fg, bold bool) RGB {
	if c == tcDefault {
		if fg {
			if bold {
				return RGB{0xff, 0xff, 0xff}
			}
			return termDefFG
		}
		return termDefBG
	}
	if c&tcRGB != 0 {
		return RGB{uint8(c >> 16), uint8(c >> 8), uint8(c)}
	}
	i, _ := c.palette()
	switch {
	case i < 8 && bold && fg:
		return termPalette16[i+8]
	case i < 16:
		return termPalette16[i]
	case i < 232:
		i -= 16
		lv := func(v int) uint8 {
			if v == 0 {
				return 0
			}
			return uint8(55 + v*40)
		}
		return RGB{lv(i / 36), lv(i / 6 % 6), lv(i % 6)}
	default:
		v := uint8(8 + (i-232)*10)
		return RGB{v, v, v}
	}
}

func (r *termRenderer) drawLine(cv *Canvas, f *termFrame, y int) {
	g := r.g
	line := f.Lines[y]
	py := g.Text.Min.Y + y*termCellH
	for x := 0; x < len(line) && x < f.Cols; x++ {
		c := line[x]
		if c.w == 0 {
			continue // 全角の右半分（左で描いた）
		}
		w := int(c.w)
		fg := termRGB(c.fg, true, c.at&attrBold != 0)
		bg := termRGB(c.bg, false, false)
		if c.at&attrDim != 0 {
			fg = RGB{fg.R / 2, fg.G / 2, fg.B / 2}
		}
		rev := c.at&attrReverse != 0
		if f.Reverse {
			rev = !rev
		}
		if f.CurOn && y == f.CurY && x == f.CurX {
			rev = !rev
		}
		if rev {
			fg, bg = bg, fg
		}
		if c.at&attrHidden != 0 {
			fg = bg
		}
		px := g.Text.Min.X + x*g.CW
		drawTermCell(cv, px, py, g.CW*w, c.r, fg, bg, c.at)
	}
}

// drawTermCell は、幅 cw の 1 文字を描く。字形の幅（4 か 8）を cw に合わせて伸ばす（縮める）。
func drawTermCell(cv *Canvas, x, y, cw int, r rune, fg, bg RGB, at termAttr) {
	var bits [fontH]uint16 // 左から cw ビット（MSB 側を使う）
	if r != 0 && r != ' ' {
		if !boxGlyph(r, cw, &bits) {
			gw, rows := termGlyph(r, cw)
			for gy, b := range rows {
				var v uint16
				for px := 0; px < cw; px++ {
					gx := px * gw / cw
					if b&(0x80>>gx) != 0 {
						v |= 0x8000 >> px
					}
				}
				bits[gy] = v
			}
		}
	}
	if at&attrUnderline != 0 {
		bits[fontH-1] = 0xffff
	}
	if at&attrStrike != 0 {
		bits[fontH/2] = 0xffff
	}
	if cv.rot == 0 && cv.pf.Bpp == 2 && x >= 0 && y >= 0 && x+cw <= cv.W && y+fontH <= cv.H {
		f16, b16 := uint16(cv.pf.pack(fg)), uint16(cv.pf.pack(bg))
		for gy := 0; gy < fontH; gy++ {
			o := (y+gy)*cv.stride + x*2
			row := cv.pix[o : o+cw*2]
			v := bits[gy]
			for px := 0; px < cw; px++ {
				c := b16
				if v&(0x8000>>px) != 0 {
					c = f16
				}
				row[px*2] = byte(c)
				row[px*2+1] = byte(c >> 8)
			}
		}
		return
	}
	cv.fill(image.Rect(x, y, x+cw, y+fontH), bg)
	for gy := 0; gy < fontH; gy++ {
		for px := 0; px < cw; px++ {
			if bits[gy]&(0x8000>>px) != 0 {
				cv.fill(image.Rect(x+px, y+gy, x+px+1, y+gy+1), fg)
			}
		}
	}
}

// termGlyph は、幅 cw の桁に描く字形。8 ドット以上の桁の ASCII は、k8x12 の全角英数（8×12）の字形を使う。
// 半角（4×12）を横に伸ばすと、& と 8、# と H などの見分けがつきにくいため。
func termGlyph(r rune, cw int) (int, []byte) {
	if cw >= 8 && r > 0x20 && r < 0x7f {
		if w, rows, ok := font.glyph(wideASCII(r)); ok && w == 8 {
			return w, rows
		}
	}
	return font.glyphOrBox(r)
}

// wideASCII は ASCII の文字を、同じ形の全角の文字にする（\ は ＼、~ は ～）。
func wideASCII(r rune) rune {
	if r > 0x20 && r < 0x7f {
		return r + 0xfee0
	}
	return r
}

// wideLabel は、タッチのキーの名前の ASCII を全角にする（大きく描いたときに見分けやすい）。
func wideLabel(s string) string {
	if len([]rune(s)) != 1 {
		return s
	}
	r := []rune(s)[0]
	if w, _, ok := font.glyph(wideASCII(r)); ok && w == 8 {
		return string(wideASCII(r))
	}
	return s
}

// boxSeg は罫線の 4 方向の太さ（0 なし、1 細、2 太、3 二重）。上、下、左、右。
type boxSeg [4]uint8

var boxLines = map[rune]boxSeg{
	'─': {0, 0, 1, 1}, '━': {0, 0, 2, 2}, '│': {1, 1, 0, 0}, '┃': {2, 2, 0, 0},
	'┌': {0, 1, 0, 1}, '┏': {0, 2, 0, 2}, '┐': {0, 1, 1, 0}, '┓': {0, 2, 2, 0},
	'└': {1, 0, 0, 1}, '┗': {2, 0, 0, 2}, '┘': {1, 0, 1, 0}, '┛': {2, 0, 2, 0},
	'├': {1, 1, 0, 1}, '┣': {2, 2, 0, 2}, '┤': {1, 1, 1, 0}, '┫': {2, 2, 2, 0},
	'┬': {0, 1, 1, 1}, '┳': {0, 2, 2, 2}, '┴': {1, 0, 1, 1}, '┻': {2, 0, 2, 2},
	'┼': {1, 1, 1, 1}, '╋': {2, 2, 2, 2},
	'╭': {0, 1, 0, 1}, '╮': {0, 1, 1, 0}, '╯': {1, 0, 1, 0}, '╰': {1, 0, 0, 1},
	'═': {0, 0, 3, 3}, '║': {3, 3, 0, 0}, '╔': {0, 3, 0, 3}, '╗': {0, 3, 3, 0},
	'╚': {3, 0, 0, 3}, '╝': {3, 0, 3, 0}, '╠': {3, 3, 0, 3}, '╣': {3, 3, 3, 0},
	'╦': {0, 3, 3, 3}, '╩': {3, 0, 3, 3}, '╬': {3, 3, 3, 3},
	'╴': {0, 0, 1, 0}, '╵': {1, 0, 0, 0}, '╶': {0, 0, 0, 1}, '╷': {0, 1, 0, 0},
}

// boxGlyph は罫線とブロックの文字を、桁の幅いっぱいに描く（隣の桁とつながるように）。描いたら true。
func boxGlyph(r rune, cw int, bits *[fontH]uint16) bool {
	full := uint16(0xffff) << (16 - cw)
	span := func(x0, x1 int) uint16 { // [x0, x1) のビット
		var v uint16
		for x := max(x0, 0); x < min(x1, cw); x++ {
			v |= 0x8000 >> x
		}
		return v
	}
	switch r {
	case '█':
		for i := range bits {
			bits[i] = full
		}
		return true
	case '▀':
		for i := 0; i < fontH/2; i++ {
			bits[i] = full
		}
		return true
	case '▄':
		for i := fontH / 2; i < fontH; i++ {
			bits[i] = full
		}
		return true
	case '▌':
		for i := range bits {
			bits[i] = span(0, cw/2)
		}
		return true
	case '▐':
		for i := range bits {
			bits[i] = span(cw/2, cw)
		}
		return true
	case '░', '▒', '▓':
		for i := range bits {
			var v uint16
			for x := 0; x < cw; x++ {
				on := false
				switch r {
				case '░':
					on = (x+2*i)%4 == 0
				case '▒':
					on = (x+i)%2 == 0
				case '▓':
					on = (x+2*i)%4 != 0
				}
				if on {
					v |= 0x8000 >> x
				}
			}
			bits[i] = v
		}
		return true
	}
	s, ok := boxLines[r]
	if !ok {
		return false
	}
	cx, cy := cw/2, fontH/2
	// 縦の線（上下）
	vline := func(y0, y1 int, wgt uint8) {
		var v uint16
		switch wgt {
		case 1:
			v = span(cx, cx+1)
		case 2:
			v = span(cx-1, cx+1)
		case 3:
			v = span(cx-1, cx) | span(cx+1, cx+2)
		}
		for y := y0; y < y1; y++ {
			bits[y] |= v
		}
	}
	hline := func(x0, x1 int, wgt uint8) {
		switch wgt {
		case 1:
			bits[cy] |= span(x0, x1)
		case 2:
			bits[cy-1] |= span(x0, x1)
			bits[cy] |= span(x0, x1)
		case 3:
			bits[cy-1] |= span(x0, x1)
			bits[cy+1] |= span(x0, x1)
		}
	}
	if s[0] > 0 {
		vline(0, cy+1, s[0])
	}
	if s[1] > 0 {
		vline(cy, fontH, s[1])
	}
	if s[2] > 0 {
		hline(0, cx+1, s[2])
	}
	if s[3] > 0 {
		hline(cx, cw, s[3])
	}
	return true
}
