package main

import (
	_ "embed"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- ヘルプのアプリ（widget: help） ----------
//
// 使い方の説明（help/ja.md）を、読み物のアプリと同じ描き方（横書き。readlayout.go）で読む。
// 中身はデーモンに埋め込むので、いつもデーモンの版と合う（{version} を版に置き換える）。
// 右の 3 分の 1 をタップすると次のページ、左で前のページ、真ん中で道具（目次、最初のページ、困ったとき、文字の大きさ、
// 読みつづける）。本体キーの ← → とスペース（Shift で戻る）でもめくる。文字の大きさは読み物と同じもの（24 か 32）。
// 読んでいる位置はデーモンが動いているあいだだけ覚える。

const widgetHelp = "help"

//go:embed help/ja.md
var helpText string

// helpDoc は、help/ja.md を読み物の形にする。「# 」は章（新しいページから）、「## 」は節、空の行で段落を分ける。
// 「・」で始まる行は、続けて 1 行ずつ書く（あいだを空けない）。
func helpDoc(text, ver string) *ReadDoc {
	d := &ReadDoc{Format: 1, Title: "ヘルプ", Kind: "text", Flow: "book"}
	var para []string
	blank := func() {
		if n := len(d.Blocks); n > 0 && !(d.Blocks[n-1].T == "p" && len(d.Blocks[n-1].R) == 0) && d.Blocks[n-1].T != "h" && d.Blocks[n-1].T != "pb" {
			d.Blocks = append(d.Blocks, RBlock{T: "p"})
		}
	}
	flush := func() {
		if len(para) > 0 {
			d.Blocks = append(d.Blocks, RBlock{T: "p", R: []RRun{{S: strings.Join(para, "")}}})
			para = nil
			blank()
		}
	}
	dropBlank := func() {
		if n := len(d.Blocks); n > 0 && d.Blocks[n-1].T == "p" && len(d.Blocks[n-1].R) == 0 {
			d.Blocks = d.Blocks[:n-1]
		}
	}
	chapters := 0
	for _, ln := range strings.Split(strings.ReplaceAll(text, "{version}", ver), "\n") {
		ln = strings.TrimRight(ln, " \t\r")
		switch {
		case strings.HasPrefix(ln, "# "):
			flush()
			dropBlank()
			if chapters > 0 {
				d.Blocks = append(d.Blocks, RBlock{T: "pb"})
			}
			chapters++
			d.Blocks = append(d.Blocks, RBlock{T: "h", L: 1, R: []RRun{{S: "■ " + strings.TrimPrefix(ln, "# ")}}})
		case strings.HasPrefix(ln, "## "):
			flush()
			dropBlank() // 見出しの前は、組むときに 1 行空ける
			d.Blocks = append(d.Blocks, RBlock{T: "h", L: 2, R: []RRun{{S: strings.TrimPrefix(ln, "## ")}}})
		case strings.HasPrefix(ln, "・"):
			flush()
			d.Blocks = append(d.Blocks, RBlock{T: "p", Indent: 1, R: []RRun{{S: ln}}})
		case ln == "":
			flush()
			if n := len(d.Blocks); n > 0 && d.Blocks[n-1].T == "p" && d.Blocks[n-1].Indent > 0 {
				blank() // 「・」の行の終わり
			}
		default:
			para = append(para, ln)
		}
	}
	flush()
	dropBlank()
	return d
}

// HelpUI は、ヘルプのアプリの状態（1 つのデーモンに 1 つ）。
type HelpUI struct {
	doc     *ReadDoc
	size    func() int // 文字の大きさ（読み物と同じ）
	setSize func(int)

	mu       sync.Mutex
	mode     int // readRead、readMenu、readTOC
	tocPage  int
	pos      ReadPos
	mkey     string
	m        readMetrics
	starts   []ReadPos
	gen      uint64
	onChange func()
}

// NewHelpUI は、ヘルプを作る。size が nil なら 24 ドット。
func NewHelpUI(size func() int, setSize func(int)) *HelpUI {
	return &HelpUI{doc: helpDoc(helpText, daemonVersion()), size: size, setSize: setSize, mode: readRead}
}

func (h *HelpUI) SetOnChange(f func()) { h.mu.Lock(); h.onChange = f; h.mu.Unlock() }

func (h *HelpUI) changedLocked() {
	if f := h.onChange; f != nil {
		go f()
	}
}

func (h *HelpUI) sizeNow() int {
	if h.size == nil {
		return 24
	}
	if s := h.size(); s == 32 {
		return 32
	}
	return 24
}

// metricsLocked は、範囲 area の寸法を決め、変わっていれば別の goroutine でページを数え直す。
func (h *HelpUI) metricsLocked(area image.Rectangle) readMetrics {
	body := image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Max.Y-readFooterH)
	s := h.sizeNow()
	m := readMetricsFor(body, s, false, false)
	key := fmt.Sprintf("%v\x00%d", body, s)
	h.m = m
	if key != h.mkey {
		h.mkey, h.starts = key, nil
		h.gen++
		gen, d := h.gen, h.doc
		go func() {
			t := time.Now()
			starts := paginate(d, m)
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.gen != gen {
				return
			}
			h.starts = starts
			h.pos = starts[pageOf(starts, h.pos)]
			vlogf("help: %d pages (size %d) in %v", len(starts), m.S, time.Since(t).Round(time.Millisecond))
			h.changedLocked()
		}()
	}
	return m
}

