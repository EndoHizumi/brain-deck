package main

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 作った記録（実機の記録の代わりに、跳ねと揺れを入れたもの） ----------

// synth は、記録を組み立てる。座標は画面のドットで書き、生の座標に直して入れる。
type synth struct {
	rec   TouchRecording
	t     time.Duration
	rnd   *rand.Rand
	every time.Duration // サンプルの間隔
	noise float64       // 揺れ（生の座標の標準偏差）
	jump  float64       // 触れた瞬間と離す瞬間の跳ね（生の座標）
}

func newSynth() *synth {
	return &synth{
		rec: TouchRecording{Header: RecHeader{Format: 1, MinX: 226, MaxX: 3938, MinY: 3803, MaxY: 384,
			Screen: [2]int{800, 480}, Cell: [4]int{0, 0, 600, 480}}},
		rnd: rand.New(rand.NewSource(1)), every: 10 * time.Millisecond, noise: 4, jump: 120,
	}
}

func (s *synth) raw(p fpt) (int32, int32) {
	h := s.rec.Header
	x := float64(h.MinX) + p.X*float64(h.MaxX-h.MinX)/float64(h.Screen[0])
	y := float64(h.MinY) + p.Y*float64(h.MaxY-h.MinY)/float64(h.Screen[1])
	return int32(math.Round(x + s.rnd.NormFloat64()*s.noise)), int32(math.Round(y + s.rnd.NormFloat64()*s.noise))
}

func (s *synth) ev(typ evdev.EvType, code evdev.EvCode, v int32) {
	s.rec.Events = append(s.rec.Events, RecEvent{s.t, typ, code, v})
}

func (s *synth) sample(p fpt, pressure int32, jump bool) {
	x, y := s.raw(p)
	if jump {
		x += int32(s.jump)
		y -= int32(s.jump)
	}
	s.ev(evdev.EV_ABS, evdev.ABS_X, x)
	s.ev(evdev.EV_ABS, evdev.ABS_Y, y)
	s.ev(evdev.EV_ABS, evdev.ABS_PRESSURE, pressure)
	s.ev(evdev.EV_SYN, evdev.SYN_REPORT, 0)
}

// stroke は、from から to まで dur かけて動かすタッチ 1 回。最初と最後のサンプルは跳ねる。
func (s *synth) stroke(from, to fpt, dur time.Duration) {
	n := max(int(dur/s.every), 1)
	s.sample(from, 300, true)
	s.ev(evdev.EV_KEY, evdev.BTN_TOUCH, 1)
	s.ev(evdev.EV_SYN, evdev.SYN_REPORT, 0)
	for i := 0; i <= n; i++ {
		s.t += s.every
		f := float64(i) / float64(n)
		s.sample(fpt{from.X + (to.X-from.X)*f, from.Y + (to.Y-from.Y)*f}, 600, false)
	}
	s.t += s.every
	s.sample(to, 200, true) // 離す瞬間の跳ね
	s.t += 2 * time.Millisecond
	s.ev(evdev.EV_KEY, evdev.BTN_TOUCH, 0)
	s.ev(evdev.EV_ABS, evdev.ABS_PRESSURE, 0)
	s.ev(evdev.EV_SYN, evdev.SYN_REPORT, 0)
}

func (s *synth) tap(p fpt, dur time.Duration) { s.stroke(p, p, dur) }
func (s *synth) wait(d time.Duration)         { s.t += d }

func replayDefault(t *testing.T, s *synth, mod func(*PadParams)) ReplayResult {
	t.Helper()
	// 書き出して読み直す（ファイルの形も確かめる）
	var buf bytes.Buffer
	writeRecording(&buf, &s.rec)
	rec, err := ReadRecording(&buf)
	if err != nil {
		t.Fatal(err)
	}
	p := defaultPad
	if mod != nil {
		mod(&p)
	}
	return replayTouch(rec, &p)
}

