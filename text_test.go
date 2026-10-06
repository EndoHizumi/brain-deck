package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const textConfig = `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: text, id: build, span: [2, 1], label: "ビルド" }
        "2,0": { widget: text, id: deploy, key: F5 }
        "3,0": { widget: clock }
        "0,1": B
`

func TestTextWidgetConfig(t *testing.T) {
	_, km := compileText(t, textConfig)
	v := buildLayout(km, km.view([]int{0}))
	if w := v.Cells[0].Widget; w == nil || w.Kind != widgetText || w.ID != "build" {
		t.Fatalf("cell 0,0 = %+v", v.Cells[0])
	}
	if a := km.Layers[0].Grid.Cells[cellPos{Col: 2, Row: 0}]; a.Kind != actKey {
		t.Errorf("a text cell with key should send the key, got %v", a.Kind)
	}
	bad := map[string]string{
		`{ widget: text }`:                          "needs id",
		`{ widget: text, id: "a b" }`:               "needs id",
		`{ widget: text, id: build, format: "15" }`: "for widget: clock",
		`{ widget: clock, id: build }`:              "for widget: text",
		`{ id: build, key: B }`:                     "needs widget: text",
	}
	for cell, want := range bad {
		cfg, err := parseConfig([]byte("touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers:\n  - name: base\n    touch:\n      cols: 2\n      rows: 2\n      cells:\n        \"0,0\": " + cell + "\n"))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", cell, err, want)
		}
	}
	cfg, _ := parseConfig([]byte(textConfig))
	if ids := textIDs(cfg); strings.Join(ids, ",") != "build,deploy" {
		t.Errorf("textIDs = %v", ids)
	}
}

func TestTextServiceSetAndPersist(t *testing.T) {
	dir := t.TempDir()
	ts := NewTextService(OpenStore(dir))
	ts.saved = make(chan struct{}, 16)
	now := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	e, set, err := ts.Set(TextRequest{Name: "build", Text: "ok\r\n\tdone", Style: textOK, TTL: 10 * time.Minute, Source: "test"}, now, true)
	if err != nil || !set {
		t.Fatal(err)
	}
	if e.Text != "ok\n done" || e.ExpiresAt == nil || !e.ExpiresAt.Equal(now.Add(10*time.Minute)) || e.ClockUnset {
		t.Errorf("entry = %+v", e)
	}
	if e.Expired(now.Add(9*time.Minute)) || !e.Expired(now.Add(10*time.Minute)) {
		t.Error("expiry")
	}
	<-ts.saved
	// デーモンを起動し直しても残る
	got := NewTextService(OpenStore(dir)).Snapshot()
	if got["build"].Text != "ok\n done" || got["build"].Style != textOK {
		t.Errorf("after restart: %+v", got)
	}
	// 消す
	if _, set, err := ts.Set(TextRequest{Name: "build", Clear: true}, now, true); err != nil || set {
		t.Fatal(err)
	}
	<-ts.saved
	if got := NewTextService(OpenStore(dir)).Snapshot(); len(got) != 0 {
		t.Errorf("after clear: %+v", got)
	}

	for _, r := range []TextRequest{
		{Name: "", Text: "x"}, {Name: "a/b", Text: "x"}, {Name: "a", Text: "x", Style: "blue"},
		{Name: "a", Text: strings.Repeat("あ", textMaxRunes+1)}, {Name: "a", Text: strings.Repeat("\n", textMaxLines)},
		{Name: "a", Text: "bell\a"}, {Name: "a", Text: "x", TTL: textMaxTTL + time.Second},
	} {
		if _, _, err := ts.Set(r, now, true); err == nil {
			t.Errorf("%+v should be rejected", r)
		}
	}
}

func TestTextServiceEvictsOldest(t *testing.T) {
	ts := NewTextService(OpenStore(t.TempDir()))
	now := time.Now()
	for i := 0; i <= textMaxEntries; i++ {
		ts.Set(TextRequest{Name: "t" + string(rune('A'+i/26)) + string(rune('a'+i%26)), Text: "x"}, now.Add(time.Duration(i)*time.Second), true)
	}
	m := ts.Snapshot()
	if len(m) != textMaxEntries {
		t.Fatalf("%d entries", len(m))
	}
	if _, ok := m["tAa"]; ok {
		t.Error("the oldest text should be dropped")
	}
}

