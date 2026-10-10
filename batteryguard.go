package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- 電池が少ないとき（設定の battery） ----------
//
// 電池だけで動いているあいだ（USB の 5V がないとき）に、残り（電圧からの見積もり）が
//   - warn 以下になったら、画面の下に知らせる（橙）
//   - critical 以下になったら、もう一度知らせ、画面を明るくする（赤）
//   - poweroff 以下が続いたら、60 秒後に電源を切ると知らせ、どれかのキーかタッチで取りやめなければ、
//     データを保存して（デーモンを止めるときに保存する）電源を切る
// USB の 5V があるあいだは何もしない。i.MX28 は、5V があるときに電源を切ると起動し直してしまうと報告されているので
// （参考の記事の「7. ハマったこと」）、電源を切るのは 5V がないときだけにする。
// 電圧は、つないだ・外した直後に跳ぶので、外してから batteryGuardMin 回読むまでは判断しない。
//
// 記録（battery.log、既定で行う）：電池だけで動いているあいだ、読むたびに 1 行を <data-dir>/battery-log.csv に足す
// （時刻、電池の電圧、平均、5V、残り、画面の明るさ、出来事）。つないだ・外したときにも 1 行。batteryLogMax を超えたら .1 に移す。
// brain-deck battery log で PC に取り出し、残りの見積もり（battery.curve）を見直すのに使う。

const (
	batteryGuardMin     = 3                // 外してから、判断するまでに読む回数
	batteryPoweroffWait = 60 * time.Second // 電源を切ると知らせてから切るまで
	batteryCancelFor    = 10 * time.Minute // 取りやめたら、次に確かめるまで
	batteryWarnDefault  = 20
	batteryCritDefault  = 10
	batteryLogName      = "battery-log.csv"
	batteryLogMax       = 512 << 10
	batteryLogHeader    = "time,uptime_s,vbatt_mv,vbatt_avg_mv,vbus_mv,percent,plugged,backlight,event"
)

// BatteryConfig は設定の battery。
type BatteryConfig struct {
	Warn     *int     `yaml:"warn,omitempty" json:"warn,omitempty"`         // これ以下で知らせる（%）。既定 20。0 で知らせない
	Critical *int     `yaml:"critical,omitempty" json:"critical,omitempty"` // これ以下で強く知らせる（%）。既定 10。0 で知らせない
	Poweroff int      `yaml:"poweroff,omitempty" json:"poweroff,omitempty"` // これ以下で電源を切る（%）。既定 0（切らない）
	Log      *bool    `yaml:"log,omitempty" json:"log,omitempty"`           // 電池だけのあいだの電圧を記録する。既定 true
	Curve    [][2]int `yaml:"curve,omitempty" json:"curve,omitempty"`       // 電圧（mV）と残り（%）の表。省略すると既定の表
}

// batteryParams は組み立てた battery。
type batteryParams struct {
	warn, critical, poweroff int
	log                      bool
	curve                    [][2]int
}

func (c *BatteryConfig) compile() (batteryParams, error) {
	p := batteryParams{warn: batteryWarnDefault, critical: batteryCritDefault, log: true}
	if c == nil {
		return p, nil
	}
	pct := func(name string, v *int, dst *int) error {
		if v == nil {
			return nil
		}
		if *v < 0 || *v > 100 {
			return fmt.Errorf("battery.%s must be 0..100 (percent)", name)
		}
		*dst = *v
		return nil
	}
	if err := pct("warn", c.Warn, &p.warn); err != nil {
		return p, err
	}
	if err := pct("critical", c.Critical, &p.critical); err != nil {
		return p, err
	}
	if err := pct("poweroff", &c.Poweroff, &p.poweroff); err != nil {
		return p, err
	}
	if p.poweroff > 50 {
		return p, fmt.Errorf("battery.poweroff must be 0..50 (percent; 0 never powers off)")
	}
	if p.critical > 0 && p.warn > 0 && p.critical > p.warn {
		return p, fmt.Errorf("battery.critical (%d) must not be more than battery.warn (%d)", p.critical, p.warn)
	}
	if p.poweroff > 0 && p.critical > 0 && p.poweroff >= p.critical {
		return p, fmt.Errorf("battery.poweroff (%d) must be less than battery.critical (%d)", p.poweroff, p.critical)
	}
	if c.Log != nil {
		p.log = *c.Log
	}
	if c.Curve != nil {
		if len(c.Curve) < 2 {
			return p, fmt.Errorf("battery.curve needs at least 2 points ([mV, percent])")
		}
		for i, pt := range c.Curve {
			if pt[0] < 2500 || pt[0] > 4500 || pt[1] < 0 || pt[1] > 100 {
				return p, fmt.Errorf("battery.curve[%d] must be [mV 2500..4500, percent 0..100]", i)
			}
			if i > 0 && (pt[0] <= c.Curve[i-1][0] || pt[1] < c.Curve[i-1][1]) {
				return p, fmt.Errorf("battery.curve must go up in mV (and not go down in percent) at [%d]", i)
			}
		}
		p.curve = c.Curve
	}
	return p, nil
}

