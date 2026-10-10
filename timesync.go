package main

import (
	"errors"
	"fmt"
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
	Source   string    `json:"source"`    // gui、brain-deck、brain（Brain のキーボード）、rtc（起動したときに RTC から）
	// PCSetAt は、最後に PC から合わせた時刻（状態の帯の「同期」）。古い clock.json にはないので、Source から補う
	PCSetAt *time.Time `json:"pc_set_at,omitempty"`
	// LastKnown は、合っていると分かっていた最後の時刻（rtc.go。起動したときの下限と、目安）
	LastKnown *time.Time `json:"last_known,omitempty"`
	// RTCLog は、PC から時刻を合わせたときの、書き戻す前の内蔵 RTC と PC の時刻の差の記録（新しいものが後。rtcLogMax まで）
	RTCLog []RTCSample `json:"rtc_log,omitempty"`
	// RTCWrittenAt は、最後に内蔵 RTC に書いた時刻
	RTCWrittenAt *time.Time `json:"rtc_written_at,omitempty"`
}

// RTCSample は、内蔵 RTC と PC の時刻の差を 1 回測ったもの（RTC の遅れ方の記録）。
type RTCSample struct {
	At       time.Time `json:"at"`               // 測った時刻（UTC）
	OffsetMS int64     `json:"offset_ms"`        // RTC − PC（RTC は秒の単位なので、±1 秒の誤差がある）
	SinceSec int64     `json:"since_write_sec"`  // 前に RTC に書いてからの時間（秒）。0 なら分からない
	Source   string    `json:"source,omitempty"` // 合わせた側
}

const rtcLogMax = 30

// 時刻を合わせた側のうち、PC でないもの
const (
	sourceBrain = "brain" // Brain のキーボード（input: clock）
	sourceRTC   = "rtc"   // 起動したときに内蔵 RTC から
)

const (
	lastKnownEvery = 30 * time.Minute // 合っているあいだ、last_known を書く間隔
	rtcBehindSlack = 2 * time.Minute  // RTC がこれより前の時刻なら、狂っているとみなす
)

// TimeService は時刻合わせと、時刻が合っているかどうかを受け持つ。
type TimeService struct {
	mu       sync.Mutex
	saveMu   sync.Mutex // clock.json の書き込みを 1 つずつ行う
	store    *Store
	bootID   string
	last     *clockRecord
	onChange func() // 時刻を合わせたとき（時計の描き直し）。待たずに返ること
	rtcMode  string // clock.rtc（off、read、write）
	rtcBoot  string // 起動したときに RTC をどうしたか（get_status の rtc.boot）
	rtcWrite string // 最後に RTC に書いた結果（空なら書いていない）
	rtcWAt   *time.Time
}

