package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"strings"
	"testing"
	"time"
)

// タッチの生座標が、そのまま画面のドットになる設定（800x480）
const todoConfig = `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: todo, span: [2, 3], label: Todo }
        "2,0": { widget: todo, rows: 2, span: [2, 1] }
        "3,2": B
`

func TestTodoWidgetConfig(t *testing.T) {
	_, km := compileText(t, todoConfig)
	a := km.Layers[0].Grid.Cells[cellPos{0, 0}]
	if a.Kind != actWidget || a.Widget.Kind != widgetTodo || !a.tappable() || !a.Widget.ownsTouch() {
		t.Fatalf("todo cell = %+v", a)
	}
	if w := km.Layers[0].Grid.Cells[cellPos{2, 0}].Widget; w.Rows != 2 {
		t.Errorf("rows = %d", w.Rows)
	}
	bad := map[string]string{
		`{ widget: todo, key: B }`:         "handles taps itself",
		`{ widget: todo, layer_to: base }`: "handles taps itself",
		`{ widget: todo, rows: 21 }`:       "rows must be 1..20",
		`{ widget: todo, rows: -1 }`:       "rows must be 1..20",
		`{ widget: todo, id: x }`:          "cannot be used with widget: todo",
		`{ widget: clock, rows: 3 }`:       "rows is for widget: todo",
		`{ key: B, rows: 3 }`:              "rows is for widget: todo",
	}
	for cell, want := range bad {
		cfg, err := parseConfig([]byte("touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers:\n  - name: base\n    touch:\n      cols: 2\n      rows: 2\n      cells:\n        \"0,0\": " + cell + "\n"))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", cell, err, want)
		}
	}
	cfg, _ := parseConfig([]byte(todoConfig))
	if !todoShown(cfg) {
		t.Error("todoShown")
	}
}

func todoTexts(items []TodoItem) string {
	var s []string
	for _, it := range items {
		x := it.Text
		if it.Done {
			x += "✓"
		}
		s = append(s, x)
	}
	return strings.Join(s, ",")
}

