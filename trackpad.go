package main

import (
	"fmt"
	"image"
	"math"
	"sync"
	"time"
)

// ---------- トラックパッド（widget: trackpad） ----------
//
// セルの中で指を動かすとカーソルが動く。短いタップで左クリック、タップしてすぐ触れて動かすとドラッグ、
// 右端の帯（scroll_width）をなぞると縦のスクロール（帯ではタップしてもクリックしない）。
// 長押しの右クリックは、既定では使わない（long_press: right で使う）。
//
// タッチパネル（mxs-lradc-ts）は抵抗膜で、サンプルは 26 ms ごと。触れた直後と離す直前は押す強さが弱く、座標が跳ねる。
// 止めていても少し揺れ、押す強さが変わるにつれて位置がゆっくり流れる。なぞる途中で一瞬離れたことになることもある。そのため、
//   - 押す強さ（ABS_PRESSURE）が min_pressure より弱いサンプルを捨てる。そういうサンプルしかないタッチは、なかったことにする
//   - 触れてから settle_ms のあいだのサンプルを捨てる
//   - 最後の smooth 個のサンプルの平均を使う
//   - 平均の位置から deadzone ドット以内の揺れは無視する（ヒステリシス。動き始めにも効く）。その内側のごく遅い流れも送らない（padCreep）
//   - 離す直前のサンプルは padHoldBack 個遅らせて使い、離したときに捨てる（離す瞬間の跳ね）
//   - タップでないタッチのあと、padLiftGrace のうちに触れ直せば、同じタッチの続きにする
// 判定は、時刻を受け取る状態機械（Pad）で行う。実機ではイベントを受け取った時刻を、
// 記録したタッチの再生（-replay-touch、テスト）では記録した時刻を使うので、同じ結果になる。

const widgetPad = "trackpad"

// PadParams は、組み立て済みのトラックパッドの設定。
type PadParams struct {
	Speed       float64       // 画面の 1 ドットの動きを、PC のマウスの何カウントにするか（遅いとき）
	Accel       float64       // 加速。速さ v（ドット/秒）のとき、Speed × (1 + Accel × min(v/padAccelRef, padAccelCap)) 倍
	ScrollWidth int           // 右端のスクロールの帯の幅（ドット）。0 なら帯なし
	Natural     bool          // true：指を下へ動かすと中身が下へ動く（上へスクロール）。false：ホイールを下へ回したのと同じ
	ScrollStep  float64       // ホイール 1 段に当たる指の動き（ドット）
	Settle      time.Duration // 触れた直後に捨てる時間
	Smooth      int           // 平均を取るサンプルの数（1 で平均しない）
	Deadzone    float64       // 無視する小さな動き（ドット）
	MinPressure int32         // これより弱いサンプルを捨てる。0 なら見ない
	TapTime     time.Duration // これより短く触れて離せばタップ
	TapMove     float64       // タップとみなす動きの上限（ドット）
	DragGap     time.Duration // タップのあと、これより早く触れればドラッグ。0 ならタップですぐクリックする
	LongPress   bool          // 長押しで右クリック
}

// 既定値。2026-10-08 に実機で記録したタッチ（testdata/touch）から決めた。根拠は docs/config.md の「既定値の根拠」
var defaultPad = PadParams{
	Speed:       1.2,
	Accel:       2.0,
	ScrollWidth: 96,
	Natural:     true,
	ScrollStep:  24,
	Settle:      80 * time.Millisecond,
	Smooth:      3,
	Deadzone:    1.5,
	MinPressure: 500,
	TapTime:     250 * time.Millisecond,
	TapMove:     8,
	DragGap:     300 * time.Millisecond,
}

const (
	padAccelRef   = 1000.0 // 加速の基準の速さ（ドット/秒）
	padAccelCap   = 3.0    // 加速の上限（padAccelRef の何倍の速さまで加速を強めるか）
	padLongPress  = 600 * time.Millisecond
	padMaxSmooth  = 10
	padSourceL    = "pad:left"
	padSourceR    = "pad:right"
	scrollNatural = "natural"
	scrollWheel   = "traditional"
	longPressNone = "none"
	longPressR    = "right"
)

