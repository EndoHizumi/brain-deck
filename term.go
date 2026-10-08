package main

import (
	"fmt"
	"unicode/utf8"
)

// ---------- 端末（VT100 / xterm の一部） ----------
//
// 端末モード（termmode.go）で、PC のログインや ssh の出力を解釈して画面の文字の格子にする。
// TERM=xterm-256color のプログラム（bash、less、vim、top、htop、mc など）が使う範囲を実装する。
// 知らない制御シーケンスは読み捨てる（画面には出さない）。
//
// 文字の幅は、PC 側（glibc の wcwidth、UTF-8）に合わせる。東アジアの全角は 2 桁、
// 罫線などの「あいまいな幅」の文字は 1 桁、結合文字は 0 桁（読み捨てる）。
//
// 描く側（termdraw.go）は TakeFrame で、前に取ったあとに変わった行だけを受け取る。
// 画面全体が上に流れたとき（ふつうの出力）は、流れた行数も受け取り、画面の点をずらして描き直しを減らす。

type termAttr uint8

const (
	attrBold termAttr = 1 << iota
	attrDim
	attrUnderline
	attrReverse
	attrStrike
	attrItalic // 字形がないので描かない
	attrHidden
)

// tcolor は文字の色。0 は既定の色、1〜256 は 256 色の番号 + 1、tcRGB が立っていれば 24 ビットの色。
type tcolor uint32

const (
	tcDefault tcolor = 0
	tcRGB     tcolor = 1 << 24
)

func tcPalette(i int) tcolor          { return tcolor(i + 1) }
func tcTrue(r, g, b uint8) tcolor     { return tcRGB | tcolor(r)<<16 | tcolor(g)<<8 | tcolor(b) }
func (c tcolor) palette() (int, bool) { return int(c) - 1, c != tcDefault && c&tcRGB == 0 }

// tcell は格子の 1 桁。全角の文字は 2 桁を使い、右の桁は w == 0。
type tcell struct {
	r      rune // 0 は空白
	w      uint8
	fg, bg tcolor
	at     termAttr
}

var blankCell = tcell{w: 1}

type tcursor struct {
	x, y     int
	pen      tcell
	origin   bool
	charsets [2]byte
	gl       int
	wrapNext bool
}

// Term は端末の状態。ロックは持たない（termmode.go が守る）。
type Term struct {
	cols, rows int
	main, alt  [][]tcell
	scr        [][]tcell // 今の画面（main か alt）
	altOn      bool
	sb         [][]tcell // 流れて消えた行（main の画面だけ）。古い順
	sbMax      int

	cur       tcursor
	savedMain tcursor
	savedAlt  tcursor
	top, bot  int // スクロールの範囲（両端を含む）
	tabs      []bool
	autowrap  bool
	insert    bool
	newline   bool // LNM：LF で行の先頭にも戻る
	appCursor bool // DECCKM：矢印を ESC O A で送る
	appKeypad bool
	cursorOn  bool
	reverse   bool // DECSCNM：画面全体の色を反転
	bracketed bool // 貼り付けの括り（記録だけ）
	lastPrint rune
	reply     func([]byte) // 問い合わせへの答え（PC に送る）
	Title     string

	// パーサ
	pstate  int
	params  []int
	sub     []bool // params[i] が ':' のあとか（SGR の 38:2:… の形）
	priv    byte   // '?'、'>'、'=' など
	inter   []byte
	osc     []byte
	oscTerm string // OSC を閉じた文字（答えに同じものを使う）
	u8      []byte

	// 描き直しの記録
	dirty    []bool
	allDirty bool
	scrolled int // 前に TakeFrame したあと、画面全体が上に流れた行数
	view     int // 履歴を見ている行数（0 なら今の画面）
	drawnCur struct {
		x, y int
		on   bool
	}
	// Received は、受け取ったバイト数（接続の表示に使う）
	Received int64
}

const (
	psGround = iota
	psEsc
	psEscInter
	psCSI
	psOSC
	psOSCEsc
	psString // DCS、SOS、PM、APC（ST まで読み捨てる）
	psStringEsc
)

// NewTerm は cols × rows の端末を作る。scrollback は履歴に残す行数。
func NewTerm(cols, rows, scrollback int, reply func([]byte)) *Term {
	t := &Term{cols: cols, rows: rows, sbMax: scrollback, reply: reply}
	t.main = newGrid(cols, rows)
	t.alt = newGrid(cols, rows)
	t.reset()
	return t
}

func newGrid(cols, rows int) [][]tcell {
	g := make([][]tcell, rows)
	for i := range g {
		g[i] = newLine(cols, blankCell)
	}
	return g
}

func newLine(cols int, c tcell) []tcell {
	l := make([]tcell, cols)
	for i := range l {
		l[i] = c
	}
	return l
}

