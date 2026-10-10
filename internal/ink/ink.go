// Package ink は、手書きの線（点の列）の描き方。Brain の画面（lefthand の手書きメモ）と、
// PC で PNG に描き出すとき（brain-deck memo get）で、同じ画素になるように、整数だけで描く。
//
// 線は、太さ w の丸いペン先を、点と点のあいだに動かした跡として描く。丸いペン先は、行ごとの横の帯（span）の
// 集まりで表し、塗るのは呼ぶ側の fill（y の行の [x0, x1) を塗る）に任せる。
package ink

import (
	"image"
	"sync"
)

// Pt は線の 1 点（ページの座標。ドット）。
type Pt struct{ X, Y int }

// Stroke は 1 本の線。Erase なら消しゴム（紙の色で塗る）。
type Stroke struct {
	W     int  `json:"w"`
	Erase bool `json:"e,omitempty"`
	// P は点の列を x0, y0, x1, y1, … と並べたもの（JSON を小さくする）
	P []int `json:"p"`
}

// Points は、P を点の列にする。
func (s *Stroke) Points() []Pt {
	out := make([]Pt, 0, len(s.P)/2)
	for i := 0; i+1 < len(s.P); i += 2 {
		out = append(out, Pt{s.P[i], s.P[i+1]})
	}
	return out
}

// Add は点を足す。
func (s *Stroke) Add(p Pt) { s.P = append(s.P, p.X, p.Y) }

// Bounds は線が塗る範囲。
func (s *Stroke) Bounds() image.Rectangle {
	var r image.Rectangle
	for i, p := range s.Points() {
		b := tipRect(p, s.W)
		if i == 0 {
			r = b
		} else {
			r = r.Union(b)
		}
	}
	return r
}

func tipRect(p Pt, w int) image.Rectangle {
	r0 := w / 2
	return image.Rect(p.X-r0, p.Y-r0, p.X-r0+max(w, 1), p.Y-r0+max(w, 1))
}

// tip は、太さ w の丸いペン先の、行ごとの帯（中心からの dy ごとの [dx0, dx1)）。
type tip struct {
	w    int
	rows [][2]int // rows[i] は dy = i - w/2 の行
}

var (
	tipsMu sync.Mutex
	tips   = map[int]*tip{}
)

func tipOf(w int) *tip {
	w = max(w, 1)
	tipsMu.Lock()
	defer tipsMu.Unlock()
	if t, ok := tips[w]; ok {
		return t
	}
	t := &tip{w: w}
	// 中心を (w-1)/2 の位置とした円。半径の 2 乗を 4 倍の整数で比べる（w が偶数でも左右対称にする）
	c := w - 1 // 中心の 2 倍
	rr := w * w
	for y := 0; y < w; y++ {
		dy := 2*y - c
		x0, x1 := w, 0
		for x := 0; x < w; x++ {
			dx := 2*x - c
			if dx*dx+dy*dy <= rr {
				x0, x1 = min(x0, x), max(x1, x+1)
			}
		}
		t.rows = append(t.rows, [2]int{x0, x1})
	}
	tips[w] = t
	return t
}

// stamp は、p を中心にペン先を 1 回置く。
func stamp(fill func(x0, x1, y int), clip image.Rectangle, p Pt, t *tip) {
	ox, oy := p.X-t.w/2, p.Y-t.w/2
	for i, r := range t.rows {
		y := oy + i
		if y < clip.Min.Y || y >= clip.Max.Y || r[0] >= r[1] {
			continue
		}
		x0, x1 := max(ox+r[0], clip.Min.X), min(ox+r[1], clip.Max.X)
		if x0 < x1 {
			fill(x0, x1, y)
		}
	}
}

// Segment は、a から b まで、太さ w の線を描く（a のペン先は描かない。続けて描くとき、同じ点を 2 度塗らない）。
// ペン先は、太さの 1/4（1 以上）のドットごとに置く。
func Segment(fill func(x0, x1, y int), clip image.Rectangle, a, b Pt, w int) {
	t := tipOf(w)
	dx, dy := b.X-a.X, b.Y-a.Y
	n := max(abs(dx), abs(dy))
	if n == 0 {
		return
	}
	step := max(t.w/4, 1)
	for i := step; ; i += step {
		if i > n {
			i = n
		}
		// 四捨五入（負の数も同じに丸める）
		p := Pt{a.X + divRound(dx*i, n), a.Y + divRound(dy*i, n)}
		stamp(fill, clip, p, t)
		if i == n {
			return
		}
	}
}

// Dot は、p にペン先を 1 回置く（線の始まり）。
func Dot(fill func(x0, x1, y int), clip image.Rectangle, p Pt, w int) {
	stamp(fill, clip, p, tipOf(w))
}

// Draw は、線を 1 本すべて描く（始まりの点と、点と点のあいだ）。
func Draw(fill func(x0, x1, y int), clip image.Rectangle, s *Stroke) {
	pts := s.Points()
	if len(pts) == 0 {
		return
	}
	Dot(fill, clip, pts[0], s.W)
	for i := 1; i < len(pts); i++ {
		Segment(fill, clip, pts[i-1], pts[i], s.W)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func divRound(a, b int) int {
	if (a < 0) != (b < 0) {
		return -((-a + b/2) / b)
	}
	return (a + b/2) / b
}

// Page は手書きメモの 1 ページ。W×H はページの大きさ（書き始めたときの画面の範囲）。
type Page struct {
	W       int      `json:"w"`
	H       int      `json:"h"`
	Strokes []Stroke `json:"strokes"`
}

// 紙とインクの色（RGB）。Brain の画面と PNG で同じ
var (
	Paper = [3]uint8{0xf4, 0xf1, 0xe6}
	Ink   = [3]uint8{0x10, 0x14, 0x20}
)

// Render は、ページを W×H の RGB（1 画素 3 バイト）に描く。
func Render(p *Page) []byte {
	buf := make([]byte, p.W*p.H*3)
	for i := 0; i < p.W*p.H; i++ {
		copy(buf[i*3:], Paper[:])
	}
	clip := image.Rect(0, 0, p.W, p.H)
	for i := range p.Strokes {
		s := &p.Strokes[i]
		c := Ink
		if s.Erase {
			c = Paper
		}
		Draw(func(x0, x1, y int) {
			for x := x0; x < x1; x++ {
				copy(buf[(y*p.W+x)*3:], c[:])
			}
		}, clip, s)
	}
	return buf
}
