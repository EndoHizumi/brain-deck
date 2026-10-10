package main

import (
	"log"
	"sync"
	"time"
)

// ---------- PC とのつながり ----------
//
// UDC の state（configured、suspended、not attached など。wakeup.go の USBState）を 2 秒ごとに読み、
// 変わったら知らせる（状態の帯の「PC」）。

const linkPoll = 2 * time.Second

// LinkMonitor は、PC とのつながりを見張る。
type LinkMonitor struct {
	mu       sync.Mutex
	state    string
	onChange func()
	read     func() string
}

// NewLinkMonitor は、見張りを始める。
func NewLinkMonitor() *LinkMonitor {
	m := &LinkMonitor{read: USBState}
	m.state = m.read()
	log.Printf("link: usb %q", m.state)
	go func() {
		for range time.Tick(linkPoll) {
			m.poll()
		}
	}()
	return m
}

func (m *LinkMonitor) poll() {
	s := m.read()
	m.mu.Lock()
	changed := s != m.state
	m.state = s
	f := m.onChange
	m.mu.Unlock()
	if changed {
		vlogf("link: usb %q", s)
		if f != nil {
			f()
		}
	}
}

// State は、今の UDC の state。分からなければ空。
func (m *LinkMonitor) State() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// SetOnChange は、変わったときに呼ぶ関数を設定する。待たずに返ること。
func (m *LinkMonitor) SetOnChange(f func()) {
	m.mu.Lock()
	m.onChange = f
	m.mu.Unlock()
}
