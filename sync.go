package main

import (
	"log"
	"sync"
	"time"
)

// ---------- 同期（brain-deck sync） ----------
//
// PC の brain-deck sync が、始めるときに set_sync（state: start）、終わったときに set_sync（state: done、ok、summary、steps）を
// 送る。Brain は、同期しているあいだ状態の帯に「同期中」と出し、終わったら最後に同期した時刻を出す（失敗があれば橙）。
// 始めと終わりには、画面の下に知らせも出す。最後の同期の結果は sync.json に残す（get_status の sync）。
// done が来ないまま syncMaxRun たったら（PC の brain-deck が止まったなど）、「同期中」をやめる。

const (
	syncStoreName = "sync"
	syncMaxRun    = 10 * time.Minute
)

// SyncStep は、同期の 1 つの段階の結果。
type SyncStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

// SyncRecord は、最後の同期（sync.json）。
type SyncRecord struct {
	At      time.Time  `json:"at"`
	OK      bool       `json:"ok"`
	Summary string     `json:"summary,omitempty"`
	Steps   []SyncStep `json:"steps,omitempty"`
	Source  string     `json:"source,omitempty"`
}

// SyncService は、同期の状態。
type SyncService struct {
	mu       sync.Mutex
	store    *Store
	running  bool
	started  time.Time
	timer    *time.Timer
	last     *SyncRecord
	onChange func()
	notify   func(msg string, ok bool)
}

func NewSyncService(store *Store) *SyncService {
	s := &SyncService{store: store}
	var r SyncRecord
	if err := store.Load(syncStoreName, &r); err == nil && !r.At.IsZero() {
		s.last = &r
	}
	return s
}

func (s *SyncService) SetOnChange(f func()) { s.mu.Lock(); s.onChange = f; s.mu.Unlock() }
func (s *SyncService) SetNotify(f func(msg string, ok bool)) {
	s.mu.Lock()
	s.notify = f
	s.mu.Unlock()
}

// Start は、同期を始めたとき。
func (s *SyncService) Start(source string) {
	s.mu.Lock()
	s.running, s.started = true, time.Now()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(syncMaxRun, func() {
		s.mu.Lock()
		was := s.running
		s.running = false
		f := s.onChange
		s.mu.Unlock()
		if was {
			log.Printf("sync: no result for %v; stopped showing 同期中", syncMaxRun)
			if f != nil {
				f()
			}
		}
	})
	f, n := s.onChange, s.notify
	s.mu.Unlock()
	log.Printf("sync: started by %s", source)
	if n != nil {
		n("同期しています…", true)
	}
	if f != nil {
		f()
	}
}

// Done は、同期が終わったとき。結果を残す。
func (s *SyncService) Done(r SyncRecord) {
	r.At = time.Now().UTC()
	s.mu.Lock()
	s.running = false
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.last = &r
	f, n := s.onChange, s.notify
	s.mu.Unlock()
	go func() {
		if err := s.store.Save(syncStoreName, r); err != nil {
			log.Printf("sync: %v", err)
		}
	}()
	log.Printf("sync: done (ok %v) %s", r.OK, r.Summary)
	if n != nil {
		msg := "同期しました"
		if !r.OK {
			msg = "同期しました（失敗したものがあります）"
		}
		if r.Summary != "" {
			msg += "：" + r.Summary
		}
		n(msg, r.OK)
	}
	if f != nil {
		f()
	}
}

// SyncInfo は get_status の sync。
type SyncInfo struct {
	Running bool        `json:"running"`
	Started *time.Time  `json:"started,omitempty"`
	Last    *SyncRecord `json:"last,omitempty"`
}

func (s *SyncService) Info() SyncInfo {
	if s == nil {
		return SyncInfo{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in := SyncInfo{Running: s.running, Last: s.last}
	if s.running {
		t := s.started
		in.Started = &t
	}
	return in
}

// envFields は、状態の帯に出すもの（同期しているか、最後に同期した時刻、失敗があったか）。
func (s *SyncService) envFields() (running bool, last *time.Time, failed bool) {
	if s == nil {
		return false, nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != nil {
		t := s.last.At
		last, failed = &t, !s.last.OK
	}
	return s.running, last, failed
}