func writeRecording(w *bytes.Buffer, r *TouchRecording) {
	fmt.Fprintf(w, "%s\n{\"format\":1,\"min_x\":%d,\"max_x\":%d,\"min_y\":%d,\"max_y\":%d,\"screen\":[%d,%d],\"cell\":[%d,%d,%d,%d],\"gesture\":%q}\n",
		recMagic, r.Header.MinX, r.Header.MaxX, r.Header.MinY, r.Header.MaxY, r.Header.Screen[0], r.Header.Screen[1],
		r.Header.Cell[0], r.Header.Cell[1], r.Header.Cell[2], r.Header.Cell[3], r.Header.Gesture)
	for _, e := range r.Events {
		fmt.Fprintf(w, "%d %d %d %d\n", e.T.Microseconds(), e.Type, e.Code, e.Value)
	}
}

func TestPadSlowSwipeMovesRight(t *testing.T) {
	s := newSynth()
	s.stroke(fpt{100, 240}, fpt{400, 240}, 1500*time.Millisecond) // 200 ドット/秒
	r := replayDefault(t, s, nil).Stats
	if r.Clicks+r.Taps != 0 {
		t.Errorf("swipe made a click: %+v", r)
	}
	// 遅いので加速はほぼない。300 ドット × speed 1.0 × (1 + 0.2 程度)
	if r.MoveX < 250 || r.MoveX > 420 || abs(r.MoveY) > 15 {
		t.Errorf("slow swipe: %+v", r)
	}
}

func TestPadFastSwipeAccelerates(t *testing.T) {
	slow, fast := newSynth(), newSynth()
	slow.stroke(fpt{100, 240}, fpt{400, 240}, 1500*time.Millisecond)
	fast.stroke(fpt{100, 240}, fpt{400, 240}, 150*time.Millisecond) // 2000 ドット/秒
	rs, rf := replayDefault(t, slow, nil).Stats, replayDefault(t, fast, nil).Stats
	if rf.MoveX < rs.MoveX*3/2 {
		t.Errorf("fast swipe should go much farther: slow %d, fast %d", rs.MoveX, rf.MoveX)
	}
	noAccel := replayDefault(t, fast, func(p *PadParams) { p.Accel = 0 }).Stats
	if noAccel.MoveX > 330 {
		t.Errorf("accel 0 should move about 300*speed: %d", noAccel.MoveX)
	}
	rv := newSynth()
	rv.stroke(fpt{300, 400}, fpt{300, 100}, 600*time.Millisecond) // 上へ
	if r := replayDefault(t, rv, nil).Stats; r.MoveY >= -200 || abs(r.MoveX) > 15 {
		t.Errorf("upward swipe: %+v", r)
	}
}

func TestPadTapClicksWithoutMoving(t *testing.T) {
	s := newSynth()
	for i := 0; i < 5; i++ {
		s.tap(fpt{300, 200}, 80*time.Millisecond)
		s.wait(time.Second)
	}
	res := replayDefault(t, s, nil)
	r := res.Stats
	if r.Clicks != 5 || r.DoubleClicks != 0 || r.Drags != 0 {
		t.Errorf("taps: %+v", r)
	}
	if r.PathX+r.PathY > 5 {
		t.Errorf("taps moved the cursor (jumps not filtered): %+v", r)
	}
	// ボタンは、タップの drag_ms あとに離す
	var press, release time.Duration
	for _, o := range res.Ops {
		if o.Kind == "press" && press == 0 {
			press = o.T
		}
		if o.Kind == "release" && release == 0 {
			release = o.T
		}
	}
	if d := release - press; d < defaultPad.DragGap-time.Millisecond || d > defaultPad.DragGap+time.Millisecond {
		t.Errorf("click held %v, want %v", d, defaultPad.DragGap)
	}
	// drag_ms 0 なら、すぐ離す
	r0 := replayDefault(t, s, func(p *PadParams) { p.DragGap = 0 })
	if r0.Stats.Clicks != 5 || r0.Ops[1].Kind != "release" || r0.Ops[1].T != r0.Ops[0].T {
		t.Errorf("drag_ms 0: %+v %+v", r0.Stats, r0.Ops[:2])
	}
}

