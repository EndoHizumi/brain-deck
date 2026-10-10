package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"

	_ "golang.org/x/image/webp"

	"lefthand/internal/imgconv"
)

// ---------- 画像ファイルの読み込み（photo add、files の画像） ----------
//
// PNG、JPEG、GIF、WebP を読み、8 ビットの RGBA（乗算済みでない）にする。JPEG の EXIF の向き（Orientation）は、
// 設定 GUI（ブラウザの imageOrientation: from-image）と同じく直す。
// 設定 GUI と画素単位で同じになるのは、PNG のように、デコーダによらず同じ画素になる形だけ
// （JPEG は、ブラウザと Go のデコーダで、色の戻し方がわずかに違う）。

// decodeImageFile は、画像ファイルを読む。
func decodeImageFile(path string) (imgconv.RGBA, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return imgconv.RGBA{}, err
	}
	return decodeImageBytes(b)
}

func decodeImageBytes(b []byte) (imgconv.RGBA, error) {
	img, format, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return imgconv.RGBA{}, fmt.Errorf("読めない画像です（PNG、JPEG、GIF、WebP に対応）: %w", err)
	}
	r := img.Bounds()
	n := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(n, n.Bounds(), img, r.Min, draw.Src)
	out := imgconv.RGBA{W: r.Dx(), H: r.Dy(), Pix: n.Pix}
	if format == "jpeg" {
		out = orient(out, exifOrientation(b))
	}
	return out, nil
}

// exifOrientation は、JPEG の EXIF の Orientation（1〜8。なければ 1）。
func exifOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) && b[i] == 0xff {
		marker := b[i+1]
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xda || size < 2 || i+2+size > len(b) {
			break
		}
		seg := b[i+4 : i+2+size]
		if marker == 0xe1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			v := int(bo.Uint16(t[e+8:]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient は、EXIF の向き o の画像を、正しい向きにする。
func orient(src imgconv.RGBA, o int) imgconv.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	w, h := src.W, src.H
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	out := imgconv.RGBA{W: dw, H: dh, Pix: make([]byte, len(src.Pix))}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var X, Y int
			switch o {
			case 2:
				X, Y = w-1-x, y
			case 3:
				X, Y = w-1-x, h-1-y
			case 4:
				X, Y = x, h-1-y
			case 5:
				X, Y = y, x
			case 6:
				X, Y = h-1-y, x
			case 7:
				X, Y = h-1-y, w-1-x
			case 8:
				X, Y = y, w-1-x
			}
			copy(out.Pix[(Y*dw+X)*4:(Y*dw+X)*4+4], src.Pix[(y*w+x)*4:(y*w+x)*4+4])
		}
	}
	return out
}
