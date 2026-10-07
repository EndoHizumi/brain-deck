package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
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
		rnd: rand.New(rand.NewSource(1)), every: 26 * time.Millisecond, noise: 4, jump: 120, // 実機と同じく 26 ms ごと
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
	// 300 ドットから、settle（80 ms で 16 ドット）と離す直前（2 サンプル）を除いた 260 ドットほど × speed 1.2 × (1 + 2 × 0.2 程度)
	if r.MoveX < 360 || r.MoveX > 520 || abs(r.MoveY) > 15 {
		t.Errorf("slow swipe: %+v", r)
	}
}

func TestPadFastSwipeAccelerates(t *testing.T) {
	// 記録のなぞりと同じくらい：400 ドットを、ゆっくり（200 ドット/秒）と速く（1000 ドット/秒、記録の速いなぞりの最大）
	slow, fast := newSynth(), newSynth()
	slow.stroke(fpt{50, 240}, fpt{450, 240}, 2000*time.Millisecond)
	fast.stroke(fpt{50, 240}, fpt{450, 240}, 400*time.Millisecond)
	rs, rf := replayDefault(t, slow, nil).Stats, replayDefault(t, fast, nil).Stats
	noAccel := replayDefault(t, fast, func(p *PadParams) { p.Accel = 0 }).Stats
	t.Logf("slow %d, fast %d, fast without accel %d", rs.MoveX, rf.MoveX, noAccel.MoveX)
	// 速いと、触れた直後と離す直前に捨てる距離が長いが、それでも遠くへ動く
	if rf.MoveX <= rs.MoveX {
		t.Errorf("fast swipe should go farther: slow %d, fast %d", rs.MoveX, rf.MoveX)
	}
	// 1000 ドット/秒で、speed × (1 + 2 × 1) = 3 倍。速さを測り始めるまでの分があるので 2 倍以上
	if rf.MoveX < 2*noAccel.MoveX {
		t.Errorf("accel: fast %d, without accel %d", rf.MoveX, noAccel.MoveX)
	}
	if noAccel.MoveX > 400*12/10 {
		t.Errorf("accel 0 should move at most 400*speed: %d", noAccel.MoveX)
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
	// ボタンは、離してから padBounce あとに押し、drag_ms あとに離す
	var press, release time.Duration
	for _, o := range res.Ops {
		if o.Kind == "press" && press == 0 {
			press = o.T
		}
		if o.Kind == "release" && release == 0 {
			release = o.T
		}
	}
	if want := defaultPad.DragGap - padBounce; release-press < want-time.Millisecond || release-press > want+time.Millisecond {
		t.Errorf("click held %v, want %v", release-press, want)
	}
	// drag_ms 0 なら、押してすぐ離す
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
	s.wait(150 * time.Millisecond) // 記録のダブルタップの間は 122〜180 ms
	s.tap(fpt{300, 200}, 80*time.Millisecond)
	s.wait(time.Second)
	if r := replayDefault(t, s, nil).Stats; r.DoubleClicks != 1 || r.Clicks != 0 || r.Drags != 0 {
		t.Errorf("double tap: %+v", r)
	}

	d := newSynth()
	d.tap(fpt{200, 200}, 80*time.Millisecond)
	d.wait(150 * time.Millisecond)
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
	s.stroke(fpt{560, 100}, fpt{560, 340}, 800*time.Millisecond) // 帯（右から 96 ドット）を下へ 240 ドット
	r := replayDefault(t, s, nil).Stats
	// natural：指を下へ → 上へスクロール（正）。240/24 = 10 段。settle と離す直前のぶん（3 段ほど）減る
	if r.Wheel < 6 || r.Wheel > 10 || r.PathX+r.PathY != 0 || r.Clicks != 0 {
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
	r = replayDefault(t, s, func(p *PadParams) { p.Settle = 0; p.Smooth = 1; p.Deadzone = 0; p.MinPressure = 0 }).Stats
	// 跳ねた最初のサンプルを使うので、上へ大きく動く
	if r.MoveY > -5 {
		t.Errorf("without settle the first jump should show up: %+v", r)
	}
}

// TestPadBounceAndDropouts は、触れ方の乱れ（記録で見たもの）を確かめる。
func TestPadBounceAndDropouts(t *testing.T) {
	// 着地の跳ね：短く触れて 40 ms 離れ、また触れて離す（合わせて 200 ms）。1 回のクリック（ダブルクリックにしない）
	s := newSynth()
	s.tap(fpt{300, 200}, 26*time.Millisecond)
	s.wait(40 * time.Millisecond)
	s.tap(fpt{300, 200}, 26*time.Millisecond)
	s.wait(time.Second)
	if r := replayDefault(t, s, nil).Stats; r.Clicks != 1 || r.DoubleClicks != 0 || r.Resumed != 1 {
		t.Errorf("bounce on landing: %+v", r)
	}
	// 跳ねてから動かす：ドラッグにも、クリックにもしない（カーソルを動かすだけ）
	s = newSynth()
	s.tap(fpt{200, 200}, 52*time.Millisecond)
	s.wait(60 * time.Millisecond)
	s.stroke(fpt{200, 200}, fpt{400, 200}, 600*time.Millisecond)
	s.wait(time.Second)
	if r := replayDefault(t, s, nil).Stats; r.Clicks+r.Drags+r.Taps != 0 || r.MoveX < 150 {
		t.Errorf("bounce then swipe: %+v", r)
	}
	// タップのあと、離した瞬間の弱い接触（押す強さ 435 が 1 サンプル）：使わない。ダブルクリックにしない
	s = newSynth()
	s.tap(fpt{300, 200}, 104*time.Millisecond)
	s.wait(150 * time.Millisecond)
	s.sample(fpt{300, 200}, 435, false)
	s.ev(evdev.EV_KEY, evdev.BTN_TOUCH, 1)
	s.ev(evdev.EV_SYN, evdev.SYN_REPORT, 0)
	s.t += 26 * time.Millisecond
	s.ev(evdev.EV_KEY, evdev.BTN_TOUCH, 0)
	s.ev(evdev.EV_SYN, evdev.SYN_REPORT, 0)
	s.wait(time.Second)
	if r := replayDefault(t, s, nil).Stats; r.Clicks != 1 || r.DoubleClicks != 0 || r.Ignored != 1 {
		t.Errorf("weak contact after a tap: %+v", r)
	}
	// ドラッグの途中で 100 ms 離れたことになる：左ボタンを離さず、触れ直したところから続ける
	s = newSynth()
	s.tap(fpt{200, 200}, 78*time.Millisecond)
	s.wait(150 * time.Millisecond)
	s.stroke(fpt{200, 200}, fpt{300, 200}, 500*time.Millisecond)
	s.wait(100 * time.Millisecond)
	s.stroke(fpt{320, 200}, fpt{420, 200}, 500*time.Millisecond)
	res := replayDefault(t, s, nil)
	if r := res.Stats; r.Drags != 1 || r.Resumed != 1 || r.Clicks != 0 || r.MoveX < 150 {
		t.Errorf("dropout while dragging: %+v", r)
	}
	releases := 0
	for _, o := range res.Ops {
		if o.Kind == "release" {
			releases++
		}
		if o.Kind == "move" && o.Buttons != mouseLeft {
			t.Fatalf("moved without the button while dragging: %+v", o)
		}
	}
	if last := res.Ops[len(res.Ops)-1]; releases != 1 || last.Kind != "release" {
		t.Errorf("the drag should end once, at the end: %d releases, last %+v", releases, last)
	}
	// 離れていた時間が padLiftGrace より長ければ、ドラッグは終わる
	s = newSynth()
	s.tap(fpt{200, 200}, 78*time.Millisecond)
	s.wait(150 * time.Millisecond)
	s.stroke(fpt{200, 200}, fpt{300, 200}, 500*time.Millisecond)
	s.wait(padLiftGrace + 50*time.Millisecond)
	s.stroke(fpt{320, 200}, fpt{420, 200}, 500*time.Millisecond)
	if r := replayDefault(t, s, nil).Stats; r.Drags != 1 || r.Resumed != 0 {
		t.Errorf("long lift while dragging: %+v", r)
	}
	// 帯のタップはクリックしない。帯をなぞる途中の切れ目では、スクロールのまま
	s = newSynth()
	s.tap(fpt{560, 200}, 78*time.Millisecond)
	s.wait(time.Second)
	s.stroke(fpt{560, 100}, fpt{560, 220}, 400*time.Millisecond)
	s.wait(80 * time.Millisecond)
	s.stroke(fpt{560, 260}, fpt{560, 400}, 400*time.Millisecond)
	if r := replayDefault(t, s, nil).Stats; r.Clicks+r.Taps != 0 || r.PathX+r.PathY != 0 || r.Wheel < 6 || r.Resumed != 1 {
		t.Errorf("scroll band: %+v", r)
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

// testdata/touch/*.touch は、2026-10-08 に Brain で lefthand -record-touch を使って記録したもの（tools/record-touch.sh）。
// 見出しの gesture ごとに、既定の設定で期待どおりに判定できるかを確かめる。
// 記録の手順は README の「トラックパッドの調整」、既定値の根拠は docs/config.md の「既定値の根拠」。
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
			if rec.Header.Gesture == "scroll" {
				// 記録では、帯（x 528〜600）の手前（x 436〜508）をなぞっていた。帯をなぞったことにするため、右へ 90 ドットずらす
				rec = shiftRecording(rec, 90)
			}
			res := replayTouch(rec, &defaultPad)
			var buf bytes.Buffer
			printReplay(&buf, f, rec, res, false)
			t.Log(buf.String())
			checkGesture(t, rec, res)
		})
	}
}

