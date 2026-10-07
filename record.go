package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- タッチの記録と再生 ----------
//
// 本物のタッチは自動では作れない（カーネルに uinput もない）。そこで、タッチパネルの生のイベントを
// そのままファイルに記録し（-record-touch）、PC で再生して、トラックパッドの判定を確かめる（-replay-touch、テスト）。
//
// ファイルの形（.touch）：
//
//	# lefthand touch recording
//	{"format":1,"gesture":"slow",...}            ← 見出し（RecHeader）
//	<最初のイベントからのマイクロ秒> <type> <code> <value>   ← イベント 1 つが 1 行
//
// 時刻は、カーネルがイベントに付けた時刻（evdev の timestamp）。

const recMagic = "# lefthand touch recording"

// RecHeader は記録の見出し。
type RecHeader struct {
	Format  int    `json:"format"`
	Gesture string `json:"gesture,omitempty"` // 何を記録したか（slow、tap など）
	Note    string `json:"note,omitempty"`
	Device  string `json:"device"`
	Started string `json:"started"`
	// タッチの設定（生座標を画面のドットにするのに使う）
	SwapXY bool                `json:"swap_xy,omitempty"`
	MinX   int32               `json:"min_x"`
	MaxX   int32               `json:"max_x"`
	MinY   int32               `json:"min_y"`
	MaxY   int32               `json:"max_y"`
	Abs    map[string][2]int32 `json:"abs,omitempty"` // ドライバーの範囲
	Screen [2]int              `json:"screen"`
	Layer  string              `json:"layer,omitempty"` // 画面に出していたレイヤー
	// Cell は、画面に出していたトラックパッドのセルの範囲 [x0, y0, x1, y1]（ドット）。なければ画面全体
	Cell [4]int `json:"cell"`
	// Params は、記録したときの設定のトラックパッドの項目（参考）
	Params *ActionSpec `json:"params,omitempty"`
}

// RecEvent はイベント 1 つ。
type RecEvent struct {
	T     time.Duration // 最初のイベントからの時間
	Type  evdev.EvType
	Code  evdev.EvCode
	Value int32
}

type TouchRecording struct {
	Header RecHeader
	Events []RecEvent
}

func (h *RecHeader) touchConfig() *TouchConfig {
	return &TouchConfig{SwapXY: h.SwapXY, MinX: h.MinX, MaxX: h.MaxX, MinY: h.MinY, MaxY: h.MaxY}
}

func (h *RecHeader) cellRect() image.Rectangle {
	r := image.Rect(h.Cell[0], h.Cell[1], h.Cell[2], h.Cell[3])
	if r.Empty() {
		return image.Rect(0, 0, h.Screen[0], h.Screen[1])
	}
	return r
}

