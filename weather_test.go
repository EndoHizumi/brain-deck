package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func ip(v int) *int { return &v }

func loadWeatherFixture(t *testing.T, name string) *WeatherData {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("gui/test/fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	var d WeatherData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWeatherValidate(t *testing.T) {
	ok := WeatherRequest{Days: []WeatherDay{{Date: "2026-10-07", Telop: "曇り"}, {Date: "2026-10-06", Telop: " 晴れ\n", Icon: "100", Max: ip(26), Rain: [4]*int{nil, ip(0), ip(10), ip(100)}}}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Days[0].Date != "2026-10-06" || ok.Days[0].Telop != "晴れ" {
		t.Errorf("not sorted or normalized: %+v", ok.Days)
	}
	bad := []WeatherRequest{
		{},
		{Days: []WeatherDay{{Date: "10/6"}}},
		{Days: []WeatherDay{{Date: "2026-10-06"}, {Date: "2026-10-06"}}},
		{Days: []WeatherDay{{Date: "2026-10-06", Icon: "1000"}}},
		{Days: []WeatherDay{{Date: "2026-10-06", Max: ip(120)}}},
		{Days: []WeatherDay{{Date: "2026-10-06", Rain: [4]*int{ip(101)}}}},
		{Days: []WeatherDay{{Date: "2026-10-06", Telop: "a\x01b"}}},
		{Days: make([]WeatherDay, 8)},
	}
	for i, r := range bad {
		if err := r.validate(); err == nil {
			t.Errorf("%d: %+v was accepted", i, r)
		}
	}
	if err := (&WeatherRequest{Error: "HTTP 500"}).validate(); err != nil {
		t.Errorf("error only: %v", err)
	}
}

// 取得に失敗したときは、前の予報を残して失敗だけを覚える。保存して、読み直せる。
func TestWeatherServiceKeepsOnError(t *testing.T) {
	dir := dataDir(t)
	ws := NewWeatherService(OpenStore(dir))
	ws.saved = make(chan struct{}, 10)
	now := mustTime(t, "2026-10-06T09:05:00+09:00")
	if _, kept, err := ws.Set(WeatherRequest{Error: "時間内に取得できませんでした"}, "test", now); err != nil || kept {
		t.Fatalf("first error: kept %v, %v", kept, err)
	}
	if d := ws.Snapshot(); len(d.Days) != 0 || !d.failed() {
		t.Errorf("first error: %+v", d)
	}
	fetched := now.Add(-time.Minute)
	d, _, err := ws.Set(WeatherRequest{Area: "東京都 東京", FetchedAt: &fetched, Days: []WeatherDay{{Date: "2026-10-06", Telop: "晴れ", Icon: "100"}}}, "test", now)
	if err != nil || d.Rev != 2 || d.failed() || d.Error != "" || !d.FetchedAt.Equal(fetched) {
		t.Fatalf("ok: %+v %v", d, err)
	}
	later := now.Add(time.Hour)
	d, kept, err := ws.Set(WeatherRequest{Error: "取得できません（HTTP 500）"}, "test", later)
	if err != nil || !kept || len(d.Days) != 1 || d.Area != "東京都 東京" || !d.failed() || !d.FetchedAt.Equal(fetched) {
		t.Fatalf("error after ok: kept %v %+v %v", kept, d, err)
	}
	// 保存は、続けて変えるとまとめて 1 回になる。最後の版が書かれるまで待つ
	for f := (WeatherData{}); ws.store.Load(weatherStoreName, &f) != nil || f.Rev != 3; {
		select {
		case <-ws.saved:
		case <-time.After(5 * time.Second):
			t.Fatalf("weather.json was not saved (rev %d)", f.Rev)
		}
	}
	ws2 := NewWeatherService(OpenStore(dir))
	if d2 := ws2.Snapshot(); d2 == nil || d2.Rev != 3 || !d2.failed() || d2.day("2026-10-06") == nil {
		t.Errorf("reloaded: %+v", d2)
	}
}

func wthWidget(t *testing.T, spec string) *WidgetDef {
	t.Helper()
	return calWidget(t, spec)
}

func wthTexts(items []wthItem) string {
	var out []string
	for _, it := range items {
		if it.Kind == "text" {
			out = append(out, it.Text)
		}
	}
	return strings.Join(out, "|")
}

func wthModeOf(t *testing.T, r image.Rectangle) string {
	_, mode, _ := wthIconBox(r)
	return mode
}

func TestWeatherLayout(t *testing.T) {
	inTokyo(t)
	d := loadWeatherFixture(t, "weather.json")
	w := wthWidget(t, `{ widget: weather }`)
	if w.Stale != defaultWthStale {
		t.Errorf("stale = %v", w.Stale)
	}
	now := mustTime(t, "2026-10-06T09:41:27+09:00")
	env := WidgetEnv{Now: now, TimeSynced: true, Weather: d}
	// 1×1：今日だけ、アイコンの右に天気と気温、降水確率（見出しなし）
	one := wthLayout(image.Rect(0, 0, 176, 136), w, env)
	s := wthTexts(one)
	for _, want := range []string{"今日 10/6(火)", "晴時々曇", "26°", "/", "--", "10%", "60%", "更新 9:05 5時発表 出典:気象庁"} {
		if !strings.Contains("|"+s+"|", "|"+want+"|") {
			t.Errorf("1x1: %q not in %q", want, s)
		}
	}
	if strings.Contains(s, "明日") {
		t.Errorf("1x1 shows tomorrow: %q", s)
	}
	// 過ぎた時間帯（0〜6 時）は薄く、今の時間帯はふつうの色
	for _, it := range one {
		switch it.Text {
		case "--":
			if it.Y > 100 && it.Color != colTodoDone.hexStr() {
				t.Errorf("past slot color %s", it.Color)
			}
		case "10%":
			if it.Color == colTodoDone.hexStr() {
				t.Errorf("current slot is dim")
			}
		}
	}
	var icons []wthItem
	for _, it := range one {
		if it.Kind == "icon" {
			icons = append(icons, it)
		}
	}
	if len(icons) != 1 || icons[0].Icon != fmt.Sprintf("101-%dx%d", icons[0].W, icons[0].H) || icons[0].W != icons[0].H*3/2 {
		t.Errorf("1x1 icons = %+v", icons)
	}
	// 2×1：今日と明日、線で分ける
	two := wthLayout(image.Rect(0, 0, 376, 136), w, env)
	if s := wthTexts(two); !strings.Contains(s, "明日 10/7(水)") || !strings.Contains(s, "曇のち一時雨") || !strings.Contains(s, "17°") {
		t.Errorf("2x1: %q", s)
	}
	rules := 0
	for _, it := range two {
		if it.Kind == "rule" {
			rules++
		}
	}
	if rules != 1 {
		t.Errorf("2x1 rules = %d", rules)
	}
	// 列の形
	for _, c := range []struct {
		r    image.Rectangle
		mode string
	}{{image.Rect(0, 0, 176, 120), "side"}, {image.Rect(0, 0, 184, 268), "tall"}, {image.Rect(0, 0, 384, 20), "line"}, {image.Rect(0, 0, 176, 47), "line"}, {image.Rect(0, 0, 176, 48), "side"}} {
		if m := wthModeOf(t, c.r); m != c.mode {
			t.Errorf("%v: mode %s, want %s", c.r, m, c.mode)
		}
	}
	// 2×2：時間帯の見出しも
	if s := wthTexts(wthLayout(image.Rect(0, 0, 376, 296), w, env)); !strings.Contains(s, "12-18") {
		t.Errorf("2x2 has no slot labels: %q", s)
	}
	// 低い行：1 行に見出し、天気、気温。最終更新の行が入らなければ省く
	line := wthLayout(image.Rect(0, 0, 776, 16), w, env)
	if s := wthTexts(line); strings.Contains(s, "更新") || !strings.Contains(s, "今日 ") || !strings.Contains(s, "明日 ") {
		t.Errorf("line: %q", s)
	}
	// 明日の予報がない
	d1 := *d
	d1.Days = d.Days[:1]
	if s := wthTexts(wthLayout(image.Rect(0, 0, 376, 136), w, WidgetEnv{Now: now, TimeSynced: true, Weather: &d1})); !strings.Contains(s, "明日 10/7(水)|データなし|更新") {
		t.Errorf("no tomorrow: %q", s)
	}
	// 予報がない、失敗だけ、時刻を合わせていない
	if s := wthTexts(wthLayout(image.Rect(0, 0, 176, 136), w, WidgetEnv{Now: now, TimeSynced: true})); s != wthNoData {
		t.Errorf("none: %q", s)
	}
	failed := &WeatherData{Error: "x", ErrorAt: &now, Days: []WeatherDay{}}
	if s := wthTexts(wthLayout(image.Rect(0, 0, 176, 136), w, WidgetEnv{Now: now, TimeSynced: true, Weather: failed})); s != wthFailedData {
		t.Errorf("failed only: %q", s)
	}
	if f, warn := wthFooter(w, d, WidgetEnv{Now: now, Weather: d}, 1000); f != unsyncedText || !warn {
		t.Errorf("unsynced footer %q %v", f, warn)
	}
}

// アイコンの箱は、列の大きさだけで決まる（天気の文や気温の長さで変わらない）。
func TestWeatherIconBoxIgnoresText(t *testing.T) {
	inTokyo(t)
	w := wthWidget(t, `{ widget: weather }`)
	now := mustTime(t, "2026-10-06T09:41:27+09:00")
	box := func(telop string, max *int, area image.Rectangle) string {
		d := &WeatherData{Days: []WeatherDay{{Date: "2026-10-06", Telop: telop, Icon: "100", Max: max, Min: max}}}
		for _, it := range wthLayout(area, w, WidgetEnv{Now: now, TimeSynced: true, Weather: d}) {
			if it.Kind == "icon" {
				return fmt.Sprintf("%d,%d %dx%d", it.X, it.Y, it.W, it.H)
			}
		}
		return ""
	}
	for _, a := range []image.Rectangle{image.Rect(0, 0, 176, 136), image.Rect(0, 0, 184, 296), image.Rect(0, 0, 776, 40), image.Rect(0, 0, 376, 296)} {
		x, y := box("晴", nil, a), box("曇時々雨で雷を伴う", ip(-10), a)
		if x != y || x == "" {
			t.Errorf("%v: %q vs %q", a, x, y)
		}
	}
}

// 0 時〜5 時：前の日の発表の「明日」が、今日の日付で来る。日付で今日を選び、発表の日時に日付を付ける。
func TestWeatherNight(t *testing.T) {
	inTokyo(t)
	d := loadWeatherFixture(t, "weather-night.json")
	w := wthWidget(t, `{ widget: weather }`)
	now := mustTime(t, "2026-10-06T03:12:00+09:00")
	s := wthTexts(wthLayout(image.Rect(0, 0, 376, 136), w, WidgetEnv{Now: now, TimeSynced: true, Weather: d}))
	for _, want := range []string{"今日 10/6(火)", "晴れ", "24°", "15°", "明日 10/7(水)", "10/5 17時発表"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q not in %q", want, s)
		}
	}
	// 0〜6 時の降水確率は、まだ過ぎていないので薄くしない
	for _, it := range wthLayout(image.Rect(0, 0, 176, 136), w, WidgetEnv{Now: now, TimeSynced: true, Weather: d}) {
		if it.Text == "0%" && it.Color == colTodoDone.hexStr() {
			t.Errorf("0-6 is dim at 03:12")
		}
	}
	// 前の日に取った予報を、日付が変わっても使う（今日の日付のものを今日として出す）
	old := loadWeatherFixture(t, "weather.json")
	s = wthTexts(wthLayout(image.Rect(0, 0, 376, 136), w, WidgetEnv{Now: mustTime(t, "2026-10-07T07:00:00+09:00"), TimeSynced: true, Weather: old}))
	if !strings.Contains(s, "今日 10/7(水)|20%|40%|70%|50%|曇のち一時雨") || !strings.Contains(s, "明日 10/8(木)|0%|0%|10%|10%|晴れ") || !strings.Contains(s, "更新 10/6 9:05 古い 10/6 5時発表") {
		t.Errorf("next day: %q", s)
	}
}

func TestWeatherFooterAndNext(t *testing.T) {
	inTokyo(t)
	d := loadWeatherFixture(t, "weather-stale.json")
	w := wthWidget(t, `{ widget: weather, stale: 3h }`)
	now := mustTime(t, "2026-10-06T09:41:27+09:00")
	env := WidgetEnv{Now: now, TimeSynced: true, Weather: d}
	f, warn := wthFooter(w, d, env, 1000)
	if f != "更新 3:00 古い 失敗 10/5 23時発表 出典:気象庁" || !warn {
		t.Errorf("footer %q %v", f, warn)
	}
	// 幅が足りなければ、発表、出典の順に省く
	for _, c := range []struct {
		w    int
		want string
	}{{1000, "出典:気象庁"}, {font.textWidth("更新 3:00 古い 失敗 10/5 23時発表 気象庁"), " 気象庁"}, {font.textWidth("更新 3:00 古い 失敗 気象庁"), "失敗 気象庁"}, {40, "…"}} {
		if f, _ := wthFooter(w, d, env, c.w); !strings.HasSuffix(f, c.want) || font.textWidth(f) > c.w {
			t.Errorf("width %d: %q", c.w, f)
		}
	}
	// 次に変わる時刻：6 時間の区切り、古くなる時刻、日付の変わり目
	fresh := loadWeatherFixture(t, "weather.json")
	cases := []struct{ now, want string }{
		{"2026-10-06T09:41:27+09:00", "2026-10-06T12:00:00+09:00"},
		{"2026-10-06T11:30:00+09:00", "2026-10-06T12:00:00+09:00"},
		{"2026-10-06T19:00:00+09:00", "2026-10-07T00:00:00+09:00"},
	}
	w1 := wthWidget(t, `{ widget: weather, stale: 2h }`)
	for _, c := range cases {
		got := wthNext(w1, WidgetEnv{Now: mustTime(t, c.now), Weather: fresh})
		want := mustTime(t, c.want)
		if c.now == "2026-10-06T09:41:27+09:00" {
			want = mustTime(t, "2026-10-06T11:05:00+09:00") // 更新 9:05 + 2 時間
		}
		if !got.Equal(want) {
			t.Errorf("next at %s = %v, want %v", c.now, got, want)
		}
	}
	// 描き直しの判断：時間帯が変われば変わる
	k1 := wthKey(w, WidgetEnv{Now: mustTime(t, "2026-10-06T11:59:00+09:00"), TimeSynced: true, Weather: fresh})
	k2 := wthKey(w, WidgetEnv{Now: mustTime(t, "2026-10-06T12:00:00+09:00"), TimeSynced: true, Weather: fresh})
	if k1 == k2 {
		t.Errorf("key does not change at 12:00")
	}
}

func TestWeatherConfig(t *testing.T) {
	for _, bad := range []string{`{ widget: weather, stale: 10s }`, `{ widget: weather, rows: 2 }`, `{ widget: weather, id: x }`, `{ widget: weather, calendars: [a] }`} {
		if _, err := compileWidget(mustSpec(t, bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if w := wthWidget(t, `{ widget: weather, stale: 90m, key: F5, label: 天気 }`); w.Stale != 90*time.Minute {
		t.Errorf("stale = %v", w.Stale)
	}
}

func mustSpec(t *testing.T, s string) ActionSpec {
	t.Helper()
	var a ActionSpec
	cfg, err := parseConfig([]byte("layers:\n  - name: base\n    touch:\n      cols: 1\n      rows: 1\n      cells:\n        \"0,0\": " + s + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	a = cfg.Layers[0].Touch.Cells["0,0"]
	return a
}

// ---------- アイコン ----------

// testIcon は、w×h の、縁がなめらかな円と、半透明の帯のアイコン（テスト用に描く。気象庁のアイコンではない）。
func testIcon(w, h int, c RGB) []byte {
	pix := make([]byte, w*h*2)
	alpha := make([]byte, w*h)
	r := float64(min(w, h)) * 0.42
	cx, cy := float64(w)/2, float64(h)/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			a := math.Max(0, math.Min(1, r-d+0.5))
			col := c
			if y > h*2/3 && y < h*5/6 { // 半透明の帯
				a = math.Max(a, 0.5)
				col = RGB{0xe0, 0xe0, 0xe8}
			}
			i := y*w + x
			alpha[i] = uint8(math.Round(a * 255))
			binary.LittleEndian.PutUint16(pix[i*2:], uint16(rgb565.pack(col)))
		}
	}
	return encodeIcon(w, h, pix, alpha)
}

func TestIconFile(t *testing.T) {
	b := testIcon(30, 20, RGB{255, 0, 0})
	ic, err := parseIcon(b)
	if err != nil || ic.W != 30 || ic.H != 20 || len(ic.A) != 600 {
		t.Fatalf("parse: %+v %v", ic, err)
	}
	for _, bad := range [][]byte{b[:len(b)-1], append([]byte("LHI1"), b[4:]...), encodeIcon(0, 1, nil, nil)} {
		if _, err := parseIcon(bad); err == nil {
			t.Errorf("accepted a broken icon")
		}
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{{"100-87x58", true}, {"10-87x58", false}, {"100-0x58", false}, {"100-481x58", false}, {"100-87x58.lha", false}, {"../100-1x1", false}} {
		if _, _, ok := parseIconName(c.name); ok != c.ok {
			t.Errorf("parseIconName(%q) = %v", c.name, ok)
		}
	}
}

// 重ねる：透明は下のまま、不透明はアイコンの色、半透明は混ぜる。
func TestBlendIcon(t *testing.T) {
	cv := NewCanvas(4, 1, 8, rgb565, 0)
	cv.fill(image.Rect(0, 0, 4, 1), RGB{0, 0, 255})
	pix := make([]byte, 4*2)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(pix[i*2:], uint16(rgb565.pack(RGB{255, 0, 0})))
	}
	ic := &Icon{W: 4, H: 1, Pix: pix, A: []byte{0, 255, 128, 64}}
	cv.blendIcon(ic, image.Rect(0, 0, 4, 1), image.Rect(0, 0, 3, 1)) // 4 つ目は clip の外
	want := []RGB{{0, 0, 255}, {255, 0, 0}, {blend8(255, 0, 128), 0, blend8(0, 255, 128)}, {0, 0, 255}}
	for x, w := range want {
		u := rgb565.unpack(rgb565.pack(w))
		if got := cv.at(x, 0); got != (RGB{u.R, u.G, u.B}) {
			t.Errorf("x=%d: %v, want %v", x, got, w)
		}
	}
}

// 速い方法（回転なしの RGB565）と、1 画素ずつの方法で、同じ結果になる。
func TestBlendIcon565SameAsGeneric(t *testing.T) {
	ic, _ := parseIcon(testIcon(61, 41, RGB{0xff, 0x80, 0x20}))
	fast := NewCanvas(100, 60, 200, rgb565, 0)
	slow := NewCanvas(100, 60, 200, rgb565, 0)
	for y := 0; y < 60; y++ {
		for x := 0; x < 100; x++ {
			c := RGB{uint8(x * 2), uint8(y * 4), uint8(x + y)}
			fast.set(x, y, c)
			slow.set(x, y, c)
		}
	}
	box, clip := image.Rect(10, 5, 80, 55), image.Rect(15, 0, 100, 50)
	fast.blendIcon(ic, box, clip)
	slow.rot = 360 // 1 画素ずつの方法を通す（phys は 0、90、180、270 以外をそのまま返す）
	slow.blendIcon(ic, box, clip)
	if !bytes.Equal(fast.pix, slow.pix) {
		t.Errorf("the fast path differs")
	}
}

func iconUpload(t *testing.T, u *Uploads, name string, data []byte) (any, error) {
	t.Helper()
	sum := sha256.Sum256(data)
	st, err := u.Begin(UploadBegin{Kind: iconUploadKnd, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)), Name: name})
	if err != nil {
		return nil, err
	}
	if _, err := u.Chunk(st.Upload, 0, data); err != nil {
		return nil, err
	}
	return u.End(st.Upload)
}

// 受け取り、上限を超えたら今の設定で使わない大きさから消す。それでも足りなければ断る。
func TestIconStoreQuota(t *testing.T) {
	dir := t.TempDir()
	old := iconQuota
	t.Cleanup(func() { iconQuota = old })
	one := len(testIcon(40, 30, RGB{255, 0, 0}))
	iconQuota = int64(one*3 + 10)
	s := OpenIconStore(dir)
	changed := make(chan struct{}, 10)
	s.SetOnChange(func() { changed <- struct{}{} })
	want := map[image.Point]bool{image.Pt(40, 30): true}
	s.SetWanted(func() map[image.Point]bool { return want })
	u := NewUploads()
	u.Register(iconUploadKnd, s.UploadKind())
	for _, n := range []string{"100-41x30", "101-41x30", "102-40x30"} {
		if _, err := iconUpload(t, u, n, testIcon(40, 30, RGB{255, 0, 0})); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		<-changed
	}
	g := s.Gen()
	// 4 つ目：使わない大きさ（41x30）の古いものから消す
	res, err := iconUpload(t, u, "103-40x30", testIcon(40, 30, RGB{0, 255, 0}))
	if err != nil {
		t.Fatal(err)
	}
	if rm := res.(map[string]any)["removed"]; fmt.Sprint(rm) != "[100-41x30]" {
		t.Errorf("removed %v", rm)
	}
	if s.Has("100-41x30") || !s.Has("101-41x30") || !s.Has("103-40x30") || s.Gen() == g {
		t.Errorf("after quota: %v", s.files)
	}
	if _, err := os.Stat(filepath.Join(dir, "100-41x30.lha")); !os.IsNotExist(err) {
		t.Errorf("file not removed: %v", err)
	}
	// すべて使う大きさなら断る
	want[image.Pt(41, 30)] = true
	if _, err := iconUpload(t, u, "104-40x30", testIcon(40, 30, RGB{0, 0, 255})); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("over quota: %v", err)
	}
	// 箱より大きい、名前が違う、形が違う
	if _, err := iconUpload(t, u, "105-30x20", testIcon(40, 30, RGB{})); err == nil {
		t.Errorf("larger than the box was accepted")
	}
	if _, err := iconUpload(t, u, "x", testIcon(4, 3, RGB{})); err == nil {
		t.Errorf("bad name was accepted")
	}
	if _, err := iconUpload(t, u, "106-40x30", []byte("LHA1xxxxxxxx")); err == nil {
		t.Errorf("broken file was accepted")
	}
	// 読み直す
	s2 := NewIconStore(dir)
	l, total := s2.List()
	if len(l) != 3 || total != int64(3*one) || l[0].W != 40 {
		t.Errorf("reload: %+v %d", l, total)
	}
	if ic := s2.Icon("101-41x30"); ic == nil || ic.W != 40 {
		t.Errorf("Icon = %+v", ic)
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, transferPartGlob)); len(parts) != 0 {
		t.Errorf("parts left: %v", parts)
	}
}

