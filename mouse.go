package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- マウスの出力 ----------
//
// マウスのレポート（hid.go の hidDescCombo）は 6 バイト：ID、ボタン、X、Y、縦のホイール、横のホイール。
// ボタンは、押している入力（キー、セル、トラックパッド）ごとに覚え、その OR を送る。
// 送れなかったボタンの状態は、State.retryLoop で送り直す（離したのが届かず押しっぱなしになるのを防ぐ）。
// 移動とホイールは、送れなければ捨てる（あとから送ると、思わぬところへ飛ぶため）。

const (
	mouseLeft   byte = 1
	mouseRight  byte = 2
	mouseMiddle byte = 4
)

// マウスの割り当て（mouse: の値）
type mouseAction struct {
	Button byte // ボタン。0 ならスクロール
	V, H   int  // スクロール（縦は上が正、横は右が正）
	Pretty string
}

var mouseActions = map[string]mouseAction{
	"left":         {Button: mouseLeft, Pretty: "左クリック"},
	"right":        {Button: mouseRight, Pretty: "右クリック"},
	"middle":       {Button: mouseMiddle, Pretty: "中クリック"},
	"scroll_up":    {V: 1, Pretty: "スクロール↑"},
	"scroll_down":  {V: -1, Pretty: "スクロール↓"},
	"scroll_left":  {H: -1, Pretty: "スクロール←"},
	"scroll_right": {H: 1, Pretty: "スクロール→"},
}

var mouseActionNames = []string{"left", "right", "middle", "scroll_up", "scroll_down", "scroll_left", "scroll_right"}

func parseMouse(s string) (mouseAction, error) {
	a, ok := mouseActions[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return a, fmt.Errorf("unknown mouse action %q (%s)", s, strings.Join(mouseActionNames, ", "))
	}
	return a, nil
}

// スクロールのキーを押し続けたときの繰り返し
var (
	scrollRepeatDelay = 350 * time.Millisecond
	scrollRepeatEvery = 70 * time.Millisecond
)

// hidOut は HID のレポートを書く先（*HIDWriter。テストでは記録するもの）。
type hidOut interface {
	Write(r []byte) error
}

// Mouse はマウスのレポートを送る。nil でもよい（何もしない）。
type Mouse struct {
	mu      sync.Mutex
	hid     hidOut
	id      byte
	held    map[string]byte // 入力 → 押しているボタン
	sent    byte            // PC に届いたボタンの状態
	dirty   bool            // ボタンの状態が PC に届いていない
	repeats map[string]*time.Timer
	on      atomic.Bool // id が 0 でない（ロックなしで読む。USB の形を切り替えているあいだも待たない）
}

// NewMouse は、レポート ID が id のマウスを作る。id が 0（ガジェットにマウスがない）なら何も送らない。
func NewMouse(hid hidOut, id byte) *Mouse {
	m := &Mouse{hid: hid, id: id, held: map[string]byte{}, repeats: map[string]*time.Timer{}}
	m.on.Store(id != 0)
	return m
}

// Available は、PC にマウスとして送れるか（ガジェットにマウスがあるか）。
func (m *Mouse) Available() bool { return m != nil && m.on.Load() }

func (m *Mouse) buttons() byte {
	var b byte
	for _, v := range m.held {
		b |= v
	}
	return b
}

// Press は、入力 src でボタンを押す。
func (m *Mouse) Press(src string, b byte) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held[src] = b
	m.sendButtons()
}

// Release は、入力 src で押したボタンを離し、スクロールの繰り返しを止める。
func (m *Mouse) Release(src string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopRepeat(src)
	if _, ok := m.held[src]; !ok {
		return
	}
	delete(m.held, src)
	m.sendButtons()
}

// ReleaseAll は、すべてのボタンを離し、スクロールの繰り返しを止める（終了、設定の再読み込み）。
func (m *Mouse) ReleaseAll() { m.ReleaseExcept("") }

// ReleaseExcept は、src 以外で押しているボタンをすべて離す（レイヤーの切り替え）。
// src が押したボタンと繰り返しは残す（今押した入力が、レイヤーを切り替えたとき）。
func (m *Mouse) ReleaseExcept(src string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.repeats {
		if k != src {
			m.stopRepeat(k)
		}
	}
	n := len(m.held)
	for k := range m.held {
		if k != src {
			delete(m.held, k)
		}
	}
	if n != len(m.held) || m.sent != m.buttons() || m.dirty {
		m.sendButtons()
	}
}

// Held は、今押しているボタン（テスト用）。
func (m *Mouse) Held() byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buttons()
}

// Move はカーソルを動かす（PC のマウスの単位）。
func (m *Mouse) Move(dx, dy int) {
	if m == nil || (dx == 0 && dy == 0) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.send(dx, dy, 0, 0)
}

// Wheel はホイールを回す。v は上が正、h は右が正（1 が 1 段）。
func (m *Mouse) Wheel(v, h int) {
	if m == nil || (v == 0 && h == 0) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.send(0, 0, v, h)
}

// StartScroll は、1 段回し、押しているあいだ繰り返す。Release(src) で止まる。
func (m *Mouse) StartScroll(src string, v, h int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopRepeat(src)
	m.send(0, 0, v, h)
	var tick func()
	var t *time.Timer
	tick = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.repeats[src] != t {
			return // 止めたあと
		}
		m.send(0, 0, v, h)
		t.Reset(scrollRepeatEvery)
	}
	t = time.AfterFunc(scrollRepeatDelay, tick)
	m.repeats[src] = t
}

// m.mu を持って呼ぶ
func (m *Mouse) stopRepeat(src string) {
	if t, ok := m.repeats[src]; ok {
		t.Stop()
		delete(m.repeats, src)
	}
}

// retry は、届いていないボタンの状態を送り直す（State.retryLoop から）。
func (m *Mouse) retry() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dirty {
		m.sendButtons()
	}
}

// m.mu を持って呼ぶ
func (m *Mouse) sendButtons() { m.send(0, 0, 0, 0) }

// send は、今のボタンと、移動とホイールを送る。1 つのレポートに入らない大きさは分けて送る。
// m.mu を持って呼ぶ。
func (m *Mouse) send(dx, dy, v, h int) {
	if m.id == 0 {
		return
	}
	b := m.buttons()
	for first := true; first || dx != 0 || dy != 0 || v != 0 || h != 0; first = false {
		cx, cy, cv, ch := clamp127(dx), clamp127(dy), clamp127(v), clamp127(h)
		dx, dy, v, h = dx-cx, dy-cy, v-cv, h-ch
		r := []byte{m.id, b, byte(int8(cx)), byte(int8(cy)), byte(int8(cv)), byte(int8(ch))}
		err := m.hid.Write(r)
		if err != nil {
			m.dirty = true
			vlogf("mouse report % x (not sent: %v)", r, err)
			return
		}
		m.sent, m.dirty = b, false
		vlogf("mouse report % x", r)
	}
}

func clamp127(v int) int { return max(-127, min(127, v)) }
