package main

import (
	"image"
	"os"
	"path/filepath"
	"testing"
)

// 画面の枠と、タッチの判定が同じ境界になっていること。
// キャリブレーションは画面の端を min/max に合わせているので、
// 画素 x の中心に当たる生座標を cellOf に通すと、x を含むセルになるはず。
func TestCellSpanMatchesTouch(t *testing.T) {
	tc := &TouchConfig{Cols: 4, Rows: 3, MinX: 226, MaxX: 3938, MinY: 3803, MaxY: 384}
	const w, h = 800, 480
	for _, n := range []int{1, 3, 4, 7} {
		for _, size := range []int{w, h, 799} {
			for i := 0; i < n; i++ {
				lo, hi := cellSpan(i, n, size)
				for x := lo; x < hi; x++ {
					if got := x * n / size; got != i {
						t.Fatalf("n=%d size=%d x=%d: span says %d, floor says %d", n, size, x, i, got)
					}
				}
			}
		}
	}
	for px := 0; px < w; px++ {
		for _, py := range []int{0, 159, 160, 319, 320, 479} {
			rawX := tc.MinX + int32((float64(px)+0.5)*float64(tc.MaxX-tc.MinX)/w)
			rawY := tc.MinY + int32((float64(py)+0.5)*float64(tc.MaxY-tc.MinY)/h)
			col, row := touchCell(tc, rawX, rawY)
			l := &Layout{Cols: tc.Cols, Rows: tc.Rows, W: w, H: h}
			if !image.Pt(px, py).In(l.rect(col, row)) {
				t.Fatalf("pixel %d,%d -> raw %d,%d -> cell %d,%d, rect %v", px, py, rawX, rawY, col, row, l.rect(col, row))
			}
		}
	}
}

func TestCellConfigForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte(`
touch:
  cols: 2
  rows: 2
  cells:
    "0,0": B
    "1,0": { key: LCTRL+Z, label: "取り消し" }
    "0,1":
      key: E
`), 0o600)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Touch.Cells
	if c["0,0"] != (CellSpec{Key: "B"}) || c["1,0"] != (CellSpec{Key: "LCTRL+Z", Label: "取り消し"}) || c["0,1"] != (CellSpec{Key: "E"}) {
		t.Fatalf("cells = %+v", c)
	}
	if !cfg.displayEnabled() || cfg.Display.Device != "/dev/fb0" {
		t.Fatalf("display defaults: %+v", cfg.Display)
	}
	l := buildLayout(cfg.Touch)
	if v := l.Cells[0]; v.Label != "B" || v.Sub != "" {
		t.Errorf("0,0: %+v", v)
	}
	if v := l.Cells[1]; v.Label != "取り消し" || v.Sub != "Ctrl+Z" {
		t.Errorf("1,0: %+v", v)
	}
	if l.Cells[3].Mapped {
		t.Errorf("1,1 should be unmapped")
	}

	os.WriteFile(path, []byte("touch:\n  cells:\n    \"0,0\": { label: x }\n"), 0o600)
	if _, err := loadConfig(path); err == nil {
		t.Error("cell without key should be an error")
	}
}

func TestFont(t *testing.T) {
	for _, r := range "ブラシ取消保存ABC[]+←→" {
		if _, _, ok := font.glyph(r); !ok {
			t.Errorf("glyph %q missing", r)
		}
	}
	if w := font.textWidth("Ctrl"); w != 16 {
		t.Errorf("half-width text width = %d, want 16", w)
	}
	if w := font.textWidth("ブラシ"); w != 24 {
		t.Errorf("full-width text width = %d, want 24", w)
	}
	if w, _ := font.glyphOrBox('\U0001F600'); w != 8 {
		t.Errorf("missing glyph width = %d", w)
	}
}

func TestPack565(t *testing.T) {
	for _, tc := range []struct {
		c    RGB
		want uint32
	}{
		{RGB{0xff, 0, 0}, 0xf800}, {RGB{0, 0xff, 0}, 0x07e0}, {RGB{0, 0, 0xff}, 0x001f}, {RGB{0xff, 0xff, 0xff}, 0xffff},
	} {
		if got := rgb565.pack(tc.c); got != tc.want {
			t.Errorf("pack(%v) = %#x want %#x", tc.c, got, tc.want)
		}
		if got := rgb565.unpack(tc.want); got.R != tc.c.R || got.G != tc.c.G || got.B != tc.c.B {
			t.Errorf("unpack(%#x) = %v", tc.want, got)
		}
	}
}

// 回転しても、セルの論理矩形と物理矩形の面積が一致し、描画が画面内に収まること。
func TestRotateAndRedraw(t *testing.T) {
	for _, rot := range []int{0, 90, 180, 270} {
		cv := NewCanvas(400, 240, 800, rgb565, rot)
		l := &Layout{Cols: 4, Rows: 3, W: cv.W, H: cv.H, Cells: make([]CellView, 12)}
		l.Cells[0] = CellView{Mapped: true, Label: "ブラシ", Sub: "B"}
		pressed := make([]bool, 12)
		drawAll(cv, l, pressed)
		r := drawCell(cv, l, 0, 0, true)
		pr := cv.physRect(r)
		if pr.Dx()*pr.Dy() != r.Dx()*r.Dy() || !pr.In(image.Rect(0, 0, 400, 240)) {
			t.Errorf("rot %d: logical %v -> physical %v", rot, r, pr)
		}
		// 押下中のセルの中心付近は押下色になっている
		img := cv.Image()
		got := img.RGBAAt(r.Min.X+cellGap+3, r.Min.Y+cellGap+3)
		if got.R < 0xf0 || got.B > 0x50 {
			t.Errorf("rot %d: pressed color = %v", rot, got)
		}
	}
}
