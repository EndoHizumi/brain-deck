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
// lid.close が off（既定）なら、閉じたら画面を消し、タッチを受け付けない（Dimmer.SetLid）。画面の描き直しも止める
// （Display.SetPaused）。開けたら画面を点けて、全体を一度描き直し、自動減光の時間を数え直す。画面の中身（今のアプリ）は
// そのまま。閉じているあいだも時計、タイマー、同期などは動き続け（描かないだけ）、フォトフレームは出さない。none なら何もしない。
//
// タブレットの形（画面を裏返す）のあいだは、裏になったキーボードが押されることがあるので、本体のキーを PC にも
// brain-deck の操作にも使わない（lid.tablet_keys: off が既定。on なら使う）。タッチはそのまま使える。
// ただし PW-SH2 では、タブレットの形のあいだキーボードがキーを送らない（2026-10-15 に実機で確かめた。割り込みも
// 起きない）ので、これは別のハードや将来のドライバのための網。設定のアプリと上書きでは変えられない（設定ファイルだけ）。

const (
	lidOff  = "off"  // 閉じたら画面を消す（既定）
	lidNone = "none" // 何もしない

	tabletKeysOff = "off" // タブレットの形のあいだ、本体のキーを使わない（既定）
	tabletKeysOn  = "on"  // タブレットの形でも、本体のキーを使う
)

// LidConfig は設定の lid。
type LidConfig struct {
	Close      string `yaml:"close,omitempty" json:"close,omitempty"`             // off（既定）か none
	TabletKeys string `yaml:"tablet_keys,omitempty" json:"tablet_keys,omitempty"` // off（既定）か on
}

// tabletKeys は、タブレットの形のときに本体のキーを使うか。
func (c *LidConfig) tabletKeys() string {
	if c == nil || c.TabletKeys == "" {
		return tabletKeysOff
	}
	return c.TabletKeys
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
	switch c.TabletKeys {
	case "", tabletKeysOff, tabletKeysOn:
	default:
		fail("/lid/tablet_keys", "lid.tablet_keys must be off (ignore the Brain keys in tablet mode) or on (use them), got %q", c.TabletKeys)
	}
}

// lidPauser は、画面の描き直しを止める係（Display）。
type lidPauser interface{ SetPaused(bool) }

// Lid は、ふたとタブレットの形の状態。
type Lid struct {
	mu       sync.Mutex
	closed   bool
	tablet   bool
	action   string
	keys     string // タブレットの形のときの本体のキー（tabletKeysOff か tabletKeysOn）
	since    time.Time
	dim      *Dimmer
	disp     lidPauser
	onChange func() // 状態が変わったとき（画面の描き直しなど）。待たずに返ること
}

// NewLid は、ふたの係を作る。dim が nil なら、画面は消さない。
func NewLid(dim *Dimmer, c *LidConfig) *Lid {
	return &Lid{dim: dim, action: c.action(), keys: c.tabletKeys(), since: time.Now()}
}

// SetDisplay は、閉じているあいだ描き直しを止める画面を設定する（今の状態をすぐ伝える）。
func (l *Lid) SetDisplay(d lidPauser) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.disp = d
	l.applyLocked()
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
		if l.keys == tabletKeysOff {
			log.Printf("lid: tablet mode %v (the Brain keys are %s)", on, map[bool]string{true: "ignored", false: "used again"}[on])
		} else {
			log.Printf("lid: tablet mode %v (lid.tablet_keys: on)", on)
		}
	default:
		vlogf("lid: switch %d = %d", code, value)
		return
	}
	if f := l.onChange; f != nil {
		go f()
	}
}

// applyLocked は、今の状態と動きを Dimmer と画面に伝える。
func (l *Lid) applyLocked() {
	off := l.closed && l.action == lidOff
	// 開けるときは、先に描き直しを始めてから点ける（古い画面が見える時間を短くする）
	if l.disp != nil && !off {
		l.disp.SetPaused(false)
	}
	l.dim.SetLid(off)
	if l.disp != nil && off {
		l.disp.SetPaused(true)
	}
}

// SetConfig は、閉じたときの動きとタブレットの形のキーを変える（設定の反映、上書き）。閉じているあいだに変えたら、すぐ合わせる。
func (l *Lid) SetConfig(c *LidConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = c.tabletKeys()
	if a := c.action(); a != l.action {
		l.action = a
		l.applyLocked()
	}
}

// SetAction は、閉じたときの動きだけを変える。
func (l *Lid) SetAction(a string) {
	l.mu.Lock()
	keys := l.keys
	l.mu.Unlock()
	l.SetConfig(&LidConfig{Close: a, TabletKeys: keys})
}

// KeysBlocked は、本体のキーを使わないとき（タブレットの形で、lid.tablet_keys が off）。
func (l *Lid) KeysBlocked() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tablet && l.keys == tabletKeysOff
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
	Closed     bool      `json:"closed"`
	Tablet     bool      `json:"tablet"`           // 画面を裏返したタブレットの形
	TabletKeys string    `json:"tablet_keys"`      // タブレットの形のときの本体のキー（off か on）
	KeysOff    bool      `json:"keys_off"`         // 今、本体のキーを使っていない
	Close      string    `json:"close"`            // 閉じたときの動き（off か none）
	Since      time.Time `json:"since,omitzero"`   // 今の開け閉めになった時刻
	Screen     string    `json:"screen,omitempty"` // 閉じていて画面を消しているときは off
}

func (l *Lid) Info() LidInfo {
	if l == nil {
		return LidInfo{Close: lidOff, TabletKeys: tabletKeysOff}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	in := LidInfo{Closed: l.closed, Tablet: l.tablet, TabletKeys: l.keys, KeysOff: l.tablet && l.keys == tabletKeysOff,
		Close: l.action, Since: l.since}
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