// 時刻を合わせる前に書いたテキストは、時刻を合わせたときに、同じだけ時刻を動かす
func TestTextClockStepped(t *testing.T) {
	ts := NewTextService(OpenStore(t.TempDir()))
	brainNow := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) // 37 時間遅れている
	ts.Set(TextRequest{Name: "a", Text: "x", TTL: 10 * time.Minute}, brainNow, false)
	ts.Set(TextRequest{Name: "b", Text: "y", TTL: 10 * time.Minute}, brainNow, true)
	off := 37 * time.Hour
	ts.ClockStepped(off)
	m := ts.Snapshot()
	if !m["a"].ExpiresAt.Equal(brainNow.Add(off+10*time.Minute)) || m["a"].ClockUnset {
		t.Errorf("a = %+v", m["a"])
	}
	if !m["b"].ExpiresAt.Equal(brainNow.Add(10 * time.Minute)) {
		t.Errorf("b should not move: %+v", m["b"])
	}
}

func TestTextLayout(t *testing.T) {
	if _, rows, ok := font.glyph([]rune(textEllipsis)[0]); !ok || rows == nil {
		t.Fatal("the font has no ellipsis")
	}
	lines, s := textLayout("OK", 168, 120)
	if s != textMaxScale || len(lines) != 1 {
		t.Errorf("short text: %q x%d", lines, s)
	}
	// 空白で折り返す
	lines, s = textLayout("deploy failed: staging", 168, 100)
	if strings.Join(lines, " ") != "deploy failed: staging" || len(lines) < 2 {
		t.Errorf("wrap at spaces: %q", lines)
	}
	for _, ln := range lines {
		if font.textWidth(ln)*s > 168 {
			t.Errorf("line %q x%d is wider than the area", ln, s)
		}
	}
	if lines := wrapText([]string{"deploy failed: staging"}, 40); strings.Join(lines, "|") != "deploy|failed:|staging" {
		t.Errorf("wrapText = %q", lines)
	}
	// 等倍でも入らなければ … で切る
	long := strings.Repeat("長い文章", 40)
	lines, s = textLayout(long, 100, 30)
	if s != 1 || len(lines) != 2 || !strings.HasSuffix(lines[1], textEllipsis) {
		t.Errorf("overflow: %q x%d", lines, s)
	}
	for _, ln := range lines {
		if font.textWidth(ln) > 100 {
			t.Errorf("line %q is wider than the area", ln)
		}
	}
}

func TestTextWidgetRedraw(t *testing.T) {
	_, km := compileText(t, textConfig)
	now := time.Date(2026, 10, 6, 9, 5, 7, 0, time.Local)
	exp := now.Add(10 * time.Second)
	env := WidgetEnv{Now: now, TimeSynced: true, Texts: map[string]TextEntry{}}
	d := &Display{cv: NewCanvas(800, 480, 1600, rgb565, 0), env: func() WidgetEnv { return env }}
	l := buildLayout(km, km.view([]int{0}))
	l.W, l.H = 800, 480
	d.layout, d.drawn = l, make([]bool, len(l.Cells))
	d.drawAllLocked(l)
	redrawn := func(what string, want ...int) {
		t.Helper()
		before := append([]string(nil), d.wkeys...)
		d.redrawWidgets()
		ref := NewCanvas(800, 480, 1600, rgb565, 0)
		rl := *l
		rl.Env = env
		drawAll(ref, &rl, d.drawn)
		if !bytes.Equal(ref.pix, d.cv.pix) {
			t.Fatalf("%s: differs from a full redraw", what)
		}
		var got []int
		for i := range before {
			if before[i] != d.wkeys[i] {
				got = append(got, i)
			}
		}
		if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
			t.Fatalf("%s: redrew %v, want %v", what, got, want)
		}
	}
	env.Texts = map[string]TextEntry{"build": {Text: "成功", Style: textOK, ExpiresAt: &exp}}
	redrawn("set build", 0)
	if !d.wakeAt.Equal(exp) {
		t.Errorf("wakeAt = %v, want the expiry %v", d.wakeAt, exp)
	}
	redrawn("nothing changed")
	env.Now = exp
	redrawn("expired", 0)
	env.Texts = map[string]TextEntry{"build": env.Texts["build"], "deploy": {Text: "x", Style: textError}}
	redrawn("set deploy", 2)
}

