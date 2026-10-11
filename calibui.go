package main

import (
	"fmt"
	"image"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- タッチのキャリブレーションの画面 ----------
//
// 「本体」→「タッチ調整」（calibrate: touch）か brain-deck calibrate で始める。画面（向き 0、ソフトキーの帯は右）に
// 十字を 1 つずつ出し、押して離してもらう。点は四隅から少し内側の 4 点と真ん中（5 点。touch9 なら 3×3 の 9 点）。
// 押しているあいだの生の座標を集め、始めと終わりの数回を捨てた残りのまん中の値を使う。次のときは、その点をやり直してもらう：
//   - 押す時間が短い（読めた回数が少ない）、押しているあいだに大きく動いた
//   - 今の変換で、十字から大きく外れている（ほかの十字を押した）
//   - 全部を押したあと、ほかの点で決めた変換で 1 点だけ大きく外れる
// 終わったら確かめの画面：押した場所に点を描く（新しい変換で）。「保存」（か 決定のソフトキー）で保存し、
// 「やり直す」で最初から、「やめる」（か 戻るのソフトキー、本体の Esc）で前のまま。保存しなければ何も変わらない。
// あいだは、タッチと本体キーをこの画面が使う（PC には送らない）。3 分さわらなければやめる。

const (
	calScreenW, calScreenH = 800, 480 // 向き 0 の画面
	calSettle              = 2        // 押し始めに捨てる回数
	calTail                = 2        // 離す前に捨てる回数
	calMinSamples          = 4        // 捨てたあとに残っていなければならない回数
	calMaxSpread           = 10.0     // 押しているあいだに動いてよい距離（ドット）
	calMaxMiss             = 100.0    // 今の変換で、十字からこれより離れていたら押し間違い（ドット）
	calOutlierLimit        = 20.0     // ほかの点で決めた変換で、これより外れたらやり直す（ドット）
	calMaxRedo             = 3        // 外れた点のやり直しの回数の上限（超えたら、そのまま確かめの画面へ）
	calIdle                = 3 * time.Minute
)

const (
	calOff = iota
	calTarget
	calVerify
)

// calibrateKinds は、設定の calibrate と点の数。calibrateLabels は、セルに出す名前。
var (
	calibrateKinds  = map[string]int{"touch": 5, "touch9": 9}
	calibrateLabels = map[string]string{"touch": "タッチ調整", "touch9": "タッチ調整(9点)"}
)

// calTargets は、n 点（5 か 9）の十字の位置（向き 0 の画面のドット）。四隅から 1 割内側。
func calTargets(n int) [][2]float64 {
	xs := []float64{calScreenW * 0.1, calScreenW * 0.5, calScreenW * 0.9}
	ys := []float64{calScreenH * 0.1, calScreenH * 0.5, calScreenH * 0.9}
	if n == 9 {
		var out [][2]float64
		for _, y := range ys {
			for _, x := range xs {
				out = append(out, [2]float64{x, y})
			}
		}
		return out
	}
	return [][2]float64{{xs[0], ys[0]}, {xs[2], ys[0]}, {xs[2], ys[2]}, {xs[0], ys[2]}, {xs[1], ys[1]}}
}

// calButtons は、確かめの画面のボタン（向き 0 の画面）。9 点の十字（1 割内側と真ん中）に重ならない高さ。
var calButtons = []struct {
	name string
	r    image.Rectangle
}{
	{"保存", image.Rect(150, 300, 310, 360)},
	{"やり直す", image.Rect(320, 300, 480, 360)},
	{"やめる", image.Rect(490, 300, 650, 360)},
}

// CalibUI は、キャリブレーションの画面と手順。
type CalibUI struct {
	mu      sync.Mutex
	dataDir string
	tc      func() *TouchConfig                     // 今の設定の touch（測る前の変換に使う）
	show    func(draw func(cv *Canvas) bool)        // 画面を全部使って描く（nil でやめる）。Display.SetFullScreen
	notify  func(msg string, level int)             // 終わったときの知らせ
	wake    func()                                  // 画面を明るくする
	timer   func(time.Duration, func()) func() bool // タイマー（テストで差し替える）

	state    int
	n        int          // 点の数
	targets  [][2]float64 // 十字
	pairs    []calPair    // 押した点（targets と同じ順。まだのものは RX が NaN）
	cur      int          // 今の十字
	redo     int          // 外れた点をやり直した回数
	msg      string       // 十字の下に出す言葉（やり直すわけなど）
	samples  [][2]int32   // 押しているあいだの生の座標
	down     bool
	result   [6]float64 // 確かめの画面の変換
	before   CalError
	after    CalError
	dots     [][2]float64 // 確かめの画面で押した場所
	source   string
	stopIdle func() bool
	last     *CalStatus // 最後の結果（brain-deck calibrate が待つ）
	gen      int
}

// CalStatus は、キャリブレーションの状態（protocol の calibrate）。
type CalStatus struct {
	State  string    `json:"state"`            // off、target、verify
	Point  int       `json:"point,omitempty"`  // 今の十字（1 から）
	Points int       `json:"points,omitempty"` // 点の数
	Result string    `json:"result,omitempty"` // 最後に終わったとき：saved、cancelled、timeout
	Before *CalError `json:"before,omitempty"`
	After  *CalError `json:"after,omitempty"`
	Gen    int       `json:"gen"` // 始めるたびに増える
}

func NewCalibUI(dataDir string) *CalibUI {
	return &CalibUI{dataDir: dataDir, timer: func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }}
}

