package main

import (
	"fmt"
	"image"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- 画面の内容 ----------

// CellView は 1 セルに描く内容。
type CellView struct {
	Mapped bool
	Layer  bool   // レイヤーを切り替えるセル
	Label  string // 大きく描く文字（改行で複数行）
	Sub    string // 下に小さく描く送信キー（Label と同じなら空）
}

// cellSpan は、セル i の画面上の範囲 [lo, hi) を返す。
// タッチ判定 cellOf（floor(v*n/size)）と同じ境界になるよう、lo は切り上げる。
func cellSpan(i, n, size int) (int, int) {
	return (i*size + n - 1) / n, ((i+1)*size + n - 1) / n
}

// Layout は画面のセル配置。
type Layout struct {
	Cols, Rows int
	W, H       int
	Cells      []CellView // row*Cols+col
	Gen        uint64     // View.Gen。古い格子への SetPressed を捨てるのに使う
	Title      string     // 隅に出すレイヤー名
	Mode       LayerMode
}

// badgeRect は、レイヤー名を出す右上の札の範囲を返す。
func (l *Layout) badgeRect() image.Rectangle {
	if l.Title == "" {
		return image.Rectangle{}
	}
	w := font.textWidth(l.Title)*badgeScale + 2*badgePad
	h := fontH*badgeScale + 2*badgePad
	return image.Rect(l.W-w, 0, l.W, h)
}

func (l *Layout) rect(col, row int) image.Rectangle {
	x0, x1 := cellSpan(col, l.Cols, l.W)
	y0, y1 := cellSpan(row, l.Rows, l.H)
	return image.Rect(x0, y0, x1, y1)
}

var (
	colBG          = RGB{0, 0, 0}
	colCell        = RGB{0x1c, 0x28, 0x38}
	colBorder      = RGB{0x8c, 0xa0, 0xbc}
	colText        = RGB{0xff, 0xff, 0xff}
	colSub         = RGB{0x96, 0xa4, 0xb4}
	colPressed     = RGB{0xff, 0xd0, 0x40}
	colPressedText = RGB{0, 0, 0}
	colPressedSub  = RGB{0x50, 0x40, 0x00}
	colEmptyBorder = RGB{0x30, 0x34, 0x3a}
	colLayerCell   = RGB{0x2a, 0x22, 0x3c} // レイヤーを切り替えるセル

	// レイヤーの入り方ごとの、枠と札の色
	modeBorder = [...]RGB{
		modeBase:    colBorder,
		modeLatched: {0x40, 0xc0, 0x70}, // 切り替えたまま（緑）
		modeTemp:    {0xff, 0x80, 0x20}, // 一時的（橙）
	}
	modeBadge = [...]RGB{
		modeBase:    {0x3a, 0x48, 0x5c},
		modeLatched: {0x2e, 0x9e, 0x5b},
		modeTemp:    {0xff, 0x80, 0x20},
	}
	modeBadgeText = [...]RGB{
		modeBase:    colText,
		modeLatched: colText,
		modeTemp:    {0, 0, 0},
	}
)

const (
	cellGap    = 4 // セル同士の隙間の半分
	textMargin = 8
	maxScale   = 6
	subScale   = 2
	badgeScale = 2
	badgePad   = 5
)

// fitScale は、行の集まりが w×h に収まる最大の倍率を返す（最小 1）。
func fitScale(lines []string, w, h int) int {
	tw := 0
	for _, s := range lines {
		tw = max(tw, font.textWidth(s))
	}
	th := len(lines) * fontH
	s := maxScale
	for s > 1 && (tw*s > w || th*s > h) {
		s--
	}
	return s
}

// drawCell はセルを裏画面に描き、書き換えた論理矩形を返す。
func drawCell(cv *Canvas, l *Layout, col, row int, pressed bool) image.Rectangle {
	cell := l.rect(col, row)
	cv.fill(cell, colBG)
	box := cell.Inset(cellGap)
	v := l.Cells[row*l.Cols+col]
	if !v.Mapped {
		cv.frame(box, 1, colEmptyBorder)
		return cell.Union(redrawBadge(cv, l, cell))
	}
	fillC, textC, subC := colCell, colText, colSub
	if v.Layer {
		fillC = colLayerCell
	}
	if pressed {
		fillC, textC, subC = colPressed, colPressedText, colPressedSub
	}
	cv.fill(box, fillC)
	cv.frame(box, 2, modeBorder[l.Mode])

	inner := box.Inset(textMargin)
	subH := 0
	if v.Sub != "" {
		subH = fontH*subScale + 4
	}
	lines := strings.Split(v.Label, "\n")
	s := fitScale(lines, inner.Dx(), inner.Dy()-subH)
	blockH := len(lines) * fontH * s
	y := inner.Min.Y + (inner.Dy()-subH-blockH)/2
	for _, ln := range lines {
		x := inner.Min.X + (inner.Dx()-font.textWidth(ln)*s)/2
		cv.text(max(x, inner.Min.X), y, ln, s, textC, inner)
		y += fontH * s
	}
	if v.Sub != "" {
		ss := fitScale([]string{v.Sub}, inner.Dx(), fontH*subScale)
		ss = min(ss, subScale)
		x := inner.Min.X + (inner.Dx()-font.textWidth(v.Sub)*ss)/2
		cv.text(max(x, inner.Min.X), inner.Max.Y-fontH*ss, v.Sub, ss, subC, inner)
	}
	return cell.Union(redrawBadge(cv, l, cell))
}

// redrawBadge は、描き直したセルが札に重なっていれば札を描き直し、その範囲を返す。
func redrawBadge(cv *Canvas, l *Layout, cell image.Rectangle) image.Rectangle {
	b := l.badgeRect()
	if !b.Overlaps(cell) {
		return image.Rectangle{}
	}
	drawBadge(cv, l)
	return b
}

// drawBadge は右上に今のレイヤー名を描く。色でレイヤーの入り方がわかる。
func drawBadge(cv *Canvas, l *Layout) {
	b := l.badgeRect()
	if b.Empty() {
		return
	}
	cv.fill(b, modeBadge[l.Mode])
	cv.text(b.Min.X+badgePad, b.Min.Y+badgePad, l.Title, badgeScale, modeBadgeText[l.Mode], b)
}

func drawAll(cv *Canvas, l *Layout, pressed []bool) {
	cv.fill(image.Rect(0, 0, cv.W, cv.H), colBG)
	for r := 0; r < l.Rows; r++ {
		for c := 0; c < l.Cols; c++ {
			drawCell(cv, l, c, r, pressed[r*l.Cols+c])
		}
	}
	drawBadge(cv, l)
}

// ---------- 描画ループ ----------

// Display は入力側から押下状態を受け取り、別の goroutine で画面を描き直す。
// 入力側の SetPressed はロックして値を書くだけで、描画を待たない。
type Display struct {
	mu      sync.Mutex
	want    []bool
	wantGen uint64  // want が対応する格子
	next    *Layout // 描き直しを待っている格子（レイヤーの切り替え）
	wake    chan struct{}

	drawMu sync.Mutex // 描画と終了処理の排他
	closed bool
	active bool // 専用 VT が表示されている
	drawn  []bool

	fb         *Framebuffer
	vt         *VT
	cv         *Canvas
	layout     *Layout // 描画側だけが触る
	layoutCols int     // 描いている格子の列数（d.mu で守る）
	vtSig      chan os.Signal
}

// StartDisplay はフレームバッファと専用 VT を開き、描画 goroutine を起動する。
func StartDisplay(dc *DisplayConfig, l *Layout) (*Display, error) {
	fb, err := OpenFramebuffer(dc.Device)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dc.Device, err)
	}
	log.Printf("display: %s", fb.Info)
	cv := NewCanvas(fb.Info.XRes, fb.Info.YRes, fb.Info.XRes*fb.Info.Format.Bpp, fb.Info.Format, dc.Rotate)
	l.W, l.H = cv.W, cv.H

	d := &Display{
		want: make([]bool, len(l.Cells)), wantGen: l.Gen, drawn: make([]bool, len(l.Cells)),
		wake: make(chan struct{}, 1), fb: fb, cv: cv, layout: l, layoutCols: l.Cols,
		vtSig: make(chan os.Signal, 4),
	}
	// VT_PROCESS のシグナルは VT を設定する前に受けられるようにしておく
	signal.Notify(d.vtSig, syscall.SIGUSR1, syscall.SIGUSR2)
	vt, err := OpenVT(dc.VT, syscall.SIGUSR1, syscall.SIGUSR2)
	if err != nil {
		signal.Stop(d.vtSig)
		fb.Close()
		return nil, fmt.Errorf("vt: %w", err)
	}
	d.vt = vt

	t := time.Now()
	drawAll(cv, l, d.drawn)
	vlogf("display: initial render %v", time.Since(t))
	if err := vt.Activate(); err != nil {
		log.Printf("display: %v", err)
	}
	log.Printf("display: tty%d (was tty%d), %dx%d, rotate %d", vt.num, vt.orig, cv.W, cv.H, dc.Rotate)
	d.drawMu.Lock()
	d.acquireLocked()
	d.drawMu.Unlock()
	go d.loop()
	return d, nil
}