func TestWeatherIconBoxesAndMissing(t *testing.T) {
	inTokyo(t)
	cfg, err := loadConfig("config/weather-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	boxes := weatherIconBoxes(km, 800, 480)
	if len(boxes) < 3 {
		t.Fatalf("boxes = %v", boxes)
	}
	for b := range boxes {
		if b.X != b.Y*3/2 && b.X != (b.Y*3/2) {
			t.Errorf("box %v is not 3:2", b)
		}
	}
	d := &WeatherData{Days: []WeatherDay{{Date: "2026-10-06", Icon: "100"}, {Date: "2026-10-07", Icon: "100"}, {Date: "2026-10-08", Icon: "201"}, {Date: "2026-10-09"}}}
	s := NewIconStore(t.TempDir())
	miss := weatherMissing(d, boxes, s)
	if len(miss) != 2*len(boxes) {
		t.Errorf("missing = %d, want %d", len(miss), 2*len(boxes))
	}
}

func TestWeatherCommands(t *testing.T) {
	d := newTestDaemon(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: weather, span: [2, 1] }
        "2,0": { widget: weather }
`, func(d *testDaemon) {
		d.ctl.uploads = NewUploads()
		d.ctl.uploads.Register(iconUploadKnd, d.icons.UploadKind())
	})
	hello := result(t, d.call("hello", nil))
	for _, c := range []string{"set_weather", "get_weather", "get_weather_icon"} {
		if !strings.Contains(fmt.Sprint(hello["commands"]), c) {
			t.Errorf("hello does not list %s", c)
		}
	}
	if !strings.Contains(fmt.Sprint(hello["upload_kinds"]), iconUploadKnd) {
		t.Errorf("upload_kinds = %v", hello["upload_kinds"])
	}
	g := result(t, d.call("get_weather", nil))
	if g["rev"] != 0.0 || g["shown"] != true || len(g["days"].([]any)) != 0 || len(g["icon_boxes"].([]any)) == 0 {
		t.Errorf("empty get_weather = %v", g)
	}
	w := map[string]any{"area": "東京都 東京", "public_time": "2026-10-05T20:00:00Z", "fetched_at": "2026-10-06T00:05:00Z",
		"days": []map[string]any{{"date": "2026-10-06", "telop": "晴れ", "icon": "100", "max": 26, "min": nil, "rain": []any{nil, 10, 30, 60}}}}
	r := result(t, d.call("set_weather", map[string]any{"weather": w, "source": "test"}))
	miss := r["icons_missing"].([]any)
	if r["rev"] != 1.0 || r["kept"] != false || r["shown"] != true || len(miss) != len(r["icon_boxes"].([]any)) || len(miss) == 0 {
		t.Fatalf("set_weather = %v", r)
	}
	m0 := miss[0].(map[string]any)
	iw, ih := int(m0["w"].(float64)), int(m0["h"].(float64))
	data := testIcon(iw, ih*2/3, RGB{255, 128, 0})
	sum := sha256.Sum256(data)
	b := result(t, d.call("upload_begin", map[string]any{"kind": iconUploadKnd, "sha256": hex.EncodeToString(sum[:]), "bytes": len(data), "name": m0["name"]}))
	result(t, d.call("upload_chunk", map[string]any{"upload": b["upload"], "offset": 0, "data": base64.StdEncoding.EncodeToString(data)}))
	result(t, d.call("upload_end", map[string]any{"upload": b["upload"]}))
	g = result(t, d.call("get_weather", nil))
	if len(g["icons"].([]any)) != 1 || len(g["icons_missing"].([]any)) != len(miss)-1 || g["days"].([]any)[0].(map[string]any)["telop"] != "晴れ" {
		t.Errorf("get_weather after upload = %v", g)
	}
	// get_weather_icon は分けて読める
	var got []byte
	for off := 0; ; {
		gi := result(t, d.call("get_weather_icon", map[string]any{"name": m0["name"], "offset": off, "length": 100}))
		chunk, _ := base64.StdEncoding.DecodeString(gi["data"].(string))
		got = append(got, chunk...)
		off += len(chunk)
		if off >= int(gi["bytes"].(float64)) || len(chunk) == 0 {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Errorf("get_weather_icon returned %d bytes, want %d", len(got), len(data))
	}
	if m := d.call("get_weather_icon", map[string]any{"name": "999-1x1"}); errCode(m) != errNotFound {
		t.Errorf("missing icon = %v", m)
	}
	// 失敗だけを知らせる
	r = result(t, d.call("set_weather", map[string]any{"weather": map[string]any{"error": "取得できません（HTTP 500）"}}))
	if r["kept"] != true || r["rev"] != 2.0 {
		t.Errorf("error = %v", r)
	}
	for _, p := range []map[string]any{{}, {"weather": "x"}, {"weather": map[string]any{}}, {"weather": map[string]any{"days": []map[string]any{{"date": "x"}}}}} {
		if m := d.call("set_weather", p); errCode(m) != errBadRequest {
			t.Errorf("set_weather %v = %v", p, m)
		}
	}
	// 保存（返事は待たない）が終わるまで待つ
	deadline := time.Now().Add(5 * time.Second)
	for f := (WeatherData{}); d.weather.store.Load(weatherStoreName, &f) != nil || f.Rev != 2; {
		if time.Now().After(deadline) {
			t.Fatalf("weather.json was not saved (rev %d)", f.Rev)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------- 設定 GUI と比べる表、プレビューの PNG の材料 ----------

// TestWeatherFixtures は、gui/test/fixtures の weatherlayout.json（Go の配置の表）と、weather-icons（テスト用のアイコン）が、
// 今のコードで作ったものと同じかを確かめる。LEFTHAND_UPDATE_WEATHER=1 で書き直す。
// アイコンはテスト用に描いたもの（円と帯）で、気象庁のアイコンではない。
func TestWeatherFixtures(t *testing.T) {
	inTokyo(t)
	update := os.Getenv("LEFTHAND_UPDATE_WEATHER") == "1"
	cfg, err := loadConfig("config/weather-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// アイコン：例の設定の箱 × 例の予報の番号
	iconDir := "gui/test/fixtures/weather-icons"
	colors := map[string]RGB{"100": {0xff, 0x80, 0x20}, "101": {0xff, 0xb0, 0x30}, "212": {0x40, 0xa0, 0xff}}
	want := map[string][]byte{}
	var boxes []image.Point
	for b := range weatherIconBoxes(km, 800, 480) {
		boxes = append(boxes, b)
	}
	sortPoints(boxes)
	for code, c := range colors {
		for _, b := range boxes {
			// 気象庁のアイコン（80×60）と同じ縦横比で箱に収める
			s := math.Min(float64(b.X)/80, float64(b.Y)/60)
			want[iconName(code, b)+iconExt] = testIcon(min(b.X, int(math.Round(80*s))), min(b.Y, int(math.Round(60*s))), c)
		}
	}
	// 配置の表
	type layoutCase struct {
		Name    string    `json:"name"`
		Now     string    `json:"now"`
		Synced  bool      `json:"synced"`
		Data    string    `json:"data,omitempty"` // fixtures の予報のファイル。空なら予報なし
		StaleS  int       `json:"stale_sec"`
		Area    [4]int    `json:"area"`
		Items   []wthItem `json:"items"`
		IconBox []int     `json:"-"`
	}
	areas := [][4]int{{12, 12, 188, 148}, {12, 40, 188, 148}, {12, 12, 388, 148}, {12, 12, 388, 308}, {12, 12, 188, 308},
		{12, 12, 788, 48}, {12, 12, 388, 48}, {12, 12, 788, 28}, {12, 12, 788, 468}, {12, 12, 112, 32}, {12, 12, 60, 60}}
	envs := []struct {
		name, now, data string
		synced          bool
		stale           time.Duration
	}{
		{"day", "2026-10-06T09:41:27+09:00", "weather.json", true, 3 * time.Hour},
		{"night", "2026-10-06T03:12:00+09:00", "weather-night.json", true, 3 * time.Hour},
		{"stale", "2026-10-06T09:41:27+09:00", "weather-stale.json", true, 3 * time.Hour},
		{"nextday", "2026-10-07T19:30:00+09:00", "weather.json", true, 0},
		{"unsynced", "2026-10-06T09:41:27+09:00", "weather.json", false, 3 * time.Hour},
		{"none", "2026-10-06T09:41:27+09:00", "", true, 3 * time.Hour},
		{"failed", "2026-10-06T09:41:27+09:00", "weather-failed.json", true, 3 * time.Hour},
	}
	var cases []layoutCase
	for _, e := range envs {
		var d *WeatherData
		if e.data != "" {
			d = loadWeatherFixture(t, e.data)
		}
		for i, a := range areas {
			if e.name != "day" && i%3 != 0 && i != 2 {
				continue // 予報の違いは、いくつかの大きさだけで
			}
			w := &WidgetDef{Kind: widgetWeather, Stale: e.stale}
			env := WidgetEnv{Now: mustTime(t, e.now), TimeSynced: e.synced, Weather: d}
			r := image.Rect(a[0], a[1], a[2], a[3])
			cases = append(cases, layoutCase{Name: e.name, Now: e.now, Synced: e.synced, Data: e.data, StaleS: int(e.stale.Seconds()),
				Area: a, Items: wthLayout(r, w, env)})
		}
	}
	layout, _ := json.MarshalIndent(cases, "", " ")
	layout = append(layout, '\n')
	lp := "gui/test/fixtures/weatherlayout.json"
	if update {
		os.MkdirAll(iconDir, 0o755)
		old, _ := filepath.Glob(filepath.Join(iconDir, "*"+iconExt))
		for _, p := range old {
			os.Remove(p)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(iconDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(lp, layout, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(iconDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s/%s differs (LEFTHAND_UPDATE_WEATHER=1 go test -run TestWeatherFixtures で書き直す): %v", iconDir, n, err)
		}
	}
	names, _ := filepath.Glob(filepath.Join(iconDir, "*"+iconExt))
	sort.Strings(names)
	if len(names) != len(want) {
		t.Errorf("%s has %d icons, want %d", iconDir, len(names), len(want))
	}
	if got, err := os.ReadFile(lp); err != nil || !bytes.Equal(got, layout) {
		t.Errorf("%s differs (LEFTHAND_UPDATE_WEATHER=1 go test -run TestWeatherFixtures で書き直す): %v", lp, err)
	}
}