func (c *CalibUI) Active() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state != calOff
}

// Start は、n 点（5 か 9）で始める。すでに始めていれば、最初からやり直す。
func (c *CalibUI) Start(n int, source string) error {
	if c == nil {
		return fmt.Errorf("touch calibration is not available")
	}
	if n != 5 && n != 9 {
		return fmt.Errorf("points must be 5 or 9")
	}
	c.mu.Lock()
	c.n, c.source = n, source
	c.gen++
	c.last = nil
	c.restartLocked()
	c.mu.Unlock()
	log.Printf("touch: calibration started (%d points, from %s)", n, source)
	if c.wake != nil {
		c.wake()
	}
	c.redrawSoon()
	return nil
}

func (c *CalibUI) restartLocked() {
	c.state, c.cur, c.redo, c.msg, c.dots = calTarget, 0, 0, "", nil
	c.targets = calTargets(c.n)
	c.pairs = make([]calPair, len(c.targets))
	for i, t := range c.targets {
		c.pairs[i] = calPair{SX: t[0], SY: t[1], RX: math.NaN(), RY: math.NaN()}
	}
	c.samples, c.down = nil, false
	c.touchIdleLocked()
}

func (c *CalibUI) touchIdleLocked() {
	if c.stopIdle != nil {
		c.stopIdle()
	}
	gen := c.gen
	c.stopIdle = c.timer(calIdle, func() {
		c.mu.Lock()
		if c.gen != gen || c.state == calOff {
			c.mu.Unlock()
			return
		}
		c.finishLocked("timeout")
		c.mu.Unlock()
		c.redrawSoon()
		c.say("タッチ調整をやめました（3 分さわらなかったので、前のまま）", noticeWarn)
	})
}

func (c *CalibUI) say(msg string, level int) {
	if c.notify != nil {
		c.notify(msg, level)
	}
}

// redrawSoon は、画面を描き直す（終わっていれば、画面を元に戻す）。
func (c *CalibUI) redrawSoon() {
	if c.show == nil {
		return
	}
	if c.Active() {
		c.show(c.draw)
	} else {
		c.show(nil)
	}
}

// Cancel は、保存せずにやめる。
func (c *CalibUI) Cancel() {
	c.mu.Lock()
	if c.state == calOff {
		c.mu.Unlock()
		return
	}
	c.finishLocked("cancelled")
	c.mu.Unlock()
	c.redrawSoon()
	c.say("タッチ調整をやめました（前のまま）", noticeInfo)
}

func (c *CalibUI) finishLocked(result string) {
	st := &CalStatus{State: "off", Result: result, Points: c.n, Gen: c.gen}
	if c.state == calVerify {
		b, a := c.before, c.after
		st.Before, st.After = &b, &a
	}
	c.last = st
	c.state = calOff
	if c.stopIdle != nil {
		c.stopIdle()
		c.stopIdle = nil
	}
	log.Printf("touch: calibration %s", result)
}

// Status は、今の状態か、最後に終わったときの結果。
func (c *CalibUI) Status() CalStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case calTarget:
		return CalStatus{State: "target", Point: c.cur + 1, Points: c.n, Gen: c.gen}
	case calVerify:
		b, a := c.before, c.after
		return CalStatus{State: "verify", Points: c.n, Before: &b, After: &a, Gen: c.gen}
	}
	if c.last != nil {
		return *c.last
	}
	return CalStatus{State: "off", Gen: c.gen}
}

// oldMap は、測る前の変換（今のキャリブレーションか、設定の min と max）で、生の座標を向き 0 の画面のドットにする。
func (c *CalibUI) oldMap(x, y float64) (float64, float64) {
	if X, Y, ok := calPhys(int32(math.Round(x)), int32(math.Round(y)), calScreenW, calScreenH); ok {
		return X, Y
	}
	tc := c.tc()
	if tc.SwapXY {
		x, y = y, x
	}
	f := func(v float64, lo, hi int32, n int) float64 {
		if hi == lo {
			return 0
		}
		return (v - float64(lo)) * float64(n) / float64(hi-lo)
	}
	return f(x, tc.MinX, tc.MaxX, calScreenW), f(y, tc.MinY, tc.MaxY, calScreenH)
}

