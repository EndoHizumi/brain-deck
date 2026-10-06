package main

import (
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- 時刻合わせ ----------
//
// Brain（PW-SH2）には使える RTC がない。起動時の時刻は、fake-hwclock（1 時間ごとに保存）と
// systemd-timesyncd が保存した時刻から戻すので、電源を切っていたあいだの分だけ遅れる。
// 設定 GUI は接続したときに PC の時刻を set_time で送り、デーモンがシステムの時刻を合わせる。
//
// 「この起動のあいだに合わせたか」は、合わせたときの boot_id を /var/lib/lefthand/clock.json に残して判断する。
// デーモンを再起動しても覚えていて、Brain を再起動すると未設定に戻る。
// NTP（timesyncd）で合っているときも、合わせたものとみなす（カーネルの STA_UNSYNC を見る）。

const (
	clockStoreName = "clock"
	stepThreshold  = 500 * time.Millisecond // これより小さいずれは、時刻を動かさない
	staUnsync      = 0x0040                 // adjtimex の status。NTP で合わせていない
)

// 受け付ける時刻の範囲。PC の時計が明らかにおかしいときに、Brain を巻き込まない
var (
	minSettable = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	maxSettable = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// テストで差し替える
var (
	setSystemClock = func(t time.Time) error {
		tv := syscall.NsecToTimeval(t.UnixNano())
		return syscall.Settimeofday(&tv)
	}
	ntpSynced = func() bool {
		var tx syscall.Timex
		state, err := syscall.Adjtimex(&tx)
		return err == nil && state != 5 /* TIME_ERROR */ && tx.Status&staUnsync == 0
	}
	readBootID = func() string {
		b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
		return strings.TrimSpace(string(b))
	}
	timezoneName = func() string {
		if tz := os.Getenv("TZ"); tz != "" {
			return strings.TrimPrefix(tz, ":")
		}
		p, err := os.Readlink("/etc/localtime")
		if err != nil {
			return ""
		}
		if i := strings.Index(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
		return p
	}
)

// clockRecord は、最後に時刻を合わせたときの記録（clock.json）。
type clockRecord struct {
	BootID   string    `json:"boot_id"`
	SetAt    time.Time `json:"set_at"`
	OffsetMS int64     `json:"offset_ms"` // 合わせる前の Brain の時刻のずれ（PC − Brain）
	Source   string    `json:"source"`
}

// TimeService は時刻合わせと、時刻が合っているかどうかを受け持つ。
type TimeService struct {
	mu       sync.Mutex
	saveMu   sync.Mutex // clock.json の書き込みを 1 つずつ行う
	store    *Store
	bootID   string
	last     *clockRecord
	onChange func() // 時刻を合わせたとき（時計の描き直し）。待たずに返ること
}

func NewTimeService(store *Store) *TimeService {
	ts := &TimeService{store: store, bootID: readBootID()}
	var r clockRecord
	if err := store.Load(clockStoreName, &r); err == nil {
		ts.last = &r
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("time: %v", err)
	}
	return ts
}

// SetOnChange は、時刻を合わせたときに呼ぶ関数を設定する。
func (ts *TimeService) SetOnChange(f func()) {
	ts.mu.Lock()
	ts.onChange = f
	ts.mu.Unlock()
}

// Synced は、この起動のあいだに時刻を合わせたかどうか。
func (ts *TimeService) Synced() bool {
	if ts == nil {
		return true
	}
	ts.mu.Lock()
	set := ts.setThisBootLocked()
	ts.mu.Unlock()
	return set || ntpSynced()
}

func (ts *TimeService) setThisBootLocked() bool {
	return ts.last != nil && ts.bootID != "" && ts.last.BootID == ts.bootID
}

// Env は、ウィジェットを描くときの状態を返す。
func (ts *TimeService) Env() WidgetEnv {
	return WidgetEnv{Now: time.Now(), TimeSynced: ts.Synced()}
}

// TimeInfo は get_status と set_time で返す、Brain の時刻の状態。
type TimeInfo struct {
	Now          time.Time  `json:"now"`
	Timezone     string     `json:"timezone"`       // /etc/localtime の名前（Asia/Tokyo など）
	UTCOffsetSec int        `json:"utc_offset_sec"` // 今の UTC からのずれ
	Synced       bool       `json:"synced"`         // この起動のあいだに合わせた（set_time か NTP）
	NTPSynced    bool       `json:"ntp_synced"`     // NTP で合っている
	LastSet      *time.Time `json:"last_set,omitempty"`
	LastSource   string     `json:"last_source,omitempty"`
}

func (ts *TimeService) Info() TimeInfo {
	now := time.Now()
	_, off := now.Zone()
	ntp := ntpSynced()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	info := TimeInfo{Now: now, Timezone: timezoneName(), UTCOffsetSec: off,
		Synced: ts.setThisBootLocked() || ntp, NTPSynced: ntp}
	if ts.last != nil {
		at := ts.last.SetAt
		info.LastSet, info.LastSource = &at, ts.last.Source
	}
	return info
}

// save は、最新の記録を clock.json に書く。
func (ts *TimeService) save() {
	ts.saveMu.Lock()
	defer ts.saveMu.Unlock()
	ts.mu.Lock()
	rec := ts.last
	ts.mu.Unlock()
	if err := ts.store.Save(clockStoreName, rec); err != nil {
		log.Printf("time: save %s: %v", clockStoreName, err)
	}
}

// SetResult は set_time の結果。
type SetResult struct {
	Stepped  bool  `json:"stepped"`   // システムの時刻を動かした（ずれが 0.5 秒以上だった）
	OffsetMS int64 `json:"offset_ms"` // 合わせる前のずれ（送られた時刻 − Brain の時刻）
	TimeInfo
}

var errBadTime = errors.New("unix_ms is out of range (2024..2100)")

// Set は、システムの時刻を t に合わせ、合わせたことを記録する。
func (ts *TimeService) Set(t time.Time, source string) (SetResult, error) {
	if t.Before(minSettable) || !t.Before(maxSettable) {
		return SetResult{}, errBadTime
	}
	off := t.Sub(time.Now())
	res := SetResult{OffsetMS: off.Milliseconds()}
	if off.Abs() >= stepThreshold {
		// 測ってから設定するまでの時間も足す
		if err := setSystemClock(time.Now().Add(off)); err != nil {
			return SetResult{}, err
		}
		res.Stepped = true
	}
	rec := &clockRecord{BootID: ts.bootID, SetAt: time.Now().UTC(), OffsetMS: res.OffsetMS, Source: source}
	ts.mu.Lock()
	ts.last = rec
	onChange := ts.onChange
	ts.mu.Unlock()
	// SD カードの fsync は数秒かかることがあるので、返事を待たせない。
	// 記録は、デーモンを再起動したときに「合わせ済み」を覚えておくためだけに使う
	go ts.save()
	log.Printf("time: set by %s (offset %v, stepped %v)", source, off.Round(time.Millisecond), res.Stepped)
	if onChange != nil {
		onChange()
	}
	res.TimeInfo = ts.Info()
	return res, nil
}
