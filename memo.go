package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lefthand/internal/ink"
)

// ---------- 手書きメモ（widget: memo） ----------
//
// スタイラスで書いた線を、点の列として保存する（/var/lib/lefthand/memo/<id>.json）。PC から外していても使える。
// セルの上の帯に道具のボタン、その下が紙。
//
//	細 中 太 消 | 戻す 全消 | ＋頁 新規 | 前 番号 次
//
// - 細、中、太：ペンの太さ（2、4、8 ドット）。消：消しゴム（紙の色で 20 ドット）
// - 戻す：今のページの最後の線を消す（全消のあとなら、消した線を戻す）。全消：長押し（0.8 秒）で今のページの線をすべて消す
// - ＋頁：今のメモにページを足す。新規：新しいメモを作る。前 次：すべてのメモのページを順にめくる（番号は「メモ-ページ」）
// - 書いている途中は、線の部分だけを描き足す（Display.Ink）。線を書き終えても、画面は描き直さない（同じ画素なので）。
//   戻す、全消、ページを替える、向きを変えたときに、紙を全体から描き直す
// - ページの大きさは、ページを作ったときの紙の範囲。別の大きさで見るとき（画面の向きを変えたなど）は、縦横比を保って
//   縮めて（広げて）中央に置き、書いた線はページの座標に戻して保存する
// - 線の描き方は internal/ink（brain-deck memo get の PNG と同じ画素）

const (
	widgetMemo = "memo"

	memoBarH       = 44
	memoMaxMemos   = 200
	memoMaxPages   = 50
	memoMaxStrokes = 3000
	memoMaxPoints  = 2000
	memoEraseW     = 20
	memoClearHold  = 800 * time.Millisecond
	memoSaveDelay  = 2 * time.Second
	memoChunkMax   = 96 << 10
)

var memoWidths = [3]int{2, 4, 8}

// memo の帯のボタン
const (
	memoBtnThin = iota
	memoBtnMid
	memoBtnThick
	memoBtnErase
	memoBtnUndo
	memoBtnClear
	memoBtnPage
	memoBtnNew
	memoBtnPrev
	memoBtnNum
	memoBtnNext
	memoBtnCount
)

var memoBtnLabels = [memoBtnCount]string{"細", "中", "太", "消", "戻す", "全消", "＋頁", "新規", "前", "", "次"}

// MemoDef は、組み立て済みの手書きメモのセル。
type MemoDef struct {
	Pen PenParams
}

func compileMemo(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: memo handles the stylus itself; remove key and layer_*")
	}
	if s.Speed != nil || s.Accel != nil || s.ScrollWidth != nil || s.ScrollDirection != "" || s.ScrollStep != nil || s.Deadzone != nil ||
		s.TapMS != nil || s.TapMove != nil || s.DragMS != nil || s.LongPress != "" || s.Map != "" || s.PCScreen != "" || s.Region != nil || s.Pressure != nil || s.PressureRange != nil {
		return nil, fmt.Errorf("widget: memo takes min_pressure, settle_ms and smooth only")
	}
	pen, err := compilePenParams(s)
	if err != nil {
		return nil, err
	}
	return &WidgetDef{Kind: widgetMemo, Memo: &MemoDef{Pen: pen}}, nil
}

// ---------- データ ----------

// Memo は 1 つのメモ。
type Memo struct {
	ID      string     `json:"id"`
	Created time.Time  `json:"created"`
	Updated time.Time  `json:"updated"`
	Pages   []ink.Page `json:"pages"`
}

// MemoInfo は list_memos の 1 つ。
type MemoInfo struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Pages   int       `json:"pages"`
	Strokes int       `json:"strokes"`
	Bytes   int64     `json:"bytes"`
}

