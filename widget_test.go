package main

import (
	"bytes"
	"encoding/json"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// テストでは、システムの時刻を決して変えない（root で動かしても）。
var fakeClockSet []time.Time

func init() {
	setSystemClock = func(t time.Time) error {
		fakeClockSet = append(fakeClockSet, t)
		return nil
	}
	ntpSynced = func() bool { return false }
	readBootID = func() string { return "test-boot" }
}

const widgetConfig = `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    keys: { KEY_ESC: { layer_to: base } }
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: clock, span: [2, 1] }
        "2,0": { widget: clock, format: "15:04:05", date_format: none, label: "UTC", tz: UTC }
        "3,0": { widget: clock, key: F5 }
        "0,1": { key: ENTER, span: [1, 2], label: "決定" }
        "1,1": B
  - name: over
    touch:
      cells:
        "1,0": { key: C }
        "3,2": { layer_to: base }
`

func compileText(t *testing.T, src string) (*Config, *Keymap) {
	t.Helper()
	cfg, err := parseConfig([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, km
}

func TestWidgetConfig(t *testing.T) {
	_, km := compileText(t, widgetConfig)
	g := km.Layers[0].Grid
	clock := g.Cells[cellPos{0, 0}]
	if clock.Kind != actWidget || clock.Widget == nil || clock.SpanW != 2 || clock.SpanH != 1 {
		t.Fatalf("clock cell = %+v", clock)
	}
	if w := clock.Widget; w.Format != defaultClockFormat || w.DateFormat != defaultDateFormat || w.Seconds || w.Loc != nil {
		t.Errorf("clock defaults = %+v", w)
	}
	if w := g.Cells[cellPos{2, 0}].Widget; !w.Seconds || w.DateFormat != "" || w.Loc != time.UTC {
		t.Errorf("seconds clock = %+v", w)
	}
	if a := g.Cells[cellPos{3, 0}]; a.Kind != actKey || a.Widget == nil || !a.tappable() {
		t.Errorf("clock with key = %+v", a)
	}
	if clock.tappable() {
		t.Error("a clock without key should not be tappable (not highlighted)")
	}

	for _, tc := range []struct{ cell, want string }{
		{`{ widget: nope }`, "unknown widget"},
		{`{ key: B, format: "15:04" }`, "need widget: clock"},
		{`{ widget: clock, tz: Mars/Base }`, "unknown tz"},
		{`{ widget: clock, key: none }`, "cannot be none"},
		{`{ widget: clock, format: "  " }`, "shows nothing"},
		{`{ key: B, span: [4, 1] }`, "out of the 4x3 grid"},
		{`{ key: B, span: [0, 1] }`, "each 1..16"},
		{`{ widget: clock, key: B, layer_to: base }`, "exactly one"},
	} {
		src := strings.Replace(widgetConfig, `"1,1": B`, `"1,1": `+tc.cell, 1)
		cfg, err := parseConfig([]byte(src))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.cell, err, tc.want)
		}
	}
	// span の重なり（同じレイヤーの中）
	src := strings.Replace(widgetConfig, `"1,1": B`, `"1,1": { key: B, span: [1, 1] }
        "0,2": { key: A }`, 1)
	cfg, _ := parseConfig([]byte(src))
	if _, _, err := compileKeymap(cfg); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("overlap: err = %v", err)
	} else if p := asProblems(err); len(p) != 2 || p[0].Path == p[1].Path ||
		!strings.Contains(p[0].Path+p[1].Path, "/layers/0/touch/cells/0,2") || !strings.Contains(p[0].Path+p[1].Path, "/layers/0/touch/cells/0,1") {
		t.Errorf("overlap problems = %v", p)
	}
	// 本体キーとソフトキーには書けない
	src = strings.Replace(widgetConfig, `KEY_ESC: { layer_to: base }`, `KEY_ESC: { layer_to: base }, KEY_Q: { widget: clock }`, 1)
	cfg, _ = parseConfig([]byte(src))
	if _, _, err := compileKeymap(cfg); err == nil || !strings.Contains(err.Error(), "only in touch cells") {
		t.Errorf("widget on a key: err = %v", err)
	}
}

