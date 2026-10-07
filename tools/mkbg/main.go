// mkbg は、config/background-example.yaml で使う背景画像の例を作る。
// 画像はすべて式で描いたもの（グラデーションと模様）で、写真などの著作物は使っていない。
//
//	go run ./tools/mkbg config/background-images
//
// 出力は lefthand の画像ファイル（<id>.565。形は image.go）と、名前を書いた index.json。
// 同じ式からは同じファイルができるので、id（SHA-256 の先頭 16 文字）も変わらない。
// 実際の使い方では、画像は設定 GUI が変換して Brain に送る。ここで作るのは、例とテスト用。
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"
)

type pattern struct {
	name string
	w, h int
	f    func(x, y float64) (r, g, b float64) // x, y は 0..1。色は 0..255
}

func mix(a, b, t float64) float64 { return a + (b-a)*t }

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

var patterns = []pattern{
	{"壁紙 夜明けのグラデーション", 800, 480, func(x, y float64) (float64, float64, float64) {
		// 左上の濃い紺から右下の紫へ。右上に淡い光
		t := clamp01((x*0.7 + y*0.5) / 1.2)
		r, g, b := mix(10, 70, t), mix(18, 30, t), mix(48, 96, t)
		d := math.Hypot(x-0.85, (y-0.1)*0.6)
		glow := math.Exp(-d * d * 18)
		return r + 70*glow, g + 50*glow, b + 60*glow
	}},
	{"壁紙 斜めのしま", 800, 480, func(x, y float64) (float64, float64, float64) {
		s := math.Sin((x*800 + y*480) / 800 * 2 * math.Pi * 10)
		v := 0.5 + 0.5*s
		v = v * v * (3 - 2*v)
		return mix(14, 22, v), mix(40, 60, v), mix(44, 64, v)
	}},
	{"セル 夕焼け", 192, 152, func(x, y float64) (float64, float64, float64) {
		return mix(150, 60, y), mix(70, 20, y), mix(40, 90, y)
	}},
	{"セル 水玉", 192, 152, func(x, y float64) (float64, float64, float64) {
		px, py := x*192, y*152
		cx, cy := math.Mod(px, 32)-16, math.Mod(py+(math.Floor(px/32))*16, 32)-16
		d := math.Hypot(cx, cy)
		dot := clamp01(9 - d)
		return mix(20, 40, dot), mix(60, 110, dot), mix(50, 90, dot)
	}},
	{"セル 波", 392, 152, func(x, y float64) (float64, float64, float64) {
		w := 0.5 + 0.5*math.Sin(x*2*math.Pi*2+math.Sin(y*math.Pi*3)*1.5)
		return mix(10, 20, w), mix(50, 80, y*0.5+w*0.3), mix(60, 110, x*0.6+w*0.2)
	}},
}

// dither は、色を Floyd–Steinberg のディザリングで RGB565 にする（設定 GUI の既定と同じ方式）。
func dither(p pattern) []byte {
	w, h := p.w, p.h
	buf := make([]float64, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b := p.f((float64(x)+0.5)/float64(w), (float64(y)+0.5)/float64(h))
			i := (y*w + x) * 3
			buf[i], buf[i+1], buf[i+2] = r, g, b
		}
	}
	bits := [3]int{5, 6, 5}
	out := make([]byte, w*h*2)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var q [3]int
			for c := 0; c < 3; c++ {
				i := (y*w+x)*3 + c
				m := float64(int(1)<<bits[c] - 1)
				v := math.Max(0, math.Min(255, buf[i]))
				q[c] = int(math.Round(v * m / 255))
				back := float64(q[c] * 255 / int(m)) // Brain の表示と同じ戻し方（fb.go の unpack）
				e := v - back
				add := func(dx, dy int, k float64) {
					if x+dx >= 0 && x+dx < w && y+dy < h {
						buf[((y+dy)*w+x+dx)*3+c] += e * k
					}
				}
				add(1, 0, 7.0/16)
				add(-1, 1, 3.0/16)
				add(0, 1, 5.0/16)
				add(1, 1, 1.0/16)
			}
			binary.LittleEndian.PutUint16(out[(y*w+x)*2:], uint16(q[0]<<11|q[1]<<5|q[2]))
		}
	}
	return out
}

type meta struct {
	Name   string    `json:"name"`
	W      int       `json:"w"`
	H      int       `json:"h"`
	Bytes  int64     `json:"bytes"`
	SHA256 string    `json:"sha256"`
	Added  time.Time `json:"added"`
	Source string    `json:"source"`
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: mkbg <output dir>")
	}
	dir := os.Args[1]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	index := map[string]meta{}
	for _, p := range patterns {
		b := make([]byte, 8, 8+p.w*p.h*2)
		copy(b, "LHI1")
		binary.LittleEndian.PutUint16(b[4:], uint16(p.w))
		binary.LittleEndian.PutUint16(b[6:], uint16(p.h))
		b = append(b, dither(p)...)
		s := sha256.Sum256(b)
		sum := hex.EncodeToString(s[:])
		id := sum[:16]
		if err := os.WriteFile(filepath.Join(dir, id+".565"), b, 0o644); err != nil {
			log.Fatal(err)
		}
		index[id] = meta{Name: p.name, W: p.w, H: p.h, Bytes: int64(len(b)), SHA256: sum,
			Added: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), Source: "mkbg"}
		fmt.Printf("%s  %dx%d  %s\n", id, p.w, p.h, p.name)
	}
	j, _ := json.MarshalIndent(map[string]any{"images": index}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "index.json"), append(j, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
}