// MemoService は、メモの置き場所と、今開いているページ、道具。タッチの goroutine、描画の goroutine、通信の goroutine から使う。
type MemoService struct {
	dir string

	mu       sync.Mutex
	memos    []*Memo // 作った順
	cur      int     // 今のメモ（memos の番号）
	page     int     // 今のページ
	tool     int     // memoBtnThin〜memoBtnErase
	drawRev  uint64  // 紙を全体から描き直すたびに増やす（widgetKey）
	cleared  []ink.Stroke
	joinable *memoStream // 離して、触れ直すのを待っている読み取り
	dirty    map[string]bool
	timer    *time.Timer
	onChange func()
	notify   func(string)
	err      error
}

// OpenMemoService は dir のメモを読む（ディレクトリは書くときに作る）。
func OpenMemoService(dir string) *MemoService {
	s := &MemoService{dir: dir, tool: memoBtnMid, dirty: map[string]bool{}}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Memo
		if err := json.Unmarshal(b, &m); err != nil || !validMemoID(m.ID) || len(m.Pages) == 0 {
			log.Printf("memo: %s: not a memo (%v); skipped", e.Name(), err)
			continue
		}
		s.memos = append(s.memos, &m)
	}
	sort.Slice(s.memos, func(i, j int) bool { return memoNum(s.memos[i].ID) < memoNum(s.memos[j].ID) })
	if n := len(s.memos); n > 0 {
		s.cur, s.page = n-1, len(s.memos[n-1].Pages)-1
	}
	return s
}

func validMemoID(id string) bool {
	n, ok := strings.CutPrefix(id, "m")
	v, err := strconv.Atoi(n)
	return ok && err == nil && v > 0 && strconv.Itoa(v) == n
}

func memoNum(id string) int {
	v, _ := strconv.Atoi(strings.TrimPrefix(id, "m"))
	return v
}

// SetOnChange は、紙や帯を描き直すときに呼ぶ関数（d.Poke）。SetNotify は、画面の下に知らせを出す関数。
func (s *MemoService) SetOnChange(f func()) { s.mu.Lock(); s.onChange = f; s.mu.Unlock() }
func (s *MemoService) SetNotify(f func(string)) {
	s.mu.Lock()
	s.notify = f
	s.mu.Unlock()
}

func (s *MemoService) changed(redraw bool) {
	if redraw {
		s.drawRev++
	}
	if f := s.onChange; f != nil {
		go f()
	}
}

func (s *MemoService) say(msg string) {
	if f := s.notify; f != nil {
		go f(msg)
	}
}

// curLocked は、今のメモとページ。メモがなければ作る（size はページの大きさ）。
func (s *MemoService) curLocked(size image.Point) (*Memo, *ink.Page) {
	if len(s.memos) == 0 {
		if !s.newMemoLocked(size) {
			return nil, nil
		}
	}
	s.cur = min(max(s.cur, 0), len(s.memos)-1)
	m := s.memos[s.cur]
	s.page = min(max(s.page, 0), len(m.Pages)-1)
	return m, &m.Pages[s.page]
}

func (s *MemoService) nextIDLocked() string {
	n := 0
	for _, m := range s.memos {
		n = max(n, memoNum(m.ID))
	}
	return "m" + strconv.Itoa(n+1)
}

// roomLocked は、SD カードの空き（背景画像と同じく 64 MiB を残す）。
func (s *MemoService) roomLocked() error {
	os.MkdirAll(s.dir, 0o755)
	free, size, err := imageFreeSpace(s.dir)
	if err != nil {
		return nil // 測れないときは断らない（書くときにエラーになる）
	}
	if free < reserveFor(size)+(1<<20) {
		return fmt.Errorf("SD カードの空きが足りません（%s）", kib(free))
	}
	return nil
}

func (s *MemoService) newMemoLocked(size image.Point) bool {
	if len(s.memos) >= memoMaxMemos {
		s.say(fmt.Sprintf("メモは %d までです。PC で brain-deck memo rm で消してください", memoMaxMemos))
		return false
	}
	if err := s.roomLocked(); err != nil {
		s.say(err.Error())
		return false
	}
	now := time.Now().UTC()
	m := &Memo{ID: s.nextIDLocked(), Created: now, Updated: now, Pages: []ink.Page{{W: size.X, H: size.Y, Strokes: []ink.Stroke{}}}}
	s.memos = append(s.memos, m)
	s.cur, s.page = len(s.memos)-1, 0
	s.cleared = nil
	s.markLocked(m)
	return true
}

