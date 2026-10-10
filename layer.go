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
	Anchor []int
	Soft   map[string]*Action
	// Wallpaper は格子に敷く壁紙の id。格子を作ったレイヤーから下へ、セルと同じく透過する範囲で、
	// いちばん上にある touch.background
	Wallpaper string
	Rot       int     // 画面の向き（0、90、180、270）
	Portrait  bool    // 縦向き（格子は PGrid）
	Bar       bool    // 上に状態の帯を出す
	Offline   bool    // PC から外れている（PC に送るセルを休みにして、薄く描く。offline.go）
	km        *Keymap // この View を作った割り当て（設定を差し替えると変わる）
	stackKey  string
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
//
// portrait なら、各レイヤーの縦向きの格子（PGrid）を使う。
func (km *Keymap) view(stack []int, portrait ...bool) *View {
	v := &View{Top: stack[len(stack)-1], Soft: map[string]*Action{}, km: km}
	if len(portrait) > 0 {
		v.Portrait = portrait[0]
	}
	var taken []bool // 上のレイヤーまでで、だれかが書いた（覆った）位置
	var anchors map[cellPos]*Action
	for i := len(stack) - 1; i >= 0; i-- {
		g := km.Layers[stack[i]].Grid
		if v.Portrait {
			g = km.Layers[stack[i]].PGrid
		}
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
		if v.Wallpaper == "" {
			v.Wallpaper = g.Background
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
	if h := km.Home; h != nil {
		v.Soft[h.SoftKey] = h.goHome // HOME は、どのレイヤーでもホーム画面へ
	}
	for name, a := range v.Soft {
		if a.Kind == actNone {
			delete(v.Soft, name)
		}
	}
	// 状態の帯：上から見て、status_bar を書いたレイヤーか、格子を持つレイヤーで決める（格子を持つレイヤーが書いていなければ status_bar.show）
	v.Bar = km.Bar.Show
	for i := len(stack) - 1; i >= 0; i-- {
		l := km.Layers[stack[i]]
		if l.StatusBar != nil {
			v.Bar = *l.StatusBar
			break
		}
		if l.Grid != nil {
			break
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
	pad      *Pad         // トラックパッドの判定。nil ならトラックパッドは動かない
	usb      *USBMode     // USB の形の切り替え。nil なら usb_mode は何もしない
	term     *TermMode    // 端末モード。nil なら terminal は何もしない。端末モードのあいだ、入力はここに回す
	pressing string       // press の途中で、押している入力（レイヤーが変わっても、この入力のマウスのボタンは離さない）
	rot      int          // 画面の向き（rotate.go）
	pw, ph   int          // 物理の画面の大きさ。0 なら 800x480
	rotStore *RotateStore // 向きの保存。nil なら保存しない
	onRotate func(int)    // 向きが変わったとき（ログと設定 GUI への通知）。ロックを持ったまま呼ぶので、待たずに返ること
	// offline は PC から外れているか（offline.go）。autoHome は、外れてホーム画面に切り替える前の重なり（nil なら切り替えていない）
	offline     bool
	autoHome    []stackEntry
	notify      func(string) // 画面の下に知らせを出す。nil なら出さない
	offNoticeAt time.Time
	input       *Prompt // Brain のキーボードで文字を入れる欄（prompt.go）。nil なら使わない
}

// SetRotation は、画面の向きと、物理の画面の大きさを決める（起動したとき）。rs は向きの保存（nil なら保存しない）。
func (e *Engine) SetRotation(rot, pw, ph int, rs *RotateStore) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rot, e.pw, e.ph, e.rotStore = rot, pw, ph, rs
	e.view = nil
	e.refresh()
}

// SetOnRotate は、向きが変わったときの通知先を設定する。
func (e *Engine) SetOnRotate(f func(int)) {
	e.mu.Lock()
	e.onRotate = f
	e.mu.Unlock()
}

// Rotation は今の画面の向き。
func (e *Engine) Rotation() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rot
}

// Rotate は、画面の向きを変える（rotateNext なら 90 度回す）。設定 GUI の set_rotate からも使う。
func (e *Engine) Rotate(r int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rotateLocked(r)
	e.refresh()
	return e.rot
}

func (e *Engine) rotateLocked(r int) {
	if r == rotateNext {
		r = (e.rot + 90) % 360
	}
	if r == e.rot || !validRotate(r) {
		return
	}
	e.rot = r
	e.pad.Cancel()
	e.rotStore.Save(r, e.km.Rotate)
	log.Printf("rotate: %d", r)
	if e.onRotate != nil {
		e.onRotate(r)
	}
}

// physSize は物理の画面の大きさ。
func (e *Engine) physSize() (int, int) {
	if e.pw == 0 {
		return 800, 480
	}
	return e.pw, e.ph
}

// touchMap は、今の向きでのタッチの座標の変換。ロックを持って呼ぶ。
func (e *Engine) touchMapLocked() touchMap {
	pw, ph := e.physSize()
	return touchMap{tc: e.km.Touch, rot: e.rot, pw: pw, ph: ph}
}

// portraitLocked は、今の向きが縦向きか。
func (e *Engine) portraitLocked() bool {
	pw, ph := e.physSize()
	return isPortrait(e.rot, pw, ph)
}

// barTop は、View で格子の上に空ける高さ（状態の帯）。
func (v *View) barTop() int {
	if v.Bar {
		return barH
	}
	return 0
}

// SetUSB は、USB の形の切り替えを設定する。
func (e *Engine) SetUSB(u *USBMode) {
	e.mu.Lock()
	e.usb = u
	e.mu.Unlock()
}

// SetTerm は、端末モードを設定する。
func (e *Engine) SetTerm(t *TermMode) {
	e.mu.Lock()
	e.term = t
	e.mu.Unlock()
}

// SetPad は、トラックパッドの判定を設定する。
func (e *Engine) SetPad(p *Pad) {
	e.mu.Lock()
	e.pad = p
	e.mu.Unlock()
}

// SetWidgets は、ウィジェットがタップを処理するのに使う状態を設定する。
func (e *Engine) SetWidgets(rt *WidgetRT) {
	e.mu.Lock()
	e.widgets = rt
	e.mu.Unlock()
}

func NewEngine(km *Keymap, out *State) *Engine {
	e := &Engine{km: km, out: out, down: map[string]*Action{}}
	e.startStack()
	e.refresh()
	return e
}

// startStack は、重なりを起動したときの形にする（ホーム画面から始めるなら、ホーム画面）。
func (e *Engine) startStack() {
	e.stack = nil
	if h := e.km.Home; h != nil && h.Start {
		e.stack = append(e.stack, stackEntry{layer: h.Layer, kind: actTo})
		h.SetPage(0)
	}
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

// PressKey は本体キーが押されたときに呼ぶ。端末モードのあいだは、レイヤーを通さずに端末に回す。
func (e *Engine) PressKey(code evdev.EvCode) {
	if e.term.Active() {
		e.term.KeyEvent(code, 1)
		return
	}
	if e.input.Active() {
		e.input.Key(code, 1) // 文字を入れる欄：PC には送らない
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.calcKeyLocked(code) {
		return
	}
	e.press(keyID(code), e.km.lookupKey(e.layers(), code))
}

// calcKeyLocked は、今の画面に電卓のセルがあり、キーが電卓の使うものなら、電卓に入れて true を返す。
// Ctrl か Alt を押しているあいだはショートカットなので、電卓に入れない。離したときは何もしない（down に nil を置く）。
func (e *Engine) calcKeyLocked(code evdev.EvCode) bool {
	if e.widgets == nil || e.widgets.calc == nil || e.view == nil {
		return false
	}
	var w *WidgetDef
	for _, a := range e.view.Cells {
		if a != nil && a.Widget != nil && a.Widget.Kind == widgetCalc {
			w = a.Widget
			break
		}
	}
	if w == nil {
		return false
	}
	held := func(cs ...evdev.EvCode) bool {
		for _, c := range cs {
			if _, ok := e.down[keyID(c)]; ok {
				return true
			}
		}
		return false
	}
	if held(evdev.KEY_LEFTCTRL, evdev.KEY_RIGHTCTRL, evdev.KEY_LEFTALT, evdev.KEY_RIGHTALT) {
		return false
	}
	in, ok := calcKeyInput(code, held(evdev.KEY_LEFTSHIFT, evdev.KEY_RIGHTSHIFT))
	if !ok {
		return false
	}
	id := keyID(code)
	if _, dup := e.down[id]; dup {
		return true
	}
	e.down[id] = nil
	e.widgets.calc.Submit(w.ID, in, w.Prefix)
	return true
}

// ReleaseKey は本体キーが離されたときに呼ぶ。端末モードに入る前に押したキーは、押したときの割り当てで離す。
func (e *Engine) ReleaseKey(code evdev.EvCode) {
	id := keyID(code)
	e.mu.Lock()
	_, mine := e.down[id]
	e.mu.Unlock()
	if !mine {
		if e.term.Active() {
			e.term.KeyEvent(code, 0)
		}
		e.input.Key(code, 0)
		return
	}
	e.Release(id)
}

// RepeatKey は、本体キーのオートリピート。端末モードのあいだだけ使う（HID は PC がリピートする）。
func (e *Engine) RepeatKey(code evdev.EvCode) {
	if e.term.Active() {
		e.term.KeyEvent(code, 2)
		return
	}
	if e.input.Active() {
		e.input.Key(code, 2)
	}
}

// SetInput は、文字を入れる欄の係を設定する。
func (e *Engine) SetInput(p *Prompt) {
	e.mu.Lock()
	e.input = p
	e.mu.Unlock()
}

// TouchHit はタッチの判定結果。
type TouchHit struct {
	Gen      uint64
	Soft     string // ソフトキーの名前（セルでないとき）
	Col, Row int
	Mapped   bool
	Own      bool // ウィジェットが押した位置で働き、自分で押したことを描く（セル全体を光らせない）
	Pad      bool // トラックパッドのセル。離すまでのサンプルを Engine.pad に渡す
}

// PressTouch はタッチの生座標で押す。判定は今の重なりの格子で行う。
func (e *Engine) PressTouch(x, y int32) TouchHit { return e.PressTouchAt(x, y, time.Now()) }

// PressTouchAt は、時刻 t に触れたとして押す（トラックパッドの判定に時刻を渡す。記録の再生では記録した時刻）。
func (e *Engine) PressTouchAt(x, y int32, t time.Time) TouchHit {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.term.Active() {
		// 端末モード：ソフトキーの区画は、割り当てによらず端末モードの働きにする（HOME で抜けるなど）。
		// 端末は設定の display.rotate の向きで描く
		soft, _ := e.km.hitSoft(x, y)
		g := e.term.geometry()
		W, H := g.W, g.H
		if W == 0 {
			W, H = 800, 480
		}
		pw, ph := e.physSize()
		m := touchMap{tc: e.km.Touch, rot: e.km.Rotate, pw: pw, ph: ph}
		pt := m.point(x, y)
		if mw, mh := m.size(); mw != W || mh != H {
			pt = touchPoint(e.km.Touch, x, y, W, H)
		}
		e.term.Touch(soft, pt)
		return TouchHit{Gen: e.view.Gen, Soft: soft, Col: -1, Row: -1, Own: true}
	}
	v := e.view
	h := TouchHit{Gen: v.Gen, Col: -1, Row: -1}
	var a *Action
	// ソフトキーの範囲は画面の右端と重なるので、今の重なりで割り当てがあるときだけ使う
	if name, ok := e.km.hitSoft(x, y); ok && v.Soft[name] != nil {
		h.Soft, a = name, v.Soft[name]
	} else if v.Cols > 0 {
		m := e.touchMapLocked()
		c, r, bar := m.cell(v.Cols, v.Rows, v.barTop(), x, y)
		if !bar {
			a, h.Col, h.Row = v.at(c, r) // span のセルは、左上のセルとして扱う
		}
		if a != nil && a.Widget.handlesTouch() {
			h.Own = a.Widget.ownsTouch()
			W, H := m.size()
			e.touchAt = &widgetTouch{cell: cellRectIn(h.Col, h.Row, a.SpanW, a.SpanH, v.Cols, v.Rows, gridArea(W, H, v.barTop())),
				pt: m.point(x, y)}
			switch {
			case a.Widget.Kind == widgetPad && e.offline:
				e.offlineNoticeLocked()
			case a.Widget.Kind == widgetPad && e.pad != nil:
				h.Pad = true
				e.pad.Down(a.Widget.Pad, e.touchAt.cell, m, t)
			case a.Widget.Kind == widgetHome:
				a = e.homeTouchLocked(a.Widget.home, e.touchAt.cell, e.touchAt.pt, v.Portrait)
			}
		}
	}
	if !h.Pad {
		e.pad.Interrupt() // トラックパッドのタップのあとの待ちを終える
	}
	h.Mapped = a.tappable()
	e.press("t", a)
	e.touchAt = nil
	return h
}

// homeTouchLocked は、ホーム画面の点 pt を押したときの割り当てを返す。アプリなら、そのレイヤーへの layer_to。
// ページ送りなら、ここでページを変えて nil を返す。
func (e *Engine) homeTouchLocked(hd *HomeDef, area image.Rectangle, pt image.Point, portrait bool) *Action {
	g := homeGeometry(area, hd, portrait, hd.Page())
	app, delta := homeHit(g, pt)
	switch {
	case app >= 0:
		a := hd.Apps[app]
		vlogf("home: %q -> layer %q", a.Name, e.km.Layers[a.Layer].Name)
		return &Action{Kind: actTo, Layer: a.Layer, SpanW: 1, SpanH: 1}
	case delta != 0:
		e.homePageLocked(hd, delta, portrait)
	}
	return nil
}

// homePageLocked は、ホーム画面のページを delta だけ送る。
func (e *Engine) homePageLocked(hd *HomeDef, delta int, portrait bool) {
	cols, rows := hd.grid(portrait)
	pages := max((len(hd.Apps)+cols*rows-1)/(cols*rows), 1)
	p := min(max(min(hd.Page(), pages-1)+delta, 0), pages-1)
	if p != hd.Page() {
		hd.SetPage(p)
		e.widgets.redraw()
	}
}

// Release は入力が離されたときに呼ぶ。押したときの割り当てで離すので、
// 押しているあいだにレイヤーが変わっても、キーが押しっぱなしにならない。
func (e *Engine) Release(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.down[id]
	if !ok {
		if id == "t" && e.term.Active() {
			e.term.TouchUp()
		}
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
		a.Widget.touchUp(e.widgets, false)
	case actMouse:
		e.out.mouse.Release(id)
	case actMedia:
		e.out.media.Release(id)
	}
}

// 呼び出し側でロック済みであること
func (e *Engine) press(id string, a *Action) {
	if _, dup := e.down[id]; dup {
		return
	}
	e.down[id] = a
	e.pressing = id
	defer func() { e.pressing = "" }()
	consumeOneshot := true
	if e.offline && a.needsPC() && a.Kind != actWidget {
		// PC から外れている：送らずに知らせる。離したときも何もしない
		e.down[id] = nil
		e.offlineNoticeLocked()
		a = nil
	}
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
			if h := e.km.Home; h != nil && a.Layer == h.Layer && e.view != nil && e.view.Top == h.Layer && h.Page() != 0 {
				h.SetPage(0) // ホーム画面で HOME を押したら 1 ページ目へ
				e.widgets.redraw()
			}
			e.stack = e.stack[:0]
			if a.Layer != 0 {
				e.stack = append(e.stack, stackEntry{layer: a.Layer, kind: actTo})
			}
			consumeOneshot = false
		case actMouse:
			if a.Mouse.Button != 0 {
				e.out.mouse.Press(id, a.Mouse.Button)
			} else {
				e.out.mouse.StartScroll(id, a.Mouse.V, a.Mouse.H)
			}
		case actMedia:
			e.out.media.Press(id, a.Media)
		case actUSB:
			if mouse, changed, err := e.usb.Request(a.Spec.UsbMode); err != nil {
				log.Printf("usb: %v", err)
			} else if changed {
				log.Printf("usb: switching to %s (the USB reconnects; keys pause for a few seconds)", map[bool]string{true: "keyboard + mouse", false: "keyboard only"}[mouse])
			}
		case actTerminal:
			if on, err := e.term.Request(a.Spec.Terminal); err != nil {
				log.Printf("terminal: %v", err)
			} else {
				log.Printf("terminal: %s requested by %s", map[bool]string{true: "on", false: "off"}[on], id)
			}
		case actRotate:
			e.rotateLocked(a.Rotate)
		case actInput:
			e.input.Open(a.Spec.Input)
		case actHomePage:
			if h := e.km.Home; h != nil {
				e.homePageLocked(h, a.Rotate, e.portraitLocked())
			}
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
	fmt.Fprintf(&b, "r%d o%v", e.rot, e.offline)
	key := b.String()
	if e.view != nil && e.view.stackKey == key {
		return
	}
	e.gen++
	v := e.km.view(e.layers(), e.portraitLocked())
	v.Gen, v.stackKey, v.Rot, v.Offline = e.gen, key, e.rot, e.offline
	if n := len(e.stack); n > 0 {
		v.Mode = modeLatched
		if k := e.stack[n-1].kind; k == actHold || k == actOneshot {
			v.Mode = modeTemp
		}
	}
	if h := e.km.Home; h != nil && v.Top == h.Layer {
		v.Mode = modeBase // ホーム画面は、出発点の色
	}
	e.view = v
	if e.gen > 1 {
		vlogf("layer: %s", e.describe())
		// レイヤーが変わったら、押しているマウスのボタンを離す（今押した入力のものは残す）。
		// トラックパッドのドラッグやタップの待ちもやめる
		e.out.mouse.ReleaseExcept(e.pressing)
		e.pad.Cancel()
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
	e.pad.Cancel()
	for _, a := range e.down { // 長押しの途中なら取り消す
		if a != nil && a.Kind == actWidget {
			a.Widget.touchUp(e.widgets, true)
		}
	}
	e.down = map[string]*Action{}
	e.km = km
	e.startStack()
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
		h.Col, h.Row, _ = e.touchMapLocked().cell(v.Cols, v.Rows, v.barTop(), x, y)
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
	// Rotate は画面の向き、Portrait は縦向きか（格子は縦向きのもの）
	Rotate   int  `json:"rotate"`
	Portrait bool `json:"portrait"`
	Bar      bool `json:"status_bar"`
	Home     bool `json:"home"` // ホーム画面を出している
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
		Stack: []StackStatus{}, Cols: v.Cols, Rows: v.Rows, Rotate: v.Rot, Portrait: v.Portrait, Bar: v.Bar, Home: top.home}
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
