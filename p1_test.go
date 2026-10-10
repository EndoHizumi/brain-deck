package main

import (
	"encoding/json"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// フェーズ P1：画面の回転、縦向きの格子、ホーム画面、状態の帯、アイコン

func TestUnrotateInvertsPhys(t *testing.T) {
	for _, rot := range []int{0, 90, 180, 270} {
		cv := NewCanvas(8, 6, 16, rgb565, rot)
		for y := 0; y < cv.H; y++ {
			for x := 0; x < cv.W; x++ {
				X, Y := cv.phys(x, y)
				if gx, gy := unrotate(rot, 8, 6, X, Y); gx != x || gy != y {
					t.Fatalf("rot %d: (%d,%d) -> phys (%d,%d) -> (%d,%d)", rot, x, y, X, Y, gx, gy)
				}
			}
		}
	}
}

// タッチで決まるセルは、そのセルを描いた範囲に、触れた点が入ること（向き、状態の帯、格子の大きさを変えて）。
func TestTouchMapCellMatchesDrawing(t *testing.T) {
	tc := &TouchConfig{MinX: 226, MaxX: 3938, MinY: 3803, MaxY: 384}
	for _, rot := range []int{0, 90, 180, 270} {
		for _, top := range []int{0, barH} {
			if rot == 0 && top == 0 {
				continue // 今までどおり、生の座標で割る
			}
			for _, sz := range [][2]int{{3, 4}, {4, 3}, {5, 2}, {1, 1}} {
				m := touchMap{tc: tc, rot: rot, pw: 800, ph: 480}
				W, H := m.size()
				for x := int32(200); x < 4000; x += 37 {
					for y := int32(300); y < 3900; y += 41 {
						c, r, bar := m.cell(sz[0], sz[1], top, x, y)
						p := m.point(x, y)
						if bar {
							if p.Y >= top {
								t.Fatalf("rot %d top %d: (%d,%d) at %v reported as the bar", rot, top, x, y, p)
							}
							continue
						}
						cell := cellRectIn(c, r, 1, 1, sz[0], sz[1], gridArea(W, H, top))
						if !p.In(cell) && p.In(image.Rect(0, 0, W, H)) {
							t.Fatalf("rot %d top %d grid %v: raw (%d,%d) -> point %v, cell %d,%d %v", rot, top, sz, x, y, p, c, r, cell)
						}
					}
				}
			}
		}
	}
}

// 回した画面で、本体の向きに合わせて触れた位置が、論理の画面のどこになるか。
func TestTouchMapRotatedCorners(t *testing.T) {
	tc := &TouchConfig{MinX: 0, MaxX: 800, MinY: 0, MaxY: 480}
	// 物理の画面の右上（ソフトキーの帯の上の端）は、90 度では論理の左上
	cases := []struct {
		rot  int
		x, y int32
		want image.Point
	}{
		{0, 799, 0, image.Pt(799, 0)},
		{90, 799, 0, image.Pt(0, 0)},
		{90, 0, 479, image.Pt(479, 799)},
		{180, 799, 479, image.Pt(0, 0)},
		{270, 0, 479, image.Pt(0, 0)},
	}
	for _, c := range cases {
		m := touchMap{tc: tc, rot: c.rot, pw: 800, ph: 480}
		if got := m.point(c.x, c.y); got != c.want {
			t.Errorf("rot %d raw (%d,%d): got %v, want %v", c.rot, c.x, c.y, got, c.want)
		}
		f := m.fpoint(c.x, c.y)
		if d := f.X - float64(c.want.X); d < -1.01 || d > 1.01 {
			t.Errorf("rot %d fpoint %v, want about %v", c.rot, f, c.want)
		}
	}
}

func TestPortraitGrid(t *testing.T) {
	src := `
touch: { min_x: 0, max_x: 400, min_y: 0, max_y: 300 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "1,0": { key: A, span: [2, 1] }
        "3,2": B
  - name: own
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": C
    touch_portrait:
      cols: 2
      rows: 5
      cells:
        "1,4": { layer_to: base }
`
	km, _, err := compileYAML(t, src)
	if err != nil {
		t.Fatal(err)
	}
	p := km.Layers[0].PGrid
	if p.Cols != 3 || p.Rows != 4 {
		t.Fatalf("transposed size %dx%d", p.Cols, p.Rows)
	}
	if a := p.Cells[cellPos{0, 1}]; a == nil || a.SpanW != 1 || a.SpanH != 2 || a.Spec.Key != "A" {
		t.Fatalf("transposed span cell: %+v", a)
	}
	if a := p.Cells[cellPos{2, 3}]; a == nil || a.Spec.Key != "B" {
		t.Fatalf("transposed 3,2: %+v", a)
	}
	if km.Layers[0].Grid.Cells[cellPos{1, 0}].SpanW != 2 {
		t.Fatal("the landscape grid changed")
	}
	if g := km.Layers[1].PGrid; g.Cols != 2 || g.Rows != 5 || g.Cells[cellPos{1, 4}] == nil {
		t.Fatalf("touch_portrait: %+v", g)
	}
	v := km.view([]int{0}, true)
	if v.Cols != 3 || v.Rows != 4 {
		t.Fatalf("portrait view %dx%d", v.Cols, v.Rows)
	}

	// stretch：横向きの格子をそのまま使う
	cfg, err := parseConfig([]byte(src + "display: { portrait_grid: stretch }\n"))
	if err != nil {
		t.Fatal(err)
	}
	km2, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if km2.Layers[0].PGrid != km2.Layers[0].Grid {
		t.Fatal("stretch should reuse the landscape grid")
	}

	for _, bad := range []struct{ src, want string }{
		{"display: { portrait_grid: rotate }\n", "portrait_grid must be"},
	} {
		if _, err := parseConfig([]byte(src + bad.src)); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q: %v", bad.src, err)
		}
	}
	if _, _, err := compileYAML(t, strings.Replace(src, `"1,4": { layer_to: base }`, `"2,4": X`, 1)); err == nil || !strings.Contains(err.Error(), "touch_portrait cell") {
		t.Errorf("out of the portrait grid: %v", err)
	}
	noTouch := `
touch: { min_x: 0, max_x: 400, min_y: 0, max_y: 300 }
layers:
  - name: base
    touch: { cols: 2, rows: 2 }
  - name: x
    touch_portrait: { cols: 1, rows: 1 }
`
	if _, _, err := compileYAML(t, noTouch); err == nil || !strings.Contains(err.Error(), "touch_portrait needs touch") {
		t.Errorf("touch_portrait without touch: %v", err)
	}
}