// SetPressed はセルの押下状態を変える。描画は待たない。d が nil でもよい。
// gen が今の格子と違う（押したあとにレイヤーが変わった）ときは何もしない。
func (d *Display) SetPressed(gen uint64, col, row int, on bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if gen == d.wantGen && col >= 0 && row >= 0 {
		if i := row*d.cols() + col; i < len(d.want) {
			d.want[i] = on
		}
	}
	d.mu.Unlock()
	d.poke()
}

// SetLayout は格子を差し替える（レイヤーの切り替え）。描画は待たない。
func (d *Display) SetLayout(l *Layout) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.next = l
	d.wantGen = l.Gen
	d.want = make([]bool, len(l.Cells))
	d.mu.Unlock()
	d.poke()
}

// cols は want が対応する格子の列数。d.mu を持って呼ぶ
func (d *Display) cols() int {
	if d.next != nil {
		return d.next.Cols
	}
	return d.layoutCols
}

func (d *Display) poke() {
	select {
	case d.wake <- struct{}{}:
	default: // すでに起こしてある
	}
}

// acquireLocked は専用 VT が表示されたときに、画面全体を描き直す。
func (d *Display) acquireLocked() {
	if d.closed || !d.vt.IsActive() || d.active {
		return // すでに表示中なら描き直さない（起動直後の acquire シグナルなど）
	}
	{
		if err := d.fb.Unblank(); err != nil {
			vlogf("display: unblank: %v", err)
		}
	}
	d.active = true
	t := time.Now()
	d.fb.Blit(d.cv, image.Rect(0, 0, d.cv.pw, d.cv.ph))
	vlogf("display: full blit %v", time.Since(t))
}