// checkGesture は、記録の手順（tools/record-touch.sh）の動きに合った判定かを確かめる。
func checkGesture(t *testing.T, rec *TouchRecording, r ReplayResult) {
	s := r.Stats
	in := 0
	for _, x := range r.Touches {
		if x.InCell {
			in++
		}
	}
	switch strings.TrimSuffix(rec.Header.Gesture, "-2") {
	case "slow", "fast":
		// なぞる：クリックしない。カーソルは動く
		if s.Clicks+s.DoubleClicks+s.Drags+s.RightClicks != 0 {
			t.Errorf("swipes made clicks: %+v", s)
		}
		if s.PathX+s.PathY < 50*in {
			t.Errorf("swipes moved too little: %+v", s)
		}
	case "tap":
		// 1 回ずつ間をあけたタップ：すべてクリック、まったく動かない
		if s.Clicks != in || s.DoubleClicks != 0 || s.Drags != 0 {
			t.Errorf("taps: %d touches, %+v", in, s)
		}
		if s.PathX+s.PathY != 0 {
			t.Errorf("taps moved the cursor: %+v", s)
		}
	case "doubletap":
		if s.DoubleClicks*2 != in || s.Clicks != 0 || s.Drags != 0 || s.PathX+s.PathY != 0 {
			t.Errorf("double taps: %d touches, %+v", in, s)
		}
	case "tapdrag":
		checkTapDrag(t, rec, s)
	case "scroll":
		// 帯：ホイールだけ。カーソルは動かさず、クリックもしない。上から下へ 4 回、下から上へ 4 回
		if s.PathX+s.PathY != 0 || s.Clicks+s.DoubleClicks+s.Drags != 0 {
			t.Errorf("scroll band moved the cursor or clicked: %+v", s)
		}
		// natural：指を下へ動かすと、ホイールは正（上へスクロール）。前半（9 秒まで）は下へ、後半は上へなぞった
		var down, up int
		for _, o := range r.Ops {
			if o.Kind != "wheel" {
				continue
			}
			if o.T < 9*time.Second {
				down += o.V
			} else {
				up += o.V
			}
		}
		if down < 4*3 || up > -4*3 {
			t.Errorf("scroll: wheel %+d while swiping down, %+d while swiping up (want 3 or more per swipe)", down, up)
		}
	case "hold":
		// 指を 4〜5 秒止める：クリックしない。止めた指の流れで、1 回あたり 4 カウントより動かさない
		if s.Clicks+s.RightClicks+s.Taps != 0 || s.PathX+s.PathY > 4*in {
			t.Errorf("resting finger: %+v", s)
		}
	case "light":
		// ごく軽く触れる：ダブルクリックやドラッグにならない。離した 82 ms あとの弱い接触（押す強さ 435）は使わない
		if s.DoubleClicks+s.Drags != 0 || s.Ignored == 0 || s.Clicks == 0 {
			t.Errorf("light touches: %+v", s)
		}
	default:
		t.Errorf("unknown gesture %q", rec.Header.Gesture)
	}
}

