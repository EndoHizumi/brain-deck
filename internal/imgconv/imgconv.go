// Package imgconv は、画像を Brain の画面の形（RGB565）にする変換。
// 設定 GUI（gui/src/image.ts）と brain-deck（Go）で、同じ元の画像（RGBA）と同じ設定から、画素単位で同じ結果を作る。
// そのため、計算はすべて整数（固定小数点）で行い、浮動小数点を使わない。手順：
//
//  1. Shrink：長い辺が SourceMaxSide を超える画像は、整数の倍率 k で k×k の画素を平均して縮める
//  2. FitCrop：出す大きさと同じ縦横比の範囲を、中心と拡大率（1000 分率）で決める（整数の画素）
//  3. Resample：範囲を出す大きさにする。出力の画素が覆う元の画素の面積で重みを付けて平均する（拡大でも同じ式）。
//     透明な部分は黒の上に重ねる。結果は 1/16 の単位（0〜4080）
//  4. Adjust：明るさ（-100〜100 の百分率）。負なら黒に、正なら白に近づける
//  5. To565：RGB565 に丸める。Floyd–Steinberg のディザリングでは、誤差を 1/16 の単位の整数で右と下に分ける
package imgconv

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	SourceMaxSide = 2048 // 読み込んだ画像の長い辺をここまで縮めてから扱う
	Unit          = 16   // Resample の結果の単位（1/16）
	Full          = 255 * Unit
	MaxZoomMilli  = 8000

	Magic  = "LHI1"
	Header = 8
)

// RGBA は 8 ビットの RGBA（乗算済みでない）。Pix は W*H*4 バイト。
type RGBA struct {
	W, H int
	Pix  []byte
}

// Crop は元の画像の中の範囲（整数の画素）。
type Crop struct{ X, Y, W, H int }

// Params は変換の設定。
type Params struct {
	CX, CY     int  // 切り抜きの中心（元の画像の座標。縮めるときは倍率で割る）。負なら中央
	ZoomMilli  int  // 拡大率の 1000 倍（1000 で、縦横比を保ってはみ出さない最大の範囲）。0 は 1000
	Brightness int  // -100〜100
	Dither     bool // Floyd–Steinberg
}

// Shrink は、長い辺が SourceMaxSide を超えるとき、整数の倍率で縮める。超えなければそのまま返す。
func Shrink(src RGBA) RGBA {
	k := ShrinkFactor(src.W, src.H)
	if k <= 1 {
		return src
	}
	w, h := max(src.W/k, 1), max(src.H/k, 1)
	out := RGBA{W: w, H: h, Pix: make([]byte, w*h*4)}
	n := k * k
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var s [4]int
			for dy := 0; dy < k; dy++ {
				o := ((y*k+dy)*src.W + x*k) * 4
				for dx := 0; dx < k; dx++ {
					for c := 0; c < 4; c++ {
						s[c] += int(src.Pix[o+dx*4+c])
					}
				}
			}
			for c := 0; c < 4; c++ {
				out.Pix[(y*w+x)*4+c] = byte((s[c] + n/2) / n)
			}
		}
	}
	return out
}

// ShrinkFactor は、Shrink の倍率（1 なら縮めない）。
func ShrinkFactor(w, h int) int {
	return (max(w, h, 1) + SourceMaxSide - 1) / SourceMaxSide
}

// FitCrop は、元の画像 sw×sh から、tw×th と同じ縦横比の範囲を決める。範囲は元の画像の中に収める。
func FitCrop(sw, sh, tw, th, zoomMilli, cx, cy int) Crop {
	if zoomMilli <= 0 {
		zoomMilli = 1000
	}
	zoomMilli = min(max(zoomMilli, 1000), MaxZoomMilli)
	var bw, bh int
	if sw*th <= sh*tw { // 元の画像のほうが細い：幅いっぱい
		bw, bh = sw, sw*th/tw
	} else {
		bh, bw = sh, sh*tw/th
	}
	w := min(max(bw*1000/zoomMilli, 1), sw)
	h := min(max(bh*1000/zoomMilli, 1), sh)
	if cx < 0 {
		cx = sw / 2
	}
	if cy < 0 {
		cy = sh / 2
	}
	x := min(max(cx-w/2, 0), sw-w)
	y := min(max(cy-h/2, 0), sh-h)
	return Crop{x, y, w, h}
}

