package main

import (
	"image"
	"strings"
	"sync"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 画面の下に重ねて出すもの：知らせと、Brain のキーボードで文字を入れる欄 ----------
//
// 知らせ（「PC につながっていません」「電池が少なくなりました」など）は、画面の下に数秒だけ出す。
// 文字を入れる欄（input: todo、input: clock）は、開いているあいだ本体キーをすべて受け取る（PC には送らない）。
// 入れられるのは英数字と記号（US 配列。端末モードと同じく「記号」とシフトで全部の記号が打てる）。日本語は入れられない。
//
//	決定        確定する（Todo を足す、時刻を合わせる）
//	戻る/調べる  やめる
//	後退        カーソルの前の 1 文字を消す
//	← →        カーソルを動かす（↑ で先頭、↓ で最後）
//	マーカーテスト（Delete） カーソルの後ろの 1 文字を消す
//
// 欄と知らせは格子の上に重ねて描く（display.go の Display.SetOverlay）。セルを描き直したときに重なっていれば、欄も描き直す。

const (
	inputTodo  = "todo"  // Todo を足す
	inputClock = "clock" // 時刻を合わせる

	inputMaxRunes  = todoMaxRunes
	noticeFor      = 4 * time.Second
	clockInputForm = "2006-01-02 15:04"
)

var inputKinds = map[string]bool{inputTodo: true, inputClock: true}

// inputLabels は、input のセルに出す文字。
var inputLabels = map[string]string{inputTodo: "Todo追加", inputClock: "時刻合わせ"}

// 知らせの重さ
const (
	noticeInfo = iota
	noticeWarn
	noticeError
)

// Overlay は、格子の上に重ねて描くもの。
type Overlay struct {
	Notice string // 知らせ。空なら出さない
	Level  int
	Input  *InputView // 文字を入れる欄。nil なら出さない
}

// InputView は、文字を入れる欄に描くもの。
type InputView struct {
	Title  string
	Text   string
	Cursor int    // 文字の位置（rune）
	Hint   string // 下に出す操作の説明
	Err    string // 確定できなかった理由（Hint の代わりに赤で出す）
}

// 欄の配置（論理の画面のドット）
const (
	ovMargin  = 8
	ovPad     = 8
	ovNoticeH = 12*2 + 2*ovPad + 4 // 倍率 2 の 1 行
	ovTextS   = 3                  // 入れている文字の倍率
	ovInputH  = 2*ovPad + 24 + 6 + 12*ovTextS + 6 + 24
	ovBorder  = 2
)

var (
	colOverlay       = RGB{0x10, 0x18, 0x24}
	colOverlayBorder = RGB{0x60, 0x90, 0xd0}
	colInputBox      = RGB{0x00, 0x00, 0x00}
	colCursor        = RGB{0xff, 0xd0, 0x40}
)

// noticeInk は知らせの文字の色。
func noticeInk(level int) RGB {
	switch level {
	case noticeWarn:
		return colInkWarn
	case noticeError:
		return colInkError
	}
	return colText
}

// overlayRects は、W×H の論理の画面での、知らせと欄の範囲（ないものは空）。欄があれば、知らせはその上に出す。
func overlayRects(o *Overlay, W, H int) (notice, input image.Rectangle) {
	if o == nil {
		return
	}
	bottom := H - ovMargin
	if o.Input != nil {
		input = image.Rect(ovMargin, bottom-ovInputH, W-ovMargin, bottom)
		bottom = input.Min.Y - ovMargin/2
	}
	if o.Notice != "" {
		notice = image.Rect(ovMargin, bottom-ovNoticeH, W-ovMargin, bottom)
	}
	return
}

// overlayRect は、重ねて描くもの全体の範囲。
func overlayRect(o *Overlay, W, H int) image.Rectangle {
	n, i := overlayRects(o, W, H)
	return n.Union(i)
}

// drawOverlay は、知らせと欄を描き、描いた範囲を返す。
func drawOverlay(cv *Canvas, o *Overlay) image.Rectangle {
	n, in := overlayRects(o, cv.W, cv.H)
	if !n.Empty() {
		cv.fill(n, colOverlay)
		cv.frame(n, ovBorder, noticeInk(o.Level))
		msg := fitText(o.Notice, n.Dx()-2*ovPad, 2)
		x := n.Min.X + (n.Dx()-font.textWidth(msg)*2)/2
		cv.text(max(x, n.Min.X+ovPad), n.Min.Y+ovPad+2, msg, 2, noticeInk(o.Level), n.Inset(ovBorder))
	}
	if iv := o.Input; iv != nil && !in.Empty() {
		cv.fill(in, colOverlay)
		cv.frame(in, ovBorder, colOverlayBorder)
		inner := in.Inset(ovPad)
		cv.text(inner.Min.X, inner.Min.Y, fitText(iv.Title, inner.Dx(), 2), 2, colText, inner)
		box := image.Rect(inner.Min.X, inner.Min.Y+24+6-4, inner.Max.X, inner.Min.Y+24+6+12*ovTextS+4)
		cv.fill(box, colInputBox)
		cv.frame(box, 1, colSub)
		drawInputText(cv, box.Inset(4), iv)
		hint, ink := iv.Hint, colSub
		if iv.Err != "" {
			hint, ink = iv.Err, colInkError
		}
		cv.text(inner.Min.X, inner.Max.Y-24, fitText(hint, inner.Dx(), 2), 2, ink, inner)
	}
	return n.Union(in)
}

// drawInputText は、入れている文字とカーソルを描く。入りきらなければ、カーソルが見える範囲を出す。
func drawInputText(cv *Canvas, r image.Rectangle, iv *InputView) {
	rs := []rune(iv.Text)
	cur := min(max(iv.Cursor, 0), len(rs))
	cw := font.textWidth("M") * ovTextS // 英数字は同じ幅
	fit := max(r.Dx()/cw-1, 1)
	start := 0
	if cur > fit {
		start = cur - fit
	}
	end := min(len(rs), start+fit)
	cv.text(r.Min.X, r.Min.Y, string(rs[start:end]), ovTextS, colText, r)
	x := r.Min.X + font.textWidth(string(rs[start:cur]))*ovTextS
	cv.fill(image.Rect(x, r.Min.Y, min(x+3, r.Max.X), r.Min.Y+12*ovTextS), colCursor)
}

// fitText は、倍率 scale で幅 w に入るように後ろを切る（切ったら … を付ける）。
func fitText(s string, w, scale int) string {
	if font.textWidth(s)*scale <= w {
		return s
	}
	rs := []rune(s)
	for len(rs) > 0 && font.textWidth(string(rs)+"…")*scale > w {
		rs = rs[:len(rs)-1]
	}
	return string(rs) + "…"
}

// ---------- 欄と知らせの係 ----------

// Prompt は、知らせと文字を入れる欄を受け持つ。
type Prompt struct {
	mu       sync.Mutex
	kind     string // 開いている欄（inputTodo など）。空なら閉じている
	text     []rune
	cur      int
	err      string
	shift    int
	notice   string
	level    int
	until    time.Time
	gen      uint64 // 知らせを出すたびに増やす（古い時間切れを捨てる）
	show     func(*Overlay)
	onCommit func(kind, text string) (string, error) // 確定。成功なら知らせる文を返す
	initial  func(kind string) string                // 開いたときに入れておく文字
	after    func(time.Duration, func())             // テストで差し替える
}

// NewPrompt は係を作る。show は、重ねて描くものが変わったときに呼ぶ（待たずに返ること）。
func NewPrompt(show func(*Overlay)) *Prompt {
	return &Prompt{show: show, after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
}

// SetHandlers は、確定したときと、開いたときに入れておく文字を決める関数を設定する。
func (p *Prompt) SetHandlers(commit func(kind, text string) (string, error), initial func(kind string) string) {
	p.mu.Lock()
	p.onCommit, p.initial = commit, initial
	p.mu.Unlock()
}

// SetShow は、重ねて描くものを渡す先を設定する（画面ができたとき）。
func (p *Prompt) SetShow(f func(*Overlay)) {
	p.mu.Lock()
	p.show = f
	p.mu.Unlock()
	p.render()
}

// Active は、欄が開いているか。
func (p *Prompt) Active() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kind != ""
}

// Open は欄を開く（開いていれば、その欄に切り替える）。
func (p *Prompt) Open(kind string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.kind, p.err, p.shift = kind, "", 0
	p.text = nil
	if p.initial != nil {
		p.text = []rune(p.initial(kind))
	}
	p.cur = len(p.text)
	p.mu.Unlock()
	p.render()
}

// Close は欄を閉じる。
func (p *Prompt) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.kind = ""
	p.mu.Unlock()
	p.render()
}

