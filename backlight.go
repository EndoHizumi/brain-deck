package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 自動減光（設定の backlight） ----------
//
// しばらく触らなければバックライトを暗くし、さらに時間がたったら消す。
// 2026-10-09 に Brain（PW-SH2）で調べた結果：/sys/class/backlight/backlight@0（pwm-backlight）。
// max_brightness は 7 で、デバイスツリーの brightness-levels は 0 4 8 16 32 64 128 255（PWM の 255 分率）。
// 起動したときは 7。0 にするとバックライトが消える。
//
// - 暗いとき（暗くしたか、消したとき）にタッチしたら、明るくするだけで、そのタッチではセルを操作しない（誤操作を防ぐ）。
//   離すまでのサンプルはすべて捨てる。
// - 本体キーは、暗いときもそのまま PC に送る。wake_on_key なら、キーでも明るくする（押すたびに時間を数え直す）。
// - 端末モードのあいだは、in_terminal でなければ暗くしない。
// - 明るさの書き込みは別の goroutine で行い、入力の処理を待たせない。
// - 終わるときは、起動したときの明るさに戻す。強制終了などで戻せなかったときは、-restore-console（ExecStopPost）が
//   /run/lefthand-backlight に書いた明るさに戻す。

// テストで差し替える
var (
	backlightClass     = "/sys/class/backlight"
	backlightStateFile = "/run/lefthand-backlight" // 戻す明るさとデバイス。暗くしているあいだだけある
)

const (
	minDimAfter = 5 * time.Second
	maxDimAfter = 24 * time.Hour
	durationOff = "off"
)

// BacklightConfig は設定の backlight。
type BacklightConfig struct {
	Device     string `yaml:"device,omitempty" json:"device,omitempty"`           // /sys/class/backlight/<名前>。省略すると最初のもの
	DimAfter   string `yaml:"dim_after,omitempty" json:"dim_after,omitempty"`     // 触らずにこの時間がたったら暗くする（1m など）。off か省略で暗くしない
	OffAfter   string `yaml:"off_after,omitempty" json:"off_after,omitempty"`     // 触らずにこの時間がたったら消す。off か省略で消さない
	Level      *int   `yaml:"level,omitempty" json:"level,omitempty"`             // ふだんの明るさ。省略すると起動したときの明るさ
	DimLevel   *int   `yaml:"dim_level,omitempty" json:"dim_level,omitempty"`     // 暗くしたときの明るさ。省略すると 1
	WakeOnKey  *bool  `yaml:"wake_on_key,omitempty" json:"wake_on_key,omitempty"` // 本体キーでも明るくする（既定 true）
	InTerminal bool   `yaml:"in_terminal,omitempty" json:"in_terminal,omitempty"` // 端末モードのあいだも暗くする（既定 false）
}

// dimParams は、組み立て済みの backlight。
type dimParams struct {
	device     string
	dimAfter   time.Duration // 0 なら暗くしない
	offAfter   time.Duration // 0 なら消さない
	level      int           // -1 なら起動したときの明るさ
	dimLevel   int
	wakeOnKey  bool
	inTerminal bool
}

