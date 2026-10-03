package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	evdev "github.com/holoplot/go-evdev"
	"gopkg.in/yaml.v3"
)

// ---------- 設定ファイルの構造 ----------
//
// 設定 GUI（WebSerial）とも共有するので、YAML と JSON のどちらでも同じ構造で書ける。
// yaml と json のタグは同じ名前にしてある。形式の説明は docs/config.md。

type Config struct {
	HIDDevice string                `yaml:"hid_device,omitempty" json:"hid_device,omitempty"`
	Keyboard  string                `yaml:"keyboard,omitempty" json:"keyboard,omitempty"` // "/dev/input/eventN" またはデバイス名
	Keys      map[string]ActionSpec `yaml:"keys,omitempty" json:"keys,omitempty"`         // 旧形式。base レイヤーとして読む
	Touch     *TouchConfig          `yaml:"touch,omitempty" json:"touch,omitempty"`
	Layers    []LayerConfig         `yaml:"layers,omitempty" json:"layers,omitempty"`
	Display   *DisplayConfig        `yaml:"display,omitempty" json:"display,omitempty"`
}

// TouchConfig はタッチパネルそのものの設定（どのレイヤーでも共通）。
type TouchConfig struct {
	Device    string                `yaml:"device,omitempty" json:"device,omitempty"`   // "/dev/input/eventN" またはデバイス名
	SwapXY    bool                  `yaml:"swap_xy,omitempty" json:"swap_xy,omitempty"` // パネルのX軸が画面の縦方向のとき
	MinX      int32                 `yaml:"min_x" json:"min_x"`
	MaxX      int32                 `yaml:"max_x" json:"max_x"`
	MinY      int32                 `yaml:"min_y" json:"min_y"`
	MaxY      int32                 `yaml:"max_y" json:"max_y"`
	SoftAreas map[string]SoftArea   `yaml:"soft_areas,omitempty" json:"soft_areas,omitempty"` // 画面外のタッチ領域（ソフトキー）
	Cols      int                   `yaml:"cols,omitempty" json:"cols,omitempty"`             // 旧形式。base レイヤーの touch として読む
	Rows      int                   `yaml:"rows,omitempty" json:"rows,omitempty"`             // 同上
	Cells     map[string]ActionSpec `yaml:"cells,omitempty" json:"cells,omitempty"`           // 同上
}

// SoftArea はソフトキーの範囲。パネルの生の座標で書く（swap_xy の影響を受けない）。
// [a, b] の順はどちらでもよい。
type SoftArea struct {
	X [2]int32 `yaml:"x" json:"x"`
	Y [2]int32 `yaml:"y" json:"y"`
}

func (a SoftArea) contains(x, y int32) bool {
	in := func(v int32, r [2]int32) bool { return min(r[0], r[1]) <= v && v <= max(r[0], r[1]) }
	return in(x, a.X) && in(y, a.Y)
}

// LayerConfig は 1 つのレイヤー。書いていないキー・セル・ソフトキーは下のレイヤーの割り当てを使う。
type LayerConfig struct {
	Name     string                `yaml:"name" json:"name"`
	Label    string                `yaml:"label,omitempty" json:"label,omitempty"` // 画面に出す名前。省略すると name
	Keys     map[string]ActionSpec `yaml:"keys,omitempty" json:"keys,omitempty"`
	Touch    *GridConfig           `yaml:"touch,omitempty" json:"touch,omitempty"`
	SoftKeys map[string]ActionSpec `yaml:"soft_keys,omitempty" json:"soft_keys,omitempty"`
}

// GridConfig はレイヤーのタッチの格子。cols/rows を省略すると base と同じ大きさ。
type GridConfig struct {
	Cols  int                   `yaml:"cols,omitempty" json:"cols,omitempty"`
	Rows  int                   `yaml:"rows,omitempty" json:"rows,omitempty"`
	Cells map[string]ActionSpec `yaml:"cells,omitempty" json:"cells,omitempty"` // "列,行"
}