func NewTimeService(store *Store) *TimeService {
	ts := &TimeService{store: store, bootID: readBootID()}
	var r clockRecord
	if err := store.Load(clockStoreName, &r); err == nil {
		if r.PCSetAt == nil && r.Source != "" && r.Source != sourceBrain && r.Source != sourceRTC {
			at := r.SetAt
			r.PCSetAt = &at
		}
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
	BootSource   string     `json:"boot_source,omitempty"` // この起動で合わせた側（gui、brain-deck、brain、rtc）
	LastPCSet    *time.Time `json:"last_pc_set,omitempty"` // 最後に PC から合わせた時刻
	LastKnown    *time.Time `json:"last_known,omitempty"`  // 合っていると分かっていた最後の時刻
	Timezone     string     `json:"timezone"`              // /etc/localtime の名前（Asia/Tokyo など）
	UTCOffsetSec int        `json:"utc_offset_sec"`        // 今の UTC からのずれ
	Synced       bool       `json:"synced"`                // この起動のあいだに合わせた（set_time か NTP）
	NTPSynced    bool       `json:"ntp_synced"`            // NTP で合っている
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
		info.LastPCSet, info.LastKnown = ts.last.PCSetAt, ts.last.LastKnown
		if ts.setThisBootLocked() {
			info.BootSource = ts.last.Source
		}
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
	// RTC は、書き戻す前の内蔵 RTC と、送られた時刻の差（RTC を読めたときだけ）
	RTC *RTCSample `json:"rtc,omitempty"`
	// RTCWrite は、この時刻合わせで RTC に書き戻すか
	RTCWrite bool `json:"rtc_write,omitempty"`
	TimeInfo
}

var errBadTime = errors.New("unix_ms is out of range (2024..2100)")

// Set は、システムの時刻を t に合わせ、合わせたことを記録する。
func (ts *TimeService) Set(t time.Time, source string) (SetResult, error) {
	if t.Before(minSettable) || !t.Before(maxSettable) {
		return SetResult{}, errBadTime
	}
	entry := time.Now()
	var sample *RTCSample
	ts.mu.Lock()
	measure := ts.rtcMode != rtcOff && ts.rtcMode != "" && source != sourceBrain && source != sourceRTC
	var since int64
	if ts.last != nil && ts.last.RTCWrittenAt != nil {
		since = int64(t.Sub(*ts.last.RTCWrittenAt).Seconds())
	}
	ts.mu.Unlock()
	if measure {
		// 書き戻す前の RTC を読み、PC の時刻と比べる（RTC の遅れ方の記録）
		if r, err := readRTC(); err == nil {
			pcAtRead := t.Add(r.ReadAt.Sub(entry))
			sample = &RTCSample{At: t.UTC(), OffsetMS: r.Time.Sub(pcAtRead).Milliseconds(), SinceSec: max(since, 0), Source: source}
		}
	}
	off := t.Sub(time.Now())
	res := SetResult{OffsetMS: off.Milliseconds(), RTC: sample}
	if off.Abs() >= stepThreshold {
		// 測ってから設定するまでの時間も足す
		if err := setSystemClock(time.Now().Add(off)); err != nil {
			return SetResult{}, err
		}
		res.Stepped = true
	}
	now := time.Now().UTC()
	rec := &clockRecord{BootID: ts.bootID, SetAt: now, OffsetMS: res.OffsetMS, Source: source, LastKnown: &now}
	ts.mu.Lock()
	if ts.last != nil {
		rec.PCSetAt = ts.last.PCSetAt
		rec.RTCLog = ts.last.RTCLog
		rec.RTCWrittenAt = ts.last.RTCWrittenAt
	}
	if sample != nil {
		rec.RTCLog = append(append([]RTCSample(nil), rec.RTCLog...), *sample)
		if len(rec.RTCLog) > rtcLogMax {
			rec.RTCLog = rec.RTCLog[len(rec.RTCLog)-rtcLogMax:]
		}
	}
	if source != sourceBrain && source != sourceRTC {
		rec.PCSetAt = &now
	}
	ts.last = rec
	onChange := ts.onChange
	write := ts.rtcMode == rtcWrite && source != sourceRTC
	res.RTCWrite = write
	ts.mu.Unlock()
	if write {
		go ts.writeBack()
	}
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

// ---------- 内蔵 RTC と、最後に分かっていた時刻（rtc.go） ----------

// BootFromRTC は、起動したときに呼ぶ。時刻が合っていなければ、内蔵 RTC の時刻（mode が off なら使わない）か、
// 最後に分かっていた時刻（今の時刻より後なら）に合わせる。結果は get_status の rtc.boot に出す。
func (ts *TimeService) BootFromRTC(mode string) {
	ts.mu.Lock()
	ts.rtcMode = mode
	var known time.Time
	if ts.last != nil && ts.last.LastKnown != nil {
		known = *ts.last.LastKnown
	}
	ts.mu.Unlock()
	if ts.Synced() {
		ts.setBoot("not needed (the clock was already set in this boot)")
		return
	}
	now := time.Now()
	lower := now
	if known.After(lower) {
		lower = known
	}
	if mode != rtcOff {
		r, err := readRTC()
		switch {
		case err != nil:
			ts.setBoot("not used: " + err.Error())
		case r.Time.Before(minSettable) || !r.Time.Before(maxSettable):
			ts.setBoot(fmt.Sprintf("not used: the RTC says %s, out of range", r.Time.Format(time.DateTime)))
		case r.Time.Before(lower.Add(-rtcBehindSlack)):
			ts.setBoot(fmt.Sprintf("not used: the RTC says %s, before the clock restored at boot or the last known time (%s)", r.Time.Format(time.DateTime), lower.Format(time.DateTime)))
		default:
			t := r.Time.Add(time.Since(r.ReadAt))
			if _, err := ts.Set(t, sourceRTC); err != nil {
				ts.setBoot("not used: " + err.Error())
				return
			}
			ts.setBoot(fmt.Sprintf("set the clock from the RTC (%s, %+v from the restored clock)", r.Time.Format(time.DateTime), r.Time.Sub(now).Round(time.Second)))
			return
		}
	} else {
		ts.setBoot("off (clock.rtc: off)")
	}
	// RTC が使えない：最後に分かっていた時刻のほうが進んでいれば、そこまで進める（未設定のまま。時計は目安を出す）
	if known.Sub(now) >= time.Second {
		if err := setSystemClock(known); err != nil {
			log.Printf("time: moving the clock to the last known time: %v", err)
		} else {
			log.Printf("time: moved the clock forward to the last known time %s (still not set)", known.Format(time.DateTime))
		}
	}
}

func (ts *TimeService) setBoot(s string) {
	ts.mu.Lock()
	ts.rtcBoot = s
	ts.mu.Unlock()
	log.Printf("time: rtc: %s", s)
}

// writeBack は、今の時刻を RTC に書く（clock.rtc: write のとき、時刻を合わせたあと）。
func (ts *TimeService) writeBack() {
	err := writeRTC(time.Now())
	now := time.Now()
	ts.mu.Lock()
	ts.rtcWAt = &now
	if err != nil {
		ts.rtcWrite = "failed: " + err.Error()
	} else {
		ts.rtcWrite = "ok"
		if ts.last != nil {
			r := *ts.last
			u := now.UTC()
			r.RTCWrittenAt = &u
			ts.last = &r
		}
	}
	ts.mu.Unlock()
	if err == nil {
		ts.save()
	}
	if err != nil {
		log.Printf("time: rtc: write: %v", err)
	} else {
		log.Printf("time: rtc: wrote %s", now.Format(time.DateTime))
	}
}

// RTCInfo は get_status の rtc。
type RTCInfo struct {
	Mode      string     `json:"mode"`                  // off、read、write
	Readable  bool       `json:"readable"`              // 読めた
	Error     string     `json:"error,omitempty"`       // 読めなかった理由
	Seconds   uint32     `json:"seconds,omitempty"`     // HW_RTC_SECONDS
	Time      *time.Time `json:"time,omitempty"`        // RTC の時刻（2000 年起点のローカル時刻とみなしたもの）
	OffsetMS  int64      `json:"offset_ms"`             // RTC − Brain のシステムの時刻
	Stat      string     `json:"stat,omitempty"`        // HW_RTC_STAT
	Persist0  string     `json:"persistent0,omitempty"` // HW_RTC_PERSISTENT0
	Boot      string     `json:"boot,omitempty"`        // 起動したときにどうしたか
	LastWrite string     `json:"last_write,omitempty"`  // 最後に書いた結果
	WrittenAt *time.Time `json:"written_at,omitempty"`
	// Log は、PC から時刻を合わせたときの、書き戻す前の RTC と PC の差の記録（新しいものが後）
	Log []RTCSample `json:"log,omitempty"`
}

// RTCInfo は、RTC を読み、今の状態を返す。
func (ts *TimeService) RTCInfo() RTCInfo {
	ts.mu.Lock()
	in := RTCInfo{Mode: ts.rtcMode, Boot: ts.rtcBoot, LastWrite: ts.rtcWrite, WrittenAt: ts.rtcWAt}
	if ts.last != nil {
		in.Log = append([]RTCSample(nil), ts.last.RTCLog...)
		if in.WrittenAt == nil {
			in.WrittenAt = ts.last.RTCWrittenAt
		}
	}
	ts.mu.Unlock()
	if in.Mode == "" {
		in.Mode = rtcRead
	}
	if in.Mode == rtcOff {
		return in
	}
	r, err := readRTC()
	if err != nil {
		in.Error = err.Error()
		return in
	}
	t := r.Time
	in.Readable, in.Seconds, in.Time = true, r.Seconds, &t
	in.OffsetMS = r.Time.Sub(r.ReadAt).Milliseconds()
	in.Stat, in.Persist0 = fmt.Sprintf("%#08x", r.Stat), fmt.Sprintf("%#08x", r.Persist0)
	return in
}

// SaveKnown は、時刻が合っていれば、今を「合っていると分かっていた最後の時刻」として残す（定期的にと、終わるとき）。
func (ts *TimeService) SaveKnown() {
	if ts == nil || !ts.Synced() {
		return
	}
	now := time.Now().UTC()
	ts.mu.Lock()
	if ts.last == nil {
		ts.last = &clockRecord{Source: "ntp", SetAt: now}
	}
	r := *ts.last
	r.LastKnown = &now
	ts.last = &r
	ts.mu.Unlock()
	ts.save()
}

// KeepKnown は、lastKnownEvery ごとに SaveKnown を呼び続ける。
func (ts *TimeService) KeepKnown() {
	for range time.Tick(lastKnownEvery) {
		ts.SaveKnown()
	}
}

// Estimate は、時刻を合わせていないときの目安（今の時刻と最後に分かっていた時刻の遅いほう）。
func (ts *TimeService) Estimate() time.Time {
	now := time.Now()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.last != nil && ts.last.LastKnown != nil && ts.last.LastKnown.After(now) {
		return ts.last.LastKnown.Local()
	}
	return now
}

// SetRTCMode は clock.rtc を差し替える（設定 GUI からの保存）。
func (ts *TimeService) SetRTCMode(mode string) {
	ts.mu.Lock()
	ts.rtcMode = mode
	ts.mu.Unlock()
}

// afterClockSet は、時刻を合わせたあと、時刻を覚えているウィジェットをずらす（動かしたとき）か、合わせたことを知らせる。
func afterClockSet(res SetResult, texts, qrs *TextService, gadgets *GadgetService, graphs *GraphService) {
	if res.Stepped {
		off := time.Duration(res.OffsetMS) * time.Millisecond
		texts.ClockStepped(off)
		qrs.ClockStepped(off)
		gadgets.ClockStepped(off)
		graphs.ClockStepped(off)
		return
	}
	texts.ClockSynced()
	qrs.ClockSynced()
	graphs.ClockSynced()
}
