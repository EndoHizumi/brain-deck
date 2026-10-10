package ink

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"testing"
)

func TestTipIsRound(t *testing.T) {
	for _, w := range []int{1, 2, 3, 4, 8, 20} {
		tp := tipOf(w)
		if len(tp.rows) != w {
			t.Fatalf("w=%d rows %d", w, len(tp.rows))
		}
		for i, r := range tp.rows {
			o := tp.rows[w-1-i]
			if r != o || r[0] != w-r[1] {
				t.Errorf("w=%d row %d %v is not symmetric (%v)", w, i, r, o)
			}
		}
	}
}

func TestDrawCoversTheLine(t *testing.T) {
	s := Stroke{W: 4}
	s.Add(Pt{10, 10})
	s.Add(Pt{50, 30})
	got := map[image.Point]bool{}
	Draw(func(x0, x1, y int) {
		for x := x0; x < x1; x++ {
			got[image.Pt(x, y)] = true
		}
	}, image.Rect(0, 0, 100, 100), &s)
	for _, p := range []image.Point{{10, 10}, {30, 20}, {50, 30}} {
		if !got[p] {
			t.Errorf("%v is not painted", p)
		}
	}
	if got[image.Pt(10, 30)] || got[image.Pt(50, 10)] {
		t.Error("painted outside the line")
	}
	b := s.Bounds()
	for p := range got {
		if !p.In(b) {
			t.Errorf("%v outside the bounds %v", p, b)
		}
	}
	// 1 本を続けて描いたもの（Dot と Segment）と Draw は同じ
	got2 := map[image.Point]bool{}
	fill := func(x0, x1, y int) {
		for x := x0; x < x1; x++ {
			got2[image.Pt(x, y)] = true
		}
	}
	Dot(fill, image.Rect(0, 0, 100, 100), Pt{10, 10}, 4)
	Segment(fill, image.Rect(0, 0, 100, 100), Pt{10, 10}, Pt{50, 30}, 4)
	if len(got2) != len(got) {
		t.Errorf("Dot+Segment %d pixels, Draw %d", len(got2), len(got))
	}
}

// 描き方を変えたら、手書きメモの PNG の見た目が変わる（brain-deck と Brain で同じになるよう、両方でこの関数を使う）
func TestRenderStable(t *testing.T) {
	p := &Page{W: 60, H: 40, Strokes: []Stroke{{W: 2, P: []int{5, 5, 55, 35}}, {W: 8, P: []int{5, 35, 30, 5, 55, 35}}, {W: 6, Erase: true, P: []int{30, 0, 30, 40}}}}
	s := sha256.Sum256(Render(p))
	if got := hex.EncodeToString(s[:8]); got != "39d48b09ae6e1cec" {
		t.Errorf("render hash %s (update the expected value if the drawing changed on purpose)", got)
	}
}
