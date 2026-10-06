package main

import (
	"fmt"
	"log"
	"strings"
	"sync"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- レイヤーの重なり ----------
//
// base（Layers[0]）は常に一番下にあり、その上に layer_hold などで重ねたレイヤーが
// 重ねた順に積まれる。いちばん上が「今のレイヤー」。
// 割り当てを探すときは上から順に見て、書いてあるレイヤーで止まる（透過）。
// `none` も「書いてある」ので、そこで止まって何もしない。

// View は、今の重なりで有効な割り当て（タッチとソフトキー）と、画面に出す情報。
type View struct {
	Gen        uint64 // 重なりが変わるたびに増える
	Top        int    // 今のレイヤー
	Mode       LayerMode
	Cols, Rows int
	Cells      []*Action // row*Cols+col。割り当ての左上のセルにだけ入る。nil は割り当てなし
	// Anchor は、セルを覆う割り当ての左上のセルの番号（row*Cols+col）。覆うものがなければ -1。
	// span のないセルでは自分自身を指す
	Anchor   []int
	Soft     map[string]*Action
	km       *Keymap // この View を作った割り当て（設定を差し替えると変わる）
	stackKey string
}

// at は、セル (col, row) を覆う割り当てと、その左上のセルを返す。
func (v *View) at(col, row int) (a *Action, ac, ar int) {
	if col < 0 || row < 0 || col >= v.Cols || row >= v.Rows {
		return nil, col, row
	}
	i := v.Anchor[row*v.Cols+col]
	if i < 0 {
		return nil, col, row
	}
	return v.Cells[i], i % v.Cols, i / v.Cols
}

// LayerMode は今のレイヤーの入り方。画面の色を変えるのに使う。
type LayerMode uint8

const (
	modeBase    LayerMode = iota // base だけ
	modeLatched                  // layer_toggle / layer_to で入った（離しても戻らない）
	modeTemp                     // layer_hold / layer_oneshot で入った（一時的）
)

// view は、レイヤーの番号を下から並べた stack について、透過を解決した格子を作る。
// 格子の大きさは、上から見て最初に touch を持つレイヤーで決まる。
// セルが下のレイヤーに透過するのは、格子の大きさが同じあいだだけ。
//
// span のあるセルは、覆う範囲がすべて上のレイヤーで空いているときだけ使う。
// 一部でも上のレイヤーのセル（none を含む）に隠れると、そのセルは出さず、隠れていない範囲は空になる。
func (km *Keymap) view(stack []int) *View {
	v := &View{Top: stack[len(stack)-1], Soft: map[string]*Action{}, km: km}
	var taken []bool // 上のレイヤーまでで、だれかが書いた（覆った）位置
	var anchors map[cellPos]*Action
	for i := len(stack) - 1; i >= 0; i-- {
		g := km.Layers[stack[i]].Grid
		if g == nil {
			continue
		}
		if taken == nil {
			v.Cols, v.Rows = g.Cols, g.Rows
			taken = make([]bool, g.Cols*g.Rows)
			anchors = map[cellPos]*Action{}
		} else if g.Cols != v.Cols || g.Rows != v.Rows {
			break
		}
		var claim []int
		for p, a := range g.Cells {
			free := true
			var cover []int
			for r := p.Row; r < p.Row+a.SpanH; r++ {
				for c := p.Col; c < p.Col+a.SpanW; c++ {
					j := r*v.Cols + c
					free = free && !taken[j]
					cover = append(cover, j)
				}
			}
			if free {
				anchors[p] = a
			}
			claim = append(claim, cover...)
		}
		for _, j := range claim { // 同じレイヤーのセルどうしは重ならない（検証済み）
			taken[j] = true
		}
	}
	v.Cells = make([]*Action, v.Cols*v.Rows)
	v.Anchor = make([]int, v.Cols*v.Rows)
	for i := range v.Anchor {
		v.Anchor[i] = -1
	}
	for p, a := range anchors {
		if a.Kind == actNone {
			continue
		}
		i := p.Row*v.Cols + p.Col
		v.Cells[i] = a
		for r := p.Row; r < p.Row+a.SpanH; r++ {
			for c := p.Col; c < p.Col+a.SpanW; c++ {
				v.Anchor[r*v.Cols+c] = i
			}
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		for name, a := range km.Layers[stack[i]].Soft {
			if _, ok := v.Soft[name]; !ok {
				v.Soft[name] = a
			}
		}
	}
	for name, a := range v.Soft {
		if a.Kind == actNone {
			delete(v.Soft, name)
		}
	}
	return v
}

// lookupKey は、stack の上から順に本体キーの割り当てを探す。
func (km *Keymap) lookupKey(stack []int, code evdev.EvCode) *Action {
	for i := len(stack) - 1; i >= 0; i-- {
		if a, ok := km.Layers[stack[i]].Keys[code]; ok {
			return a
		}
	}
	return nil
}

// hitSoft は生の座標がどのソフトキーの範囲に入るかを返す。
func (km *Keymap) hitSoft(x, y int32) (string, bool) {
	for _, a := range km.Areas {
		if a.contains(x, y) {
			return a.Name, true
		}
	}
	return "", false
}

// ---------- 入力の処理 ----------

type stackEntry struct {
	layer int
	kind  ActKind // actHold, actToggle, actOneshot, actTo
	src   string  // layer_hold を押している入力
}

// Engine は入力を受け取り、レイヤーを解決して HID の送信と画面の更新を行う。
// キーボードとタッチの goroutine から呼ばれるので、ロックで守る。
type Engine struct {
	mu     sync.Mutex
	km     *Keymap
	out    *State
	stack  []stackEntry       // base の上に重ねたもの（下から順）
	down   map[string]*Action // 押している入力と、押したときの割り当て
	view   *View
	gen    uint64
	onView func(*View) // 重なりが変わったとき（画面の描き直し）。待たずに返ること
	// onStatus は重なりが変わったとき（設定 GUI への通知）。ロックを持ったまま呼ぶので、待たずに返ること
	onStatus func(EngineStatus)
	widgets  *WidgetRT    // 押した位置で働くウィジェット（Todo）に渡す状態。nil なら 800x480 とみなす
	touchAt  *widgetTouch // PressTouch から press に、押した位置を渡す
}

// SetWidgets は、ウィジェットがタップを処理するのに使う状態を設定する。
func (e *Engine) SetWidgets(rt *WidgetRT) {
	e.mu.Lock()
	e.widgets = rt
	e.mu.Unlock()
}

func NewEngine(km *Keymap, out *State) *Engine {
	e := &Engine{km: km, out: out, down: map[string]*Action{}}
	e.refresh()
	return e
}

func (e *Engine) layers() []int {
	s := make([]int, 1, len(e.stack)+1) // base
	for _, en := range e.stack {
		s = append(s, en.layer)
	}
	return s
}

// View は今の重なりを返す。
func (e *Engine) View() *View {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.view
}

func keyID(code evdev.EvCode) string { return fmt.Sprintf("k:%d", code) }

// PressKey は本体キーが押されたときに呼ぶ。
func (e *Engine) PressKey(code evdev.EvCode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.press(keyID(code), e.km.lookupKey(e.layers(), code))
}

// ReleaseKey は本体キーが離されたときに呼ぶ。
func (e *Engine) ReleaseKey(code evdev.EvCode) { e.Release(keyID(code)) }

// TouchHit はタッチの判定結果。
type TouchHit struct {
	Gen      uint64
	Soft     string // ソフトキーの名前（セルでないとき）
	Col, Row int
	Mapped   bool
	Own      bool // ウィジェットが押した位置で働き、自分で押したことを描く（セル全体を光らせない）
}

// PressTouch はタッチの生座標で押す。判定は今の重なりの格子で行う。
func (e *Engine) PressTouch(x, y int32) TouchHit {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.view
	h := TouchHit{Gen: v.Gen, Col: -1, Row: -1}
	var a *Action
	// ソフトキーの範囲は画面の右端と重なるので、今の重なりで割り当てがあるときだけ使う
	if name, ok := e.km.hitSoft(x, y); ok && v.Soft[name] != nil {
		h.Soft, a = name, v.Soft[name]
	} else if v.Cols > 0 {
		c, r := touchCell(e.km.Touch, v.Cols, v.Rows, x, y)
		a, h.Col, h.Row = v.at(c, r) // span のセルは、左上のセルとして扱う
		if a != nil && a.Widget.ownsTouch() {
			h.Own = true
			W, H := 800, 480
			if e.widgets != nil {
				W, H = e.widgets.screen()
			}
			e.touchAt = &widgetTouch{cell: cellRect(h.Col, h.Row, a.SpanW, a.SpanH, v.Cols, v.Rows, W, H),
				pt: touchPoint(e.km.Touch, x, y, W, H)}
		}
	}
	h.Mapped = a.tappable()
	e.press("t", a)
	e.touchAt = nil
	return h
}

// Release は入力が離されたときに呼ぶ。押したときの割り当てで離すので、
// 押しているあいだにレイヤーが変わっても、キーが押しっぱなしにならない。
func (e *Engine) Release(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.down[id]
	if !ok {
		return
	}
	delete(e.down, id)
	if a == nil {
		return
	}
	switch a.Kind {
	case actKey:
		e.out.release(id)
	case actHold:
		e.removeIf(func(en stackEntry) bool { return en.kind == actHold && en.src == id })
		e.refresh()
	case actWidget:
		a.Widget.touchUp(e.widgets)
	}
}

// 呼び出し側でロック済みであること
func (e *Engine) press(id string, a *Action) {
	if _, dup := e.down[id]; dup {
		return
	}
	e.down[id] = a
	consumeOneshot := true
	if a != nil {
		switch a.Kind {
		case actKey:
			e.out.press(id, a.Combo) // HID を先に送る
		case actHold:
			e.stack = append(e.stack, stackEntry{a.Layer, actHold, id})
			consumeOneshot = false
		case actToggle:
			if !e.removeIf(func(en stackEntry) bool {
				return en.layer == a.Layer && (en.kind == actToggle || en.kind == actTo)
			}) {
				e.stack = append(e.stack, stackEntry{layer: a.Layer, kind: actToggle})
			}
			consumeOneshot = false
		case actOneshot:
			// 待っている oneshot をもう一度押すと取り消す
			if !e.removeIf(func(en stackEntry) bool { return en.kind == actOneshot && en.layer == a.Layer }) {
				e.stack = append(e.stack, stackEntry{layer: a.Layer, kind: actOneshot})
			}
			consumeOneshot = false
		case actTo:
			e.stack = e.stack[:0]
			if a.Layer != 0 {
				e.stack = append(e.stack, stackEntry{layer: a.Layer, kind: actTo})
			}
			consumeOneshot = false
		case actWidget:
			// ウィジェットに任せる。待たずに返る
			if e.touchAt != nil {
				a.Widget.touchDown(e.widgets, *e.touchAt, a.Spec.Label)
			}
		}
	}
	// layer_* 以外を押したら（割り当てのないキーでも）、oneshot は使い終わる
	if consumeOneshot {
		e.removeIf(func(en stackEntry) bool { return en.kind == actOneshot })
	}
	e.refresh()
}

// removeIf は条件に合う重なりを外し、外したかどうかを返す。
func (e *Engine) removeIf(f func(stackEntry) bool) bool {
	kept := e.stack[:0]
	removed := false
	for _, en := range e.stack {
		if f(en) {
			removed = true
		} else {
			kept = append(kept, en)
		}
	}
	e.stack = kept
	return removed
}

// refresh は重なりが変わっていれば View を作り直し、画面に知らせる。
func (e *Engine) refresh() {
	var b strings.Builder
	for _, en := range e.stack {
		fmt.Fprintf(&b, "%d/%d,", en.layer, en.kind)
	}
	key := b.String()
	if e.view != nil && e.view.stackKey == key {
		return
	}
	e.gen++
	v := e.km.view(e.layers())
	v.Gen, v.stackKey = e.gen, key
	if n := len(e.stack); n > 0 {
		v.Mode = modeLatched
		if k := e.stack[n-1].kind; k == actHold || k == actOneshot {
			v.Mode = modeTemp
		}
	}
	e.view = v
	if e.gen > 1 {
		vlogf("layer: %s", e.describe())
	}
	if e.onView != nil {
		e.onView(v)
	}
	if e.onStatus != nil {
		e.onStatus(e.statusLocked())
	}
}

// SetOnStatus は、重なりが変わったときの通知先を設定する（設定 GUI へのレイヤーの通知）。
func (e *Engine) SetOnStatus(f func(EngineStatus)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onStatus = f
}

func (e *Engine) describe() string {
	parts := []string{e.km.Layers[0].Name}
	for _, en := range e.stack {
		parts = append(parts, fmt.Sprintf("%s(%s)", e.km.Layers[en.layer].Name, strings.TrimPrefix(en.kind.String(), "layer_")))
	}
	return strings.Join(parts, " > ")
}

// Reload は割り当てを差し替える（設定 GUI からの保存）。デーモンは止めない。
// 押しているキーをすべて離し（空のレポートを送る）、重なりを base だけに戻してから差し替える。
// 押したまま差し替えたキーやタッチは、離しても何も送らない。
func (e *Engine) Reload(km *Keymap) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.out.releaseAll()
	for _, a := range e.down { // 長押しの途中なら取り消す
		if a != nil && a.Kind == actWidget {
			a.Widget.touchUp(e.widgets)
		}
	}
	e.down = map[string]*Action{}
	e.stack = nil
	e.km = km
	e.view = nil // 重なりが同じでも作り直し、画面に知らせる
	e.refresh()
	log.Printf("config reloaded")
	logLayers(km)
}

// HitTest は、タッチの生座標が今の格子のどのセルか、どのソフトキーの範囲かを返す（押さない）。
// Soft は割り当ての有無によらず、範囲に入っていれば名前を返す。
func (e *Engine) HitTest(x, y int32) TouchHit {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.view
	h := TouchHit{Gen: v.Gen, Col: -1, Row: -1}
	if name, ok := e.km.hitSoft(x, y); ok {
		h.Soft = name
	}
	if v.Cols > 0 {
		h.Col, h.Row = touchCell(e.km.Touch, v.Cols, v.Rows, x, y)
	}
	return h
}

// EngineStatus は設定 GUI に返す、今のレイヤーの状態。
type EngineStatus struct {
	Layer string        `json:"layer"` // 今のレイヤー（いちばん上）
	Label string        `json:"label"`
	Mode  string        `json:"mode"` // base、latched（切り替えたまま）、temp（一時的）
	Stack []StackStatus `json:"stack"`
	Cols  int           `json:"cols"`
	Rows  int           `json:"rows"`
}

type StackStatus struct {
	Layer string `json:"layer"`
	Kind  string `json:"kind"`
}

var modeNames = [...]string{modeBase: "base", modeLatched: "latched", modeTemp: "temp"}

func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statusLocked()
}

func (e *Engine) statusLocked() EngineStatus {
	v := e.view
	top := e.km.Layers[v.Top]
	st := EngineStatus{Layer: top.Name, Label: top.title(), Mode: modeNames[v.Mode],
		Stack: []StackStatus{}, Cols: v.Cols, Rows: v.Rows}
	for _, en := range e.stack {
		st.Stack = append(st.Stack, StackStatus{e.km.Layers[en.layer].Name, en.kind.String()})
	}
	return st
}

// SetOnView は画面への通知先を設定する。shown は画面に描いてある View.Gen で、
// そのあとに重なりが変わっていれば、今の重なりを一度知らせる。
func (e *Engine) SetOnView(f func(*View), shown uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onView = f
	if f != nil && e.view.Gen != shown {
		f(e.view)
	}
}

func logLayers(km *Keymap) {
	for i, l := range km.Layers {
		cells, cols, rows := 0, 0, 0
		if l.Grid != nil {
			cells, cols, rows = len(l.Grid.Cells), l.Grid.Cols, l.Grid.Rows
		}
		log.Printf("layer %d %q (%s): %d keys, touch %dx%d %d cells, %d soft keys",
			i, l.Name, l.title(), len(l.Keys), cols, rows, cells, len(l.Soft))
	}
}
