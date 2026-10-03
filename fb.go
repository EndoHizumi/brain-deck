package main

import (
	"fmt"
	"image"
	"image/color"
	"syscall"
	"unsafe"
)

// ---------- フレームバッファ ----------

const (
	ioctlFBIOGET_VSCREENINFO = 0x4600
	ioctlFBIOGET_FSCREENINFO = 0x4602
	ioctlFBIOBLANK           = 0x4611
	fbBlankUnblank           = 0
)

type bitfield struct{ offset, length uint32 }

// PixelFormat は fb_var_screeninfo の色の並び。
type PixelFormat struct {
	Bpp     int // 1 ピクセルのバイト数
	R, G, B bitfield
}

var rgb565 = PixelFormat{Bpp: 2, R: bitfield{11, 5}, G: bitfield{5, 6}, B: bitfield{0, 5}}

type RGB struct{ R, G, B uint8 }

func (pf PixelFormat) pack(c RGB) uint32 {
	ch := func(v uint8, f bitfield) uint32 {
		if f.length == 0 {
			return 0
		}
		return uint32(v) >> (8 - min(f.length, 8)) << f.offset
	}
	return ch(c.R, pf.R) | ch(c.G, pf.G) | ch(c.B, pf.B)
}

func (pf PixelFormat) unpack(v uint32) color.RGBA {
	ch := func(f bitfield) uint8 {
		if f.length == 0 {
			return 0
		}
		x := (v >> f.offset) & (1<<f.length - 1)
		return uint8(x * 255 / (1<<f.length - 1))
	}
	return color.RGBA{ch(pf.R), ch(pf.G), ch(pf.B), 0xff}
}

// FBInfo は /dev/fb0 の ioctl で得た仕様。
type FBInfo struct {
	ID               string
	XRes, YRes       int
	XOffset, YOffset int
	BitsPerPixel     int
	LineLength       int
	SmemLen          int
	Visual           int
	Format           PixelFormat
}

func (i FBInfo) String() string {
	return fmt.Sprintf("%s %dx%d %dbpp stride=%d R%d/%d G%d/%d B%d/%d visual=%d mem=%d",
		i.ID, i.XRes, i.YRes, i.BitsPerPixel, i.LineLength,
		i.Format.R.offset, i.Format.R.length, i.Format.G.offset, i.Format.G.length,
		i.Format.B.offset, i.Format.B.length, i.Visual, i.SmemLen)
}

func ioctl(fd int, req uintptr, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg); e != 0 {
		return e
	}
	return nil
}

func queryFB(fd int) (FBInfo, error) {
	var info FBInfo
	var v [160]byte
	if err := ioctl(fd, ioctlFBIOGET_VSCREENINFO, uintptr(unsafe.Pointer(&v[0]))); err != nil {
		return info, fmt.Errorf("FBIOGET_VSCREENINFO: %w", err)
	}
	u32 := func(b []byte, off int) uint32 { return *(*uint32)(unsafe.Pointer(&b[off])) }
	info.XRes, info.YRes = int(u32(v[:], 0)), int(u32(v[:], 4))
	info.XOffset, info.YOffset = int(u32(v[:], 16)), int(u32(v[:], 20))
	info.BitsPerPixel = int(u32(v[:], 24))
	info.Format = PixelFormat{
		Bpp: (info.BitsPerPixel + 7) / 8,
		R:   bitfield{u32(v[:], 32), u32(v[:], 36)},
		G:   bitfield{u32(v[:], 44), u32(v[:], 48)},
		B:   bitfield{u32(v[:], 56), u32(v[:], 60)},
	}

	// fb_fix_screeninfo は unsigned long を含むので、ポインタ幅で配置が変わる
	var f [96]byte
	if err := ioctl(fd, ioctlFBIOGET_FSCREENINFO, uintptr(unsafe.Pointer(&f[0]))); err != nil {
		return info, fmt.Errorf("FBIOGET_FSCREENINFO: %w", err)
	}
	id := f[:16]
	for i, c := range id {
		if c == 0 {
			id = id[:i]
			break
		}
	}
	info.ID = string(id)
	ul := int(unsafe.Sizeof(uintptr(0)))
	o := 16 + ul // smem_len
	info.SmemLen = int(u32(f[:], o))
	info.Visual = int(u32(f[:], o+12))
	lo := o + 16 + 6 // xpanstep, ypanstep, ywrapstep (u16 x3)
	lo = (lo + 3) &^ 3
	info.LineLength = int(u32(f[:], lo))
	if info.LineLength == 0 {
		info.LineLength = info.XRes * info.Format.Bpp
	}
	if info.Format.Bpp != 2 && info.Format.Bpp != 3 && info.Format.Bpp != 4 {
		return info, fmt.Errorf("unsupported bits_per_pixel %d", info.BitsPerPixel)
	}
	return info, nil
}

// Framebuffer は mmap した /dev/fb0。
type Framebuffer struct {
	fd   int
	mem  []byte
	base int // 表示中の領域の先頭（yoffset/xoffset を反映）
	Info FBInfo
}

func OpenFramebuffer(path string) (*Framebuffer, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	info, err := queryFB(fd)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	size := info.SmemLen
	if need := (info.YOffset + info.YRes) * info.LineLength; size < need {
		size = need
	}
	mem, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("mmap: %w", err)
	}
	return &Framebuffer{
		fd: fd, mem: mem, Info: info,
		base: info.YOffset*info.LineLength + info.XOffset*info.Format.Bpp,
	}, nil
}

