package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testForecast は、API の返事の形の予報（テスト用に書いたもの）。today の日付から 3 日分。
func testForecast(today time.Time, icon1 string) string {
	d := func(n int) string { return today.AddDate(0, 0, n).Format("2006-01-02") }
	return `{"publicTime":"` + today.Add(5*time.Hour).Format(time.RFC3339) + `","publishingOffice":"気象庁","title":"東京都 東京 の天気",
 "location":{"area":"関東","prefecture":"東京都","district":"東京地方","city":"東京"},
 "forecasts":[
  {"date":"` + d(0) + `","dateLabel":"今日","telop":"晴時々曇","temperature":{"min":{"celsius":null,"fahrenheit":null},"max":{"celsius":"26","fahrenheit":"78.8"}},
   "chanceOfRain":{"T00_06":"--%","T06_12":"10%","T12_18":"30%","T18_24":"60%"},"image":{"title":"晴時々曇","url":"ICONURL/` + icon1 + `.svg","width":80,"height":60}},
  {"date":"` + d(1) + `","dateLabel":"明日","telop":"雨","temperature":{"min":{"celsius":"17"},"max":{"celsius":"22"}},
   "chanceOfRain":{"T00_06":"20%","T06_12":"40%","T12_18":"70%","T18_24":"50%"},"image":{"url":"ICONURL/300.svg"}},
  {"date":"` + d(2) + `","dateLabel":"明後日","telop":"晴れ","temperature":{"min":{"celsius":"15"},"max":{"celsius":"25.4"}},
   "chanceOfRain":{"T00_06":"0%","T06_12":"0%","T12_18":"10%","T18_24":"10%"},"image":{"url":"ICONURL/` + icon1 + `.svg"}}]}`
}

// testSVG は、テスト用に書いたアイコン（80×60。円とグラデーション）。気象庁のアイコンではない。
const testSVG = `<svg width="80" height="60" viewBox="0 0 80 60" xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">
<defs><linearGradient id="a"><stop stop-color="#f60" offset="0"/><stop stop-color="#fc0" offset="1"/></linearGradient>
<linearGradient id="b" xlink:href="#a" x1="0" x2="1"/></defs>
<circle cx="40" cy="30" r="20" fill="url(#b)"/></svg>`

