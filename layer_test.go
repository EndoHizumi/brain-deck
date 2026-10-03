package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	evdev "github.com/holoplot/go-evdev"
)

const layerYAML = `
touch:
  min_x: 0
  max_x: 400
  min_y: 0
  max_y: 300
  soft_areas:
    home: { x: [3950, 4095], y: [2000, 4095] }
    back: { x: [3950, 4095], y: [0, 1999] }
layers:
  - name: base
    label: "基本"
    keys:
      KEY_Q: B
      KEY_W: E
      KEY_E: LCTRL+Z
      KEY_LEFTALT: { layer_hold: edit }
      KEY_TAB: { layer_toggle: num }
      KEY_PAGEDOWN: { layer_oneshot: edit }
      KEY_INSERT: { layer_to: num }
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { key: B, label: "ブラシ" }
        "1,0": E
        "3,2": { layer_toggle: num, label: "数字" }
    soft_keys:
      home: { layer_to: base }
  - name: edit
    label: "編集"
    keys:
      KEY_Q: LCTRL+C
      KEY_W: none
    touch:
      cells:
        "0,0": LCTRL+X
  - name: num
    label: "数字"
    keys:
      KEY_Q: "1"
    touch:
      cols: 2
      rows: 1
      cells:
        "0,0": "1"
`

func compileYAML(t *testing.T, src string) (*Keymap, []string, error) {
	t.Helper()
	cfg, err := parseConfig([]byte(src))
	if err != nil {
		return nil, nil, err
	}
	return compileKeymap(cfg)
}