// compilePad は、トラックパッドのセルの書き方を検証して組み立てる。
func compilePad(s ActionSpec) (*WidgetDef, error) {
	if s.Format != "" || s.DateFormat != "" || s.TZ != "" || s.ID != "" || s.Rows != 0 || s.PageReset != "" || s.Stale != "" || s.Calendars != nil {
		return nil, fmt.Errorf("format, date_format, tz, id, rows, page_reset, stale and calendars cannot be used with widget: trackpad")
	}
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: trackpad handles taps itself (tap to click); remove key, layer_* and mouse (put mouse buttons in other cells)")
	}
	p := defaultPad
	f := func(v *float64, dst *float64, name string, lo, hi float64) error {
		if v == nil {
			return nil
		}
		if math.IsNaN(*v) || *v < lo || *v > hi {
			return fmt.Errorf("%s must be %g..%g", name, lo, hi)
		}
		*dst = *v
		return nil
	}
	ms := func(v *int, dst *time.Duration, name string, lo, hi int) error {
		if v == nil {
			return nil
		}
		if *v < lo || *v > hi {
			return fmt.Errorf("%s must be %d..%d (milliseconds)", name, lo, hi)
		}
		*dst = time.Duration(*v) * time.Millisecond
		return nil
	}
	in := func(v *int, dst *int, name string, lo, hi int) error {
		if v == nil {
			return nil
		}
		if *v < lo || *v > hi {
			return fmt.Errorf("%s must be %d..%d", name, lo, hi)
		}
		*dst = *v
		return nil
	}
	var mp int = int(p.MinPressure)
	for _, err := range []error{
		f(s.Speed, &p.Speed, "speed", 0.05, 20),
		f(s.Accel, &p.Accel, "accel", 0, 10),
		in(s.ScrollWidth, &p.ScrollWidth, "scroll_width", 0, 400),
		f(s.ScrollStep, &p.ScrollStep, "scroll_step", 2, 400),
		ms(s.SettleMS, &p.Settle, "settle_ms", 0, 500),
		in(s.Smooth, &p.Smooth, "smooth", 1, padMaxSmooth),
		f(s.Deadzone, &p.Deadzone, "deadzone", 0, 50),
		in(s.MinPressure, &mp, "min_pressure", 0, 4095),
		ms(s.TapMS, &p.TapTime, "tap_ms", 30, 1000),
		f(s.TapMove, &p.TapMove, "tap_move", 0, 200),
		ms(s.DragMS, &p.DragGap, "drag_ms", 0, 1000),
	} {
		if err != nil {
			return nil, err
		}
	}
	p.MinPressure = int32(mp)
	switch s.ScrollDirection {
	case "", scrollNatural:
	case scrollWheel:
		p.Natural = false
	default:
		return nil, fmt.Errorf("scroll_direction must be %s or %s", scrollNatural, scrollWheel)
	}
	switch s.LongPress {
	case "", longPressNone:
	case longPressR:
		p.LongPress = true
	default:
		return nil, fmt.Errorf("long_press must be %s or %s", longPressNone, longPressR)
	}
	return &WidgetDef{Kind: widgetPad, Pad: &p}, nil
}

// ---------- 判定 ----------

// padOut は、トラックパッドの出力先（*Mouse。テストでは記録するもの）。
type padOut interface {
	Press(src string, b byte)
	Release(src string)
	Move(dx, dy int)
	Wheel(v, h int)
}

type padState uint8

const (
	padIdle   padState = iota
	padTouch           // 触れている（カーソルを動かす。離したときに短く小さければタップ）
	padScroll          // スクロールの帯で触れている
	padWait            // タップのあと。左ボタンを押したまま、次に触れるのを drag_ms まで待つ
	padTap2            // タップのあとに触れた。左ボタンを押したまま
	padDrag            // ドラッグ中（左ボタンを押したまま動かす）
	padLift            // タップでないタッチで指が離れた。padLiftGrace のあいだに触れ直せば、同じタッチの続きにする
	padTapUp           // タップで指が離れた。padBounce のあいだに触れ直せば、同じタッチの続き（着地の跳ね）。過ぎたら左ボタンを押して padWait
)

var padStateNames = [...]string{"idle", "touch", "scroll", "wait", "tap2", "drag", "lift", "tapup"}

func (s padState) String() string { return padStateNames[s] }