const p1YAML = `
touch:
  min_x: 0
  max_x: 400
  min_y: 0
  max_y: 300
  soft_areas:
    home: { x: [3950, 4095], y: [3000, 4095] }
    left: { x: [3950, 4095], y: [2000, 2999] }
    right: { x: [3950, 4095], y: [1000, 1999] }
layers:
  - name: base
    status_bar: false
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { rotate: next }
        "1,0": { rotate: "180" }
        "3,2": { layer_to: tools }
    soft_keys:
      home: B
  - name: tools
    touch:
      cols: 2
      rows: 2
      cells:
        "0,0": C
  - name: hold
    status_bar: true
    keys:
      KEY_A: D
home:
  apps:
    - { name: 左手, icon: keyboard, layer: base }
    - { name: 道具, icon: stopwatch, layer: tools }
    - { name: a3, layer: tools }
    - { name: a4, layer: tools }
    - { name: a5, layer: tools }
    - { name: a6, layer: tools }
    - { name: a7, layer: tools }
    - { name: a8, layer: tools }
    - { name: a9, layer: tools }
    - { name: a10, layer: tools }
    - { name: a11, layer: tools }
    - { name: a12, layer: tools }
    - { name: 次, icon: info, layer: base }
status_bar: { show: true }
`

// raw は、800x480（向き 0）の画面の点を、p1YAML のタッチの生の座標にする。
func raw(x, y int) (int32, int32) { return int32(x * 400 / 800), int32(y * 300 / 480) }

