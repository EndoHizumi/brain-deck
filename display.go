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
	Label  string // 大きく描く文字（改行で複数行）。ウィジェットでは上に小さく出す見出し
	Sub    string // 下に小さく描く送信キー（Label と同じなら空）
	// SpanW、SpanH はセルの大きさ（列数、行数）。0 は 1 とみなす
	SpanW, SpanH int
	Covered      bool       // 左上以外の、span のセルに覆われた位置。描かない
	Widget       *WidgetDef // ウィジェットのセル
	Background   string     // セルの背景画像の id。空ならなし
}

// ImageSource は背景画像を返す（*ImageStore）。ない画像は nil。
type ImageSource interface {
	Image(id string) *Image
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
	Press      string    // 押したときの見せ方。pressFill 以外は枠を光らせる
	Env        WidgetEnv // ウィジェットを描くときの状態。描く側が描く前に入れる
	Wallpaper  string    // 格子全体に敷く壁紙の id。空ならなし
	// Images は背景画像の読み込み先。描く側が描く前に入れる。nil なら背景画像を描かない
	Images ImageSource
}

// image は id の背景画像を返す。id が空、画像がないときは nil。
func (l *Layout) image(id string) *Image {
	if id == "" || l.Images == nil {
		return nil
	}
	return l.Images.Image(id)
}

// placed は、画面に置いた画像。
type placed struct {
	img *Image
	dst image.Rectangle
}

// centered は、画像を area の中央に置く。大きさが合わなければ、はみ出す分は切れ、足りない分は下の色が見える。
// gui/src/preview.ts の centered と同じ（負の数の割り算は 0 に向けて切り捨てる）。
func centered(img *Image, area image.Rectangle) placed {
	if img == nil {
		return placed{}
	}
	x := area.Min.X + (area.Dx()-img.W)/2
	y := area.Min.Y + (area.Dy()-img.H)/2
	return placed{img, image.Rect(x, y, x+img.W, y+img.H)}
}

// paintBack は r を、base の色の上に imgs を順に重ねた背景で塗る。
// 後の画像が r を覆いきるときは、その下は描かない。
func paintBack(cv *Canvas, r image.Rectangle, base RGB, imgs ...placed) {
	first := 0
	for i, p := range imgs {
		if p.img != nil && r.In(p.dst) {
			first = i
		}
	}
	if first == 0 && (imgs[0].img == nil || !r.In(imgs[0].dst)) {
		cv.fill(r, base)
	}
	for _, p := range imgs[first:] {
		if p.img != nil {
			cv.blitImage(p.img, p.dst, r)
		}
	}
}

// cellBack は、セル v の箱の中の背景（壁紙、その上にセルの背景画像）と、画像があるかどうかを返す。
func (l *Layout) cellBack(v *CellView, box image.Rectangle) (wall, own placed, has bool) {
	wall = centered(l.image(l.Wallpaper), image.Rect(0, 0, l.W, l.H))
	own = centered(l.image(v.Background), box)
	return wall, own, wall.img != nil || own.img != nil
}

func (l *Layout) pressFill() bool { return l.Press == pressFill }

