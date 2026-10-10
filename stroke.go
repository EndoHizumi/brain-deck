package main

import (
	"fmt"
	"image"
	"math"
	"time"
)

// ---------- スタイラスの線（ペンタブレットと手書きメモで共通） ----------
//
// 抵抗膜のタッチパネル（mxs-lradc-ts）は、サンプルが 26 ms ごとで、触れた直後と離す直前に座標が跳ねる
// （トラックパッドと同じ。trackpad.go）。指で記録したタッチ（testdata/touch、2026-10-08）から分かること：
//   - 押す強さ（ABS_PRESSURE、0〜4095）：最初のサンプルは 300〜1500（中央 1000 前後）、なぞっているあいだは 300〜2600
//     （中央 1450〜1900）、軽く触れると 450〜1270（中央 800）。同じ線の中でも 2〜3 倍変わる
//   - 1 つ目と 2 つ目のサンプルのあいだの動き：中央 2〜9 ドット、多いと 47 ドット（触れた瞬間の跳ね）
//   - 最後のサンプルの動き：中央 1〜17 ドット、多いと 29 ドット（離す瞬間の跳ね）
//   - なぞる途中で、54〜229 ms だけ離れたことになる（帯のスクロールの記録）
// そこで、トラックパッドと同じ考え方で、
//   - 押す強さが min_pressure より弱いサンプルを捨てる
//   - 触れてから settle のあいだのサンプルを捨てる（既定は 1 つ目だけ。線の書き始めを失わないよう、トラックパッドより短い）
//   - 離す直前の hold_back 個（既定 2）のサンプルを使わない（1 では、slow.touch の終わりに跳ねが残った）
//   - 最後の smooth 個（既定 3）の平均を使う
//   - 短いタップで、遅らせたサンプルしかないときは、その最初のものを 1 点の線（点）にする
//   - join のうちに、離れた位置から join_dist ドット以内に触れ直したら、同じ線の続きにする
// ペンで記録したものはまだない（無人のセッションで作ったので、指の記録から決めた仮の値）。ペンの記録を取ったら、
// -replay-pen で値を見直す（README の「ペンの記録を取る」）。

// PenParams は、線の読み取りの設定（pentablet と memo の項目）。
type PenParams struct {
	MinPressure int32         // これより弱いサンプルを捨てる
	Settle      time.Duration // 触れた直後に捨てる時間
	Smooth      int           // 平均を取るサンプルの数
	HoldBack    int           // 離す直前に捨てるサンプルの数
	Join        time.Duration // これより短く離れて触れ直したら、同じ線の続き
	JoinDist    float64       // 続きとみなす、離れた位置からの距離（ドット）
	PressureLo  int32         // 筆圧の 0 にする押す強さ（pentablet）
	PressureHi  int32         // 筆圧の最大にする押す強さ
}

// 仮の既定値（指の記録から。ペンの記録で見直す）
var defaultPen = PenParams{
	MinPressure: 400,
	Settle:      20 * time.Millisecond,
	Smooth:      3,
	HoldBack:    2,
	Join:        120 * time.Millisecond,
	JoinDist:    24,
	PressureLo:  400,
	PressureHi:  2400,
}

// compilePenParams は、セルの書き方の pen_* の項目を読む。
func compilePenParams(s ActionSpec) (PenParams, error) {
	p := defaultPen
	if s.MinPressure != nil {
		if *s.MinPressure < 0 || *s.MinPressure > 4095 {
			return p, fmt.Errorf("min_pressure must be 0..4095")
		}
		p.MinPressure = int32(*s.MinPressure)
	}
	if s.SettleMS != nil {
		if *s.SettleMS < 0 || *s.SettleMS > 500 {
			return p, fmt.Errorf("settle_ms must be 0..500 (milliseconds)")
		}
		p.Settle = time.Duration(*s.SettleMS) * time.Millisecond
	}
	if s.Smooth != nil {
		if *s.Smooth < 1 || *s.Smooth > padMaxSmooth {
			return p, fmt.Errorf("smooth must be 1..%d", padMaxSmooth)
		}
		p.Smooth = *s.Smooth
	}
	if s.PressureRange != nil {
		r := s.PressureRange
		if r[0] < 0 || r[1] > 4095 || r[0] >= r[1] {
			return p, fmt.Errorf("pressure_range must be [low, high] with 0 <= low < high <= 4095")
		}
		p.PressureLo, p.PressureHi = int32(r[0]), int32(r[1])
	}
	return p, nil
}

// strokeSink は、読み取った線の行き先（ペンタブレット、手書きメモ）。pt は画面のドット（小数）、pressure は生の押す強さ（平均）。
type strokeSink interface {
	StrokeDown(pt fpt, pressure int32)
	StrokeMove(pt fpt, pressure int32)
	StrokeUp()
}

// StrokeFilter は、タッチのサンプルから線を読み取る。タッチの goroutine だけが使う（タイマーは使わない。
// join の待ちは、次に触れたときか、Flush で終える）。
type StrokeFilter struct {
	p    PenParams
	tm   touchMap
	sink strokeSink

	active  bool // 指（ペン）が触れている
	drawing bool // 線を出している（StrokeDown を送った）
	tDown   time.Time
	win     []fpt
	winP    []int32
	queue   []fpt
	queueP  []int32
	last    fpt       // 最後に送った位置
	upAt    time.Time // 離れた時刻（join の待ち）
	pending bool      // 離れたが、まだ StrokeUp を送っていない（join を待っている）
	used    bool      // このタッチで、サンプルを使った

	stats StrokeStats
}

