package main

import (
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHWWeather は、天気のセルを実機の画面に出す。予報を受け取ったとき、アイコンが増えたときの描き直しの時間と、
// 描いている最中に押したキーのセルが待たないことを測り、画面が全体を描いたものと同じになることを確かめる。
// アイコンはテスト用に描いたもの（円と帯。気象庁のアイコンではない）を、一時ディレクトリに置く。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 60 ./lefthand.test -test.run HWWeather -test.v
func TestHWWeather(t *testing.T) {
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
        "0,0": { widget: weather, span: [2, 2] }
        "2,0": { widget: weather, span: [2, 1] }
        "2,1": { widget: weather, label: 天気 }
        "0,2": { widget: weather, span: [2, 1] }
        "3,2": B
`)
	dir := t.TempDir()
	icons := NewIconStore(dir)
	addIcon := func(code string, b image.Point) {
		s := math.Min(float64(b.X)/80, float64(b.Y)/60)
		data := testIcon(min(b.X, int(math.Round(80*s))), min(b.Y, int(math.Round(60*s))), RGB{0xff, 0x90, 0x20})
		n := iconName(code, b)
		p := filepath.Join(dir, n+iconExt)
		os.WriteFile(p+".tmp", data, 0o644)
		f, _ := os.Open(p + ".tmp")
		if _, err := icons.add(n, f, p+".tmp"); err != nil {
			t.Fatal(err)
		}
	}
	boxes := weatherIconBoxes(km, 800, 480)
	for b := range boxes {
		addIcon("100", b)
	}
	ws := NewWeatherService(OpenStore(t.TempDir()))
	now := time.Now()
	day := func(n int) string { return dayStart(now).AddDate(0, 0, n).Format(calDayLayout) }
	set := func(rev int, icon2 string) {
		f := now.Add(-time.Duration(rev) * time.Minute)
		if _, _, err := ws.Set(WeatherRequest{Area: "テスト", PublicTime: &f, FetchedAt: &f, Days: []WeatherDay{
			{Date: day(0), Telop: "晴時々曇", Icon: "100", Max: ip(20 + rev), Rain: [4]*int{nil, ip(10), ip(30), ip(60)}},
			{Date: day(1), Telop: "曇のち一時雨", Icon: icon2, Max: ip(22), Min: ip(15), Rain: [4]*int{ip(20), ip(40), ip(70), ip(50)}}}}, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	set(1, "100")
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	e.SetWidgets(rt)
	env := func() WidgetEnv {
		return WidgetEnv{Now: time.Now(), TimeSynced: true, Weather: ws.Snapshot(), WeatherIcons: icons}
	}
	rt.SetEnv(env)
	l := buildLayout(km, e.View())
	d, err := StartDisplay(cfg.Display, l, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ws.SetOnChange(d.Poke)
	icons.SetOnChange(d.Poke)
	rt.SetScreen(l.W, l.H, d.Poke)
	w := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget
	waitKey := func(what string, t0 time.Time) time.Duration {
		t.Helper()
		for time.Since(t0) < 5*time.Second {
			want := wthKey(w, env())
			d.drawMu.Lock()
			ok := d.wkeys[0] == want && d.wkeys[2] == want
			d.drawMu.Unlock()
			if ok {
				return time.Since(t0)
			}
			time.Sleep(500 * time.Microsecond)
		}
		t.Fatalf("%s: not redrawn", what)
		return 0
	}
	time.Sleep(time.Second)
	hwSameAsFull(t, d, env, "weather-idle")
	// 描くものの並びを作る時間（描画の goroutine が描くたびに作る）
	area := widgetArea(cellRect(0, 0, 2, 2, 4, 3, l.W, l.H), "")
	t0 := time.Now()
	for i := 0; i < 20; i++ {
		wthLayout(area, w, env())
	}
	t.Logf("wthLayout (2x2): %v", time.Since(t0)/20)
	// 予報を受け取ったとき。描いている最中にキーのセル（3,2）を押す
	kx, ky := int32(700), int32(400)
	for i := 2; i <= 4; i++ {
		t0 := time.Now()
		set(i, "100")
		time.Sleep(5 * time.Millisecond)
		p0 := time.Now()
		e.PressTouch(kx, ky)
		pe := time.Since(p0)
		e.Release("t")
		if pe > hwInputLimit {
			t.Errorf("PressTouch blocked for %v during a weather redraw", pe)
		}
		t.Logf("set_weather %d: redrawn in %v (key cell during the redraw: %v)", i, waitKey("set", t0), pe)
	}
	hwSameAsFull(t, d, env, "weather-set")
	// 明日の天気が変わり、まだないアイコン（201）を受け取ったとき
	set(5, "201")
	waitKey("201", time.Now())
	for b := range boxes {
		// 描く時間を測りに入れないよう、ファイルは先に作る（ARMv5 では浮動小数点が遅い）
		pre := iconName("201", b)
		s := math.Min(float64(b.X)/80, float64(b.Y)/60)
		body := testIcon(min(b.X, int(math.Round(80*s))), min(b.Y, int(math.Round(60*s))), RGB{0x40, 0xa0, 0xff})
		p := filepath.Join(dir, pre+iconExt+".tmp")
		os.WriteFile(p, body, 0o644)
		f, _ := os.Open(p)
		t0 := time.Now()
		if _, err := icons.add(pre, f, p); err != nil {
			t.Fatal(err)
		}
		t.Logf("icon 201 %dx%d: redrawn in %v", b.X, b.Y, waitKey(fmt.Sprintf("icon %v", b), t0))
	}
	hwSameAsFull(t, d, env, "weather-icons")
}