// reset は RIS（ESC c）。履歴は残す。
func (t *Term) reset() {
	t.scr = t.main
	t.altOn = false
	for _, g := range [][][]tcell{t.main, t.alt} {
		for _, l := range g {
			for i := range l {
				l[i] = blankCell
			}
		}
	}
	t.cur = tcursor{pen: blankCell}
	t.savedMain, t.savedAlt = t.cur, t.cur
	t.top, t.bot = 0, t.rows-1
	t.tabs = make([]bool, t.cols)
	for i := 8; i < t.cols; i += 8 {
		t.tabs[i] = true
	}
	t.autowrap, t.insert, t.newline = true, false, false
	t.appCursor, t.appKeypad, t.cursorOn, t.reverse, t.bracketed = false, false, true, false, false
	t.dirty = make([]bool, t.rows)
	t.allDirty = true
}

// Size は桁数と行数。
func (t *Term) Size() (cols, rows int) { return t.cols, t.rows }

// AppCursor は、矢印を ESC O A の形で送るか（DECCKM）。
func (t *Term) AppCursor() bool { return t.appCursor }

// ---------- 入力（PC からのバイト列） ----------

// Write は PC から届いたバイト列を解釈する。
func (t *Term) Write(b []byte) {
	t.Received += int64(len(b))
	for _, c := range b {
		t.byte1(c)
	}
}

func (t *Term) byte1(c byte) {
	switch t.pstate {
	case psOSC:
		switch {
		case c == 0x07:
			t.oscTerm = "\a"
			t.oscDone()
		case c == 0x1b:
			t.pstate = psOSCEsc
		case c == 0x18 || c == 0x1a:
			t.pstate = psGround
		default:
			if len(t.osc) < 512 {
				t.osc = append(t.osc, c)
			}
		}
		return
	case psOSCEsc:
		if c == '\\' {
			t.oscTerm = "\x1b\\"
			t.oscDone()
			return
		}
		t.pstate = psGround
		t.byte1(0x1b)
		t.byte1(c)
		return
	case psString:
		switch c {
		case 0x1b:
			t.pstate = psStringEsc
		case 0x18, 0x1a, 0x07:
			t.pstate = psGround
		}
		return
	case psStringEsc:
		if c == '\\' {
			t.pstate = psGround
		} else {
			t.pstate = psString
		}
		return
	}

	// UTF-8 の途中
	if len(t.u8) > 0 {
		if c&0xc0 == 0x80 {
			t.u8 = append(t.u8, c)
			if utf8.FullRune(t.u8) {
				r, _ := utf8.DecodeRune(t.u8)
				t.u8 = t.u8[:0]
				t.print(r)
			}
			return
		}
		t.u8 = t.u8[:0]
		t.print(utf8.RuneError)
	}

	if c < 0x20 || c == 0x7f {
		t.control(c)
		return
	}
	switch t.pstate {
	case psGround:
		if c >= 0x80 {
			if c >= 0xc2 && c <= 0xf4 {
				t.u8 = append(t.u8, c)
			} else {
				t.print(utf8.RuneError)
			}
			return
		}
		t.print(rune(c))
	case psEsc:
		t.escape(c)
	case psEscInter:
		if c >= 0x20 && c <= 0x2f {
			t.inter = append(t.inter, c)
			return
		}
		t.escInter(c)
		t.pstate = psGround
	case psCSI:
		t.csiByte(c)
	}
}

func (t *Term) control(c byte) {
	switch c {
	case 0x1b:
		t.pstate = psEsc
		t.inter = t.inter[:0]
		return
	case 0x18, 0x1a: // CAN、SUB：シーケンスを取り消す
		t.pstate = psGround
		return
	case 0x07: // BEL
	case 0x08: // BS
		t.cur.wrapNext = false
		if t.cur.x > 0 {
			t.cur.x--
		}
	case 0x09:
		t.tab(1)
	case 0x0a, 0x0b, 0x0c:
		t.lineFeed()
		if t.newline {
			t.cur.x = 0
		}
	case 0x0d:
		t.cur.x = 0
		t.cur.wrapNext = false
	case 0x0e: // SO
		t.cur.gl = 1
	case 0x0f: // SI
		t.cur.gl = 0
	}
	// ほかの C0 と DEL は無視する。CSI の途中でも、制御文字は実行してシーケンスを続ける
}

func (t *Term) escape(c byte) {
	t.pstate = psGround
	switch c {
	case '[':
		t.pstate = psCSI
		t.params, t.sub, t.priv, t.inter = t.params[:0], t.sub[:0], 0, t.inter[:0]
		t.params = append(t.params, -1)
		t.sub = append(t.sub, false)
	case ']':
		t.pstate = psOSC
		t.osc = t.osc[:0]
	case 'P', 'X', '^', '_':
		t.pstate = psString
	case '7':
		t.saveCursor()
	case '8':
		t.restoreCursor()
	case 'D':
		t.lineFeed()
	case 'E':
		t.lineFeed()
		t.cur.x = 0
	case 'H':
		t.tabs[t.cur.x] = true
	case 'M':
		t.reverseIndex()
	case 'c':
		t.reset()
	case '=':
		t.appKeypad = true
	case '>':
		t.appKeypad = false
	case 'Z':
		t.send("\x1b[?1;2c")
	case '\\': // ST（閉じる相手がない）
	default:
		if c >= 0x20 && c <= 0x2f {
			t.inter = append(t.inter[:0], c)
			t.pstate = psEscInter
		}
	}
}