// HelpView は、描くときの写し。
type HelpView struct {
	mode, tocPage int
	pos           ReadPos
	page, pages   int
	size          int
	ui            *HelpUI
}

func (h *HelpUI) Snapshot() *HelpView {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	v := &HelpView{mode: h.mode, tocPage: h.tocPage, pos: h.pos, size: h.sizeNow(), ui: h}
	if h.starts != nil {
		v.page, v.pages = pageOf(h.starts, h.pos)+1, len(h.starts)
	}
	return v
}

func helpKey(env WidgetEnv) string {
	v := env.Help
	if v == nil {
		return "help\x00none"
	}
	return fmt.Sprintf("help\x00%d\x00%d\x00%v\x00%d\x00%d\x00%d", v.mode, v.tocPage, v.pos, v.page, v.pages, v.size)
}

// helpSection は、位置 p の章か節の見出し。
func helpSection(d *ReadDoc, p ReadPos) string {
	s := ""
	for i := 0; i < len(d.Blocks) && i <= p.B; i++ {
		if d.Blocks[i].T == "h" {
			s = strings.TrimPrefix(blockText(&d.Blocks[i]), "■ ")
		}
	}
	return s
}

func (v *HelpView) menuLabels() []string {
	sz := "文字を大きく（32）"
	if v.size == 32 {
		sz = "文字を小さく（24）"
	}
	return []string{"目次", "最初のページ", "困ったとき", sz, "読みつづける"}
}

func drawHelp(cv *Canvas, area image.Rectangle, env WidgetEnv) {
	v := env.Help
	if v == nil {
		drawAppPlaceholder(cv, area, []string{"ヘルプ", "使い方を Brain の画面で読む"})
		return
	}
	h := v.ui
	if v.mode == readTOC {
		drawReadTOC(cv, area, &ReaderView{doc: h.doc, tocPage: v.tocPage})
		return
	}
	cv.fill(area, colReadPaper)
	h.mu.Lock()
	m := h.metricsLocked(area)
	pos := h.pos
	h.mu.Unlock()
	drawReadPage(cv, h.doc, m, layoutPage(h.doc, m, pos, nil))
	foot := image.Rect(area.Min.X, area.Max.Y-readFooterH, area.Max.X, area.Max.Y)
	cv.fill(foot, colReadBar)
	num := "ページを数えています"
	if v.pages > 0 {
		num = fmt.Sprintf("%d / %d", v.page, v.pages)
	}
	nw := font.textWidth(num)
	cv.text(foot.Max.X-nw-6, foot.Min.Y+3, num, 1, colReadSub, foot)
	title := "ヘルプ"
	if s := helpSection(h.doc, pos); s != "" && pos.B > 0 {
		title += "：" + s
	}
	cv.text(foot.Min.X+6, foot.Min.Y+3, clipText(title+"（真ん中をタップで目次）", foot.Dx()-nw-24, 1), 1, colText, foot)
	if v.mode == readMenu {
		bs := menuGeometry(area)
		box := bs[len(bs)-1]
		cv.fill(box, colReadMenu)
		cv.frame(box, 2, colReadFrame)
		for i, l := range v.menuLabels() {
			b := bs[i]
			cv.fill(b, colReadRow)
			s := min(fitScaleMax([]string{l}, b.Dx()-8, b.Dy()-4, 2), 2)
			drawCentered(cv, b, b.Min.Y+(b.Dy()-fontH*s)/2, l, s, colText)
		}
	}
}