func TestHomeEngine(t *testing.T) {
	e, _ := testEngine(t, p1YAML)
	km := e.km
	h := km.Home
	if h == nil || km.Layers[h.Layer].Name != "home" {
		t.Fatal("no home layer")
	}
	v := e.View()
	if v.Top != h.Layer || !v.Bar || v.Mode != modeBase {
		t.Fatalf("start: top %d bar %v mode %v", v.Top, v.Bar, v.Mode)
	}
	// 4×3 のアプリの格子。2 番目（道具）は、帯の下の 1 行目の 2 列目
	area := gridArea(800, 480, barH)
	g := homeGeometry(area, h, false, 0)
	if g.pages != 2 || g.per != 12 || g.nav.Empty() {
		t.Fatalf("geometry: %+v", g)
	}
	tap := func(p image.Point) TouchHit {
		x, y := raw(p.X, p.Y)
		hit := e.PressTouch(x, y)
		e.Release("t")
		return hit
	}
	center := func(r image.Rectangle) image.Point { return r.Min.Add(r.Size().Div(2)) }
	if hit := tap(center(g.slots[1])); !hit.Own {
		t.Fatalf("home tap: %+v", hit)
	}
	if v := e.View(); km.Layers[v.Top].Name != "tools" || !v.Bar {
		t.Fatalf("after the app: top %q bar %v", km.Layers[v.Top].Name, v.Bar)
	}
	// HOME はどこからでもホーム画面へ（base の soft_keys の home: B より優先）
	homeX, homeY := int32(4000), int32(3500)
	e.PressTouch(homeX, homeY)
	e.Release("t")
	if e.View().Top != h.Layer {
		t.Fatal("HOME did not go home")
	}
	// ページ送り：下の帯の ▶ と、画面右の帯の ◀
	tap(center(g.next))
	if h.Page() != 1 {
		t.Fatalf("page after ▶: %d", h.Page())
	}
	tap(center(g.next)) // 最後のページ
	if h.Page() != 1 {
		t.Fatalf("page past the end: %d", h.Page())
	}
	g2 := homeGeometry(area, h, false, 1)
	if g2.first != 12 || g2.last != 13 {
		t.Fatalf("page 2 range %d..%d", g2.first, g2.last)
	}
	if hit := tap(center(g2.slots[5])); hit.Mapped || e.View().Top != h.Layer {
		t.Fatal("an empty slot did something")
	}
	e.PressTouch(4000, 2500) // left
	e.Release("t")
	if h.Page() != 0 {
		t.Fatalf("page after ◀: %d", h.Page())
	}
	h.SetPage(1)
	e.PressTouch(homeX, homeY) // ホーム画面で HOME：1 ページ目へ
	e.Release("t")
	if h.Page() != 0 {
		t.Fatalf("HOME on home: page %d", h.Page())
	}
	tap(center(g.slots[0]))
	if v := e.View(); v.Top != 0 || v.Bar {
		t.Fatalf("base app: top %d bar %v", v.Top, v.Bar)
	}
	// 設定を差し替えたらホーム画面に戻る
	e.Reload(km)
	if e.View().Top != h.Layer {
		t.Fatal("reload did not start at home")
	}
}

func TestStatusBarRule(t *testing.T) {
	km, _, err := compileYAML(t, p1YAML)
	if err != nil {
		t.Fatal(err)
	}
	if km.view([]int{0}).Bar {
		t.Error("base has status_bar: false")
	}
	if !km.view([]int{0, 1}).Bar {
		t.Error("tools should use status_bar.show")
	}
	if !km.view([]int{0, 2}).Bar {
		t.Error("hold (no touch) writes status_bar: true")
	}
	km.Layers[2].StatusBar = nil
	if km.view([]int{0, 2}).Bar {
		t.Error("hold without status_bar should follow base")
	}
	if got := barItems(barItemsDefault, WidgetEnv{Now: time.Date(2026, 10, 6, 9, 41, 0, 0, time.UTC), MouseOff: true, Link: "suspended"}); len(got) != 4 ||
		got[0].text != "未同期" || got[1].text != "PC休止" || got[2].text != "電池?" || got[3].text != "09:41" {
		t.Errorf("bar items: %+v", got)
	}
}