// Touch は、生のタッチ（touchProc から。押しているあいだ読むたびと、離したとき）。
func (c *CalibUI) Touch(down bool, x, y int32) {
	c.mu.Lock()
	if c.state == calOff {
		c.mu.Unlock()
		return
	}
	c.touchIdleLocked()
	if down {
		c.down = true
		c.samples = append(c.samples, [2]int32{x, y})
		c.mu.Unlock()
		return
	}
	if !c.down {
		c.mu.Unlock()
		return
	}
	s := c.samples
	c.down, c.samples = false, nil
	var save bool
	switch c.state {
	case calTarget:
		c.pressLocked(s)
	case calVerify:
		save = c.verifyTapLocked(s)
	}
	c.mu.Unlock()
	if save {
		c.save()
	}
	c.redrawSoon()
}

// pressLocked は、十字を押して離したとき。
func (c *CalibUI) pressLocked(s [][2]int32) {
	if len(s) < calSettle+calTail+calMinSamples {
		c.msg = "短すぎました。十字を押して、少し止めてから離してください"
		return
	}
	s = s[calSettle : len(s)-calTail]
	xs, ys := make([]float64, len(s)), make([]float64, len(s))
	for i, p := range s {
		xs[i], ys[i] = float64(p[0]), float64(p[1])
	}
	mx, my := median(xs), median(ys)
	// 動いた距離は、今の変換のドットで測る
	X0, Y0 := c.oldMap(mx, my)
	spread := 0.0
	for _, p := range s {
		X, Y := c.oldMap(float64(p[0]), float64(p[1]))
		spread = max(spread, math.Hypot(X-X0, Y-Y0))
	}
	if spread > calMaxSpread {
		c.msg = fmt.Sprintf("押しているあいだに動きました（%.0f ドット）。もう一度、動かさずに押してください", spread)
		return
	}
	t := c.targets[c.cur]
	if d := math.Hypot(X0-t[0], Y0-t[1]); d > calMaxMiss {
		c.msg = fmt.Sprintf("十字から離れています（%.0f ドット）。光っている十字を押してください", d)
		return
	}
	c.pairs[c.cur].RX, c.pairs[c.cur].RY = mx, my
	c.msg = ""
	// まだ押していない点へ
	for i := range c.pairs {
		if math.IsNaN(c.pairs[i].RX) {
			c.cur = i
			return
		}
	}
	c.computeLocked()
}

// computeLocked は、全部の点がそろったときに、外れた点を調べて、変換を決める。
func (c *CalibUI) computeLocked() {
	if i, d := calOutlier(c.pairs, calOutlierLimit); i >= 0 && c.redo < calMaxRedo {
		c.redo++
		c.cur = i
		c.pairs[i].RX, c.pairs[i].RY = math.NaN(), math.NaN()
		c.msg = fmt.Sprintf("この点だけ、ほかと合いません（%.0f ドット）。もう一度押してください", d)
		log.Printf("touch: calibration point %d is an outlier (%.1f dots); asking again", i+1, d)
		return
	}
	a, err := fitAffine(c.pairs)
	if err != nil {
		c.msg = "計算できませんでした。最初からやり直します：" + err.Error()
		c.restartLocked()
		return
	}
	c.result = a
	c.before, _ = calErrors(c.pairs, c.oldMap)
	c.after, _ = calErrors(c.pairs, affineFunc(a))
	c.state, c.msg, c.dots = calVerify, "", nil
	log.Printf("touch: calibration computed: before %s, after %s", c.before, c.after)
}

// verifyTapLocked は、確かめの画面で押したとき。保存するなら true。
func (c *CalibUI) verifyTapLocked(s [][2]int32) bool {
	if len(s) == 0 {
		return false
	}
	p := s[len(s)/2]
	X, Y := affineFunc(c.result)(float64(p[0]), float64(p[1]))
	for _, b := range calButtons {
		if image.Pt(int(X), int(Y)).In(b.r) {
			switch b.name {
			case "保存":
				return true
			case "やり直す":
				c.restartLocked()
				return false
			case "やめる":
				c.finishLocked("cancelled")
				return false
			}
		}
	}
	c.dots = append(c.dots, [2]float64{X, Y})
	if len(c.dots) > 40 {
		c.dots = c.dots[1:]
	}
	return false
}