// touch は、セル cell の点 pt をタップしたとき。
func (h *HelpUI) touch(cell image.Rectangle, pt image.Point) {
	area := cell.Inset(cellGap + textMargin)
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.changedLocked()
	switch h.mode {
	case readTOC:
		toc := tocEntries(h.doc)
		body := image.Rect(area.Min.X, area.Min.Y+fontH*2+8, area.Max.X, area.Max.Y)
		g := libGeometry(body, len(toc))
		pages := (len(toc) + g.per - 1) / g.per
		switch {
		case pt.In(g.up):
			h.tocPage = max(h.tocPage-1, 0)
		case pt.In(g.down):
			h.tocPage = min(h.tocPage+1, pages-1)
		default:
			for i, rr := range g.rows {
				k := h.tocPage*g.per + i
				if pt.In(rr) && k < len(toc) {
					h.jumpLocked(ReadPos{B: toc[k]})
				}
			}
			h.mode = readRead
		}
	case readMenu:
		bs := menuGeometry(area)
		h.mode = readRead
		for i := 0; i < 5; i++ {
			if !pt.In(bs[i]) {
				continue
			}
			switch i {
			case 0:
				h.mode, h.tocPage = readTOC, 0
			case 1:
				h.jumpLocked(ReadPos{})
			case 2:
				for b := range h.doc.Blocks {
					if h.doc.Blocks[b].T == "h" && strings.Contains(blockText(&h.doc.Blocks[b]), "困ったとき") {
						h.jumpLocked(ReadPos{B: b})
						break
					}
				}
			case 3:
				if h.setSize != nil {
					n := map[int]int{24: 32, 32: 24}[h.sizeNow()]
					go h.setSize(n) // 読み物の係のロックを取るので、別の goroutine で
				}
			}
		}
	default:
		third := area.Dx() / 3
		switch {
		case pt.X < area.Min.X+third:
			h.turnLocked(false)
		case pt.X >= area.Max.X-third:
			h.turnLocked(true)
		default:
			h.mode = readMenu
		}
	}
}

func (h *HelpUI) turnLocked(next bool) {
	if h.starts == nil {
		if next {
			if pg := layoutPage(h.doc, h.m, h.pos, nil); !pg.end {
				h.pos = pg.next
			}
		}
		return
	}
	i := pageOf(h.starts, h.pos)
	if next && i+1 < len(h.starts) {
		i++
	} else if !next && i > 0 {
		i--
	}
	h.pos = h.starts[i]
}

func (h *HelpUI) jumpLocked(p ReadPos) {
	if h.starts != nil {
		p = h.starts[pageOf(h.starts, p)]
	}
	h.pos = p
}

// key は、ヘルプのセルが画面にあるときの本体キー。使ったら true。
func (h *HelpUI) key(code evdev.EvCode, shift bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch code {
	case evdev.KEY_LEFT, evdev.KEY_PAGEUP:
		h.turnLocked(false)
	case evdev.KEY_RIGHT, evdev.KEY_PAGEDOWN:
		h.turnLocked(true)
	case evdev.KEY_SPACE:
		h.turnLocked(!shift)
	case evdev.KEY_BACKSPACE, evdev.KEY_ESC:
		if h.mode == readRead {
			h.mode = readMenu
		} else {
			h.mode = readRead
		}
		h.changedLocked()
		return true
	default:
		return false
	}
	h.mode = readRead
	h.changedLocked()
	return true
}

func compileHelp(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: help handles taps itself (left and right to turn pages, the middle for the contents); remove key and layer_*")
	}
	return &WidgetDef{Kind: widgetHelp}, nil
}

// renderHelpView は、-render-help の画面（page ページ目。1 から）を、数え終えてから作る。
func renderHelpView(area image.Rectangle, page, size int) *HelpView {
	h := NewHelpUI(func() int { return size }, nil)
	body := image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Max.Y-readFooterH)
	h.starts = paginate(h.doc, readMetricsFor(body, h.sizeNow(), false, false))
	h.mkey = fmt.Sprintf("%v\x00%d", body, h.sizeNow())
	h.m = readMetricsFor(body, h.sizeNow(), false, false)
	h.pos = h.starts[min(max(page-1, 0), len(h.starts)-1)]
	return h.Snapshot()
}