func (t *Term) escInter(c byte) {
	if len(t.inter) == 0 {
		return
	}
	switch t.inter[0] {
	case '(', ')':
		t.cur.charsets[t.inter[0]-'('] = c
	case '#':
		if c == '8' { // DECALN：画面を E で埋める
			for y := range t.scr {
				for x := range t.scr[y] {
					t.scr[y][x] = tcell{r: 'E', w: 1}
				}
			}
			t.allDirty = true
		}
	}
}

func (t *Term) csiByte(c byte) {
	n := len(t.params) - 1
	switch {
	case c >= '0' && c <= '9':
		if t.params[n] < 0 {
			t.params[n] = 0
		}
		if t.params[n] < 100000 {
			t.params[n] = t.params[n]*10 + int(c-'0')
		}
	case c == ';' || c == ':':
		if len(t.params) < 32 {
			t.params = append(t.params, -1)
			t.sub = append(t.sub, c == ':')
		}
	case c >= '<' && c <= '?':
		t.priv = c
	case c >= 0x20 && c <= 0x2f:
		t.inter = append(t.inter, c)
	case c >= 0x40 && c <= 0x7e:
		t.pstate = psGround
		t.csi(c)
	default:
		t.pstate = psGround
	}
}

// arg は i 番目の引数。省略（または 0）なら def。
func (t *Term) arg(i, def int) int {
	if i >= len(t.params) || t.params[i] <= 0 {
		return def
	}
	return t.params[i]
}

func (t *Term) send(s string) {
	if t.reply != nil {
		t.reply([]byte(s))
	}
}

func (t *Term) csi(c byte) {
	if len(t.inter) > 0 {
		switch {
		case t.inter[0] == '!' && c == 'p': // DECSTR
			t.softReset()
		case t.inter[0] == '$' && c == 'p': // DECRQM
			t.reportMode()
		}
		// DECSCUSR（カーソルの形）などは無視する
		return
	}
	switch t.priv {
	case '?':
		switch c {
		case 'h', 'l':
			for i := range t.params {
				t.decset(t.params[i], c == 'h')
			}
		case 'n':
			if t.arg(0, 0) == 6 {
				t.send(fmt.Sprintf("\x1b[?%d;%dR", t.cur.y-t.originTop()+1, t.cur.x+1))
			}
		}
		return
	case '>':
		if c == 'c' { // DA2：古い xterm のふりをする（問い合わせを増やさない）
			t.send("\x1b[>0;10;1c")
		}
		return
	case 0:
	default:
		return
	}
	cx, cy := t.cur.x, t.cur.y
	switch c {
	case '@':
		t.insertChars(t.arg(0, 1))
	case 'A':
		t.moveTo(cx, max(cy-t.arg(0, 1), t.upLimit()))
	case 'B', 'e':
		t.moveTo(cx, min(cy+t.arg(0, 1), t.downLimit()))
	case 'C', 'a':
		t.moveTo(cx+t.arg(0, 1), cy)
	case 'D':
		t.moveTo(cx-t.arg(0, 1), cy)
	case 'E':
		t.moveTo(0, min(cy+t.arg(0, 1), t.downLimit()))
	case 'F':
		t.moveTo(0, max(cy-t.arg(0, 1), t.upLimit()))
	case 'G', '`':
		t.moveTo(t.arg(0, 1)-1, cy)
	case 'H', 'f':
		t.moveTo(t.arg(1, 1)-1, t.originTop()+t.arg(0, 1)-1)
	case 'I':
		t.tab(t.arg(0, 1))
	case 'J':
		t.eraseDisplay(t.arg(0, 0))
	case 'K':
		t.eraseLine(t.arg(0, 0))
	case 'L':
		t.insertLines(t.arg(0, 1))
	case 'M':
		t.deleteLines(t.arg(0, 1))
	case 'P':
		t.deleteChars(t.arg(0, 1))
	case 'S':
		t.scrollUp(t.top, t.bot, t.arg(0, 1))
	case 'T':
		if len(t.params) == 1 { // 引数が多いのはマウスの追跡（無視する）
			t.scrollDown(t.top, t.bot, t.arg(0, 1))
		}
	case 'X':
		n := min(t.arg(0, 1), t.cols-cx)
		t.erase(cy, cx, cx+n)
	case 'Z':
		t.backTab(t.arg(0, 1))
	case 'b':
		if t.lastPrint != 0 {
			for range min(t.arg(0, 1), t.cols*t.rows) {
				t.print(t.lastPrint)
			}
		}
	case 'c':
		if t.arg(0, 0) == 0 {
			t.send("\x1b[?1;2c")
		}
	case 'd':
		t.moveTo(cx, t.originTop()+t.arg(0, 1)-1)
	case 'g':
		switch t.arg(0, 0) {
		case 0:
			t.tabs[cx] = false
		case 3:
			clear(t.tabs)
		}
	case 'h', 'l':
		for i := range t.params {
			switch t.params[i] {
			case 4:
				t.insert = c == 'h'
			case 20:
				t.newline = c == 'h'
			}
		}
	case 'm':
		t.sgr()
	case 'n':
		switch t.arg(0, 0) {
		case 5:
			t.send("\x1b[0n")
		case 6:
			t.send(fmt.Sprintf("\x1b[%d;%dR", t.cur.y-t.originTop()+1, t.cur.x+1))
		}
	case 'r':
		top, bot := t.arg(0, 1)-1, t.arg(1, t.rows)-1
		if top < bot && bot < t.rows {
			t.top, t.bot = top, bot
			t.moveTo(0, t.originTop())
		}
	case 's':
		t.saveCursor()
	case 'u':
		t.restoreCursor()
	case 't':
		switch t.arg(0, 0) {
		case 18:
			t.send(fmt.Sprintf("\x1b[8;%d;%dt", t.rows, t.cols))
		}
	}
}