// markLocked は、m を書き換えたので、少しあとに保存する。
func (s *MemoService) markLocked(m *Memo) {
	m.Updated = time.Now().UTC()
	s.dirty[m.ID] = true
	if s.timer == nil {
		s.timer = time.AfterFunc(memoSaveDelay, s.Flush)
	}
}

// Flush は、まだ書いていないメモを書く（少しあとと、デーモンが終わるとき）。
func (s *MemoService) Flush() {
	s.mu.Lock()
	s.timer = nil
	var out []struct {
		id string
		b  []byte
	}
	for id := range s.dirty {
		for _, m := range s.memos {
			if m.ID == id {
				b, _ := json.Marshal(m)
				out = append(out, struct {
					id string
					b  []byte
				}{id, b})
			}
		}
	}
	s.dirty = map[string]bool{}
	s.mu.Unlock()
	if len(out) == 0 {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		log.Printf("memo: %v", err)
		return
	}
	for _, o := range out {
		if err := writeFileAtomic(filepath.Join(s.dir, o.id+".json"), o.b, false); err != nil {
			log.Printf("memo: save %s: %v", o.id, err)
		}
	}
}

// List は list_memos の結果。
func (s *MemoService) List() []MemoInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []MemoInfo{}
	for _, m := range s.memos {
		in := MemoInfo{ID: m.ID, Created: m.Created, Updated: m.Updated, Pages: len(m.Pages)}
		for _, p := range m.Pages {
			in.Strokes += len(p.Strokes)
		}
		if fi, err := os.Stat(filepath.Join(s.dir, m.ID+".json")); err == nil {
			in.Bytes = fi.Size()
		}
		out = append(out, in)
	}
	return out
}

var errMemoNotFound = errors.New("memo not found")

// Read は、メモの JSON の offset から最大 n バイトを返す（get_memo）。書いていない変更も含める。
func (s *MemoService) Read(id string, offset int64, n int) ([]byte, int64, error) {
	s.mu.Lock()
	var b []byte
	for _, m := range s.memos {
		if m.ID == id {
			b, _ = json.Marshal(m)
		}
	}
	s.mu.Unlock()
	if b == nil {
		return nil, 0, fmt.Errorf("%w: %s", errMemoNotFound, id)
	}
	if n <= 0 || n > memoChunkMax {
		n = memoChunkMax
	}
	if offset < 0 || offset > int64(len(b)) {
		return nil, 0, fmt.Errorf("%w: offset %d is outside the memo (%d bytes)", errTransferBad, offset, len(b))
	}
	end := min(offset+int64(n), int64(len(b)))
	return b[offset:end], int64(len(b)), nil
}

// Delete は id のメモを消す。
func (s *MemoService) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.memos {
		if m.ID != id {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.memos = append(s.memos[:i], s.memos[i+1:]...)
		delete(s.dirty, id)
		if s.cur >= i {
			s.cur = max(s.cur-1, 0)
			s.page = 0
		}
		s.cleared = nil
		s.changed(true)
		log.Printf("memo: removed %s", id)
		return nil
	}
	return fmt.Errorf("%w: %s", errMemoNotFound, id)
}

// ---------- 配置 ----------

// memoGeom は、セルの中の配置。
type memoGeom struct {
	bar   image.Rectangle
	btns  [memoBtnCount]image.Rectangle
	paper image.Rectangle // 紙の範囲（セルの中）
}

func memoGeometry(area image.Rectangle) memoGeom {
	g := memoGeom{bar: image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Min.Y+min(memoBarH, area.Dy()/3))}
	g.paper = image.Rect(area.Min.X, g.bar.Max.Y+2, area.Max.X, area.Max.Y)
	for i := 0; i < memoBtnCount; i++ {
		x0, _ := cellSpan(i, memoBtnCount, g.bar.Dx())
		_, x1 := cellSpan(i, memoBtnCount, g.bar.Dx())
		g.btns[i] = image.Rect(g.bar.Min.X+x0, g.bar.Min.Y, g.bar.Min.X+x1, g.bar.Max.Y)
	}
	return g
}