// checkTapDrag は、tapdrag の記録（タップしてから触れて動かす、を 7 回。2 回目のタップは 1.1 秒押していて、タップにならない）を確かめる。
// 記録では、タップから次に触れるまで 0.40〜0.85 秒あいていて、既定の drag_ms（0.3 秒）より長い。
func checkTapDrag(t *testing.T, rec *TouchRecording, s PadStats) {
	const pairs = 6 // タップ（250 ms より短い）のあとに、触れて動かした組
	// 記録のままなら、クリックしてからカーソルを動かす（ドラッグにしない）
	if s.Drags != 0 || s.Clicks != pairs || s.DoubleClicks != 0 || s.MoveX < 100*pairs {
		t.Errorf("tap and drag as recorded (gaps longer than drag_ms): %+v", s)
	}
	// 間を詰めて（150 ms。ダブルタップと同じくらい）再生すると、ドラッグになる
	quick := replayTouch(tightenGaps(rec, 150*time.Millisecond), &defaultPad)
	if q := quick.Stats; q.Drags != pairs || q.Clicks != 0 || q.DoubleClicks != 0 {
		t.Errorf("tap and drag with 150 ms gaps: %+v", q)
	}
	for _, o := range quick.Ops {
		if o.Kind == "move" && o.Buttons != mouseLeft && o.Buttons != 0 {
			t.Fatalf("unexpected buttons while moving: %+v", o)
		}
	}
	if last := quick.Ops[len(quick.Ops)-1]; last.Buttons != 0 {
		t.Errorf("left button still held at the end: %+v", last)
	}
	// drag_ms を記録の間より長くすれば、記録のままでもドラッグになる
	p := defaultPad
	p.DragGap = 900 * time.Millisecond
	if l := replayTouch(rec, &p).Stats; l.Drags != pairs {
		t.Errorf("tap and drag with drag_ms 900: %+v", l)
	}
}

