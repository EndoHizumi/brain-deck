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
	// Background は格子全体に敷く壁紙（背景画像の id）。セルの background があれば、そちらを上に描く
	Background string `yaml:"background,omitempty" json:"background,omitempty"`
}

// ActionSpec はキー・セル・ソフトキー 1 つの割り当て。
// `B`、`none`、`{ key: B, label: "ブラシ" }`、`{ layer_hold: edit }` のように書ける。
// key と layer_* のうち、ちょうど 1 つを書く。
//
// タッチのセルには、ウィジェット（`{ widget: clock, format: "15:04" }`、`{ widget: text, id: build }`）も書ける。
// ウィジェットのセルでは key と layer_* は省略でき、書けばタップしたときにそれが働く。
// span はセルの大きさ（列数、行数）で、どのセルにも書ける。
type ActionSpec struct {
	Key          string `yaml:"key,omitempty" json:"key,omitempty"` // 送るキー。"none" で無効（下のレイヤーを使わない）
	LayerHold    string `yaml:"layer_hold,omitempty" json:"layer_hold,omitempty"`
	LayerToggle  string `yaml:"layer_toggle,omitempty" json:"layer_toggle,omitempty"`
	LayerOneshot string `yaml:"layer_oneshot,omitempty" json:"layer_oneshot,omitempty"`
	LayerTo      string `yaml:"layer_to,omitempty" json:"layer_to,omitempty"`
	// Mouse はマウスの操作（left、right、middle、scroll_up など。mouse.go）。押しているあいだボタンを押す
	Mouse string `yaml:"mouse,omitempty" json:"mouse,omitempty"`
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	Span  Span   `yaml:"span,omitempty" json:"span,omitzero"` // [列数, 行数]。タッチのセルだけ
	// Background はセルの背景画像の id（/var/lib/lefthand/images/<id>.565）。タッチのセルだけ
	Background string `yaml:"background,omitempty" json:"background,omitempty"`

	// ウィジェット（タッチのセルだけ）。項目の意味は widget.go と docs/config.md
	Widget     string   `yaml:"widget,omitempty" json:"widget,omitempty"`
	Format     string   `yaml:"format,omitempty" json:"format,omitempty"`           // clock：時刻の行の書式（Go の書式）
	DateFormat string   `yaml:"date_format,omitempty" json:"date_format,omitempty"` // clock：日付の行の書式。none で出さない
	TZ         string   `yaml:"tz,omitempty" json:"tz,omitempty"`                   // clock：タイムゾーン（IANA の名前）
	ID         string   `yaml:"id,omitempty" json:"id,omitempty"`                   // text：中身の名前（set_text の name）
	Rows       int      `yaml:"rows,omitempty" json:"rows,omitempty"`               // todo、calendar：1 ページの行数
	PageReset  string   `yaml:"page_reset,omitempty" json:"page_reset,omitempty"`   // todo、calendar：触らなければ最初のページに戻るまでの時間。off で戻らない
	Stale      string   `yaml:"stale,omitempty" json:"stale,omitempty"`             // calendar：最終更新がこれより古ければ、古いと分かるように出す
	Calendars  []string `yaml:"calendars,omitempty" json:"calendars,omitempty"`     // calendar：出すカレンダーの名前。省略するとすべて

}

// Span はセルの大きさ [列数, 行数]。書かなければ [1, 1]。
type Span [2]int

func (s Span) IsZero() bool { return s == Span{} }

// MarshalYAML は `[2, 1]` の 1 行で書き出す。
func (s Span) MarshalYAML() (any, error) {
	var n yaml.Node
	if err := n.Encode([2]int(s)); err != nil {
		return nil, err
	}
	n.Style = yaml.FlowStyle
	return &n, nil
}

var actionFields = map[string]bool{
	"key": true, "layer_hold": true, "layer_toggle": true, "layer_oneshot": true, "layer_to": true, "label": true,
	"span": true, "widget": true, "format": true, "date_format": true, "tz": true, "id": true, "rows": true,
	"page_reset": true, "stale": true, "calendars": true, "background": true, "mouse": true,
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
				return fmt.Errorf("line %d: unknown field %q (key, layer_hold, layer_toggle, layer_oneshot, layer_to, mouse, label, span, widget, format, date_format, tz, id, rows, page_reset, stale, calendars, background)", k.Line, k.Value)
			}
		}
	}
	type plain ActionSpec // UnmarshalYAML を持たない型で中身を読む
	if err := n.Decode((*plain)(a)); err != nil {
		return err
	}
	if c := a.count(); c > 1 || (c == 0 && a.Widget == "") {
		return fmt.Errorf("line %d: write exactly one of key, layer_hold, layer_toggle, layer_oneshot, layer_to, mouse (a widget cell may omit them)", n.Line)
	}
	return nil
}

