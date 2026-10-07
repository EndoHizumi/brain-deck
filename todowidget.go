package main

import (
	"fmt"
	"image"
	"log"
	"sync"
	"time"
)

// ---------- Todo のウィジェット（widget: todo） ----------
//
// 項目を 1 行ずつ、未完了のあとに完了（薄く、取り消し線）の順に並べる。入りきらなければページに分け、
// 下の帯の ▲ ▼ をタップしてページを送る（本番の設定では、右の帯の ▲▼ をレイヤーの切り替えに使っているため）。
// 項目を 0.5 秒押し続けると、完了を切り替える。押しているあいだは、その行を黄色で塗る。
// 配置は todoGeometry で決め、描くときとタップしたときで同じものを使う。
// gui/src/todowidget.ts の配置、描き方と同じ結果にする。

const (
	todoRowH         = 52 // rows を書かないときの、1 行の高さ（指で押せる大きさ）
	todoNavH         = 44 // ページ送りの帯の高さ
	todoPad          = 6  // 行の左右の余白、チェックの箱と文字の間
	todoMaxTextScale = 3
	todoMaxRows      = 20
	todoEmptyText    = "Todo はありません"
)

// todoHold は、完了を切り替えるまで押し続ける時間（テストで短くするので変数）。
var todoHold = 500 * time.Millisecond

var (
	colTodoDone = RGB{0x6a, 0x74, 0x80} // 完了した項目（textColors の normal の期限切れと同じ）
	colTodoRule = RGB{0x30, 0x3c, 0x4c} // 行の区切り
	colTodoOff  = RGB{0x40, 0x48, 0x54} // これ以上送れないときの ▲ ▼
)

// pagerState は、ページを送るウィジェット（Todo、カレンダー）のセル 1 つの、画面での状態（ページ、押している行）。
// 入力の goroutine（押した、離した）と、長押しのタイマーと、描画の goroutine から触るので、ロックで守る。
type pagerState struct {
	mu      sync.Mutex
	page    int       // 最後に送ったページ（touched から PageReset のあいだだけ使う）
	touched time.Time // 最後に触った時刻。ゼロなら一度も触っていない（既定のページを出す）
	held    string    // 長押ししている項目の ID
	nav     int       // 押している ▲（-1）か ▼（+1）
	token   uint64    // 押すたび、離すたびに増やす。古い長押しのタイマーを無効にする
	timer   *time.Timer
}

// pageLocked は、now の時点で出すページ。触ってから reset が過ぎていれば（一度も触っていなければ）、既定のページ def。
// reset が 0 なら、触ったあとは戻らない。s.mu を持って呼ぶ。
func (s *pagerState) pageLocked(now time.Time, reset time.Duration, def int) int {
	if s.touched.IsZero() || (reset > 0 && !now.Before(s.touched.Add(reset))) {
		return def
	}
	return s.page
}