// pageView は、ページを紙の範囲に置く変換（縦横比を保つ）。scale は分数 num/den。
type pageView struct {
	off      image.Point // 画面での、ページの左上
	num, den int
	area     image.Rectangle // ページが占める画面の範囲
}

func viewOf(paper image.Rectangle, p *ink.Page) pageView {
	if p.W == paper.Dx() && p.H == paper.Dy() || p.W <= 0 || p.H <= 0 {
		return pageView{off: paper.Min, num: 1, den: 1, area: image.Rect(paper.Min.X, paper.Min.Y, paper.Min.X+p.W, paper.Min.Y+p.H).Intersect(paper)}
	}
	num, den := paper.Dx(), p.W
	if paper.Dy()*p.W < paper.Dx()*p.H {
		num, den = paper.Dy(), p.H
	}
	w, h := p.W*num/den, p.H*num/den
	off := image.Pt(paper.Min.X+(paper.Dx()-w)/2, paper.Min.Y+(paper.Dy()-h)/2)
	return pageView{off: off, num: num, den: den, area: image.Rect(off.X, off.Y, off.X+w, off.Y+h)}
}

func (v pageView) toScreen(p ink.Pt) ink.Pt {
	return ink.Pt{X: v.off.X + p.X*v.num/v.den, Y: v.off.Y + p.Y*v.num/v.den}
}

func (v pageView) toPage(pt fpt) ink.Pt {
	return ink.Pt{X: int(math.Round((pt.X - float64(v.off.X)) * float64(v.den) / float64(v.num))),
		Y: int(math.Round((pt.Y - float64(v.off.Y)) * float64(v.den) / float64(v.num)))}
}

func (v pageView) width(w int) int { return max(w*v.num/v.den, 1) }

// ---------- 描画 ----------

var (
	colMemoPaper = RGB{ink.Paper[0], ink.Paper[1], ink.Paper[2]}
	colMemoInk   = RGB{ink.Ink[0], ink.Ink[1], ink.Ink[2]}
	colMemoBar   = RGB{0x24, 0x2c, 0x38}
	colMemoBtn   = RGB{0x34, 0x40, 0x50}
	colMemoOn    = RGB{0x40, 0xa0, 0xe0}
	colMemoEdge  = RGB{0x10, 0x14, 0x18}
)

// memoKey は、紙と帯の中身を表す文字列（線を書き足しても変わらない。Display.Ink で描き足すため）。
func memoKey(w *WidgetDef, env WidgetEnv) string {
	ms := env.Memo
	if ms == nil {
		return "memo\x00none"
	}
	return "memo\x00" + ms.key
}

// MemoView は、描画の goroutine に渡す、今のページの写し。
type MemoView struct {
	key   string
	label string // 帯の番号（「メモ-ページ」）
	tool  int
	page  *ink.Page // 写し（描画だけが読む）
}

// Snapshot は、描画に使う今のページの写し（なければ作る大きさは、最初に描いたときに決める）。
func (s *MemoService) Snapshot() *MemoView {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := &MemoView{tool: s.tool}
	if len(s.memos) == 0 {
		v.label = "-"
		v.key = fmt.Sprintf("empty\x00%d\x00%d", s.drawRev, s.tool)
		return v
	}
	s.cur = min(max(s.cur, 0), len(s.memos)-1)
	m := s.memos[s.cur]
	s.page = min(max(s.page, 0), len(m.Pages)-1)
	p := m.Pages[s.page]
	p.Strokes = append([]ink.Stroke(nil), p.Strokes...)
	v.page = &p
	v.label = fmt.Sprintf("%d-%d", memoNum(m.ID), s.page+1)
	v.key = fmt.Sprintf("%s\x00%d\x00%d\x00%d", m.ID, s.page, s.drawRev, s.tool)
	return v
}