// StrokeStats は、読み取りの結果の数（-replay-pen とテスト）。
type StrokeStats struct {
	Touches, Strokes, Points, Joined, Dropped, Ignored int
	MaxStep                                            float64 // 送った点と点のあいだの最大の距離
	Length                                             float64 // 送った線の長さの合計
}

// NewStrokeFilter は、sink に線を送る読み取りを作る。
func NewStrokeFilter(p PenParams, tm touchMap, sink strokeSink) *StrokeFilter {
	return &StrokeFilter{p: p, tm: tm, sink: sink}
}

// Down は、触れたとき。
func (f *StrokeFilter) Down(t time.Time) {
	f.stats.Touches++
	if f.pending && t.Sub(f.upAt) > f.p.Join {
		f.finish()
	}
	f.active = true
	f.used = false
	f.tDown = t
	f.win, f.winP = f.win[:0], f.winP[:0]
	f.queue, f.queueP = f.queue[:0], f.queueP[:0]
}

// Sample は、触れているあいだの生のサンプル。
func (f *StrokeFilter) Sample(x, y, pressure int32, t time.Time) {
	if !f.active {
		return
	}
	if pressure < f.p.MinPressure || t.Sub(f.tDown) < f.p.Settle {
		f.stats.Dropped++
		return
	}
	pt := f.tm.fpoint(x, y)
	f.queue = append(f.queue, pt)
	f.queueP = append(f.queueP, pressure)
	if len(f.queue) > f.p.HoldBack {
		f.use(f.queue[0], f.queueP[0])
		f.queue, f.queueP = f.queue[1:], f.queueP[1:]
	}
}

func (f *StrokeFilter) use(pt fpt, pr int32) {
	f.used = true
	f.win = append(f.win, pt)
	f.winP = append(f.winP, pr)
	if len(f.win) > f.p.Smooth {
		f.win, f.winP = f.win[1:], f.winP[1:]
	}
	var avg fpt
	var ap int64
	for i, q := range f.win {
		avg.X += q.X
		avg.Y += q.Y
		ap += int64(f.winP[i])
	}
	n := float64(len(f.win))
	avg.X /= n
	avg.Y /= n
	pr = int32(ap / int64(len(f.win)))
	switch {
	case f.pending && math.Hypot(avg.X-f.last.X, avg.Y-f.last.Y) <= f.p.JoinDist:
		// 一瞬離れたあとの続き
		f.pending, f.drawing = false, true
		f.stats.Joined++
		f.move(avg, pr)
	case f.pending:
		f.finish()
		fallthrough
	case !f.drawing:
		f.drawing = true
		f.last = avg
		f.stats.Strokes++
		f.stats.Points++
		f.sink.StrokeDown(avg, pr)
	default:
		f.move(avg, pr)
	}
}

func (f *StrokeFilter) move(pt fpt, pr int32) {
	d := math.Hypot(pt.X-f.last.X, pt.Y-f.last.Y)
	f.stats.MaxStep = max(f.stats.MaxStep, d)
	f.stats.Length += d
	f.stats.Points++
	f.last = pt
	f.sink.StrokeMove(pt, pr)
}

// Up は、離したとき。遅らせていたサンプルは使わない。線は join のあいだ終えない（Flush か、次の Down で終える）。
func (f *StrokeFilter) Up(t time.Time) {
	if !f.active {
		return
	}
	f.active = false
	if !f.used && len(f.queue) > 0 {
		// 短いタップ（点を打つ）：遅らせていたサンプルしかなければ、最初のものを 1 点の線にする
		f.use(f.queue[0], f.queueP[0])
	}
	f.queue, f.queueP = f.queue[:0], f.queueP[:0]
	if !f.drawing && !f.pending {
		f.stats.Ignored++
		return
	}
	if !f.used && f.pending && t.Sub(f.upAt) > f.p.Join {
		f.finish() // 触れ直したが、使えるサンプルがないまま離した
		return
	}
	if f.drawing {
		f.drawing, f.pending, f.upAt = false, true, t
	}
	if f.p.Join <= 0 {
		f.finish()
	}
}

// Due は、join の待ちが終わる時刻（待っていなければゼロ）。触れ直しているあいだは、続きかどうかを
// 最初に使うサンプルで決めるので、待ちを終えない。
func (f *StrokeFilter) Due() time.Time {
	if !f.pending || f.active {
		return time.Time{}
	}
	return f.upAt.Add(f.p.Join)
}

// Flush は、join の待ちを終えて、線を終える（時間が過ぎた、別のところを押した）。
func (f *StrokeFilter) Flush() {
	if f.pending {
		f.finish()
	}
}

func (f *StrokeFilter) finish() {
	f.pending = false
	f.sink.StrokeUp()
}

// Stats は、これまでの結果。
func (f *StrokeFilter) Stats() StrokeStats { return f.stats }

// inCell は、pt を cell の中に収める。
func (pt fpt) inCell(r image.Rectangle) fpt {
	return fpt{min(max(pt.X, float64(r.Min.X)), float64(r.Max.X-1)), min(max(pt.Y, float64(r.Min.Y)), float64(r.Max.Y-1))}
}
