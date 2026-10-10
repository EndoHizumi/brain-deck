// mkreadfont は、読み物のアプリ（widget: reader）の文字（font/reader-*.lrf）を作る。
// 自由なライセンス（SIL Open Font License 1.1）の Noto Serif JP（明朝体）を、決めた大きさで 16 階調のビットマップにする。
//
//	go run ./tools/mkreadfont -otf NotoSerifJP-Regular.otf -o font   （font/reader-{12,16,24,32}.lrf.gz を書く）
//
// NotoSerifJP-Regular.otf は https://github.com/notofonts/noto-cjk（Serif/SubsetOTF/JP）から取る。
// 作ったフォントも OFL 1.1（font/reader-OFL.txt）。名前に Noto は使わない（「lefthand 読み物の明朝」）。
//
// 文字は、本文の 24、32 ドットが JIS X 0208 のすべて（記号、英数字、かな、第一水準と第二水準の漢字）と、ASCII、読み物でよく使ういくつかの記号。
// ルビの 12、16 ドットは、かなと ASCII と記号だけ。
// 入っていない文字は、lefthand が k8x12 で代わりに描く。
//
// ファイルの形（LRF1、リトルエンディアン）：
//
//	0..3   "LRF1"
//	4..5   大きさ S（ドット。全角の文字の箱は S×S、半角は S/2×S）
//	6..7   ベースライン（上から）
//	8..11  文字の数 N
//	12..   N 個の [rune uint32、幅 uint8（S か S/2）、予備 3 バイト、データの位置 uint32]（rune の順）
//	...    データ：1 文字は 幅×S 画素、1 画素 4 ビット（上位 4 ビットが先）の濃さ（0 が紙、15 がインク）
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"log"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/text/encoding/japanese"
)

func charset() []rune {
	set := map[rune]bool{}
	for r := rune(0x20); r < 0x7f; r++ {
		set[r] = true
	}
	// JIS X 0208：EUC-JP の 2 バイト（0xa1〜0xfe の組）をすべて Unicode にする
	dec := japanese.EUCJP.NewDecoder()
	for hi := 0xa1; hi <= 0xfe; hi++ {
		for lo := 0xa1; lo <= 0xfe; lo++ {
			b, err := dec.Bytes([]byte{byte(hi), byte(lo)})
			if err != nil {
				continue
			}
			r := []rune(string(b))
			if len(r) == 1 && r[0] != 0xfffd {
				set[r[0]] = true
			}
		}
	}
	for _, r := range "〜～―…‥・「」『』（）〔〕［］｛｝〈〉《》【】〝〟“”‘’—–‐々〆ヶヵゝゞヽヾ〃仝ゐゑヰヱヴ゛゜＃％＆＊＠￥＄！？" {
		set[r] = true
	}
	out := make([]rune, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func halfWidth(r rune) bool { return r >= 0x20 && r < 0x7f }

func build(f *opentype.Font, size int, runes []rune) ([]byte, int, error) {
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, 0, err
	}
	defer face.Close()
	m := face.Metrics()
	// 全角の箱（S×S）の中で、ベースラインは上から ascent × S / (ascent + descent)
	asc, desc := m.Ascent.Round(), m.Descent.Round()
	base := (asc*size + (asc+desc)/2) / (asc + desc)
	type ent struct {
		r   rune
		w   int
		off int
	}
	var ents []ent
	var data []byte
	for _, r := range runes {
		if _, ok := face.GlyphAdvance(r); !ok {
			continue
		}
		w := size
		if halfWidth(r) {
			w = size / 2
		}
		img := image.NewAlpha(image.Rect(0, 0, w, size))
		adv, _ := face.GlyphAdvance(r)
		// 箱の横の中央に置く（全角の文字は送り幅が S なので、そのまま）
		x := fixed.I(w)/2 - adv/2
		d := font.Drawer{Dst: img, Src: image.Opaque, Face: face, Dot: fixed.Point26_6{X: x, Y: fixed.I(base)}}
		d.DrawString(string(r))
		_ = draw.Src
		off := len(data)
		n := w * size
		pix := make([]byte, (n+1)/2)
		for i := 0; i < n; i++ {
			v := img.Pix[i] >> 4
			if i%2 == 0 {
				pix[i/2] |= v << 4
			} else {
				pix[i/2] |= v
			}
		}
		data = append(data, pix...)
		ents = append(ents, ent{r, w, off})
	}
	hdr := make([]byte, 12+len(ents)*12)
	copy(hdr, "LRF1")
	binary.LittleEndian.PutUint16(hdr[4:], uint16(size))
	binary.LittleEndian.PutUint16(hdr[6:], uint16(base))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(ents)))
	for i, e := range ents {
		o := 12 + i*12
		binary.LittleEndian.PutUint32(hdr[o:], uint32(e.r))
		hdr[o+4] = byte(e.w)
		binary.LittleEndian.PutUint32(hdr[o+8:], uint32(e.off))
	}
	return append(hdr, data...), len(ents), nil
}

func main() {
	otf := flag.String("otf", "", "Noto Serif JP の OTF")
	out := flag.String("o", "font", "書き出すディレクトリ")
	flag.Parse()
	b, err := os.ReadFile(*otf)
	if err != nil {
		log.Fatal(err)
	}
	f, err := opentype.Parse(b)
	if err != nil {
		log.Fatal(err)
	}
	runes := charset()
	var ruby []rune // ルビの文字（12、16 ドット）：かな、ASCII、記号だけ（漢字は k8x12 で代わりに描く）
	for _, r := range runes {
		if r < 0x7f || r >= 0x3000 && r < 0x3100 || r == 0x30fc || r >= 0xff00 && r < 0xff10 {
			ruby = append(ruby, r)
		}
	}
	for _, size := range []int{12, 16, 24, 32} {
		set := runes
		if size < 20 {
			set = ruby
		}
		data, n, err := build(f, size, set)
		if err != nil {
			log.Fatal(err)
		}
		// gzip にして置く（lefthand に埋め込み、使う大きさだけを読むときに戻す）
		p := filepath.Join(*out, fmt.Sprintf("reader-%d.lrf.gz", size))
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(data)
		zw.Close()
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %d glyphs, %d bytes (%d before gzip)\n", p, n, buf.Len(), len(data))
	}
}