// span は、出力の i 番目が覆う元の画素（範囲の中の番号）と重み（合計は n）。
type span struct {
	j0 int
	w  []int
}

// spans は、長さ n の範囲を out 個に分けたときの、出力ごとの重み。元の画素 j は [j*out, (j+1)*out)、出力 i は [i*n, (i+1)*n) を覆う。
func spans(n, out int) []span {
	sp := make([]span, out)
	for i := 0; i < out; i++ {
		a, b := i*n, (i+1)*n
		j0, j1 := a/out, (b+out-1)/out
		s := span{j0: j0}
		for j := j0; j < j1; j++ {
			if wt := min(b, (j+1)*out) - max(a, j*out); wt > 0 {
				s.w = append(s.w, wt)
			} else {
				s.w = append(s.w, 0)
			}
		}
		sp[i] = s
	}
	return sp
}

// Resample は、元の画像の crop の範囲を tw×th にして、RGB を 1/16 の単位（0〜4080）で返す。
func Resample(src RGBA, c Crop, tw, th int) []int32 {
	xs, ys := spans(c.W, tw), spans(c.H, th)
	out := make([]int32, tw*th*3)
	row := make([]int64, tw*3) // 横に縮めた 1 行（重み × 乗算済みの色）
	rowY := -1
	acc := make([]int64, tw*3)
	d := int64(c.W) * int64(c.H) * 255
	for j := 0; j < th; j++ {
		for i := range acc {
			acc[i] = 0
		}
		for k, wy := range ys[j].w {
			if wy == 0 {
				continue
			}
			sy := c.Y + ys[j].j0 + k
			if sy != rowY {
				base := sy * src.W * 4
				for i := 0; i < tw; i++ {
					var r, g, b int64
					for q, wx := range xs[i].w {
						if wx == 0 {
							continue
						}
						o := base + (c.X+xs[i].j0+q)*4
						a := int64(src.Pix[o+3]) * int64(wx)
						r += int64(src.Pix[o]) * a
						g += int64(src.Pix[o+1]) * a
						b += int64(src.Pix[o+2]) * a
					}
					row[i*3], row[i*3+1], row[i*3+2] = r, g, b
				}
				rowY = sy
			}
			for i := range acc {
				acc[i] += row[i] * int64(wy)
			}
		}
		o := j * tw * 3
		for i, s := range acc {
			out[o+i] = int32((s*Unit + d/2) / d)
		}
	}
	return out
}

// Adjust は明るさを変える（pct は -100〜100）。
func Adjust(v []int32, pct int) {
	t := int32(min(max(pct, -100), 100))
	if t == 0 {
		return
	}
	for i, x := range v {
		if t < 0 {
			v[i] = x * (100 + t) / 100
		} else {
			v[i] = x + (Full-x)*t/100
		}
	}
}

// To565 は RGB（1/16 の単位）を RGB565 にする。
func To565(v []int32, w, h int, dither bool) []uint16 {
	buf := append([]int32(nil), v...)
	out := make([]uint16, w*h)
	maxes := [3]int32{31, 63, 31}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var q [3]int32
			for c := 0; c < 3; c++ {
				i := (y*w+x)*3 + c
				m := maxes[c]
				p := min(max(buf[i], 0), Full)
				q[c] = (p*m*2 + Full) / (Full * 2)
				if !dither {
					continue
				}
				e := p - (q[c]*255/m)*Unit
				e7, e3, e5 := (e*7)>>4, (e*3)>>4, (e*5)>>4
				e1 := e - e7 - e3 - e5
				if x+1 < w {
					buf[i+3] += e7
				}
				if y+1 < h {
					d := i + w*3
					if x > 0 {
						buf[d-3] += e3
					}
					buf[d] += e5
					if x+1 < w {
						buf[d+3] += e1
					}
				}
			}
			out[y*w+x] = uint16(q[0]<<11 | q[1]<<5 | q[2])
		}
	}
	return out
}