func TestProtocolSetText(t *testing.T) {
	d := newTestDaemon(t, textConfig)
	d.texts.saved = make(chan struct{}, 16)
	r := result(t, d.call("set_text", map[string]any{"name": "build", "text": "ビルド成功", "style": "ok", "ttl_sec": 600, "source": "test"}))
	if r["shown"] != true || r["cleared"] != false {
		t.Errorf("set_text = %v", r)
	}
	e := r["entry"].(map[string]any)
	if e["text"] != "ビルド成功" || e["style"] != "ok" || e["expired"] != false || e["expires_at"] == nil || e["clock_unset"] != true {
		t.Errorf("entry = %v", e)
	}
	<-d.texts.saved
	b, err := os.ReadFile(filepath.Join(filepath.Dir(d.path), "data", "text.json"))
	if err != nil || !strings.Contains(string(b), "ビルド成功") {
		t.Errorf("text.json = %s %v", b, err)
	}
	r = result(t, d.call("set_text", map[string]any{"name": "other", "text": ""}))
	if r["shown"] != false {
		t.Errorf("an id not in the config: %v", r)
	}
	g := result(t, d.call("get_text", nil))
	texts := g["texts"].(map[string]any)
	if len(texts) != 2 || texts["build"].(map[string]any)["text"] != "ビルド成功" {
		t.Errorf("get_text = %v", g)
	}
	if ids, _ := json.Marshal(g["ids"]); string(ids) != `["build","deploy"]` {
		t.Errorf("ids = %s", ids)
	}
	// 時刻を合わせると、合わせる前に書いたテキストの時刻も動く
	before := d.texts.Snapshot()["build"].ExpiresAt
	result(t, d.call("set_time", map[string]any{"unix_ms": time.Now().Add(time.Hour).UnixMilli()}))
	after := d.texts.Snapshot()["build"]
	if after.ClockUnset || after.ExpiresAt.Sub(*before) < 59*time.Minute {
		t.Errorf("after set_time: %v -> %+v", before, after)
	}
	r = result(t, d.call("set_text", map[string]any{"name": "build", "clear": true}))
	if r["cleared"] != true {
		t.Errorf("clear = %v", r)
	}
	for _, p := range []map[string]any{
		{"text": "x"}, {"name": "build"}, {"name": "build", "text": "x", "style": "pink"},
		{"name": "build", "text": "x", "ttl_sec": 0}, {"name": "bad name", "text": "x"},
	} {
		if c := errCode(d.call("set_text", p)); c != errBadRequest {
			t.Errorf("set_text %v: %s", p, c)
		}
	}
}

// 設定 GUI のプレビュー（gui/src/textwidget.ts）が Go と同じように折り返すことを、GUI のテストで確かめるための表。
// 折り返し方を変えたら LEFTHAND_UPDATE_TEXTLAYOUT=1 go test -run TextLayoutTable で書き直す。
const guiTextLayoutJSON = "gui/test/fixtures/textlayout.json"

type textLayoutCase struct {
	Text  string   `json:"text"`
	W     int      `json:"w"`
	H     int      `json:"h"`
	Lines []string `json:"lines"`
	Scale int      `json:"scale"`
}

func textLayoutTable() []textLayoutCase {
	texts := []string{"OK", "ビルド成功", "ビルド成功 (128 tests)", "deploy failed: staging", "a  b   c", " 先頭の空白",
		"1行目\n2行目\n\n4行目", strings.Repeat("長い文章", 40), "averyveryverylongwordwithoutanyspaces and more",
		"混在 mixed テキスト text 🙂", "x"}
	sizes := [][2]int{{168, 128}, {368, 128}, {100, 30}, {40, 200}, {8, 12}, {568, 288}}
	var out []textLayoutCase
	for _, s := range texts {
		for _, sz := range sizes {
			lines, sc := textLayout(s, sz[0], sz[1])
			out = append(out, textLayoutCase{s, sz[0], sz[1], lines, sc})
		}
	}
	return out
}

func TestTextLayoutTable(t *testing.T) {
	want, _ := json.MarshalIndent(textLayoutTable(), "", " ")
	want = append(want, '\n')
	if os.Getenv("LEFTHAND_UPDATE_TEXTLAYOUT") == "1" {
		if err := os.WriteFile(guiTextLayoutJSON, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(guiTextLayoutJSON); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date (%v); run LEFTHAND_UPDATE_TEXTLAYOUT=1 go test -run TextLayoutTable", guiTextLayoutJSON, err)
	}
}