// Notify は、画面の下に知らせを noticeFor のあいだ出す。
func (p *Prompt) Notify(msg string, level int) { p.NotifyFor(msg, level, noticeFor) }

// NotifyFor は、画面の下に知らせを d のあいだ出す。d が 0 なら、次の知らせか Clear まで出したままにする。
func (p *Prompt) NotifyFor(msg string, level int, d time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.gen++
	gen := p.gen
	p.notice, p.level = msg, level
	p.mu.Unlock()
	p.render()
	if d > 0 {
		p.after(d, func() {
			p.mu.Lock()
			if p.gen != gen {
				p.mu.Unlock()
				return
			}
			p.notice = ""
			p.mu.Unlock()
			p.render()
		})
	}
}

// ClearNotice は、出している知らせが msg なら消す（空なら何でも）。
func (p *Prompt) ClearNotice(msg string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.notice == "" || (msg != "" && p.notice != msg) {
		p.mu.Unlock()
		return
	}
	p.gen++
	p.notice = ""
	p.mu.Unlock()
	p.render()
}

// Overlay は、今重ねて描くもの。何もなければ nil。
func (p *Prompt) Overlay() *Overlay {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overlayLocked()
}

func (p *Prompt) overlayLocked() *Overlay {
	if p.kind == "" && p.notice == "" {
		return nil
	}
	o := &Overlay{Notice: p.notice, Level: p.level}
	if p.kind != "" {
		o.Input = &InputView{Title: inputTitle[p.kind], Text: string(p.text), Cursor: p.cur, Hint: inputHint[p.kind], Err: p.err}
	}
	return o
}