func TestPadLongTouchIsNotTap(t *testing.T) {
	s := newSynth()
	s.tap(fpt{300, 200}, time.Second) // 指を置いたまま止める
	r := replayDefault(t, s, nil).Stats
	if r.Clicks != 0 || r.RightClicks != 0 || r.PathX+r.PathY > 4 {
		t.Errorf("resting finger: %+v", r)
	}
	lp := replayDefault(t, s, func(p *PadParams) { p.LongPress = true }).Stats
	if lp.RightClicks != 1 || lp.Clicks != 0 {
		t.Errorf("long_press: right: %+v", lp)
	}
}

func TestPadDoubleTapAndDrag(t *testing.T) {
	s := newSynth()
	s.tap(fpt{300, 200}, 80*time.Millisecond)
	s.wait(100 * time.Millisecond)
	s.tap(fpt{300, 200}, 80*time.Millisecond)
	s.wait(time.Second)
	if r := replayDefault(t, s, nil).Stats; r.DoubleClicks != 1 || r.Clicks != 0 || r.Drags != 0 {
		t.Errorf("double tap: %+v", r)
	}

	d := newSynth()
	d.tap(fpt{200, 200}, 80*time.Millisecond)
	d.wait(100 * time.Millisecond)
	d.stroke(fpt{200, 200}, fpt{400, 300}, 500*time.Millisecond)
	res := replayDefault(t, d, nil)
	if r := res.Stats; r.Drags != 1 || r.Clicks != 0 || r.MoveX < 150 {
		t.Errorf("tap drag: %+v", r)
	}
	moved := 0
	for _, o := range res.Ops {
		if o.Kind == "move" {
			moved++
			if o.Buttons != mouseLeft {
				t.Fatalf("moved without the button while dragging: %+v", o)
			}
		}
	}
	if last := res.Ops[len(res.Ops)-1]; moved == 0 || last.Kind != "release" || last.Buttons != 0 {
		t.Errorf("drag must end with the button released: %+v", last)
	}
}

func TestPadScrollBand(t *testing.T) {
	s := newSynth()
	s.stroke(fpt{570, 100}, fpt{570, 340}, 800*time.Millisecond) // 帯（右から 72 ドット）を下へ 240 ドット
	r := replayDefault(t, s, nil).Stats
	// natural：指を下へ → 上へスクロール（正）。240/24 = 10 段（ヒステリシスで少し減る）
	if r.Wheel < 8 || r.Wheel > 10 || r.PathX+r.PathY != 0 || r.Clicks != 0 {
		t.Errorf("scroll band: %+v", r)
	}
	tr := replayDefault(t, s, func(p *PadParams) { p.Natural = false }).Stats
	if tr.Wheel != -r.Wheel {
		t.Errorf("traditional: %d, natural: %d", tr.Wheel, r.Wheel)
	}
	off := replayDefault(t, s, func(p *PadParams) { p.ScrollWidth = 0 }).Stats
	if off.Wheel != 0 || off.MoveY < 200 {
		t.Errorf("scroll_width 0 should move the cursor: %+v", off)
	}
}

func TestPadPressureAndSettleDropSamples(t *testing.T) {
	s := newSynth()
	s.stroke(fpt{100, 240}, fpt{400, 240}, 500*time.Millisecond)
	// 強さ 600 のサンプルしかないので、700 より弱いものを捨てると何も動かない（離したあとの跳ねもない）
	r := replayDefault(t, s, func(p *PadParams) { p.MinPressure = 700 }).Stats
	if r.PathX+r.PathY != 0 || r.Dropped == 0 {
		t.Errorf("min_pressure: %+v", r)
	}
	r = replayDefault(t, s, func(p *PadParams) { p.Settle = 0; p.Smooth = 1; p.Deadzone = 0 }).Stats
	// 跳ねた最初のサンプルを使うので、上へ大きく動く
	if r.MoveY > -5 {
		t.Errorf("without settle the first jump should show up: %+v", r)
	}
}