// drawMemo は、手書きメモのセルを描く（帯と、紙の全体）。
func drawMemo(cv *Canvas, area image.Rectangle, env WidgetEnv) {
	g := memoGeometry(area)
	cv.fill(g.bar, colMemoBar)
	mv := env.Memo
	tool := memoBtnMid
	label := "-"
	if mv != nil {
		tool, label = mv.tool, mv.label
	}
	for i, r := range g.btns {
		b := r.Inset(2)
		txt := memoBtnLabels[i]
		bg := colMemoBtn
		if i == memoBtnNum {
			txt, bg = label, colMemoBar
		}
		if i == tool {
			bg = colMemoOn
		}
		cv.fill(b, bg)
		s := min(fitScaleMax([]string{txt}, b.Dx()-2, b.Dy()-2, 2), 2)
		drawCentered(cv, b, b.Min.Y+(b.Dy()-fontH*s)/2, txt, s, colText)
	}
	cv.fill(image.Rect(area.Min.X, g.bar.Max.Y, area.Max.X, g.paper.Min.Y), colMemoEdge)
	cv.fill(g.paper, colMemoEdge)
	if mv == nil || mv.page == nil {
		cv.fill(g.paper, colMemoPaper)
		return
	}
	v := viewOf(g.paper, mv.page)
	cv.fill(v.area, colMemoPaper)
	for i := range mv.page.Strokes {
		drawStrokeOn(cv, v, &mv.page.Strokes[i])
	}
}

// drawStrokeOn は、線をページの変換 v で描く。
func drawStrokeOn(cv *Canvas, v pageView, s *ink.Stroke) image.Rectangle {
	c := colMemoInk
	if s.Erase {
		c = colMemoPaper
	}
	fill := func(x0, x1, y int) { cv.fill(image.Rect(x0, y, x1, y+1), c) }
	if v.num == v.den {
		sh := ink.Stroke{W: s.W, Erase: s.Erase, P: make([]int, len(s.P))}
		for i := 0; i+1 < len(s.P); i += 2 {
			sh.P[i], sh.P[i+1] = s.P[i]+v.off.X, s.P[i+1]+v.off.Y
		}
		ink.Draw(fill, v.area, &sh)
		return sh.Bounds().Intersect(v.area)
	}
	sh := ink.Stroke{W: v.width(s.W), Erase: s.Erase}
	for _, p := range s.Points() {
		sh.Add(v.toScreen(p))
	}
	ink.Draw(fill, v.area, &sh)
	return sh.Bounds().Intersect(v.area)
}

// ---------- 操作 ----------

// memoStream は、手書きメモのセルに触れているあいだの読み取り。一瞬離れて触れ直したときは、同じものを続けて使う
// （join。stroke.go）。待ちを終えるタイマーからも使うので、mu で守る。
type memoStream struct {
	mu    sync.Mutex
	timer *time.Timer
	s     *MemoService
	g     memoGeom
	f     *StrokeFilter
	ink   func(func(*Canvas) image.Rectangle) // 線を描き足す（Display.Ink）
	view  pageView
	cur   *ink.Stroke
	last  ink.Pt
	btn   int // 押した帯のボタン（-1 は紙）
	tDown time.Time
	held  *time.Timer
}