func (t *Term) originTop() int {
	if t.cur.origin {
		return t.top
	}
	return 0
}

// upLimit、downLimit は、カーソルを上下に動かせる端（スクロールの範囲の中なら、範囲の端）。
func (t *Term) upLimit() int {
	if t.cur.y >= t.top {
		return t.top
	}
	return 0
}

func (t *Term) downLimit() int {
	if t.cur.y <= t.bot {
		return t.bot
	}
	return t.rows - 1
}

func (t *Term) moveTo(x, y int) {
	lo, hi := 0, t.rows-1
	if t.cur.origin {
		lo, hi = t.top, t.bot
	}
	t.cur.x = min(max(x, 0), t.cols-1)
	t.cur.y = min(max(y, lo), hi)
	t.cur.wrapNext = false
}

func (t *Term) decset(p int, on bool) {
	switch p {
	case 1:
		t.appCursor = on
	case 5:
		if t.reverse != on {
			t.reverse = on
			t.allDirty = true
		}
	case 6:
		t.cur.origin = on
		t.moveTo(0, t.originTop())
	case 7:
		t.autowrap = on
	case 25:
		t.cursorOn = on
	case 47, 1047:
		t.setAlt(on, false)
	case 1048:
		if on {
			t.saveCursor()
		} else {
			t.restoreCursor()
		}
	case 1049:
		if on {
			t.saveCursor()
			t.setAlt(true, true)
		} else {
			t.setAlt(false, false)
			t.restoreCursor()
		}
	case 2004:
		t.bracketed = on
	}
}

func (t *Term) setAlt(on, clearIt bool) {
	if on == t.altOn {
		return
	}
	t.altOn = on
	if on {
		t.scr = t.alt
		if clearIt {
			for y := range t.alt {
				t.fillLine(y, 0, t.cols, t.blank())
			}
		}
	} else {
		t.scr = t.main
	}
	t.view = 0
	t.allDirty = true
}

func (t *Term) modeState(p int) int {
	b := func(v bool) int {
		if v {
			return 1
		}
		return 2
	}
	switch p {
	case 1:
		return b(t.appCursor)
	case 5:
		return b(t.reverse)
	case 6:
		return b(t.cur.origin)
	case 7:
		return b(t.autowrap)
	case 25:
		return b(t.cursorOn)
	case 47, 1047, 1049:
		return b(t.altOn)
	case 2004:
		return b(t.bracketed)
	}
	return 0 // 知らない
}

func (t *Term) reportMode() {
	p := t.arg(0, 0)
	if t.priv == '?' {
		t.send(fmt.Sprintf("\x1b[?%d;%d$y", p, t.modeState(p)))
	} else {
		v := 0
		switch p {
		case 4:
			v = map[bool]int{true: 1, false: 2}[t.insert]
		case 20:
			v = map[bool]int{true: 1, false: 2}[t.newline]
		}
		t.send(fmt.Sprintf("\x1b[%d;%d$y", p, v))
	}
}

func (t *Term) softReset() {
	t.cursorOn, t.insert, t.cur.origin, t.autowrap, t.appCursor, t.appKeypad = true, false, false, true, false, false
	t.top, t.bot = 0, t.rows-1
	t.cur.pen = blankCell
	t.cur.charsets, t.cur.gl = [2]byte{}, 0
	t.savedMain, t.savedAlt = tcursor{pen: blankCell}, tcursor{pen: blankCell}
}

func (t *Term) saveCursor() {
	if t.altOn {
		t.savedAlt = t.cur
	} else {
		t.savedMain = t.cur
	}
}

func (t *Term) restoreCursor() {
	s := t.savedMain
	if t.altOn {
		s = t.savedAlt
	}
	t.cur = s
	if t.cur.pen.w == 0 {
		t.cur.pen = blankCell
	}
	t.cur.x = min(t.cur.x, t.cols-1)
	t.cur.y = min(t.cur.y, t.rows-1)
}

func (t *Term) oscDone() {
	t.pstate = psGround
	s := string(t.osc)
	num, rest, _ := cut(s, ';')
	switch num {
	case "0", "2":
		t.Title = rest
	case "10", "11":
		// 文字と背景の色の問い合わせ（vim が背景の明るさを調べる）。黒地に明るい灰色と答える
		if rest == "?" {
			col := map[string]string{"10": "c0c0/c0c0/c0c0", "11": "0000/0000/0000"}[num]
			t.send("\x1b]" + num + ";rgb:" + col + t.oscTerm)
		}
	}
}

