package main

import (
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// フェーズ P2：PC から外したときの使い方（つながりの見分け、休みにするもの、文字を入れる欄、内蔵 RTC、電池）

func TestClassifyLink(t *testing.T) {
	cases := []struct {
		state string
		vbus  int
		want  string
	}{
		{"configured", 0, "configured"},
		{"suspended", -1, "suspended"},
		{"", 4900, ""},
		{"not attached", 4900, linkPower},
		{"powered", 4900, linkPower},
		{"default", 120, "not attached"},
		{"not attached", -1, "not attached"},
		{"powered", -1, "powered"},
	}
	for _, c := range cases {
		read := false
		got := classifyLink(c.state, func() int { read = true; return c.vbus })
		if got != c.want {
			t.Errorf("classifyLink(%q, %d) = %q, want %q", c.state, c.vbus, got, c.want)
		}
		if linkOnline(c.state) && read {
			t.Errorf("classifyLink(%q) read the 5V", c.state)
		}
	}
	for s, want := range map[string]bool{"": true, "configured": true, "suspended": true, linkPower: false, "not attached": false} {
		if linkOnline(s) != want {
			t.Errorf("linkOnline(%q) != %v", s, want)
		}
	}
}

// 5V があるまま configured でなくなったら（USB の付け直し）、linkOfflineAfter 続くまで外れたとみなさない。
// 5V がなくなったら、すぐに外れたとみなす。つながったら、すぐにつながったとみなす。
func TestLinkMonitorDebounce(t *testing.T) {
	state, vbus := "configured", 4900
	now := time.Unix(1000, 0)
	m := newLinkMonitor(func() string { return state }, func() int { return vbus }, func() time.Time { return now })
	var got []bool
	m.OnOnline(func(on bool) { got = append(got, on) })
	step := func(d time.Duration) { now = now.Add(d); m.poll() }

	state = "not attached"
	step(2 * time.Second)
	if m.State() != "configured" || !m.Online() {
		t.Fatalf("rebind counted as unplugged at once: %q", m.State())
	}
	step(4 * time.Second)
	state = "configured" // 付け直し終わった
	step(2 * time.Second)
	if len(got) != 0 || m.State() != "configured" {
		t.Fatalf("a short rebind: events %v state %q", got, m.State())
	}
	state = "not attached" // 充電器だけ
	for i := 0; i < 5; i++ {
		step(2 * time.Second)
	}
	if m.State() != linkPower || m.Online() || len(got) != 1 || got[0] {
		t.Fatalf("power only: state %q events %v", m.State(), got)
	}
	vbus = 0 // ケーブルを抜いた
	step(2 * time.Second)
	if m.State() != "not attached" || len(got) != 1 {
		t.Fatalf("unplugged: state %q events %v", m.State(), got)
	}
	state, vbus = "configured", 4900
	step(2 * time.Second)
	if !m.Online() || len(got) != 2 || !got[1] {
		t.Fatalf("plugged in: state %q events %v", m.State(), got)
	}
	state, vbus = "not attached", 0 // 5V がなければ、すぐ
	step(2 * time.Second)
	if m.Online() || len(got) != 3 {
		t.Fatalf("pulled: state %q events %v", m.State(), got)
	}
}

const p2YAML = `
touch:
  min_x: 0
  max_x: 400
  min_y: 0
  max_y: 300
  soft_areas:
    home: { x: [3950, 4095], y: [3000, 4095] }
layers:
  - name: base
    keys:
      KEY_A: B
      KEY_Q: { input: todo }
    touch:
      cols: 2
      rows: 2
      cells:
        "0,0": C
        "1,0": { layer_to: tools }
        "0,1": { widget: text, id: build }
        "1,1": { widget: clock }
  - name: tools
    touch:
      cols: 2
      rows: 1
      cells:
        "0,0": { mouse: left }
        "1,0": { input: clock }
home:
  apps:
    - { name: 左手, layer: base }
    - { name: 道具, layer: tools }
offline: { home: true }
`

func TestOfflineEngine(t *testing.T) {
	e, reports := testEngine(t, p2YAML)
	km := e.km
	var notices []string
	e.SetNotify(func(m string) { notices = append(notices, m) })
	// 左手（base）に入り、キーを押したまま外れる
	e.mu.Lock()
	e.stack = nil
	e.refresh()
	e.mu.Unlock()
	reports()
	e.PressKey(evdev.KEY_A)
	expectReports(t, reports(), report(0, hidB))
	e.SetOffline(true)
	expectReports(t, reports(), report(0)) // 押していたキーを離す
	v := e.View()
	if !v.Offline || v.Top != km.Home.Layer {
		t.Fatalf("offline: view offline %v top %d", v.Offline, v.Top)
	}
	e.ReleaseKey(evdev.KEY_A)
	// 外れているあいだ：キーもセルも送らず、知らせる（続けて押したら 1 回）
	e.PressKey(evdev.KEY_A)
	e.ReleaseKey(evdev.KEY_A)
	e.PressKey(evdev.KEY_A)
	e.ReleaseKey(evdev.KEY_A)
	if r := reports(); len(r) != 0 {
		t.Fatalf("sent while offline: % x", r)
	}
	if len(notices) != 1 || notices[0] != offlineNoticeText {
		t.Fatalf("notices %q", notices)
	}
	// つなぎ直したら、外れる前のレイヤー（base）に戻る
	e.SetOffline(false)
	if v := e.View(); v.Offline || v.Top != 0 {
		t.Fatalf("online again: offline %v top %d", v.Offline, v.Top)
	}
	e.PressKey(evdev.KEY_A)
	e.ReleaseKey(evdev.KEY_A)
	expectReports(t, reports(), report(0, hidB), report(0))

	// 外れているあいだにほかのアプリに入ったら、つなぎ直してもそのまま
	e.SetOffline(true)
	e.mu.Lock()
	e.press("x", &Action{Kind: actTo, Layer: 1})
	delete(e.down, "x")
	e.mu.Unlock()
	e.SetOffline(false)
	if km.Layers[e.View().Top].Name != "tools" {
		t.Fatalf("moved while offline, then back online: top %q", km.Layers[e.View().Top].Name)
	}
	// layout：PC に送るセルと、PC から受け取るタイルに印。外れているあいだだけ休みと描く
	e.SetOffline(true)
	e.mu.Lock()
	e.stack = nil
	e.refresh()
	e.mu.Unlock()
	l := buildLayout(km, e.View())
	if !l.Offline || !l.Cells[0].NeedsPC || l.Cells[1].NeedsPC || !l.Cells[2].NeedsPC || l.Cells[3].NeedsPC {
		t.Fatalf("layout: offline %v needsPC %v %v %v %v", l.Offline, l.Cells[0].NeedsPC, l.Cells[1].NeedsPC, l.Cells[2].NeedsPC, l.Cells[3].NeedsPC)
	}
}

// offline.home を書かなければ、外れてもレイヤーは変わらない。home がなければ警告する。
func TestOfflineNoHome(t *testing.T) {
	src := strings.Replace(p2YAML, "offline: { home: true }", "", 1)
	e, _ := testEngine(t, src)
	e.mu.Lock()
	e.stack = nil
	e.refresh()
	e.mu.Unlock()
	e.SetOffline(true)
	if e.View().Top != 0 {
		t.Fatal("went home without offline.home")
	}
	noHome := strings.Replace(p2YAML, "home:\n  apps:\n    - { name: 左手, layer: base }\n    - { name: 道具, layer: tools }\n", "", 1)
	noHome = strings.Replace(noHome, `"1,0": { input: clock }`, `"1,0": { layer_to: base }`, 1)
	_, warns, err := compileYAML(t, noHome)
	if err != nil || !strings.Contains(strings.Join(warns, "\n"), "offline.home has no effect") {
		t.Fatalf("warns %q err %v", warns, err)
	}
}

// 外れているあいだ、PC に送るセルは「PCなし」と薄く描く（つながっているときと違う絵になる）。
func TestRenderOfflineCells(t *testing.T) {
	km, _, err := compileYAML(t, p2YAML)
	if err != nil {
		t.Fatal(err)
	}
	draw := func(off bool) *Canvas {
		e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
		e.mu.Lock()
		e.stack, e.offline = nil, off
		e.refresh()
		e.mu.Unlock()
		l := buildLayout(km, e.View())
		cv := NewCanvas(800, 480, 1600, rgb565, 0)
		l.W, l.H, l.Env = 800, 480, WidgetEnv{Now: time.Date(2026, 10, 10, 9, 41, 0, 0, time.Local), TimeSynced: true}
		drawAll(cv, l, make([]bool, len(l.Cells)))
		return cv
	}
	on, off := draw(false), draw(true)
	diff := func(r image.Rectangle) bool {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				if on.at(x, y) != off.at(x, y) {
					return true
				}
			}
		}
		return false
	}
	if !diff(image.Rect(0, 0, 400, 240)) || !diff(image.Rect(0, 240, 400, 480)) {
		t.Error("the key cell or the text tile looks the same offline")
	}
	if diff(image.Rect(400, 0, 800, 240)) || diff(image.Rect(400, 240, 800, 480)) {
		t.Error("the layer cell or the clock changed offline")
	}
}

