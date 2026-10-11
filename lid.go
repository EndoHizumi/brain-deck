package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- ふたの開け閉め（設定の lid） ----------
//
// 2026-10-11 に Brain（PW-SH2）で調べた結果：キーボード（brain-kbd-i2c）が、ふたを閉じると EV_SW SW_LID 1、開けると 0 を出す。
// 半分まで閉じたときは何も出ない。画面を裏返してタブレットの形にすると SW_TABLET_MODE 1、戻すと 0。どちらもタッチやキーは出ない。
// ドライバは起動したときの状態を読まない（変わったときだけ知らせる）ので、電源を入れたときは開いているとみなす
// （デーモンを起動し直したときは、カーネルが覚えている状態を EVIOCGSW で読む）。
//
// lid.close が off（既定）なら、閉じたら画面を消し、タッチを受け付けない（Dimmer.SetLid）。開けたら画面を点けて、
// 自動減光の時間を数え直す。画面の中身（今のアプリ）はそのまま。閉じているあいだも時計、タイマー、同期などは動き続け、
// フォトフレームは出さない。none なら何もしない。

const (
	lidOff  = "off"  // 閉じたら画面を消す（既定）
	lidNone = "none" // 何もしない
)

// LidConfig は設定の lid。
type LidConfig struct {
	Close string `yaml:"close,omitempty" json:"close,omitempty"` // off（既定）か none
}

// action は、閉じたときの動き。
func (c *LidConfig) action() string {
	if c == nil || c.Close == "" {
		return lidOff
	}
	return c.Close
}

func (c *LidConfig) validate(fail func(path, format string, args ...any)) {
	if c == nil {
		return
	}
	switch c.Close {
	case "", lidOff, lidNone:
	default:
		fail("/lid/close", "lid.close must be off (turn the screen off) or none (do nothing), got %q", c.Close)
	}
}

// Lid は、ふたとタブレットの形の状態。
type Lid struct {
	mu       sync.Mutex
	closed   bool
	tablet   bool
	action   string
	since    time.Time
	dim      *Dimmer
	onChange func() // 状態が変わったとき（画面の描き直しなど）。待たずに返ること
}

// NewLid は、ふたの係を作る。dim が nil なら、画面は消さない。
func NewLid(dim *Dimmer, action string) *Lid {
	return &Lid{dim: dim, action: action, since: time.Now()}
}

// Init は、キーボードのデバイスから今のスイッチの状態を読む（デーモンを起動し直したとき）。
func (l *Lid) Init(dev *evdev.InputDevice) {
	if dev == nil {
		return
	}
	st, err := dev.State(evdev.EV_SW)
	if err != nil {
		return
	}
	if st[evdev.SW_TABLET_MODE] {
		l.Switch(evdev.SW_TABLET_MODE, 1)
	}
	if st[evdev.SW_LID] {
		l.Switch(evdev.SW_LID, 1)
	}
}

// SetOnChange は、状態が変わったときに呼ぶ関数を設定する。
func (l *Lid) SetOnChange(f func()) {
	l.mu.Lock()
	l.onChange = f
	l.mu.Unlock()
}

// Switch は、キーボードの EV_SW のイベント。
func (l *Lid) Switch(code evdev.EvCode, value int32) {
	on := value != 0
	l.mu.Lock()
	defer l.mu.Unlock()
	switch code {
	case evdev.SW_LID:
		if l.closed == on {
			return
		}
		l.closed, l.since = on, time.Now()
		what := map[bool]string{true: "closed", false: "opened"}[on]
		if l.action == lidOff {
			log.Printf("lid: %s (the screen is turned %s)", what, map[bool]string{true: "off", false: "on"}[on])
		} else {
			log.Printf("lid: %s (lid.close: %s)", what, l.action)
		}
		l.applyLocked()
	case evdev.SW_TABLET_MODE:
		if l.tablet == on {
			return
		}
		l.tablet = on
		log.Printf("lid: tablet mode %v", on)
	default:
		vlogf("lid: switch %d = %d", code, value)
		return
	}
	if f := l.onChange; f != nil {
		go f()
	}
}

// applyLocked は、今の状態と動きを Dimmer に伝える。
func (l *Lid) applyLocked() {
	l.dim.SetLid(l.closed && l.action == lidOff)
}

// SetAction は、閉じたときの動きを変える（設定の反映、上書き）。閉じているあいだに変えたら、すぐ合わせる。
func (l *Lid) SetAction(a string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a == l.action {
		return
	}
	l.action = a
	l.applyLocked()
}

// Closed は、ふたが閉じているか。
func (l *Lid) Closed() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// LidInfo は get_status の lid。
type LidInfo struct {
	Closed bool      `json:"closed"`
	Tablet bool      `json:"tablet"`           // 画面を裏返したタブレットの形
	Close  string    `json:"close"`            // 閉じたときの動き（off か none）
	Since  time.Time `json:"since,omitzero"`   // 今の開け閉めになった時刻
	Screen string    `json:"screen,omitempty"` // 閉じていて画面を消しているときは off
}

func (l *Lid) Info() LidInfo {
	if l == nil {
		return LidInfo{Close: lidOff}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	in := LidInfo{Closed: l.closed, Tablet: l.tablet, Close: l.action, Since: l.since}
	if l.closed && l.action == lidOff {
		in.Screen = "off"
	}
	return in
}

// lidLabel は、閉じたときの動きの日本語の名前（設定のアプリ、brain-deck）。
func lidLabel(a string) string {
	switch a {
	case lidNone:
		return "何もしない"
	case lidOff, "":
		return "画面を消す"
	}
	return fmt.Sprintf("%q", a)
}