// testEngine は送ったレポートをファイルに記録するエンジンを作る。
func testEngine(t *testing.T, src string) (*Engine, func() [][]byte) {
	t.Helper()
	km, _, err := compileYAML(t, src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hidg")
	os.WriteFile(path, nil, 0o600)
	e := NewEngine(km, &State{hid: NewHIDWriter(path), active: map[string]Combo{}})
	read := 0 // 前回までに読んだ分（書き込み側のオフセットがあるので、ファイルは切り詰めない）
	reports := func() [][]byte {
		b, _ := os.ReadFile(path)
		b, read = b[read:], len(b)
		var out [][]byte
		for len(b) >= 8 {
			out = append(out, b[:8])
			b = b[8:]
		}
		return out
	}
	return e, reports
}

func report(mods byte, keys ...byte) []byte {
	r := make([]byte, 8)
	r[0] = mods
	copy(r[2:], keys)
	return r
}

func expectReports(t *testing.T, got [][]byte, want ...[]byte) {
	t.Helper()
	if !bytes.Equal(bytes.Join(got, nil), bytes.Join(want, nil)) {
		t.Fatalf("reports:\n got % x\nwant % x", got, want)
	}
}

const (
	hidB = 0x05
	hidC = 0x06
	hidE = 0x08
	hidX = 0x1b
	hid1 = 0x1e
)

func (e *Engine) top() string { return e.km.Layers[e.View().Top].Name }

func TestLayerHoldAndTransparency(t *testing.T) {
	e, reports := testEngine(t, layerYAML)
	e.PressKey(evdev.KEY_LEFTALT)
	if e.top() != "edit" || e.View().Mode != modeTemp {
		t.Fatalf("top = %s mode %d", e.top(), e.View().Mode)
	}
	e.PressKey(evdev.KEY_Q) // edit: Ctrl+C
	e.ReleaseKey(evdev.KEY_Q)
	e.PressKey(evdev.KEY_E) // edit に書いていないので base の Ctrl+Z
	e.ReleaseKey(evdev.KEY_E)
	e.PressKey(evdev.KEY_W) // none: 何も送らず、base の E にもならない
	e.ReleaseKey(evdev.KEY_W)
	e.ReleaseKey(evdev.KEY_LEFTALT)
	if e.top() != "base" || e.View().Mode != modeBase {
		t.Fatalf("after release top = %s", e.top())
	}
	e.PressKey(evdev.KEY_Q)
	e.ReleaseKey(evdev.KEY_Q)
	expectReports(t, reports(),
		report(0x01, hidC), report(0),
		report(0x01, 0x1d), report(0),
		report(0, hidB), report(0))
}

// あるレイヤーで押したキーは、レイヤーが変わってから離しても、押したときの割り当てで離す。
func TestReleaseUsesPressTimeBinding(t *testing.T) {
	e, reports := testEngine(t, layerYAML)
	e.PressKey(evdev.KEY_LEFTALT)
	e.PressKey(evdev.KEY_Q)         // Ctrl+C
	e.ReleaseKey(evdev.KEY_LEFTALT) // base に戻る
	e.PressKey(evdev.KEY_W)         // base: E
	e.ReleaseKey(evdev.KEY_Q)       // Ctrl+C を離す（B ではない）
	e.ReleaseKey(evdev.KEY_W)
	expectReports(t, reports(),
		report(0x01, hidC),
		report(0x01, hidC, hidE),
		report(0, hidE),
		report(0))

	// 逆向き：base で押したまま hold に入り、離す
	e.PressKey(evdev.KEY_Q) // B
	e.PressKey(evdev.KEY_LEFTALT)
	e.ReleaseKey(evdev.KEY_Q)
	e.ReleaseKey(evdev.KEY_LEFTALT)
	expectReports(t, reports(), report(0, hidB), report(0))
}

func TestLayerToggleAndTo(t *testing.T) {
	e, reports := testEngine(t, layerYAML)
	e.PressKey(evdev.KEY_TAB)
	e.ReleaseKey(evdev.KEY_TAB)
	if e.top() != "num" || e.View().Mode != modeLatched {
		t.Fatalf("toggle on: %s", e.top())
	}
	e.PressKey(evdev.KEY_Q)
	e.ReleaseKey(evdev.KEY_Q)
	// num の上に hold で edit を重ね、外すと num に戻る
	e.PressKey(evdev.KEY_LEFTALT)
	if e.top() != "edit" {
		t.Fatalf("hold over toggle: %s", e.top())
	}
	e.ReleaseKey(evdev.KEY_LEFTALT)
	if e.top() != "num" {
		t.Fatalf("after hold: %s", e.top())
	}
	// num に KEY_TAB はないので base の layer_toggle num が使われ、num が外れる
	e.PressKey(evdev.KEY_TAB)
	e.ReleaseKey(evdev.KEY_TAB)
	if e.top() != "base" {
		t.Fatalf("toggle off: %s", e.top())
	}
	expectReports(t, reports(), report(0, hid1), report(0))

	e.PressKey(evdev.KEY_INSERT) // layer_to num
	e.ReleaseKey(evdev.KEY_INSERT)
	if e.top() != "num" {
		t.Fatalf("layer_to: %s", e.top())
	}
	// 割り当てのないソフトキー back の範囲は、格子の右端として扱う
	h := e.PressTouch(4000, 100)
	e.Release("t")
	if h.Soft != "" || h.Col != 1 || e.top() != "num" {
		t.Fatalf("unassigned soft key: %+v top %s", h, e.top())
	}
	h = e.PressTouch(4000, 3000) // ソフトキー home: layer_to base
	e.Release("t")
	if h.Soft != "home" || !h.Mapped || e.top() != "base" {
		t.Fatalf("soft key: %+v top %s", h, e.top())
	}
}

func TestOneshot(t *testing.T) {
	e, reports := testEngine(t, layerYAML)
	e.PressKey(evdev.KEY_PAGEDOWN)
	e.ReleaseKey(evdev.KEY_PAGEDOWN)
	if e.top() != "edit" || e.View().Mode != modeTemp {
		t.Fatalf("oneshot: %s", e.top())
	}
	e.PressKey(evdev.KEY_Q) // edit の Ctrl+C。押した時点で oneshot は終わる
	if e.top() != "base" {
		t.Fatalf("oneshot not consumed: %s", e.top())
	}
	e.PressKey(evdev.KEY_W) // base の E
	e.ReleaseKey(evdev.KEY_Q)
	e.ReleaseKey(evdev.KEY_W)
	expectReports(t, reports(),
		report(0x01, hidC), report(0x01, hidC, hidE), report(0, hidE), report(0))

	// もう一度押すと取り消せる
	e.PressKey(evdev.KEY_PAGEDOWN)
	e.ReleaseKey(evdev.KEY_PAGEDOWN)
	e.PressKey(evdev.KEY_PAGEDOWN)
	e.ReleaseKey(evdev.KEY_PAGEDOWN)
	if e.top() != "base" {
		t.Fatalf("oneshot not cancelled: %s", e.top())
	}
	// 割り当てのないキーでも使い終わる
	e.PressKey(evdev.KEY_PAGEDOWN)
	e.ReleaseKey(evdev.KEY_PAGEDOWN)
	e.PressKey(evdev.KEY_Z)
	e.ReleaseKey(evdev.KEY_Z)
	if e.top() != "base" {
		t.Fatalf("oneshot not consumed by unmapped key: %s", e.top())
	}
}

func TestTouchGridPerLayer(t *testing.T) {
	e, reports := testEngine(t, layerYAML)
	v := e.View()
	if v.Cols != 4 || v.Rows != 3 {
		t.Fatalf("base grid %dx%d", v.Cols, v.Rows)
	}
	// edit は cols/rows を省略しているので base と同じ 4x3。書いていないセルは base を使う
	e.PressKey(evdev.KEY_LEFTALT)
	v = e.View()
	if v.Cols != 4 || v.Rows != 3 || v.Cells[0].Spec.Key != "LCTRL+X" || v.Cells[1].Spec.Key != "E" {
		t.Fatalf("edit view: %dx%d %+v", v.Cols, v.Rows, v.Cells[:2])
	}
	h := e.PressTouch(10, 10) // 0,0
	e.Release("t")
	if h.Col != 0 || h.Row != 0 || !h.Mapped || h.Gen != v.Gen {
		t.Fatalf("hit %+v", h)
	}
	e.ReleaseKey(evdev.KEY_LEFTALT)
	expectReports(t, reports(), report(0x01, hidX), report(0))

	// num は 2x1。大きさが違うので base のセルは透過しない
	e.PressKey(evdev.KEY_TAB)
	v = e.View()
	if v.Cols != 2 || v.Rows != 1 || v.Cells[1] != nil {
		t.Fatalf("num view: %dx%d %+v", v.Cols, v.Rows, v.Cells)
	}
	h = e.PressTouch(390, 290)
	e.Release("t")
	if h.Col != 1 || h.Row != 0 || h.Mapped {
		t.Fatalf("num hit %+v", h)
	}
	l := buildLayout(e.km, v)
	if l.Title != "数字" || l.Mode != modeLatched || l.Cells[0].Label != "1" {
		t.Fatalf("layout %+v", l)
	}
}

func TestLayerCellView(t *testing.T) {
	km, _, err := compileYAML(t, layerYAML)
	if err != nil {
		t.Fatal(err)
	}
	l := buildLayout(km, km.view([]int{0}))
	if v := l.Cells[11]; !v.Layer || v.Label != "数字" || v.Sub != "切替:数字" {
		t.Errorf("layer cell: %+v", v)
	}
	if l.Title != "基本" || l.badgeRect().Empty() {
		t.Errorf("title %q", l.Title)
	}
}

func TestLegacyConfigIsBaseLayer(t *testing.T) {
	cfg, err := parseConfig([]byte(`
keys:
  KEY_A: LCTRL+Z
touch:
  cols: 2
  rows: 2
  cells:
    "1,1": SPACE
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Layers) != 1 || cfg.Layers[0].Name != "base" || cfg.Keys != nil || cfg.Touch.Cells != nil {
		t.Fatalf("normalized: %+v", cfg)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a := km.lookupKey([]int{0}, evdev.KEY_A); a == nil || a.Combo.Mods != 0x01 {
		t.Fatalf("KEY_A: %+v", a)
	}
	if v := km.view([]int{0}); v.Cols != 2 || v.Cells[3] == nil {
		t.Fatalf("view: %+v", v)
	}
}

func TestJSONConfig(t *testing.T) {
	// 設定 GUI が書く JSON も、そのまま読める
	_, _, err := compileYAML(t, `{"layers":[{"name":"base","keys":{"KEY_Q":"B","KEY_W":{"layer_hold":"x"}}},{"name":"x","keys":{"KEY_Q":{"key":"LCTRL+Z","label":"取り消し"}}}]}`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"unknown layer", `
layers:
  - name: base
    keys: { KEY_Q: { layer_hold: nope } }`, `unknown layer "nope"`},
		{"no way back", `
layers:
  - name: base
    keys: { KEY_Q: { layer_to: a } }
  - name: a
    keys: { KEY_Q: B }`, `no way back to the base layer "base" from layer "a"`},
		{"cycle without base", `
layers:
  - name: base
    keys: { KEY_Q: { layer_to: a } }
  - name: a
    keys: { KEY_Q: { layer_to: b } }
  - name: b
    keys: { KEY_Q: { layer_to: a } }`, `from layer "a", "b"`},
		{"toggle blocked by none", `
layers:
  - name: base
    keys: { KEY_TAB: { layer_toggle: a } }
  - name: a
    keys: { KEY_TAB: none }`, `no way back`},
		{"toggle base", `
layers:
  - name: base
    keys: { KEY_Q: { layer_toggle: base } }`, `cannot target the base layer`},
		{"duplicate name", `
layers:
  - name: base
  - name: base`, `defined twice`},
		{"two actions", `
layers:
  - name: base
    keys: { KEY_Q: { key: B, layer_hold: base } }`, `exactly one`},
		{"typo in action", `
layers:
  - name: base
    keys: { KEY_Q: { layer_hld: a } }`, `unknown field "layer_hld"`},
		{"typo at top", `
layer:
  - name: base`, `field layer not found`},
		{"legacy mixed", `
keys: { KEY_Q: B }
layers:
  - name: base`, `write keys and touch cols/rows/cells inside each layer`},
		{"unknown source key", `
layers:
  - name: base
    keys: { KEY_NOPE: B }`, `unknown source key`},
		{"cell out of grid", `
touch: { min_x: 0, max_x: 1, min_y: 0, max_y: 1 }
layers:
  - name: base
    touch: { cols: 2, rows: 2, cells: { "2,0": B } }`, `out of the 2x2 grid`},
		{"undefined soft key", `
touch: { min_x: 0, max_x: 1, min_y: 0, max_y: 1 }
layers:
  - name: base
    touch: { cols: 1, rows: 1 }
    soft_keys: { home: B }`, `not defined in touch.soft_areas`},
		{"base without grid", `
touch: { min_x: 0, max_x: 1, min_y: 0, max_y: 1 }
layers:
  - name: base`, `needs touch cols and rows`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := compileYAML(t, tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWayBackAccepted(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		// toggle したキーを上のレイヤーに書かなければ、base の toggle で戻れる
		{"toggle falls through", `
layers:
  - name: base
    keys: { KEY_TAB: { layer_toggle: a } }
  - name: a
    keys: { KEY_Q: B }`},
		// 行き来するだけでも、どこかで base に戻れればよい
		{"to chain", `
layers:
  - name: base
    keys: { KEY_Q: { layer_to: a } }
  - name: a
    keys: { KEY_Q: { layer_to: b } }
  - name: b
    keys: { KEY_Q: { layer_to: base } }`},
		// 押しているあいだのレイヤーの中に戻る手段がある
		{"exit inside hold", `
layers:
  - name: base
    keys: { KEY_Q: { layer_to: a } }
  - name: a
    keys: { KEY_Q: { layer_hold: fn } }
  - name: fn
    keys: { KEY_W: { layer_to: base } }`},
		// hold だけで入るレイヤーは、離せば戻るので調べない
		{"hold only", `
layers:
  - name: base
    keys: { KEY_Q: { layer_hold: a } }
  - name: a
    keys: { KEY_Q: none }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := compileYAML(t, tc.src); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnreachableWarning(t *testing.T) {
	_, warns, err := compileYAML(t, `
layers:
  - name: base
  - name: lonely`)
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], `"lonely"`) {
		t.Fatalf("warns %v err %v", warns, err)
	}
}

func TestSampleConfigs(t *testing.T) {
	for _, p := range []string{"config.yaml"} {
		cfg, err := loadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := compileKeymap(cfg); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}