func TestParseForecast(t *testing.T) {
	today := time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local)
	w, err := parseForecast([]byte(testForecast(today, "101")), time.Date(2026, 10, 6, 9, 5, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	if w.Area != "東京都 東京" || w.Office != "気象庁" || w.PublicTime == nil || len(w.Days) != 3 {
		t.Fatalf("parsed %+v", w)
	}
	d0, d1, d2 := w.Days[0], w.Days[1], w.Days[2]
	if d0.Icon != "101" || d0.Telop != "晴時々曇" || *d0.Max != 26 || d0.Min != nil || d0.Rain[0] != nil || *d0.Rain[3] != 60 {
		t.Errorf("day 0: %+v", d0)
	}
	if d1.Icon != "300" || *d1.Min != 17 || *d1.Rain[2] != 70 || *d2.Max != 25 {
		t.Errorf("days 1, 2: %+v %+v", d1, d2)
	}
	b, _ := json.Marshal(d0)
	if !strings.Contains(string(b), `"min":null`) || !strings.Contains(string(b), `"rain":[null,10,30,60]`) || strings.Contains(string(b), "ICONURL") {
		t.Errorf("JSON for the Brain: %s", b)
	}
	for _, bad := range []string{`{}`, `{"forecasts":[]}`, `not json`, `{"forecasts":[{"date":"x"}]}`,
		`{"forecasts":[{"date":"2026-10-06","temperature":{"max":{"celsius":"暑い"}}}]}`} {
		if _, err := parseForecast([]byte(bad), time.Now()); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

func TestLoadWeatherCity(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	o := &options{}
	if _, err := loadWeatherCity(o); err == nil || !strings.Contains(err.Error(), "weather.yaml") || !strings.Contains(err.Error(), "primary_area.xml") {
		t.Errorf("no file: %v", err)
	}
	p := filepath.Join(cfgDir, "brain-deck", "weather.yaml")
	os.MkdirAll(filepath.Dir(p), 0o700)
	for _, c := range []struct {
		body, want string
		ok         bool
	}{{"city: \"123456\"\n", "123456", true}, {"city: 654321\n", "654321", true}, {"city: \"12345\"\n", "", false}, {"town: 1\n", "", false}} {
		os.WriteFile(p, []byte(c.body), 0o600)
		got, err := loadWeatherCity(o)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%q: %q %v", c.body, got, err)
		}
		if err != nil && strings.Contains(err.Error(), "12345") {
			t.Errorf("the city id leaked into the error: %v", err)
		}
	}
	if got, err := loadWeatherCity(&options{city: "111111"}); err != nil || got != "111111" {
		t.Errorf("--city: %q %v", got, err)
	}
	if _, err := loadWeatherCity(&options{city: "abc"}); err == nil {
		t.Errorf("bad --city was accepted")
	}
}

// 続けてアクセスするときは、前のアクセスから間をあける（brain-deck を続けて実行したときも）。
func TestPoliteWait(t *testing.T) {
	t.Setenv("BRAIN_DECK_CACHE", t.TempDir())
	t0 := time.Now()
	politeWait("x", 300*time.Millisecond)
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Errorf("first access waited %v", d)
	}
	politeWait("x", 300*time.Millisecond)
	if d := time.Since(t0); d < 250*time.Millisecond {
		t.Errorf("second access waited only %v", d)
	}
}

func TestMakeIcon(t *testing.T) {
	data, sz, err := makeIcon([]byte(testSVG), image.Pt(87, 58))
	if err != nil {
		t.Fatal(err)
	}
	// 80×60 の絵を 87×58 の箱に収めると 77×58
	if sz != image.Pt(77, 58) || string(data[:4]) != "LHA1" || len(data) != 8+77*58*3 {
		t.Fatalf("size %v, %d bytes", sz, len(data))
	}
	w, h := int(binary.LittleEndian.Uint16(data[4:])), int(binary.LittleEndian.Uint16(data[6:]))
	alpha := data[8+w*h*2:]
	px := func(x, y int) (uint16, uint8) {
		i := y*w + x
		return binary.LittleEndian.Uint16(data[8+i*2:]), alpha[i]
	}
	if c, a := px(0, 0); a != 0 || c != 0 {
		t.Errorf("corner: %04x %d (transparent pixels must be 0)", c, a)
	}
	if _, a := px(w/2, h/2); a != 255 {
		t.Errorf("center alpha %d", a)
	}
	// 円の左はだいだい（#f60 寄り）、右は黄色（#fc0 寄り）：グラデーションを href から受け継いでいる
	l, _ := px(w/2-15, h/2)
	r, _ := px(w/2+15, h/2)
	if g := func(v uint16) int { return int(v >> 5 & 63) }; g(l) >= g(r) {
		t.Errorf("gradient: left %04x right %04x", l, r)
	}
	if _, _, err := makeIcon([]byte(testSVG), image.Pt(0, 10)); err == nil {
		t.Errorf("empty box was accepted")
	}
	if _, _, err := makeIcon([]byte("<html/>"), image.Pt(10, 10)); err == nil {
		t.Errorf("not an SVG was accepted")
	}
}

func TestWeatherSync(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("BRAIN_DECK_CACHE", t.TempDir())
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	var mu sync.Mutex
	var uas []string
	var iconGets int
	fail := false
	today := time.Now()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		uas = append(uas, r.UserAgent())
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/forecast/city/"):
			if fail {
				http.Error(w, "oops", 500)
				return
			}
			w.Write([]byte(strings.ReplaceAll(testForecast(today, "101"), "ICONURL", srv.URL+"/img")))
		case r.URL.Path == "/img/101.svg":
			iconGets++
			w.Write([]byte(testSVG))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	http.DefaultClient = srv.Client() // テストの TLS の証明書を信じる
	t.Cleanup(func() { http.DefaultClient = &http.Client{} })
	t.Setenv("BRAIN_DECK_WEATHER_API", srv.URL+"/api/forecast/city/")

	f := newFakeBrain(t)
	var sets []map[string]any
	uploads := map[string][]byte{}
	var cur []byte
	var curName string
	f.handler = func(req map[string]any) (any, bool) {
		switch req["cmd"] {
		case "hello":
			return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test",
				"commands":     []string{"hello", "get_status", "set_time", "set_weather", "get_weather", "upload_begin", "upload_chunk", "upload_end", "upload_abort"},
				"upload_kinds": []string{"update", "image", "weather_icon"}}, true
		case "set_weather":
			sets = append(sets, req)
			w := req["weather"].(map[string]any)
			if w["days"] == nil {
				return map[string]any{"rev": 2, "kept": true, "icons_missing": []any{}}, true
			}
			miss := []any{}
			for _, b := range [][2]int{{87, 58}, {30, 20}} {
				for _, code := range []string{"101", "300"} {
					n := fmt.Sprintf("%s-%dx%d", code, b[0], b[1])
					if _, ok := uploads[n]; !ok {
						miss = append(miss, map[string]any{"name": n, "icon": code, "w": b[0], "h": b[1]})
					}
				}
			}
			return map[string]any{"rev": 1, "kept": false, "shown": true, "icons_missing": miss}, true
		case "upload_begin":
			if req["kind"] != "weather_icon" {
				return fakeErr{Code: "bad_request", Message: "kind"}, true
			}
			cur, curName = nil, req["name"].(string)
			return map[string]any{"upload": "u1", "chunk_bytes": 4000}, true
		case "upload_chunk":
			b, _ := base64.StdEncoding.DecodeString(req["data"].(string))
			cur = append(cur, b...)
			return map[string]any{"received": len(cur)}, true
		case "upload_end":
			uploads[curName] = cur
			return map[string]any{"name": curName}, true
		case "get_weather":
			return map[string]any{"rev": 1, "area": "東京都 東京", "days": sets[0]["weather"].(map[string]any)["days"], "shown": true,
				"icons": []any{map[string]any{"name": "101-87x58"}}, "icons_bytes": 1234, "icons_missing": []any{}}, true
		}
		return nil, false
	}
	go f.serve([]string{"hello", "get_status", "set_time"})

	// 設定ファイルがなければ、例を出して使い方の誤り（ID は伏せた例）
	code, _, errs := runCLI(t, "--port", f.slave, "weather", "sync")
	if code != exitUsage || !strings.Contains(errs, "weather.yaml") || !strings.Contains(errs, "<一次細分区域の ID>") {
		t.Fatalf("no config: exit %d %s", code, errs)
	}
	p := filepath.Join(cfgDir, "brain-deck", "weather.yaml")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("city: \"987654\"\n"), 0o600)

	// --dry-run は Brain に送らない
	code, out, errs := runCLI(t, "weather", "sync", "--dry-run")
	if code != exitOK || !strings.Contains(out, "東京都 東京") || !strings.Contains(out, "晴時々曇  最高 26℃ 最低 --  降水 --/10%/30%/60%") || !strings.Contains(out, "送っていません") {
		t.Fatalf("dry-run: exit %d\n%s\n%s", code, out, errs)
	}
	// sync：時刻を合わせてから送り、まだないアイコンだけを送る（300 は取れないので文字だけ）
	for len(f.got) > 0 {
		<-f.got
	}
	code, out, errs = runCLI(t, "--port", f.slave, "-v", "weather", "sync")
	if code != exitOK || !strings.Contains(out, "東京都 東京 の予報を送りました") || !strings.Contains(out, "アイコン 2 個を送りました") {
		t.Fatalf("sync: exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, "300：取得できません（HTTP 404）") {
		t.Errorf("missing icon warning: %s", errs)
	}
	if strings.Contains(out+errs, "987654") {
		t.Errorf("the city id leaked:\n%s\n%s", out, errs)
	}
	var cmds []string
	for len(f.got) > 0 {
		cmds = append(cmds, (<-f.got)["cmd"].(string))
	}
	if !strings.HasPrefix(strings.Join(cmds, ","), "hello,set_time,set_weather,upload_begin") {
		t.Errorf("commands = %v", cmds)
	}
	if len(uploads) != 2 || uploads["101-87x58"] == nil || uploads["101-30x20"] == nil || string(uploads["101-87x58"][:4]) != "LHA1" {
		t.Errorf("uploads = %v", len(uploads))
	}
	w := sets[0]["weather"].(map[string]any)
	if w["area"] != "東京都 東京" || len(w["days"].([]any)) != 3 || w["fetched_at"] == nil {
		t.Errorf("set_weather = %v", w)
	}
	// 2 回目：アイコンは PC のキャッシュから（取りに行かない）。Brain にあるので送らない
	mu.Lock()
	gets := iconGets
	mu.Unlock()
	code, out, _ = runCLI(t, "--port", f.slave, "weather", "sync", "--no-time-sync")
	if code != exitOK || strings.Contains(out, "アイコン") {
		t.Errorf("second sync: exit %d %s", code, out)
	}
	mu.Lock()
	if iconGets != gets {
		t.Errorf("the icon was fetched again (%d → %d)", gets, iconGets)
	}
	for _, ua := range uas {
		if !strings.HasPrefix(ua, "brain-deck/") {
			t.Errorf("User-Agent %q", ua)
		}
	}
	fail = true
	mu.Unlock()
	// 取得に失敗：Brain に失敗だけを知らせ、終了コード 7
	code, out, errs = runCLI(t, "--port", f.slave, "weather", "sync", "--no-time-sync")
	if code != exitFetch || !strings.Contains(errs, "HTTP 500") || !strings.Contains(out, "前の予報を残し") {
		t.Errorf("failure: exit %d\n%s\n%s", code, out, errs)
	}
	last := sets[len(sets)-1]["weather"].(map[string]any)
	if last["error"] != "取得できません（HTTP 500）" || last["days"] != nil {
		t.Errorf("failure set_weather = %v", last)
	}
	// list
	code, out, _ = runCLI(t, "--port", f.slave, "weather")
	if code != exitOK || !strings.Contains(out, "晴時々曇") || !strings.Contains(out, "アイコン：1 個") {
		t.Errorf("list: exit %d\n%s", code, out)
	}
	for _, bad := range [][]string{{"weather", "sync", "x"}, {"weather", "nope"}, {"weather", "list", "--city", "123456"}, {"text", "a", "b", "--city", "123456"}} {
		if code, _, _ := runCLI(t, bad...); code != exitUsage {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}