func cut(s string, sep byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// ---------- SGR（文字の色と飾り） ----------

func (t *Term) sgr() {
	p := &t.cur.pen
	for i := 0; i < len(t.params); i++ {
		v := t.params[i]
		if v < 0 {
			v = 0
		}
		// ':' で続く下位の引数（4:3 の下線の種類など）は、ここでは読み飛ばす
		next := func() int {
			j := i + 1
			for j < len(t.params) && t.sub[j] {
				j++
			}
			return j
		}
		switch {
		case v == 0:
			*p = blankCell
		case v == 1:
			p.at |= attrBold
		case v == 2:
			p.at |= attrDim
		case v == 3:
			p.at |= attrItalic
		case v == 4:
			if i+1 < len(t.params) && t.sub[i+1] && t.params[i+1] == 0 {
				p.at &^= attrUnderline
			} else {
				p.at |= attrUnderline
			}
			i = next() - 1
		case v == 7:
			p.at |= attrReverse
		case v == 8:
			p.at |= attrHidden
		case v == 9:
			p.at |= attrStrike
		case v == 21:
			p.at |= attrUnderline
		case v == 22:
			p.at &^= attrBold | attrDim
		case v == 23:
			p.at &^= attrItalic
		case v == 24:
			p.at &^= attrUnderline
		case v == 27:
			p.at &^= attrReverse
		case v == 28:
			p.at &^= attrHidden
		case v == 29:
			p.at &^= attrStrike
		case v >= 30 && v <= 37:
			p.fg = tcPalette(v - 30)
		case v == 39:
			p.fg = tcDefault
		case v >= 40 && v <= 47:
			p.bg = tcPalette(v - 40)
		case v == 49:
			p.bg = tcDefault
		case v >= 90 && v <= 97:
			p.fg = tcPalette(v - 90 + 8)
		case v >= 100 && v <= 107:
			p.bg = tcPalette(v - 100 + 8)
		case v == 38 || v == 48:
			c, used := t.extColor(i + 1)
			if c != nil {
				if v == 38 {
					p.fg = *c
				} else {
					p.bg = *c
				}
			}
			i += used
		}
	}
}

// extColor は 38 と 48 の続き（5;n、2;r;g;b、2:cs:r:g:b）を読む。使った引数の数も返す。
func (t *Term) extColor(i int) (*tcolor, int) {
	get := func(j int) int {
		if j < len(t.params) && t.params[j] > 0 {
			return min(t.params[j], 255)
		}
		return 0
	}
	if i >= len(t.params) {
		return nil, 0
	}
	colon := t.sub[i]
	switch t.params[i] {
	case 5:
		c := tcPalette(get(i + 1))
		return &c, 2
	case 2:
		j := i + 1
		// 38:2:<色空間>:r:g:b（コロンの形は色空間の番号が入ることがある）
		if colon {
			n := 0
			for k := j; k < len(t.params) && t.sub[k]; k++ {
				n++
			}
			if n >= 4 {
				j++
			}
			c := tcTrue(uint8(get(j)), uint8(get(j+1)), uint8(get(j+2)))
			return &c, n + 1
		}
		c := tcTrue(uint8(get(j)), uint8(get(j+1)), uint8(get(j+2)))
		return &c, 4
	}
	return nil, 1
}

// ---------- 文字を置く ----------

// decGraphics は DEC の線画の文字集合（ESC ( 0）。mc や dialog の枠に使われる。
var decGraphics = map[rune]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊', 'f': '°', 'g': '±', 'h': '␤', 'i': '␋',
	'j': '┘', 'k': '┐', 'l': '┌', 'm': '└', 'n': '┼', 'o': '⎺', 'p': '⎻', 'q': '─', 'r': '⎼', 's': '⎽',
	't': '├', 'u': '┤', 'v': '┴', 'w': '┬', 'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠', '}': '£', '~': '·',
}

func (t *Term) blank() tcell {
	return tcell{w: 1, bg: t.cur.pen.bg}
}

func (t *Term) print(r rune) {
	if t.cur.charsets[t.cur.gl] == '0' {
		if g, ok := decGraphics[r]; ok {
			r = g
		}
	}
	w := runeWidth(r)
	if w == 0 {
		return // 結合文字などは読み捨てる
	}
	t.lastPrint = r
	if t.cur.wrapNext {
		t.cur.wrapNext = false
		if t.autowrap {
			t.cur.x = 0
			t.lineFeed()
		}
	}
	if w == 2 && t.cur.x == t.cols-1 {
		if t.autowrap {
			t.fillLine(t.cur.y, t.cur.x, t.cols, t.blank())
			t.cur.x = 0
			t.lineFeed()
		} else {
			w = 1 // 入らない全角は、右端に半分だけ置かない
			r = ' '
		}
	}
	y, x := t.cur.y, t.cur.x
	if t.insert {
		t.insertChars(w)
	}
	line := t.scr[y]
	t.unsplit(y, x)
	if w == 2 {
		t.unsplit(y, x+1)
	}
	c := t.cur.pen
	c.r, c.w = r, uint8(w)
	line[x] = c
	if w == 2 {
		c.r, c.w = 0, 0
		line[x+1] = c
	}
	t.dirty[y] = true
	if x+w >= t.cols {
		t.cur.x = t.cols - 1
		t.cur.wrapNext = true
	} else {
		t.cur.x = x + w
	}
}

