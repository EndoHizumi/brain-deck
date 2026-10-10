package main

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ---------- i.MX28 の内蔵 RTC（設定の clock.rtc） ----------
//
// i.MX28 には内蔵 RTC があり、本体の電池が残っている限り数え続ける。Brain 純正のファームウェアは
// HW_RTC_SECONDS（0x80056030）を「2000-01-01 00:00:00 起点の秒、ローカル時刻（日本の時刻）」として使う
// （https://qiita.com/kurokouji/items/66ddd4884cefbc89d736 の「6. 時計」。PW-SH6 での話）。
// Linux の rtc-stmp3xxx は 1970 年起点の UTC が前提なので、Brainux のデバイスツリーでは無効になっている。
// そこでドライバを使わず、/dev/mem からレジスタを直接読む（カーネルは CONFIG_STRICT_DEVMEM=y だが
// CONFIG_IO_STRICT_DEVMEM は無効で、ドライバが使っていない RTC の範囲は読める。2026-10-10 に PW-SH2 で確かめた）。
//
// - 起動したとき：時刻が合っていなければ（この起動で合わせていない、NTP でもない）、RTC の時刻に合わせる。
//   ただし、RTC の時刻が今のシステムの時刻（systemd-timesyncd が前に止まった時刻から戻したもの）か、
//   最後に分かっていた時刻（clock.json の last_known）より前なら、RTC が狂っているとみなして使わない。
// - 書き戻し：clock.rtc が write のときだけ、PC や Brain のキーボードで時刻を合わせたら RTC に書く。
//   書くのは STAT の NEW_REGS が 0 のときだけで、PERSISTENT のレジスタには触らない。
//   既定は read（読むだけ）。純正のファームウェアの時計と同じ形かを確かめてから write にする（README の「時刻」）。

const (
	rtcBase      = 0x80056000
	rtcMapSize   = 0x1000
	rtcStat      = 0x10
	rtcSeconds   = 0x30
	rtcPersist0  = 0x60
	rtcPresent   = 1 << 31
	rtcNewRegs   = 0xff << 8
	rtcNewSecond = 0x80 << 8 // NEW_REGS の SECONDS（Persistent 0〜5、Alarm、Seconds の順）
	rtcWriteWait = 100 * time.Millisecond

	rtcOff   = "off"
	rtcRead  = "read"
	rtcWrite = "write"
)

var rtcModes = map[string]bool{rtcOff: true, rtcRead: true, rtcWrite: true}

// テストで差し替える
var (
	rtcMemPath = "/dev/mem"
	rtcEpoch   = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) // この「壁時計の時刻」をローカル時刻として数える
)

// ClockConfig は設定の clock。
type ClockConfig struct {
	// RTC は内蔵 RTC の使い方。read（既定）は起動したときに読んで合わせるだけ、write は時刻を合わせたら書き戻す、off は使わない
	RTC string `yaml:"rtc,omitempty" json:"rtc,omitempty"`
}

func (c *ClockConfig) validate(fail func(path, format string, args ...any)) {
	if c != nil && c.RTC != "" && !rtcModes[c.RTC] {
		fail("/clock/rtc", "clock.rtc must be read, write or off, got %q", c.RTC)
	}
}

// rtcMode は clock.rtc（省略すると read）。
func (c *ClockConfig) rtcMode() string {
	if c == nil || c.RTC == "" {
		return rtcRead
	}
	return c.RTC
}

// rtcToTime は、RTC の秒を loc の時刻にする（2000-01-01 00:00:00 の壁時計から数えた秒）。
func rtcToTime(sec uint32, loc *time.Location) time.Time {
	w := rtcEpoch.Add(time.Duration(sec) * time.Second)
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, loc)
}

// timeToRTC は、時刻を RTC の秒にする（loc の壁時計で数える）。
func timeToRTC(t time.Time, loc *time.Location) (uint32, error) {
	t = t.In(loc)
	w := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	d := w.Sub(rtcEpoch) / time.Second
	if d < 0 || d > 0xffffffff {
		return 0, fmt.Errorf("%v is out of the RTC range", t)
	}
	return uint32(d), nil
}

// rtcMap は、RTC のレジスタを写した範囲。
type rtcMap struct{ mem []byte }

func openRTC(write bool) (*rtcMap, error) {
	flag, prot := os.O_RDONLY, syscall.PROT_READ
	if write {
		flag, prot = os.O_RDWR, syscall.PROT_READ|syscall.PROT_WRITE
	}
	f, err := os.OpenFile(rtcMemPath, flag|syscall.O_SYNC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	mem, err := syscall.Mmap(int(f.Fd()), rtcBase, rtcMapSize, prot, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap %s at %#x: %w", rtcMemPath, uint32(rtcBase), err)
	}
	return &rtcMap{mem: mem}, nil
}

// reg と set は 32 ビットで読み書きする（バイト単位で触らない）。
func (m *rtcMap) reg(off int) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&m.mem[off])))
}
func (m *rtcMap) set(off int, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&m.mem[off])), v)
}
func (m *rtcMap) close() { syscall.Munmap(m.mem) }

// RTCReading は、RTC を 1 回読んだ結果。
type RTCReading struct {
	Seconds  uint32    // HW_RTC_SECONDS
	Stat     uint32    // HW_RTC_STAT
	Persist0 uint32    // HW_RTC_PERSISTENT0
	Time     time.Time // Seconds をローカル時刻とみなした時刻
	ReadAt   time.Time // 読んだときのシステムの時刻
}

var errNoRTC = errors.New("the RTC is not present (HW_RTC_STAT.RTC_PRESENT is 0)")

// readRTC は RTC を読む（テストで差し替える）。
var readRTC = func() (RTCReading, error) {
	m, err := openRTC(false)
	if err != nil {
		return RTCReading{}, err
	}
	defer m.close()
	r := RTCReading{Stat: m.reg(rtcStat), Persist0: m.reg(rtcPersist0)}
	if r.Stat&rtcPresent == 0 {
		return r, errNoRTC
	}
	r.Seconds, r.ReadAt = m.reg(rtcSeconds), time.Now()
	r.Time = rtcToTime(r.Seconds, time.Local)
	return r, nil
}

// writeRTC は、t を RTC に書く（テストで差し替える）。前の書き込みが RTC に届いていなければ（NEW_REGS が 0 でなければ）書かない。
var writeRTC = func(t time.Time) error {
	sec, err := timeToRTC(t, time.Local)
	if err != nil {
		return err
	}
	m, err := openRTC(true)
	if err != nil {
		return err
	}
	defer m.close()
	st := m.reg(rtcStat)
	if st&rtcPresent == 0 {
		return errNoRTC
	}
	if st&rtcNewRegs != 0 {
		return fmt.Errorf("the RTC is still copying an earlier write (HW_RTC_STAT %#08x); not written", st)
	}
	m.set(rtcSeconds, sec)
	deadline := time.Now().Add(rtcWriteWait)
	for m.reg(rtcStat)&rtcNewSecond != 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("the RTC did not take the write within %v (HW_RTC_STAT %#08x)", rtcWriteWait, m.reg(rtcStat))
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}
