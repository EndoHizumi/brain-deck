package main

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// フェーズ F の実機のテスト（グラフ、電卓、プレゼン用タイマー）。本番のサービスを止めてから実行する。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 120 ./lefthand.test -test.run 'HWGraph|HWCalc|HWPresenter' -test.v

func hwStart(t *testing.T, src string, env func() WidgetEnv) (*Engine, *Display, *WidgetRT, *Keymap) {
	t.Helper()
	cfg, km := compileText(t, src)
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	e.SetWidgets(rt)
	rt.SetEnv(env)
	l := buildLayout(km, e.View())
	d, err := StartDisplay(cfg.Display, l, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	rt.SetScreen(l.W, l.H, d.Poke)
	return e, d, rt, km
}

// hwWaitKey は、セル i（row*cols+col）が want の中身で描かれるまで待ち、かかった時間を返す。
func hwWaitKey(t *testing.T, d *Display, i int, want func() string, what string, t0 time.Time) time.Duration {
	t.Helper()
	for time.Since(t0) < 5*time.Second {
		w := want()
		d.drawMu.Lock()
		ok := d.wkeys[i] == w
		d.drawMu.Unlock()
		if ok {
			return time.Since(t0)
		}
		time.Sleep(500 * time.Microsecond)
	}
	t.Fatalf("%s: not redrawn", what)
	return 0
}

func TestHWGraph(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	verbose = true
	gs := NewGraphService(OpenStore(t.TempDir()))
	gs.saved = make(chan struct{}, 8)
	env := func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true, Graph: gs.Snapshot()} }
	e, d, _, km := hwStart(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: graph, id: cpu, label: CPU, unit: "%", min: 0, max: 100, span: [2, 1] }
        "2,0": { widget: graph, id: mem, label: メモリ, unit: "%", min: 0, max: 100, chart: bar, span: [2, 1] }
        "0,1": { widget: graph, id: temp, label: 温度, unit: ℃, points: 120, span: [2, 2] }
        "2,1": { widget: graph, id: cpu, label: CPU, unit: "%", points: 30 }
        "3,2": B
`, env)
	gs.SetOnChange(d.Poke)
	// 120 個ためる（描き直しは最後の 1 回にまとまる）
	for i := 0; i < graphMaxPoints; i++ {
		v := float64(i%37) * 2.5
		gs.Add(graphVals("cpu", v, "mem", 100-v, "temp", 45+float64(i%11)), time.Now(), true, "hw")
	}
	time.Sleep(time.Second)
	hwSameAsFull(t, d, env, "graph-full")
	w := km.Layers[0].Grid.Cells[cellPos{0, 1}].Widget
	area := widgetArea(cellRect(0, 1, 2, 2, 4, 3, 800, 480), "温度")
	t0 := time.Now()
	for i := 0; i < 20; i++ {
		graphLayout(area, w, env())
	}
	t.Logf("graphLayout (2x2, 120 points): %v", time.Since(t0)/20)
	// 値を 1 つずつ足す。描いている最中にキーのセル（3,2）を押す
	key := func() string { return graphKey(w, env()) }
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		gs.Add(graphVals("cpu", float64(10*i), "mem", float64(5*i), "temp", 50+float64(i)), time.Now(), true, "hw")
		time.Sleep(5 * time.Millisecond)
		p0 := time.Now()
		e.PressTouch(700, 400)
		pe := time.Since(p0)
		e.Release("t")
		if pe > hwInputLimit {
			t.Errorf("PressTouch blocked for %v during a graph redraw", pe)
		}
		t.Logf("add_graph %d: redrawn in %v (key cell during the redraw: %v)", i, hwWaitKey(t, d, 4, key, "add", t0), pe)
		time.Sleep(300 * time.Millisecond)
	}
	hwSameAsFull(t, d, env, "graph-added")
	// SD カードへの保存（間引かずに 1 回書く時間）
	t0 = time.Now()
	gs.Flush()
	<-gs.saved
	t.Logf("graph.json (4 ids x 120): saved in %v", time.Since(t0))
}

func TestHWCalc(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	verbose = true
	cs := NewCalcService(OpenStore(t.TempDir()))
	env := func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true, Calc: cs.Snapshot()} }
	e, d, rt, km := hwStart(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    keys: { KEY_TAB: TAB }
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: calculator, span: [3, 3] }
        "3,0": B
`, env)
	rt.calc = cs
	cs.SetOnChange(d.Poke)
	// 打ち込みは、/dev/hidg0 ではなくファイルに書いて確かめる（PC のフォーカスのある窓に文字が入らないように）
	hidFile := filepath.Join(t.TempDir(), "hid")
	os.WriteFile(hidFile, nil, 0o644)
	st := &State{hid: NewHIDWriter(hidFile), active: map[string]Combo{}}
	cs.SetTyper(func(s string) error { return typeOnHID(st, s) })
	w := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget
	g := newCalcGeom(widgetArea(cellRect(0, 0, 3, 3, 4, 3, 800, 480), ""))
	press := func(in string) time.Duration {
		for i, r := range g.buttons {
			if calcButtons[i/calcCols][i%calcCols] == in {
				c := r.Min.Add(r.Size().Div(2))
				t0 := time.Now()
				e.PressTouch(int32(c.X), int32(c.Y))
				pe := time.Since(t0)
				e.Release("t")
				cs.Sync()
				return pe
			}
		}
		t.Fatalf("no button %q", in)
		return 0
	}
	key := func() string { return calcKey(w, env()) }
	time.Sleep(500 * time.Millisecond)
	hwSameAsFull(t, d, env, "calc-idle")
	// ボタン：1 ÷ 3 × 3 =。押した処理（計算を含む）の時間と、描き直しの時間
	for _, in := range []string{"1", "/", "3", "*", "3", calcEq} {
		t0 := time.Now()
		pe := press(in)
		if pe > hwInputLimit {
			t.Errorf("button %s: PressTouch took %v", in, pe)
		}
		t.Logf("button %s: input %v, redrawn in %v", in, pe, hwWaitKey(t, d, 0, key, in, t0))
		time.Sleep(100 * time.Millisecond)
	}
	if s := cs.Snapshot()["calculator"]; s.Result != "1/1" {
		t.Fatalf("1/3*3 = %v", s.Result)
	}
	// Brain のキー：Q L W 決定（1+2=）。TAB は電卓が使わないので、ふつうのキーとして働く
	for _, k := range []evdev.EvCode{evdev.KEY_Q, evdev.KEY_L, evdev.KEY_W, evdev.KEY_ENTER, evdev.KEY_TAB} {
		t0 := time.Now()
		e.PressKey(k)
		pe := time.Since(t0)
		e.ReleaseKey(k)
		cs.Sync()
		if pe > hwInputLimit {
			t.Errorf("key %v: PressKey took %v", k, pe)
		}
		t.Logf("key %v: %v", k, pe)
		time.Sleep(100 * time.Millisecond)
	}
	if s := cs.Snapshot()["calculator"]; s.Result != "3/1" {
		t.Fatalf("1+2 = %v (%s)", s.Result, s.Msg)
	}
	// 大きな数の掛け算（有理数。ARMv5 には浮動小数点の回路がないが、整数の計算なので速い）
	t0 := time.Now()
	s := newCalcState()
	calcFeed(s, "1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 / 7 * 9 8 7 6 5 4 3 2 1 0 9 8 7 6 5 4 / 3 =")
	t.Logf("big rational: %v (%s)", time.Since(t0), s.Result)
	// 結果を打ち込む：1/3 → 0.333333333333
	press(calcAC)
	for _, in := range []string{"1", "/", "3", calcType} {
		press(in)
	}
	time.Sleep(time.Second)
	got, _ := os.ReadFile(hidFile)
	want := typedReports("0.333333333333")
	if !bytes.Equal(got, want) {
		t.Errorf("HID reports: %d bytes, want %d\n% x", len(got), len(want), got)
	}
	t.Logf("typed: %q", cs.Snapshot()["calculator"].Msg)
	hwSameAsFull(t, d, env, "calc-typed")
}