// memoTouch は、手書きメモのセルに触れたときに、読み取りを始める。
func (s *MemoService) memoTouch(d *MemoDef, cell image.Rectangle, tm touchMap, pt image.Point, t time.Time, inkFn func(func(*Canvas) image.Rectangle)) *memoStream {
	g := memoGeometry(cell.Inset(cellGap + textMargin))
	ms := &memoStream{s: s, g: g, ink: inkFn, btn: -1, tDown: t}
	if pt.In(g.bar) {
		for i, r := range g.btns {
			if pt.In(r) {
				ms.btn = i
			}
		}
		ms.pressButton()
		return ms
	}
	s.mu.Lock()
	prev := s.joinable
	s.joinable = nil
	s.mu.Unlock()
	if prev != nil && prev.g == g {
		prev.mu.Lock()
		if prev.timer != nil {
			prev.timer.Stop()
		}
		if !prev.f.Due().IsZero() && !t.After(prev.f.Due()) {
			prev.f.Down(t) // 一瞬離れたあとの続きかもしれない（最初に使うサンプルの位置で決める）
			prev.mu.Unlock()
			return prev
		}
		prev.f.Flush()
		prev.mu.Unlock()
	}
	ms.f = NewStrokeFilter(d.Pen, tm, ms)
	ms.f.Down(t)
	return ms
}

func (ms *memoStream) Sample(x, y, p int32, t time.Time) {
	if ms.f != nil {
		ms.mu.Lock()
		ms.f.Sample(x, y, p, t)
		ms.mu.Unlock()
	}
}

func (ms *memoStream) Up(t time.Time) {
	if ms.f != nil {
		ms.mu.Lock()
		defer ms.mu.Unlock()
		ms.f.Up(t)
		if d := ms.f.Due(); !d.IsZero() {
			// 触れ直すのを join のあいだ待つ。待ちが終わったら線を終える
			ms.s.mu.Lock()
			ms.s.joinable = ms
			ms.s.mu.Unlock()
			ms.timer = time.AfterFunc(time.Until(d)+time.Millisecond, func() {
				ms.mu.Lock()
				ms.f.Flush()
				ms.mu.Unlock()
			})
		}
		return
	}
	if ms.held != nil && ms.held.Stop() && ms.btn == memoBtnClear {
		ms.s.say("全消は長押し（0.8 秒）で、今のページの線をすべて消します")
	}
}

// pressButton は、帯のボタンを押したとき（全消は長押しで）。
func (ms *memoStream) pressButton() {
	s := ms.s
	size := ms.g.paper.Size()
	if ms.btn == memoBtnClear {
		ms.held = time.AfterFunc(memoClearHold, func() { s.clearPage() })
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ms.btn {
	case memoBtnThin, memoBtnMid, memoBtnThick, memoBtnErase:
		s.tool = ms.btn
		s.changed(false)
	case memoBtnUndo:
		m, p := s.curLocked(size)
		if m == nil {
			return
		}
		switch {
		case len(p.Strokes) > 0:
			p.Strokes = p.Strokes[:len(p.Strokes)-1]
		case s.cleared != nil:
			p.Strokes, s.cleared = s.cleared, nil
		default:
			return
		}
		s.markLocked(m)
		s.changed(true)
	case memoBtnPage:
		m, _ := s.curLocked(size)
		if m == nil {
			return
		}
		if len(m.Pages) >= memoMaxPages {
			s.say(fmt.Sprintf("1 つのメモのページは %d までです。「新規」で新しいメモにしてください", memoMaxPages))
			return
		}
		if err := s.roomLocked(); err != nil {
			s.say(err.Error())
			return
		}
		m.Pages = append(m.Pages, ink.Page{W: size.X, H: size.Y, Strokes: []ink.Stroke{}})
		s.page = len(m.Pages) - 1
		s.cleared = nil
		s.markLocked(m)
		s.changed(true)
	case memoBtnNew:
		if s.newMemoLocked(size) {
			s.changed(true)
		}
	case memoBtnPrev, memoBtnNext:
		if len(s.memos) == 0 {
			return
		}
		d := 1
		if ms.btn == memoBtnPrev {
			d = -1
		}
		s.curLocked(size)
		np := s.page + d
		switch {
		case np < 0 && s.cur > 0:
			s.cur--
			s.page = len(s.memos[s.cur].Pages) - 1
		case np >= len(s.memos[s.cur].Pages) && s.cur < len(s.memos)-1:
			s.cur++
			s.page = 0
		case np >= 0 && np < len(s.memos[s.cur].Pages):
			s.page = np
		default:
			return
		}
		s.cleared = nil
		s.changed(true)
	}
}

// clearPage は、今のページの線をすべて消す（戻すで戻せる）。
func (s *MemoService) clearPage() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.memos) == 0 {
		return
	}
	m := s.memos[s.cur]
	p := &m.Pages[s.page]
	if len(p.Strokes) == 0 {
		return
	}
	s.cleared, p.Strokes = p.Strokes, []ink.Stroke{}
	s.markLocked(m)
	s.changed(true)
	s.say("このページの線をすべて消しました（「戻す」で戻せます）")
}

