package main

import (
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// フェーズ P2 の実機のテスト（PC から外したときの使い方）。本番のサービスを止めてから実行する。
// 設定は、テストの実行ファイルと同じディレクトリの pda-example.yaml（config/pda-example.yaml の写し）。
// システムの時刻、RTC、本番のデータ（/var/lib/lefthand）には触れない（Todo と電池の記録はテストの一時ディレクトリ）。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 120 ./lefthand.test -test.run HWOffline -test.v

// hwSameAsFullOverlay は、hwSameAsFull と同じく、裏画面とフレームバッファが、今の状態で全体を描いたもの
// （重ねて描く知らせと欄も）と同じかを確かめる。
func hwSameAsFullOverlay(t *testing.T, d *Display, env func() WidgetEnv, name string) {
	t.Helper()
	time.Sleep(200 * time.Millisecond) // DRM の遅延書き込みを待つ
	cv := d.cv
	ref := NewCanvas(cv.pw, cv.ph, cv.stride, cv.pf, cv.rot)
	d.drawMu.Lock()
	rl := *d.layout
	rl.Env = env()
	drawAll(ref, &rl, d.drawn)
	if d.ovShown != nil {
		drawOverlay(ref, d.ovShown)
	}
	diffCV, diffFB := 0, 0
	for y := 0; y < cv.ph; y++ {
		n := cv.pw * cv.pf.Bpp
		a := ref.pix[y*cv.stride : y*cv.stride+n]
		b := cv.pix[y*cv.stride : y*cv.stride+n]
		f := d.fb.mem[d.fb.base+y*d.fb.Info.LineLength : d.fb.base+y*d.fb.Info.LineLength+n]
		for i := range a {
			if a[i] != b[i] {
				diffCV++
			}
			if a[i] != f[i] {
				diffFB++
			}
		}
	}
	d.drawMu.Unlock()
	os.WriteFile("/tmp/lefthand-fb-"+name+".raw", d.fb.mem[:d.fb.Info.LineLength*d.fb.Info.YRes], 0o644)
	if diffCV != 0 || diffFB != 0 {
		t.Errorf("%s: %d bytes differ from a full redraw (%d on the framebuffer)", name, diffCV, diffFB)
	}
}

// hwWaitOverlay は、画面が o（nil なら消えた）を描き終えるまで待ち、かかった時間を返す。
func hwWaitOverlay(t *testing.T, d *Display, want func(*Overlay) bool, t0 time.Time, what string) time.Duration {
	t.Helper()
	for time.Since(t0) < 5*time.Second {
		d.mu.Lock()
		pending := d.ovNew
		d.mu.Unlock()
		d.drawMu.Lock()
		ok := !pending && want(d.ovShown)
		d.drawMu.Unlock()
		if ok {
			return time.Since(t0)
		}
		time.Sleep(500 * time.Microsecond)
	}
	t.Fatalf("%s: overlay not redrawn", what)
	return 0
}

func TestHWOffline(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)

	// ---- 内蔵 RTC（読むだけ）と USB の 5V ----
	r, err := readRTC()
	if err != nil {
		t.Fatalf("read RTC: %v", err)
	}
	t.Logf("RTC: SECONDS %#x STAT %#08x PERSISTENT0 %#08x -> %s (RTC - system %v)", r.Seconds, r.Stat, r.Persist0,
		r.Time.Format(time.DateTime), r.Time.Sub(r.ReadAt).Round(time.Second))
	time.Sleep(2100 * time.Millisecond)
	r2, _ := readRTC()
	if d := r2.Seconds - r.Seconds; d < 1 || d > 3 {
		t.Errorf("the RTC advanced %d s in 2.1 s", d)
	}
	vbus := findVBus("/sys")
	var vs []int
	var cost []time.Duration
	for i := 0; i < 10; i++ {
		t0 := time.Now()
		vs = append(vs, vbus())
		cost = append(cost, time.Since(t0))
	}
	sort.Slice(cost, func(i, j int) bool { return cost[i] < cost[j] })
	t.Logf("VDD5V %v mV, read %v (median) .. %v (max)", vs, cost[5], cost[9])
	lm := newLinkMonitor(USBState, vbus, time.Now)
	if lm.State() != "configured" || !lm.Online() {
		t.Errorf("link %q (USB %q): the PC should be connected during this test", lm.State(), USBState())
	}

	// ---- 外れたときの画面 ----
	verbose = true
	raw, err := os.ReadFile("pda-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, km := compileText(t, string(raw))
	if !km.OfflineHome {
		t.Fatal("pda-example.yaml has no offline.home")
	}
	link := "configured"
	env := func() WidgetEnv {
		return WidgetEnv{Now: time.Now(), TimeSynced: true, Battery: &BatteryInfo{Percent: 80, Source: "test"}, Link: link, MouseOff: true}
	}
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
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
	e.SetOnView(func(v *View) { d.SetLayout(buildLayout(v.km, v)) }, l.Gen)
	p := NewPrompt(d.SetOverlay)
	e.SetInput(p)
	e.SetNotify(func(m string) { p.Notify(m, noticeWarn) })
	todos := NewTodoService(OpenStore(t.TempDir()))
	p.SetHandlers(func(kind, text string) (string, error) {
		switch kind {
		case inputTodo:
			it, _, err := todos.Add(text, 0, time.Now(), sourceBrain)
			if err != nil {
				return "", inputError(err.Error())
			}
			return "Todo に足しました：" + it.Text, nil
		case inputClock: // システムの時刻は変えない（形を確かめるだけ）
			if _, err := parseClockInput(text, time.Local); err != nil {
				return "", err
			}
			return "（テスト）時刻の形は正しい：" + text, nil
		}
		return "", nil
	}, func(kind string) string {
		if kind == inputClock {
			return time.Now().Format(clockInputForm)
		}
		return ""
	})

	// 左手（base）に入る
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
	enter("base")

	// 外れた：ホーム画面に切り替わり、帯は「PCなし」
	link = "not attached"
	t0 := time.Now()
	e.SetOffline(true)
	in := time.Since(t0)
	t.Logf("offline: SetOffline %v, home redrawn in %v", in, hwWaitLayout(t, d, e.View().Gen, t0, "offline"))
	if e.View().Top != km.Home.Layer || !e.View().Offline {
		t.Fatalf("offline: top %q", km.Layers[e.View().Top].Name)
	}
	hwSameAsFullOverlay(t, d, env, "p2-offline-home")

	// 外れたまま左手に入る：PC に送るセルは「PCなし」と薄く。押すと送らずに知らせる
	enter("base")
	hwSameAsFullOverlay(t, d, env, "p2-offline-base")
	a := cfg.Touch
	x, y := hwRaw(a, 0, 800, 480, 100, 80)
	t0 = time.Now()
	hit := e.PressTouch(x, y)
	pe := time.Since(t0)
	e.Release("t")
	t.Logf("offline key cell: %+v, input %v, notice drawn in %v", hit, pe, hwWaitOverlay(t, d, func(o *Overlay) bool { return o != nil && o.Notice == offlineNoticeText }, t0, "notice"))
	if pe > hwInputLimit {
		t.Errorf("PressTouch took %v", pe)
	}
	hwSameAsFullOverlay(t, d, env, "p2-offline-notice")
	p.ClearNotice("")
	t.Logf("notice cleared (full redraw) in %v", hwWaitOverlay(t, d, func(o *Overlay) bool { return o == nil }, time.Now(), "clear"))
	hwSameAsFullOverlay(t, d, env, "p2-offline-cleared")

	// Todo を Brain のキーボードで足す（Todo のレイヤーの 決定 で開く）
	enter("todo")
	t0 = time.Now()
	e.PressKey(evdev.KEY_ENTER)
	e.ReleaseKey(evdev.KEY_ENTER)
	t.Logf("todo input opened in %v", hwWaitOverlay(t, d, func(o *Overlay) bool { return o != nil && o.Input != nil }, t0, "open"))
	var keyIn, keyDraw []time.Duration
	typeKey := func(c evdev.EvCode) {
		k0 := time.Now()
		e.PressKey(c)
		keyIn = append(keyIn, time.Since(k0))
		e.ReleaseKey(c)
		want := p.Overlay()
		keyDraw = append(keyDraw, hwWaitOverlay(t, d, func(o *Overlay) bool { return o != nil && o.Input != nil && o.Input.Text == want.Input.Text }, k0, "key"))
	}
	e.PressKey(evdev.KEY_LEFTSHIFT)
	typeKey(evdev.KEY_B)
	e.ReleaseKey(evdev.KEY_LEFTSHIFT)
	for _, c := range []evdev.EvCode{evdev.KEY_U, evdev.KEY_Y, evdev.KEY_SPACE, evdev.KEY_M, evdev.KEY_I, evdev.KEY_L, evdev.KEY_K, evdev.KEY_SPACE, evdev.KEY_2} {
		typeKey(c)
	}
	sort.Slice(keyIn, func(i, j int) bool { return keyIn[i] < keyIn[j] })
	sort.Slice(keyDraw, func(i, j int) bool { return keyDraw[i] < keyDraw[j] })
	t.Logf("typing: input %v (median) .. %v (max), drawn %v (median) .. %v (max)", keyIn[len(keyIn)/2], keyIn[len(keyIn)-1], keyDraw[len(keyDraw)/2], keyDraw[len(keyDraw)-1])
	if keyIn[len(keyIn)-1] > hwInputLimit {
		t.Errorf("a key took %v", keyIn[len(keyIn)-1])
	}
	hwSameAsFullOverlay(t, d, env, "p2-todo-input")
	t0 = time.Now()
	e.PressKey(evdev.KEY_ENTER)
	e.ReleaseKey(evdev.KEY_ENTER)
	t.Logf("todo committed: input closed and notice drawn in %v", hwWaitOverlay(t, d, func(o *Overlay) bool {
		return o != nil && o.Input == nil && strings.HasPrefix(o.Notice, "Todo に足しました")
	}, t0, "commit"))
	if l := todos.Snapshot(); len(l.Items) != 1 || l.Items[0].Text != "Buy milk 2" || l.Items[0].Source != sourceBrain {
		t.Fatalf("todo: %+v", l.Items)
	}
	hwSameAsFullOverlay(t, d, env, "p2-todo-added")

	// 時刻の欄（形だけ確かめる。システムの時刻は変えない）
	t0 = time.Now()
	p.Open(inputClock)
	hwWaitOverlay(t, d, func(o *Overlay) bool { return o != nil && o.Input != nil }, t0, "clock")
	hwSameAsFullOverlay(t, d, env, "p2-clock-input")
	e.PressKey(evdev.KEY_ENTER)
	e.ReleaseKey(evdev.KEY_ENTER)
	if p.Active() {
		t.Fatalf("clock input: %+v", p.Overlay().Input)
	}
	p.ClearNotice("")
	hwWaitOverlay(t, d, func(o *Overlay) bool { return o == nil }, time.Now(), "clear")

	// つなぎ直した：外れてから入ったレイヤー（Todo）のまま。帯は「PC」に戻る
	link = "configured"
	t0 = time.Now()
	e.SetOffline(false)
	t.Logf("online: redrawn in %v", hwWaitLayout(t, d, e.View().Gen, t0, "online"))
	if km.Layers[e.View().Top].Name != "todo" || e.View().Offline {
		t.Fatalf("online: top %q", km.Layers[e.View().Top].Name)
	}
	hwSameAsFullOverlay(t, d, env, "p2-online")

	// ---- 電池の係（USB につないでいるので、何もしない。記録は「start」の 1 行） ----
	dir := t.TempDir()
	g := NewBatteryGuard(batteryParams{warn: 20, critical: 10, poweroff: 5, log: true}, dir)
	g.poweroff = func() error { t.Error("poweroff called"); return nil }
	bs := NewBatteryService("/sys")
	defer close(bs.stop)
	bi := bs.Info()
	if bi == nil {
		t.Fatal("no battery")
	}
	g.Sample(*bi)
	logText, _ := readBatteryLog(dir, 0)
	t.Logf("battery: %+v, log:\n%s", *bi, logText)
	if !bi.Plugged || !strings.HasSuffix(strings.TrimSpace(logText), ",start") || g.PoweringOff() {
		t.Errorf("battery guard on USB: plugged %v, log %q", bi.Plugged, logText)
	}
}
