package main

import (
	"errors"
	"image"
	"log"
	"os"
	"strconv"
	"sync"
)

// ---------- 画面の回転 ----------
//
// キー・セル・ソフトキーの `rotate: next`（90 度ずつ）と `rotate: 90` などで、動いているあいだに画面の向きを変える。
// 向きは display.rotate と同じ意味（時計回りの角度。90 では画面の上が本体の右側、ソフトキーの帯の側になる）。
// 変えた向きは /var/lib/lefthand/display.json に残し、デーモンや Brain を再起動しても続ける。
//
// タッチは、キャリブレーションの値で生の座標を物理の画面のドットにしてから、向きに合わせて回す（touchMap）。
// 画面右のソフトキーの帯はベゼルに印刷されていて動かないので、生の座標のまま判定し、向きの影響を受けない。
//
// 縦向き（480x800）では、レイヤーの touch_portrait の格子を使う。書いていないレイヤーは、
// display.portrait_grid のとおり横向きの格子から作る（transpose：列と行を入れ替える。stretch：そのまま縦に伸ばす）。

const (
	rotateNext = -1 // rotate: next

	portraitTranspose = "transpose"
	portraitStretch   = "stretch"

	displayStoreName = "display"
)

// rotateLabels は、rotate のセルに出す名前（gui/src/model.ts の ROTATE_LABELS と同じ）。
var rotateLabels = map[string]string{"next": "回す", "0": "横向き", "90": "縦向き", "180": "横向き(逆)", "270": "縦向き(逆)"}

// parseRotate は rotate の値を読む。
func parseRotate(s string) (int, bool) {
	if s == "next" {
		return rotateNext, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || !validRotate(n) {
		return 0, false
	}
	return n, true
}

func validRotate(n int) bool { return n == 0 || n == 90 || n == 180 || n == 270 }

// isPortrait は、向き rot で、物理の大きさ pw×ph の画面が縦長になるか。
func isPortrait(rot, pw, ph int) bool {
	w, h := rotatedSize(rot, pw, ph)
	return w < h
}

// rotatedSize は、向き rot での画面の論理の大きさ。
func rotatedSize(rot, pw, ph int) (int, int) {
	if rot == 90 || rot == 270 {
		return ph, pw
	}
	return pw, ph
}

// portraitOf は、touch_portrait を書いていないレイヤーの縦向きの格子を、横向きの格子 g から作る。
func portraitOf(g *Grid, mode string) *Grid {
	if mode == portraitStretch {
		return g
	}
	t := &Grid{Cols: g.Rows, Rows: g.Cols, Cells: make(map[cellPos]*Action, len(g.Cells)), Background: g.Background}
	for p, a := range g.Cells {
		b := *a
		b.SpanW, b.SpanH = a.SpanH, a.SpanW
		t.Cells[cellPos{p.Row, p.Col}] = &b
	}
	return t
}

// touchMap は、タッチの生の座標を、今の向きの画面のドットにする。
type touchMap struct {
	tc     *TouchConfig
	rot    int
	pw, ph int // 物理の画面の大きさ
}

// size は論理の画面の大きさ。
func (m touchMap) size() (int, int) { return rotatedSize(m.rot, m.pw, m.ph) }

// unrotate は、物理の画面の点を論理の点にする（fb.go の Canvas.phys の逆）。
func unrotate(rot, pw, ph, X, Y int) (int, int) {
	switch rot {
	case 90:
		return Y, pw - 1 - X
	case 180:
		return pw - 1 - X, ph - 1 - Y
	case 270:
		return ph - 1 - Y, X
	}
	return X, Y
}

// point は、生の座標を論理の画面のドットにする。向きが 0 なら touchPoint と同じ。
func (m touchMap) point(x, y int32) image.Point {
	p := touchPoint(m.tc, x, y, m.pw, m.ph)
	X, Y := unrotate(m.rot, m.pw, m.ph, p.X, p.Y)
	return image.Pt(X, Y)
}

// fpoint は、生の座標を論理の画面のドット（小数）にする（トラックパッド）。
func (m touchMap) fpoint(x, y int32) fpt {
	tc := m.tc
	rx, ry := x, y // キャリブレーションは swap_xy の前の生の座標で決めてある
	if tc.SwapXY {
		x, y = y, x
	}
	f := func(v, lo, hi int32, n int) float64 {
		if hi == lo {
			return 0
		}
		return float64(v-lo) * float64(n) / float64(hi-lo)
	}
	X, Y, ok := calPhys(rx, ry, m.pw, m.ph) // キャリブレーション（touchcal.go）
	if !ok {
		X, Y = f(x, tc.MinX, tc.MaxX, m.pw), f(y, tc.MinY, tc.MaxY, m.ph)
	}
	switch m.rot {
	case 90:
		return fpt{Y, float64(m.pw) - X}
	case 180:
		return fpt{float64(m.pw) - X, float64(m.ph) - Y}
	case 270:
		return fpt{float64(m.ph) - Y, X}
	}
	return fpt{X, Y}
}

// cell は、生の座標が cols×rows の格子のどのセルかを返す。格子は画面の上 top ドットを除いた範囲に並ぶ。
// bar は、上の top ドット（状態の帯）に触れたとき。向きが 0 で帯がなければ、今までどおり生の座標で割る（touchCell）。
func (m touchMap) cell(cols, rows, top int, x, y int32) (col, row int, bar bool) {
	if m.rot == 0 && top == 0 {
		col, row = touchCell(m.tc, cols, rows, x, y)
		return col, row, false
	}
	W, H := m.size()
	p := m.point(x, y)
	if p.Y < top {
		return -1, -1, true
	}
	clamp := func(v, n int) int { return min(max(v, 0), n-1) }
	return clamp(p.X*cols/W, cols), clamp((p.Y-top)*rows/max(H-top, 1), rows), false
}

// ---------- 向きの保存 ----------

// rotateRecord は display.json。ConfigRotate は、保存したときの display.rotate。
// 設定の display.rotate を書き換えたら（ConfigRotate と違えば）、保存した向きより設定を使う。
type rotateRecord struct {
	Rotate       int `json:"rotate"`
	ConfigRotate int `json:"config_rotate"`
}

// RotateStore は、画面の向きの保存。書き込みは待たない（SD カードへの書き込みは遅いことがある）。
type RotateStore struct {
	mu    sync.Mutex
	wg    sync.WaitGroup
	store *Store
}

// Wait は、書きかけの保存を待つ（テスト用）。
func (rs *RotateStore) Wait() { rs.wg.Wait() }

func NewRotateStore(store *Store) *RotateStore { return &RotateStore{store: store} }

// Initial は、起動したときの向き。保存がなければ、設定の display.rotate。
func (rs *RotateStore) Initial(configRotate int) int {
	if rs == nil {
		return configRotate
	}
	var r rotateRecord
	if err := rs.store.Load(displayStoreName, &r); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("rotate: %v", err)
		}
		return configRotate
	}
	if r.ConfigRotate != configRotate || !validRotate(r.Rotate) {
		return configRotate
	}
	return r.Rotate
}

// Save は、向きを保存する。待たずに返る。
func (rs *RotateStore) Save(rot, configRotate int) {
	if rs == nil {
		return
	}
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		rs.mu.Lock()
		defer rs.mu.Unlock()
		if err := rs.store.Save(displayStoreName, rotateRecord{Rotate: rot, ConfigRotate: configRotate}); err != nil {
			log.Printf("rotate: save: %v", err)
		}
	}()
}