func (fb *Framebuffer) Unblank() error {
	return ioctl(fb.fd, ioctlFBIOBLANK, fbBlankUnblank)
}

func (fb *Framebuffer) Close() {
	syscall.Munmap(fb.mem)
	syscall.Close(fb.fd)
}

// Blit は裏画面の矩形（物理座標）をフレームバッファに写す。
func (fb *Framebuffer) Blit(cv *Canvas, r image.Rectangle) {
	r = r.Intersect(image.Rect(0, 0, cv.pw, cv.ph))
	if r.Empty() {
		return
	}
	bpp := cv.pf.Bpp
	for y := r.Min.Y; y < r.Max.Y; y++ {
		src := y*cv.stride + r.Min.X*bpp
		dst := fb.base + y*fb.Info.LineLength + r.Min.X*bpp
		copy(fb.mem[dst:dst+r.Dx()*bpp], cv.pix[src:src+r.Dx()*bpp])
	}
}

// ---------- 裏画面 ----------

// Canvas は論理座標（回転後の画面の向き）で描き、物理配置のバッファに書き込む。
type Canvas struct {
	W, H        int // 論理サイズ
	pw, ph      int // 物理サイズ
	stride, rot int
	pf          PixelFormat
	pix         []byte
}

func NewCanvas(pw, ph, stride int, pf PixelFormat, rot int) *Canvas {
	cv := &Canvas{pw: pw, ph: ph, stride: stride, pf: pf, rot: rot, pix: make([]byte, stride*ph)}
	cv.W, cv.H = pw, ph
	if rot == 90 || rot == 270 {
		cv.W, cv.H = ph, pw
	}
	return cv
}

// phys は論理座標を物理座標に変換する（rot は時計回りの回転角）。
func (cv *Canvas) phys(x, y int) (int, int) {
	switch cv.rot {
	case 90:
		return cv.pw - 1 - y, x
	case 180:
		return cv.pw - 1 - x, cv.ph - 1 - y
	case 270:
		return y, cv.ph - 1 - x
	}
	return x, y
}

func (cv *Canvas) physRect(r image.Rectangle) image.Rectangle {
	if r.Empty() {
		return r
	}
	x0, y0 := cv.phys(r.Min.X, r.Min.Y)
	x1, y1 := cv.phys(r.Max.X-1, r.Max.Y-1)
	return image.Rect(min(x0, x1), min(y0, y1), max(x0, x1)+1, max(y0, y1)+1)
}

func (cv *Canvas) fill(r image.Rectangle, c RGB) {
	r = r.Intersect(image.Rect(0, 0, cv.W, cv.H))
	if r.Empty() {
		return
	}
	v := cv.pf.pack(c)
	bpp := cv.pf.Bpp
	px := make([]byte, bpp)
	for i := range px {
		px[i] = byte(v >> (8 * i))
	}
	if cv.rot == 0 {
		// よく使う向きは行単位で埋める
		row := cv.pix[r.Min.Y*cv.stride+r.Min.X*bpp : r.Min.Y*cv.stride+r.Max.X*bpp]
		for i := 0; i < len(row); i += bpp {
			copy(row[i:], px)
		}
		for y := r.Min.Y + 1; y < r.Max.Y; y++ {
			copy(cv.pix[y*cv.stride+r.Min.X*bpp:], row)
		}
		return
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			X, Y := cv.phys(x, y)
			copy(cv.pix[Y*cv.stride+X*bpp:], px)
		}
	}
}

// frame は幅 t の枠を描く。
func (cv *Canvas) frame(r image.Rectangle, t int, c RGB) {
	cv.fill(image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+t), c)
	cv.fill(image.Rect(r.Min.X, r.Max.Y-t, r.Max.X, r.Max.Y), c)
	cv.fill(image.Rect(r.Min.X, r.Min.Y+t, r.Min.X+t, r.Max.Y-t), c)
	cv.fill(image.Rect(r.Max.X-t, r.Min.Y+t, r.Max.X, r.Max.Y-t), c)
}

// text は (x, y) を左上として、scale 倍で文字列を描く。範囲外ははみ出さない。
func (cv *Canvas) text(x, y int, s string, scale int, c RGB, clip image.Rectangle) {
	for _, r := range s {
		w, rows := font.glyphOrBox(r)
		for gy, bits := range rows {
			if bits == 0 {
				continue
			}
			for gx := 0; gx < 8; gx++ {
				if bits&(0x80>>gx) == 0 {
					continue
				}
				px := image.Rect(x+gx*scale, y+gy*scale, x+(gx+1)*scale, y+(gy+1)*scale)
				cv.fill(px.Intersect(clip), c)
			}
		}
		x += w * scale
	}
}

func (cv *Canvas) Image() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, cv.W, cv.H))
	bpp := cv.pf.Bpp
	for y := 0; y < cv.H; y++ {
		for x := 0; x < cv.W; x++ {
			X, Y := cv.phys(x, y)
			o := Y*cv.stride + X*bpp
			var v uint32
			for i := 0; i < bpp; i++ {
				v |= uint32(cv.pix[o+i]) << (8 * i)
			}
			img.SetRGBA(x, y, cv.pf.unpack(v))
		}
	}
	return img
}
