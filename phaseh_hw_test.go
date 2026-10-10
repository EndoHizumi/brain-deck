package main

import (
	"image"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// フェーズ H の実機のテスト（手書きメモの描き足しの速さと、画面が全体を描いたものと同じか）。本番のサービスを止めてから実行する。
// 設定は、テストの実行ファイルと同じディレクトリの pen-example.yaml（config/pen-example.yaml の写し）。
// メモはテストの一時ディレクトリに書く（本番のデータには触れない）。ペンタブレットのレポートは /dev/null に書く（PC には送らない）。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 120 ./lefthand.test -test.run HWMemo -test.v
func TestHWMemo(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	raw, err := os.ReadFile("pen-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, km := compileText(t, string(raw))
	memos := OpenMemoService(t.TempDir())
	env := func() WidgetEnv {
		return WidgetEnv{Now: time.Now(), TimeSynced: true, Battery: &BatteryInfo{Percent: 80, Source: "test"}, Link: "configured", Memo: memos.Snapshot()}
	}
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	rt.memos = memos
	e.SetWidgets(rt)
	rt.SetEnv(env)
	e.SetRotation(0, 800, 480, nil)
	l := buildLayout(km, e.View())
	d, err := StartDisplay(cfg.Display, l, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	rt.SetScreen(l.W, l.H, d.Poke)
	memos.SetOnChange(d.Poke)
	e.SetOnView(func(v *View) { d.SetLayout(buildLayout(v.km, v)) }, l.Gen)
	// 描き足しの遅れ（タッチの goroutine が頼んでから、描画の goroutine が描き終えるまで）を測る
	var mu sync.Mutex
	var lat, cost []time.Duration
	rt.SetInk(func(f func(*Canvas) image.Rectangle) {
		t0 := time.Now()
		d.Ink(func(cv *Canvas) image.Rectangle {
			t1 := time.Now()
			r := f(cv)
			mu.Lock()
			lat = append(lat, t1.Sub(t0))
			cost = append(cost, time.Since(t1))
			mu.Unlock()
			return r
		})
	})
	enter := func(name string) {
		t.Helper()
		e.mu.Lock()
		e.stack = nil
		for i, ly := range km.Layers {
			if ly.Name == name && i != 0 {
				e.stack = []stackEntry{{layer: i, kind: actTo}}
			}
		}
		e.refresh()
		e.mu.Unlock()
		hwWaitLayout(t, d, e.View().Gen, time.Now(), "enter "+name)
	}
	tp := &touchProc{e: e}
	// write は、物理の画面のドットで、26 ms ごとのサンプルで線を書く（実機のタッチパネルと同じ間隔で、実時間で流す）
	write := func(rot int, pts [][2]int) {
		send := func(typ evdev.EvType, code evdev.EvCode, v int32) {
			tp.event(&evdev.InputEvent{Type: typ, Code: code, Value: v}, time.Now())
		}
		for i, p := range pts {
			x, y := hwRaw(cfg.Touch, rot, 800, 480, p[0], p[1])
			send(evdev.EV_ABS, evdev.ABS_X, x)
			send(evdev.EV_ABS, evdev.ABS_Y, y)
			send(evdev.EV_ABS, evdev.ABS_PRESSURE, 1500)
			if i == 0 {
				send(evdev.EV_KEY, evdev.BTN_TOUCH, 1)
			}
			send(evdev.EV_SYN, evdev.SYN_REPORT, 0)
			time.Sleep(26 * time.Millisecond)
		}
		send(evdev.EV_KEY, evdev.BTN_TOUCH, 0)
		send(evdev.EV_SYN, evdev.SYN_REPORT, 0)
		time.Sleep(defaultPen.Join + 100*time.Millisecond)
	}
	wave := func(x0, y0, n int) [][2]int {
		var out [][2]int
		for i := 0; i < n; i++ {
			out = append(out, [2]int{x0 + i*6, y0 + (i%8)*6})
		}
		return out
	}
	report := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		if len(lat) == 0 {
			t.Errorf("%s: no ink was drawn", what)
			return
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		sort.Slice(cost, func(i, j int) bool { return cost[i] < cost[j] })
		t.Logf("%s: %d ink ops; wait %v (median) %v (90%%) %v (max); draw %v (median) %v (max)", what, len(lat),
			lat[len(lat)/2], lat[len(lat)*9/10], lat[len(lat)-1], cost[len(cost)/2], cost[len(cost)-1])
		if lat[len(lat)*9/10] > 30*time.Millisecond {
			t.Errorf("%s: the ink waits too long", what)
		}
		lat, cost = nil, nil
	}

	enter("memo")
	for i := 0; i < 6; i++ {
		write(0, wave(60+i*20, 120+i*50, 100))
	}
	report("landscape, 6 strokes")
	hwSameAsFull(t, d, env, "memo-landscape")
	// 太いペンと消しゴム（帯のボタンは押した瞬間に働く）
	g := memoGeometry(d.layout.rect(0, 0).Inset(cellGap + textMargin))
	tap := func(rot, b int) {
		r := g.btns[b]
		write(rot, [][2]int{{(r.Min.X + r.Max.X) / 2, (r.Min.Y + r.Max.Y) / 2}})
	}
	tap(0, memoBtnThick)
	write(0, wave(100, 380, 80))
	tap(0, memoBtnErase)
	write(0, [][2]int{{300, 100}, {300, 160}, {300, 220}, {300, 280}, {300, 340}})
	report("thick and eraser")
	hwSameAsFull(t, d, env, "memo-eraser")
	// 戻す：紙を全体から描き直す時間
	t0 := time.Now()
	tap(0, memoBtnUndo)
	t.Logf("undo with %d strokes: %v (including the tap)", len(memos.Snapshot().page.Strokes), time.Since(t0))
	hwSameAsFull(t, d, env, "memo-undo")
	// 縦向き：横向きのページを縮めて出し、書き足す
	e.Rotate(90)
	enter("memo")
	g = memoGeometry(d.layout.rect(0, 0).Inset(cellGap + textMargin))
	tap(90, memoBtnPage)
	for i := 0; i < 3; i++ {
		write(90, wave(40+i*10, 200+i*120, 60))
	}
	report("portrait")
	hwSameAsFull(t, d, env, "memo-portrait")
	e.Rotate(0)
	memos.Flush()
	t.Logf("memos: %+v", memos.List())
}
