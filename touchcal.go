package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// ---------- タッチのキャリブレーション（保存したもの） ----------
//
// 画面に十字を出して押してもらい（calibui.go）、パネルの生の座標から画面のドットへのアフィン変換
// （tslib と同じ。ずれ、傾き、伸び縮みをまとめて直す）を最小二乗法で決める：
//   X = a0·x + a1·y + a2、Y = a3·x + a4·y + a5（x、y はパネルの生の座標。swap_xy の前。X、Y は向き 0 の画面のドット）
// 結果は <data-dir>/touch-cal.json に置き、設定の touch.min_x などより優先する（touchPoint、touchCell、fpoint）。
// ソフトキー（touch.soft_areas）は、画面の外の帯なので、今までどおり生の座標で決める（変換しない）。
// 消す（brain-deck calibrate --reset）と、設定の min と max に戻る（前のファイルは .prev に残す）。

const (
	touchCalName    = "touch-cal.json"
	touchCalVersion = 1
)

// TouchCalib は、使っているアフィン変換。W×H は測ったときの画面（向き 0）の大きさ。
type TouchCalib struct {
	A    [6]float64
	W, H int
}

// apply は、生の座標を、測ったときの画面のドット（小数）にする。
func (c *TouchCalib) apply(x, y int32) (float64, float64) {
	fx, fy := float64(x), float64(y)
	return c.A[0]*fx + c.A[1]*fy + c.A[2], c.A[3]*fx + c.A[4]*fy + c.A[5]
}

// touchCalNow は、今使っているキャリブレーション（nil なら設定の min と max）。デバイスに 1 つなので、パッケージで持つ。
var touchCalNow atomic.Pointer[TouchCalib]

// calPhys は、キャリブレーションがあれば、生の座標を W×H の画面（向き 0）のドット（小数）にする。
func calPhys(x, y int32, W, H int) (float64, float64, bool) {
	c := touchCalNow.Load()
	if c == nil || c.W <= 0 || c.H <= 0 {
		return 0, 0, false
	}
	X, Y := c.apply(x, y)
	return X * float64(W) / float64(c.W), Y * float64(H) / float64(c.H), true
}

// calCell は、ドット（小数）v を、n 個に割った範囲（0..size）のどれかにする（セルの境界は cellSpan と同じ割り方）。
func calCell(v float64, size, n int) int {
	if size <= 0 || n <= 0 {
		return 0
	}
	p := int(math.Floor(v))
	p = min(max(p, 0), size-1)
	return p * n / size
}

// ---------- 計算 ----------

// calPair は、十字（画面のドット）と、押した生の座標の組。
type calPair struct {
	SX, SY float64 // 十字
	RX, RY float64 // 押した生の座標（数回読んだまん中の値）
}

// fitAffine は、組から最小二乗法でアフィン変換を決める（3 点以上。点が一直線に並ぶと決まらない）。
func fitAffine(ps []calPair) ([6]float64, error) {
	var a [6]float64
	if len(ps) < 3 {
		return a, errors.New("at least 3 points are needed")
	}
	// 生の座標を平均と広がりで正規化してから解く（4000 前後の値の 2 乗で、精度を落とさないため）
	var mx, my float64
	for _, p := range ps {
		mx += p.RX
		my += p.RY
	}
	n := float64(len(ps))
	mx, my = mx/n, my/n
	var s float64
	for _, p := range ps {
		s += math.Abs(p.RX-mx) + math.Abs(p.RY-my)
	}
	s /= 2 * n
	if s == 0 {
		return a, errors.New("all points are at the same place")
	}
	// 正規方程式 [Σuu Σuv Σu; Σuv Σvv Σv; Σu Σv n] · [p q r] = [ΣuX ΣvX ΣX]
	var m [3][3]float64
	var bx, by [3]float64
	for _, p := range ps {
		u, v := (p.RX-mx)/s, (p.RY-my)/s
		row := [3]float64{u, v, 1}
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				m[i][j] += row[i] * row[j]
			}
			bx[i] += row[i] * p.SX
			by[i] += row[i] * p.SY
		}
	}
	px, ok1 := solve3(m, bx)
	py, ok2 := solve3(m, by)
	if !ok1 || !ok2 {
		return a, errors.New("the points are in a line; the transform cannot be decided")
	}
	// X = p·(x−mx)/s + q·(y−my)/s + r を、生の座標の式に戻す
	conv := func(c [3]float64) (float64, float64, float64) {
		return c[0] / s, c[1] / s, c[2] - c[0]*mx/s - c[1]*my/s
	}
	a[0], a[1], a[2] = conv(px)
	a[3], a[4], a[5] = conv(py)
	return a, nil
}

// solve3 は、3 元の連立方程式を、部分ピボットのガウスの消去法で解く。
func solve3(m [3][3]float64, b [3]float64) ([3]float64, bool) {
	var x [3]float64
	for c := 0; c < 3; c++ {
		p := c
		for r := c + 1; r < 3; r++ {
			if math.Abs(m[r][c]) > math.Abs(m[p][c]) {
				p = r
			}
		}
		if math.Abs(m[p][c]) < 1e-9 {
			return x, false
		}
		m[c], m[p] = m[p], m[c]
		b[c], b[p] = b[p], b[c]
		for r := c + 1; r < 3; r++ {
			f := m[r][c] / m[c][c]
			for k := c; k < 3; k++ {
				m[r][k] -= f * m[c][k]
			}
			b[r] -= f * b[c]
		}
	}
	for r := 2; r >= 0; r-- {
		v := b[r]
		for k := r + 1; k < 3; k++ {
			v -= m[r][k] * x[k]
		}
		x[r] = v / m[r][r]
	}
	return x, true
}