// ActionSpec はキー・セル・ソフトキー 1 つの割り当て。
// `B`、`none`、`{ key: B, label: "ブラシ" }`、`{ layer_hold: edit }` のように書ける。
// key と layer_* のうち、ちょうど 1 つを書く。
type ActionSpec struct {
	Key          string `yaml:"key,omitempty" json:"key,omitempty"` // 送るキー。"none" で無効（下のレイヤーを使わない）
	LayerHold    string `yaml:"layer_hold,omitempty" json:"layer_hold,omitempty"`
	LayerToggle  string `yaml:"layer_toggle,omitempty" json:"layer_toggle,omitempty"`
	LayerOneshot string `yaml:"layer_oneshot,omitempty" json:"layer_oneshot,omitempty"`
	LayerTo      string `yaml:"layer_to,omitempty" json:"layer_to,omitempty"`
	Label        string `yaml:"label,omitempty" json:"label,omitempty"`
}

var actionFields = map[string]bool{
	"key": true, "layer_hold": true, "layer_toggle": true, "layer_oneshot": true, "layer_to": true, "label": true,
}

func (a *ActionSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value == "" {
			return fmt.Errorf("line %d: empty key", n.Line)
		}
		*a = ActionSpec{Key: n.Value}
		return nil
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if k := n.Content[i]; !actionFields[k.Value] {
				return fmt.Errorf("line %d: unknown field %q (key, layer_hold, layer_toggle, layer_oneshot, layer_to, label)", k.Line, k.Value)
			}
		}
	}
	type plain ActionSpec // UnmarshalYAML を持たない型で中身を読む
	if err := n.Decode((*plain)(a)); err != nil {
		return err
	}
	if a.count() != 1 {
		return fmt.Errorf("line %d: write exactly one of key, layer_hold, layer_toggle, layer_oneshot, layer_to", n.Line)
	}
	return nil
}

func (a ActionSpec) count() int {
	n := 0
	for _, s := range []string{a.Key, a.LayerHold, a.LayerToggle, a.LayerOneshot, a.LayerTo} {
		if s != "" {
			n++
		}
	}
	return n
}

// DisplayConfig は画面表示の設定。display を省略してもタッチがあれば表示する。
type DisplayConfig struct {
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"` // false で画面を使わない
	Device  string `yaml:"device,omitempty" json:"device,omitempty"`   // フレームバッファ
	VT      int    `yaml:"vt,omitempty" json:"vt,omitempty"`           // 使う VT の番号。0 なら tty8 以降の空きを使う
	Rotate  int    `yaml:"rotate,omitempty" json:"rotate,omitempty"`   // 画面の回転（0, 90, 180, 270）
}

const (
	defaultHID      = "/dev/hidg0"
	defaultKeyboard = "brain-kbd-i2c"
	defaultTouch    = "mxs-lradc-ts"
	defaultFB       = "/dev/fb0"
)

