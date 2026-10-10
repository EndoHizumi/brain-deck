package main

import (
	"fmt"
	"image"
	"log"
	"strings"
	"sync"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 読み物のアプリ（widget: reader） ----------
//
// 一覧：Brain にある読み物（新しいものから）。行をタップすると開く。▲▼ でページを送る。
// 読む：画面の左の 3 分の 1 をタップすると次のページ（縦書き。横書きでは前のページ）、右の 3 分の 1 で反対、
// 真ん中で道具（一覧、目次、文字の大きさ、縦書き・横書き、閉じる）。本体キーの ← → とスペース（Shift で戻る）でもめくる。
// 下の帯に題名と「ページ / ページの数」。ページの数は、開いたとき（文字の大きさや向きを変えたとき）に別の goroutine で数える。
// しおり：開いているページの始まりの位置を覚える（reader.json。5 秒後とデーモンの終わりに書く）。
// PC から外していても読める。

const (
	widgetReader = "reader"

	readFooterH = 18
	readRowH    = 52
	readNavH    = 40
)

// 画面の状態
const (
	readLib = iota
	readRead
	readMenu
	readTOC
)

// ReaderService は、読み物のアプリの状態（1 つのデーモンに 1 つ。セルが 2 つあっても同じものを出す）。
type ReaderService struct {
	store *ReadStore

	mu       sync.Mutex
	mode     int
	libPage  int
	tocPage  int
	docID    string
	doc      *ReadDoc
	opening  bool
	size     int
	vertical bool
	mkey     string      // 数えたページの寸法
	m        readMetrics // 今の寸法
	starts   []ReadPos   // ページの始まり（数え終えたら）
	counting bool
	pos      ReadPos // 今のページの始まり
	gen      uint64
	onChange func()
	notify   func(string)
	prefix   []int // ブロックごとの、それより前の文字の数（しおりの進み具合）
}

// NewReaderService は、store の読み物を読むアプリを作る。開いていた読み物があれば開く。
func NewReaderService(store *ReadStore) *ReaderService {
	r := &ReaderService{store: store}
	r.size, r.vertical, r.docID = store.Settings()
	if r.docID != "" {
		r.mode = readRead
		r.pos = store.Bookmark(r.docID)
		go r.load(r.docID)
	}
	return r
}

func (r *ReaderService) SetOnChange(f func())     { r.mu.Lock(); r.onChange = f; r.mu.Unlock() }
func (r *ReaderService) SetNotify(f func(string)) { r.mu.Lock(); r.notify = f; r.mu.Unlock() }

func (r *ReaderService) changedLocked() {
	if f := r.onChange; f != nil {
		go f()
	}
}

func (r *ReaderService) sayLocked(msg string) {
	if f := r.notify; f != nil {
		go f(msg)
	}
}

// load は、読み物を戻す（別の goroutine）。
func (r *ReaderService) load(id string) {
	t := time.Now()
	d, err := r.store.Load(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opening = false
	if r.docID != id {
		return
	}
	if err != nil {
		log.Printf("reader: open %s: %v", id, err)
		r.sayLocked("読み物を開けませんでした：" + err.Error())
		r.docID, r.doc, r.mode = "", nil, readLib
		r.changedLocked()
		return
	}
	r.doc = d
	r.mkey = ""
	r.prefix = make([]int, len(d.Blocks)+1)
	for i := range d.Blocks {
		n := 0
		for _, run := range d.Blocks[i].R {
			n += len([]rune(run.S))
		}
		r.prefix[i+1] = r.prefix[i] + n
	}
	vlogf("reader: opened %s %q (%d blocks) in %v", id, d.Title, len(d.Blocks), time.Since(t))
	r.changedLocked()
}

// open は、読み物を開く（しおりの位置から）。
func (r *ReaderService) openLocked(id string) {
	r.docID, r.doc, r.opening = id, nil, true
	r.pos = r.store.Bookmark(id)
	r.starts, r.mkey = nil, ""
	r.mode = readRead
	r.store.SetSettings(r.size, r.vertical, id)
	go r.load(id)
}

// metricsLocked は、セルの範囲 area の、本文の寸法を決め、変わっていればページを数え直す。
func (r *ReaderService) metricsLocked(area image.Rectangle) readMetrics {
	body := image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Max.Y-readFooterH)
	art := r.doc != nil && r.doc.Flow == "article"
	m := readMetricsFor(body, r.size, r.vertical, art)
	key := fmt.Sprintf("%s\x00%v\x00%d\x00%v", r.docID, body, r.size, r.vertical)
	r.m = m
	if key != r.mkey && r.doc != nil {
		r.mkey = key
		r.starts = nil
		r.counting = true
		r.gen++
		gen, d := r.gen, r.doc
		go func() {
			t := time.Now()
			starts := paginate(d, m)
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.gen != gen {
				return
			}
			r.starts, r.counting = starts, false
			// しおりの位置を含むページを開く（数え終えたページの始まりにそろえる）
			r.pos = starts[pageOf(starts, r.pos)]
			log.Printf("reader: %q: %d pages (size %d, vertical %v) in %v", d.Title, len(starts), m.S, m.vertical, time.Since(t).Round(time.Millisecond))
			r.changedLocked()
		}()
	}
	return m
}

// ReaderView は、描くときの写し。
type ReaderView struct {
	mode        int
	list        []ReadInfo
	libPage     int
	tocPage     int
	doc         *ReadDoc
	title       string
	opening     bool
	pos         ReadPos
	page, pages int // 1 から。pages が 0 なら数えている
	size        int
	vertical    bool
	svc         *ReaderService
}

// Snapshot は、描画の goroutine が使う写し。
func (r *ReaderService) Snapshot() *ReaderView {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := &ReaderView{mode: r.mode, libPage: r.libPage, tocPage: r.tocPage, doc: r.doc, opening: r.opening, pos: r.pos, size: r.size, vertical: r.vertical, svc: r}
	if r.mode == readLib {
		v.list = r.store.List()
	}
	if m, ok := r.store.meta(r.docID); ok {
		v.title = m.Title
	}
	if r.starts != nil {
		v.page, v.pages = pageOf(r.starts, r.pos)+1, len(r.starts)
	}
	return v
}

func readerKey(env WidgetEnv) string {
	v := env.Reader
	if v == nil {
		return "reader\x00none"
	}
	k := fmt.Sprintf("reader\x00%d\x00%d\x00%d\x00%v\x00%d\x00%d\x00%d\x00%v\x00%v\x00%s", v.mode, v.libPage, v.tocPage, v.pos, v.page, v.pages, v.size, v.vertical, v.opening, v.title)
	if v.mode == readLib {
		for _, d := range v.list {
			k += "\x00" + d.ID
			if d.Pos != nil {
				k += fmt.Sprint(*d.Pos)
			}
		}
	}
	return k
}

// ---------- 描く ----------

var (
	colReadBar   = RGB{0x24, 0x2c, 0x38}
	colReadRow   = RGB{0x1c, 0x28, 0x38}
	colReadSub   = RGB{0x96, 0xa4, 0xb4}
	colReadMenu  = RGB{0x2a, 0x34, 0x44}
	colReadFrame = RGB{0x8c, 0xa0, 0xbc}
)

// libGeom は、一覧の配置。
type libGeom struct {
	rows     []image.Rectangle
	per      int
	nav      image.Rectangle
	up, down image.Rectangle
}

func libGeometry(area image.Rectangle, n int) libGeom {
	g := libGeom{per: max(area.Dy()/readRowH, 1)}
	if n > g.per {
		g.per = max((area.Dy()-readNavH)/readRowH, 1)
		g.nav = image.Rect(area.Min.X, area.Max.Y-readNavH, area.Max.X, area.Max.Y)
		g.up = image.Rect(g.nav.Min.X, g.nav.Min.Y, g.nav.Min.X+g.nav.Dx()/3, g.nav.Max.Y)
		g.down = image.Rect(g.nav.Max.X-g.nav.Dx()/3, g.nav.Min.Y, g.nav.Max.X, g.nav.Max.Y)
	}
	for i := 0; i < g.per; i++ {
		g.rows = append(g.rows, image.Rect(area.Min.X, area.Min.Y+i*readRowH, area.Max.X, area.Min.Y+(i+1)*readRowH))
	}
	return g
}

// menuGeom は、読んでいるときの道具の配置（5 つのボタン）。
func menuGeometry(area image.Rectangle) []image.Rectangle {
	w := min(area.Dx()-40, 360)
	h := 5*44 + 6*8
	box := image.Rect(area.Min.X+(area.Dx()-w)/2, area.Min.Y+(area.Dy()-h)/2, 0, 0)
	box.Max = box.Min.Add(image.Pt(w, h))
	var out []image.Rectangle
	for i := 0; i < 5; i++ {
		y := box.Min.Y + 8 + i*52
		out = append(out, image.Rect(box.Min.X+8, y, box.Max.X-8, y+44))
	}
	return append(out, box)
}

func (v *ReaderView) menuLabels() []string {
	dir := "横書きにする"
	if !v.vertical {
		dir = "縦書きにする"
	}
	sz := "文字を大きく（32）"
	if v.size == 32 {
		sz = "文字を小さく（24）"
	}
	return []string{"一覧に戻る", "目次", sz, dir, "読みつづける"}
}

// tocEntries は、目次（見出しのブロック）。
func tocEntries(d *ReadDoc) []int {
	var out []int
	for i, b := range d.Blocks {
		if b.T == "h" {
			out = append(out, i)
		}
	}
	return out
}

func blockText(b *RBlock) string {
	var s strings.Builder
	for _, r := range b.R {
		s.WriteString(r.S)
	}
	return s.String()
}

// clipText は、幅 w（ドット、倍率 sc）に入るように切り、入らなければ … を付ける。
func clipText(s string, w, sc int) string {
	if font.textWidth(s)*sc <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && font.textWidth(string(r)+textEllipsis)*sc > w {
		r = r[:len(r)-1]
	}
	return string(r) + textEllipsis
}

// drawReader は、読み物のアプリのセルを描く。
func drawReader(cv *Canvas, area image.Rectangle, env WidgetEnv) {
	v := env.Reader
	if v == nil || v.mode == readLib {
		drawReadLib(cv, area, v)
		return
	}
	if v.mode == readTOC && v.doc != nil {
		drawReadTOC(cv, area, v)
		return
	}
	cv.fill(area, colReadPaper)
	if v.doc == nil {
		drawCentered(cv, area, area.Min.Y+area.Dy()/2-fontH, "開いています…", 2, colReadInfo)
		return
	}
	v.svc.mu.Lock()
	m := v.svc.metricsLocked(area)
	pos := v.svc.pos
	v.svc.mu.Unlock()
	pg := layoutPage(v.doc, m, pos, nil)
	drawReadPage(cv, v.doc, m, pg)
	// 下の帯：題名と、ページ
	foot := image.Rect(area.Min.X, area.Max.Y-readFooterH, area.Max.X, area.Max.Y)
	cv.fill(foot, colReadBar)
	num := "ページを数えています"
	if v.pages > 0 {
		num = fmt.Sprintf("%d / %d", v.page, v.pages)
	}
	nw := font.textWidth(num)
	cv.text(foot.Max.X-nw-6, foot.Min.Y+3, num, 1, colReadSub, foot)
	cv.text(foot.Min.X+6, foot.Min.Y+3, clipText(v.title, foot.Dx()-nw-24, 1), 1, colText, foot)
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

func drawReadLib(cv *Canvas, area image.Rectangle, v *ReaderView) {
	cv.fill(area, colReadBar)
	var list []ReadInfo
	page := 0
	if v != nil {
		list, page = v.list, v.libPage
	}
	if len(list) == 0 {
		lines := []string{"読み物がありません", "PC で brain-deck read add <ファイル | URL>"}
		y := area.Min.Y + area.Dy()/2 - fontH*2
		for i, l := range lines {
			s := min(fitScaleMax([]string{l}, area.Dx()-8, fontH*2, 2), 2)
			drawCentered(cv, area, y+i*fontH*2+4*i, l, s, colReadSub)
		}
		return
	}
	g := libGeometry(area, len(list))
	pages := (len(list) + g.per - 1) / g.per
	page = min(max(page, 0), pages-1)
	for i, r := range g.rows {
		k := page*g.per + i
		if k >= len(list) {
			break
		}
		d := list[k]
		b := r.Inset(3)
		cv.fill(b, colReadRow)
		cv.text(b.Min.X+8, b.Min.Y+4, clipText(d.Title, b.Dx()-16, 2), 2, colText, b)
		sub := d.Author
		if d.Date != "" {
			sub = strings.TrimSpace(sub + " " + d.Date)
		}
		if d.Pos != nil && d.Chars > 0 {
			sub += fmt.Sprintf("　しおり %d%%", min(int(d.Progress*100+0.5), 100))
		}
		cv.text(b.Min.X+8, b.Min.Y+4+fontH*2+2, clipText(sub, b.Dx()-16, 1), 1, colReadSub, b)
	}
	if !g.nav.Empty() {
		cv.fill(g.up.Inset(3), colReadRow)
		cv.fill(g.down.Inset(3), colReadRow)
		drawCentered(cv, g.up, g.up.Min.Y+(g.up.Dy()-fontH*2)/2, "▲", 2, colText)
		drawCentered(cv, g.down, g.down.Min.Y+(g.down.Dy()-fontH*2)/2, "▼", 2, colText)
		drawCentered(cv, g.nav, g.nav.Min.Y+(g.nav.Dy()-fontH*2)/2, fmt.Sprintf("%d/%d", page+1, pages), 2, colReadSub)
	}
}

func drawReadTOC(cv *Canvas, area image.Rectangle, v *ReaderView) {
	cv.fill(area, colReadBar)
	toc := tocEntries(v.doc)
	head := image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Min.Y+fontH*2+8)
	drawCentered(cv, head, head.Min.Y+4, "目次（タップで開く）", 2, colReadSub)
	body := image.Rect(area.Min.X, head.Max.Y, area.Max.X, area.Max.Y)
	if len(toc) == 0 {
		drawCentered(cv, body, body.Min.Y+20, "見出しがありません（タップで戻る）", 2, colReadSub)
		return
	}
	g := libGeometry(body, len(toc))
	pages := (len(toc) + g.per - 1) / g.per
	page := min(max(v.tocPage, 0), pages-1)
	for i, r := range g.rows {
		k := page*g.per + i
		if k >= len(toc) {
			break
		}
		b := r.Inset(3)
		cv.fill(b, colReadRow)
		cv.text(b.Min.X+8, b.Min.Y+(b.Dy()-fontH*2)/2, clipText(blockText(&v.doc.Blocks[toc[k]]), b.Dx()-16, 2), 2, colText, b)
	}
	if !g.nav.Empty() {
		cv.fill(g.up.Inset(3), colReadRow)
		cv.fill(g.down.Inset(3), colReadRow)
		drawCentered(cv, g.up, g.up.Min.Y+(g.up.Dy()-fontH*2)/2, "▲", 2, colText)
		drawCentered(cv, g.down, g.down.Min.Y+(g.down.Dy()-fontH*2)/2, "▼", 2, colText)
		drawCentered(cv, g.nav, g.nav.Min.Y+(g.nav.Dy()-fontH*2)/2, fmt.Sprintf("%d/%d", page+1, pages), 2, colReadSub)
	}
}

// ---------- 操作 ----------

// readerTouch は、セル cell の点 pt をタップしたとき。
func (r *ReaderService) touch(cell image.Rectangle, pt image.Point) {
	area := cell.Inset(cellGap + textMargin)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.mode {
	case readLib:
		list := r.store.List()
		if len(list) == 0 {
			return
		}
		g := libGeometry(area, len(list))
		pages := (len(list) + g.per - 1) / g.per
		switch {
		case pt.In(g.up):
			r.libPage = max(r.libPage-1, 0)
		case pt.In(g.down):
			r.libPage = min(r.libPage+1, pages-1)
		default:
			for i, rr := range g.rows {
				k := r.libPage*g.per + i
				if pt.In(rr) && k < len(list) {
					r.openLocked(list[k].ID)
				}
			}
		}
	case readTOC:
		if r.doc == nil {
			return
		}
		toc := tocEntries(r.doc)
		body := image.Rect(area.Min.X, area.Min.Y+fontH*2+8, area.Max.X, area.Max.Y)
		if len(toc) == 0 {
			r.mode = readRead
			break
		}
		g := libGeometry(body, len(toc))
		pages := (len(toc) + g.per - 1) / g.per
		switch {
		case pt.In(g.up):
			r.tocPage = max(r.tocPage-1, 0)
		case pt.In(g.down):
			r.tocPage = min(r.tocPage+1, pages-1)
		default:
			for i, rr := range g.rows {
				k := r.tocPage*g.per + i
				if pt.In(rr) && k < len(toc) {
					r.jumpLocked(ReadPos{B: toc[k]})
					r.mode = readRead
				}
			}
		}
	case readMenu:
		bs := menuGeometry(area)
		for i := 0; i < 5; i++ {
			if !pt.In(bs[i]) {
				continue
			}
			switch i {
			case 0:
				r.mode = readLib
				r.store.SetSettings(r.size, r.vertical, "")
			case 1:
				r.mode, r.tocPage = readTOC, 0
			case 2:
				r.size = map[int]int{24: 32, 32: 24}[r.size]
				r.mode = readRead
				r.store.SetSettings(r.size, r.vertical, r.docID)
			case 3:
				r.vertical = !r.vertical
				r.mode = readRead
				r.store.SetSettings(r.size, r.vertical, r.docID)
			case 4:
				r.mode = readRead
			}
			r.changedLocked()
			return
		}
		r.mode = readRead // 道具の外をタップ
	case readRead:
		third := area.Dx() / 3
		switch {
		case pt.X < area.Min.X+third:
			r.turnLocked(r.vertical) // 縦書きでは左が次
		case pt.X >= area.Max.X-third:
			r.turnLocked(!r.vertical)
		default:
			r.mode = readMenu
		}
	}
	r.changedLocked()
}

// turnLocked は、ページをめくる（next なら次）。
func (r *ReaderService) turnLocked(next bool) {
	if r.doc == nil {
		return
	}
	if r.starts == nil {
		// 数えているあいだ：次は組んで進める。前は数え終えるまで待つ
		if next {
			pg := layoutPage(r.doc, r.m, r.pos, nil)
			if !pg.end {
				r.pos = pg.next
			}
		}
	} else {
		i := pageOf(r.starts, r.pos)
		if next && i+1 < len(r.starts) {
			i++
		} else if !next && i > 0 {
			i--
		}
		r.pos = r.starts[i]
	}
	r.bookmarkLocked()
}

// bookmarkLocked は、今のページの始まりをしおりにする。
func (r *ReaderService) bookmarkLocked() {
	prog := 0.0
	if n := len(r.prefix); n > 0 && r.prefix[n-1] > 0 && r.pos.B < n {
		prog = float64(r.prefix[r.pos.B]+r.pos.C) / float64(r.prefix[n-1])
	}
	r.store.SetBookmark(r.docID, r.pos, prog)
}

func (r *ReaderService) jumpLocked(p ReadPos) {
	if r.starts != nil {
		p = r.starts[pageOf(r.starts, p)]
	}
	r.pos = p
	r.bookmarkLocked()
}

// key は、読み物のアプリのセルが画面にあるときの本体キー。使ったら true。
func (r *ReaderService) key(code evdev.EvCode, shift bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode != readRead && r.mode != readMenu {
		return false
	}
	switch code {
	case evdev.KEY_LEFT:
		r.turnLocked(r.vertical)
	case evdev.KEY_RIGHT:
		r.turnLocked(!r.vertical)
	case evdev.KEY_SPACE, evdev.KEY_PAGEDOWN:
		r.turnLocked(!shift)
	case evdev.KEY_PAGEUP:
		r.turnLocked(false)
	case evdev.KEY_BACKSPACE, evdev.KEY_ESC:
		if r.mode == readMenu {
			r.mode = readRead
		} else {
			r.mode = readMenu
		}
	default:
		return false
	}
	if r.mode == readMenu && code != evdev.KEY_BACKSPACE && code != evdev.KEY_ESC {
		r.mode = readRead
	}
	r.changedLocked()
	return true
}

// Closed は、読み物 id を消したとき（開いていれば一覧に戻る）。
func (r *ReaderService) Closed(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.docID == id {
		r.docID, r.doc, r.mode = "", nil, readLib
	}
	r.changedLocked()
}

// Added は、読み物を足したとき（一覧を描き直す）。
func (r *ReaderService) Added() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changedLocked()
}

// compileReader は、読み物のアプリのセルの書き方を検証する。
func compileReader(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: reader handles taps itself (left and right to turn pages, the middle for the menu); remove key and layer_*")
	}
	return &WidgetDef{Kind: widgetReader}, nil
}
