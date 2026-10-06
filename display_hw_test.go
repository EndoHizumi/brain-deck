package main

import (
	"os"
	"testing"
	"time"
)

// 実機用のテスト。Brain 上で root として実行する:
//
//	GOOS=linux GOARCH=arm GOARM=5 go test -c -o lefthand.test
//	sudo LEFTHAND_HW_TEST=1 timeout 60 ./lefthand.test -test.run HW -test.v
//
// 専用 VT に切り替えてセルを押下・解除し、そのたびに画面を
// /tmp/lefthand-fb-*.raw に保存してから、元の VT に戻ることを確かめる。
func TestHWDisplay(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	verbose = true
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := buildLayout(km, km.view([]int{0}))
	d, err := StartDisplay(cfg.Display, l)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	orig := d.vt.orig

	waitDrawn := func(i int, on bool) time.Duration {
		t0 := time.Now()
		for time.Since(t0) < 2*time.Second {
			d.drawMu.Lock()
			ok := d.drawn[i] == on
			d.drawMu.Unlock()
			if ok {
				return time.Since(t0)
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("cell %d not redrawn", i)
		return 0
	}
	snap := func(name string) {
		time.Sleep(200 * time.Millisecond) // DRM の遅延書き込みを待つ
		os.WriteFile("/tmp/lefthand-fb-"+name+".raw", d.fb.mem[:d.fb.Info.LineLength*d.fb.Info.YRes], 0o644)
	}
	if !d.vt.IsActive() {
		t.Fatal("dedicated VT is not active")
	}
	snap("idle")

	// 入力側の呼び出しが描画を待たないこと
	t0 := time.Now()
	d.SetPressed(l.Gen, 0, 0, true)
	d.SetPressed(l.Gen, 3, 2, true)
	if el := time.Since(t0); el > 5*time.Millisecond {
		t.Errorf("SetPressed blocked for %v", el)
	}
	t.Logf("press redraw: %v / %v", waitDrawn(0, true), waitDrawn(11, true))
	snap("pressed")
	d.SetPressed(l.Gen, 0, 0, false)
	d.SetPressed(l.Gen, 3, 2, false)
	t.Logf("release redraw: %v / %v", waitDrawn(0, false), waitDrawn(11, false))
	snap("released")

	// press_style ごとに、押す・離すを繰り返す。1 回ごとの描き直しの時間は -v のログ
	// （display: cell ... redraw）に出る。3,0 は右上の札に重なるセル
	cycle := func(gen uint64) {
		for n := 0; n < 5; n++ {
			for _, c := range [][2]int{{1, 1}, {3, 0}} {
				i := c[1]*l.Cols + c[0]
				d.SetPressed(gen, c[0], c[1], true)
				waitDrawn(i, true)
				d.SetPressed(gen, c[0], c[1], false)
				waitDrawn(i, false)
			}
		}
	}
	cycle(l.Gen)
	fl := *l
	fl.Gen, fl.Press = l.Gen+100, pressFill
	d.SetLayout(&fl)
	cycle(fl.Gen)
	bl := *l
	bl.Gen = l.Gen + 101
	d.SetLayout(&bl)
	l = &bl

	// 連打しても入力側は止まらず、最後の状態が描かれる
	t0 = time.Now()
	for i := 0; i < 1000; i++ {
		d.SetPressed(l.Gen, 1, 0, i%2 == 0)
	}
	t.Logf("1000 SetPressed calls: %v", time.Since(t0))
	waitDrawn(1, false)

	// レイヤーを切り替える：SetLayout は待たずに返り、描画側が全体を描き直す
	if len(km.Layers) > 1 {
		nl := buildLayout(km, km.view([]int{0, 1}))
		nl.Gen = l.Gen + 1
		nl.Mode = modeTemp
		t0 = time.Now()
		d.SetLayout(nl)
		if el := time.Since(t0); el > 5*time.Millisecond {
			t.Errorf("SetLayout blocked for %v", el)
		}
		for time.Since(t0) < 2*time.Second {
			d.drawMu.Lock()
			ok := d.layout == nl
			d.drawMu.Unlock()
			if ok {
				break
			}
			time.Sleep(time.Millisecond)
		}
		t.Logf("layer switch redraw: %v", time.Since(t0))
		d.SetPressed(nl.Gen, 0, 0, true)
		d.SetPressed(l.Gen, 1, 0, true) // 古い格子への押下は捨てる
		waitDrawn(0, true)
		snap("layer")
		d.mu.Lock()
		stale := d.want[1]
		d.mu.Unlock()
		if stale {
			t.Error("SetPressed with an old generation changed the new layout")
		}
	}

	d.Close()
	vt0, err := os.ReadFile("/sys/class/tty/tty0/active")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("active VT after close: %s", vt0)
	if want := "tty" + itoa(orig) + "\n"; string(vt0) != want {
		t.Errorf("active VT = %q, want %q", vt0, want)
	}
	if _, err := os.Stat(vtStateFile); err == nil {
		t.Errorf("%s left behind", vtStateFile)
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
