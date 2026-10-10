package main

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"image"
	"io"
	"log"
	"sort"
	"sync"
)

// ---------- 読み物の文字（font/reader-*.lrf.gz） ----------
//
// 長い文章を読むための明朝体（Noto Serif JP を tools/mkreadfont で 16 階調のビットマップにしたもの。SIL OFL 1.1、
// font/reader-OFL.txt）。本文は 24 ドットと 32 ドット、ルビは 12 ドットと 16 ドット（かなと記号だけ）。
// gzip のまま lefthand に埋め込み、使う大きさを初めて使うときに戻す（24 ドットで 2.2 MB、32 ドットで 3.9 MB のメモリ）。
// 入っていない文字は、k8x12 を 2 倍にして描く。

//go:embed font/reader-12.lrf.gz
var readFont12 []byte

//go:embed font/reader-16.lrf.gz
var readFont16 []byte

//go:embed font/reader-24.lrf.gz
var readFont24 []byte

//go:embed font/reader-32.lrf.gz
var readFont32 []byte

var readFontFiles = map[int][]byte{12: readFont12, 16: readFont16, 24: readFont24, 32: readFont32}

// ReadFont は、1 つの大きさの読み物の文字。
type ReadFont struct {
	S    int
	base int
	n    int
	idx  []byte // [rune uint32、幅 uint8、予備 3、位置 uint32] × n
	data []byte
}

var (
	readFontsMu sync.Mutex
	readFonts   = map[int]*ReadFont{}
)

// readFontOf は、大きさ s の文字を返す（初めてなら戻す）。ないか壊れていれば nil（k8x12 で描く）。
func readFontOf(s int) *ReadFont {
	readFontsMu.Lock()
	defer readFontsMu.Unlock()
	if f, ok := readFonts[s]; ok {
		return f
	}
	f, err := parseReadFont(readFontFiles[s])
	if err != nil {
		log.Printf("reader: font %d: %v (drawing with k8x12)", s, err)
	}
	readFonts[s] = f
	return f
}

func parseReadFont(gz []byte) (*ReadFont, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 || string(b[:4]) != "LRF1" {
		return nil, io.ErrUnexpectedEOF
	}
	f := &ReadFont{S: int(binary.LittleEndian.Uint16(b[4:])), base: int(binary.LittleEndian.Uint16(b[6:])), n: int(binary.LittleEndian.Uint32(b[8:]))}
	if len(b) < 12+f.n*12 {
		return nil, io.ErrUnexpectedEOF
	}
	f.idx, f.data = b[12:12+f.n*12], b[12+f.n*12:]
	return f, nil
}

// glyph は、r の幅と、4 ビットの濃さの画素（幅 × S）を返す。
func (f *ReadFont) glyph(r rune) (w int, pix []byte, ok bool) {
	if f == nil {
		return 0, nil, false
	}
	at := func(i int) rune { return rune(binary.LittleEndian.Uint32(f.idx[i*12:])) }
	i := sort.Search(f.n, func(i int) bool { return at(i) >= r })
	if i >= f.n || at(i) != r {
		return 0, nil, false
	}
	w = int(f.idx[i*12+4])
	off := int(binary.LittleEndian.Uint32(f.idx[i*12+8:]))
	return w, f.data[off : off+(w*f.S+1)/2], true
}

// halfWidthRune は、半角の文字（箱の幅が S/2）か。
func halfWidthRune(r rune) bool { return r >= 0x20 && r < 0x7f }

// ---------- 縦書きのときの文字の向きと位置 ----------

// vertRotate は、縦書きで 90 度右に回して描く文字（横書きの字形を回すと、縦書きの形になるもの）。
var vertRotate = map[rune]bool{}

// vertShift は、縦書きで右上に寄せる文字（句読点）。
var vertShift = map[rune]bool{'、': true, '。': true, '，': true, '．': true, '､': true, '｡': true}

// vertSmall は、縦書きで少し右上に寄せる小さいかな。
var vertSmall = map[rune]bool{}

func init() {
	for _, r := range "ー―‐－—–〜～…‥＝＿｜→←「」『』（）〔〕［］｛｝〈〉《》【】〝〟＜＞≪≫(){}[]<>“”‘’:;：；" {
		vertRotate[r] = true
	}
	for _, r := range "ぁぃぅぇぉっゃゅょゎゕゖァィゥェォッャュョヮヵヶ" {
		vertSmall[r] = true
	}
}

// inkLevels は、紙の色からインクの色までの 16 段の色（RGB565 の値。描く前に作る）。
type inkLevels [16]uint16

