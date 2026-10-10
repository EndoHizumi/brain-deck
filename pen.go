package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- ペンタブレット（widget: pentablet） ----------
//
// セルの中をスタイラスでなぞると、PC には HID のデジタイザのペン（絶対座標、ペン先が触れているか、押す強さ）として届く。
// USB のエンドポイントに空きがないので、ペンはマウスと同じ HID（hid.go の hidDescCombo、レポート ID 4）に入れた。
// そのため、**キーボードとマウスの形（usb_mode: mouse）のときだけ使える**。キーボードだけの形では、セルに
// 「マウスありの形で使えます」と出し、何も送らない。PC から外しているあいだは休み（offline.go）。
//
// 抵抗膜はペンを浮かせた位置（ホバー）を読めないので、触れているあいだだけ In Range と Tip Switch を立て、
// 離したら両方を落とす。座標はセルの範囲を、PC の画面の map の範囲（全体、縦横比を保つ、一部）に当てはめて、
// 0〜32767 で送る（PC の OS は、デジタイザの全体を画面全体に当てはめる）。押す強さ（ABS_PRESSURE）は、
// pressure_range の範囲を 0〜1023 にして送る。pressure: false なら、触れているあいだ 1023 を送る。

const (
	widgetPenTab = "pentablet"

	penLogicalMax  = 32767
	penPressureMax = 1023

	penMapFull   = "full"   // セル全体を PC の画面全体に（縦横比は変わる）
	penMapAspect = "aspect" // セルと同じ縦横比の、PC の画面の中のいちばん大きな範囲に（中央）
	penMapRegion = "region" // PC の画面の region の範囲に
)

// PenTabDef は、組み立て済みのペンタブレットのセル。
type PenTabDef struct {
	Map      string
	Screen   image.Point     // PC の画面の大きさ（aspect、region で使う）
	Region   image.Rectangle // region の範囲（PC の画面の画素）
	Pressure bool
	Pen      PenParams
}

// compilePenTab は、ペンタブレットのセルの書き方を検証して組み立てる。
func compilePenTab(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: pentablet handles the stylus itself; remove key, layer_* and mouse")
	}
	if s.Speed != nil || s.Accel != nil || s.ScrollWidth != nil || s.ScrollDirection != "" || s.ScrollStep != nil || s.Deadzone != nil ||
		s.TapMS != nil || s.TapMove != nil || s.DragMS != nil || s.LongPress != "" {
		return nil, fmt.Errorf("speed, accel, scroll_*, deadzone, tap_*, drag_ms and long_press are for widget: trackpad (widget: pentablet takes map, pc_screen, region, pressure, pressure_range, min_pressure, settle_ms and smooth)")
	}
	pen, err := compilePenParams(s)
	if err != nil {
		return nil, err
	}
	d := &PenTabDef{Map: penMapFull, Pressure: true, Pen: pen}
	if s.Pressure != nil {
		d.Pressure = *s.Pressure
	}
	switch s.Map {
	case "", penMapFull:
	case penMapAspect, penMapRegion:
		d.Map = s.Map
	default:
		return nil, fmt.Errorf("map must be %s, %s or %s, got %q", penMapFull, penMapAspect, penMapRegion, s.Map)
	}
	if s.PCScreen != "" {
		var w, h int
		if _, err := fmt.Sscanf(s.PCScreen, "%dx%d", &w, &h); err != nil || w < 1 || h < 1 || w > 32767 || h > 32767 {
			return nil, fmt.Errorf("pc_screen must be WIDTHxHEIGHT in pixels (such as 1920x1080), got %q", s.PCScreen)
		}
		d.Screen = image.Pt(w, h)
	}
	if d.Map != penMapFull && d.Screen == (image.Point{}) {
		return nil, fmt.Errorf("map: %s needs pc_screen (the size of the PC screen, such as 1920x1080)", d.Map)
	}
	if s.Region != nil {
		if d.Map != penMapRegion {
			return nil, fmt.Errorf("region is for map: region")
		}
		r := image.Rect(s.Region[0], s.Region[1], s.Region[0]+s.Region[2], s.Region[1]+s.Region[3])
		if s.Region[2] < 1 || s.Region[3] < 1 || !r.In(image.Rect(0, 0, d.Screen.X, d.Screen.Y)) {
			return nil, fmt.Errorf("region must be [x, y, width, height] inside pc_screen %dx%d", d.Screen.X, d.Screen.Y)
		}
		d.Region = r
	} else if d.Map == penMapRegion {
		return nil, fmt.Errorf("map: region needs region ([x, y, width, height] in PC screen pixels)")
	}
	return &WidgetDef{Kind: widgetPenTab, PenTab: d}, nil
}