type fpt struct{ X, Y float64 }

// 細かい調整（設定には出さない。テストで変える）。値の根拠は docs/config.md の「既定値の根拠」
var (
	// padLiftGrace：なぞっている途中で、指が一瞬離れたと判定されることがある（帯のスクロールで 28〜204 ms）。
	// タップでないタッチ（なぞる、スクロール、ドラッグ）のあと、この時間のうちに触れ直せば続きとして扱う。
	// ドラッグなら左ボタンを離さず、帯ならスクロールのまま。位置は触れ直したところから測り直す（跳ばない）
	padLiftGrace = 150 * time.Millisecond
	// padCreep：平均の位置が deadzone の内側にあるあいだ、基準の位置をこの速さ（ドット/秒）まで指に寄せる。
	// 指を止めていても、押す強さが変わって位置が 2〜3 ドット/秒で流れるので、それより遅い動きを送らない
	padCreep = 6.0
	// padHoldBack：離す直前のこの数のサンプルは使わない（遅らせて使い、離したときに捨てる）
	padHoldBack = 2
	// padBounce：タップで離れてから、この時間のうちに触れ直したら、着地の跳ね（同じタッチの続き）とみなす。
	// 左ボタンは、この時間が過ぎてから押す。記録では、跳ねの切れ目は 28〜105 ms、ダブルタップの間は 122〜180 ms
	padBounce = 110 * time.Millisecond
	// padResumeSettle：触れ直して続けるときは、settle_ms の代わりにこの時間だけ捨てる（指は動いているので、押し始めの流れより、捨てて失う動きのほうが大きい）
	padResumeSettle = 30 * time.Millisecond
)

// Pad はトラックパッドの判定。タッチの goroutine と、タイマーから呼ばれるのでロックで守る。
type Pad struct {
	mu  sync.Mutex
	out padOut

	p       *PadParams
	cell    image.Rectangle // セルの画面上の範囲（span を含む）
	tc      *TouchConfig
	W, H    int
	active  bool // 指が触れている
	pending bool // 触れたが、まだ押す強さの足りるサンプルがない（なければ、そのタッチはなかったことにする）
	st      padState

	tDown    time.Time // 触れた時刻（着地の跳ねの続きなら、最初に触れた時刻）
	tContact time.Time // いちばん最近に触れた時刻（settle はここから測る）
	waitTill time.Time // padWait の終わり
	liftTill time.Time // padLift の終わり
	liftFrom padState  // padLift の前の状態
	tapUpT   time.Time // padTapUp：タップで離れた時刻
	resumed  bool      // 一瞬離れたあとの続き（タップにしない）
	n        int       // 使ったサンプルの数
	win      []fpt     // 平均を取るサンプル
	queue    []fpt     // 遅らせているサンプル（離す瞬間の跳ねを捨てる）
	queueT   []time.Time
	start    fpt // 最初の平均の位置
	filt     fpt // ヒステリシスを通した位置
	lastT    time.Time
	travel   float64 // start からいちばん離れた距離
	vel      float64 // 速さ（ドット/秒）
	accX     float64 // 送り残したカウント
	accY     float64
	accW     float64 // スクロールの送り残し（ドット）
	longDone bool    // 長押しの右クリックを送った

	// 実機では、待ち時間（drag_ms、長押し、padLiftGrace）が過ぎたらタイマーで Tick を呼ぶ
	realtime bool
	timer    *time.Timer
	stats    PadStats
}

// PadStats は、判定の結果の数（-replay-touch とテスト用）。
type PadStats struct {
	Touches, Taps, Clicks, DoubleClicks, Drags, RightClicks int
	MoveX, MoveY                                            int // 送った移動の合計
	PathX, PathY                                            int // 送った移動の絶対値の合計
	Wheel                                                   int // 送ったホイールの合計
	WheelAbs                                                int
	Dropped                                                 int // 捨てたサンプル（settle、pressure）
	Ignored                                                 int // なかったことにしたタッチ（押す強さの足りるサンプルがない）
	Resumed                                                 int // 一瞬離れたあとに続けたタッチ
}

// NewPad は判定を作る。realtime なら、待ち時間をタイマーで処理する（実機）。
func NewPad(out padOut, realtime bool) *Pad { return &Pad{out: out, realtime: realtime} }