func (c *BatteryConfig) validate(fail func(path, format string, args ...any)) {
	if _, err := c.compile(); err != nil {
		fail("/battery", "%v", err)
	}
}

// BatteryGuard は、電池が少ないときの知らせと電源断、記録を受け持つ。
type BatteryGuard struct {
	mu        sync.Mutex
	p         batteryParams
	notify    func(msg string, level int, d time.Duration) // 画面の下に知らせる（d が 0 なら出したまま）
	clear     func(msg string)                             // 出したままの知らせを消す
	wake      func()                                       // 画面を明るくする
	poweroff  func() error                                 // 電源を切る
	backlight func() int                                   // 今の画面の明るさ（記録用）。nil なら -1
	logPath   string                                       // 記録のファイル。空なら記録しない
	now       func() time.Time
	after     func(time.Duration, func()) func() bool // タイマー（止める関数を返す）。テストで差し替える

	warned   int // 知らせた段階（0 なし、1 warn、2 critical）
	below    int // poweroff 以下が続いた回数
	stopOff  func() bool
	offAt    time.Time // 電源を切る時刻（ゼロなら切ろうとしていない）
	cancelTo time.Time // 取りやめたので、この時刻までは切らない
	plugged  *bool     // 前に読んだときに 5V があったか
	offMsg   string
}

// NewBatteryGuard は係を作る。dataDir は記録の置き場所。
func NewBatteryGuard(p batteryParams, dataDir string) *BatteryGuard {
	return &BatteryGuard{p: p, logPath: filepath.Join(dataDir, batteryLogName), now: time.Now,
		after: func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }}
}

// Apply は設定を差し替える（設定 GUI からの保存）。
func (g *BatteryGuard) Apply(p batteryParams) {
	g.mu.Lock()
	g.p = p
	g.mu.Unlock()
}

// Sample は、電池を読むたびに呼ぶ（BatteryService.OnPoll）。
func (g *BatteryGuard) Sample(b BatteryInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	event := ""
	if g.plugged == nil || *g.plugged != b.Plugged {
		if g.plugged != nil {
			event = map[bool]string{true: "plugged", false: "unplugged"}[b.Plugged]
		} else {
			event = "start"
		}
		pl := b.Plugged
		g.plugged = &pl
	}
	if b.Plugged {
		g.warned, g.below = 0, 0
		if !g.offAt.IsZero() {
			g.cancelLocked("USB の電源につながったので、電源を切るのをやめました")
		}
		if event != "" {
			g.logLocked(now, b, event)
		}
		return
	}
	logged := false
	defer func() {
		if !logged {
			g.logLocked(now, b, event)
		}
	}()
	if b.Percent < 0 || (b.Source == "lradc" && b.Samples < batteryGuardMin) {
		return
	}
	pct := b.Percent
	switch {
	case g.p.warn > 0 && pct > g.p.warn+5:
		g.warned = 0 // 充電して戻った（電源につながずに、電圧が戻ることもある）
	case g.p.critical > 0 && pct <= g.p.critical && g.warned < 2:
		g.warned = 2
		event = joinEvent(event, "critical")
		g.say(fmt.Sprintf("電池がほとんどありません（%d%%）。USB につないでください", pct), noticeError, 10*time.Second)
		if g.wake != nil {
			g.wake()
		}
	case g.p.warn > 0 && pct <= g.p.warn && g.warned < 1:
		g.warned = 1
		event = joinEvent(event, "warn")
		g.say(fmt.Sprintf("電池が少なくなりました（%d%%）", pct), noticeWarn, 6*time.Second)
	}
	if g.p.poweroff > 0 && pct <= g.p.poweroff {
		g.below++
	} else {
		g.below = 0
	}
	if g.below >= batteryGuardMin && g.offAt.IsZero() && !now.Before(g.cancelTo) {
		g.offAt = now.Add(batteryPoweroffWait)
		event = joinEvent(event, "poweroff-countdown")
		g.offMsg = fmt.Sprintf("電池が残りわずか（%d%%）。%s に電源を切ります（キーかタッチでやめる）", pct, g.offAt.Format("15:04:05"))
		g.say(g.offMsg, noticeError, 0)
		if g.wake != nil {
			g.wake()
		}
		log.Printf("battery: %d%% (%d mV) <= poweroff %d%%; powering off at %s unless cancelled", pct, b.VoltageMV, g.p.poweroff, g.offAt.Format(time.TimeOnly))
		g.stopOff = g.after(batteryPoweroffWait, g.fire)
	}
}