func TestPadTouchOutsideCellIsIgnored(t *testing.T) {
	s := newSynth()
	s.tap(fpt{300, 200}, 80*time.Millisecond)
	s.wait(50 * time.Millisecond)
	s.tap(fpt{700, 200}, 80*time.Millisecond) // セルの外（ボタン）。タップの待ちを終える
	res := replayDefault(t, s, nil)
	if r := res.Stats; r.Clicks != 1 || r.DoubleClicks != 0 {
		t.Errorf("%+v", r)
	}
	if res.Touches[1].InCell {
		t.Error("second touch should be outside the cell")
	}
}

func TestReadRecordingErrors(t *testing.T) {
	for _, s := range []string{"", "hello\n{}\n", recMagic + "\n{\"format\":2}\n", recMagic + "\n{\"format\":1}\n1 2 3\n"} {
		if _, err := ReadRecording(strings.NewReader(s)); err == nil {
			t.Errorf("%q: want an error", s)
		}
	}
}

// ---------- 実機の記録 ----------

// testdata/touch/*.touch は、Brain で lefthand -record-touch を使って記録したもの（tools/record-touch.sh）。
// 見出しの gesture ごとに、既定の設定で期待どおりに判定できるかを確かめる。
// 記録の手順は README の「トラックパッドの調整」。
func TestPadRealRecordings(t *testing.T) {
	files, _ := filepath.Glob("testdata/touch/*.touch")
	if len(files) == 0 {
		t.Skip("no recordings in testdata/touch")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			rec, err := LoadRecording(f)
			if err != nil {
				t.Fatal(err)
			}
			res := replayTouch(rec, &defaultPad)
			var buf bytes.Buffer
			printReplay(&buf, f, rec, res, false)
			t.Log(buf.String())
			checkGesture(t, rec.Header.Gesture, res)
		})
	}
}

// checkGesture は、記録の手順（tools/record-touch.sh）の動きに合った判定かを確かめる。
func checkGesture(t *testing.T, g string, r ReplayResult) {
	s := r.Stats
	in := 0
	for _, x := range r.Touches {
		if x.InCell {
			in++
		}
	}
	switch strings.TrimSuffix(g, "-2") {
	case "slow", "fast":
		// なぞる：クリックしない。カーソルは動く
		if s.Clicks+s.DoubleClicks+s.Drags+s.RightClicks != 0 {
			t.Errorf("swipes made clicks: %+v", s)
		}
		if s.PathX+s.PathY < 50*in {
			t.Errorf("swipes moved too little: %+v", s)
		}
	case "tap":
		// 1 回ずつ間をあけたタップ：すべてクリック、ほとんど動かない
		if s.Clicks != in || s.DoubleClicks != 0 || s.Drags != 0 {
			t.Errorf("taps: %d touches, %+v", in, s)
		}
		if s.PathX+s.PathY > 3*in {
			t.Errorf("taps moved the cursor: %+v", s)
		}
	case "doubletap":
		if s.DoubleClicks*2 != in || s.Drags != 0 {
			t.Errorf("double taps: %d touches, %+v", in, s)
		}
	case "tapdrag":
		if s.Drags*2 != in || s.Clicks != 0 {
			t.Errorf("tap and drag: %d touches, %+v", in, s)
		}
	case "scroll":
		if s.WheelAbs < 3*in || s.PathX+s.PathY != 0 || s.Clicks != 0 {
			t.Errorf("scroll band: %d touches, %+v", in, s)
		}
	case "hold":
		if s.Clicks+s.RightClicks != 0 || s.PathX+s.PathY > 4*in {
			t.Errorf("resting finger: %+v", s)
		}
	}
}