// save は、確かめの画面の変換を保存して使う。
func (c *CalibUI) save() {
	c.mu.Lock()
	if c.state != calVerify {
		c.mu.Unlock()
		return
	}
	tc := c.tc()
	f := &calFile{Version: touchCalVersion, Affine: c.result, W: calScreenW, H: calScreenH, Before: c.before, After: c.after,
		Config: [4]int32{tc.MinX, tc.MaxX, tc.MinY, tc.MaxY}, SwapXY: tc.SwapXY, Created: time.Now(), Source: c.source}
	for _, p := range c.pairs {
		f.Points = append(f.Points, calPoint{Screen: [2]float64{p.SX, p.SY}, Raw: [2]float64{p.RX, p.RY}})
	}
	err := saveTouchCal(c.dataDir, f)
	if err == nil {
		c.finishLocked("saved")
	}
	c.mu.Unlock()
	if err != nil {
		c.say("タッチ調整を保存できませんでした："+err.Error(), noticeError)
		return
	}
	c.say(fmt.Sprintf("タッチ調整を保存しました（ずれ：前 %s → 後 %s）", c.before, c.after), noticeInfo)
}

// SoftKey は、ソフトキーの帯（生の座標で決める）を押したとき。使ったら true。
func (c *CalibUI) SoftKey(name string) bool {
	switch name {
	case "back", "home":
		c.Cancel()
		return true
	case "enter":
		c.mu.Lock()
		v := c.state == calVerify
		c.mu.Unlock()
		if v {
			c.save()
			c.redrawSoon()
		}
		return true
	}
	return c.Active() // ほかのソフトキーは何もしない
}

// Key は、本体キー（押したとき）。Esc でやめる、決定で保存（確かめの画面）。キーは PC に送らない。
func (c *CalibUI) Key(code evdev.EvCode) {
	switch code {
	case evdev.KEY_ESC, evdev.KEY_BACKSPACE:
		c.Cancel()
	case evdev.KEY_ENTER:
		c.SoftKey("enter")
	}
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ---------- 描く ----------

var (
	colCalTarget = RGB{0xff, 0xd0, 0x40}
	colCalDone   = RGB{0x60, 0x70, 0x80}
	colCalDot    = RGB{0xff, 0x50, 0x50}
	colCalButton = RGB{0x1c, 0x28, 0x38}
)

func drawCross(cv *Canvas, x, y float64, size int, c RGB) {
	X, Y := int(math.Round(x)), int(math.Round(y))
	cv.fill(image.Rect(X-size, Y, X+size+1, Y+1), c)
	cv.fill(image.Rect(X, Y-size, X+1, Y+size+1), c)
	cv.fill(image.Rect(X-2, Y-2, X+3, Y+3), c)
}

// draw は、向き 0 の画面（800×480）に、今の状態を描く。Display.SetFullScreen から呼ぶ。
func (c *CalibUI) draw(cv *Canvas) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == calOff {
		return false
	}
	full := image.Rect(0, 0, cv.W, cv.H)
	cv.fill(full, colBG)
	head := image.Rect(0, 0, cv.W, 24)
	switch c.state {
	case calTarget:
		for i, t := range c.targets {
			if i == c.cur {
				continue
			}
			if !math.IsNaN(c.pairs[i].RX) {
				drawCross(cv, t[0], t[1], 6, colCalDone)
			}
		}
		t := c.targets[c.cur]
		drawCross(cv, t[0], t[1], 16, colCalTarget)
		drawCentered(cv, head, 4, fmt.Sprintf("タッチ調整 %d/%d：光っている十字の真ん中を、ペン先か爪で押して、少し止めてから離す", c.cur+1, len(c.targets)), 1, colText)
		if c.msg != "" {
			drawCentered(cv, full, cv.H/2+40, clipText(c.msg, cv.W-40, 2), 2, colInkWarn)
		}
		drawCentered(cv, full, cv.H-20, "戻る（ソフトキー）か Esc でやめる（前のまま）", 1, colSub)
	case calVerify:
		for _, t := range c.targets {
			drawCross(cv, t[0], t[1], 10, colCalDone)
		}
		for _, d := range c.dots {
			X, Y := int(math.Round(d[0])), int(math.Round(d[1]))
			cv.fill(image.Rect(X-3, Y-3, X+4, Y+4), colCalDot)
		}
		drawCentered(cv, head, 4, "確かめ：画面を押すと、押した場所に赤い点を描きます。十字の真ん中を押して、点が重なるか見てください", 1, colText)
		drawCentered(cv, full, 120, fmt.Sprintf("ずれ　前：%s　→　後：%s", c.before, c.after), 1, colText)
		for _, b := range calButtons {
			cv.fill(b.r, colCalButton)
			cv.frame(b.r, 2, colCalDone)
			drawCentered(cv, b.r, b.r.Min.Y+(b.r.Dy()-fontH*2)/2, b.name, 2, colText)
		}
		drawCentered(cv, full, 372, "保存しなければ前のまま。決定（ソフトキー）でも保存、戻るでやめる", 1, colSub)
	}
	return true
}