// StrokeDown など：読み取った線を、ページに足しながら描き足す。
func (ms *memoStream) StrokeDown(pt fpt, _ int32) {
	s := ms.s
	s.mu.Lock()
	m, p := s.curLocked(ms.g.paper.Size())
	if m == nil {
		s.mu.Unlock()
		return
	}
	if len(p.Strokes) >= memoMaxStrokes {
		s.mu.Unlock()
		s.say(fmt.Sprintf("1 ページの線は %d 本までです。「＋頁」でページを足してください", memoMaxStrokes))
		return
	}
	ms.view = viewOf(ms.g.paper, p)
	w, erase := memoWidths[1], false
	switch s.tool {
	case memoBtnThin, memoBtnMid, memoBtnThick:
		w = memoWidths[s.tool]
	case memoBtnErase:
		w, erase = memoEraseW, true
	}
	s.cleared = nil
	s.mu.Unlock()
	pt = pt.inCell(ms.view.area)
	q := ms.view.toPage(pt)
	ms.cur = &ink.Stroke{W: w, Erase: erase}
	ms.cur.Add(q)
	ms.last = q
	st := *ms.cur
	v := ms.view
	ms.ink(func(cv *Canvas) image.Rectangle { return drawStrokeOn(cv, v, &st) })
}

func (ms *memoStream) StrokeMove(pt fpt, _ int32) {
	if ms.cur == nil {
		return
	}
	q := ms.view.toPage(pt.inCell(ms.view.area))
	if q == ms.last {
		return
	}
	if len(ms.cur.P)/2 >= memoMaxPoints {
		// 長すぎる線は、ここで切って続きを新しい線にする
		ms.StrokeUp()
		ms.StrokeDown(pt, 0)
		return
	}
	a := ms.last
	ms.cur.Add(q)
	ms.last = q
	seg := ink.Stroke{W: ms.cur.W, Erase: ms.cur.Erase, P: []int{a.X, a.Y, q.X, q.Y}}
	v := ms.view
	ms.ink(func(cv *Canvas) image.Rectangle { return drawSegmentOn(cv, v, &seg) })
}

// drawSegmentOn は、2 点の線のうち、始まりのペン先を除いた部分を描く（描き足し）。
func drawSegmentOn(cv *Canvas, v pageView, s *ink.Stroke) image.Rectangle {
	c := colMemoInk
	if s.Erase {
		c = colMemoPaper
	}
	fill := func(x0, x1, y int) { cv.fill(image.Rect(x0, y, x1, y+1), c) }
	a, b := v.toScreen(ink.Pt{X: s.P[0], Y: s.P[1]}), v.toScreen(ink.Pt{X: s.P[2], Y: s.P[3]})
	w := s.W
	if v.num != v.den {
		w = v.width(s.W)
	}
	ink.Segment(fill, v.area, a, b, w)
	r := (&ink.Stroke{W: w, P: []int{a.X, a.Y, b.X, b.Y}}).Bounds()
	return r.Intersect(v.area)
}

func (ms *memoStream) StrokeUp() {
	if ms.cur == nil {
		return
	}
	st := *ms.cur
	ms.cur = nil
	s := ms.s
	s.mu.Lock()
	defer s.mu.Unlock()
	m, p := s.curLocked(ms.g.paper.Size())
	if m == nil {
		return
	}
	p.Strokes = append(p.Strokes, st)
	s.markLocked(m)
	s.changed(false) // 描き足してあるので、紙は描き直さない
}