// 保存する YAML（設定 GUI の set_config）に、ウィジェットと span が 1 行で入り、読み直して同じになる。
func TestWidgetConfigRoundTrip(t *testing.T) {
	cfg, _ := compileText(t, widgetConfig)
	out, err := marshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"0,0": {span: [2, 1], widget: clock}`, `"0,1": {key: ENTER, label: 決定, span: [1, 2]}`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("saved YAML lacks %q:\n%s", want, out)
		}
	}
	again, _, _, err := checkConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := marshalConfig(again)
	if !bytes.Equal(a, out) {
		t.Errorf("round trip differs:\n%s\n---\n%s", out, a)
	}
}

func TestFormatClock(t *testing.T) {
	tm := time.Date(2026, 10, 6, 9, 5, 7, 0, time.UTC) // 火曜日
	for _, tc := range []struct{ layout, want string }{
		{defaultClockFormat, "09:05"},
		{defaultDateFormat, "10月6日(火)"},
		{"2006/01/02 Mon {wday}曜", "2026/10/06 Tue 火曜"},
		{"3:04 PM", "9:05 AM"},
	} {
		if got := formatClock(tm, tc.layout); got != tc.want {
			t.Errorf("%q = %q, want %q", tc.layout, got, tc.want)
		}
	}
	for layout, want := range map[string]bool{"15:04": false, "15:04:05": true, "15:04:5": true, "15:04:05.000": true, "1月2日": false} {
		if got := changesWithin(layout); got != want {
			t.Errorf("changesWithin(%q) = %v", layout, got)
		}
	}
	w := &WidgetDef{Kind: widgetClock}
	v := &CellView{Widget: w}
	if got := widgetNext(v, WidgetEnv{Now: tm}); !got.Equal(time.Date(2026, 10, 6, 9, 6, 0, 0, time.UTC)) {
		t.Errorf("next minute = %v", got)
	}
	w.Seconds = true
	if got := widgetNext(v, WidgetEnv{Now: tm.Add(300 * time.Millisecond)}); !got.Equal(tm.Add(time.Second)) {
		t.Errorf("next second = %v", got)
	}
}

// span のセルの透過：上のレイヤーで一部でも隠れたら出さない。タッチは左上のセルとして扱う。
func TestSpanView(t *testing.T) {
	_, km := compileText(t, widgetConfig)
	v := km.view([]int{0})
	if a, c, r := v.at(1, 0); a == nil || a.Widget == nil || c != 0 || r != 0 {
		t.Errorf("1,0 in base = %v %d,%d", a, c, r)
	}
	if a, c, r := v.at(0, 2); a == nil || a.Spec.Key != "ENTER" || c != 0 || r != 1 {
		t.Errorf("0,2 in base = %v %d,%d", a, c, r)
	}
	if v.Cells[1] != nil || v.Anchor[1] != 0 {
		t.Errorf("covered cell: Cells[1]=%v Anchor[1]=%d", v.Cells[1], v.Anchor[1])
	}
	over := km.view([]int{0, 1})
	if a, _, _ := over.at(1, 0); a == nil || a.Spec.Key != "C" {
		t.Errorf("1,0 over = %v", a)
	}
	if a, _, _ := over.at(0, 0); a != nil {
		t.Errorf("a span partly hidden by an upper layer should not show; 0,0 = %v", a)
	}
	if a, _, _ := over.at(0, 1); a == nil || a.Spec.Key != "ENTER" {
		t.Errorf("an untouched span should show through; 0,1 = %v", a)
	}

	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	h := e.PressTouch(100, 400) // セル 0,2 → span の左上 0,1
	if h.Col != 0 || h.Row != 1 || !h.Mapped {
		t.Errorf("touch on a span = %+v", h)
	}
	e.Release("t")
	h = e.PressTouch(300, 50) // 時計（key なし）は光らせない
	if h.Col != 0 || h.Row != 0 || h.Mapped {
		t.Errorf("touch on a clock = %+v", h)
	}
	e.Release("t")

	l := buildLayout(km, v)
	l.W, l.H = 800, 480
	if !l.Cells[1].Covered || l.Cells[0].SpanW != 2 || l.rect(0, 0) != image.Rect(0, 0, 400, 160) {
		t.Errorf("layout: cells[1]=%+v rect=%v", l.Cells[1], l.rect(0, 0))
	}
	if l.rect(0, 1) != image.Rect(0, 160, 200, 480) {
		t.Errorf("ENTER rect = %v", l.rect(0, 1))
	}
}

// ウィジェットの描き直し：変わったセルだけを描き直し、結果は全体を描き直したものと同じ。
func TestWidgetRedraw(t *testing.T) {
	_, km := compileText(t, widgetConfig)
	tm := time.Date(2026, 10, 6, 9, 5, 7, 0, time.Local)
	env := WidgetEnv{Now: tm, TimeSynced: true}
	d := &Display{cv: NewCanvas(800, 480, 1600, rgb565, 0), env: func() WidgetEnv { return env }}
	l := buildLayout(km, km.view([]int{0}))
	l.W, l.H = 800, 480
	d.layout = l
	d.drawn = make([]bool, len(l.Cells))
	d.drawAllLocked(l)
	if !d.wakeAt.Equal(tm.Add(time.Second).Truncate(time.Second)) {
		t.Errorf("wakeAt = %v (the UTC clock shows seconds)", d.wakeAt)
	}

	check := func(what string, changed ...int) {
		t.Helper()
		before := append([]byte(nil), d.cv.pix...)
		d.redrawWidgets()
		ref := NewCanvas(800, 480, 1600, rgb565, 0)
		rl := *l
		rl.Env = env
		drawAll(ref, &rl, d.drawn)
		if !bytes.Equal(ref.pix, d.cv.pix) {
			t.Fatalf("%s: differs from a full redraw", what)
		}
		var touched []int
		for i := range l.Cells {
			if l.Cells[i].Covered {
				continue
			}
			r := d.cv.physRect(l.rect(i%l.Cols, i/l.Cols))
			same := true
			for y := r.Min.Y; y < r.Max.Y && same; y++ {
				o := y*d.cv.stride + r.Min.X*2
				same = bytes.Equal(before[o:o+r.Dx()*2], d.cv.pix[o:o+r.Dx()*2])
			}
			if !same {
				touched = append(touched, i)
			}
		}
		if len(touched) != len(changed) {
			t.Fatalf("%s: redrew cells %v, want %v", what, touched, changed)
		}
		for i := range touched {
			if touched[i] != changed[i] {
				t.Fatalf("%s: redrew cells %v, want %v", what, touched, changed)
			}
		}
	}
	env.Now = tm.Add(time.Second)
	check("one second later", 2) // 秒を出す時計だけ
	env.Now = tm.Add(time.Minute)
	check("one minute later", 0, 2, 3)
	env.TimeSynced = false
	check("unsynced", 0, 2, 3) // 日付の行を出さない時計も「時刻未設定」を出す
}

func TestUnsyncedClockLooks(t *testing.T) {
	w, _ := compileWidget(ActionSpec{Widget: widgetClock})
	a, b, u := clockLines(w, WidgetEnv{Now: time.Now(), TimeSynced: false})
	if !u || b != unsyncedText || a == "" {
		t.Errorf("unsynced lines = %q %q %v", a, b, u)
	}
	cv := NewCanvas(400, 160, 800, rgb565, 0)
	l := &Layout{Cols: 1, Rows: 1, W: 400, H: 160, Cells: []CellView{{Mapped: true, Widget: w}}, Env: WidgetEnv{Now: time.Now()}}
	drawAll(cv, l, []bool{false})
	found := false
	img := cv.Image()
	for y := 0; y < 160 && !found; y++ {
		for x := 0; x < 400 && !found; x++ {
			c := img.RGBAAt(x, y)
			found = c.R > 0xf0 && c.G > 0x70 && c.G < 0x90 && c.B < 0x30
		}
	}
	if !found {
		t.Error("an unsynced clock should be drawn in the warning color")
	}
}

func TestSetTime(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	st := result(t, d.call("get_status", nil))
	tm := st["time"].(map[string]any)
	if tm["synced"] != false {
		t.Errorf("synced before set_time: %v", tm)
	}
	fakeClockSet = nil
	target := time.Now().Add(36 * time.Hour)
	r := result(t, d.call("set_time", map[string]any{"unix_ms": target.UnixMilli(), "source": "test"}))
	if r["stepped"] != true || r["synced"] != true {
		t.Errorf("set_time = %v", r)
	}
	if off := r["offset_ms"].(float64); off < float64(36*time.Hour/time.Millisecond)-5000 {
		t.Errorf("offset_ms = %v", off)
	}
	if len(fakeClockSet) != 1 || fakeClockSet[0].Sub(target).Abs() > 5*time.Second {
		t.Errorf("system clock set to %v, want about %v", fakeClockSet, target)
	}
	// clock.json は返事のあとに書く
	clockFile := filepath.Join(filepath.Dir(d.path), "data", "clock.json")
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(clockFile); err == nil && strings.Contains(string(b), `"source": "test"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, err := os.ReadFile(clockFile); err != nil || !strings.Contains(string(b), `"boot_id": "test-boot"`) {
		t.Errorf("clock.json = %s %v", b, err)
	}
	// 小さなずれは動かさないが、合わせたことにはなる
	fakeClockSet = nil
	r = result(t, d.call("set_time", map[string]any{"unix_ms": time.Now().UnixMilli()}))
	if r["stepped"] != false || len(fakeClockSet) != 0 || r["last_source"] != "unknown" {
		t.Errorf("small offset: %v %v", r, fakeClockSet)
	}
	for _, p := range []map[string]any{nil, {"unix_ms": 0}, {"unix_ms": int64(5e12)}} {
		if c := errCode(d.call("set_time", p)); c != errBadRequest {
			t.Errorf("set_time %v: %s", p, c)
		}
	}

	// デーモンを起動し直しても、同じ起動のあいだなら合わせたまま。Brain を再起動すると未設定に戻る
	d.clock.save() // 返事のあとに書いている分を、書き終えるまで待つ
	dir := filepath.Join(filepath.Dir(d.path), "data")
	if !NewTimeService(OpenStore(dir)).Synced() {
		t.Error("restarting the daemon should keep the synced state")
	}
	readBootID = func() string { return "next-boot" }
	defer func() { readBootID = func() string { return "test-boot" } }()
	if NewTimeService(OpenStore(dir)).Synced() {
		t.Error("after a reboot, the clock should be unsynced again")
	}
}