// Convert は、元の画像を tw×th の RGB565 にする。返す範囲は元の画像の座標。
func Convert(src RGBA, tw, th int, p Params) ([]uint16, Crop) {
	k := ShrinkFactor(src.W, src.H)
	src = Shrink(src)
	cx, cy := p.CX, p.CY
	if cx >= 0 {
		cx /= k
	}
	if cy >= 0 {
		cy /= k
	}
	c := FitCrop(src.W, src.H, tw, th, p.ZoomMilli, cx, cy)
	v := Resample(src, c, tw, th)
	Adjust(v, p.Brightness)
	return To565(v, tw, th, p.Dither), Crop{c.X * k, c.Y * k, c.W * k, c.H * k}
}

// Encode は、Brain の画像ファイル（LHI1）を作る。
func Encode(w, h int, pix []uint16) []byte {
	b := make([]byte, Header+len(pix)*2)
	copy(b, Magic)
	binary.LittleEndian.PutUint16(b[4:], uint16(w))
	binary.LittleEndian.PutUint16(b[6:], uint16(h))
	for i, v := range pix {
		binary.LittleEndian.PutUint16(b[Header+i*2:], v)
	}
	return b
}

// Decode は、Brain の画像ファイルを読む。
func Decode(b []byte) (w, h int, pix []uint16, err error) {
	if len(b) < Header || string(b[:4]) != Magic {
		return 0, 0, nil, errors.New("not a lefthand image (LHI1)")
	}
	w, h = int(binary.LittleEndian.Uint16(b[4:])), int(binary.LittleEndian.Uint16(b[6:]))
	if len(b) != Header+w*h*2 {
		return 0, 0, nil, fmt.Errorf("%dx%d needs %d bytes, got %d", w, h, Header+w*h*2, len(b))
	}
	pix = make([]uint16, w*h)
	for i := range pix {
		pix[i] = binary.LittleEndian.Uint16(b[Header+i*2:])
	}
	return w, h, pix, nil
}

// Unpack は RGB565 を、Brain の画面と同じ 8 ビットの色に戻す（fb.go の unpack と同じ）。
func Unpack(v uint16) (r, g, b uint8) {
	return uint8(uint32(v>>11&31) * 255 / 31), uint8(uint32(v>>5&63) * 255 / 63), uint8(uint32(v&31) * 255 / 31)
}

// Fit565 は、RGB565 の画像 sw×sh を、縦横比を保って dw×dh の中央に縮めて置き、残りを黒にする
// （縦向きの画面に横向きの写真を出すときの上下の帯）。縮めるのは面積の平均で、ディザリングはしない。
// Brain（photo.go）と設定 GUI のプレビューで同じ結果にする。
func Fit565(src []uint16, sw, sh, dw, dh int) []uint16 {
	out := make([]uint16, dw*dh)
	var w, h int
	if sw*dh <= sh*dw { // 高さで合わせる
		h, w = dh, max(sw*dh/sh, 1)
	} else {
		w, h = dw, max(sh*dw/sw, 1)
	}
	ox, oy := (dw-w)/2, (dh-h)/2
	xs, ys := spans(sw, w), spans(sh, h)
	d := int64(sw) * int64(sh)
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			var r, g, b int64
			for k, wy := range ys[j].w {
				if wy == 0 {
					continue
				}
				base := (ys[j].j0 + k) * sw
				for q, wx := range xs[i].w {
					if wx == 0 {
						continue
					}
					p := src[base+xs[i].j0+q]
					wt := int64(wx) * int64(wy)
					r += int64(p>>11&31) * wt
					g += int64(p>>5&63) * wt
					b += int64(p&31) * wt
				}
			}
			out[(oy+j)*dw+ox+i] = uint16((r+d/2)/d)<<11 | uint16((g+d/2)/d)<<5 | uint16((b+d/2)/d)
		}
	}
	return out
}
