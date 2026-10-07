package main

import (
	"fmt"
	"image"
	"os"
	"runtime"
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
	d, err := StartDisplay(cfg.Display, l, nil, nil)
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

// hwInputLimit は、入力の処理（PressTouch）が描画を待っていないとみなす上限。描き直しは 20〜40 ms かかる。
// CPU が 1 つなので、描画のスレッドが動いている最中に押すと、OS の切り替えまで数 ms 待つことがある
// （フェーズ 3 の版でも 8 ms があった）。それより長ければ、描画を待っている。
const hwInputLimit = 10 * time.Millisecond

// hwProcs は、デーモン（main）と同じく、CPU が 1 つでも GOMAXPROCS を 2 にする。
// 1 のままだと、描画の goroutine が動いているあいだ、入力の goroutine が Go のスケジューラで待たされる。
func hwProcs(t *testing.T) {
	old := runtime.GOMAXPROCS(max(runtime.GOMAXPROCS(0), 2))
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
}

// hwSameAsFull は、裏画面とフレームバッファが、今の状態で全体を描いたものと同じかを確かめる。
// フレームバッファは /tmp/lefthand-fb-<name>.raw に残す。
func hwSameAsFull(t *testing.T, d *Display, env func() WidgetEnv, name string) {
	t.Helper()
	time.Sleep(200 * time.Millisecond) // DRM の遅延書き込みを待つ
	cv := d.cv
	ref := NewCanvas(cv.pw, cv.ph, cv.stride, cv.pf, cv.rot)
	d.drawMu.Lock()
	rl := *d.layout
	rl.Env = env()
	drawAll(ref, &rl, d.drawn)
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

// TestHWCalendar は、カレンダーのセルを実機の画面に出す。予定の始まりと終わりでの描き直し、▲▼ のページ送り、
// PC から予定を送ったときの描き直しの時間を測り、画面が全体を描いたものと同じになることを確かめる。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 60 ./lefthand.test -test.run HWCalendar -test.v
func TestHWCalendar(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	verbose = true
	cfg, km := compileText(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: calendar, span: [2, 3], label: 予定 }
        "2,0": { widget: calendar, span: [2, 1], calendars: [家] }
        "3,2": B
`)
	cs := NewCalendarService(OpenStore(t.TempDir()))
	now := time.Now()
	at := func(d time.Duration) *time.Time { x := now.Add(d).Truncate(time.Second); return &x }
	ev := func(title string, a, b time.Duration) CalEvent {
		return CalEvent{Title: title, Start: at(a), End: at(b)}
	}
	// 6 秒後に始まって 9 秒後に終わる予定で、時刻での描き直しを確かめる（日付の変わり目の近くでは、今日に収まらないことがある）
	work := []CalEvent{ev("朝会", -3*time.Hour, -150*time.Minute), ev("すぐ始まる会議", 6*time.Second, 9*time.Second),
		ev("設計レビュー", -10*time.Minute, 50*time.Minute), ev("1on1", 2*time.Hour, 150*time.Minute), {Title: "社内イベント", Day: now.Format(calDayLayout)}}
	for i := 0; i < 6; i++ {
		work = append(work, ev(fmt.Sprintf("あとの予定 %d", i+1), time.Duration(3+i)*time.Hour, time.Duration(3+i)*time.Hour+30*time.Minute))
	}
	home := []CalEvent{ev("歯医者", 90*time.Minute, 2*time.Hour), {Title: "燃えないごみ", Day: now.AddDate(0, 0, 1).Format(calDayLayout)}}
	fetched := at(-time.Minute)
	if _, _, err := cs.Set(CalendarRequest{Calendars: []Calendar{{Name: "仕事", FetchedAt: fetched, Events: work},
		{Name: "家", FetchedAt: fetched, Events: home}}}, now); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	e.SetWidgets(rt)
	env := func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true, Calendar: cs.Snapshot()} }
	rt.SetEnv(env)
	l := buildLayout(km, e.View())
	d, err := StartDisplay(cfg.Display, l, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cs.SetOnChange(d.Poke)
	rt.SetScreen(l.W, l.H, d.Poke)
	w := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget
	waitKey := func(what string, t0 time.Time) time.Duration {
		t.Helper()
		for time.Since(t0) < 3*time.Second {
			want := calKey(w, env())
			d.drawMu.Lock()
			ok := d.wkeys[0] == want
			d.drawMu.Unlock()
			if ok {
				return time.Since(t0)
			}
			time.Sleep(500 * time.Microsecond)
		}
		t.Fatalf("%s: not redrawn", what)
		return 0
	}
	hwSameAsFull(t, d, env, "cal-idle")
	// 予定が始まる時刻と終わる時刻に、描画の goroutine が自分で起きて描き直す
	for _, c := range []struct {
		name string
		at   time.Time
	}{{"start", *work[1].Start}, {"end", *work[1].End}} {
		time.Sleep(time.Until(c.at))
		t.Logf("event %s: redrawn %v after the time", c.name, waitKey(c.name, c.at))
		hwSameAsFull(t, d, env, "cal-"+c.name)
	}
	cell := cellRect(0, 0, 2, 3, 4, 3, l.W, l.H)
	cl := calLayout(widgetArea(cell, "予定"), w, env())
	if cl.geom.pages < 2 {
		t.Fatalf("want 2 or more pages, got %d", cl.geom.pages)
	}
	pt := func(r image.Rectangle) (int32, int32) { return int32(r.Min.X + r.Dx()/2), int32(r.Min.Y + r.Dy()/2) }
	for _, nav := range []image.Rectangle{cl.geom.down, cl.geom.up} {
		x, y := pt(nav)
		t0 := time.Now()
		e.PressTouch(x, y)
		if el := time.Since(t0); el > hwInputLimit {
			t.Errorf("PressTouch blocked for %v", el)
		}
		t.Logf("page turn: drawn in %v", waitKey("page", t0))
		e.Release("t")
		waitKey("released", time.Now())
	}
	hwSameAsFull(t, d, env, "cal-paged")
	// PC から送り直したとき（set_calendar と同じ）。家の取得に失敗し、前の予定を残す
	t0 := time.Now()
	cs.Set(CalendarRequest{Calendars: []Calendar{{Name: "仕事", FetchedAt: at(0), Events: work[:4]}, {Name: "家", Error: "HTTP 404"}}}, time.Now())
	t.Logf("set_calendar: redrawn in %v", waitKey("set", t0))
	hwSameAsFull(t, d, env, "cal-set")
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// TestHWTodo は、Todo のセルを実機の画面で押す。長押しの黄色、完了の切り替え、ページ送りの描き直しの時間を測り、
// 画面（フレームバッファ）が、同じ状態で全体を描いたものと同じになることを確かめる。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 60 ./lefthand.test -test.run HWTodo -test.v
func TestHWTodo(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	verbose = true
	cfg, km := compileText(t, todoConfig)
	ts := NewTodoService(OpenStore(t.TempDir()))
	for _, s := range []string{"牛乳を買う", "PR #42 のレビュー", "brain-deck の README を書き直す（cron の例と、終了コードの表も）", "歯医者の予約",
		"ゴミ出し", "SD カードを交換する", "請求書を送る", "カーネルの設定の差分を見直す", "傘を返す", "Brain の電池を充電"} {
		ts.Add(s, -1, time.Now(), "test")
	}
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(ts)
	e.SetWidgets(rt)
	env := func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true, Todo: ts.Snapshot()} }
	l := buildLayout(km, e.View())
	d, err := StartDisplay(cfg.Display, l, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ts.SetOnChange(d.Poke)
	rt.SetScreen(l.W, l.H, d.Poke)
	w := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget

	// waitKey は、セル 0,0 が、今の状態で描き直されるまで待ち、かかった時間を返す
	waitKey := func(what string, t0 time.Time) time.Duration {
		t.Helper()
		for time.Since(t0) < 2*time.Second {
			want := todoKey(w, env())
			d.drawMu.Lock()
			ok := d.wkeys[0] == want
			d.drawMu.Unlock()
			if ok {
				return time.Since(t0)
			}
			time.Sleep(500 * time.Microsecond)
		}
		t.Fatalf("%s: not redrawn", what)
		return 0
	}
	same := func(what string) { t.Helper(); hwSameAsFull(t, d, env, "todo-"+what) }
	pt := func(r image.Rectangle) (int32, int32) { return int32(r.Min.X + r.Dx()/2), int32(r.Min.Y + r.Dy()/2) }
	geom := func() todoGeom {
		return todoGeometry(widgetArea(cellRect(0, 0, 2, 3, 4, 3, l.W, l.H), todoCaption("Todo", ts.Snapshot().Items)), len(ts.Snapshot().Items), 0)
	}
	same("idle")

	for n := 0; n < 5; n++ {
		g := geom()
		x, y := pt(g.rows[1])
		t0 := time.Now()
		h := e.PressTouch(x, y)
		el := time.Since(t0)
		t.Logf("PressTouch returned in %v", el)
		if el > hwInputLimit {
			t.Errorf("PressTouch blocked for %v", el)
		}
		if !h.Own {
			t.Fatalf("hit = %+v", h)
		}
		t.Logf("held: yellow row drawn in %v", waitKey("held", t0))
		if n == 0 {
			same("held")
		}
		t1 := time.Now().Add(todoHold)
		time.Sleep(todoHold + 20*time.Millisecond)
		t.Logf("toggled: redrawn %v after the 0.5 s", waitKey("toggled", t1))
		e.Release("t")
		if n == 0 {
			same("toggled")
		}
	}
	g := geom()
	x, y := pt(g.down)
	t0 := time.Now()
	e.PressTouch(x, y)
	t.Logf("▼: page 2 drawn in %v", waitKey("page", t0))
	e.Release("t")
	waitKey("page released", time.Now())
	same("page2")
	if page, _, _ := w.pager.view(time.Now(), w.PageReset, 0); page != 1 {
		t.Errorf("page = %d", page)
	}
	// PC から書き換えたとき（todo_add と同じ）
	t0 = time.Now()
	ts.Add("PC から足した項目", 0, time.Now(), "test")
	t.Logf("added from the PC: redrawn in %v", waitKey("added", t0))
	same("added")
}