func makeInkLevels(paper, ink RGB) inkLevels {
	var l inkLevels
	for i := range l {
		mix := func(a, b uint8) uint8 { return uint8((int(a)*(15-i) + int(b)*i + 7) / 15) }
		l[i] = uint16(rgb565.pack(RGB{mix(paper.R, ink.R), mix(paper.G, ink.G), mix(paper.B, ink.B)}))
	}
	return l
}

// readGlyph は、どう描くかを決めた 1 文字。
type readGlyph struct {
	f      *ReadFont
	w      int    // 箱の幅（S か S/2）
	pix    []byte // 4 ビットの濃さ。nil なら k8x12 で描く
	r      rune
	rotate bool // 90 度右に回す
	dx, dy int  // ずらす量
}

// glyphFor は、r を大きさ f で描くときの形（vertical なら縦書きの向きと位置）。
func glyphFor(f *ReadFont, r rune, vertical bool) readGlyph {
	g := readGlyph{f: f, r: r, w: f.sizeOr(24)}
	if w, pix, ok := f.glyph(r); ok {
		g.w, g.pix = w, pix
	} else if halfWidthRune(r) {
		g.w = f.sizeOr(24) / 2
	}
	if !vertical {
		return g
	}
	S := f.sizeOr(24)
	switch {
	case halfWidthRune(r) || vertRotate[r]:
		g.rotate = true
	case vertShift[r]:
		g.dx, g.dy = S*7/12, -S/2
	case vertSmall[r]:
		g.dx, g.dy = S/10, -S/10
	}
	return g
}

func (f *ReadFont) sizeOr(def int) int {
	if f == nil {
		return def
	}
	return f.S
}

// drawGlyph は、文字 g を、左上 (x, y) の箱に描く。回すときは、箱は（高さ g.w、幅 S）になる。clip の外は描かない。
func drawGlyph(cv *Canvas, g readGlyph, x, y int, lv *inkLevels, clip image.Rectangle) {
	S := g.f.sizeOr(24)
	x, y = x+g.dx, y+g.dy
	if g.pix == nil {
		// k8x12 を 2 倍にして、箱の中央に描く（半角の文字は 1 倍）
		sc := max(S/12, 1)
		if halfWidthRune(g.r) {
			sc = max(S/24, 1)
		}
		w, rows := font.glyphOrBox(g.r)
		bw, bh := w*sc, fontH*sc
		ox, oy := x+(g.w-bw)/2, y+(S-bh)/2
		if g.rotate {
			ox, oy = x+(S-bh)/2, y+(g.w-bw)/2
		}
		for gy, bits := range rows {
			for gx := 0; gx < w; gx++ {
				if bits&(0x80>>gx) == 0 {
					continue
				}
				px, py := gx, gy
				if g.rotate {
					px, py = fontH-1-gy, gx
				}
				cv.fill(image.Rect(ox+px*sc, oy+py*sc, ox+(px+1)*sc, oy+(py+1)*sc).Intersect(clip), cv.unpackRGB(lv[15]))
			}
		}
		return
	}
	w := g.w
	for gy := 0; gy < S; gy++ {
		for gx := 0; gx < w; gx++ {
			i := gy*w + gx
			v := g.pix[i/2]
			if i%2 == 0 {
				v >>= 4
			}
			v &= 15
			if v == 0 {
				continue
			}
			px, py := x+gx, y+gy
			if g.rotate {
				px, py = x+S-1-gy, y+gx
			}
			if px < clip.Min.X || px >= clip.Max.X || py < clip.Min.Y || py >= clip.Max.Y {
				continue
			}
			cv.set565(px, py, lv[v])
		}
	}
}

// set565 は、(x, y) を RGB565 の値 v にする（読み物の文字。Brain の画面は RGB565）。
func (cv *Canvas) set565(x, y int, v uint16) {
	X, Y := cv.phys(x, y)
	o := Y*cv.stride + X*cv.pf.Bpp
	if cv.pf == rgb565 {
		cv.pix[o], cv.pix[o+1] = byte(v), byte(v>>8)
		return
	}
	c := rgb565.unpack(uint32(v))
	p := cv.pf.pack(RGB{c.R, c.G, c.B})
	for i := 0; i < cv.pf.Bpp; i++ {
		cv.pix[o+i] = byte(p >> (8 * i))
	}
}

func (cv *Canvas) unpackRGB(v uint16) RGB {
	c := rgb565.unpack(uint32(v))
	return RGB{c.R, c.G, c.B}
}