// 設定 GUI のプレビュー（gui/src/clock.ts）が Go と同じ書式で時刻を書くことを、GUI のテストで確かめるための表。
// 書式やタイムゾーンを足したら LEFTHAND_UPDATE_GOFORMAT=1 go test -run GoFormatTable で書き直す。
const guiGoFormatJSON = "gui/test/fixtures/goformat.json"

func goFormatTable() [][4]string {
	layouts := []string{"15:04", "15:04:05", "3:04 PM", "3:04pm", "2006/01/02", "06-1-2", "Jan 2 Mon", "January _2 Monday",
		"Monday", "Mond", "Janet", "15:04:05.000", "15:04:05.999", "15:04:05,00", ".05", "-0700", "-07:00", "Z07:00", "-07",
		"002 __2", defaultDateFormat, "2006年1月2日({wday}) 15時4分", "1/2 {wday}"}
	zones := []string{"UTC", "Asia/Tokyo", "America/Los_Angeles", "Asia/Kolkata"}
	times := []time.Time{time.Date(2026, 10, 6, 0, 5, 7, 120_000_000, time.UTC), time.Date(2026, 1, 1, 23, 59, 59, 0, time.UTC)}
	var out [][4]string
	for _, tm := range times {
		for _, z := range zones {
			loc, _ := time.LoadLocation(z)
			for _, l := range layouts {
				out = append(out, [4]string{tm.Format(time.RFC3339Nano), z, l, formatClock(tm.In(loc), l)})
			}
		}
	}
	return out
}

func TestGoFormatTable(t *testing.T) {
	want, _ := json.MarshalIndent(goFormatTable(), "", " ")
	want = append(want, '\n')
	if os.Getenv("LEFTHAND_UPDATE_GOFORMAT") == "1" {
		if err := os.WriteFile(guiGoFormatJSON, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(guiGoFormatJSON); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date (%v); run LEFTHAND_UPDATE_GOFORMAT=1 go test -run GoFormatTable", guiGoFormatJSON, err)
	}
}