func TestTodoServiceOps(t *testing.T) {
	dir := dataDir(t)
	ts := NewTodoService(OpenStore(dir))
	ts.saved = make(chan struct{}, 64)
	now := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	a, _, err := ts.Add("  牛乳\nを買う\t ", -1, now, "test")
	if err != nil || a.Text != "牛乳 を買う" || a.ID != "t1" || a.Rev != 1 {
		t.Fatalf("add = %+v, %v", a, err)
	}
	b, _, _ := ts.Add("b", -1, now, "test")
	c, l, _ := ts.Add("c", 0, now, "test") // 先頭に
	if todoTexts(l.Items) != "c,牛乳 を買う,b" || l.Rev != 3 {
		t.Fatalf("items = %s rev %d", todoTexts(l.Items), l.Rev)
	}
	for _, bad := range []string{"", "  ", "a\x01b", strings.Repeat("あ", todoMaxRunes+1)} {
		if _, _, err := ts.Add(bad, -1, now, "test"); !errors.Is(err, errBadTodo) {
			t.Errorf("add %q: %v", bad, err)
		}
	}
	// 完了にする。rev が違えば conflict で、何も変えない
	done := true
	stale := a.Rev
	it, l, err := ts.Edit(a.ID, &stale, TodoEdit{Done: &done}, now.Add(time.Minute), "gui")
	if err != nil || !it.Done || it.DoneAt == nil || it.Rev != 4 || it.Source != "gui" {
		t.Fatalf("edit = %+v, %v", it, err)
	}
	if _, _, err := ts.Edit(a.ID, &stale, TodoEdit{Text: ptr("x")}, now, "gui"); !errors.Is(err, errTodoConflict) {
		t.Errorf("stale rev: %v", err)
	}
	if _, _, err := ts.Edit("t99", nil, TodoEdit{Text: ptr("x")}, now, "gui"); !errors.Is(err, errTodoNotFound) {
		t.Errorf("missing: %v", err)
	}
	if ts.Snapshot().Rev != 4 {
		t.Errorf("a failed edit must not change rev: %d", ts.Snapshot().Rev)
	}
	// 画面の順：未完了のあとに完了
	if got := todoTexts(todoOrder(l.Items)); got != "c,b,牛乳 を買う✓" {
		t.Errorf("order = %s", got)
	}
	// 並べ替え、消す
	if l, err = ts.Move(b.ID, nil, 0); err != nil || todoTexts(l.Items) != "b,c,牛乳 を買う✓" {
		t.Fatalf("move = %s, %v", todoTexts(l.Items), err)
	}
	if _, err := ts.Move(b.ID, nil, 3); !errors.Is(err, errBadTodo) {
		t.Errorf("move out of range: %v", err)
	}
	if l, err = ts.Delete(c.ID, &c.Rev); err != nil || todoTexts(l.Items) != "b,牛乳 を買う✓" {
		t.Fatalf("delete = %s, %v", todoTexts(l.Items), err)
	}
	// 完了を消す
	n, l, err := ts.ClearDone()
	if err != nil || n != 1 || todoTexts(l.Items) != "b" {
		t.Fatalf("clear done = %d %s %v", n, todoTexts(l.Items), err)
	}
	rev := l.Rev
	if n, l, _ = ts.ClearDone(); n != 0 || l.Rev != rev {
		t.Errorf("nothing to clear: n=%d rev %d→%d", n, rev, l.Rev)
	}
	// 切り替え（Brain での長押し）
	if it, err := ts.Toggle(b.ID, now, "brain"); err != nil || !it.Done {
		t.Fatalf("toggle = %+v %v", it, err)
	}
	if it, _ := ts.Toggle(b.ID, now, "brain"); it.Done || it.DoneAt != nil {
		t.Fatalf("toggle back = %+v", it)
	}
	waitTodoSaved(t, ts)
	// デーモンを起動し直しても残り、ID を使い回さない
	ts2 := NewTodoService(OpenStore(dir))
	if got := ts2.Snapshot(); todoTexts(got.Items) != "b" || got.Rev != ts.Snapshot().Rev {
		t.Fatalf("after restart: %s rev %d (want rev %d)", todoTexts(got.Items), got.Rev, ts.Snapshot().Rev)
	}
	ts2.saved = make(chan struct{}, 64)
	if d, _, _ := ts2.Add("d", -1, now, "test"); d.ID != "t4" {
		t.Errorf("new id after restart = %s, want t4", d.ID)
	}
	waitTodoSaved(t, ts2) // 書いている途中で TempDir を消さない
}

