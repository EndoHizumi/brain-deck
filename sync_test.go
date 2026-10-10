package main

import (
	"strings"
	"testing"
	"time"
)

func TestSyncServiceAndBar(t *testing.T) {
	store := OpenStore(t.TempDir())
	s := NewSyncService(store)
	var notes []string
	s.SetNotify(func(m string, ok bool) { notes = append(notes, m) })
	s.Start("test")
	running, last, _ := s.envFields()
	if !running || last != nil {
		t.Fatal("start")
	}
	now := time.Date(2026, 10, 11, 9, 41, 0, 0, time.Local)
	items := barItems([]string{barSync}, WidgetEnv{Now: now, Syncing: true})
	if len(items) != 1 || items[0].text != "同期中" {
		t.Errorf("bar %+v", items)
	}
	s.Done(SyncRecord{OK: false, Summary: "4 件を行った、1 件が失敗", Steps: []SyncStep{{Name: "天気", OK: false}}})
	running, last, failed := s.envFields()
	if running || last == nil || !failed {
		t.Fatal("done")
	}
	at := last.In(time.Local)
	items = barItems([]string{barSync}, WidgetEnv{Now: at, LastSync: &at, SyncFailed: true})
	if items[0].text != "同期"+at.Format("15:04") || items[0].ink != colInkWarn {
		t.Errorf("bar after a failed sync %+v", items)
	}
	if len(notes) != 2 || !strings.Contains(notes[1], "失敗したものがあります") {
		t.Errorf("notes %q", notes)
	}
	// sync.json に残り、開き直しても同じ
	time.Sleep(50 * time.Millisecond)
	s2 := NewSyncService(store)
	if in := s2.Info(); in.Running || in.Last == nil || in.Last.OK || in.Last.Summary == "" {
		t.Errorf("reload %+v", in)
	}
}

func TestSetTimeLogsRTC(t *testing.T) {
	oldR, oldW, oldS := readRTC, writeRTC, setSystemClock
	defer func() { readRTC, writeRTC, setSystemClock = oldR, oldW, oldS }()
	rtcNow := time.Now().Add(-3 * time.Second) // RTC は 3 秒遅れている
	readRTC = func() (RTCReading, error) {
		return RTCReading{Time: rtcNow.Truncate(time.Second), ReadAt: time.Now()}, nil
	}
	wrote := make(chan time.Time, 1)
	writeRTC = func(t time.Time) error { wrote <- t; return nil }
	setSystemClock = func(time.Time) error { return nil }
	ts := NewTimeService(OpenStore(t.TempDir()))
	ts.SetRTCMode(rtcWrite)
	res, err := ts.Set(time.Now(), "brain-deck")
	if err != nil {
		t.Fatal(err)
	}
	if res.RTC == nil || res.RTC.OffsetMS > -2000 || res.RTC.OffsetMS < -4100 || !res.RTCWrite {
		t.Fatalf("rtc sample %+v", res.RTC)
	}
	<-wrote
	time.Sleep(20 * time.Millisecond)
	in := ts.RTCInfo()
	if len(in.Log) != 1 || in.WrittenAt == nil {
		t.Fatalf("log %+v", in)
	}
	// 2 回目は、前に書いてからの時間を残す。Brain のキーボードで合わせたときは測らない
	time.Sleep(1100 * time.Millisecond)
	res, _ = ts.Set(time.Now(), "brain-deck")
	if res.RTC == nil || res.RTC.SinceSec < 1 {
		t.Errorf("since write %+v", res.RTC)
	}
	<-wrote
	if res, _ := ts.Set(time.Now(), sourceBrain); res.RTC != nil {
		t.Error("the Brain keyboard measured the RTC")
	}
	<-wrote
}