func pressName(l *Layout) string {
	if l.pressFill() {
		return pressFill
	}
	return pressBorder
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

// rect は、セルの画面上の範囲を返す。span のセルは、覆う範囲全体になる。
func (l *Layout) rect(col, row int) image.Rectangle {
	w, h := 1, 1
	if i := row*l.Cols + col; i < len(l.Cells) {
		w, h = l.Cells[i].SpanW, l.Cells[i].SpanH
	}
	return cellRect(col, row, w, h, l.Cols, l.Rows, l.W, l.H)
}

// cellRect は、cols×rows の格子で (col, row) から spanW×spanH のセルの、W×H の画面上の範囲を返す。
func cellRect(col, row, spanW, spanH, cols, rows, W, H int) image.Rectangle {
	x0, _ := cellSpan(col, cols, W)
	_, x1 := cellSpan(col+max(spanW, 1)-1, cols, W)
	y0, _ := cellSpan(row, rows, H)
	_, y1 := cellSpan(row+max(spanH, 1)-1, rows, H)
	return image.Rect(x0, y0, x1, y1)
}

var (
	colBG          = RGB{0, 0, 0}
	colCell        = RGB{0x1c, 0x28, 0x38}
	colBorder      = RGB{0x8c, 0xa0, 0xbc}
	colText        = RGB{0xff, 0xff, 0xff}
	colSub         = RGB{0x96, 0xa4, 0xb4}
	colPressed     = RGB{0xff, 0xd0, 0x40} // 押したときの色（fill の塗り、border の明るい線）
	colPressedText = RGB{0, 0, 0}
	colPressedSub  = RGB{0x50, 0x40, 0x00}
	colPressEdge   = RGB{0, 0, 0} // border の外側の暗い線
	colEmptyBorder = RGB{0x30, 0x34, 0x3a}
	colHalo        = RGB{0, 0, 0}          // 背景画像の上の文字の縁取り
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

	borderW = 2 // ふだんの枠の太さ
	// press_style: border で押したときの二重の枠。セルの内側に描く。
	// 外側の暗い線で、明るい背景や画像の上でも縁が分かる。
	// 合わせて textMargin より細くし、ラベルには重ならないようにする
	pressEdgeW = 2 // 外側の暗い線
	pressGlowW = 5 // 内側の明るい線
	pressRingW = pressEdgeW + pressGlowW
)

// fitScale は、行の集まりが w×h に収まる最大の倍率を返す（最小 1）。
func fitScale(lines []string, w, h int) int { return fitScaleMax(lines, w, h, maxScale) }

// fitScaleMax は、倍率の上限を指定する fitScale。
func fitScaleMax(lines []string, w, h, most int) int {
	tw := 0
	for _, s := range lines {
		tw = max(tw, font.textWidth(s))
	}
	th := len(lines) * fontH
	s := most
	for s > 1 && (tw*s > w || th*s > h) {
		s--
	}
	return s
}

// drawCell はセルを裏画面に描き、書き換えた論理矩形を返す。
//
// 背景は、下から 黒（または壁紙）→ セルの塗り（壁紙があれば壁紙のまま）→ セルの背景画像 の順に重ねる。
// 背景画像（壁紙かセルの画像）があるセルでは、文字に 1 ドットの縁取り（colHalo）を付ける。
// fill で押しているあいだは、今までどおり黄色で塗りつぶす（画像は隠れる）。
func drawCell(cv *Canvas, l *Layout, col, row int, pressed bool) image.Rectangle {
	cell := l.rect(col, row)
	box := cell.Inset(cellGap)
	v := l.Cells[row*l.Cols+col]
	wall, own, hasImg := l.cellBack(&v, box)
	paintBack(cv, cell, colBG, wall)
	if !v.Mapped {
		cv.frame(box, 1, colEmptyBorder)
		return cell.Union(redrawBadge(cv, l, cell))
	}
	fillC, textC, subC := cellFill(v), colText, colSub
	if pressed && l.pressFill() {
		fillC, textC, subC = colPressed, colPressedText, colPressedSub
		cv.fill(box, fillC)
		hasImg = false
	} else {
		paintBack(cv, box, fillC, wall, own)
	}
	cv.frame(box, borderW, modeBorder[l.Mode])
	if pressed && !l.pressFill() {
		drawRing(cv, l, &v, box, true)
	}

	inner := box.Inset(textMargin)
	if hasImg {
		cv.halo = &colHalo
		defer func() { cv.halo = nil }()
	}
	if v.Widget != nil {
		drawWidget(cv, inner, &v, l.Env, textC, subC)
		cv.halo = nil
		return cell.Union(redrawBadge(cv, l, cell))
	}
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
	cv.halo = nil
	return cell.Union(redrawBadge(cv, l, cell))
}

// cellFill は割り当てのあるセルの、ふだんの塗りの色（背景画像がないとき）。
func cellFill(v CellView) RGB {
	if v.Layer {
		return colLayerCell
	}
	return colCell
}

// drawRing は、box の内側 pressRingW の帯を描く。
// on なら押したときの二重の枠、そうでなければふだんの枠と、その内側の背景（塗りか画像）に戻す。
func drawRing(cv *Canvas, l *Layout, v *CellView, box image.Rectangle, on bool) {
	if on {
		cv.frame(box, pressEdgeW, colPressEdge)
		cv.frame(box.Inset(pressEdgeW), pressGlowW, colPressed)
		return
	}
	cv.frame(box, borderW, modeBorder[l.Mode])
	wall, own, _ := l.cellBack(v, box)
	for _, r := range ringRects(box.Inset(borderW), pressRingW-borderW) {
		paintBack(cv, r, cellFill(*v), wall, own)
	}
}

// ringRects は、box の内側 t の帯を、重ならない 4 つの矩形で返す。
func ringRects(box image.Rectangle, t int) []image.Rectangle {
	return []image.Rectangle{
		image.Rect(box.Min.X, box.Min.Y, box.Max.X, box.Min.Y+t),
		image.Rect(box.Min.X, box.Max.Y-t, box.Max.X, box.Max.Y),
		image.Rect(box.Min.X, box.Min.Y+t, box.Min.X+t, box.Max.Y-t),
		image.Rect(box.Max.X-t, box.Min.Y+t, box.Max.X, box.Max.Y-t),
	}
}

// drawPress は press_style: border で、押したとき・離したときに枠の帯だけを描き直し、
// 書き換えた論理矩形を返す。ラベルは帯の内側にあるので描き直さない。
// 札に重なれば札も描き直す。割り当てのないセルは押しても変わらない。
func drawPress(cv *Canvas, l *Layout, col, row int, on bool) []image.Rectangle {
	v := l.Cells[row*l.Cols+col]
	if !v.Mapped {
		return nil
	}
	box := l.rect(col, row).Inset(cellGap)
	drawRing(cv, l, &v, box, on)
	rs := ringRects(box, pressRingW)
	if b := redrawBadge(cv, l, box); !b.Empty() {
		rs = append(rs, b)
	}
	return rs
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
			if !l.Cells[r*l.Cols+c].Covered {
				drawCell(cv, l, c, r, pressed[r*l.Cols+c])
			}
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

	images  ImageSource // 背景画像。nil なら描かない
	invalid bool        // 画面全体を描き直す（画像が増えた・減った）。d.mu で守る

	// ウィジェット（描画の goroutine だけが触る）
	env    func() WidgetEnv // 今の時刻など。nil なら時刻だけ
	wkeys  []string         // セルごとに、前に描いたウィジェットの中身（widgetKey）
	wakeAt time.Time        // 次にウィジェットの中身が変わる時刻。ゼロなら起きなくてよい
}

// StartDisplay はフレームバッファと専用 VT を開き、描画 goroutine を起動する。
// env は、ウィジェットを描くときの状態を返す関数（nil なら時刻だけ使う）。
// images は背景画像の読み込み先（nil なら背景画像を描かない）。
func StartDisplay(dc *DisplayConfig, l *Layout, env func() WidgetEnv, images ImageSource) (*Display, error) {
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
		vtSig: make(chan os.Signal, 4), env: env, images: images,
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
	d.drawAllLocked(l)
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

// Invalidate は、画面全体を描き直させる（背景画像を受け取った、消したなど）。描画は待たない。
func (d *Display) Invalidate() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.invalid = true
	d.mu.Unlock()
	d.poke()
}

// Poke は、ウィジェットの中身を確かめ直させる（時刻を合わせた、データが変わったなど）。描画は待たない。
func (d *Display) Poke() {
	if d != nil {
		d.poke()
	}
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
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		timer.Reset(d.untilWake())
		var ok bool
		select {
		case sig := <-d.vtSig:
			ok = d.handleVTSignal(sig)
		case <-d.wake:
			ok = d.redraw()
		case <-timer.C:
			ok = d.redraw()
		}
		if !ok {
			return
		}
	}
}

