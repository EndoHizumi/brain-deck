package main

import (
	"log"
	"path/filepath"
	"sync"
	"time"
)

// ---------- PC とのつながり ----------
//
// UDC の state（configured、suspended、not attached など。wakeup.go の USBState）と、USB の 5V（LRADC の ch15。battery.go）を
// 2 秒ごとに読み、PC とつながっているか、電源だけか、外れているかを見分ける（状態の帯の「PC」と、外したときの動き）。
//
//	UDC の state            5V     つながり（Link）
//	configured              -      configured（PC とつながっている）
//	suspended               -      suspended（PC がスリープ中。つながっているとみなし、キーで起こせる）
//	それ以外                 ある    power（電源だけ：充電器、電源の入っていない PC、USB の付け直しの途中）
//	それ以外                 ない    not attached（外れている）
//	それ以外                 不明    UDC の state のまま（not attached なら外れている）
//	（UDC がない）           -      空（分からない。外れたとはみなさない）
//
// configured と suspended のほかを「外れている」（offline）とみなす。USB の形の切り替えや端末モードでは、
// USB を付け直すあいだ数秒 configured でなくなるので、5V があるあいだは linkOfflineAfter 続いてから外れたとみなす。
// 5V がなくなったら（ケーブルを抜いた）すぐに外れたとみなす。つながったら、すぐにつながったとみなす。

const (
	linkPoll         = 2 * time.Second
	linkOfflineAfter = 8 * time.Second // 5V があるまま configured でなくなってから、外れたとみなすまで

	linkPower = "power" // 電源だけ（5V はあるが、PC と通信していない）
)

// linkOnline は、つながり（Link）が PC とつながっている（スリープ中を含む）か。分からないときもつながっているとみなす。
func linkOnline(link string) bool {
	return link == "" || link == "configured" || link == "suspended"
}

// classifyLink は、UDC の state と USB の 5V（mV。負なら分からない）から、つながりを決める。
// vbus は要るときだけ読む（LRADC の変換に 2 ミリ秒ほどかかるので、PC とつながっているあいだは読まない）。
func classifyLink(state string, vbus func() int) string {
	switch state {
	case "", "configured", "suspended":
		return state
	}
	switch vbusMV := vbus(); {
	case vbusMV < 0:
		return state
	case vbusMV > vdd5vPresent:
		return linkPower
	}
	return "not attached"
}

// LinkMonitor は、PC とのつながりを見張る。
type LinkMonitor struct {
	mu        sync.Mutex
	raw       string    // 今読んだつながり
	state     string    // 知らせているつながり（外れたとみなすのを linkOfflineAfter 待つ）
	since     time.Time // raw が外れた状態になった時刻
	onChange  func()
	listeners []func(online bool)
	read      func() string
	vbus      func() int // USB の 5V（mV）。負なら分からない
	now       func() time.Time
}

// NewLinkMonitor は、見張りを始める。sysRoot は 5V を読む /sys。
func NewLinkMonitor(sysRoot string) *LinkMonitor {
	m := newLinkMonitor(USBState, findVBus(sysRoot), time.Now)
	log.Printf("link: %q (usb %q)", m.state, m.raw)
	go func() {
		for range time.Tick(linkPoll) {
			m.poll()
		}
	}()
	return m
}

// newLinkMonitor は、見張りを作る（読み始めない。テストで使う）。起動したときのつながりは、待たずにそのまま使う。
func newLinkMonitor(read func() string, vbus func() int, now func() time.Time) *LinkMonitor {
	m := &LinkMonitor{read: read, vbus: vbus, now: now}
	m.raw = classifyLink(read(), vbus)
	m.state = m.raw
	m.since = now()
	return m
}

func (m *LinkMonitor) poll() {
	raw := classifyLink(m.read(), m.vbus)
	now := m.now()
	m.mu.Lock()
	if linkOnline(raw) != linkOnline(m.raw) {
		m.since = now
	}
	m.raw = raw
	next := raw
	if !linkOnline(raw) && linkOnline(m.state) && raw == linkPower && now.Sub(m.since) < linkOfflineAfter {
		next = m.state // USB の付け直しかもしれない。もう少し待つ
	}
	changed := next != m.state
	wasOnline := linkOnline(m.state)
	m.state = next
	f, ls := m.onChange, m.listeners
	m.mu.Unlock()
	if !changed {
		return
	}
	if online := linkOnline(next); online != wasOnline {
		log.Printf("link: %q (%s)", next, map[bool]string{true: "connected to the PC", false: "not connected to the PC"}[online])
		for _, l := range ls {
			l(online)
		}
	} else {
		vlogf("link: %q", next)
	}
	if f != nil {
		f()
	}
}

// State は、今のつながり（configured、suspended、power、not attached など）。分からなければ空。
func (m *LinkMonitor) State() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Online は、PC とつながっている（スリープ中を含む。分からないときも）か。
func (m *LinkMonitor) Online() bool { return linkOnline(m.State()) }

// SetOnChange は、変わったときに呼ぶ関数（描き直し）を設定する。待たずに返ること。
func (m *LinkMonitor) SetOnChange(f func()) {
	m.mu.Lock()
	m.onChange = f
	m.mu.Unlock()
}

// OnOnline は、つながった・外れたときに呼ぶ関数を足す。待たずに返ること。
func (m *LinkMonitor) OnOnline(f func(online bool)) {
	m.mu.Lock()
	m.listeners = append(m.listeners, f)
	m.mu.Unlock()
}

// findVBus は、USB の 5V（mV）を読む関数を返す。LRADC がなければ、いつも -1。
func findVBus(sysRoot string) func() int {
	iio, _ := filepath.Glob(filepath.Join(sysRoot, "bus/iio/devices/iio:device*"))
	for _, d := range iio {
		if readSysString(filepath.Join(d, "name")) == lradcName {
			r := &lradcReader{dir: d}
			return func() int {
				v, err := r.channel(lradcVDD5V)
				if err != nil {
					return -1
				}
				return v
			}
		}
	}
	return func() int { return -1 }
}