func (d *Display) loop() {
	// 描画用のスレッドの優先度を下げ、入力の goroutine を先に走らせる
	runtime.LockOSThread()
	syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), 10)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("display: panic: %v (input keeps running)", r)
			d.Close()
		}
	}()
	for {
		var ok bool
		select {
		case sig := <-d.vtSig:
			ok = d.handleVTSignal(sig)
		case <-d.wake:
			ok = d.redraw()
		}
		if !ok {
			return
		}
	}
}

func (d *Display) handleVTSignal(sig os.Signal) bool {
	d.drawMu.Lock()
	defer d.drawMu.Unlock()
	if d.closed {
		return false
	}
	if sig == syscall.SIGUSR1 {
		// ほかの VT への切り替え要求。描画を止めてから許可する
		// デーモンの動作中はキーボードを専有しているので、コンソールを見せても使えない。
		// ly の再起動などで切り替えられたときは、少し待ってから取り戻す
		log.Printf("display: another process switched the VT away, reclaiming in %v", vtReclaimDelay)
		d.active = false
		d.vt.Release()
		time.AfterFunc(vtReclaimDelay, d.reclaim)
	} else {
		vlogf("display: VT acquire")
		d.vt.AckAcquire()
		d.acquireLocked()
	}
	return true
}

const vtReclaimDelay = 2 * time.Second

// reclaim は専用 VT に切り替え直す。表示されたら acquire シグナルで全体を描き直す。
func (d *Display) reclaim() {
	d.drawMu.Lock()
	defer d.drawMu.Unlock()
	if d.closed || d.active {
		return
	}
	if err := ioctl(d.vt.tty0, ioctlVT_ACTIVATE, uintptr(d.vt.num)); err != nil {
		log.Printf("display: reclaim VT %d: %v", d.vt.num, err)
	}
}

// redraw は、格子が変わっていれば全体を、そうでなければ押下状態が変わったセルだけを描き直す。
func (d *Display) redraw() bool {
	d.mu.Lock()
	next := d.next
	d.next = nil
	if next != nil {
		d.layoutCols = next.Cols
	}
	want := append([]bool(nil), d.want...)
	d.mu.Unlock()
	d.drawMu.Lock()
	defer d.drawMu.Unlock()
	if d.closed {
		return false
	}
	if next != nil {
		t := time.Now()
		next.W, next.H = d.cv.W, d.cv.H
		d.layout = next
		d.drawn = make([]bool, len(next.Cells))
		drawAll(d.cv, next, d.drawn)
		if d.active {
			d.fb.Blit(d.cv, image.Rect(0, 0, d.cv.pw, d.cv.ph))
		}
		vlogf("display: layer %q redraw %v", next.Title, time.Since(t))
	}
	for i := range want {
		if want[i] == d.drawn[i] {
			continue
		}
		t := time.Now()
		col, row := i%d.layout.Cols, i/d.layout.Cols
		r := drawCell(d.cv, d.layout, col, row, want[i])
		d.drawn[i] = want[i]
		if d.active {
			d.fb.Blit(d.cv, d.cv.physRect(r))
		}
		vlogf("display: cell %d,%d pressed=%v redraw %v", col, row, want[i], time.Since(t))
	}
	return true
}

// Close は描画を止め、元の VT とテキストモードに戻す。何度呼んでもよい。
func (d *Display) Close() {
	if d == nil {
		return
	}
	d.drawMu.Lock()
	defer d.drawMu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	d.vt.Close()
	signal.Stop(d.vtSig)
	d.fb.Close()
	log.Printf("display: console restored to tty%d", d.vt.orig)
}