// Stats は、これまでの判定の結果を返す。
func (g *Pad) Stats() PadStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// State は今の状態（テスト用）。
func (g *Pad) State() padState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st
}

// Down は、トラックパッドのセルに触れたときに呼ぶ。cell はセルの画面上の範囲、W×H は画面の大きさ。
// 状態は、押す強さの足りる最初のサンプル（begin）で変える。
func (g *Pad) Down(p *PadParams, cell image.Rectangle, tc *TouchConfig, W, H int, t time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.arm()
	defer g.mu.Unlock()
	switch {
	case g.st == padWait && p != g.p:
		g.finishWait() // 別のトラックパッド
	case g.st == padLift && p != g.p:
		g.finishLift()
	case g.st == padTapUp && p != g.p:
		g.finishTapUp(true)
	case g.st == padTap2 || g.st == padDrag || g.st == padTouch || g.st == padScroll:
		g.out.Release(padSourceL) // 離したのを受け取れなかった
		g.st = padIdle
	}
	g.p, g.cell, g.tc, g.W, g.H = p, cell, tc, W, H
	g.active, g.pending = true, true
	if g.st != padTapUp {
		g.tDown = t // 跳ねの続きなら、最初に触れた時刻のまま（タップの長さは、最初から測る）
	}
	g.tContact = t
	g.queue, g.queueT = g.queue[:0], g.queueT[:0]
	g.stats.Touches++
}

// begin は、押す強さの足りる最初のサンプル（位置 pt）で、タッチを始める。g.mu を持って呼ぶ。
// 帯かどうかは、この位置で決める（settle で捨てるサンプルでも。短いタッチを帯の外と取り違えないように）。
func (g *Pad) begin(pt fpt) {
	g.pending = false
	g.n, g.win = 0, g.win[:0]
	g.lastT = g.tContact
	g.vel, g.accX, g.accY = 0, 0, 0
	g.longDone = false
	switch g.st {
	case padTapUp:
		g.st = padTouch // 着地の跳ね。travel は残し、まだタップになりうる
		g.stats.Resumed++
	case padWait:
		g.st, g.waitTill = padTap2, time.Time{} // 左ボタンは押したまま
		g.travel, g.resumed, g.accW = 0, false, 0
	case padLift:
		g.st, g.liftTill = g.liftFrom, time.Time{} // 続き。travel とスクロールの送り残しは残す
		g.resumed = true
		g.stats.Resumed++
	default:
		g.st = padTouch
		g.travel, g.resumed, g.accW = 0, false, 0
		if g.p.ScrollWidth > 0 && pt.X >= float64(g.cell.Max.X-g.p.ScrollWidth) {
			g.st = padScroll
		}
	}
}

// Sample は、触れているあいだの生のサンプル（座標と押す強さ）を渡す。
func (g *Pad) Sample(x, y, pressure int32, t time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.arm()
	defer g.mu.Unlock()
	if !g.active {
		return
	}
	g.checkLong(t)
	if g.p.MinPressure > 0 && pressure < g.p.MinPressure {
		g.stats.Dropped++
		return
	}
	pt := g.screen(x, y)
	if g.pending {
		g.begin(pt)
	}
	settle := g.p.Settle
	if g.resumed || g.tContact != g.tDown {
		settle = min(settle, padResumeSettle)
	}
	if t.Sub(g.tContact) < settle {
		g.stats.Dropped++
		return
	}
	g.queue = append(g.queue, pt)
	g.queueT = append(g.queueT, t)
	if len(g.queue) > padHoldBack {
		g.use(g.queue[0], g.queueT[0])
		g.queue, g.queueT = g.queue[1:], g.queueT[1:]
	}
}