// widgetWakeMax は、ウィジェットがあるときに眠る最長の時間。
// 壁時計が NTP などで飛んでも、これより長くは古い表示のままにならない。
const widgetWakeMax = time.Minute

// widgetWakeSlack は、切り替わりの時刻を確実に過ぎてから起きるための余裕。
const widgetWakeSlack = 5 * time.Millisecond

func (d *Display) untilWake() time.Duration {
	d.drawMu.Lock()
	at := d.wakeAt
	d.drawMu.Unlock()
	if at.IsZero() {
		return time.Hour
	}
	return min(max(time.Until(at)+widgetWakeSlack, 0), widgetWakeMax)
}

func (d *Display) widgetEnv() WidgetEnv {
	if d.env != nil {
		return d.env()
	}
	return WidgetEnv{Now: time.Now()}
}

// drawAllLocked は格子 l の全体を裏画面に描き、ウィジェットの中身を覚える。drawMu を持って呼ぶ。
func (d *Display) drawAllLocked(l *Layout) {
	l.Env = d.widgetEnv()
	l.Images = d.images
	drawAll(d.cv, l, d.drawn)
	d.wkeys = make([]string, len(l.Cells))
	d.wakeAt = time.Time{}
	for i := range l.Cells {
		if v := &l.Cells[i]; v.Widget != nil && !v.Covered {
			d.wkeys[i] = widgetKey(v, l.Env)
			d.scheduleWidget(v, l.Env)
		}
	}
}