// unsplit は、(y, x) を書き換える前に、そこにかかっている全角の文字を消す（半分だけ残さない）。
func (t *Term) unsplit(y, x int) {
	if x < 0 || x >= t.cols {
		return
	}
	line := t.scr[y]
	switch {
	case line[x].w == 0 && x > 0:
		line[x-1].r, line[x-1].w = 0, 1
		line[x].r, line[x].w = 0, 1
	case line[x].w == 2 && x+1 < t.cols:
		line[x+1].r, line[x+1].w = 0, 1
		line[x].r, line[x].w = 0, 1
	}
}

func (t *Term) fillLine(y, x0, x1 int, c tcell) {
	line := t.scr[y]
	for x := max(x0, 0); x < min(x1, t.cols); x++ {
		line[x] = c
	}
	t.dirty[y] = true
}

// erase は y 行の [x0, x1) を消す（今の背景色で）。
func (t *Term) erase(y, x0, x1 int) {
	t.unsplit(y, x0)
	t.unsplit(y, x1-1)
	t.fillLine(y, x0, x1, t.blank())
}

func (t *Term) eraseDisplay(mode int) {
	cx, cy := t.cur.x, t.cur.y
	switch mode {
	case 0:
		t.erase(cy, cx, t.cols)
		for y := cy + 1; y < t.rows; y++ {
			t.fillLine(y, 0, t.cols, t.blank())
		}
	case 1:
		for y := 0; y < cy; y++ {
			t.fillLine(y, 0, t.cols, t.blank())
		}
		t.erase(cy, 0, cx+1)
	case 2:
		for y := 0; y < t.rows; y++ {
			t.fillLine(y, 0, t.cols, t.blank())
		}
	case 3: // 履歴を消す
		t.sb = nil
		t.view = 0
	}
	t.cur.wrapNext = false
}

func (t *Term) eraseLine(mode int) {
	cx, cy := t.cur.x, t.cur.y
	switch mode {
	case 0:
		t.erase(cy, cx, t.cols)
	case 1:
		t.erase(cy, 0, cx+1)
	case 2:
		t.fillLine(cy, 0, t.cols, t.blank())
	}
	t.cur.wrapNext = false
}

func (t *Term) insertChars(n int) {
	y, x := t.cur.y, t.cur.x
	n = min(n, t.cols-x)
	line := t.scr[y]
	t.unsplit(y, x)
	t.unsplit(y, t.cols-n)
	copy(line[x+n:], line[x:t.cols-n])
	for i := x; i < x+n; i++ {
		line[i] = t.blank()
	}
	t.dirty[y] = true
	t.cur.wrapNext = false
}

func (t *Term) deleteChars(n int) {
	y, x := t.cur.y, t.cur.x
	n = min(n, t.cols-x)
	line := t.scr[y]
	t.unsplit(y, x)
	t.unsplit(y, x+n-1)
	copy(line[x:], line[x+n:])
	for i := t.cols - n; i < t.cols; i++ {
		line[i] = t.blank()
	}
	t.dirty[y] = true
	t.cur.wrapNext = false
}

func (t *Term) insertLines(n int) {
	if t.cur.y < t.top || t.cur.y > t.bot {
		return
	}
	t.scrollDown(t.cur.y, t.bot, n)
	t.cur.x = 0
}

func (t *Term) deleteLines(n int) {
	if t.cur.y < t.top || t.cur.y > t.bot {
		return
	}
	t.scrollUp(t.cur.y, t.bot, n)
	t.cur.x = 0
}

func (t *Term) lineFeed() {
	t.cur.wrapNext = false
	switch {
	case t.cur.y == t.bot:
		t.scrollUp(t.top, t.bot, 1)
	case t.cur.y < t.rows-1:
		t.cur.y++
	}
}

func (t *Term) reverseIndex() {
	t.cur.wrapNext = false
	switch {
	case t.cur.y == t.top:
		t.scrollDown(t.top, t.bot, 1)
	case t.cur.y > 0:
		t.cur.y--
	}
}

// scrollUp は top〜bot の行を n 行上に流す。画面の上端から流れた行は履歴に残す（main の画面だけ）。
func (t *Term) scrollUp(top, bot, n int) {
	n = min(n, bot-top+1)
	if n <= 0 {
		return
	}
	g := t.scr
	if top == 0 && !t.altOn && t.sbMax > 0 {
		for i := 0; i < n; i++ {
			t.pushHistory(g[i])
		}
	}
	// 行の配列を回して、消えた行を下に回して使い直す
	gone := append([][]tcell(nil), g[top:top+n]...)
	copy(g[top:], g[top+n:bot+1])
	for i, l := range gone {
		for x := range l {
			l[x] = t.blank()
		}
		g[bot-n+1+i] = l
	}
	if top == 0 && bot == t.rows-1 && !t.allDirty {
		// 画面全体が流れた：描いた点をずらせるよう、行数を覚え、汚れた行の印も一緒に流す
		t.scrolled += n
		copy(t.dirty, t.dirty[n:])
		for i := t.rows - n; i < t.rows; i++ {
			t.dirty[i] = true
		}
		t.drawnCur.y -= n
		if t.view > 0 {
			t.view = min(t.view+n, len(t.sb))
		}
		return
	}
	for y := top; y <= bot; y++ {
		t.dirty[y] = true
	}
}

