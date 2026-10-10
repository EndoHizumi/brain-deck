package imgconv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "testdata/imgconv の結果を書き直す")

// Pattern は、試験の元の画像（式で描いた模様。gui/test/imgconv.test.ts と同じ式）。
func Pattern(w, h int, alpha bool) RGBA {
	p := RGBA{W: w, H: h, Pix: make([]byte, w*h*4)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			p.Pix[o] = byte((x*5 + y*3) % 256)
			p.Pix[o+1] = byte((x * y) % 256)
			p.Pix[o+2] = byte((x ^ y) & 255)
			p.Pix[o+3] = 255
			if alpha {
				p.Pix[o+3] = byte((x + y) % 256)
			}
		}
	}
	return p
}

// Cases は、Go と TypeScript で同じ結果になることを確かめる組（gui/test/imgconv.test.ts と同じ）。
var Cases = []struct {
	Name   string
	SW, SH int
	Alpha  bool
	TW, TH int
	P      Params
}{
	{"up-fs", 300, 200, false, 800, 480, Params{CX: -1, CY: -1, Dither: true}},
	{"crop-zoom-dark", 300, 200, false, 120, 200, Params{CX: 100, CY: 80, ZoomMilli: 1500, Brightness: -35, Dither: true}},
	{"down-none-bright", 300, 200, false, 64, 40, Params{CX: -1, CY: -1, Brightness: 20}},
	{"alpha", 90, 70, true, 50, 50, Params{CX: -1, CY: -1, Dither: true}},
	{"shrink", 2100, 40, false, 400, 20, Params{CX: -1, CY: -1, Dither: true}},
	{"portrait", 300, 200, false, 480, 800, Params{CX: 250, CY: -1, Dither: true}},
}

// golden は、結果の SHA-256（testdata/imgconv.json）。gui/test/imgconv.test.ts も同じファイルと比べる。
const golden = "../../testdata/imgconv.json"

func TestGolden(t *testing.T) {
	got := map[string]string{}
	sum := func(name string, b []byte) {
		s := sha256.Sum256(b)
		got[name] = hex.EncodeToString(s[:])
	}
	for _, c := range Cases {
		pix, _ := Convert(Pattern(c.SW, c.SH, c.Alpha), c.TW, c.TH, c.P)
		sum(c.Name, Encode(c.TW, c.TH, pix))
	}
	// 横向きの写真を縦向きの画面に（上下に黒い帯）と、その逆
	pix, _ := Convert(Pattern(300, 200, false), 800, 480, Cases[0].P)
	sum("fit-portrait", Encode(480, 800, Fit565(pix, 800, 480, 480, 800)))
	sum("fit-landscape", Encode(800, 480, Fit565(Fit565(pix, 800, 480, 480, 800), 480, 800, 800, 480)))
	if *update {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(filepath.FromSlash(golden), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(filepath.FromSlash(golden))
	if err != nil {
		t.Fatalf("%v (go test ./internal/imgconv -update で作る)", err)
	}
	want := map[string]string{}
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	for k, v := range got {
		if want[k] != v {
			t.Errorf("%s: SHA-256 %s, want %s", k, v[:16], want[k])
		}
	}
	if len(want) != len(got) {
		t.Errorf("%d cases in %s, %d here", len(want), golden, len(got))
	}
}

func TestFitCrop(t *testing.T) {
	c := FitCrop(1000, 500, 192, 232, 1000, 500, 250)
	if c.H != 500 || c.W != 413 || c.X != 500-206 || c.Y != 0 {
		t.Errorf("crop %+v", c)
	}
	z := FitCrop(1000, 500, 192, 232, 4000, 0, 0)
	if z.X != 0 || z.Y != 0 || z.W != 103 {
		t.Errorf("zoom %+v", z)
	}
	e := FitCrop(1000, 500, 192, 232, 2000, 5000, 5000)
	if e.X+e.W != 1000 || e.Y+e.H != 500 {
		t.Errorf("edge %+v", e)
	}
	if FitCrop(100, 100, 10, 10, 99000, 50, 50).W != 12 {
		t.Error("zoom limit")
	}
}

func TestResampleAndDither(t *testing.T) {
	src := RGBA{W: 4, H: 1, Pix: []byte{255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 255, 0, 0, 0, 255}}
	if v := Resample(src, Crop{0, 0, 4, 1}, 1, 1); v[0] != Full/2+0 && v[0] != Full/2 {
		t.Errorf("average %v", v)
	}
	if v := Resample(src, Crop{0, 0, 4, 1}, 2, 1); v[0] != Full || v[3] != 0 {
		t.Errorf("halves %v", v)
	}
	// 段の間の灰色：ディザリングなしは 1 色、あれば平均を保つ
	w, h := 64, 64
	v := make([]int32, w*h*3)
	for i := range v {
		v[i] = 12 * Unit
	}
	none, fs := To565(v, w, h, false), To565(v, w, h, true)
	mean := func(p []uint16) float64 {
		s := 0
		for _, x := range p {
			r, _, _ := Unpack(x)
			s += int(r)
		}
		return float64(s) / float64(len(p))
	}
	if m := mean(fs); m < 11.7 || m > 12.3 {
		t.Errorf("fs mean %v", m)
	}
	if m := mean(none); m > 9 && m < 15 {
		t.Errorf("none mean %v", m)
	}
}