// use は、遅らせていたサンプルを判定に使う。g.mu を持って呼ぶ。
func (g *Pad) use(pt fpt, t time.Time) {
	g.win = append(g.win, pt)
	if len(g.win) > g.p.Smooth {
		g.win = g.win[len(g.win)-g.p.Smooth:]
	}
	var avg fpt
	for _, q := range g.win {
		avg.X += q.X
		avg.Y += q.Y
	}
	avg.X /= float64(len(g.win))
	avg.Y /= float64(len(g.win))
	g.n++
	if g.n == 1 {
		g.start, g.filt, g.lastT = avg, avg, t
		return
	}
	g.travel = max(g.travel, math.Hypot(avg.X-g.start.X, avg.Y-g.start.Y))
	// ヒステリシス：平均の位置が filt から deadzone より離れたぶんだけ、filt を動かす。
	// 内側なら、filt を padCreep の速さまで寄せる（送らない。止めた指の流れ）
	dx, dy := avg.X-g.filt.X, avg.Y-g.filt.Y
	d := math.Hypot(dx, dy)
	dt := t.Sub(g.lastT).Seconds()
	var mx, my float64
	if d > g.p.Deadzone {
		k := (d - g.p.Deadzone) / d
		mx, my = dx*k, dy*k
		g.filt.X += mx
		g.filt.Y += my
	} else if d > 0 && dt > 0 {
		k := min(padCreep*dt, d) / d
		g.filt.X += dx * k
		g.filt.Y += dy * k
	}
	if dt > 0 {
		v := math.Hypot(mx, my) / dt
		g.vel = 0.5*g.vel + 0.5*v
	}
	g.lastT = t
	switch g.st {
	case padScroll:
		dir := 1.0
		if !g.p.Natural {
			dir = -1
		}
		g.accW += my
		steps := int(g.accW / g.p.ScrollStep)
		if steps != 0 {
			g.accW -= float64(steps) * g.p.ScrollStep
			// 指を下へ（my > 0）：natural なら中身が下へ動く＝上へスクロール（ホイールは正）
			g.out.Wheel(int(dir)*steps, 0)
			g.stats.Wheel += int(dir) * steps
			g.stats.WheelAbs += abs(steps)
		}
	case padTouch, padTap2, padDrag:
		if g.st == padTap2 && g.travel > g.p.TapMove {
			g.st = padDrag
			g.stats.Drags++
		}
		if g.longDone {
			return
		}
		gain := g.p.Speed * (1 + g.p.Accel*min(g.vel/padAccelRef, padAccelCap))
		g.accX += mx * gain
		g.accY += my * gain
		ix, iy := int(g.accX), int(g.accY)
		if ix != 0 || iy != 0 {
			g.accX -= float64(ix)
			g.accY -= float64(iy)
			g.out.Move(ix, iy)
			g.stats.MoveX += ix
			g.stats.MoveY += iy
			g.stats.PathX += abs(ix)
			g.stats.PathY += abs(iy)
		}
	}
}

// Up は、指を離したときに呼ぶ。遅らせていたサンプル（離す瞬間の跳ね）は使わない。
func (g *Pad) Up(t time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.arm()
	defer g.mu.Unlock()
	if !g.active {
		return
	}
	g.checkLong(t)
	g.active = false
	g.queue, g.queueT = g.queue[:0], g.queueT[:0]
	if g.pending {
		// 押す強さの足りるサンプルがなかった（離す瞬間の跳ね、ごく軽い接触）。タップの待ちなどはそのまま続ける
		g.pending = false
		g.stats.Ignored++
		return
	}
	tap := t.Sub(g.tDown) <= g.p.TapTime && g.travel <= g.p.TapMove && !g.longDone && !g.resumed
	switch g.st {
	case padTouch:
		if !tap {
			g.lift(padTouch, t)
			return
		}
		g.st, g.tapUpT = padTapUp, t
		if padBounce <= 0 {
			g.finishTapUp(false)
		}
	case padScroll:
		g.lift(padScroll, t) // 帯ではタップしてもクリックしない（なぞる途中の切れ目がタップに見えるため）
	case padTap2:
		g.st = padIdle
		g.out.Release(padSourceL)
		if tap { // もう一度短くタップした：ダブルクリック
			g.stats.Taps++
			g.out.Press(padSourceL, mouseLeft)
			g.out.Release(padSourceL)
			g.stats.DoubleClicks++
		} else {
			g.stats.Clicks++ // 押したまま止めて離した。1 回のクリック（長いもの）
		}
	case padDrag:
		g.lift(padDrag, t) // ボタンは、触れ直さないまま padLiftGrace が過ぎたら離す
	}
}