func (t *Term) scrollDown(top, bot, n int) {
	n = min(n, bot-top+1)
	if n <= 0 {
		return
	}
	g := t.scr
	gone := append([][]tcell(nil), g[bot-n+1:bot+1]...)
	copy(g[top+n:], g[top:bot-n+1])
	for i, l := range gone {
		for x := range l {
			l[x] = t.blank()
		}
		g[top+i] = l
	}
	for y := top; y <= bot; y++ {
		t.dirty[y] = true
	}
}

func (t *Term) pushHistory(l []tcell) {
	cp := append([]tcell(nil), l...)
	if len(t.sb) >= t.sbMax {
		copy(t.sb, t.sb[1:])
		t.sb[len(t.sb)-1] = cp
		return
	}
	t.sb = append(t.sb, cp)
}

func (t *Term) tab(n int) {
	x := t.cur.x
	for ; n > 0 && x < t.cols-1; n-- {
		x++
		for x < t.cols-1 && !t.tabs[x] {
			x++
		}
	}
	t.cur.x = x
	t.cur.wrapNext = false
}

func (t *Term) backTab(n int) {
	x := t.cur.x
	for ; n > 0 && x > 0; n-- {
		x--
		for x > 0 && !t.tabs[x] {
			x--
		}
	}
	t.cur.x = x
	t.cur.wrapNext = false
}

// ---------- 履歴を見る ----------

// Scroll は履歴を見る位置を delta 行動かす（正で古いほうへ）。動いたら true。
func (t *Term) Scroll(delta int) bool {
	if t.altOn {
		return false
	}
	v := min(max(t.view+delta, 0), len(t.sb))
	if v == t.view {
		return false
	}
	t.view = v
	t.allDirty = true
	return true
}

// View は、履歴を何行さかのぼって見ているか（0 なら今の画面）と、履歴の行数。
func (t *Term) View() (int, int) { return t.view, len(t.sb) }

// line は、見ている位置での y 行目。
func (t *Term) line(y int) []tcell {
	if t.view > 0 {
		i := len(t.sb) - t.view + y
		if i < len(t.sb) {
			return t.sb[i]
		}
		return t.scr[i-len(t.sb)]
	}
	return t.scr[y]
}

// ---------- 描く側へ渡す ----------

// termFrame は、描き直す行と、カーソルの位置。TakeFrame が中身を入れる（前の中身を使い直す）。
type termFrame struct {
	Cols, Rows int
	Scrolled   int  // 画面全体が上に流れた行数（描いた点をずらしてよい）。All なら 0
	All        bool // 全体を描き直す
	Dirty      []bool
	Lines      [][]tcell // Dirty の行だけ中身が入る
	CurX, CurY int
	CurOn      bool
	Reverse    bool
	View       int // 履歴を見ている行数
	History    int // 履歴の行数
}

// TakeFrame は、前に呼んだあとに変わった行を f に写し、変わったという記録を消す。
func (t *Term) TakeFrame(f *termFrame) {
	f.Cols, f.Rows = t.cols, t.rows
	if len(f.Dirty) != t.rows {
		f.Dirty = make([]bool, t.rows)
		f.Lines = make([][]tcell, t.rows)
	}
	f.All = t.allDirty
	f.Scrolled = 0
	if !f.All && t.scrolled > 0 {
		if t.scrolled >= t.rows {
			f.All = true
		} else {
			f.Scrolled = t.scrolled
		}
	}
	curOn := t.cursorOn && t.view == 0
	// カーソルを描いていた行と、今の行も描き直す
	if t.drawnCur.on && t.drawnCur.y >= 0 && t.drawnCur.y < t.rows {
		t.dirty[t.drawnCur.y] = true
	}
	if curOn {
		t.dirty[t.cur.y] = true
	}
	for y := 0; y < t.rows; y++ {
		d := f.All || t.dirty[y]
		f.Dirty[y] = d
		if d {
			f.Lines[y] = append(f.Lines[y][:0], t.line(y)...)
		}
	}
	f.CurX, f.CurY, f.CurOn, f.Reverse = t.cur.x, t.cur.y, curOn, t.reverse
	f.View, f.History = t.view, len(t.sb)
	t.drawnCur.x, t.drawnCur.y, t.drawnCur.on = t.cur.x, t.cur.y, curOn
	clear(t.dirty)
	t.allDirty = false
	t.scrolled = 0
}

// Invalidate は、次の TakeFrame で全体を描き直させる。
func (t *Term) Invalidate() { t.allDirty = true }

// Text は画面の文字を行ごとに返す（テスト用。右の空白は除く）。
func (t *Term) Text() []string {
	out := make([]string, t.rows)
	for y := 0; y < t.rows; y++ {
		out[y] = t.LineText(y)
	}
	return out
}