func TestHomeErrors(t *testing.T) {
	cases := []struct{ change, want string }{
		{"layer: base }\n    - { name: x, icon: nope, layer: base", "icon must be a built-in icon"},
		{"layer: base }\n    - { name: x, layer: missing", `unknown layer "missing"`},
		{"layer: base }\n    - { name: '', layer: base", "name must be 1..16"},
	}
	for _, c := range cases {
		src := strings.Replace(p1YAML, "layer: base }\n    - { name: 道具", c.change+" }\n    - { name: 道具", 1)
		if _, _, err := compileYAML(t, src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.want, err)
		}
	}
	if _, _, err := compileYAML(t, strings.Replace(p1YAML, "  - name: hold", "  - name: home", 1)); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("layer named home: %v", err)
	}
	if _, _, err := compileYAML(t, strings.Replace(p1YAML, "    home: { x:", "    menu: { x:", 1)); err == nil || !strings.Contains(err.Error(), `soft key "home"`) {
		t.Errorf("no home soft area: %v", err)
	}
	if _, _, err := compileYAML(t, strings.Replace(p1YAML, "status_bar: { show: true }", "status_bar: { items: [time, clock] }", 1)); err == nil || !strings.Contains(err.Error(), "status_bar.items") {
		t.Errorf("bad item: %v", err)
	}
	_, warns, err := compileYAML(t, p1YAML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warns, "\n"), `layer "base" soft key "home": with home`) {
		t.Errorf("warnings: %q", warns)
	}
	// home を書かなければ今までどおり（base から始まる）
	plain := strings.Replace(p1YAML[:strings.Index(p1YAML, "home:\n  apps")], `"0,0": C`, `"0,0": { layer_to: base }`, 1)
	e, _ := testEngine(t, plain)
	if e.View().Top != 0 || e.km.Home != nil {
		t.Fatal("without home, the engine should start at base")
	}
}