// waitTodoSaved は、保存は返事のあとに行われるので、今の版が todo.json に書かれるまで待つ。ts.saved を作っておくこと。
func waitTodoSaved(t *testing.T, ts *TodoService) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		var f todoFile
		if ts.store.Load(todoStoreName, &f) == nil && f.Rev == ts.Snapshot().Rev {
			return
		}
		select {
		case <-ts.saved:
		case <-time.After(time.Until(deadline)):
			t.Fatal("todo.json was not saved")
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestTodoGeometry(t *testing.T) {
	area := image.Rect(12, 40, 388, 468) // 2×3 のセルに見出しを付けたとき
	g := todoGeometry(area, 3, 0)
	if g.per != 8 || g.pages != 1 || !g.nav.Empty() || g.scale != 3 {
		t.Fatalf("fits: %+v", g)
	}
	g = todoGeometry(area, 10, 0)
	if g.per != 7 || g.pages != 2 || g.nav.Dy() != todoNavH || g.rows[6].Max.Y > g.nav.Min.Y {
		t.Fatalf("pages: %+v", g)
	}
	if !g.up.Union(g.mid).Union(g.down).Eq(g.nav) || g.up.Dx() != g.down.Dx() {
		t.Errorf("nav zones %v %v %v", g.up, g.mid, g.down)
	}
	g = todoGeometry(area, 10, 3)
	if g.per != 3 || g.pages != 4 || g.rows[0].Dy() != (428-todoNavH)/3 {
		t.Errorf("rows: 3: %+v", g)
	}
	g = todoGeometry(image.Rect(0, 0, 100, 30), 5, 0)
	if g.per != 1 || g.scale != 1 || len(g.rows) != 1 {
		t.Errorf("tiny: %+v", g)
	}
}

type todoTest struct {
	t  *testing.T
	ts *TodoService
	e  *Engine
	rt *WidgetRT
	w  *WidgetDef
}

func newTodoTest(t *testing.T, n int) *todoTest {
	t.Helper()
	old := todoHold
	todoHold = 60 * time.Millisecond
	t.Cleanup(func() { todoHold = old })
	_, km := compileText(t, todoConfig)
	ts := NewTodoService(OpenStore(dataDir(t)))
	ts.saved = make(chan struct{}, 256)
	t.Cleanup(func() { waitTodoSaved(t, ts) }) // TempDir を消す前に、保存が終わるのを待つ
	for i := 0; i < n; i++ {
		ts.Add(string(rune('a'+i)), -1, time.Now(), "test")
	}
	e := NewEngine(km, &State{hid: NewHIDWriter("/dev/null"), active: map[string]Combo{}})
	rt := NewWidgetRT(ts)
	e.SetWidgets(rt)
	return &todoTest{t: t, ts: ts, e: e, rt: rt, w: km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget}
}

// row は、2×3 の Todo のセル（見出し付き）の i 行目の中ほどの点。
func (tt *todoTest) row(i int) image.Point {
	items := todoOrder(tt.ts.Snapshot().Items)
	g := todoGeometry(todoArea(image.Rect(0, 0, 400, 480), "Todo"), len(items), 0)
	r := g.rows[i]
	return image.Pt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2)
}

func (tt *todoTest) tap(p image.Point, hold time.Duration) TouchHit {
	h := tt.e.PressTouch(int32(p.X), int32(p.Y))
	time.Sleep(hold)
	tt.e.Release("t")
	return h
}

func TestTodoLongPressToggles(t *testing.T) {
	tt := newTodoTest(t, 3)
	h := tt.e.PressTouch(int32(tt.row(1).X), int32(tt.row(1).Y))
	if !h.Mapped || !h.Own || h.Col != 0 || h.Row != 0 {
		t.Fatalf("hit = %+v", h)
	}
	if _, held, _ := tt.w.todo.view(); held != "t2" {
		t.Fatalf("held = %q, want t2 (shown while pressing)", held)
	}
	time.Sleep(todoHold * 3)
	if _, held, _ := tt.w.todo.view(); held != "" {
		t.Errorf("held after toggling = %q", held)
	}
	tt.e.Release("t")
	if got := todoTexts(tt.ts.Snapshot().Items); got != "a,b✓,c" {
		t.Fatalf("after long press: %s", got)
	}
	// 短く押しただけでは変わらない
	tt.tap(tt.row(0), todoHold/3)
	time.Sleep(todoHold * 2)
	if got := todoTexts(tt.ts.Snapshot().Items); got != "a,b✓,c" {
		t.Fatalf("after a short tap: %s", got)
	}
	// 完了した項目は下に並ぶ。下の行を長押しすると、未完了に戻る
	tt.tap(tt.row(2), todoHold*3)
	if got := todoTexts(tt.ts.Snapshot().Items); got != "a,b,c" {
		t.Fatalf("toggle back: %s", got)
	}
	// 項目のない行は何もしない
	tt.tap(tt.row(5), todoHold*3)
	if got := todoTexts(tt.ts.Snapshot().Items); got != "a,b,c" {
		t.Fatalf("empty row: %s", got)
	}
}

