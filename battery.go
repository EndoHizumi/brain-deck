package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 電池（widget: battery） ----------
//
// 2026-10-09 に Brain（PW-SH2、カーネル 6.1.70）で調べた結果：
// - /sys/class/power_supply はない（カーネルの CONFIG_POWER_SUPPLY が無効）。
// - i.MX28 の LRADC が IIO（/sys/bus/iio/devices/iio:deviceN、名前 mxs-lradc-adc）に出ていて、
//   ch7 が電池の電圧（VBATT）、ch15 が USB の 5V（VDD5V）。値は in_voltageN_raw × in_voltageN_scale（mV）。
//   USB で PC につないだとき、ch7 は 4171 mV、ch15 は 4943 mV だった。
// - i.MX28 の内蔵の充電回路は止めてある（HW_POWER_CHARGE の PWD_BATTCHRG が 1）。充電は外付けの IC が行い、
//   その状態はソフトウェアからは読めない（GPIO にも出ていない）。
// そこで、残りは電圧から見積もり、「充電中」は USB の 5V があって満充電の電圧より低いときとみなす（推定）。
// USB につないでいるあいだは電圧が高めに出るので、残りも多めに出る。
//
// power_supply がある機種（ほかのカーネル）では、そちらの capacity と status を使う。

const (
	batteryPoll    = 30 * time.Second
	batteryAverage = 5    // 電圧の移動平均を取る回数（2 分半）
	vdd5vPresent   = 4400 // これより高ければ、USB の 5V がある（mV）
	batteryFullMV  = 4150 // USB の 5V があって、これ以上なら満充電とみなす
	lradcName      = "mxs-lradc-adc"
	lradcVBatt     = 7
	lradcVDD5V     = 15
)

// Li-ion の電圧（mV）と残り（%）の目安。あいだは直線で補う
var liionCurve = [][2]int{
	{3400, 0}, {3500, 5}, {3600, 12}, {3650, 22}, {3700, 33}, {3750, 45}, {3800, 55},
	{3900, 72}, {4000, 85}, {4100, 95}, {4150, 100},
}

// BatteryInfo は電池の状態（get_status の battery、ウィジェットの中身）。
type BatteryInfo struct {
	Percent   int    `json:"percent"`              // 残り（0〜100）。-1 なら分からない
	VoltageMV int    `json:"voltage_mv,omitempty"` // 電池の電圧（移動平均）
	Plugged   bool   `json:"plugged"`              // USB などから給電されている
	Charging  bool   `json:"charging"`             // 充電中（lradc では推定）
	Source    string `json:"source"`               // power_supply か lradc
	Estimated bool   `json:"estimated,omitempty"`  // 残りと充電中を電圧から見積もった
}

// voltagePercent は、電圧から残りを見積もる。
func voltagePercent(mv int) int {
	c := liionCurve
	if mv <= c[0][0] {
		return 0
	}
	for i := 1; i < len(c); i++ {
		if mv <= c[i][0] {
			a, b := c[i-1], c[i]
			return a[1] + (mv-a[0])*(b[1]-a[1])/(b[0]-a[0])
		}
	}
	return 100
}

// batteryReader は、電池の状態を 1 回読む。
type batteryReader interface {
	read() (BatteryInfo, error)
}

// psReader は /sys/class/power_supply を読む。
type psReader struct {
	battery string   // 電池のディレクトリ
	mains   []string // 給電（USB、Mains）のディレクトリ
}