// ReadRecording は .touch のファイルを読む。
func ReadRecording(r io.Reader) (*TouchRecording, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	rec := &TouchRecording{}
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		switch {
		case line == 1:
			if s != recMagic {
				return nil, fmt.Errorf("not a touch recording (the first line must be %q)", recMagic)
			}
		case line == 2:
			if err := json.Unmarshal([]byte(s), &rec.Header); err != nil {
				return nil, fmt.Errorf("line 2: %v", err)
			}
			if rec.Header.Format != 1 {
				return nil, fmt.Errorf("unknown format %d", rec.Header.Format)
			}
		case s == "" || strings.HasPrefix(s, "#"):
		default:
			f := strings.Fields(s)
			if len(f) != 4 {
				return nil, fmt.Errorf("line %d: want 4 fields", line)
			}
			var n [4]int64
			for i := range f {
				v, err := strconv.ParseInt(f[i], 10, 64)
				if err != nil {
					return nil, fmt.Errorf("line %d: %v", line, err)
				}
				n[i] = v
			}
			rec.Events = append(rec.Events, RecEvent{time.Duration(n[0]) * time.Microsecond, evdev.EvType(n[1]), evdev.EvCode(n[2]), int32(n[3])})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if line < 2 {
		return nil, errors.New("empty recording")
	}
	return rec, nil
}

// LoadRecording はファイルを読む。
func LoadRecording(path string) (*TouchRecording, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rec, err := ReadRecording(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rec, nil
}

// ---------- 再生 ----------

// padLog は、トラックパッドの出力を時刻つきで覚える（再生とテスト用）。
type padLog struct {
	now  func() time.Duration
	held map[string]byte
	ops  []padOp
}

type padOp struct {
	T       time.Duration
	Kind    string // press、release、move、wheel
	Src     string
	Btn     byte
	DX, DY  int
	V, H    int
	Buttons byte // この操作のあとに押しているボタン
}

func newPadLog(now func() time.Duration) *padLog { return &padLog{now: now, held: map[string]byte{}} }

func (l *padLog) buttons() byte {
	var b byte
	for _, v := range l.held {
		b |= v
	}
	return b
}

func (l *padLog) Press(src string, b byte) {
	l.held[src] = b
	l.ops = append(l.ops, padOp{T: l.now(), Kind: "press", Src: src, Btn: b, Buttons: l.buttons()})
}

func (l *padLog) Release(src string) {
	b, ok := l.held[src]
	if !ok {
		return
	}
	delete(l.held, src)
	l.ops = append(l.ops, padOp{T: l.now(), Kind: "release", Src: src, Btn: b, Buttons: l.buttons()})
}

func (l *padLog) Move(dx, dy int) {
	l.ops = append(l.ops, padOp{T: l.now(), Kind: "move", DX: dx, DY: dy, Buttons: l.buttons()})
}

func (l *padLog) Wheel(v, h int) {
	l.ops = append(l.ops, padOp{T: l.now(), Kind: "wheel", V: v, H: h, Buttons: l.buttons()})
}

// ReplayResult は、記録を再生した結果。
type ReplayResult struct {
	Stats   PadStats
	Ops     []padOp
	Touches []TouchSummary
}

// TouchSummary は、記録の中のタッチ 1 回（BTN_TOUCH の 1 から 0 まで）のまとめ。
type TouchSummary struct {
	Start, Dur         time.Duration
	Samples            int
	X0, Y0, X1, Y1     float64 // 最初と最後のサンプル（画面のドット）
	MinP, MaxP         int32
	FirstJump, MaxStep float64 // 最初の 2 サンプルの間の距離、隣り合うサンプルの距離の最大（ドット）
	InCell             bool
	Open               bool // 記録の終わりまで離さなかった
}

// replayTouch は、記録を p の設定のトラックパッドで再生する。
// トラックパッドのセルは、記録したときのもの（見出しの cell）。touchProc と同じく、
// 触れて最初のサンプルの位置がセルの中ならトラックパッドで判定し、外なら（ボタンなどを押したとして）判定しない。
func replayTouch(rec *TouchRecording, p *PadParams) ReplayResult {
	h := &rec.Header
	base := time.Unix(1_000_000, 0) // 再生の時刻の起点（何でもよい）
	var now time.Duration
	lg := newPadLog(func() time.Duration { return now })
	pad := NewPad(lg, false)
	tc := h.touchConfig()
	W, H := h.Screen[0], h.Screen[1]
	cell := h.cellRect()
	var (
		x, y, pr      int32
		down, pressed bool
		onPad         bool
		touches       []TouchSummary
		cur           *TouchSummary
		last          fpt
	)
	tick := func(until time.Duration) {
		for {
			d := pad.Deadline()
			if d.IsZero() || d.Sub(base) > until {
				return
			}
			now = d.Sub(base)
			pad.Tick(d)
		}
	}
	for _, ev := range rec.Events {
		tick(ev.T)
		now = ev.T
		t := base.Add(ev.T)
		switch {
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_X:
			x = ev.Value
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_Y:
			y = ev.Value
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_PRESSURE:
			pr = ev.Value
		case ev.Type == evdev.EV_KEY && ev.Code == evdev.BTN_TOUCH:
			down = ev.Value == 1
			if !down && pressed {
				if onPad {
					pad.Up(t)
				}
				pressed, onPad = false, false
				if cur != nil {
					cur.Dur = ev.T - cur.Start
					touches = append(touches, *cur)
					cur = nil
				}
			}
		case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT:
			if !down {
				continue
			}
			pt := (&Pad{tc: tc, W: W, H: H}).screen(x, y)
			if !pressed {
				pressed = true
				cur = &TouchSummary{Start: ev.T, X0: pt.X, Y0: pt.Y, MinP: pr, MaxP: pr, FirstJump: -1}
				ip := image.Pt(int(pt.X), int(pt.Y))
				onPad = ip.In(cell)
				cur.InCell = onPad
				if onPad {
					pad.Down(p, cell, tc, W, H, t)
				} else {
					pad.Interrupt()
				}
			} else {
				step := math.Hypot(pt.X-last.X, pt.Y-last.Y)
				if cur.FirstJump < 0 {
					cur.FirstJump = step
				}
				cur.MaxStep = max(cur.MaxStep, step)
			}
			last = pt
			cur.Samples++
			cur.X1, cur.Y1 = pt.X, pt.Y
			cur.MinP, cur.MaxP = min(cur.MinP, pr), max(cur.MaxP, pr)
			if onPad {
				pad.Sample(x, y, pr, t)
			}
		}
	}
	if cur != nil { // 記録の終わりまで触れていた
		if n := len(rec.Events); n > 0 {
			cur.Dur = rec.Events[n-1].T - cur.Start
		}
		cur.Open = true
		touches = append(touches, *cur)
	}
	tick(time.Duration(math.MaxInt64))
	return ReplayResult{Stats: pad.Stats(), Ops: lg.ops, Touches: touches}
}

// padParamsFor は、再生に使うトラックパッドの設定。config にトラックパッドのセルがあれば、記録したレイヤーのもの
// （なければ最初のもの）、なければ既定値。override（ActionSpec の JSON）があれば、その項目を上書きする。
func padParamsFor(km *Keymap, layer string, override string) (*PadParams, error) {
	var spec ActionSpec
	if km != nil {
		var found *ActionSpec
		for _, l := range km.Layers {
			if l.Grid == nil {
				continue
			}
			for _, a := range l.Grid.Cells {
				if a.Widget != nil && a.Widget.Kind == widgetPad && (found == nil || l.Name == layer) {
					s := a.Spec
					found = &s
				}
			}
		}
		if found != nil {
			spec = *found
		}
	}
	if override != "" {
		if err := json.Unmarshal([]byte(override), &spec); err != nil {
			return nil, fmt.Errorf("-replay-params: %v", err)
		}
	}
	spec.Widget = widgetPad
	spec.Label, spec.Span, spec.Background = "", Span{}, ""
	w, err := compilePad(spec)
	if err != nil {
		return nil, err
	}
	return w.Pad, nil
}

// printReplay は、再生の結果を人が読める形で書く。
func printReplay(w io.Writer, path string, rec *TouchRecording, r ReplayResult, ops bool) {
	h := rec.Header
	fmt.Fprintf(w, "%s: gesture=%q, %d events, cell %v\n", path, h.Gesture, len(rec.Events), h.cellRect())
	for i, t := range r.Touches {
		fmt.Fprintf(w, "  touch #%d at %6.3fs: %4d ms, %3d samples, (%5.1f,%5.1f) -> (%5.1f,%5.1f), pressure %d..%d, first step %.1f, max step %.1f%s\n",
			i+1, t.Start.Seconds(), t.Dur.Milliseconds(), t.Samples, t.X0, t.Y0, t.X1, t.Y1, t.MinP, t.MaxP, t.FirstJump, t.MaxStep,
			map[bool]string{true: "", false: " (outside the trackpad)"}[t.InCell]+map[bool]string{true: " (not released before the end)"}[t.Open])
	}
	s := r.Stats
	fmt.Fprintf(w, "  => taps %d, clicks %d, double clicks %d, drags %d, right clicks %d, move (%d, %d) path (%d, %d), wheel %d (|%d|), dropped samples %d, ignored touches %d, resumed touches %d\n",
		s.Taps, s.Clicks, s.DoubleClicks, s.Drags, s.RightClicks, s.MoveX, s.MoveY, s.PathX, s.PathY, s.Wheel, s.WheelAbs, s.Dropped, s.Ignored, s.Resumed)
	if ops {
		for _, o := range r.Ops {
			switch o.Kind {
			case "move":
				fmt.Fprintf(w, "    %8.3fs move %4d %4d  buttons=%d\n", o.T.Seconds(), o.DX, o.DY, o.Buttons)
			case "wheel":
				fmt.Fprintf(w, "    %8.3fs wheel %d\n", o.T.Seconds(), o.V)
			default:
				fmt.Fprintf(w, "    %8.3fs %s %s  buttons=%d\n", o.T.Seconds(), o.Kind, o.Src, o.Buttons)
			}
		}
	}
}

// ---------- 記録 ----------

// recordOptions は -record-touch の設定。
type recordOptions struct {
	Out     string
	Gesture string
	Note    string
	Layer   string
	For     time.Duration
}

// padCellOf は、km の layer のレイヤー（空ならトラックパッドのある最初のレイヤー）を base に重ねた画面で、
// トラックパッドのセルの範囲を返す。セルがなければ、レイヤーの番号と空の範囲。
func padCellOf(km *Keymap, layer string, W, H int) (int, image.Rectangle, *ActionSpec, error) {
	li := -1
	for i, l := range km.Layers {
		if layer != "" {
			if l.Name == layer {
				li = i
			}
			continue
		}
		if li < 0 && l.Grid != nil {
			for _, a := range l.Grid.Cells {
				if a.Widget != nil && a.Widget.Kind == widgetPad {
					li = i
				}
			}
		}
	}
	if layer != "" && li < 0 {
		return 0, image.Rectangle{}, nil, fmt.Errorf("unknown layer %q", layer)
	}
	if li < 0 {
		li = 0
	}
	stack := []int{0}
	if li != 0 {
		stack = append(stack, li)
	}
	v := km.view(stack)
	for i, a := range v.Cells {
		if a != nil && a.Widget != nil && a.Widget.Kind == widgetPad {
			s := a.Spec
			return li, cellRect(i%v.Cols, i/v.Cols, a.SpanW, a.SpanH, v.Cols, v.Rows, W, H), &s, nil
		}
	}
	return li, image.Rectangle{}, nil, nil
}

// runRecord は、タッチパネルの生のイベントをファイルに記録する。lefthand.service を止めてから使う。
// 画面があれば、記録するレイヤー（トラックパッドのあるレイヤー）を出す。
func runRecord(cfg *Config, km *Keymap, o recordOptions) error {
	if cfg.Touch == nil {
		return errors.New("config has no touch section")
	}
	dev, err := openInput(cfg.Touch.Device)
	if err != nil {
		return fmt.Errorf("open touch: %w", err)
	}
	if err := dev.Grab(); err != nil {
		return fmt.Errorf("grab %s: %v (lefthand.service が動いていれば、sudo systemctl stop lefthand してから記録してください)", dev.Path(), err)
	}
	W, H := screenSize(cfg)
	li, cell, spec, err := padCellOf(km, o.Layer, W, H)
	if err != nil {
		return err
	}
	if spec != nil {
		spec.Label, spec.Span, spec.Background, spec.Widget = "", Span{}, "", ""
	}
	tc := cfg.Touch
	h := RecHeader{Format: 1, Gesture: o.Gesture, Note: o.Note, Device: describe(dev), Started: time.Now().Format(time.RFC3339),
		SwapXY: tc.SwapXY, MinX: tc.MinX, MaxX: tc.MaxX, MinY: tc.MinY, MaxY: tc.MaxY, Abs: map[string][2]int32{},
		Screen: [2]int{W, H}, Layer: km.Layers[li].Name, Cell: [4]int{cell.Min.X, cell.Min.Y, cell.Max.X, cell.Max.Y}, Params: spec}
	if infos, err := dev.AbsInfos(); err == nil {
		for code, ai := range infos {
			h.Abs[evdev.CodeName(evdev.EV_ABS, code)] = [2]int32{ai.Minimum, ai.Maximum}
		}
	}
	f, err := os.Create(o.Out)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	hb, _ := json.Marshal(h)
	fmt.Fprintf(bw, "%s\n%s\n", recMagic, hb)

	// 画面：記録するレイヤーを出す。右上の札に「記録」と出す
	if cfg.displayEnabled() {
		e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
		if li != 0 {
			e.press("record", &Action{Kind: actToggle, Layer: li})
		}
		l := buildLayout(km, e.View())
		l.Title = "記録中 " + o.Gesture
		if d, err := StartDisplay(cfg.Display, l, nil, OpenImageStore(defaultDataDir+"/images")); err != nil {
			log.Printf("display disabled: %v", err)
		} else {
			defer d.Close()
		}
	}

	var (
		mu      sync.Mutex
		first   time.Time
		n       int
		touches int
		done    = make(chan struct{})
		cur     *TouchSummary
		x, y, p int32
		down    bool
		pressed bool
	)
	conv := &Pad{tc: tc, W: W, H: H}
	go func() {
		defer close(done)
		for {
			ev, err := dev.ReadOne()
			if err != nil {
				log.Printf("read touch: %v", err)
				return
			}
			t := evTime(ev)
			mu.Lock()
			if first.IsZero() {
				first = t
			}
			fmt.Fprintf(bw, "%d %d %d %d\n", t.Sub(first).Microseconds(), ev.Type, ev.Code, ev.Value)
			n++
			// 画面に出すまとめ（記録できているか分かるように）
			switch {
			case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_X:
				x = ev.Value
			case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_Y:
				y = ev.Value
			case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_PRESSURE:
				p = ev.Value
			case ev.Type == evdev.EV_KEY && ev.Code == evdev.BTN_TOUCH:
				down = ev.Value == 1
				if !down && pressed {
					pressed = false
					touches++
					fmt.Fprintf(os.Stderr, "touch #%d: %d ms, %d samples, (%.0f,%.0f) -> (%.0f,%.0f), pressure %d..%d\n",
						touches, t.Sub(first).Milliseconds()-cur.Start.Milliseconds(), cur.Samples, cur.X0, cur.Y0, cur.X1, cur.Y1, cur.MinP, cur.MaxP)
				}
			case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT && down:
				pt := conv.screen(x, y)
				if !pressed {
					pressed = true
					cur = &TouchSummary{Start: t.Sub(first), X0: pt.X, Y0: pt.Y, MinP: p, MaxP: p}
				}
				cur.Samples++
				cur.X1, cur.Y1 = pt.X, pt.Y
				cur.MinP, cur.MaxP = min(cur.MinP, p), max(cur.MaxP, p)
			}
			mu.Unlock()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	fmt.Fprintf(os.Stderr, "recording %q to %s for %v (layer %q, trackpad cell %v). Ctrl-C to stop early.\n",
		o.Gesture, o.Out, o.For, h.Layer, cell)
	select {
	case <-time.After(o.For):
	case <-sig:
	case <-done:
	}
	dev.Ungrab()
	mu.Lock()
	defer mu.Unlock()
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved %s: %d events, %d touches\n", o.Out, n, touches)
	return nil
}