// ---------- エンジンを通した再生（タッチの処理、マウスの出力、ボタンを離す処理） ----------

const padConfig = `
touch: { min_x: 226, max_x: 3938, min_y: 3803, max_y: 384 }
layers:
  - name: base
    keys:
      KEY_A: { mouse: left }
      KEY_S: { layer_hold: other }
      KEY_D: { mouse: scroll_down }
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: trackpad, span: [3, 3], scroll_width: 72 }
        "3,0": { mouse: left }
        "3,1": { mouse: right, label: 右 }
  - name: other
    keys:
      KEY_Q: { mouse: middle }
`

type padRig struct {
	t   *testing.T
	h   *hidRec
	e   *Engine
	tp  *touchProc
	s   *synth
	km  *Keymap
	pad *Pad
}

func newPadRig(t *testing.T) *padRig {
	cfg, err := parseConfig([]byte(padConfig))
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &hidRec{}
	st := &State{hid: h, active: map[string]Combo{}, kbdID: hidKeyboardID, mouse: NewMouse(h, hidMouseID)}
	e := NewEngine(km, st)
	pad := NewPad(st.mouse, false)
	e.SetPad(pad)
	s := newSynth()
	s.rec.Header.Cell = [4]int{0, 0, 600, 480}
	return &padRig{t: t, h: h, e: e, tp: &touchProc{e: e}, s: s, km: km, pad: pad}
}

// play は、synth に貯めたイベントをエンジンに流す（トラックパッドの待ち時間も処理する）。
func (r *padRig) play() {
	base := time.Unix(1_000_000, 0)
	for _, ev := range r.s.rec.Events {
		for d := r.pad.Deadline(); !d.IsZero() && d.Sub(base) <= ev.T; d = r.pad.Deadline() {
			r.pad.Tick(d)
		}
		e := evdev.InputEvent{Type: ev.Type, Code: ev.Code, Value: ev.Value}
		r.tp.event(&e, base.Add(ev.T))
	}
	r.s.rec.Events = nil
}

func (r *padRig) mouseReports() [][]byte {
	var out [][]byte
	for _, rep := range r.h.take() {
		if rep[0] == hidMouseID {
			out = append(out, rep)
		}
	}
	return out
}

func TestEnginePadSwipeAndButtonCells(t *testing.T) {
	r := newPadRig(t)
	r.s.stroke(fpt{100, 240}, fpt{400, 240}, time.Second)
	r.play()
	sum := 0
	for _, rep := range r.mouseReports() {
		sum += int(int8(rep[2]))
		if rep[1] != 0 {
			t.Fatalf("button pressed while swiping: % x", rep)
		}
	}
	if sum < 250 {
		t.Errorf("cursor moved %d", sum)
	}
	// ボタンのセル（3,1：右クリック）を押しているあいだ、右ボタンを押す
	r.s.wait(time.Second)
	r.s.tap(fpt{700, 240}, 100*time.Millisecond)
	r.play()
	reps := r.mouseReports()
	if len(reps) != 2 || reps[0][1] != mouseRight || reps[1][1] != 0 {
		t.Fatalf("right button cell: % x", reps)
	}
}