// touchSpans は、記録の中のタッチ（BTN_TOUCH の 1 から 0）の時刻を返す。
func touchSpans(rec *TouchRecording) [][2]time.Duration {
	var out [][2]time.Duration
	for _, e := range rec.Events {
		if e.Type == evdev.EV_KEY && e.Code == evdev.BTN_TOUCH {
			if e.Value == 1 {
				out = append(out, [2]time.Duration{e.T, -1})
			} else if len(out) > 0 {
				out[len(out)-1][1] = e.T
			}
		}
	}
	return out
}

// tightenGaps は、短いタッチ（250 ms 以下）のあと gap より長くあいて次に触れたところを、gap に詰めた記録を返す。
func tightenGaps(rec *TouchRecording, gap time.Duration) *TouchRecording {
	sp := touchSpans(rec)
	out := &TouchRecording{Header: rec.Header, Events: append([]RecEvent(nil), rec.Events...)}
	var shift time.Duration
	j := 0
	for i := range out.Events {
		e := &out.Events[i]
		for j < len(sp) && e.T >= sp[j][0] {
			if j > 0 && sp[j-1][1] >= 0 && sp[j-1][1]-sp[j-1][0] <= 250*time.Millisecond && sp[j][0]-sp[j-1][1] > gap {
				shift += sp[j][0] - sp[j-1][1] - gap
			}
			j++
		}
		e.T -= shift
	}
	return out
}

