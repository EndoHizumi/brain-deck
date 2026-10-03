package main

import (
	_ "embed"
	"encoding/binary"
	"sort"
)

// k8x12（Num Kadoma 作、8x12 ドットの日本語ビットマップフォント）を
// tools/mkfont で変換したもの。ライセンスは font/k8x12-LICENSE.txt を参照。
//
//go:embed font/k8x12.bin
var fontData []byte

const (
	fontH       = 12
	glyphStride = 16 // rune(3) + 送り幅(1) + 12 行
)

type Font struct {
	n    int
	data []byte // グリフ表（ヘッダを除く）
}

var font = loadFont(fontData)

func loadFont(b []byte) *Font {
	if len(b) < 6 || string(b[:4]) != "K812" {
		panic("embedded font is broken")
	}
	n := int(binary.LittleEndian.Uint16(b[4:6]))
	return &Font{n: n, data: b[6 : 6+n*glyphStride]}
}

func (f *Font) runeAt(i int) rune {
	p := f.data[i*glyphStride:]
	return rune(p[0]) | rune(p[1])<<8 | rune(p[2])<<16
}

// glyph はグリフの送り幅とビットマップを返す。ないときは ok=false。
func (f *Font) glyph(r rune) (width int, rows []byte, ok bool) {
	i := sort.Search(f.n, func(i int) bool { return f.runeAt(i) >= r })
	if i >= f.n || f.runeAt(i) != r {
		return 0, nil, false
	}
	p := f.data[i*glyphStride:]
	return int(p[3]), p[4 : 4+fontH], true
}

// 字形のない文字の代わりに描く枠（全角幅）
var missingGlyph = []byte{0, 0xFE, 0x82, 0x82, 0x82, 0x82, 0x82, 0x82, 0x82, 0xFE, 0, 0}

func (f *Font) glyphOrBox(r rune) (int, []byte) {
	if w, rows, ok := f.glyph(r); ok {
		return w, rows
	}
	if r < 0x80 {
		return 4, missingGlyph // 半角は左半分だけ使う
	}
	return 8, missingGlyph
}

// textWidth は等倍での文字列の幅（ドット）を返す。
func (f *Font) textWidth(s string) int {
	w := 0
	for _, r := range s {
		gw, _ := f.glyphOrBox(r)
		w += gw
	}
	return w
}