// view は、now の時点のページ（既定は def）、長押ししている項目、押している矢印を返す。
func (s *pagerState) view(now time.Time, reset time.Duration, def int) (page int, held string, nav int) {
	if s == nil {
		return def, "", 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pageLocked(now, reset, def), s.held, s.nav
}

// resetAt は、最初のページに戻る時刻（描き直す時刻）。戻らないならゼロ。
func (s *pagerState) resetAt(reset time.Duration, now time.Time) time.Time {
	if s == nil || reset <= 0 {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.touched.Add(reset); !s.touched.IsZero() && now.Before(t) {
		return t
	}
	return time.Time{}
}

// setPage は、ページを触ったことにして決める（-render-*-page で使う）。
func (s *pagerState) setPage(page int, now time.Time) {
	s.mu.Lock()
	s.page, s.touched = page, now
	s.mu.Unlock()
}

// WidgetRT は、ウィジェットがタップを処理するのに使う、外の状態。
type WidgetRT struct {
	mu    sync.Mutex
	w, h  int // 画面の大きさ（論理）。画面を使わないときは 800x480
	todos *TodoService
	envFn func() WidgetEnv // 今の時刻とデータ。nil なら Todo だけ
	poke  func()           // 画面の描き直し。待たずに返ること
}

func NewWidgetRT(todos *TodoService) *WidgetRT { return &WidgetRT{w: 800, h: 480, todos: todos} }

// SetEnv は、押した位置を解くときに使う、今の時刻とデータを返す関数を設定する（描画と同じもの）。
func (rt *WidgetRT) SetEnv(f func() WidgetEnv) {
	rt.mu.Lock()
	rt.envFn = f
	rt.mu.Unlock()
}

func (rt *WidgetRT) env() WidgetEnv {
	rt.mu.Lock()
	f := rt.envFn
	rt.mu.Unlock()
	if f != nil {
		return f()
	}
	return WidgetEnv{Now: time.Now(), TimeSynced: true, Todo: rt.todos.Snapshot()}
}

// SetScreen は、画面の大きさと描き直しの関数を設定する（画面を開いたあと）。
func (rt *WidgetRT) SetScreen(w, h int, poke func()) {
	rt.mu.Lock()
	rt.w, rt.h, rt.poke = w, h, poke
	rt.mu.Unlock()
}

func (rt *WidgetRT) screen() (int, int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.w, rt.h
}

func (rt *WidgetRT) redraw() {
	rt.mu.Lock()
	f := rt.poke
	rt.mu.Unlock()
	if f != nil {
		f()
	}
}

// widgetTouch は、ウィジェットのセルを押した位置。
type widgetTouch struct {
	cell image.Rectangle // セルの画面上の範囲（span を含む）
	pt   image.Point     // 押した点（画面のドット）
}

// ownsTouch は、ウィジェットがタップを自分で処理し、押した位置で働くか（セル全体を光らせない）。
func (w *WidgetDef) ownsTouch() bool {
	return w != nil && (w.Kind == widgetTodo || w.Kind == widgetCal || w.Kind == widgetPad)
}

// todoGeom は、Todo のセルの中の配置。
type todoGeom struct {
	rows          []image.Rectangle // 1 ページの行（per 個）
	per, pages    int
	scale         int             // 項目の文字の倍率
	nav           image.Rectangle // ページ送りの帯。1 ページに収まれば空
	up, mid, down image.Rectangle // ▲、ページ番号、▼
}

// todoGeometry は、見出しの下の範囲 area に n 個の項目を並べる配置を決める。
// rows（設定の rows）が 0 なら、todoRowH の高さで入るだけ並べる。
func todoGeometry(area image.Rectangle, n, rows int) todoGeom {
	fit := func(h int) int {
		if rows > 0 {
			return rows
		}
		return max(h/todoRowH, 1)
	}
	per := fit(area.Dy())
	navH := 0
	if n > per {
		navH = min(todoNavH, area.Dy()/2)
		per = fit(area.Dy() - navH)
	}
	g := todoGeom{per: per, pages: max((n+per-1)/per, 1)}
	bodyH := area.Dy() - navH
	rowH := max(bodyH/per, 1)
	for i := 0; i < per; i++ {
		g.rows = append(g.rows, image.Rect(area.Min.X, area.Min.Y+i*rowH, area.Max.X, area.Min.Y+(i+1)*rowH))
	}
	g.scale = 1
	for s := todoMaxTextScale; s > 1; s-- {
		if fontH*s <= rowH-8 {
			g.scale = s
			break
		}
	}
	if navH > 0 {
		g.nav = image.Rect(area.Min.X, area.Max.Y-navH, area.Max.X, area.Max.Y)
		a, b := area.Min.X+area.Dx()*3/8, area.Max.X-area.Dx()*3/8
		g.up = image.Rect(area.Min.X, g.nav.Min.Y, a, g.nav.Max.Y)
		g.mid = image.Rect(a, g.nav.Min.Y, b, g.nav.Max.Y)
		g.down = image.Rect(b, g.nav.Min.Y, area.Max.X, g.nav.Max.Y)
	}
	return g
}

// todoEllipsis は、s が等倍で幅 w に入らなければ、入るところまでで切って … を付ける。
func todoEllipsis(s string, w int) string {
	if font.textWidth(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && font.textWidth(string(r)+textEllipsis) > w {
		r = r[:len(r)-1]
	}
	return string(r) + textEllipsis
}

// drawTodo は、Todo のセルの中身を描く。area は見出しの下の範囲。
func drawTodo(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv, subInk RGB) {
	items := todoOrder(env.Todo.Items)
	page, held, nav := w.pager.view(env.Now, w.PageReset, 0)
	if len(items) == 0 {
		s := min(fitScale([]string{todoEmptyText}, area.Dx(), area.Dy()), 2)
		drawCentered(cv, area, area.Min.Y+(area.Dy()-fontH*s)/2, todoEmptyText, s, subInk)
		return
	}
	g := todoGeometry(area, len(items), w.Rows)
	page = min(page, g.pages-1)
	for i, r := range g.rows {
		k := page*g.per + i
		if k >= len(items) {
			break
		}
		if i > 0 {
			cv.fill(image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+1), colTodoRule)
		}
		drawTodoRow(cv, r, items[k], g.scale, items[k].ID == held)
	}
	if g.nav.Empty() {
		return
	}
	drawPageNav(cv, g, page, nav)
}

// drawPageNav は、ページ送りの帯（▲、ページ番号、▼）を描く。Todo とカレンダーで同じ。
func drawPageNav(cv *Canvas, g todoGeom, page, nav int) {
	cv.fill(image.Rect(g.nav.Min.X, g.nav.Min.Y, g.nav.Max.X, g.nav.Min.Y+1), colTodoRule)
	arrow := func(zone image.Rectangle, s string, ok, pressed bool) {
		c := colText
		if !ok {
			c = colTodoOff
		} else if pressed {
			cv.fill(zone.Inset(2), colPressed)
			c = colPressedText
		}
		sc := min(fitScale([]string{s}, zone.Dx(), zone.Dy()-4), 3)
		drawCentered(cv, zone, zone.Min.Y+(zone.Dy()-fontH*sc)/2, s, sc, c)
	}
	arrow(g.up, "▲", page > 0, nav < 0)
	arrow(g.down, "▼", page < g.pages-1, nav > 0)
	p := fmt.Sprintf("%d/%d", page+1, g.pages)
	s := min(fitScale([]string{p}, g.mid.Dx(), fontH*2), 2)
	drawCentered(cv, g.mid, g.mid.Min.Y+(g.mid.Dy()-fontH*s)/2, p, s, colSub)
}

// drawTodoRow は 1 行を描く。左にチェックの箱、右に項目の文。
func drawTodoRow(cv *Canvas, r image.Rectangle, it TodoItem, scale int, held bool) {
	textC, boxC := colText, colSub
	switch {
	case held:
		cv.fill(r.Inset(1), colPressed)
		textC, boxC = colPressedText, colPressedText
	case it.Done:
		textC, boxC = colTodoDone, colTodoDone
	}
	bs := fontH*scale - 2
	box := image.Rect(r.Min.X+todoPad, r.Min.Y+(r.Dy()-bs)/2, r.Min.X+todoPad+bs, r.Min.Y+(r.Dy()-bs)/2+bs)
	cv.frame(box, 2, boxC)
	if it.Done {
		cv.fill(box.Inset(4), boxC)
	}
	tx := box.Max.X + todoPad
	s := todoEllipsis(it.Text, (r.Max.X-todoPad-tx)/scale)
	ty := r.Min.Y + (r.Dy()-fontH*scale)/2
	cv.text(tx, ty, s, scale, textC, r)
	if it.Done {
		y := ty + fontH*scale/2
		cv.fill(image.Rect(tx, y, min(tx+font.textWidth(s)*scale, r.Max.X-todoPad), y+max(scale-1, 1)), textC)
	}
}

// todoKey は、描き直すかどうかを決める中身。
func todoKey(w *WidgetDef, env WidgetEnv) string {
	page, held, nav := w.pager.view(env.Now, w.PageReset, 0)
	return fmt.Sprintf("%d\x00%d\x00%s\x00%d", env.Todo.Rev, page, held, nav)
}

// todoCaption は、Todo のセルの見出し。label のあとに、残り（未完了）の件数を足す。項目がなければ label だけ。
func todoCaption(label string, items []TodoItem) string {
	if len(items) == 0 {
		return label
	}
	n := 0
	for _, it := range items {
		if !it.Done {
			n++
		}
	}
	c := fmt.Sprintf("残り %d", n)
	if n == 0 {
		c = "すべて完了"
	}
	if label == "" {
		return c
	}
	return label + " " + c
}

// widgetArea は、セルの範囲から、見出しの下の範囲を求める。drawCell、drawWidget と同じ計算。
func widgetArea(cell image.Rectangle, caption string) image.Rectangle {
	_, _, area := widgetCaption(cell.Inset(cellGap).Inset(textMargin), caption)
	return area
}

// touchDown は、Todo かカレンダーのセルを押したとき。入力の goroutine から呼ばれるので、待たずに返る。
// ▲▼ ならページを送り、Todo の項目なら長押しを始める。どこを押しても「触った」ことにし、PageReset のあいだページを保つ。
func (w *WidgetDef) touchDown(rt *WidgetRT, t widgetTouch, label string) {
	if !w.ownsTouch() || rt == nil || w.pager == nil { // トラックパッドは Pad が処理する
		return
	}
	env := rt.env()
	var (
		g     todoGeom
		items []TodoItem
		def   int  // 触っていないときに出すページ
		empty bool // ページ送りの帯がない（中身がない）
	)
	switch w.Kind {
	case widgetTodo:
		items = todoOrder(env.Todo.Items)
		g = todoGeometry(widgetArea(t.cell, todoCaption(label, items)), len(items), w.Rows)
		empty = len(items) == 0
	case widgetCal:
		cl := calLayout(widgetArea(t.cell, label), w, env)
		g, def, empty = cl.geom, cl.AutoPage, cl.Message != ""
	}
	s := w.pager
	s.mu.Lock()
	s.token++
	page := min(s.pageLocked(env.Now, w.PageReset, def), g.pages-1)
	s.page, s.touched = page, env.Now
	switch {
	case empty:
	case t.pt.In(g.up):
		s.nav = -1
		s.page = max(page-1, 0)
	case t.pt.In(g.down):
		s.nav = 1
		s.page = min(page+1, g.pages-1)
	case t.pt.In(g.mid), w.Kind != widgetTodo:
	default:
		for i, r := range g.rows {
			if k := page*g.per + i; t.pt.In(r) && k < len(items) {
				s.held = items[k].ID
				tok := s.token
				s.timer = time.AfterFunc(todoHold, func() { s.fire(rt, tok) })
			}
		}
	}
	s.mu.Unlock()
	rt.redraw()
}

// fire は、長押しが続いていれば完了を切り替える（タイマーの goroutine）。
func (s *pagerState) fire(rt *WidgetRT, tok uint64) {
	s.mu.Lock()
	id := s.held
	if s.token != tok || id == "" {
		s.mu.Unlock()
		return
	}
	s.held = ""
	s.mu.Unlock()
	it, err := rt.todos.Toggle(id, time.Now(), "brain")
	if err != nil {
		log.Printf("todo: toggle %s: %v", id, err)
		rt.redraw()
		return
	}
	vlogf("todo: %s %q done=%v (long press)", it.ID, it.Text, it.Done)
}

// touchUp は、離したとき（または設定の差し替えで押していたものを捨てたとき）。長押しを取り消す。
func (w *WidgetDef) touchUp(rt *WidgetRT) {
	if !w.ownsTouch() || w.pager == nil {
		return
	}
	s := w.pager
	s.mu.Lock()
	s.token++
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	changed := s.held != "" || s.nav != 0
	s.held, s.nav = "", 0
	s.mu.Unlock()
	if changed && rt != nil {
		rt.redraw()
	}
}