// CalError は、十字と、押した位置（ある変換で画面のドットにしたもの）のずれ（ドット）。
type CalError struct {
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
}

func (e CalError) String() string {
	return fmt.Sprintf("平均 %.1f、最大 %.1f ドット", e.Mean, e.Max)
}

// calErrors は、f で変換したときのずれ。
func calErrors(ps []calPair, f func(x, y float64) (float64, float64)) (CalError, []float64) {
	var e CalError
	ds := make([]float64, len(ps))
	for i, p := range ps {
		X, Y := f(p.RX, p.RY)
		d := math.Hypot(X-p.SX, Y-p.SY)
		ds[i] = d
		e.Mean += d
		e.Max = max(e.Max, d)
	}
	if len(ps) > 0 {
		e.Mean /= float64(len(ps))
	}
	return e, ds
}

func affineFunc(a [6]float64) func(x, y float64) (float64, float64) {
	return func(x, y float64) (float64, float64) { return a[0]*x + a[1]*y + a[2], a[3]*x + a[4]*y + a[5] }
}

// calOutlier は、1 点だけ大きく外れている点を探す。1 点ずつ除いて変換を決め直し、残りがいちばんよく合う点を選ぶ。
// 残りがよく合い（ずれが limit の半分まで）、除いた点がその変換で limit より外れていれば、その点を返す。
// 5 点以上のときだけ調べる。外れている点がなければ -1。2 つ目の値は、選んだ点のずれ（ドット）。
func calOutlier(ps []calPair, limit float64) (int, float64) {
	if len(ps) < 5 {
		return -1, 0
	}
	best, bestRest, bestD := -1, math.Inf(1), 0.0
	for i := range ps {
		rest := append(append([]calPair(nil), ps[:i]...), ps[i+1:]...)
		a, err := fitAffine(rest)
		if err != nil {
			continue
		}
		f := affineFunc(a)
		re, _ := calErrors(rest, f)
		X, Y := f(ps[i].RX, ps[i].RY)
		if re.Max < bestRest {
			best, bestRest, bestD = i, re.Max, math.Hypot(X-ps[i].SX, Y-ps[i].SY)
		}
	}
	if best < 0 || bestD <= limit || bestRest > limit/2 {
		return -1, bestD
	}
	return best, bestD
}

// ---------- ファイル ----------

// calFile は touch-cal.json。
type calFile struct {
	Version int        `json:"version"`
	Affine  [6]float64 `json:"affine"` // X = a0·x + a1·y + a2、Y = a3·x + a4·y + a5
	W       int        `json:"w"`      // 測ったときの画面（向き 0）の大きさ
	H       int        `json:"h"`
	Points  []calPoint `json:"points"`
	Before  CalError   `json:"before"` // 測る前の変換（設定の min と max か、前のキャリブレーション）でのずれ
	After   CalError   `json:"after"`  // この変換でのずれ（同じ点で）
	// 測ったときの設定の min と max（設定の値に戻すとき、何を使っていたかを残す）
	Config  [4]int32  `json:"config_min_max"` // min_x、max_x、min_y、max_y
	SwapXY  bool      `json:"swap_xy"`
	Created time.Time `json:"created"`
	Source  string    `json:"source,omitempty"` // brain（本体のボタン）か brain-deck
}

type calPoint struct {
	Screen [2]float64 `json:"screen"`
	Raw    [2]float64 `json:"raw"`
}

func touchCalPath(dataDir string) string { return filepath.Join(dataDir, touchCalName) }

// loadTouchCal は、保存したキャリブレーションを読んで使う。なければ（読めなければ）設定の min と max。
func loadTouchCal(dataDir string) (*calFile, error) {
	b, err := os.ReadFile(touchCalPath(dataDir))
	if err != nil {
		touchCalNow.Store(nil)
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f calFile
	if err := json.Unmarshal(b, &f); err != nil {
		touchCalNow.Store(nil)
		return nil, fmt.Errorf("%s: %w", touchCalName, err)
	}
	if f.Version != touchCalVersion || f.W <= 0 || f.H <= 0 {
		touchCalNow.Store(nil)
		return nil, fmt.Errorf("%s: unknown version %d or size %dx%d", touchCalName, f.Version, f.W, f.H)
	}
	touchCalNow.Store(&TouchCalib{A: f.Affine, W: f.W, H: f.H})
	return &f, nil
}

// saveTouchCal は、キャリブレーションを書いて使う。前のファイルは .prev に残す。
func saveTouchCal(dataDir string, f *calFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	p := touchCalPath(dataDir)
	if _, err := os.Stat(p); err == nil {
		os.Rename(p, p+".prev")
	}
	if err := writeFileAtomic(p, append(b, '\n'), false); err != nil {
		return err
	}
	touchCalNow.Store(&TouchCalib{A: f.Affine, W: f.W, H: f.H})
	log.Printf("touch: calibration saved (%d points; before %s, after %s)", len(f.Points), f.Before, f.After)
	return nil
}

// resetTouchCal は、キャリブレーションをやめて、設定の min と max に戻す（ファイルは .prev に移す）。
func resetTouchCal(dataDir string) (bool, error) {
	p := touchCalPath(dataDir)
	touchCalNow.Store(nil)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return false, nil
	}
	if err := os.Rename(p, p+".prev"); err != nil {
		return false, err
	}
	log.Printf("touch: calibration reset (back to touch.min_x etc. in the config)")
	return true, nil
}