func TestTodoPagesAndCancel(t *testing.T) {
	tt := newTodoTest(t, 10)
	g := todoGeometry(todoArea(image.Rect(0, 0, 400, 480), "Todo"), 10, 0)
	center := func(r image.Rectangle) image.Point { return image.Pt(r.Min.X+r.Dx()/2, r.Min.Y+r.Dy()/2) }
	tt.e.PressTouch(int32(center(g.down).X), int32(center(g.down).Y))
	if page, _, nav := tt.w.todo.view(); page != 1 || nav != 1 {
		t.Fatalf("▼: page %d nav %d", page, nav)
	}
	tt.e.Release("t")
	tt.tap(center(g.down), 0) // 最後のページより先には行かない
	if page, _, nav := tt.w.todo.view(); page != 1 || nav != 0 {
		t.Fatalf("▼ on the last page: page %d nav %d", page, nav)
	}
	// 2 ページ目の 1 行目は 8 番目の項目
	tt.tap(tt.row(0), todoHold*3)
	if it := tt.ts.Snapshot().Items[7]; !it.Done || it.Text != "h" {
		t.Fatalf("page 2 row 0 = %+v", it)
	}
	tt.tap(center(g.up), 0)
	tt.tap(center(g.up), 0)
	if page, _, _ := tt.w.todo.view(); page != 0 {
		t.Fatalf("▲: page %d", page)
	}
	// 長押しの途中で設定を差し替えたら、切り替えない
	tt.e.PressTouch(int32(tt.row(0).X), int32(tt.row(0).Y))
	_, km := compileText(t, todoConfig)
	tt.e.Reload(km)
	time.Sleep(todoHold * 3)
	if tt.ts.Snapshot().Items[0].Done {
		t.Fatal("reload during a long press must cancel it")
	}
	if _, held, _ := tt.w.todo.view(); held != "" {
		t.Errorf("held = %q after reload", held)
	}
}

// 入力の処理は、保存（SD カード）を待たない
func TestTodoTapNeverBlocks(t *testing.T) {
	tt := newTodoTest(t, 3)
	tt.ts.store.mu.Lock() // 保存が詰まっている
	defer tt.ts.store.mu.Unlock()
	start := time.Now()
	for i := 0; i < 50; i++ {
		tt.e.PressTouch(int32(tt.row(0).X), int32(tt.row(0).Y))
		tt.e.Release("t")
		tt.ts.Toggle("t1", time.Now(), "test")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("50 taps and toggles took %v while saving was stuck", d)
	}
}