// compile は backlight を検証する。nil なら、自動減光をしない（nil を返す）。
func (c *BacklightConfig) compile() (*dimParams, error) {
	if c == nil {
		return nil, nil
	}
	p := &dimParams{device: c.Device, level: -1, dimLevel: 1, wakeOnKey: true, inTerminal: c.InTerminal}
	if c.Device != "" && !strings.HasPrefix(c.Device, backlightClass+"/") {
		return nil, fmt.Errorf("device must be %s/<name>, got %q", backlightClass, c.Device)
	}
	dur := func(name, s string) (time.Duration, error) {
		if s == "" || s == durationOff {
			return 0, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < minDimAfter || d > maxDimAfter {
			return 0, fmt.Errorf("%s must be a duration from %v to %v (such as 30s or 5m), or off", name, minDimAfter, maxDimAfter)
		}
		return d, nil
	}
	var err error
	if p.dimAfter, err = dur("dim_after", c.DimAfter); err != nil {
		return nil, err
	}
	if p.offAfter, err = dur("off_after", c.OffAfter); err != nil {
		return nil, err
	}
	if p.dimAfter > 0 && p.offAfter > 0 && p.offAfter <= p.dimAfter {
		return nil, fmt.Errorf("off_after (%v) must be longer than dim_after (%v)", p.offAfter, p.dimAfter)
	}
	lvl := func(name string, v *int, dst *int) error {
		if v == nil {
			return nil
		}
		if *v < 0 || *v > 1000 {
			return fmt.Errorf("%s must be 0..max_brightness (0..7 on the PW-SH2)", name)
		}
		*dst = *v
		return nil
	}
	if err := lvl("level", c.Level, &p.level); err != nil {
		return nil, err
	}
	if c.Level != nil && *c.Level == 0 {
		return nil, fmt.Errorf("level must be 1 or more (0 turns the backlight off)")
	}
	if err := lvl("dim_level", c.DimLevel, &p.dimLevel); err != nil {
		return nil, err
	}
	if c.WakeOnKey != nil {
		p.wakeOnKey = *c.WakeOnKey
	}
	return p, nil
}

func (c *BacklightConfig) validate(fail func(path, format string, args ...any)) {
	if _, err := c.compile(); err != nil {
		fail("/backlight", "backlight: %v", err)
	}
}

// 明るさの段階
const (
	dimBright = iota
	dimDim
	dimOff
)

var dimNames = [...]string{"bright", "dim", "off"}

// Backlight は /sys/class/backlight/<名前> の 1 つ。
type Backlight struct {
	dir  string
	max  int
	orig int // 起動したときの明るさ（戻す明るさ）
}

// openBacklight は device（空なら最初のもの）を開く。
func openBacklight(device string) (*Backlight, error) {
	dir := device
	if dir == "" {
		ds, _ := filepath.Glob(filepath.Join(backlightClass, "*"))
		if len(ds) == 0 {
			return nil, errors.New("no backlight in " + backlightClass)
		}
		dir = ds[0]
	}
	max, err := readSysInt(filepath.Join(dir, "max_brightness"))
	if err != nil {
		return nil, err
	}
	cur, err := readSysInt(filepath.Join(dir, "brightness"))
	if err != nil {
		return nil, err
	}
	b := &Backlight{dir: dir, max: max, orig: cur}
	// 前のデーモンが暗くしたまま終わっていたら、そのとき戻すはずだった明るさを使う
	if s, err := os.ReadFile(backlightStateFile); err == nil {
		var lv int
		var d string
		if _, err := fmt.Sscanf(string(s), "%d %s", &lv, &d); err == nil && d == dir && lv > 0 && lv <= max {
			b.orig = lv
		}
	}
	if b.orig <= 0 {
		b.orig = max
	}
	return b, nil
}

func (b *Backlight) write(level int) error {
	return os.WriteFile(filepath.Join(b.dir, "brightness"), []byte(strconv.Itoa(level)+"\n"), 0)
}

// restoreBacklight は、暗くしたまま終わったデーモンの明るさを戻す（-restore-console）。
func restoreBacklight() {
	s, err := os.ReadFile(backlightStateFile)
	if err != nil {
		return
	}
	var lv int
	var dir string
	if _, err := fmt.Sscanf(string(s), "%d %s", &lv, &dir); err == nil && strings.HasPrefix(dir, backlightClass+"/") && lv > 0 {
		b := &Backlight{dir: dir}
		if err := b.write(lv); err != nil {
			log.Printf("restore backlight: %v", err)
		} else {
			log.Printf("restored backlight %s to %d", dir, lv)
		}
	}
	os.Remove(backlightStateFile)
}

// Dimmer は自動減光を行う。p が nil なら何もしない（タッチも捨てない）。
type Dimmer struct {
	mu      sync.Mutex
	p       *dimParams
	bl      *Backlight
	state   int
	holding bool // 指が触れている
	term    bool // 端末モードのあいだ
	timer   *time.Timer
	gen     uint64 // タイマーを数え直すたびに増やす（古いタイマーを無効にする）
	writes  chan int
	written int // 最後に書いた明るさ（書く goroutine だけが触る）
	closed  bool
	since   time.Time // 今の段階になった時刻
}

// NewDimmer は、設定の backlight で自動減光を始める。p が nil なら何もしない。
func NewDimmer(p *dimParams) *Dimmer {
	d := &Dimmer{writes: make(chan int, 1), written: -1}
	go d.writer()
	d.Apply(p)
	return d
}

// Apply は設定を差し替える（設定 GUI からの保存）。明るくして、時間を数え直す。
func (d *Dimmer) Apply(p *dimParams) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if p != nil && (d.bl == nil || (p.device != "" && p.device != d.bl.dir)) {
		bl, err := openBacklight(p.device)
		if err != nil {
			log.Printf("backlight: %v (auto dimming is off)", err)
			p = nil
		} else {
			if d.bl != nil && d.bl.dir != bl.dir { // 前のデバイスを戻す
				d.queueLocked(d.bl.orig)
			}
			d.bl = bl
			log.Printf("backlight: %s, %d/%d", bl.dir, bl.orig, bl.max)
		}
	}
	old := d.p
	d.p = p
	if p == nil {
		if old != nil && d.bl != nil {
			d.setLocked(dimBright) // 自動減光をやめたら、明るくしておく
		}
		d.stopTimerLocked()
		return
	}
	for _, x := range []struct {
		name string
		v    *int
	}{{"level", &p.level}, {"dim_level", &p.dimLevel}} {
		if *x.v > d.bl.max {
			log.Printf("backlight: %s %d is more than max_brightness %d; using %d", x.name, *x.v, d.bl.max, d.bl.max)
			*x.v = d.bl.max
		}
	}
	d.state = -1 // 同じ段階でも、新しい明るさを書く
	d.activeLocked()
}

// brightLevel は、ふだんの明るさ。
func (d *Dimmer) brightLevel() int {
	if d.p.level > 0 {
		return d.p.level
	}
	return d.bl.orig
}