func (d *Display) scheduleWidget(v *CellView, env WidgetEnv) {
	if t := widgetNext(v, env); !t.IsZero() && (d.wakeAt.IsZero() || t.Before(d.wakeAt)) {
		d.wakeAt = t
	}
}

// redrawWidgets は、中身が変わったウィジェットのセルだけを描き直す。drawMu を持って呼ぶ。
func (d *Display) redrawWidgets() {
	l := d.layout
	l.Env = d.widgetEnv()
	d.wakeAt = time.Time{}
	for i := range l.Cells {
		v := &l.Cells[i]
		if v.Widget == nil || v.Covered {
			continue
		}
		d.scheduleWidget(v, l.Env)
		k := widgetKey(v, l.Env)
		if k == d.wkeys[i] {
			continue
		}
		t := time.Now()
		col, row := i%l.Cols, i/l.Cols
		r := drawCell(d.cv, l, col, row, d.drawn[i])
		d.wkeys[i] = k
		if d.active {
			d.fb.Blit(d.cv, d.cv.physRect(r))
		}
		vlogf("display: widget %s at %d,%d redraw %v", v.Widget.Kind, col, row, time.Since(t))
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
	invalid := d.invalid
	d.invalid = false
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
		d.drawAllLocked(next)
		if d.active {
			d.fb.Blit(d.cv, image.Rect(0, 0, d.cv.pw, d.cv.ph))
		}
		vlogf("display: layer %q redraw %v", next.Title, time.Since(t))
	} else if invalid {
		t := time.Now()
		d.drawAllLocked(d.layout)
		if d.active {
			d.fb.Blit(d.cv, image.Rect(0, 0, d.cv.pw, d.cv.ph))
		}
		vlogf("display: full redraw (images changed) %v", time.Since(t))
	}
	for i := range want {
		if want[i] == d.drawn[i] {
			continue
		}
		t := time.Now()
		col, row := i%d.layout.Cols, i/d.layout.Cols
		var rs []image.Rectangle
		if d.layout.pressFill() {
			d.layout.Env = d.widgetEnv()
			rs = []image.Rectangle{drawCell(d.cv, d.layout, col, row, want[i])}
			if v := &d.layout.Cells[i]; v.Widget != nil {
				d.wkeys[i] = widgetKey(v, d.layout.Env)
			}
		} else {
			rs = drawPress(d.cv, d.layout, col, row, want[i])
		}
		d.drawn[i] = want[i]
		if d.active {
			for _, r := range rs {
				d.fb.Blit(d.cv, d.cv.physRect(r))
			}
		}
		vlogf("display: cell %d,%d pressed=%v %s redraw %v", col, row, want[i], pressName(d.layout), time.Since(t))
	}
	d.redrawWidgets()
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