// target は、セル（W×H）を当てはめる、PC の画面の範囲（0〜32767 の座標）。
func (d *PenTabDef) target(cw, ch int) (x0, y0, x1, y1 float64) {
	switch d.Map {
	case penMapRegion:
		sx, sy := float64(penLogicalMax)/float64(d.Screen.X), float64(penLogicalMax)/float64(d.Screen.Y)
		return float64(d.Region.Min.X) * sx, float64(d.Region.Min.Y) * sy, float64(d.Region.Max.X) * sx, float64(d.Region.Max.Y) * sy
	case penMapAspect:
		// セルと同じ縦横比の、いちばん大きな範囲（PC の画面の画素で考える）
		W, H := float64(d.Screen.X), float64(d.Screen.Y)
		w, h := W, W*float64(ch)/float64(cw)
		if h > H {
			h, w = H, H*float64(cw)/float64(ch)
		}
		ox, oy := (W-w)/2, (H-h)/2
		sx, sy := float64(penLogicalMax)/W, float64(penLogicalMax)/H
		return ox * sx, oy * sy, (ox + w) * sx, (oy + h) * sy
	}
	return 0, 0, penLogicalMax, penLogicalMax
}

// penPoint は、セルの中の点 pt（画面のドット）を、PC に送る座標にする。
func (d *PenTabDef) penPoint(cell image.Rectangle, pt fpt) (uint16, uint16) {
	x0, y0, x1, y1 := d.target(cell.Dx(), cell.Dy())
	fx := (pt.X - float64(cell.Min.X)) / float64(max(cell.Dx()-1, 1))
	fy := (pt.Y - float64(cell.Min.Y)) / float64(max(cell.Dy()-1, 1))
	fx, fy = min(max(fx, 0), 1), min(max(fy, 0), 1)
	return uint16(x0 + fx*(x1-x0) + 0.5), uint16(y0 + fy*(y1-y0) + 0.5)
}

// penPressure は、生の押す強さを 0〜1023 にする。
func (d *PenTabDef) penPressure(p int32) uint16 {
	if !d.Pressure {
		return penPressureMax
	}
	lo, hi := d.Pen.PressureLo, d.Pen.PressureHi
	v := (int64(p-lo)*penPressureMax + int64(hi-lo)/2) / int64(hi-lo)
	return uint16(min(max(v, 1), penPressureMax)) // 触れているあいだは 0 にしない（0 だと描かないアプリがある）
}

// ---------- HID のペンのレポート ----------

// Pen は、ペンのレポート（レポート ID 4、8 バイト：ID、フラグ、X、Y、押す強さ）を送る。nil でもよい（何もしない）。
type Pen struct {
	mu    sync.Mutex
	hid   hidOut
	id    byte
	on    atomic.Bool
	last  []byte // 最後に送ろうとしたレポート
	dirty bool   // 最後のレポート（離したこと）が PC に届いていない
	down  bool
}

const (
	penTip     = 1 // Tip Switch
	penInRange = 2 // In Range
)

// NewPen は、レポート ID が id のペンを作る。id が 0（ガジェットにペンがない）なら何も送らない。
func NewPen(hid hidOut, id byte) *Pen {
	p := &Pen{hid: hid, id: id}
	p.on.Store(id != 0)
	return p
}

// Available は、PC にペンとして送れるか（キーボードとマウスの形か）。
func (p *Pen) Available() bool { return p != nil && p.on.Load() }

// Report は、ペン先が触れている（tip）位置と押す強さを送る。tip が false なら離したこと。
func (p *Pen) Report(tip bool, x, y, pressure uint16) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.id == 0 {
		return
	}
	r := make([]byte, 8)
	r[0] = p.id
	if tip {
		r[1] = penTip | penInRange
	} else {
		pressure = 0
	}
	binary.LittleEndian.PutUint16(r[2:], x)
	binary.LittleEndian.PutUint16(r[4:], y)
	binary.LittleEndian.PutUint16(r[6:], pressure)
	p.down = tip
	p.last = r
	err := p.hid.Write(r)
	// 離したのが届かないと、PC ではペンが触れたままになる。retry で送り直す（動かしているあいだのものは捨てる）
	p.dirty = err != nil && !tip
	if tip {
		hostWaker.Request()
	}
}

