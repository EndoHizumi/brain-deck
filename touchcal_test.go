package main

import (
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// 本番の設定の touch（swap なし。y は上下が逆）
var calTestTC = &TouchConfig{MinX: 226, MaxX: 3938, MinY: 3803, MaxY: 384}

// trueRaw は、「本当の」パネル：画面のドットを生の座標にする。設定の min と max から、少しずれて傾いたもの。
func trueRaw(X, Y float64) (float64, float64) {
	x := 226 + X*(3938-226)/800 + 0.02*Y*(3938-226)/800 + 40 // 右に 8 ドットほど、下へ行くほど右へ
	y := 3803 + Y*(384-3803)/480 - 0.01*X*(384-3803)/480 - 30
	return x, y
}

func TestFitAffine(t *testing.T) {
	var ps []calPair
	for _, tg := range calTargets(9) {
		x, y := trueRaw(tg[0], tg[1])
		ps = append(ps, calPair{SX: tg[0], SY: tg[1], RX: x, RY: y})
	}
	a, err := fitAffine(ps)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := calErrors(ps, affineFunc(a)); e.Max > 1e-6 {
		t.Errorf("fit error %v", e)
	}
	// 1 点を大きく外すと、その点を見つける
	bad := append([]calPair(nil), ps...)
	bad[3].RX += 300
	if i, d := calOutlier(bad, calOutlierLimit); i != 3 || d < 40 {
		t.Errorf("outlier = %d (%.1f)", i, d)
	}
	if i, _ := calOutlier(ps, calOutlierLimit); i != -1 {
		t.Errorf("no outlier expected, got %d", i)
	}
	// ふつうの揺れ（数ドット）では、5 点でもやり直さない
	noise := [][2]float64{{3, -2}, {-4, 1}, {2, 4}, {-1, -3}, {4, 2}}
	var np []calPair
	for i, tg := range calTargets(5) {
		x, y := trueRaw(tg[0]+noise[i][0], tg[1]+noise[i][1])
		np = append(np, calPair{SX: tg[0], SY: tg[1], RX: x, RY: y})
	}
	if i, d := calOutlier(np, calOutlierLimit); i != -1 {
		t.Errorf("noise flagged point %d (%.1f)", i, d)
	}
	// 一直線の点では決まらない
	if _, err := fitAffine([]calPair{{0, 0, 1, 1}, {1, 1, 2, 2}, {2, 2, 3, 3}}); err == nil {
		t.Error("collinear points: no error")
	}
}

func setCal(t *testing.T, c *TouchCalib) {
	t.Helper()
	old := touchCalNow.Load()
	touchCalNow.Store(c)
	t.Cleanup(func() { touchCalNow.Store(old) })
}

func TestTouchMappingWithCalibration(t *testing.T) {
	// 設定の min と max と同じ変換をキャリブレーションで書くと、同じ結果になる
	var ps []calPair
	for _, tg := range calTargets(5) {
		x := 226 + tg[0]*(3938-226)/800
		y := 3803 + tg[1]*(384-3803)/480
		ps = append(ps, calPair{SX: tg[0], SY: tg[1], RX: x, RY: y})
	}
	a, _ := fitAffine(ps)
	for _, r := range [][2]int32{{300, 3700}, {2000, 2000}, {3900, 400}, {1000, 3000}} {
		touchCalNow.Store(nil)
		p0 := touchPoint(calTestTC, r[0], r[1], 800, 480)
		c0, r0 := touchCell(calTestTC, 4, 3, r[0], r[1])
		m := touchMap{tc: calTestTC, rot: 90, pw: 800, ph: 480}
		q0 := m.point(r[0], r[1])
		f0 := m.fpoint(r[0], r[1])
		setCal(t, &TouchCalib{A: a, W: 800, H: 480})
		p1 := touchPoint(calTestTC, r[0], r[1], 800, 480)
		c1, r1 := touchCell(calTestTC, 4, 3, r[0], r[1])
		q1 := m.point(r[0], r[1])
		f1 := m.fpoint(r[0], r[1])
		if abs(p0.X-p1.X) > 1 || abs(p0.Y-p1.Y) > 1 || c0 != c1 || r0 != r1 || abs(q0.X-q1.X) > 1 || abs(q0.Y-q1.Y) > 1 ||
			math.Abs(f0.X-f1.X) > 0.01 || math.Abs(f0.Y-f1.Y) > 0.01 {
			t.Errorf("raw %v: point %v/%v cell %d,%d/%d,%d rotated %v/%v f %v/%v", r, p0, p1, c0, r0, c1, r1, q0, q1, f0, f1)
		}
	}
	// ずらしたキャリブレーション：画面のドットが変わる。ソフトキー（生の座標）は変わらない
	shift := a
	shift[2] += 50
	setCal(t, &TouchCalib{A: shift, W: 800, H: 480})
	if p := touchPoint(calTestTC, 2082, 2093, 800, 480); abs(p.X-450) > 2 || abs(p.Y-240) > 2 {
		t.Errorf("shifted point %v", p)
	}
	km := &Keymap{Areas: []namedArea{{"home", SoftArea{X: [2]int32{3740, 4095}, Y: [2]int32{3377, 4095}}}}}
	if n, ok := km.hitSoft(3900, 3500); !ok || n != "home" {
		t.Errorf("soft key %q %v", n, ok)
	}
	// 画面の外は端に寄せる
	if p := touchPoint(calTestTC, 4095, 0, 800, 480); p.X != 799 || p.Y != 479 {
		t.Errorf("clamped %v", p)
	}
}

type calRec struct {
	shown   int
	stopped int
	notices []string
	timers  []func()
}

func newTestCalib(t *testing.T) (*CalibUI, *calRec) {
	t.Helper()
	setCal(t, nil)
	r := &calRec{}
	c := NewCalibUI(t.TempDir())
	c.tc = func() *TouchConfig { return calTestTC }
	c.show = func(f func(*Canvas) bool) {
		if f == nil {
			r.stopped++
		} else {
			r.shown++
			cv := NewCanvas(800, 480, 1600, rgb565, 0)
			f(cv)
		}
	}
	c.notify = func(m string, _ int) { r.notices = append(r.notices, m) }
	c.timer = func(_ time.Duration, f func()) func() bool {
		r.timers = append(r.timers, f)
		i := len(r.timers) - 1
		return func() bool { r.timers[i] = nil; return true }
	}
	return c, r
}

// press は、画面のドット (X, Y) を n 回読んで離す（本当のパネルで生の座標にする）。jitter で動かす。
func press(c *CalibUI, X, Y float64, n int, jitter float64) {
	for i := 0; i < n; i++ {
		dx := 0.0
		if i == n/2 {
			dx = jitter
		}
		x, y := trueRaw(X+dx, Y)
		c.Touch(true, int32(math.Round(x)), int32(math.Round(y)))
	}
	c.Touch(false, 0, 0)
}

func TestCalibUI(t *testing.T) {
	c, r := newTestCalib(t)
	if err := c.Start(7, "test"); err == nil {
		t.Error("7 points accepted")
	}
	if err := c.Start(5, "test"); err != nil || !c.Active() || r.shown != 1 {
		t.Fatalf("start: %v %v %d", err, c.Active(), r.shown)
	}
	tg := calTargets(5)
	// 短い、動いた、ほかの十字を押した：同じ点のまま
	press(c, tg[0][0], tg[0][1], 4, 0)
	if st := c.Status(); st.Point != 1 || !strings.Contains(c.msg, "短すぎ") {
		t.Fatalf("short press: %+v %q", st, c.msg)
	}
	press(c, tg[0][0], tg[0][1], 12, 40)
	if st := c.Status(); st.Point != 1 || !strings.Contains(c.msg, "動きました") {
		t.Fatalf("moved: %+v %q", st, c.msg)
	}
	press(c, tg[2][0], tg[2][1], 12, 0)
	if st := c.Status(); st.Point != 1 || !strings.Contains(c.msg, "十字から離れています") {
		t.Fatalf("wrong target: %+v %q", st, c.msg)
	}
	// 4 点を正しく、最後の 1 点を少しずらして押す → その点をやり直す
	for i := 0; i < 4; i++ {
		press(c, tg[i][0], tg[i][1], 12, 0)
	}
	if st := c.Status(); st.Point != 5 {
		t.Fatalf("point %+v", st)
	}
	press(c, tg[4][0]+45, tg[4][1], 12, 0)
	if st := c.Status(); st.State != "target" || st.Point != 5 || !strings.Contains(c.msg, "ほかと合いません") {
		t.Fatalf("outlier: %+v %q", st, c.msg)
	}
	press(c, tg[4][0], tg[4][1], 12, 0)
	st := c.Status()
	if st.State != "verify" || st.Before == nil || st.After == nil || st.After.Max > 1.5 || st.Before.Mean < 5 {
		t.Fatalf("verify: %+v %+v %+v", st, st.Before, st.After)
	}
	// 確かめの画面：押した場所に点。ボタンの「保存」
	press(c, 100, 200, 6, 0)
	if len(c.dots) != 1 || math.Abs(c.dots[0][0]-100) > 1.5 || math.Abs(c.dots[0][1]-200) > 1.5 {
		t.Fatalf("dot %v", c.dots)
	}
	press(c, 230, 330, 6, 0) // 保存
	if c.Active() || c.Status().Result != "saved" || r.stopped != 1 || !strings.Contains(r.notices[len(r.notices)-1], "保存しました") {
		t.Fatalf("save: %+v %d %q", c.Status(), r.stopped, r.notices)
	}
	// 保存したものを使う：本当のパネルの座標が、十字の位置になる
	x, y := trueRaw(600, 100)
	if p := touchPoint(calTestTC, int32(x), int32(y), 800, 480); abs(p.X-600) > 1 || abs(p.Y-100) > 1 {
		t.Errorf("calibrated point %v", p)
	}
	touchCalNow.Store(nil)
	f, err := loadTouchCal(c.dataDir)
	if err != nil || f == nil || len(f.Points) != 5 || f.Config != [4]int32{226, 3938, 3803, 384} || touchCalNow.Load() == nil || f.Source != "test" {
		t.Fatalf("load: %+v %v", f, err)
	}
	// 戻す
	if had, err := resetTouchCal(c.dataDir); !had || err != nil || touchCalNow.Load() != nil {
		t.Fatalf("reset %v %v", had, err)
	}
	if _, err := os.Stat(filepath.Join(c.dataDir, touchCalName+".prev")); err != nil {
		t.Error("no .prev after reset")
	}
}

func TestCalibUICancel(t *testing.T) {
	c, r := newTestCalib(t)
	c.Start(5, "test")
	tg := calTargets(5)
	for i := 0; i < 5; i++ {
		press(c, tg[i][0], tg[i][1], 10, 0)
	}
	// 確かめの画面で「やり直す」→ 最初の点から
	press(c, 400, 330, 6, 0)
	if st := c.Status(); st.State != "target" || st.Point != 1 {
		t.Fatalf("redo: %+v", st)
	}
	// 本体の Esc でやめる。何も保存しない
	c.Key(evdev.KEY_ESC)
	if c.Active() || c.Status().Result != "cancelled" || touchCalNow.Load() != nil {
		t.Fatalf("cancel: %+v", c.Status())
	}
	if _, err := os.Stat(touchCalPath(c.dataDir)); err == nil {
		t.Error("saved after cancel")
	}
	// 決定のソフトキーで保存、戻るでやめる
	c.Start(5, "test")
	for i := 0; i < 5; i++ {
		press(c, tg[i][0], tg[i][1], 10, 0)
	}
	if !c.SoftKey("enter") || c.Status().Result != "saved" {
		t.Fatalf("enter: %+v", c.Status())
	}
	c.Start(9, "test")
	if !c.SoftKey("back") || c.Active() {
		t.Fatal("back did not cancel")
	}
	// 3 分さわらなければやめる
	c.Start(5, "test")
	r.timers[len(r.timers)-1]()
	if c.Active() || c.Status().Result != "timeout" {
		t.Fatalf("timeout: %+v", c.Status())
	}
	// 描く：今の十字は黄色
	c.Start(5, "test")
	cv := NewCanvas(800, 480, 1600, rgb565, 0)
	if !c.draw(cv) {
		t.Fatal("draw returned false")
	}
	ref := NewCanvas(800, 480, 1600, rgb565, 0)
	ref.fill(image.Rect(0, 0, 1, 1), colCalTarget)
	if i := 48*cv.stride + (80+10)*2; cv.pix[i] != ref.pix[0] || cv.pix[i+1] != ref.pix[1] {
		t.Errorf("cross pixel %x%x, want %x%x", cv.pix[i], cv.pix[i+1], ref.pix[0], ref.pix[1])
	}
}

func TestCalibrateCommand(t *testing.T) {
	setCal(t, nil)
	d := newTestDaemon(t, testConfig)
	d.ctl.calib = NewCalibUI(d.dataDir)
	d.ctl.calib.tc = func() *TouchConfig { return calTestTC }
	r := result(t, d.call("calibrate", map[string]any{"mode": "start", "kind": "touch9"}))
	if st := r["status"].(map[string]any); st["state"] != "target" || st["points"] != float64(9) {
		t.Fatalf("start: %v", r)
	}
	if c := errCode(d.call("calibrate", map[string]any{"mode": "start", "kind": "touch3"})); c != errBadRequest {
		t.Errorf("bad kind: %s", c)
	}
	r = result(t, d.call("calibrate", map[string]any{"mode": "cancel"}))
	if st := r["status"].(map[string]any); st["state"] != "off" || st["result"] != "cancelled" {
		t.Fatalf("cancel: %v", r)
	}
	r = result(t, d.call("calibrate", map[string]any{"mode": "reset"}))
	if r["reset"] != false || r["calibration"].(map[string]any)["source"] != "config" {
		t.Fatalf("reset: %v", r)
	}
}

// 「タッチ調整」のセルで始め、そのあとのタッチはキャリブレーションに回る。ソフトキー（生の座標）の戻るでやめる（touchProc）
func TestCalibrateFromCell(t *testing.T) {
	setCal(t, nil)
	_, km := compileText(t, `
hid_device: /dev/null
touch:
  min_x: 0
  max_x: 4000
  min_y: 0
  max_y: 2400
  soft_areas:
    back: {x: [4001, 4095], "y": [0, 2400]}
layers:
  - name: base
    touch:
      cols: 2
      rows: 1
      cells:
        "0,0": {calibrate: touch}
        "1,0": {key: A}
`)
	hidPath := filepath.Join(t.TempDir(), "hidg0")
	os.WriteFile(hidPath, nil, 0o644)
	sent := func() int64 { fi, _ := os.Stat(hidPath); return fi.Size() }
	e := NewEngine(km, &State{hid: NewHIDWriter(hidPath), active: map[string]Combo{}})
	c := NewCalibUI(t.TempDir())
	c.tc = func() *TouchConfig { return km.Touch }
	e.calib = c
	if v := e.View(); v.Cells[0].Spec.Calibrate != "touch" {
		t.Fatalf("cell %+v", v.Cells[0].Spec)
	}
	tp := &touchProc{e: e}
	touch := func(x, y int32, n int) {
		tp.event(absEv(evdev.ABS_X, x), time.Now())
		tp.event(absEv(evdev.ABS_Y, y), time.Now())
		tp.event(keyEv(evdev.BTN_TOUCH, 1), time.Now())
		tp.event(synEv(), time.Now())
		for i := 1; i < n; i++ {
			tp.event(absEv(evdev.ABS_X, x), time.Now())
			tp.event(synEv(), time.Now())
		}
		tp.event(keyEv(evdev.BTN_TOUCH, 0), time.Now())
		tp.event(synEv(), time.Now())
	}
	touch(1000, 1200, 1) // 「タッチ調整」
	if !c.Active() || c.Status().Points != 5 {
		t.Fatalf("not started: %+v", c.Status())
	}
	// 1 つ目の十字（80, 48）→ 生の座標 (400, 240)。A のセルの上でも、キーは送らない
	before := sent()
	touch(400, 240, 10)
	if st := c.Status(); st.Point != 2 {
		t.Fatalf("first point: %+v %q", st, c.msg)
	}
	touch(3000, 1200, 10) // 十字から遠い（A のセル）
	if sent() != before {
		t.Errorf("sent keys during calibration")
	}
	// 本体キーも PC に送らない。戻るのソフトキーでやめる
	e.PressKey(evdev.KEY_A)
	e.ReleaseKey(evdev.KEY_A)
	if sent() != before {
		t.Errorf("sent a key during calibration")
	}
	touch(4050, 1000, 3)
	if c.Active() || c.Status().Result != "cancelled" {
		t.Fatalf("back did not cancel: %+v", c.Status())
	}
	// やめたあとは、いつもどおり
	touch(3000, 1200, 2)
	if sent() == before {
		t.Error("A was not sent after calibration")
	}
}