// ---------- 文字を入れる欄 ----------

type promptRec struct {
	mu   sync.Mutex
	last *Overlay
	n    int
}

func (r *promptRec) show(o *Overlay) {
	r.mu.Lock()
	r.last, r.n = o, r.n+1
	r.mu.Unlock()
}

func TestPromptTyping(t *testing.T) {
	rec := &promptRec{}
	p := NewPrompt(rec.show)
	p.after = func(time.Duration, func()) {} // 知らせは消さない
	var got []string
	p.SetHandlers(func(kind, text string) (string, error) {
		got = append(got, kind+":"+text)
		if text == "bad" {
			return "", inputError("だめ")
		}
		return "ok " + text, nil
	}, func(kind string) string { return map[string]string{inputClock: "2026-10-10 09:41"}[kind] })
	press := func(codes ...evdev.EvCode) {
		for _, c := range codes {
			p.Key(c, 1)
			p.Key(c, 0)
		}
	}
	p.Open(inputTodo)
	if !p.Active() || rec.last == nil || rec.last.Input == nil || rec.last.Input.Text != "" {
		t.Fatalf("open: %+v", rec.last)
	}
	p.Key(evdev.KEY_LEFTSHIFT, 1)
	press(evdev.KEY_H, evdev.KEY_1) // H!
	p.Key(evdev.KEY_LEFTSHIFT, 0)
	press(evdev.KEY_I, evdev.KEY_SPACE, evdev.KEY_COMMA, evdev.KEY_BACKSPACE, evdev.KEY_X)
	press(evdev.KEY_LEFT, evdev.KEY_LEFT, evdev.KEY_DELETE) // H!i x → H!ix
	if iv := p.Overlay().Input; iv.Text != "H!ix" || iv.Cursor != 3 {
		t.Fatalf("text %q cursor %d", iv.Text, iv.Cursor)
	}
	p.Key(evdev.KEY_BACKSPACE, 2) // 繰り返し
	press(evdev.KEY_DOWN, evdev.KEY_ENTER)
	if p.Active() || len(got) != 1 || got[0] != "todo:H!x" {
		t.Fatalf("commit: active %v got %q", p.Active(), got)
	}
	if o := p.Overlay(); o == nil || o.Notice != "ok H!x" || o.Input != nil {
		t.Fatalf("after commit: %+v", o)
	}
	// 確定できなければ、欄は開いたまま理由を出す。戻るで閉じる
	p.Open(inputTodo)
	press(evdev.KEY_B, evdev.KEY_A, evdev.KEY_D, evdev.KEY_ENTER)
	if !p.Active() || p.Overlay().Input.Err != "だめ" {
		t.Fatalf("error: %+v", p.Overlay().Input)
	}
	press(evdev.KEY_ESC)
	if p.Active() {
		t.Fatal("Esc did not close")
	}
	p.Open(inputClock)
	if iv := p.Overlay().Input; iv.Text != "2026-10-10 09:41" || iv.Cursor != 16 {
		t.Fatalf("clock initial %+v", iv)
	}
}