func TestRotateAction(t *testing.T) {
	e, _ := testEngine(t, p1YAML)
	dir := t.TempDir()
	rs := NewRotateStore(OpenStore(dir))
	e.SetRotation(rs.Initial(0), 800, 480, rs)
	// base に移って、0,0 の rotate: next を押す
	e.mu.Lock()
	e.press("x", &Action{Kind: actTo, Layer: 0})
	e.mu.Unlock()
	e.Release("x")
	x, y := raw(100, 80)
	e.PressTouch(x, y)
	e.Release("t")
	if e.Rotation() != 90 {
		t.Fatalf("rotation %d", e.Rotation())
	}
	v := e.View()
	if !v.Portrait || v.Cols != 3 || v.Rows != 4 || v.Rot != 90 {
		t.Fatalf("view after rotate: %dx%d portrait %v rot %d", v.Cols, v.Rows, v.Portrait, v.Rot)
	}
	// 縦向きの論理の左上のセル（転置した格子の 0,0 = 横向きの 0,0 の rotate: next）は、物理の右上
	tm := e.touchMapLocked()
	c, r, _ := tm.cell(v.Cols, v.Rows, v.barTop(), 399, 10)
	if c != 0 || r != 0 {
		t.Fatalf("physical top-right -> cell %d,%d", c, r)
	}
	// 保存（書き込みは待たない）
	var rec rotateRecord
	for i := 0; i < 100; i++ {
		if err := OpenStore(dir).Load(displayStoreName, &rec); err == nil && rec.Rotate == 90 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Rotate != 90 || rec.ConfigRotate != 0 {
		t.Fatalf("saved %+v", rec)
	}
	if got := NewRotateStore(OpenStore(dir)).Initial(0); got != 90 {
		t.Fatalf("initial after restart %d", got)
	}
	if got := NewRotateStore(OpenStore(dir)).Initial(180); got != 180 {
		t.Fatalf("display.rotate changed: initial %d, want the config's 180", got)
	}
	defer rs.Wait()
	if got := e.Rotate(180); got != 180 {
		t.Fatalf("Rotate(180) = %d", got)
	}
	if e.View().Portrait {
		t.Fatal("180 is landscape")
	}
	for _, s := range []string{"next", "0", "90", "180", "270"} {
		if _, ok := parseRotate(s); !ok {
			t.Errorf("parseRotate(%q)", s)
		}
	}
	for _, s := range []string{"45", "", "prev"} {
		if _, ok := parseRotate(s); ok {
			t.Errorf("parseRotate(%q) accepted", s)
		}
	}
	if _, _, err := compileYAML(t, strings.Replace(p1YAML, `rotate: "180"`, `rotate: "45"`, 1)); err == nil || !strings.Contains(err.Error(), "rotate must be") {
		t.Errorf("rotate 45: %v", err)
	}
}

// ソフトキーは、回しても生の座標のまま判定する（ベゼルに印刷された帯は動かない）。
func TestSoftKeysIgnoreRotation(t *testing.T) {
	e, _ := testEngine(t, p1YAML)
	for _, r := range []int{0, 90, 180, 270} {
		e.Rotate(r)
		e.mu.Lock()
		e.press("x", &Action{Kind: actTo, Layer: 1})
		e.mu.Unlock()
		e.Release("x")
		if hit := e.PressTouch(4000, 3500); hit.Soft != "home" {
			t.Fatalf("rot %d: %+v", r, hit)
		}
		e.Release("t")
		if e.View().Top != e.km.Home.Layer {
			t.Fatalf("rot %d: HOME did not go home", r)
		}
	}
}

func TestIconsWellFormed(t *testing.T) {
	for name, a := range iconArts {
		if _, ok := iconPalette[a.tile]; !ok {
			t.Errorf("%s: tile %q is not in the palette", name, a.tile)
		}
		for y, row := range a.glyph {
			if len(row) != 12 {
				t.Errorf("%s row %d: %d characters", name, y, len(row))
			}
			for i := 0; i < len(row); i++ {
				if _, ok := iconPalette[row[i]]; !ok && row[i] != '.' {
					t.Errorf("%s row %d: %q is not in the palette", name, y, row[i])
				}
			}
		}
	}
	for _, k := range append(append([]string{}, widgetKinds...), "keyboard", "terminal", "settings", "memo", "home", "app") {
		if k == widgetPad {
			k = "trackpad"
		}
		if builtinIcons[k] == nil {
			t.Errorf("no built-in icon for %q", k)
		}
	}
}

// IconsJSON：GUI に同梱するアイコンの表（gui/src/icons.json）が、Go の表と同じこと。
func TestIconsJSON(t *testing.T) {
	type iconsFile struct {
		Palette map[string][3]uint8 `json:"palette"`
		Icons   map[string][]string `json:"icons"`
		Names   []string            `json:"names"`
	}
	f := iconsFile{Palette: map[string][3]uint8{}, Icons: builtinIcons}
	for c, rgb := range iconPalette {
		f.Palette[string(c)] = [3]uint8{rgb.R, rgb.G, rgb.B}
	}
	for n := range builtinIcons {
		f.Names = append(f.Names, n)
	}
	sort.Strings(f.Names)
	want, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("gui", "src", "icons.json")
	if os.Getenv("LEFTHAND_UPDATE_ICONS") != "" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is out of date; run LEFTHAND_UPDATE_ICONS=1 go test -run IconsJSON", path)
	}
}

// 回した画面と、ホーム画面と、状態の帯を描いても落ちず、帯の下に格子が並ぶこと。
func TestRenderRotatedHome(t *testing.T) {
	cfg, err := loadConfig("config/pda-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, rot := range []int{0, 90, 180, 270} {
		out := filepath.Join(t.TempDir(), "x.png")
		env := WidgetEnv{Now: time.Date(2026, 10, 6, 9, 41, 0, 0, time.Local), TimeSynced: true}
		for _, layer := range []string{"", "tools", "system", "base", "mouse"} {
			if err := renderPNG(cfg, km, out, layer, "0,0", "", 800, 480, rot, 1, env, nil); err != nil {
				t.Fatalf("rot %d layer %q: %v", rot, layer, err)
			}
		}
	}
}