var inputTitle = map[string]string{inputTodo: "Todo を足す（英数字と記号）", inputClock: "時刻を合わせる（年-月-日 時:分）"}
var inputHint = map[string]string{inputTodo: "決定：足す　戻る：やめる　後退：1 字消す", inputClock: "決定：合わせる　戻る：やめる　例 2026-10-10 09:41"}

func (p *Prompt) render() {
	p.mu.Lock()
	o, f := p.overlayLocked(), p.show
	p.mu.Unlock()
	if f != nil {
		f(o)
	}
}

// Key は、欄が開いているあいだの本体キーのイベント（value：1 押す、0 離す、2 繰り返し）。
func (p *Prompt) Key(code evdev.EvCode, value int32) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.kind == "" {
		p.mu.Unlock()
		return
	}
	switch code {
	case evdev.KEY_LEFTSHIFT, evdev.KEY_RIGHTSHIFT:
		p.shift = max(p.shift+map[int32]int{1: 1, 0: -1}[value], 0)
		p.mu.Unlock()
		return
	}
	if value == 0 {
		p.mu.Unlock()
		return
	}
	commit := false
	switch code {
	case evdev.KEY_ENTER, evdev.KEY_KPENTER:
		commit = value == 1
	case evdev.KEY_ESC:
		if value == 1 {
			p.kind = ""
		}
	case evdev.KEY_BACKSPACE:
		if p.cur > 0 {
			p.text = append(p.text[:p.cur-1], p.text[p.cur:]...)
			p.cur--
		}
	case evdev.KEY_DELETE:
		if p.cur < len(p.text) {
			p.text = append(p.text[:p.cur], p.text[p.cur+1:]...)
		}
	case evdev.KEY_LEFT:
		p.cur = max(p.cur-1, 0)
	case evdev.KEY_RIGHT:
		p.cur = min(p.cur+1, len(p.text))
	case evdev.KEY_UP, evdev.KEY_HOME:
		p.cur = 0
	case evdev.KEY_DOWN, evdev.KEY_END:
		p.cur = len(p.text)
	default:
		ch, ok := usKeys[code]
		if !ok || len(p.text) >= inputMaxRunes {
			p.mu.Unlock()
			return
		}
		c := ch[0]
		if p.shift > 0 {
			c = ch[1]
		}
		p.text = append(p.text[:p.cur], append([]rune{rune(c)}, p.text[p.cur:]...)...)
		p.cur++
	}
	p.err = ""
	kind, text, f := p.kind, string(p.text), p.onCommit
	p.mu.Unlock()
	if commit && f != nil {
		msg, err := f(kind, text)
		p.mu.Lock()
		if err != nil {
			p.err = err.Error()
		} else if p.kind == kind {
			p.kind = ""
		}
		p.mu.Unlock()
		if err == nil && msg != "" {
			p.Notify(msg, noticeInfo) // render も行う
			return
		}
	}
	p.render()
}

// parseClockInput は、時刻の欄の文字（2026-10-10 09:41、秒も書ける）を loc の時刻にする。
func parseClockInput(s string, loc *time.Location) (time.Time, error) {
	s = strings.Join(strings.Fields(s), " ")
	for _, f := range []string{"2006-01-02 15:04:05", clockInputForm, "2006-1-2 15:04"} {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errClockInput
}

type inputError string

func (e inputError) Error() string { return string(e) }

const errClockInput = inputError("形が違います（2026-10-10 09:41 の形で）")