func joinEvent(a, b string) string {
	if a == "" {
		return b
	}
	return a + "+" + b
}

func (g *BatteryGuard) say(msg string, level int, d time.Duration) {
	if g.notify != nil {
		g.notify(msg, level, d)
	}
}

// fire は、電源を切る時刻になったときに呼ぶ。
func (g *BatteryGuard) fire() {
	g.mu.Lock()
	if g.offAt.IsZero() || (g.plugged != nil && *g.plugged) {
		g.mu.Unlock()
		return
	}
	g.offAt = time.Time{}
	g.logLocked(g.now(), BatteryInfo{Percent: -1}, "poweroff")
	f := g.poweroff
	g.mu.Unlock()
	log.Printf("battery: powering off (low battery)")
	if f == nil {
		return
	}
	if err := f(); err != nil {
		log.Printf("battery: poweroff: %v", err)
		g.say("電源を切れませんでした："+err.Error(), noticeError, 10*time.Second)
	}
}

// Activity は、キーかタッチがあったときに呼ぶ。電源を切ろうとしていれば取りやめる。
func (g *BatteryGuard) Activity() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.offAt.IsZero() {
		return
	}
	g.cancelTo = g.now().Add(batteryCancelFor)
	g.cancelLocked(fmt.Sprintf("電源を切るのをやめました（%d 分後にもう一度確かめます）", int(batteryCancelFor/time.Minute)))
}

func (g *BatteryGuard) cancelLocked(msg string) {
	if g.stopOff != nil {
		g.stopOff()
		g.stopOff = nil
	}
	g.offAt, g.below = time.Time{}, 0
	if g.clear != nil {
		g.clear(g.offMsg)
	}
	log.Printf("battery: poweroff cancelled")
	g.logLocked(g.now(), BatteryInfo{Percent: -1}, "poweroff-cancelled")
	g.say(msg, noticeInfo, 5*time.Second)
}

// PoweringOff は、電源を切ろうとしている（知らせている）か。
func (g *BatteryGuard) PoweringOff() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.offAt.IsZero()
}

// logLocked は、記録に 1 行を足す。
func (g *BatteryGuard) logLocked(now time.Time, b BatteryInfo, event string) {
	if !g.p.log || g.logPath == "" {
		return
	}
	bl := -1
	if g.backlight != nil {
		bl = g.backlight()
	}
	var up float64
	if s, err := os.ReadFile("/proc/uptime"); err == nil {
		fmt.Sscanf(string(s), "%f", &up)
	}
	line := fmt.Sprintf("%s,%d,%d,%d,%d,%d,%v,%d,%s\n", now.Format(time.RFC3339), int(up), b.RawMV, b.VoltageMV, b.VBusMV,
		b.Percent, b.Plugged, bl, event)
	if st, err := os.Stat(g.logPath); err == nil && st.Size() > batteryLogMax {
		os.Rename(g.logPath, g.logPath+".1")
	}
	f, err := os.OpenFile(g.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		log.Printf("battery: log: %v", err)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		f.WriteString(batteryLogHeader + "\n")
	}
	f.WriteString(line)
}

// readBatteryLog は、記録（.1 と今のもの）を古い順につないで返す。lines が正なら、最後の lines 行だけ（見出しは付ける）。
func readBatteryLog(dataDir string, lines int) (string, error) {
	var rows []string
	p := filepath.Join(dataDir, batteryLogName)
	for _, f := range []string{p + ".1", p} {
		b, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if ln != "" && ln != batteryLogHeader {
				rows = append(rows, ln)
			}
		}
	}
	if lines > 0 && len(rows) > lines {
		rows = rows[len(rows)-lines:]
	}
	return batteryLogHeader + "\n" + strings.Join(append(rows, ""), "\n"), nil
}