// MarshalYAML は、手で読み書きしやすい形で書き出す。
// キーだけなら `LCTRL+Z`、それ以外は 1 行のフロー形式 `{ key: B, label: ブラシ }` にする。
func (a ActionSpec) MarshalYAML() (any, error) {
	if a.Key != "" && a.count() == 1 && a.Label == "" && a.Widget == "" && a.Span.IsZero() && a.Background == "" {
		return a.Key, nil
	}
	type plain ActionSpec
	var n yaml.Node
	if err := n.Encode(plain(a)); err != nil {
		return nil, err
	}
	n.Style = yaml.FlowStyle
	return &n, nil
}

// MarshalYAML は `{ x: [a, b], y: [c, d] }` の 1 行で書き出す。
func (a SoftArea) MarshalYAML() (any, error) {
	type plain SoftArea
	var n yaml.Node
	if err := n.Encode(plain(a)); err != nil {
		return nil, err
	}
	n.Style = yaml.FlowStyle
	return &n, nil
}

func (a ActionSpec) count() int {
	n := 0
	for _, s := range []string{a.Key, a.LayerHold, a.LayerToggle, a.LayerOneshot, a.LayerTo, a.Mouse} {
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
	// 押しているセルの見せ方。border（既定）は枠を光らせ、fill はセルを塗りつぶす。
	// 動いているデーモンにも、設定 GUI からの保存ですぐ反映する
	PressStyle string `yaml:"press_style,omitempty" json:"press_style,omitempty"`
}

// 押しているセルの見せ方（display.press_style）
const (
	pressBorder = "border"
	pressFill   = "fill"
)

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
		return nil, Problems{{Path: "/display/rotate", Message: "display.rotate must be 0, 90, 180 or 270"}}
	}
	switch cfg.Display.PressStyle {
	case "", pressBorder, pressFill:
	default:
		return nil, Problems{{Path: "/display/press_style", Message: "display.press_style must be border or fill"}}
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
			return Problems{{Path: "/layers", Message: "with layers, write keys and touch cols/rows/cells inside each layer"}}
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

// Problem は設定の誤り 1 つ。Path は誤りの場所を JSON Pointer（RFC 6901）で表す。
// 例：/layers/1/keys/KEY_Q、/layers/0/touch/cells/0,0。場所が分からないときは空。
// 設定 GUI は Path を見て、誤りをその場所に表示する。
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Problems はまとめて見つかった誤り。error として使える。
type Problems []Problem

func (p Problems) Error() string {
	msgs := make([]string, len(p))
	for i, e := range p {
		msgs[i] = e.Message
	}
	return strings.Join(msgs, "\n")
}

func (p Problems) sort() { sort.Slice(p, func(i, j int) bool { return p[i].Message < p[j].Message }) }

// asProblems は err を Problems にする。場所の分からない誤り（YAML の文法など）は Path を空にする。
func asProblems(err error) Problems {
	var p Problems
	if errors.As(err, &p) {
		return p
	}
	return Problems{{Message: err.Error()}}
}

func pointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

type ActKind uint8

const (
	actNone    ActKind = iota // 何もしない（下のレイヤーも使わない）
	actKey                    // キーを送る
	actHold                   // 押しているあいだレイヤーを重ねる
	actToggle                 // 押すたびにレイヤーを重ねる・外す
	actOneshot                // 次の 1 キーだけレイヤーを重ねる
	actTo                     // base とそのレイヤーだけにする
	actWidget                 // ウィジェットのセルで、key も layer_* も書いていない（タップはウィジェットに任せる）
	actMouse                  // マウスのボタンかスクロール
)

var actNames = [...]string{"none", "key", "layer_hold", "layer_toggle", "layer_oneshot", "layer_to", "widget", "mouse"}

func (k ActKind) String() string { return actNames[k] }

func (k ActKind) isLayer() bool { return k >= actHold && k <= actTo }

// Action は組み立て済みの割り当て。
type Action struct {
	Kind         ActKind
	Combo        Combo       // actKey のとき
	Mouse        mouseAction // actMouse のとき
	Layer        int         // layer_* の行き先
	Spec         ActionSpec
	SpanW, SpanH int        // セルの大きさ（タッチのセル）。1 以上
	Widget       *WidgetDef // ウィジェットのセルなら、その中身
}

// tappable は、押したときに何かが起きるかどうか（押したセルを光らせるかどうか）。
func (a *Action) tappable() bool {
	if a == nil {
		return false
	}
	if a.Kind == actWidget {
		return a.Widget.ownsTouch()
	}
	return a.Kind != actNone
}

type cellPos struct{ Col, Row int }

type Grid struct {
	Cols, Rows int
	Cells      map[cellPos]*Action
	Background string // 壁紙の id。空ならなし
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
	Press  string // display.press_style。空なら border
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
	if cfg.Display != nil {
		km.Press = cfg.Display.PressStyle
	}
	if len(cfg.Layers) == 0 {
		return nil, nil, Problems{{Path: "/layers", Message: "no layers"}}
	}
	index := map[string]int{}
	var nameErrs Problems
	for i, lc := range cfg.Layers {
		path := fmt.Sprintf("/layers/%d/name", i)
		if lc.Name == "" {
			nameErrs = append(nameErrs, Problem{path, fmt.Sprintf("layers[%d]: name is required", i)})
			continue
		}
		if _, dup := index[lc.Name]; dup {
			nameErrs = append(nameErrs, Problem{path, fmt.Sprintf("layer %q is defined twice", lc.Name)})
			continue
		}
		index[lc.Name] = i
	}
	if len(nameErrs) > 0 {
		return nil, nil, nameErrs
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

	var errs Problems
	fail := func(path, format string, args ...any) {
		errs = append(errs, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	action := func(path, where string, self int, s ActionSpec, cell bool) *Action {
		a := &Action{Spec: s, SpanW: 1, SpanH: 1}
		target := ""
		if !cell && (s.Widget != "" || !s.Span.IsZero() || s.Background != "") {
			fail(path, "%s: widget, span and background can be used only in touch cells", where)
		}
		if s.Background != "" && !validImageID(s.Background) {
			fail(path, "%s: background must be the 16-digit lowercase hex id of an image (as the settings GUI writes it), got %q", where, s.Background)
		}
		if s.Widget != "" || s.hasWidgetFields() {
			w, err := compileWidget(s)
			if err != nil {
				fail(path, "%s: %v", where, err)
			}
			a.Widget = w
			if strings.EqualFold(s.Key, "none") {
				fail(path, "%s: a widget cell cannot be none (omit key to show the widget without a tap action)", where)
			}
		}
		if !s.Span.IsZero() {
			if s.Span[0] < 1 || s.Span[1] < 1 || s.Span[0] > 16 || s.Span[1] > 16 {
				fail(path, "%s: span must be [cols, rows], each 1..16", where)
			} else {
				a.SpanW, a.SpanH = s.Span[0], s.Span[1]
			}
		}
		switch {
		case s.count() == 0 && s.Widget != "":
			a.Kind = actWidget
		case strings.EqualFold(s.Key, "none"):
			a.Kind = actNone
		case s.Key != "":
			a.Kind = actKey
			c, err := parseCombo(s.Key)
			if err != nil {
				fail(path, "%s: %v", where, err)
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
		case s.Mouse != "":
			a.Kind = actMouse
			m, err := parseMouse(s.Mouse)
			if err != nil {
				fail(path, "%s: %v", where, err)
			}
			a.Mouse = m
		default:
			fail(path, "%s: empty assignment", where)
		}
		if a.Kind.isLayer() {
			t, ok := index[target]
			switch {
			case !ok:
				fail(path, "%s: %s refers to unknown layer %q", where, a.Kind, target)
			case t == 0 && (a.Kind == actToggle):
				fail(path, "%s: layer_toggle cannot target the base layer %q (use layer_to)", where, target)
			}
			a.Layer = t
		}
		return a
	}

	var baseGrid *Grid
	for li, lc := range cfg.Layers {
		lp := fmt.Sprintf("/layers/%d", li)
		l := &Layer{Name: lc.Name, Label: lc.Label, Keys: map[evdev.EvCode]*Action{}, Soft: map[string]*Action{}}
		for name, s := range lc.Keys {
			path := lp + "/keys/" + pointerEscape(name)
			where := fmt.Sprintf("layer %q key %s", lc.Name, name)
			code, ok := evdev.KEYFromString[name]
			if !ok {
				fail(path, "%s: unknown source key (use Linux names such as KEY_A)", where)
				continue
			}
			l.Keys[code] = action(path, where, li, s, false)
		}
		if lc.Touch != nil {
			tp := lp + "/touch"
			if cfg.Touch == nil {
				fail("/touch", "layer %q has touch, but the touch section (device and calibration) is missing", lc.Name)
			}
			g := &Grid{Cols: lc.Touch.Cols, Rows: lc.Touch.Rows, Cells: map[cellPos]*Action{}, Background: lc.Touch.Background}
			if g.Background != "" && !validImageID(g.Background) {
				fail(tp+"/background", "layer %q touch background must be the 16-digit lowercase hex id of an image (as the settings GUI writes it), got %q", lc.Name, g.Background)
			}
			if (g.Cols == 0) != (g.Rows == 0) {
				fail(tp, "layer %q touch: write both cols and rows, or neither", lc.Name)
			}
			if g.Cols == 0 && baseGrid != nil {
				g.Cols, g.Rows = baseGrid.Cols, baseGrid.Rows
			}
			if g.Cols <= 0 || g.Rows <= 0 || g.Cols > 16 || g.Rows > 16 {
				fail(tp, "layer %q touch: cols and rows must be 1..16", lc.Name)
				g.Cols, g.Rows = max(g.Cols, 1), max(g.Rows, 1)
			}
			for k, s := range lc.Touch.Cells {
				path := tp + "/cells/" + pointerEscape(k)
				where := fmt.Sprintf("layer %q touch cell %q", lc.Name, k)
				var p cellPos
				if _, err := fmt.Sscanf(k, "%d,%d", &p.Col, &p.Row); err != nil ||
					p.Col < 0 || p.Col >= g.Cols || p.Row < 0 || p.Row >= g.Rows {
					fail(path, "%s: out of the %dx%d grid", where, g.Cols, g.Rows)
					continue
				}
				if _, dup := g.Cells[p]; dup {
					fail(path, "%s: defined twice", where)
				}
				a := action(path, where, li, s, true)
				if p.Col+a.SpanW > g.Cols || p.Row+a.SpanH > g.Rows {
					fail(path, "%s: span %dx%d goes out of the %dx%d grid", where, a.SpanW, a.SpanH, g.Cols, g.Rows)
					a.SpanW, a.SpanH = 1, 1
				}
				g.Cells[p] = a
			}
			checkOverlaps(g, tp, lc.Name, fail)
			l.Grid = g
			if li == 0 {
				baseGrid = g
			}
		} else if li == 0 && cfg.Touch != nil {
			fail(lp+"/touch", "base layer %q needs touch cols and rows", lc.Name)
		}
		for name, s := range lc.SoftKeys {
			path := lp + "/soft_keys/" + pointerEscape(name)
			where := fmt.Sprintf("layer %q soft key %q", lc.Name, name)
			if _, ok := km.area(name); !ok {
				fail(path, "%s: not defined in touch.soft_areas", where)
			}
			l.Soft[name] = action(path, where, li, s, false)
		}
		km.Layers = append(km.Layers, l)
	}
	if len(errs) > 0 {
		errs.sort()
		return nil, nil, errs
	}

	if err := km.checkWayBack(); err != nil {
		return nil, nil, err
	}
	warns = km.unreachable()
	return km, warns, nil
}

// checkOverlaps は、同じレイヤーの中で span のセルが重なっていないことを確かめる。
func checkOverlaps(g *Grid, tp, layer string, fail func(path, format string, args ...any)) {
	ps := make([]cellPos, 0, len(g.Cells))
	for p := range g.Cells {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Row < ps[j].Row || ps[i].Row == ps[j].Row && ps[i].Col < ps[j].Col })
	owner := map[cellPos]cellPos{}
	for _, p := range ps {
		a := g.Cells[p]
		for r := p.Row; r < p.Row+a.SpanH; r++ {
			for c := p.Col; c < p.Col+a.SpanW; c++ {
				if o, ok := owner[cellPos{c, r}]; ok {
					// 両方のセルに誤りを付ける（設定 GUI では、覆われた側のセルは選べないため）
					pk, ok := fmt.Sprintf("%d,%d", p.Col, p.Row), fmt.Sprintf("%d,%d", o.Col, o.Row)
					fail(tp+"/cells/"+pk, "layer %q touch cell %q: overlaps cell %q (check span)", layer, pk, ok)
					fail(tp+"/cells/"+ok, "layer %q touch cell %q: overlaps cell %q (check span)", layer, ok, pk)
					return
				}
				owner[cellPos{c, r}] = p
			}
		}
	}
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
	var bad Problems
	for i := 1; i < n; i++ {
		if latched[i] && !good[i] {
			bad = append(bad, Problem{fmt.Sprintf("/layers/%d", i), fmt.Sprintf(
				"no way back to the base layer %q from layer %q: "+
					"put `layer_to: %s` (or a layer_toggle of the layer itself) on a key or cell that is usable there",
				km.Layers[0].Name, km.Layers[i].Name, km.Layers[0].Name)})
		}
	}
	if len(bad) > 0 {
		return bad
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

// hasWidgetFields は、ウィジェットにだけ書ける項目があるか。
func (a ActionSpec) hasWidgetFields() bool {
	return a.Format != "" || a.DateFormat != "" || a.TZ != "" || a.ID != "" || a.Rows != 0 ||
		a.PageReset != "" || a.Stale != "" || a.Calendars != nil
}

// usesMouse は、設定のどこかでマウスを使うか（mouse:）。
func (km *Keymap) usesMouse() bool {
	for _, l := range km.Layers {
		for _, a := range km.allActions(l) {
			if a.Kind == actMouse {
				return true
			}
		}
	}
	return false
}