// finishTapUp は、タップのあと padBounce が過ぎた（または、ほかに触れた）ので、左ボタンを押す。
// drag_ms があれば、タップで離れてから drag_ms まで押したまま待つ。now なら、待たずにクリックを終える。g.mu を持って呼ぶ。
func (g *Pad) finishTapUp(now bool) {
	g.stats.Taps++
	g.out.Press(padSourceL, mouseLeft)
	if g.p.DragGap > 0 && !now {
		g.st, g.waitTill = padWait, g.tapUpT.Add(max(g.p.DragGap, padBounce))
		return
	}
	g.out.Release(padSourceL)
	g.st = padIdle
	g.stats.Clicks++
}

// lift は、タップでないタッチで指が離れたときに、触れ直すのを待つ。g.mu を持って呼ぶ。
func (g *Pad) lift(from padState, t time.Time) {
	g.st, g.liftFrom = padLift, from
	if padLiftGrace <= 0 {
		g.finishLift()
		return
	}
	g.liftTill = t.Add(padLiftGrace)
}

// finishLift は、触れ直さなかったので、タッチを終える（ドラッグならボタンを離す）。g.mu を持って呼ぶ。
func (g *Pad) finishLift() {
	if g.liftFrom == padDrag {
		g.out.Release(padSourceL)
	}
	g.st, g.liftTill = padIdle, time.Time{}
}

// Tick は、待ち時間（drag_ms、長押し、padLiftGrace）を処理する。t は今の時刻。
func (g *Pad) Tick(t time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.arm()
	defer g.mu.Unlock()
	if g.st == padWait && !t.Before(g.waitTill) {
		g.finishWait()
	}
	if g.st == padLift && !t.Before(g.liftTill) {
		g.finishLift()
	}
	if g.st == padTapUp && !t.Before(g.tapUpT.Add(padBounce)) {
		g.finishTapUp(false)
	}
	if g.st == padWait && !t.Before(g.waitTill) {
		g.finishWait()
	}
	if g.active {
		g.checkLong(t)
	}
}

// finishWait は、タップのあとに次のタッチが来なかったので、左ボタンを離してクリックを終える。g.mu を持って呼ぶ。
func (g *Pad) finishWait() {
	g.out.Release(padSourceL)
	g.st, g.waitTill = padIdle, time.Time{}
	g.stats.Clicks++
}

// checkLong は、長押しの右クリックを判定する。g.mu を持って呼ぶ。
func (g *Pad) checkLong(t time.Time) {
	if !g.p.LongPress || g.longDone || g.pending || g.st != padTouch || g.resumed || g.travel > g.p.TapMove || t.Sub(g.tDown) < padLongPress {
		return
	}
	g.longDone = true
	g.out.Press(padSourceR, mouseRight)
	g.out.Release(padSourceR)
	g.stats.RightClicks++
}

// Deadline は、次に Tick を呼ぶ時刻。ゼロなら待つものはない。
func (g *Pad) Deadline() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deadline()
}

func (g *Pad) deadline() time.Time {
	switch {
	case g.st == padWait:
		return g.waitTill
	case g.st == padLift:
		return g.liftTill
	case g.st == padTapUp:
		return g.tapUpT.Add(padBounce)
	case g.active && !g.pending && g.p.LongPress && g.st == padTouch && !g.resumed && !g.longDone:
		return g.tDown.Add(padLongPress)
	}
	return time.Time{}
}

// Interrupt は、トラックパッドではない場所に触れたときに呼ぶ。タップのあとの待ちを終え、クリックを終える。
func (g *Pad) Interrupt() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.arm()
	defer g.mu.Unlock()
	switch g.st {
	case padWait:
		g.finishWait()
	case padLift:
		g.finishLift()
	case padTapUp:
		g.finishTapUp(true)
	}
}

// Cancel は、レイヤーの切り替えや設定の再読み込みで、今のタッチとタップの待ちをやめ、ボタンを離す。
// 指を離すまで、そのタッチではもう何もしない。
func (g *Pad) Cancel() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.arm()
	defer g.mu.Unlock()
	if g.st != padIdle {
		g.out.Release(padSourceL)
		g.out.Release(padSourceR)
	}
	g.st, g.active, g.pending, g.waitTill, g.liftTill, g.tapUpT = padIdle, false, false, time.Time{}, time.Time{}, time.Time{}
	g.queue, g.queueT = g.queue[:0], g.queueT[:0]
}