func TestTodoWidgetRedraw(t *testing.T) {
	tt := newTodoTest(t, 3)
	km := tt.e.km
	env := WidgetEnv{Now: time.Now(), TimeSynced: true, Todo: tt.ts.Snapshot()}
	d := &Display{cv: NewCanvas(800, 480, 1600, rgb565, 0), env: func() WidgetEnv { return env }}
	l := buildLayout(km, km.view([]int{0}))
	l.W, l.H = 800, 480
	d.layout, d.drawn = l, make([]bool, len(l.Cells))
	d.drawAllLocked(l)
	redrawn := func(what string, want ...int) {
		t.Helper()
		before := append([]string(nil), d.wkeys...)
		d.redrawWidgets()
		ref := NewCanvas(800, 480, 1600, rgb565, 0)
		rl := *l
		rl.Env = env
		drawAll(ref, &rl, d.drawn)
		if !bytes.Equal(ref.pix, d.cv.pix) {
			t.Fatalf("%s: differs from a full redraw", what)
		}
		var got []int
		for i := range before {
			if before[i] != d.wkeys[i] {
				got = append(got, i)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: redrew %v, want %v", what, got, want)
		}
	}
	redrawn("nothing changed")
	tt.ts.Add("d", -1, time.Now(), "test")
	env.Todo = tt.ts.Snapshot()
	redrawn("added", 0, 2) // 同じ一覧を出すセルは両方
	// 押しているあいだは、そのセルだけ描き直す
	tt.e.PressTouch(int32(tt.row(0).X), int32(tt.row(0).Y))
	redrawn("held", 0)
	tt.e.Release("t")
	redrawn("released", 0)
	if !d.wakeAt.IsZero() {
		t.Errorf("todo needs no timer: wakeAt %v", d.wakeAt)
	}
}

func TestProtocolTodo(t *testing.T) {
	d := newTestDaemon(t, todoConfig)
	hello := result(t, d.call("hello", nil))
	for _, c := range []string{"get_todo", "todo_add", "todo_update", "todo_delete", "todo_move", "todo_clear_done", "subscribe_data"} {
		if !strings.Contains(fmt.Sprint(hello["commands"]), c) {
			t.Errorf("hello does not list %s", c)
		}
	}
	r := result(t, d.call("get_todo", nil))
	if r["rev"] != float64(0) || len(r["items"].([]any)) != 0 || r["shown"] != true {
		t.Fatalf("empty get_todo = %v", r)
	}
	r = result(t, d.call("todo_add", map[string]any{"text": "牛乳", "source": "gui"}))
	item := r["item"].(map[string]any)
	if item["id"] != "t1" || item["text"] != "牛乳" || item["done"] != false || r["rev"] != float64(1) || len(r["items"].([]any)) != 1 {
		t.Fatalf("todo_add = %v", r)
	}
	result(t, d.call("todo_add", map[string]any{"text": "卵", "index": 0}))
	// 完了にする（rev が合っている）
	r = result(t, d.call("todo_update", map[string]any{"item": "t1", "rev": 1, "done": true, "source": "gui"}))
	if it := r["item"].(map[string]any); it["done"] != true || it["done_at"] == nil || it["rev"] != float64(3) {
		t.Fatalf("todo_update = %v", r)
	}
	// 古い rev では書き換えない
	m := d.call("todo_update", map[string]any{"item": "t1", "rev": 1, "text": "x"})
	if errCode(m) != "conflict" {
		t.Fatalf("stale rev: %v", m)
	}
	for _, c := range []struct {
		cmd    string
		params map[string]any
		code   string
	}{
		{"todo_update", map[string]any{"item": "t9", "done": true}, "not_found"},
		{"todo_update", map[string]any{"item": "t1"}, "bad_request"},
		{"todo_update", map[string]any{"done": true}, "bad_request"},
		{"todo_add", map[string]any{}, "bad_request"},
		{"todo_add", map[string]any{"text": "  "}, "bad_request"},
		{"todo_move", map[string]any{"item": "t1"}, "bad_request"},
		{"todo_move", map[string]any{"item": "t1", "index": 5}, "bad_request"},
		{"todo_delete", map[string]any{"item": "t1", "rev": 1}, "conflict"},
	} {
		if m := d.call(c.cmd, c.params); errCode(m) != c.code {
			t.Errorf("%s %v: %v, want %s", c.cmd, c.params, m, c.code)
		}
	}
	r = result(t, d.call("todo_move", map[string]any{"item": "t1", "index": 0}))
	if got := fmt.Sprint(r["items"].([]any)[0].(map[string]any)["id"]); got != "t1" {
		t.Errorf("todo_move: first = %s", got)
	}
	// Brain で長押しして切り替えたら、購読している GUI に知らせる
	result(t, d.call("subscribe_data", map[string]any{"enable": true}))
	if _, err := d.todos.Toggle("t2", time.Now(), "brain"); err != nil {
		t.Fatal(err)
	}
	// 購読の前の変更の通知が、遅れて届くことがある。rev 5 まで読む
	var ev map[string]any
	for ev = d.readMsg(); ev["event"] == "todo" && ev["rev"].(float64) < 5; ev = d.readMsg() {
	}
	if ev["event"] != "todo" || ev["rev"] != float64(5) {
		t.Fatalf("event = %v", ev)
	}
	items := ev["items"].([]any)
	if it := items[1].(map[string]any); it["id"] != "t2" || it["done"] != true || it["source"] != "brain" {
		t.Fatalf("event items = %v", items)
	}
	// 自分で書き換えたときも届く（返事の前か後か決まっていない。返事と同じ rev なので、GUI は読み捨ててよい）
	r = result(t, d.call("todo_clear_done", nil))
	if r["removed"] != float64(2) || len(r["items"].([]any)) != 0 {
		t.Fatalf("todo_clear_done = %v", r)
	}
	// 購読をやめたら届かない
	result(t, d.call("subscribe_data", map[string]any{"enable": false}))
	d.todos.Add("x", -1, time.Now(), "test")
	if r := result(t, d.call("get_todo", nil)); r["rev"] != float64(7) {
		t.Fatalf("get_todo = %v", r)
	}
	if d.monitor.dataCh != nil {
		t.Error("still subscribed")
	}
}

// 設定 GUI のプレビュー（gui/src/todowidget.ts）が Go と同じ配置と省略をすることを、GUI のテストで確かめるための表。
// 配置を変えたら LEFTHAND_UPDATE_TODOLAYOUT=1 go test -run TodoLayoutTable で書き直す。
const guiTodoLayoutJSON = "gui/test/fixtures/todolayout.json"

type todoLayoutCase struct {
	Area  [4]int     `json:"area"`
	N     int        `json:"n"`
	Rows  int        `json:"rows"`
	Per   int        `json:"per"`
	Pages int        `json:"pages"`
	Scale int        `json:"scale"`
	First [4]int     `json:"first"`
	Last  [4]int     `json:"last"`
	Nav   *[3][4]int `json:"nav"` // ▲、ページ番号、▼
}

type todoEllipsisCase struct {
	Text string `json:"text"`
	W    int    `json:"w"`
	Out  string `json:"out"`
}

func rect4(r image.Rectangle) [4]int { return [4]int{r.Min.X, r.Min.Y, r.Max.X, r.Max.Y} }

func todoLayoutTable() map[string]any {
	var layout []todoLayoutCase
	areas := []image.Rectangle{image.Rect(12, 40, 388, 468), image.Rect(12, 12, 388, 148), image.Rect(212, 172, 588, 308),
		image.Rect(0, 0, 100, 30), image.Rect(5, 7, 791, 473), image.Rect(12, 12, 188, 468)}
	for _, a := range areas {
		for _, n := range []int{1, 3, 7, 8, 9, 30} {
			for _, rows := range []int{0, 1, 3, 20} {
				g := todoGeometry(a, n, rows)
				c := todoLayoutCase{Area: rect4(a), N: n, Rows: rows, Per: g.per, Pages: g.pages, Scale: g.scale,
					First: rect4(g.rows[0]), Last: rect4(g.rows[len(g.rows)-1])}
				if !g.nav.Empty() {
					c.Nav = &[3][4]int{rect4(g.up), rect4(g.mid), rect4(g.down)}
				}
				layout = append(layout, c)
			}
		}
	}
	var ell []todoEllipsisCase
	for _, s := range []string{"牛乳を買う", "brain-deck の README を書き直す（cron の例と、終了コードの表も）", "x", "混在 mixed 🙂 text", strings.Repeat("あ", 200)} {
		for _, w := range []int{0, 6, 30, 100, 160, 1000} {
			ell = append(ell, todoEllipsisCase{s, w, todoEllipsis(s, w)})
		}
	}
	return map[string]any{"layout": layout, "ellipsis": ell}
}

func TestTodoLayoutTable(t *testing.T) {
	want, _ := json.MarshalIndent(todoLayoutTable(), "", " ")
	want = append(want, '\n')
	if os.Getenv("LEFTHAND_UPDATE_TODOLAYOUT") == "1" {
		if err := os.WriteFile(guiTodoLayoutJSON, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(guiTodoLayoutJSON); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date (%v); run LEFTHAND_UPDATE_TODOLAYOUT=1 go test -run TodoLayoutTable", guiTodoLayoutJSON, err)
	}
}