// TestEnginePadCancelOnLayerChange は、トラックパッドのドラッグ中にレイヤーが変わると、ボタンを離し、
// そのタッチではもう動かさないことを確かめる（キーのボタンは mouse_test.go）。
func TestEnginePadCancelOnLayerChange(t *testing.T) {
	r := newPadRig(t)
	mouse := r.e.out.mouse
	r.s.tap(fpt{200, 200}, 80*time.Millisecond)
	r.s.wait(100 * time.Millisecond)
	r.s.stroke(fpt{200, 200}, fpt{300, 200}, 300*time.Millisecond)
	ev := r.s.rec.Events
	cut := len(ev) - 12 // 2 回目のタッチの途中
	r.s.rec.Events = ev[:cut]
	r.play()
	if mouse.Held() != mouseLeft || r.pad.State() != padDrag {
		t.Fatalf("should be dragging: held %d, state %v", mouse.Held(), r.pad.State())
	}
	r.e.PressKey(evdev.KEY_S)
	if mouse.Held() != 0 || r.pad.State() != padIdle {
		t.Fatalf("layer change during drag: held %d, state %v", mouse.Held(), r.pad.State())
	}
	r.h.take()
	r.s.rec.Events = ev[cut:]
	r.play()
	for _, rep := range r.mouseReports() {
		if rep[1] != 0 || rep[2] != 0 || rep[3] != 0 {
			t.Fatalf("pad kept working after the layer change: % x", rep)
		}
	}
	r.e.ReleaseKey(evdev.KEY_S)

	// 設定の再読み込みでも、タップの待ちをやめてボタンを離す
	r.s.tap(fpt{200, 200}, 80*time.Millisecond)
	r.play()
	if mouse.Held() != mouseLeft || r.pad.State() != padWait {
		t.Fatalf("after a tap: held %d, state %v", mouse.Held(), r.pad.State())
	}
	r.e.Reload(r.km)
	if mouse.Held() != 0 || r.pad.State() != padIdle {
		t.Fatalf("reload: held %d, state %v", mouse.Held(), r.pad.State())
	}
}

func TestPadConfigValidation(t *testing.T) {
	bad := map[string]string{
		`{ widget: trackpad, key: A }`:               "handles taps itself",
		`{ widget: trackpad, speed: 0 }`:             "speed must be",
		`{ widget: trackpad, scroll_direction: up }`: "scroll_direction",
		`{ widget: trackpad, long_press: left }`:     "long_press",
		`{ widget: clock, speed: 1 }`:                "are for widget: trackpad",
		`{ widget: trackpad, rows: 3 }`:              "cannot be used with widget: trackpad",
		`{ mouse: wheel }`:                           "unknown mouse action",
		`{ mouse: left, key: A }`:                    "exactly one",
		`{ widget: trackpad, smooth: 11 }`:           "smooth must be",
	}
	for cell, want := range bad {
		y := "touch: { min_x: 0, max_x: 100, min_y: 0, max_y: 100 }\nlayers:\n  - name: base\n    touch:\n      cols: 2\n      rows: 2\n      cells:\n        \"0,0\": " + cell + "\n"
		cfg, err := parseConfig([]byte(y))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", cell, err, want)
		}
	}
	y := "touch: { min_x: 0, max_x: 100, min_y: 0, max_y: 100 }\nlayers:\n  - name: base\n    keys:\n      KEY_A: { mouse: scroll_left }\n    touch:\n      cols: 2\n      rows: 2\n      cells:\n        \"0,0\": { widget: trackpad, speed: 2.5, accel: 0, scroll_width: 0, scroll_direction: traditional, settle_ms: 0, smooth: 1, deadzone: 0, min_pressure: 100, tap_ms: 250, tap_move: 20, drag_ms: 0, long_press: right }\n        \"1,0\": { widget: clock, mouse: left }\n"
	cfg, err := parseConfig([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget.Pad
	want := PadParams{Speed: 2.5, ScrollStep: defaultPad.ScrollStep, Smooth: 1, MinPressure: 100, TapTime: 250 * time.Millisecond, TapMove: 20, LongPress: true}
	if *p != want {
		t.Errorf("params:\n got %+v\nwant %+v", *p, want)
	}
	if !km.usesMouse() {
		t.Error("usesMouse")
	}
	// YAML に書き戻して、読み直しても同じ
	b, err := marshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := bytes.NewBuffer(b)
	cfg2, err := parseConfig(out.Bytes())
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if _, _, err := compileKeymap(cfg2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "speed: 2.5") || !strings.Contains(out.String(), "accel: 0") {
		t.Errorf("YAML: %s", out.String())
	}
}