// arm は、実機のとき、次の待ち時間にタイマーを合わせる。g.mu を離してから呼ぶ（defer の順）。
func (g *Pad) arm() {
	if !g.realtime {
		return
	}
	g.mu.Lock()
	d := g.deadline()
	if g.timer != nil {
		g.timer.Stop()
	}
	if !d.IsZero() {
		g.timer = time.AfterFunc(max(time.Until(d), 0), func() { g.Tick(time.Now()) })
	}
	g.mu.Unlock()
}

// screen は、タッチの生座標を画面のドット（小数）にする。touchPoint と同じ割り方。
func (g *Pad) screen(x, y int32) fpt {
	tc := g.tc
	if tc.SwapXY {
		x, y = y, x
	}
	f := func(v, lo, hi int32, n int) float64 {
		if hi == lo {
			return 0
		}
		return float64(v-lo) * float64(n) / float64(hi-lo)
	}
	return fpt{f(x, tc.MinX, tc.MaxX, g.W), f(y, tc.MinY, tc.MaxY, g.H)}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// mouseOut は、Mouse を padOut として使う。
var _ padOut = (*Mouse)(nil)

// ---------- 描画 ----------

var (
	colPadBand = RGB{0x26, 0x34, 0x48} // スクロールの帯
	colPadRail = RGB{0x50, 0x60, 0x74} // 帯の中の線
)

// padBand は、セル cell（span を含む範囲）の箱 box の中に描く、スクロールの帯の範囲。帯がなければ空。
// タッチの判定（Pad.use）と同じく、セルの右端から scroll_width ドット。
func padBand(p *PadParams, cell, box image.Rectangle) image.Rectangle {
	if p.ScrollWidth <= 0 {
		return image.Rectangle{}
	}
	in := box.Inset(borderW)
	return image.Rect(max(cell.Max.X-p.ScrollWidth, in.Min.X), in.Min.Y, in.Max.X, in.Max.Y)
}

// drawPad は、トラックパッドのセルの中身（見出しとスクロールの帯）を描く。inner はラベルの範囲。
// gui/src/preview.ts の drawPad と同じ。
func drawPad(cv *Canvas, cell, box, inner image.Rectangle, v *CellView, env WidgetEnv, subInk RGB) {
	band := padBand(v.Widget.Pad, cell, box)
	area := inner
	if !band.Empty() {
		area.Max.X = min(area.Max.X, band.Min.X-textMargin)
		halo := cv.halo
		cv.halo = nil
		cv.fill(band, colPadBand)
		cx := band.Min.X + band.Dx()/2
		s := min(fitScaleMax([]string{"▲"}, band.Dx()-4, band.Dy()/4, 2), 2)
		top, bottom := band.Min.Y+textMargin, band.Max.Y-textMargin-fontH*s
		railTop, railBottom := top+fontH*s+4, bottom-4
		if railBottom > railTop {
			cv.fill(image.Rect(cx-1, railTop, cx+1, railBottom), colPadRail)
		}
		drawCentered(cv, band, top, "▲", s, colSub)
		drawCentered(cv, band, bottom, "▼", s, colSub)
		cv.halo = halo
	}
	if area.Dx() <= 0 {
		return
	}
	if v.Label != "" {
		caption, s, rest := widgetCaption(area, v.Label)
		drawCentered(cv, area, area.Min.Y, caption, s, subInk)
		area = rest
	}
	if env.MouseOff {
		// USB がキーボードだけの形（ブートキーボード）。usb_mode のキーかセルで切り替える
		s1 := min(fitScaleMax([]string{padOffText}, area.Dx(), area.Dy()/2, 3), 3)
		s2 := min(fitScaleMax([]string{padOffHint}, area.Dx(), area.Dy()/4, 2), 2)
		h := fontH*s1 + widgetLineGap + fontH*s2
		y := area.Min.Y + (area.Dy()-h)/2
		drawCentered(cv, area, y, padOffText, s1, colPadOff)
		drawCentered(cv, area, y+fontH*s1+widgetLineGap, padOffHint, s2, subInk)
	}
}

const (
	padOffText = "マウスはオフ"
	padOffHint = "USB はキーボードだけの形です"
)

var colPadOff = RGB{0xff, 0x80, 0x20}