// retry は、届いていない「離した」レポートを送り直す（State.retryLoop から）。
func (p *Pen) retry() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dirty && p.id != 0 {
		p.dirty = p.hid.Write(p.last) != nil
	}
}

// Lift は、触れたままなら離したことを送る（終了、USB の付け直しの前、設定の再読み込み）。
func (p *Pen) Lift() {
	if p == nil {
		return
	}
	p.mu.Lock()
	down, last := p.down, p.last
	p.mu.Unlock()
	if down && len(last) == 8 {
		p.Report(false, binary.LittleEndian.Uint16(last[2:]), binary.LittleEndian.Uint16(last[4:]), 0)
	}
}

// reset は、USB を付け直したあとに、レポート ID を変える。p.mu を持って呼ぶ。
func (p *Pen) reset(id byte) {
	p.id = id
	p.on.Store(id != 0)
	p.down, p.dirty = false, false
}

// ---------- セルでの読み取り ----------

// penTabStream は、ペンタブレットのセルに触れているあいだの読み取り（タッチの goroutine だけが使う）。
type penTabStream struct {
	d    *PenTabDef
	cell image.Rectangle
	pen  *Pen
	f    *StrokeFilter
	lx   uint16
	ly   uint16
}

func newPenTabStream(d *PenTabDef, cell image.Rectangle, tm touchMap, pen *Pen, t time.Time) *penTabStream {
	s := &penTabStream{d: d, cell: cell, pen: pen}
	s.f = NewStrokeFilter(d.Pen, tm, s)
	s.f.p.Join = 0 // PC のアプリが線を続けるかは PC が決める（Brain では待たない）
	s.f.Down(t)
	return s
}

func (s *penTabStream) Sample(x, y, p int32, t time.Time) { s.f.Sample(x, y, p, t) }
func (s *penTabStream) Up(t time.Time)                    { s.f.Up(t) }

func (s *penTabStream) StrokeDown(pt fpt, pr int32) { s.StrokeMove(pt, pr) }
func (s *penTabStream) StrokeMove(pt fpt, pr int32) {
	s.lx, s.ly = s.d.penPoint(s.cell, pt)
	s.pen.Report(true, s.lx, s.ly, s.d.penPressure(pr))
}
func (s *penTabStream) StrokeUp() { s.pen.Report(false, s.lx, s.ly, 0) }

// ---------- 描画 ----------

var (
	colPenFrame = RGB{0x5a, 0x6a, 0x80}
	colPenHint  = RGB{0x96, 0xa4, 0xb4}
	colPenOff   = RGB{0xff, 0x80, 0x20}
)

// penTabLines は、セルに出す説明。
func penTabLines(d *PenTabDef, env WidgetEnv) ([]string, RGB) {
	if env.MouseOff {
		return []string{"ペンタブレット", "マウスありの形で使えます", "（usb_mode: mouse）"}, colPenOff
	}
	m := map[string]string{penMapFull: "PC の画面全体", penMapAspect: "PC の画面（縦横比を保つ）", penMapRegion: "PC の画面の一部"}[d.Map]
	p := "筆圧あり"
	if !d.Pressure {
		p = "筆圧なし"
	}
	return []string{"ペンタブレット", m, p}, colPenHint
}

// drawPenTab は、ペンタブレットのセルを描く（なぞる範囲の枠と、説明）。
func drawPenTab(cv *Canvas, area image.Rectangle, d *PenTabDef, env WidgetEnv) {
	cv.frame(area, 1, colPenFrame)
	lines, c := penTabLines(d, env)
	s := min(fitScaleMax(lines, area.Dx()-16, area.Dy()/2, 2), 2)
	y := area.Min.Y + (area.Dy()-len(lines)*fontH*s)/2
	for _, ln := range lines {
		drawCentered(cv, area, y, ln, s, c)
		y += fontH * s
	}
}

func penTabKey(d *PenTabDef, env WidgetEnv) string {
	l, _ := penTabLines(d, env)
	return "pentablet\x00" + strings.Join(l, "\x00")
}
