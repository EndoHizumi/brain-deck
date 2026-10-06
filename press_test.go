package main

import (
	"bytes"
	"image"
	"math/rand"
	"testing"
)

// pressTestLayout は、右上の札に重なるセル、レイヤーのセル、割り当てのないセルを含む格子。
func pressTestLayout(w, h int, press string) *Layout {
	l := &Layout{Cols: 4, Rows: 3, W: w, H: h, Cells: make([]CellView, 12), Title: "編集", Mode: modeTemp, Press: press}
	for i := range l.Cells {
		l.Cells[i] = CellView{Mapped: true, Label: "ブラシ", Sub: "B"}
	}
	l.Cells[3] = CellView{Mapped: true, Label: "右上\n2行", Sub: "Ctrl+Shift+Z"} // 札に重なる
	l.Cells[11] = CellView{Mapped: true, Layer: true, Label: "表示", Sub: "切替"}
	l.Cells[8] = CellView{} // 割り当てなし
	return l
}

// 枠だけを描き直した結果が、全体を描き直した結果と画素単位で同じになること。
// また、書き換えた画素はすべて、返した矩形（Blit する範囲）に入っていること。
func TestPressRingMatchesFullRedraw(t *testing.T) {
	for _, rot := range []int{0, 90, 180, 270} {
		cv := NewCanvas(800, 480, 1600, rgb565, rot)
		l := pressTestLayout(cv.W, cv.H, pressBorder)
		state := make([]bool, len(l.Cells))
		drawAll(cv, l, state)
		ref := NewCanvas(800, 480, 1600, rgb565, rot)
		rng := rand.New(rand.NewSource(int64(rot)))
		for step := 0; step < 60; step++ {
			i := rng.Intn(len(l.Cells))
			state[i] = !state[i]
			before := append([]byte(nil), cv.pix...)
			rs := drawPress(cv, l, i%l.Cols, i/l.Cols, state[i])

			drawAll(ref, l, state)
			if !bytes.Equal(cv.pix, ref.pix) {
				t.Fatalf("rot %d step %d cell %d on=%v: incremental redraw differs from full redraw", rot, step, i, state[i])
			}
			var phys []image.Rectangle
			for _, r := range rs {
				phys = append(phys, cv.physRect(r))
			}
			for o := 0; o < len(before); o += 2 {
				if before[o] == cv.pix[o] && before[o+1] == cv.pix[o+1] {
					continue
				}
				p := image.Pt(o%cv.stride/2, o/cv.stride)
				in := false
				for _, r := range phys {
					in = in || p.In(r)
				}
				if !in {
					t.Fatalf("rot %d cell %d: pixel %v changed outside the blit rects %v", rot, i, p, phys)
				}
			}
		}
	}
}

// 押したときの見た目：border は二重の枠でラベルはそのまま、fill は塗りつぶす。
func TestPressStyles(t *testing.T) {
	at := func(press string, pressed bool) (*image.RGBA, image.Rectangle) {
		cv := NewCanvas(800, 480, 1600, rgb565, 0)
		l := pressTestLayout(cv.W, cv.H, press)
		st := make([]bool, len(l.Cells))
		st[5] = pressed
		drawAll(cv, l, st)
		return cv.Image(), l.rect(1, 1).Inset(cellGap)
	}
	near := func(got [4]uint8, c RGB) bool {
		d := func(a, b uint8) bool { return int(a)-int(b) <= 8 && int(b)-int(a) <= 8 }
		return d(got[0], c.R) && d(got[1], c.G) && d(got[2], c.B)
	}
	px := func(img *image.RGBA, x, y int) [4]uint8 {
		c := img.RGBAAt(x, y)
		return [4]uint8{c.R, c.G, c.B, c.A}
	}

	img, box := at(pressBorder, true)
	for _, tc := range []struct {
		off  int
		want RGB
	}{{0, colPressEdge}, {pressEdgeW - 1, colPressEdge}, {pressEdgeW, colPressed}, {pressRingW - 1, colPressed}, {pressRingW, colCell}} {
		if got := px(img, box.Min.X+tc.off, box.Min.Y+box.Dy()/2); !near(got, tc.want) {
			t.Errorf("border: %d px inside the left edge = %v, want %v", tc.off, got, tc.want)
		}
	}
	idle, _ := at(pressBorder, false)
	inner := box.Inset(pressRingW)
	for y := inner.Min.Y; y < inner.Max.Y; y++ {
		for x := inner.Min.X; x < inner.Max.X; x++ {
			if img.RGBAAt(x, y) != idle.RGBAAt(x, y) {
				t.Fatalf("border: pixel %d,%d inside the ring changed when pressed", x, y)
			}
		}
	}
	if pressRingW >= textMargin {
		t.Errorf("ring (%d) overlaps the label area (textMargin %d)", pressRingW, textMargin)
	}

	img, box = at(pressFill, true)
	if got := px(img, box.Min.X+textMargin/2, box.Min.Y+box.Dy()/2); !near(got, colPressed) {
		t.Errorf("fill: inside color = %v", got)
	}
}

func TestPressStyleConfig(t *testing.T) {
	for _, tc := range []struct {
		yaml, want string
		ok         bool
	}{
		{"", "", true},
		{"display: { press_style: border }\n", pressBorder, true},
		{"display: { press_style: fill }\n", pressFill, true},
		{"display: { press_style: glow }\n", "", false},
	} {
		cfg, err := parseConfig([]byte(tc.yaml + "layers: [{ name: base }]\n"))
		if (err == nil) != tc.ok {
			t.Errorf("%q: err = %v", tc.yaml, err)
			continue
		}
		if err != nil {
			continue
		}
		km, _, err := compileKeymap(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if km.Press != tc.want {
			t.Errorf("%q: press = %q", tc.yaml, km.Press)
		}
	}
	// 設定 GUI からの保存で変えられる（デーモンの再起動は要らない）
	cur, _ := parseConfig([]byte("layers: [{ name: base }]\n"))
	next, _ := parseConfig([]byte("display: { press_style: fill }\nlayers: [{ name: base }]\n"))
	if p := restartProblems(cur, next); len(p) != 0 {
		t.Errorf("changing press_style should be allowed live: %v", p)
	}
}

// 押したとき・離したときの描き直しと、フレームバッファへの転送にかかる時間。
// 実機では go test -c で作ったバイナリを -test.bench PressRedraw で動かす。
func BenchmarkPressRedraw(b *testing.B) {
	for _, press := range []string{pressFill, pressBorder} {
		for _, cell := range []struct {
			name     string
			col, row int
		}{{"cell", 1, 1}, {"badge-cell", 3, 0}} {
			b.Run(press+"/"+cell.name, func(b *testing.B) {
				cv := NewCanvas(800, 480, 1600, rgb565, 0)
				l := pressTestLayout(cv.W, cv.H, press)
				drawAll(cv, l, make([]bool, len(l.Cells)))
				fb := &Framebuffer{mem: make([]byte, 1600*480), Info: FBInfo{LineLength: 1600}}
				for i := 0; i < b.N; i++ {
					on := i%2 == 0
					var rs []image.Rectangle
					if l.pressFill() {
						rs = []image.Rectangle{drawCell(cv, l, cell.col, cell.row, on)}
					} else {
						rs = drawPress(cv, l, cell.col, cell.row, on)
					}
					for _, r := range rs {
						fb.Blit(cv, cv.physRect(r))
					}
				}
			})
		}
	}
}