// typedReports は、文字を 1 つずつ押して離したときのキーボードのレポート（ブートキーボードの 8 バイト）。
func typedReports(s string) []byte {
	var out []byte
	for _, r := range s {
		c, _ := calcTypeKey(r)
		out = append(out, 0, 0, c.Keys[0], 0, 0, 0, 0, 0)
		out = append(out, 0, 0, 0, 0, 0, 0, 0, 0)
	}
	return out
}

func TestHWPresenter(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	verbose = true
	gs := NewGadgetService(OpenStore(t.TempDir()))
	env := func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true, Gadgets: gs.Snapshot()} }
	_, d, rt, km := hwStart(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: presenter, id: talk, duration: 12s, colors: [{ remaining: 8s, color: "#ffc030" }, { remaining: 4s, color: "#ff5858" }], span: [2, 2] }
        "2,0": { widget: clock }
        "3,2": B
`, env)
	rt.gadgets = gs
	gs.SetOnChange(d.Poke)
	// 自動減光：1.5 秒で暗くする設定。タイマーが動いているあいだは暗くしない
	p := dimParamsFor(t, `{dim_after: 5s, dim_level: 1}`)
	p.dimAfter = 1500 * time.Millisecond
	dim := NewDimmer(p)
	defer dim.Close()
	gs.SetOnBusy(dim.SetBusy)
	time.Sleep(2 * time.Second)
	if st := dim.Info().State; st != "dim" {
		t.Errorf("before start: backlight %q, want dim", st)
	}
	w := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget
	key := func() string { return gadgetKey(&CellView{Widget: w}, env()) }
	gs.PresenterToggle("talk")
	time.Sleep(50 * time.Millisecond)
	if st := dim.Info().State; st != "bright" {
		t.Errorf("after start: backlight %q, want bright", st)
	}
	// 1 秒ごとの描き直しの時間（区切りの色が変わるところも含む）を測る
	var worst time.Duration
	for i := 0; i < 15; i++ {
		t0 := time.Now()
		next := presenterView(w, env()).Next
		time.Sleep(time.Until(next) + 2*time.Millisecond)
		d0 := hwWaitKey(t, d, 0, key, "tick", next) // 切り替わる時刻からの遅れ
		worst = max(worst, d0)
		_ = t0
		if i == 6 {
			hwSameAsFull(t, d, env, "presenter-mid")
		}
	}
	t.Logf("presenter redraw after each change: worst %v", worst)
	if st := dim.Info().State; st != "bright" {
		t.Errorf("while running: backlight %q, want bright", st)
	}
	hwSameAsFull(t, d, env, "presenter-over")
	gs.PresenterToggle("talk")
	time.Sleep(2 * time.Second)
	if st := dim.Info().State; st != "dim" {
		t.Errorf("after pause: backlight %q, want dim", st)
	}
	_ = image.Point{}
}