func TestParseClockInput(t *testing.T) {
	loc := time.FixedZone("JST", 9*3600)
	for in, want := range map[string]string{
		"2026-10-10 09:41":     "2026-10-10T09:41:00+09:00",
		" 2026-10-10   09:41 ": "2026-10-10T09:41:00+09:00",
		"2026-10-10 09:41:30":  "2026-10-10T09:41:30+09:00",
		"2026-1-2 9:05":        "2026-01-02T09:05:00+09:00",
		"2026-1-2 09:05":       "2026-01-02T09:05:00+09:00",
		"10:41":                "",
	} {
		got, err := parseClockInput(in, loc)
		if want == "" {
			if err == nil {
				t.Errorf("%q: no error", in)
			}
			continue
		}
		if err != nil || got.Format(time.RFC3339) != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
}

// input のセルとキーで欄が開き、開いているあいだのキーは PC に送らない。
func TestEnginePromptRoutesKeys(t *testing.T) {
	e, reports := testEngine(t, p2YAML)
	p := NewPrompt(nil)
	p.after = func(time.Duration, func()) {}
	var added []string
	p.SetHandlers(func(kind, text string) (string, error) { added = append(added, text); return "", nil }, nil)
	e.SetInput(p)
	e.mu.Lock()
	e.stack = nil
	e.refresh()
	e.mu.Unlock()
	reports()
	e.PressKey(evdev.KEY_Q) // input: todo
	e.ReleaseKey(evdev.KEY_Q)
	if !p.Active() {
		t.Fatal("KEY_Q did not open the input")
	}
	for _, c := range []evdev.EvCode{evdev.KEY_A, evdev.KEY_ENTER} {
		e.PressKey(c)
		e.ReleaseKey(c)
	}
	if r := reports(); len(r) != 0 || len(added) != 1 || added[0] != "a" {
		t.Fatalf("reports % x added %q", r, added)
	}
	e.PressKey(evdev.KEY_A)
	e.ReleaseKey(evdev.KEY_A)
	expectReports(t, reports(), report(0, hidB), report(0))
}

func TestDrawOverlay(t *testing.T) {
	for _, rot := range []int{0, 90} {
		cv := NewCanvas(800, 480, 1600, rgb565, rot)
		o := &Overlay{Notice: "電池が少なくなりました（20%）", Level: noticeWarn,
			Input: &InputView{Title: inputTitle[inputTodo], Text: strings.Repeat("abc", 30), Cursor: 90, Hint: inputHint[inputTodo]}}
		r := drawOverlay(cv, o)
		n, in := overlayRects(o, cv.W, cv.H)
		if r != n.Union(in) || !r.In(image.Rect(0, 0, cv.W, cv.H)) || n.Max.Y > in.Min.Y || in.Max.Y != cv.H-ovMargin {
			t.Fatalf("rot %d: rects %v %v %v", rot, r, n, in)
		}
		if cv.at(in.Min.X, in.Min.Y) == cv.at(in.Min.X+ovBorder+1, in.Min.Y+ovBorder+1) {
			t.Fatalf("rot %d: no border", rot)
		}
	}
	if overlayRect(nil, 800, 480) != (image.Rectangle{}) {
		t.Fatal("nil overlay has a rect")
	}
}

// ---------- 内蔵 RTC ----------

func TestRTCConversion(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	// 2026-10-10 に PW-SH2 で読んだ値
	if got := rtcToTime(0x3255c7fb, jst); got.Format(time.DateTime) != "2026-10-05 02:29:15" || got.Location() != jst {
		t.Fatalf("rtcToTime = %v", got)
	}
	for _, s := range []string{"2000-01-01T00:00:00+09:00", "2026-10-10T20:44:27+09:00", "2099-12-31T23:59:59+09:00"} {
		tm, _ := time.Parse(time.RFC3339, s)
		sec, err := timeToRTC(tm, jst)
		if err != nil || !rtcToTime(sec, jst).Equal(tm) {
			t.Errorf("%s: %d %v -> %v", s, sec, err, rtcToTime(sec, jst))
		}
	}
	// ローカル時刻で数える：UTC で渡しても日本の壁時計
	sec, _ := timeToRTC(time.Date(2000, 1, 1, 0, 0, 10, 0, time.UTC), jst)
	if sec != 9*3600+10 {
		t.Fatalf("UTC input: %d", sec)
	}
	if _, err := timeToRTC(time.Date(1999, 12, 31, 0, 0, 0, 0, jst), jst); err == nil {
		t.Fatal("before 2000 accepted")
	}
}

// fakeClock は、システムの時刻と RTC を差し替える。
type fakeClock struct {
	set     []time.Time
	rtc     RTCReading
	rtcErr  error
	written []time.Time
}

func useFakeClock(t *testing.T) *fakeClock {
	f := &fakeClock{}
	oldSet, oldNTP, oldBoot, oldRead, oldWrite := setSystemClock, ntpSynced, readBootID, readRTC, writeRTC
	setSystemClock = func(tm time.Time) error { f.set = append(f.set, tm); return nil }
	ntpSynced = func() bool { return false }
	readBootID = func() string { return "boot-2" }
	readRTC = func() (RTCReading, error) { return f.rtc, f.rtcErr }
	writeRTC = func(tm time.Time) error { f.written = append(f.written, tm); return nil }
	t.Cleanup(func() {
		setSystemClock, ntpSynced, readBootID, readRTC, writeRTC = oldSet, oldNTP, oldBoot, oldRead, oldWrite
	})
	return f
}

func TestBootFromRTC(t *testing.T) {
	now := time.Now()
	t.Run("ahead", func(t *testing.T) {
		f := useFakeClock(t)
		f.rtc = RTCReading{Time: now.Add(3 * time.Hour), ReadAt: now, Stat: rtcPresent}
		ts := NewTimeService(nil) // clock.json は書かない（書くのは別の goroutine で、TempDir の後片付けと重なる）
		ts.BootFromRTC(rtcRead)
		if len(f.set) != 1 || f.set[0].Sub(now) < 3*time.Hour-time.Second || !ts.Synced() || ts.Info().BootSource != sourceRTC {
			t.Fatalf("set %v synced %v info %+v", f.set, ts.Synced(), ts.Info())
		}
		if len(f.written) != 0 {
			t.Fatal("wrote back the RTC's own time")
		}
		if !strings.HasPrefix(ts.RTCInfo().Boot, "set the clock") {
			t.Fatalf("boot %q", ts.RTCInfo().Boot)
		}
	})
	t.Run("behind", func(t *testing.T) {
		f := useFakeClock(t)
		f.rtc = RTCReading{Time: now.Add(-5 * 24 * time.Hour), ReadAt: now, Stat: rtcPresent}
		ts := NewTimeService(OpenStore(t.TempDir()))
		ts.BootFromRTC(rtcRead)
		if len(f.set) != 0 || ts.Synced() || !strings.Contains(ts.RTCInfo().Boot, "before the clock restored at boot") {
			t.Fatalf("set %v boot %q", f.set, ts.RTCInfo().Boot)
		}
		if in := ts.RTCInfo(); !in.Readable || in.OffsetMS > -4*24*3600*1000 {
			t.Fatalf("info %+v", in)
		}
	})
	t.Run("behind the last known time", func(t *testing.T) {
		f := useFakeClock(t)
		dir := t.TempDir()
		known := now.Add(2 * time.Hour).UTC()
		OpenStore(dir).Save(clockStoreName, clockRecord{BootID: "boot-1", Source: "gui", SetAt: now.Add(-time.Hour), LastKnown: &known})
		f.rtc = RTCReading{Time: now.Add(time.Hour), ReadAt: now, Stat: rtcPresent}
		ts := NewTimeService(OpenStore(dir))
		ts.BootFromRTC(rtcRead)
		// RTC は使わず、最後に分かっていた時刻まで進める（未設定のまま）
		if len(f.set) != 1 || !f.set[0].Equal(known) || ts.Synced() {
			t.Fatalf("set %v synced %v", f.set, ts.Synced())
		}
		if ts.Info().LastPCSet == nil {
			t.Fatal("an old clock.json lost the PC sync time")
		}
		if e := ts.Estimate(); !e.Equal(known) {
			t.Fatalf("estimate %v", e)
		}
	})
	t.Run("unreadable and off", func(t *testing.T) {
		f := useFakeClock(t)
		f.rtcErr = errors.New("permission denied")
		ts := NewTimeService(OpenStore(t.TempDir()))
		ts.BootFromRTC(rtcRead)
		if len(f.set) != 0 || !strings.Contains(ts.RTCInfo().Boot, "permission denied") || ts.RTCInfo().Readable {
			t.Fatalf("boot %+v", ts.RTCInfo())
		}
		ts2 := NewTimeService(OpenStore(t.TempDir()))
		ts2.BootFromRTC(rtcOff)
		if in := ts2.RTCInfo(); in.Mode != rtcOff || in.Readable || len(f.set) != 0 {
			t.Fatalf("off: %+v", in)
		}
	})
	t.Run("already set", func(t *testing.T) {
		f := useFakeClock(t)
		dir := t.TempDir()
		OpenStore(dir).Save(clockStoreName, clockRecord{BootID: "boot-2", Source: "gui", SetAt: now})
		f.rtc = RTCReading{Time: now.Add(time.Hour), ReadAt: now, Stat: rtcPresent}
		ts := NewTimeService(OpenStore(dir))
		ts.BootFromRTC(rtcRead)
		if len(f.set) != 0 || !strings.HasPrefix(ts.RTCInfo().Boot, "not needed") {
			t.Fatalf("set %v boot %q", f.set, ts.RTCInfo().Boot)
		}
	})
}

// clock.rtc: write のときだけ、合わせた時刻を RTC に書き戻す。状態の帯の「同期」は PC から合わせたときだけ。
func TestRTCWriteBack(t *testing.T) {
	f := useFakeClock(t)
	f.rtcErr = errNoRTC
	ts := NewTimeService(nil)
	ts.BootFromRTC(rtcRead)
	if _, err := ts.Set(time.Now().Add(time.Minute), "gui"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if len(f.written) != 0 {
		t.Fatal("wrote in read mode")
	}
	pc := ts.Info().LastPCSet
	ts.SetRTCMode(rtcWrite)
	if _, err := ts.Set(time.Now().Add(2*time.Minute), sourceBrain); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && ts.RTCInfo().LastWrite == ""; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.written) != 1 || ts.RTCInfo().LastWrite != "ok" {
		t.Fatalf("written %v info %+v", f.written, ts.RTCInfo())
	}
	if in := ts.Info(); in.LastPCSet == nil || !in.LastPCSet.Equal(*pc) || in.LastSource != sourceBrain {
		t.Fatalf("a Brain-keyboard set changed the PC sync time: %+v", in)
	}
}

// ---------- 電池 ----------

func TestBatteryConfig(t *testing.T) {
	for src, want := range map[string]string{
		"battery: { warn: 120 }\n":                                    "warn must be 0..100",
		"battery: { warn: 10, critical: 20 }\n":                       "must not be more than battery.warn",
		"battery: { critical: 5, poweroff: 5 }\n":                     "must be less than battery.critical",
		"battery: { poweroff: 60, critical: 0, warn: 0 }\n":           "poweroff must be 0..50",
		"battery: { curve: [[3500, 0]] }\n":                           "at least 2 points",
		"battery: { curve: [[3500, 0], [3400, 100]] }\n":              "must go up",
		"clock: { rtc: sometimes }\n":                                 "clock.rtc must be",
		"backlight: { dim_after: 1m, offline: { dim_after: 1s } }\n":  "offline: dim_after must be",
		"backlight: { dim_after: 1m, offline: { off_after: 30s } }\n": "offline: off_after (30s) must be longer",
	} {
		_, _, err := compileYAML(t, src+"layers:\n  - name: base\n")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", src, err)
		}
	}
	cfg, err := parseConfig([]byte("battery: { warn: 30, critical: 15, poweroff: 5, log: false, curve: [[3500, 0], [4100, 100]] }\nlayers:\n  - name: base\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Battery.compile()
	if err != nil || p.warn != 30 || p.critical != 15 || p.poweroff != 5 || p.log || curvePercent(p.curve, 3800) != 50 {
		t.Fatalf("%+v %v", p, err)
	}
	d, _ := (*BatteryConfig)(nil).compile()
	if d.warn != 20 || d.critical != 10 || d.poweroff != 0 || !d.log {
		t.Fatalf("defaults %+v", d)
	}
}

type guardRec struct {
	notices []string
	cleared []string
	powered int
	timers  []func()
	wakes   int
}

func newTestGuard(t *testing.T, p batteryParams) (*BatteryGuard, *guardRec, *time.Time) {
	r := &guardRec{}
	g := NewBatteryGuard(p, t.TempDir())
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.Local)
	g.now = func() time.Time { return now }
	g.after = func(d time.Duration, f func()) func() bool {
		r.timers = append(r.timers, f)
		i := len(r.timers) - 1
		return func() bool { r.timers[i] = nil; return true }
	}
	g.notify = func(m string, _ int, _ time.Duration) { r.notices = append(r.notices, m) }
	g.clear = func(m string) { r.cleared = append(r.cleared, m) }
	g.wake = func() { r.wakes++ }
	g.poweroff = func() error { r.powered++; return nil }
	g.backlight = func() int { return 7 }
	return g, r, &now
}

func TestBatteryGuard(t *testing.T) {
	g, r, now := newTestGuard(t, batteryParams{warn: 20, critical: 10, poweroff: 5, log: true})
	feed := func(pct int, plugged bool, samples int) {
		*now = now.Add(30 * time.Second)
		g.Sample(BatteryInfo{Percent: pct, Plugged: plugged, Source: "lradc", Samples: samples, VoltageMV: 3600, RawMV: 3610, VBusMV: 30})
	}
	feed(100, true, 5)
	feed(18, false, 1) // 外した直後は判断しない
	if len(r.notices) != 0 {
		t.Fatalf("judged right after unplugging: %q", r.notices)
	}
	feed(18, false, 3)
	feed(17, false, 4)
	feed(9, false, 5)
	feed(9, false, 5)
	if len(r.notices) != 2 || !strings.Contains(r.notices[0], "少なくなりました（18%）") || !strings.Contains(r.notices[1], "ほとんどありません") || r.wakes != 1 {
		t.Fatalf("notices %q wakes %d", r.notices, r.wakes)
	}
	feed(5, false, 5)
	feed(4, false, 5)
	if len(r.timers) != 0 {
		t.Fatal("counting down after 2 samples")
	}
	feed(4, false, 5)
	if len(r.timers) != 1 || !g.PoweringOff() || !strings.Contains(r.notices[len(r.notices)-1], "21:05:30 に電源を切ります") {
		t.Fatalf("countdown: timers %d notices %q", len(r.timers), r.notices)
	}
	// キーで取りやめる。10 分は切らない
	g.Activity()
	if g.PoweringOff() || r.timers[0] != nil || len(r.cleared) != 1 {
		t.Fatalf("cancel: %v %d", g.PoweringOff(), len(r.cleared))
	}
	for i := 0; i < 5; i++ {
		feed(4, false, 5)
	}
	if len(r.timers) != 1 {
		t.Fatal("counted down again within 10 minutes")
	}
	for i := 0; i < 20; i++ {
		feed(4, false, 5)
	}
	if len(r.timers) != 2 {
		t.Fatalf("no countdown after 10 minutes: %d", len(r.timers))
	}
	r.timers[1]()
	if r.powered != 1 {
		t.Fatal("did not power off")
	}
	// USB につないだら取りやめる
	feed(4, false, 5)
	feed(4, false, 5)
	feed(4, false, 5)
	if !g.PoweringOff() {
		t.Fatal("no countdown")
	}
	feed(4, true, 5)
	if g.PoweringOff() || r.timers[2] != nil {
		t.Fatal("plugging in did not cancel")
	}
	log, err := readBatteryLog(filepath.Dir(g.logPath), 0)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if lines[0] != batteryLogHeader || !strings.HasSuffix(lines[1], ",true,7,start") || !strings.Contains(log, ",3610,3600,30,18,false,7,unplugged\n") ||
		!strings.Contains(log, ",warn\n") || !strings.Contains(log, ",poweroff-countdown\n") || !strings.Contains(log, ",poweroff\n") ||
		!strings.HasSuffix(log, ",true,7,plugged\n") {
		t.Fatalf("log:\n%s", log)
	}
	if tail, _ := readBatteryLog(filepath.Dir(g.logPath), 2); strings.Count(tail, "\n") != 3 || !strings.HasPrefix(tail, batteryLogHeader) {
		t.Fatalf("tail:\n%s", tail)
	}
}

// 電源を切るのは、知らせてから 60 秒のあいだに USB につながなかったときだけ。設定で切らないなら、知らせるだけ。
func TestBatteryGuardNoPoweroff(t *testing.T) {
	g, r, now := newTestGuard(t, batteryParams{warn: 20, critical: 10, log: true})
	for i := 0; i < 10; i++ {
		*now = now.Add(30 * time.Second)
		g.Sample(BatteryInfo{Percent: 1, Source: "lradc", Samples: 5})
	}
	if len(r.timers) != 0 || len(r.notices) != 1 {
		t.Fatalf("timers %d notices %q", len(r.timers), r.notices)
	}
	if _, err := os.Stat(g.logPath); err != nil {
		t.Fatal("no log")
	}
	g2, _, _ := newTestGuard(t, batteryParams{})
	g2.Sample(BatteryInfo{Percent: 1, Source: "lradc", Samples: 5})
	if _, err := os.Stat(g2.logPath); err == nil {
		t.Fatal("logged with log: false")
	}
}

func TestBacklightOffline(t *testing.T) {
	fb := newFakeBacklight(t, 7)
	p := dimParamsFor(t, "{ dim_after: 1m, level: 6, offline: { dim_after: 5s, dim_level: 2 } }")
	if p.offline == nil || p.offline.dimAfter != 5*time.Second || p.offline.dimLevel != 2 || p.offline.level != 6 || p.dimAfter != time.Minute {
		t.Fatalf("%+v %+v", p, p.offline)
	}
	d := NewDimmer(p)
	defer d.Close()
	fb.waitLevel(6)
	d.SetOffline(true)
	if in := d.Info(); !in.Offline || in.State != "bright" {
		t.Fatalf("offline info %+v", in)
	}
	d.mu.Lock()
	d.setLocked(dimDim)
	d.mu.Unlock()
	fb.waitLevel(2)
	d.SetOffline(false) // つないだら明るくして、ふだんの設定に戻る
	fb.waitLevel(6)
	if d.Info().Offline {
		t.Fatal("still offline")
	}
	none := dimParamsFor(t, "{ dim_after: 1m }")
	if none.offline != nil {
		t.Fatal("offline params without offline")
	}
}