// setLocked は段階を変え、明るさを書く（待たない）。
func (d *Dimmer) setLocked(st int) {
	if st == d.state {
		return
	}
	lv := d.brightLevel()
	switch st {
	case dimDim:
		lv = d.p.dimLevel
	case dimOff:
		lv = 0
	}
	if d.state >= 0 || st != dimBright {
		vlogf("backlight: %s (%d)", dimNames[st], lv)
	}
	d.state = st
	d.since = time.Now()
	d.queueLocked(lv)
}

func (d *Dimmer) queueLocked(lv int) {
	select {
	case <-d.writes: // 書く前のものは捨て、最新だけを書く
	default:
	}
	d.writes <- lv
}

// writer は明るさを書く。書くのは、変わったときだけ。暗くしているあいだは、戻す明るさを /run に残す。
func (d *Dimmer) writer() {
	for lv := range d.writes {
		d.mu.Lock()
		bl := d.bl
		d.mu.Unlock()
		if bl == nil || lv == d.written {
			continue
		}
		if lv < bl.orig {
			os.WriteFile(backlightStateFile, []byte(fmt.Sprintf("%d %s\n", bl.orig, bl.dir)), 0o644)
		}
		if err := bl.write(lv); err != nil {
			log.Printf("backlight: %v", err)
			continue
		}
		d.written = lv
		if lv >= bl.orig {
			os.Remove(backlightStateFile)
		}
	}
}

func (d *Dimmer) stopTimerLocked() {
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// activeLocked は、明るくして、時間を数え直す。
func (d *Dimmer) activeLocked() {
	d.setLocked(dimBright)
	d.stopTimerLocked()
	if d.holding || (d.term && !d.p.inTerminal) {
		return // 触れているあいだと、端末モードのあいだは暗くしない
	}
	d.scheduleLocked(time.Now())
}

// scheduleLocked は、次に暗くする（消す）時刻にタイマーを掛ける。
func (d *Dimmer) scheduleLocked(from time.Time) {
	var next time.Duration
	var st int
	switch d.state {
	case dimBright:
		switch {
		case d.p.dimAfter > 0:
			next, st = d.p.dimAfter, dimDim
		case d.p.offAfter > 0:
			next, st = d.p.offAfter, dimOff
		default:
			return
		}
	case dimDim:
		if d.p.offAfter == 0 {
			return
		}
		next, st = d.p.offAfter-d.p.dimAfter, dimOff
	default:
		return
	}
	gen := d.gen
	d.timer = time.AfterFunc(time.Until(from.Add(next)), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.gen != gen || d.p == nil || d.closed {
			return
		}
		d.setLocked(st)
		d.scheduleLocked(time.Now())
	})
}

// TouchDown は、タッチしたときに、ほかの処理より先に呼ぶ。true なら、そのタッチを捨てる（暗かったので明るくしただけ）。
func (d *Dimmer) TouchDown() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p == nil || d.closed {
		return false
	}
	dark := d.state != dimBright
	d.holding = true
	d.activeLocked()
	if dark {
		log.Printf("backlight: touch woke the screen (the touch is ignored)")
	}
	return dark
}

// TouchUp は、指を離したときに呼ぶ。そこから時間を数え直す。
func (d *Dimmer) TouchUp() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p == nil || d.closed {
		return
	}
	d.holding = false
	d.activeLocked()
}

// Key は、本体キーを押したときに呼ぶ。キーはいつも PC に送る（ここでは捨てない）。
func (d *Dimmer) Key() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p == nil || d.closed || !d.p.wakeOnKey {
		return
	}
	d.activeLocked()
}

// Wake は、知らせることがあるとき（ポモドーロの切り替わり）に明るくする。
func (d *Dimmer) Wake() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p == nil || d.closed {
		return
	}
	d.activeLocked()
}

// SetTerminal は、端末モードに入った・抜けたときに呼ぶ。
func (d *Dimmer) SetTerminal(on bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.term == on {
		return
	}
	d.term = on
	if d.p != nil && !d.closed {
		d.activeLocked()
	}
}

// DimInfo は get_status の backlight。
type DimInfo struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state,omitempty"` // bright、dim、off
	Level   int    `json:"level,omitempty"` // 今の明るさ
	Max     int    `json:"max,omitempty"`
	Device  string `json:"device,omitempty"`
}

// Info は今の状態。
func (d *Dimmer) Info() DimInfo {
	if d == nil {
		return DimInfo{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p == nil || d.bl == nil || d.state < 0 {
		return DimInfo{}
	}
	lv := d.brightLevel()
	switch d.state {
	case dimDim:
		lv = d.p.dimLevel
	case dimOff:
		lv = 0
	}
	return DimInfo{Enabled: true, State: dimNames[d.state], Level: lv, Max: d.bl.max, Device: d.bl.dir}
}

// Close は、起動したときの明るさに戻す（デーモンが終わるとき）。
func (d *Dimmer) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.stopTimerLocked()
	bl := d.bl
	d.mu.Unlock()
	if bl == nil {
		return
	}
	if err := bl.write(bl.orig); err != nil {
		log.Printf("backlight: %v", err)
	}
	os.Remove(backlightStateFile)
}