func readSysInt(p string) (int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func readSysString(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func (r *psReader) read() (BatteryInfo, error) {
	pct, err := readSysInt(filepath.Join(r.battery, "capacity"))
	if err != nil {
		return BatteryInfo{}, err
	}
	b := BatteryInfo{Percent: min(max(pct, 0), 100), Source: "power_supply"}
	if uv, err := readSysInt(filepath.Join(r.battery, "voltage_now")); err == nil {
		b.VoltageMV = uv / 1000
	}
	switch readSysString(filepath.Join(r.battery, "status")) {
	case "Charging":
		b.Charging, b.Plugged = true, true
	case "Full", "Not charging":
		b.Plugged = true
	}
	for _, m := range r.mains {
		if v, err := readSysInt(filepath.Join(m, "online")); err == nil && v == 1 {
			b.Plugged = true
		}
	}
	return b, nil
}

// lradcReader は i.MX28 の LRADC（IIO）を読む。
type lradcReader struct{ dir string }

func (r *lradcReader) channel(n int) (int, error) {
	raw, err := readSysInt(filepath.Join(r.dir, "in_voltage"+strconv.Itoa(n)+"_raw"))
	if err != nil {
		return 0, err
	}
	sc, err := strconv.ParseFloat(readSysString(filepath.Join(r.dir, "in_voltage"+strconv.Itoa(n)+"_scale")), 64)
	if err != nil {
		return 0, err
	}
	return int(float64(raw)*sc + 0.5), nil
}

func (r *lradcReader) read() (BatteryInfo, error) {
	// 3 回読んで、まん中の値を使う（ほかの変換と重なったときの外れ値を除く）
	var vs []int
	for i := 0; i < 3; i++ {
		v, err := r.channel(lradcVBatt)
		if err != nil {
			return BatteryInfo{}, err
		}
		vs = append(vs, v)
	}
	sort.Ints(vs)
	b := BatteryInfo{VoltageMV: vs[1], Source: "lradc", Estimated: true}
	if v, err := r.channel(lradcVDD5V); err == nil && v > vdd5vPresent {
		b.Plugged = true
	}
	return b, nil
}

// findBatteryReader は、使える読み方を探す。なければ nil。
func findBatteryReader(sysRoot string) batteryReader {
	ps, _ := filepath.Glob(filepath.Join(sysRoot, "class/power_supply/*"))
	r := &psReader{}
	for _, d := range ps {
		switch readSysString(filepath.Join(d, "type")) {
		case "Battery":
			if r.battery == "" {
				r.battery = d
			}
		case "Mains", "USB":
			r.mains = append(r.mains, d)
		}
	}
	if r.battery != "" {
		return r
	}
	iio, _ := filepath.Glob(filepath.Join(sysRoot, "bus/iio/devices/iio:device*"))
	for _, d := range iio {
		if readSysString(filepath.Join(d, "name")) == lradcName {
			return &lradcReader{dir: d}
		}
	}
	return nil
}

// BatteryService は、電池の状態を定期的に読む。
type BatteryService struct {
	mu       sync.Mutex
	r        batteryReader
	info     *BatteryInfo
	volts    []int // lradc の電圧の移動平均
	plugged  bool
	onChange func()
	stop     chan struct{}
}

// NewBatteryService は、電池の読み方を探して、読み始める。読めなければ、Info はいつも nil。
func NewBatteryService(sysRoot string) *BatteryService {
	bs := &BatteryService{r: findBatteryReader(sysRoot), stop: make(chan struct{})}
	if bs.r == nil {
		log.Printf("battery: no power_supply battery or %s found (widget: battery shows unknown)", lradcName)
		return bs
	}
	bs.poll()
	if in := bs.Info(); in != nil {
		log.Printf("battery: %s, %d%% (%d mV), plugged=%v", in.Source, in.Percent, in.VoltageMV, in.Plugged)
	}
	go func() {
		t := time.NewTicker(batteryPoll)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				bs.poll()
			case <-bs.stop:
				return
			}
		}
	}()
	return bs
}

// SetOnChange は、表示が変わったときに呼ぶ関数を設定する。
func (bs *BatteryService) SetOnChange(f func()) {
	bs.mu.Lock()
	bs.onChange = f
	bs.mu.Unlock()
}

// Info は今の状態の写し。読めなければ nil。
func (bs *BatteryService) Info() *BatteryInfo {
	if bs == nil {
		return nil
	}
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.info == nil {
		return nil
	}
	in := *bs.info
	return &in
}

func (bs *BatteryService) poll() {
	b, err := bs.r.read()
	bs.mu.Lock()
	if err != nil {
		if bs.info != nil {
			log.Printf("battery: %v", err)
		}
		bs.info = nil
		bs.mu.Unlock()
		return
	}
	if b.Source == "lradc" {
		if b.Plugged != bs.plugged { // つないだ・外したときは電圧が跳ぶので、平均を取り直す
			bs.volts = nil
		}
		bs.plugged = b.Plugged
		bs.volts = append(bs.volts, b.VoltageMV)
		if len(bs.volts) > batteryAverage {
			bs.volts = bs.volts[1:]
		}
		sum := 0
		for _, v := range bs.volts {
			sum += v
		}
		b.VoltageMV = sum / len(bs.volts)
		b.Percent = voltagePercent(b.VoltageMV)
		b.Charging = b.Plugged && b.VoltageMV < batteryFullMV
	}
	changed := bs.info == nil || bs.info.Percent != b.Percent || bs.info.Plugged != b.Plugged || bs.info.Charging != b.Charging
	bs.info = &b
	f := bs.onChange
	bs.mu.Unlock()
	if changed && f != nil {
		f()
	}
}
