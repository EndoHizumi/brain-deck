// mkfont は 8x12 の BDF フォント（k8x12）を、lefthand に埋め込む固定長バイナリに変換する。
//
//	go run ./tools/mkfont k8x12.bdf font/k8x12.bin
//
// 出力形式（リトルエンディアン）:
//
//	"K812" + グリフ数 (uint16)
//	グリフごとに 16 バイト（コードポイント順）:
//	  rune (3 バイト) + 送り幅 (1 バイト) + 12 行ぶんのビットマップ (各 1 バイト、MSB が左端)
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	cellH  = 12
	ascent = 10 // k8x12 の FONT_ASCENT
)

type glyph struct {
	r     rune
	width byte
	rows  [cellH]byte
}

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage: %s in.bdf out.bin", os.Args[0])
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	var (
		glyphs         []glyph
		g              glyph
		enc            = -1
		bw, bh, bx, by int
		inBitmap       bool
		row            int
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case fields[0] == "STARTCHAR":
			g, enc, inBitmap, row = glyph{}, -1, false, 0
		case fields[0] == "ENCODING":
			enc, _ = strconv.Atoi(fields[1])
		case fields[0] == "DWIDTH":
			w, _ := strconv.Atoi(fields[1])
			g.width = byte(w)
		case fields[0] == "BBX":
			bw, _ = strconv.Atoi(fields[1])
			bh, _ = strconv.Atoi(fields[2])
			bx, _ = strconv.Atoi(fields[3])
			by, _ = strconv.Atoi(fields[4])
			if bw+bx > 8 || bh > cellH {
				log.Fatalf("glyph %d: bbx %v too large", enc, fields[1:])
			}
		case fields[0] == "BITMAP":
			inBitmap = true
		case fields[0] == "ENDCHAR":
			if enc >= 0 && enc <= 0xFFFFFF {
				g.r = rune(enc)
				glyphs = append(glyphs, g)
			}
			inBitmap = false
		case inBitmap:
			v, err := strconv.ParseUint(fields[0], 16, 8)
			if err != nil {
				log.Fatalf("glyph %d: %v", enc, err)
			}
			y := ascent - (by + bh) + row // セル上端からの行
			if y >= 0 && y < cellH {
				g.rows[y] = byte(v) >> bx
			}
			row++
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	sort.Slice(glyphs, func(i, j int) bool { return glyphs[i].r < glyphs[j].r })

	out := []byte("K812")
	out = binary.LittleEndian.AppendUint16(out, uint16(len(glyphs)))
	for _, g := range glyphs {
		out = append(out, byte(g.r), byte(g.r>>8), byte(g.r>>16), g.width)
		out = append(out, g.rows[:]...)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d glyphs, %d bytes\n", len(glyphs), len(out))
}