// loadConfig は設定を読み、既定値を入れ、旧形式を layers の形にそろえる。
// 割り当ての中身の検証は compileKeymap で行う。
func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))                        // JSON も YAML として読める
	dec.KnownFields(true)                                               // 書き間違えた項目を見逃さない
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) { // 空のファイルは io.EOF
		return nil, err
	}
	if cfg.HIDDevice == "" {
		cfg.HIDDevice = defaultHID
	}
	if cfg.Keyboard == "" {
		cfg.Keyboard = defaultKeyboard
	}
	if cfg.Touch != nil && cfg.Touch.Device == "" {
		cfg.Touch.Device = defaultTouch
	}
	if cfg.Display == nil {
		cfg.Display = &DisplayConfig{}
	}
	if cfg.Display.Device == "" {
		cfg.Display.Device = defaultFB
	}
	switch cfg.Display.Rotate {
	case 0, 90, 180, 270:
	default:
		return nil, errors.New("display.rotate must be 0, 90, 180 or 270")
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// normalize は旧形式（トップレベルの keys と touch.cells）を base レイヤーに移す。
func (c *Config) normalize() error {
	t := c.Touch
	legacyTouch := t != nil && (t.Cols != 0 || t.Rows != 0 || t.Cells != nil)
	if len(c.Layers) > 0 {
		if c.Keys != nil || legacyTouch {
			return errors.New("with layers, write keys and touch cols/rows/cells inside each layer")
		}
		return nil
	}
	base := LayerConfig{Name: "base", Keys: c.Keys}
	if t != nil {
		base.Touch = &GridConfig{Cols: t.Cols, Rows: t.Rows, Cells: t.Cells}
		t.Cols, t.Rows, t.Cells = 0, 0, nil
	}
	c.Keys = nil
	c.Layers = []LayerConfig{base}
	return nil
}

// displayEnabled は画面を使うかどうか。タッチがなければ描くものがない。
func (c *Config) displayEnabled() bool {
	return c.Touch != nil && (c.Display.Enabled == nil || *c.Display.Enabled)
}

// ---------- 割り当ての組み立てと検証 ----------

type ActKind uint8

const (
	actNone    ActKind = iota // 何もしない（下のレイヤーも使わない）
	actKey                    // キーを送る
	actHold                   // 押しているあいだレイヤーを重ねる
	actToggle                 // 押すたびにレイヤーを重ねる・外す
	actOneshot                // 次の 1 キーだけレイヤーを重ねる
	actTo                     // base とそのレイヤーだけにする
)

var actNames = [...]string{"none", "key", "layer_hold", "layer_toggle", "layer_oneshot", "layer_to"}

func (k ActKind) String() string { return actNames[k] }

func (k ActKind) isLayer() bool { return k >= actHold }

// Action は組み立て済みの割り当て。
type Action struct {
	Kind  ActKind
	Combo Combo // actKey のとき
	Layer int   // layer_* の行き先
	Spec  ActionSpec
}

type cellPos struct{ Col, Row int }

type Grid struct {
	Cols, Rows int
	Cells      map[cellPos]*Action
}

type Layer struct {
	Name, Label string
	Keys        map[evdev.EvCode]*Action
	Grid        *Grid // nil なら下のレイヤーの格子をそのまま使う
	Soft        map[string]*Action
}

func (l *Layer) title() string {
	if l.Label != "" {
		return l.Label
	}
	return l.Name
}

type namedArea struct {
	Name string
	SoftArea
}

// Keymap は全レイヤーの割り当て。Layers[0] が base。
type Keymap struct {
	Layers []*Layer
	Areas  []namedArea // 名前順。重なったときは先のものを使う
	Touch  *TouchConfig
}

func (km *Keymap) area(name string) (SoftArea, bool) {
	for _, a := range km.Areas {
		if a.Name == name {
			return a.SoftArea, true
		}
	}
	return SoftArea{}, false
}

// compileKeymap は設定を検証し、実行時に使う形にする。
// warns は動作には差し支えないが、気づいてほしいこと（使われないレイヤーなど）。
func compileKeymap(cfg *Config) (km *Keymap, warns []string, err error) {
	km = &Keymap{Touch: cfg.Touch}
	if len(cfg.Layers) == 0 {
		return nil, nil, errors.New("no layers")
	}
	index := map[string]int{}
	for i, lc := range cfg.Layers {
		if lc.Name == "" {
			return nil, nil, fmt.Errorf("layers[%d]: name is required", i)
		}
		if _, dup := index[lc.Name]; dup {
			return nil, nil, fmt.Errorf("layer %q is defined twice", lc.Name)
		}
		index[lc.Name] = i
	}
	if cfg.Touch != nil {
		names := make([]string, 0, len(cfg.Touch.SoftAreas))
		for n := range cfg.Touch.SoftAreas {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			km.Areas = append(km.Areas, namedArea{n, cfg.Touch.SoftAreas[n]})
		}
	}

	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	action := func(where string, self int, s ActionSpec) *Action {
		a := &Action{Spec: s}
		target := ""
		switch {
		case strings.EqualFold(s.Key, "none"):
			a.Kind = actNone
		case s.Key != "":
			a.Kind = actKey
			c, err := parseCombo(s.Key)
			if err != nil {
				fail("%s: %v", where, err)
			}
			a.Combo = c
		case s.LayerHold != "":
			a.Kind, target = actHold, s.LayerHold
		case s.LayerToggle != "":
			a.Kind, target = actToggle, s.LayerToggle
		case s.LayerOneshot != "":
			a.Kind, target = actOneshot, s.LayerOneshot
		case s.LayerTo != "":
			a.Kind, target = actTo, s.LayerTo
		default:
			fail("%s: empty assignment", where)
		}
		if a.Kind.isLayer() {
			t, ok := index[target]
			switch {
			case !ok:
				fail("%s: %s refers to unknown layer %q", where, a.Kind, target)
			case t == 0 && (a.Kind == actToggle):
				fail("%s: layer_toggle cannot target the base layer %q (use layer_to)", where, target)
			}
			a.Layer = t
		}
		return a
	}

	var baseGrid *Grid
	for li, lc := range cfg.Layers {
		l := &Layer{Name: lc.Name, Label: lc.Label, Keys: map[evdev.EvCode]*Action{}, Soft: map[string]*Action{}}
		for name, s := range lc.Keys {
			where := fmt.Sprintf("layer %q key %s", lc.Name, name)
			code, ok := evdev.KEYFromString[name]
			if !ok {
				fail("%s: unknown source key (use Linux names such as KEY_A)", where)
				continue
			}
			l.Keys[code] = action(where, li, s)
		}
		if lc.Touch != nil {
			if cfg.Touch == nil {
				fail("layer %q has touch, but the touch section (device and calibration) is missing", lc.Name)
			}
			g := &Grid{Cols: lc.Touch.Cols, Rows: lc.Touch.Rows, Cells: map[cellPos]*Action{}}
			if (g.Cols == 0) != (g.Rows == 0) {
				fail("layer %q touch: write both cols and rows, or neither", lc.Name)
			}
			if g.Cols == 0 && baseGrid != nil {
				g.Cols, g.Rows = baseGrid.Cols, baseGrid.Rows
			}
			if g.Cols <= 0 || g.Rows <= 0 || g.Cols > 16 || g.Rows > 16 {
				fail("layer %q touch: cols and rows must be 1..16", lc.Name)
				g.Cols, g.Rows = max(g.Cols, 1), max(g.Rows, 1)
			}
			for k, s := range lc.Touch.Cells {
				where := fmt.Sprintf("layer %q touch cell %q", lc.Name, k)
				var p cellPos
				if _, err := fmt.Sscanf(k, "%d,%d", &p.Col, &p.Row); err != nil ||
					p.Col < 0 || p.Col >= g.Cols || p.Row < 0 || p.Row >= g.Rows {
					fail("%s: out of the %dx%d grid", where, g.Cols, g.Rows)
					continue
				}
				if _, dup := g.Cells[p]; dup {
					fail("%s: defined twice", where)
				}
				g.Cells[p] = action(where, li, s)
			}
			l.Grid = g
			if li == 0 {
				baseGrid = g
			}
		} else if li == 0 && cfg.Touch != nil {
			fail("base layer %q needs touch cols and rows", lc.Name)
		}
		for name, s := range lc.SoftKeys {
			where := fmt.Sprintf("layer %q soft key %q", lc.Name, name)
			if _, ok := km.area(name); !ok {
				fail("%s: not defined in touch.soft_areas", where)
			}
			l.Soft[name] = action(where, li, s)
		}
		km.Layers = append(km.Layers, l)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, nil, errors.New(strings.Join(errs, "\n"))
	}

	if err := km.checkWayBack(); err != nil {
		return nil, nil, err
	}
	warns = km.unreachable()
	return km, warns, nil
}

// actionsOn は、stack を重ねたときに使える割り当てをすべて返す（透過を解決したもの）。
func (km *Keymap) actionsOn(stack []int) []*Action {
	var out []*Action
	seen := map[evdev.EvCode]bool{}
	for i := len(stack) - 1; i >= 0; i-- {
		for code, a := range km.Layers[stack[i]].Keys {
			if !seen[code] {
				seen[code] = true
				out = append(out, a)
			}
		}
	}
	v := km.view(stack)
	for _, a := range v.Cells {
		if a != nil {
			out = append(out, a)
		}
	}
	for _, a := range v.Soft {
		out = append(out, a)
	}
	return out
}

// checkWayBack は、押すたびに切り替わる（layer_toggle）か、移る（layer_to）ことで
// 入るレイヤーのそれぞれに、base へ戻る手段があることを確かめる。
// layer_hold と layer_oneshot は、離せば（次のキーで）自然に戻るので対象外。
//
// 戻る手段とみなすもの（そのレイヤーを base の上に重ねたときに押せるもの）：
//   - layer_to base
//   - 自分自身の layer_toggle（重なりから外れる）
//   - 戻れるレイヤーへの layer_to
//   - 押しているあいだ（次の 1 キーだけ）重なるレイヤーの中の、上のいずれか
//
// layer_to どうしで行き来するだけで base に戻れない輪も、ここで見つかる。
func (km *Keymap) checkWayBack() error {
	n := len(km.Layers)
	latched := make([]bool, n) // layer_toggle / layer_to で入る
	for _, l := range km.Layers {
		for _, a := range km.allActions(l) {
			if a.Kind == actToggle || a.Kind == actTo {
				latched[a.Layer] = true
			}
		}
	}
	good := make([]bool, n)
	good[0] = true
	exits := func(acts []*Action, self int) bool {
		for _, a := range acts {
			switch {
			case a.Kind == actTo && good[a.Layer]:
				return true
			case a.Kind == actToggle && a.Layer == self:
				return true
			}
		}
		return false
	}
	for changed := true; changed; {
		changed = false
		for i := 1; i < n; i++ {
			if good[i] {
				continue
			}
			acts := km.actionsOn([]int{0, i})
			ok := exits(acts, i)
			for _, a := range acts {
				if !ok && (a.Kind == actHold || a.Kind == actOneshot) && a.Layer != i {
					ok = exits(km.actionsOn([]int{0, i, a.Layer}), i)
				}
			}
			if ok {
				good[i] = true
				changed = true
			}
		}
	}
	var bad []string
	for i := 1; i < n; i++ {
		if latched[i] && !good[i] {
			bad = append(bad, fmt.Sprintf("%q", km.Layers[i].Name))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("no way back to the base layer %q from layer %s: "+
			"put `layer_to: %s` (or a layer_toggle of the layer itself) on a key or cell that is usable there",
			km.Layers[0].Name, strings.Join(bad, ", "), km.Layers[0].Name)
	}
	return nil
}

func (km *Keymap) allActions(l *Layer) []*Action {
	var out []*Action
	for _, a := range l.Keys {
		out = append(out, a)
	}
	if l.Grid != nil {
		for _, a := range l.Grid.Cells {
			out = append(out, a)
		}
	}
	for _, a := range l.Soft {
		out = append(out, a)
	}
	return out
}

// unreachable は、どこからも参照されないレイヤーを返す。
func (km *Keymap) unreachable() []string {
	used := make([]bool, len(km.Layers))
	used[0] = true
	for _, l := range km.Layers {
		for _, a := range km.allActions(l) {
			if a.Kind.isLayer() {
				used[a.Layer] = true
			}
		}
	}
	var w []string
	for i, u := range used {
		if !u {
			w = append(w, fmt.Sprintf("layer %q is never activated (no layer_* refers to it)", km.Layers[i].Name))
		}
	}
	return w
}