// shiftRecording は、x を dx ドット右へずらした記録を返す。
func shiftRecording(rec *TouchRecording, dx float64) *TouchRecording {
	h := rec.Header
	raw := int32(math.Round(dx * float64(h.MaxX-h.MinX) / float64(h.Screen[0])))
	out := &TouchRecording{Header: h, Events: append([]RecEvent(nil), rec.Events...)}
	for i := range out.Events {
		if e := &out.Events[i]; e.Type == evdev.EV_ABS && e.Code == evdev.ABS_X {
			e.Value += raw
		}
	}
	return out
}

// TestPadRecordedScrollOffBand は、帯の手前をなぞった記録（そのまま）で、帯の外でも縦のなぞりがドラッグなどにならないかを見る。
// なぞる途中の切れ目（28〜204 ms）で、短いかけらがタップに見えることがある（docs/config.md の「調整の余地」）。
func TestPadRecordedScrollOffBand(t *testing.T) {
	rec, err := LoadRecording("testdata/touch/scroll.touch")
	if err != nil {
		t.Skip(err)
	}
	s := replayTouch(rec, &defaultPad).Stats
	t.Logf("%+v", s)
	// 跳ねをまとめる前は、ドラッグ 3、ダブルクリック 2 になっていた
	if s.DoubleClicks != 0 || s.Drags > 2 || s.Clicks > 1 || s.Wheel != 0 {
		t.Errorf("vertical swipes off the band: %+v", s)
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
	r.s.wait(150 * time.Millisecond)
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
	r.pad.Tick(time.Unix(1_000_000, 0).Add(r.s.t + padBounce)) // 跳ねを待ってから、左ボタンを押す
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

// padDefaultsJSON は、トラックパッドの既定値を設定の項目の名前で書いたもの。GUI（gui/src/model.ts の PAD_DEFAULTS）と比べる。
func padDefaultsJSON() map[string]any {
	p := defaultPad
	dir, lp := scrollWheel, longPressNone
	if p.Natural {
		dir = scrollNatural
	}
	if p.LongPress {
		lp = longPressR
	}
	return map[string]any{
		"speed": p.Speed, "accel": p.Accel, "scroll_width": p.ScrollWidth, "scroll_direction": dir, "scroll_step": p.ScrollStep,
		"settle_ms": p.Settle.Milliseconds(), "smooth": p.Smooth, "deadzone": p.Deadzone, "min_pressure": p.MinPressure,
		"tap_ms": p.TapTime.Milliseconds(), "tap_move": p.TapMove, "drag_ms": p.DragGap.Milliseconds(), "long_press": lp,
	}
}

// TestPadDefaultsFixture は、gui/test/fixtures/pad-defaults.json が Go の既定値と同じことを確かめる。
// 既定値を変えたら LEFTHAND_UPDATE_PADDEFAULTS=1 go test -run PadDefaults で書き直し、gui/src/model.ts の PAD_DEFAULTS も直す。
func TestPadDefaultsFixture(t *testing.T) {
	const path = "gui/test/fixtures/pad-defaults.json"
	want, _ := json.MarshalIndent(padDefaultsJSON(), "", "  ")
	want = append(want, '\n')
	if os.Getenv("LEFTHAND_UPDATE_PADDEFAULTS") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale (LEFTHAND_UPDATE_PADDEFAULTS=1 go test -run PadDefaults):\n%s", path, want)
	}
}