// LineText は、見ている位置での y 行目の文字（右の空白は除く）。
func (t *Term) LineText(y int) string {
	var b []rune
	for _, c := range t.line(y) {
		switch {
		case c.w == 0:
		case c.r == 0:
			b = append(b, ' ')
		default:
			b = append(b, c.r)
		}
	}
	s := string(b)
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

// Cursor は今のカーソルの位置（テスト用）。
func (t *Term) Cursor() (x, y int) { return t.cur.x, t.cur.y }

// ---------- 文字の幅 ----------

// runeWidth は、端末で使う桁数（0、1、2）。glibc の wcwidth（UTF-8、あいまいな幅は 1）に合わせた近似。
func runeWidth(r rune) int {
	switch {
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case r < 0x300:
		return 1
	}
	for _, z := range zeroWidth {
		if r >= z[0] && r <= z[1] {
			return 0
		}
	}
	for _, w := range wideRanges {
		if r < w[0] {
			break
		}
		if r <= w[1] {
			return 2
		}
	}
	return 1
}

var zeroWidth = [][2]rune{
	{0x0300, 0x036f}, {0x0483, 0x0489}, {0x0591, 0x05bd}, {0x0610, 0x061a}, {0x064b, 0x065f},
	{0x1ab0, 0x1aff}, {0x1dc0, 0x1dff}, {0x200b, 0x200f}, {0x2028, 0x202e}, {0x2060, 0x206f},
	{0x20d0, 0x20ff}, {0x302a, 0x302d}, {0x3099, 0x309a}, {0xfe00, 0xfe0f}, {0xfe20, 0xfe2f},
	{0xfeff, 0xfeff}, {0xe0100, 0xe01ef},
}

// wideRanges は東アジアの全角（W と F）の範囲。昇順。
var wideRanges = [][2]rune{
	{0x1100, 0x115f}, {0x231a, 0x231b}, {0x2329, 0x232a}, {0x23e9, 0x23ec}, {0x23f0, 0x23f0},
	{0x23f3, 0x23f3}, {0x25fd, 0x25fe}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267f, 0x267f},
	{0x2693, 0x2693}, {0x26a1, 0x26a1}, {0x26aa, 0x26ab}, {0x26bd, 0x26be}, {0x26c4, 0x26c5},
	{0x26ce, 0x26ce}, {0x26d4, 0x26d4}, {0x26ea, 0x26ea}, {0x26f2, 0x26f3}, {0x26f5, 0x26f5},
	{0x26fa, 0x26fa}, {0x26fd, 0x26fd}, {0x2705, 0x2705}, {0x270a, 0x270b}, {0x2728, 0x2728},
	{0x274c, 0x274c}, {0x274e, 0x274e}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27b0, 0x27b0}, {0x27bf, 0x27bf}, {0x2b1b, 0x2b1c}, {0x2b50, 0x2b50}, {0x2b55, 0x2b55},
	{0x2e80, 0x303e}, {0x3041, 0x33ff}, {0x3400, 0x4dbf}, {0x4e00, 0x9fff}, {0xa000, 0xa4cf},
	{0xa960, 0xa97f}, {0xac00, 0xd7a3}, {0xf900, 0xfaff}, {0xfe10, 0xfe19}, {0xfe30, 0xfe6f},
	{0xff00, 0xff60}, {0xffe0, 0xffe6}, {0x16fe0, 0x16fe4}, {0x17000, 0x18cff}, {0x1b000, 0x1b2ff},
	{0x1f004, 0x1f004}, {0x1f0cf, 0x1f0cf}, {0x1f18e, 0x1f18e}, {0x1f191, 0x1f19a}, {0x1f200, 0x1f251},
	{0x1f300, 0x1f320}, {0x1f32d, 0x1f335}, {0x1f337, 0x1f37c}, {0x1f37e, 0x1f393}, {0x1f3a0, 0x1f3ca},
	{0x1f3cf, 0x1f3d3}, {0x1f3e0, 0x1f3f0}, {0x1f3f4, 0x1f3f4}, {0x1f3f8, 0x1f43e}, {0x1f440, 0x1f440},
	{0x1f442, 0x1f4fc}, {0x1f4ff, 0x1f53d}, {0x1f54b, 0x1f54e}, {0x1f550, 0x1f567}, {0x1f57a, 0x1f57a},
	{0x1f595, 0x1f596}, {0x1f5a4, 0x1f5a4}, {0x1f5fb, 0x1f64f}, {0x1f680, 0x1f6c5}, {0x1f6cc, 0x1f6cc},
	{0x1f6d0, 0x1f6d2}, {0x1f6d5, 0x1f6d7}, {0x1f6eb, 0x1f6ec}, {0x1f6f4, 0x1f6fc}, {0x1f7e0, 0x1f7eb},
	{0x1f90c, 0x1f93a}, {0x1f93c, 0x1f945}, {0x1f947, 0x1f9ff}, {0x1fa70, 0x1faff}, {0x20000, 0x2fffd},
	{0x30000, 0x3fffd},
}
