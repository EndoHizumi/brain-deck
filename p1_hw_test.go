package main

import (
	"image"
	"os"
	"testing"
	"time"
)

// フェーズ P1 の実機のテスト（ホーム画面、状態の帯、画面の回転）。本番のサービスを止めてから実行する。
// 設定は、テストの実行ファイルと同じディレクトリの pda-example.yaml（config/pda-example.yaml の写し）。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 120 ./lefthand.test -test.run HWHomeRotate -test.v

// hwRaw は、向き rot の論理の画面の点 (x, y) に触れたときの、タッチの生の座標（キャリブレーションの逆）。
func hwRaw(tc *TouchConfig, rot, pw, ph, x, y int) (int32, int32) {
	cv := Canvas{pw: pw, ph: ph, rot: rot}
	X, Y := cv.phys(x, y)
	f := func(v, lo, hi int32, n int) int32 { return lo + int32((int64(v)*2+1)*int64(hi-lo)/int64(2*n)) }
	rx, ry := f(int32(X), tc.MinX, tc.MaxX, pw), f(int32(Y), tc.MinY, tc.MaxY, ph)
	if tc.SwapXY {
		rx, ry = ry, rx
	}
	return rx, ry
}

// hwWaitLayout は、画面が gen の格子を描き終えるまで待ち、かかった時間を返す。
func hwWaitLayout(t *testing.T, d *Display, gen uint64, t0 time.Time, what string) time.Duration {
	t.Helper()
	for time.Since(t0) < 5*time.Second {
		d.mu.Lock()
		pending := d.next != nil
		d.mu.Unlock()
		d.drawMu.Lock()
		ok := !pending && d.layout.Gen == gen
		d.drawMu.Unlock()
		if ok {
			return time.Since(t0)
		}
		time.Sleep(500 * time.Microsecond)
	}
	t.Fatalf("%s: not redrawn", what)
	return 0
}

func TestHWHomeRotate(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	verbose = true
	raw, err := os.ReadFile("pda-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, km := compileText(t, string(raw))
	batt := &BatteryInfo{Percent: 80, Source: "test"}
	env := func() WidgetEnv {
		return WidgetEnv{Now: time.Now(), TimeSynced: true, Battery: batt, Link: USBState(), MouseOff: true}
	}
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	e.SetWidgets(rt)
	rt.SetEnv(env)
	rs := NewRotateStore(OpenStore(t.TempDir()))
	e.SetRotation(0, 800, 480, rs)
	l := buildLayout(km, e.View())
	d, err := StartDisplay(cfg.Display, l, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	t.Cleanup(rs.Wait)
	rt.SetScreen(l.W, l.H, d.Poke)
	e.SetOnView(func(v *View) { d.SetLayout(buildLayout(v.km, v)) }, l.Gen)
	pw, ph := d.PhysSize()
	if pw != 800 || ph != 480 {
		t.Fatalf("framebuffer %dx%d", pw, ph)
	}
	h := km.Home
	if e.View().Top != h.Layer {
		t.Fatal("did not start at home")
	}
	time.Sleep(300 * time.Millisecond)
	hwSameAsFull(t, d, env, "p1-home")

	// アプリをタップする（横向き。9 番目の「道具」）
	tap := func(rot, x, y int, what string) {
		t.Helper()
		rx, ry := hwRaw(cfg.Touch, rot, pw, ph, x, y)
		p0 := time.Now()
		hit := e.PressTouch(rx, ry)
		pe := time.Since(p0)
		e.Release("t")
		if pe > hwInputLimit {
			t.Errorf("%s: PressTouch took %v", what, pe)
		}
		t.Logf("%s: raw (%d,%d) -> %+v, input %v, redrawn in %v", what, rx, ry, hit, pe, hwWaitLayout(t, d, e.View().Gen, p0, what))
	}
	center := func(r image.Rectangle) (int, int) { return (r.Min.X + r.Max.X) / 2, (r.Min.Y + r.Max.Y) / 2 }
	g := homeGeometry(gridArea(800, 480, barH), h, false, 0)
	x, y := center(g.slots[8])
	tap(0, x, y, "tap 道具 (landscape)")
	if top := e.View().Top; top != h.Apps[8].Layer {
		t.Fatalf("tapped %q, got layer %q", h.Apps[8].Name, km.Layers[top].Name)
	}
	hwSameAsFull(t, d, env, "p1-tools")

	// HOME のソフトキー（帯の座標は生の座標）
	a := cfg.Touch.SoftAreas[h.SoftKey]
	home := func(what string) {
		p0 := time.Now()
		e.PressTouch((a.X[0]+a.X[1])/2, (a.Y[0]+a.Y[1])/2)
		e.Release("t")
		t.Logf("%s: HOME redrawn in %v", what, hwWaitLayout(t, d, e.View().Gen, p0, what))
		if e.View().Top != h.Layer {
			t.Fatalf("%s: HOME did not go home", what)
		}
	}
	home("landscape")

	// 回す。90 度（縦向き）、180、270、0
	for _, rot := range []int{90, 180, 270, 0} {
		t0 := time.Now()
		e.Rotate(rot)
		t.Logf("rotate %d: redrawn in %v", rot, hwWaitLayout(t, d, e.View().Gen, t0, "rotate"))
		hwSameAsFull(t, d, env, "p1-home-"+map[int]string{0: "0", 90: "90", 180: "180", 270: "270"}[rot])
		if rot == 90 || rot == 270 {
			// 縦向きのアプリ（5 番目の「電卓」）を、本体を回して持ったときの位置でタップする
			W, H := 480, 800
			g := homeGeometry(gridArea(W, H, barH), h, true, 0)
			x, y := center(g.slots[4])
			tap(rot, x, y, "tap 電卓 (portrait)")
			if top := e.View().Top; top != h.Apps[4].Layer {
				t.Fatalf("rot %d: tapped %q, got layer %q", rot, h.Apps[4].Name, km.Layers[top].Name)
			}
			hwSameAsFull(t, d, env, "p1-calc-portrait")
			home("portrait")
		}
	}

	// 状態の帯：電池が変わったら帯だけを描き直す
	batt = &BatteryInfo{Percent: 20, Source: "test"}
	t0 := time.Now()
	d.Poke()
	for time.Since(t0) < 3*time.Second {
		d.drawMu.Lock()
		k := d.barKey
		d.drawMu.Unlock()
		if k == barKey(d.layout, env()) {
			break
		}
		time.Sleep(500 * time.Microsecond)
	}
	t.Logf("status bar (battery): redrawn in %v", time.Since(t0))
	hwSameAsFull(t, d, env, "p1-bar")

	// 描画の時間（全体、帯、アイコン）
	cv := NewCanvas(800, 480, 1600, rgb565, 0)
	ll := buildLayout(km, e.View())
	ll.W, ll.H, ll.Env = 800, 480, env()
	t0 = time.Now()
	drawAll(cv, ll, make([]bool, len(ll.Cells)))
	t.Logf("drawAll home (landscape): %v", time.Since(t0))
	t0 = time.Now()
	for i := 0; i < 10; i++ {
		drawBar(cv, ll, ll.Env)
	}
	t.Logf("drawBar: %v", time.Since(t0)/10)
}
