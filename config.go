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
	Terminal  *TerminalConfig       `yaml:"terminal,omitempty" json:"terminal,omitempty"`     // 端末モード（termmode.go）。省略すると既定値
	Backlight *BacklightConfig      `yaml:"backlight,omitempty" json:"backlight,omitempty"`   // 自動減光（backlight.go）。省略すると減光しない
	Home      *HomeConfig           `yaml:"home,omitempty" json:"home,omitempty"`             // ホーム画面（home.go）。省略すると使わない
	StatusBar *StatusBarConfig      `yaml:"status_bar,omitempty" json:"status_bar,omitempty"` // 画面の上の状態の帯（statusbar.go）
	Offline   *OfflineConfig        `yaml:"offline,omitempty" json:"offline,omitempty"`       // PC から外したときの動き（offline.go）
	Battery   *BatteryConfig        `yaml:"battery,omitempty" json:"battery,omitempty"`       // 電池が少ないときの知らせと電源断、記録（battery.go）
	Clock     *ClockConfig          `yaml:"clock,omitempty" json:"clock,omitempty"`           // 時刻（内蔵 RTC。rtc.go）
	Photo     *PhotoConfig          `yaml:"photo,omitempty" json:"photo,omitempty"`           // フォトフレーム（photo.go。backlight.photo で使う）
	Files     *FilesConfig          `yaml:"files,omitempty" json:"files,omitempty"`           // ファイルエクスプローラの見られる場所（files.go）。省略すると既定の場所
	Lid       *LidConfig            `yaml:"lid,omitempty" json:"lid,omitempty"`               // ふたを閉じたときの動き（lid.go）。省略すると画面を消す
	IME       *IMEConfig            `yaml:"ime,omitempty" json:"ime,omitempty"`               // 日本語の入力（ime.go）。省略すると、uim-fep が入っていれば使う
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
	// TouchPortrait は縦向き（480x800）のときの格子。省略すると display.portrait_grid のとおり横向きの格子から作る（rotate.go）
	TouchPortrait *GridConfig `yaml:"touch_portrait,omitempty" json:"touch_portrait,omitempty"`
	// StatusBar は、このレイヤーで画面の上に状態の帯を出すか。省略すると、格子（touch）を持つレイヤーなら status_bar.show、
	// 持たないレイヤーなら下のレイヤーのとおり
	StatusBar *bool `yaml:"status_bar,omitempty" json:"status_bar,omitempty"`
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
	// Media はメディアキー（play_pause、volume_up など。media.go）。キーボードとマウスの形（usb_mode: mouse）のときだけ送れる
	Media string `yaml:"media,omitempty" json:"media,omitempty"`
	// UsbMode は USB の形の切り替え（keyboard、mouse、toggle。usbmode.go）。USB を付け直すので、2〜3 秒切れる
	UsbMode string `yaml:"usb_mode,omitempty" json:"usb_mode,omitempty"`
	// Terminal は端末モードの切り替え（on、off、toggle。termmode.go）
	Terminal string `yaml:"terminal,omitempty" json:"terminal,omitempty"`
	// Rotate は画面の向きを変える（next で 90 度ずつ、0、90、180、270 でその向き。rotate.go）
	Rotate string `yaml:"rotate,omitempty" json:"rotate,omitempty"`
	// Input は、Brain のキーボードで文字を入れる欄を開く（todo で Todo を足す、clock で時刻を合わせる。prompt.go）
	Input string `yaml:"input,omitempty" json:"input,omitempty"`
	// Calibrate は、タッチのキャリブレーションを始める（touch で 5 点、touch9 で 9 点。calibui.go）。PC がなくても使える
	Calibrate string `yaml:"calibrate,omitempty" json:"calibrate,omitempty"`
	Label     string `yaml:"label,omitempty" json:"label,omitempty"`
	Span      Span   `yaml:"span,omitempty" json:"span,omitzero"` // [列数, 行数]。タッチのセルだけ
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
	Calendars  []string `yaml:"calendars,omitempty" json:"calendars,omitempty"`     // calendar：出すカレンダーの名前。month：印を付けるカレンダー。省略するとすべて
	Follow     string   `yaml:"follow,omitempty" json:"follow,omitempty"`           // calendar：同じレイヤーの月の表示（その id）で選んだ日の予定を出す
	WeekStart  string   `yaml:"week_start,omitempty" json:"week_start,omitempty"`   // month：週の始まり（sunday か monday）
	Work       string   `yaml:"work,omitempty" json:"work,omitempty"`               // pomodoro：作業の時間（25m など）
	Break      string   `yaml:"break,omitempty" json:"break,omitempty"`             // pomodoro：休憩の時間（5m など）
	// States は presence：タップで順に切り替える状態（名前と色）。省略すると在席、集中、会議中、休憩
	States []PresenceSpec `yaml:"states,omitempty" json:"states,omitempty"`
	// graph：グラフ（graphwidget.go）
	Unit     string   `yaml:"unit,omitempty" json:"unit,omitempty"`         // 値のあとに付ける単位（%、°C など）
	Min      *float64 `yaml:"min,omitempty" json:"min,omitempty"`           // 縦の範囲の下。省略すると値から決める
	Max      *float64 `yaml:"max,omitempty" json:"max,omitempty"`           // 縦の範囲の上。省略すると値から決める
	Chart    string   `yaml:"chart,omitempty" json:"chart,omitempty"`       // line（折れ線、既定）か bar（棒）
	Points   *int     `yaml:"points,omitempty" json:"points,omitempty"`     // 描く値の数（2〜120、既定 60）
	Decimals *int     `yaml:"decimals,omitempty" json:"decimals,omitempty"` // 小数点以下の桁数。省略すると値の大きさで決める
	// presenter：プレゼン用タイマー（presenter.go）
	Duration string               `yaml:"duration,omitempty" json:"duration,omitempty"` // 持ち時間（20m など）
	Colors   []PresenterColorSpec `yaml:"colors,omitempty" json:"colors,omitempty"`     // 残り時間に応じた色。省略すると残り 5 分で黄、1 分で赤
	// calculator：電卓（calc.go）
	Prefix bool `yaml:"prefix,omitempty" json:"prefix,omitempty"` // 16 進と 2 進の結果を、0x と 0b を付けて打ち込む

	// trackpad：トラックパッド（trackpad.go）。省略すると既定値
	Speed           *float64 `yaml:"speed,omitempty" json:"speed,omitempty"`                       // 感度（画面の 1 ドットを、PC のマウスの何カウントにするか）
	Accel           *float64 `yaml:"accel,omitempty" json:"accel,omitempty"`                       // 加速（0 で加速しない）
	ScrollWidth     *int     `yaml:"scroll_width,omitempty" json:"scroll_width,omitempty"`         // 右端のスクロールの帯の幅（ドット）。0 で帯なし
	ScrollDirection string   `yaml:"scroll_direction,omitempty" json:"scroll_direction,omitempty"` // natural か traditional
	ScrollStep      *float64 `yaml:"scroll_step,omitempty" json:"scroll_step,omitempty"`           // ホイール 1 段に当たる、指の動き（ドット）
	SettleMS        *int     `yaml:"settle_ms,omitempty" json:"settle_ms,omitempty"`               // 触れた直後に捨てる時間
	Smooth          *int     `yaml:"smooth,omitempty" json:"smooth,omitempty"`                     // 平均を取るサンプルの数
	Deadzone        *float64 `yaml:"deadzone,omitempty" json:"deadzone,omitempty"`                 // これより小さな動きを無視する（ドット）
	MinPressure     *int     `yaml:"min_pressure,omitempty" json:"min_pressure,omitempty"`         // 押す強さ（ABS_PRESSURE）がこれより弱いサンプルを捨てる
	TapMS           *int     `yaml:"tap_ms,omitempty" json:"tap_ms,omitempty"`                     // これより短く触れて離せばタップ
	TapMove         *float64 `yaml:"tap_move,omitempty" json:"tap_move,omitempty"`                 // タップとみなす動きの上限（ドット）
	DragMS          *int     `yaml:"drag_ms,omitempty" json:"drag_ms,omitempty"`                   // タップのあと、これより早く触れればドラッグ
	LongPress       string   `yaml:"long_press,omitempty" json:"long_press,omitempty"`             // none か right（長押しで右クリック）

	// pentablet：ペンタブレット（pen.go）。memo：手書きメモ（memo.go）も min_pressure、settle_ms、smooth を使う
	Map           string  `yaml:"map,omitempty" json:"map,omitempty"`                       // full（既定）、aspect、region
	PCScreen      string  `yaml:"pc_screen,omitempty" json:"pc_screen,omitempty"`           // PC の画面の大きさ（1920x1080 など）
	Region        *[4]int `yaml:"region,omitempty" json:"region,omitempty"`                 // map: region の範囲 [x, y, 幅, 高さ]（PC の画面の画素）
	Pressure      *bool   `yaml:"pressure,omitempty" json:"pressure,omitempty"`             // 筆圧を送る（既定 true）
	PressureRange *[2]int `yaml:"pressure_range,omitempty" json:"pressure_range,omitempty"` // 筆圧の 0 と最大にする押す強さ（既定 [400, 2400]）
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
	"page_reset": true, "stale": true, "calendars": true, "follow": true, "week_start": true, "background": true, "mouse": true, "media": true, "usb_mode": true,
	"terminal": true, "rotate": true, "input": true, "calibrate": true, "work": true, "break": true, "states": true,
	"unit": true, "min": true, "max": true, "chart": true, "points": true, "decimals": true, "duration": true, "colors": true, "prefix": true,
	"speed": true, "accel": true, "scroll_width": true, "scroll_direction": true, "scroll_step": true, "settle_ms": true,
	"smooth": true, "deadzone": true, "min_pressure": true, "tap_ms": true, "tap_move": true, "drag_ms": true, "long_press": true,
	"map": true, "pc_screen": true, "region": true, "pressure": true, "pressure_range": true,
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
				return fmt.Errorf("line %d: unknown field %q (key, layer_hold, layer_toggle, layer_oneshot, layer_to, mouse, media, usb_mode, terminal, rotate, input, calibrate, label, span, widget, format, date_format, tz, id, rows, page_reset, stale, calendars, follow, week_start, work, break, states, unit, min, max, chart, points, decimals, duration, colors, prefix, background, and the trackpad fields)", k.Line, k.Value)
			}
		}
	}
	type plain ActionSpec // UnmarshalYAML を持たない型で中身を読む
	if err := n.Decode((*plain)(a)); err != nil {
		return err
	}
	if c := a.count(); c > 1 || (c == 0 && a.Widget == "") {
		return fmt.Errorf("line %d: write exactly one of key, layer_hold, layer_toggle, layer_oneshot, layer_to, mouse, media, usb_mode, terminal, rotate, input (a widget cell may omit them)", n.Line)
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

// hasGraphFields は、グラフの項目を書いたか。
func (a ActionSpec) hasGraphFields() bool {
	return a.Unit != "" || a.Min != nil || a.Max != nil || a.Chart != "" || a.Points != nil || a.Decimals != nil
}

func (a ActionSpec) count() int {
	n := 0
	for _, s := range []string{a.Key, a.LayerHold, a.LayerToggle, a.LayerOneshot, a.LayerTo, a.Mouse, a.Media, a.UsbMode, a.Terminal, a.Rotate, a.Input, a.Calibrate} {
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
	// PortraitGrid は、touch_portrait を書いていないレイヤーを縦向きでどう並べるか。
	// transpose（既定）は列と行を入れ替え、stretch は横向きの格子をそのまま縦に伸ばす（rotate.go）
	PortraitGrid string `yaml:"portrait_grid,omitempty" json:"portrait_grid,omitempty"`
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
	switch cfg.Display.PortraitGrid {
	case "", portraitTranspose, portraitStretch:
	default:
		return nil, Problems{{Path: "/display/portrait_grid", Message: "display.portrait_grid must be transpose or stretch"}}
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
	actNone      ActKind = iota // 何もしない（下のレイヤーも使わない）
	actKey                      // キーを送る
	actHold                     // 押しているあいだレイヤーを重ねる
	actToggle                   // 押すたびにレイヤーを重ねる・外す
	actOneshot                  // 次の 1 キーだけレイヤーを重ねる
	actTo                       // base とそのレイヤーだけにする
	actWidget                   // ウィジェットのセルで、key も layer_* も書いていない（タップはウィジェットに任せる）
	actMouse                    // マウスのボタンかスクロール
	actUSB                      // USB の形の切り替え
	actTerminal                 // 端末モードの切り替え
	actMedia                    // メディアキー
	actRotate                   // 画面の向きを変える
	actHomePage                 // ホーム画面のページを送る（ホーム画面の ◀ ▶ のソフトキー。設定には書けない）
	actInput                    // Brain のキーボードで文字を入れる欄を開く（prompt.go）
	actCalibrate                // タッチのキャリブレーションを始める（calibui.go）
)

var actNames = [...]string{"none", "key", "layer_hold", "layer_toggle", "layer_oneshot", "layer_to", "widget", "mouse", "usb_mode", "terminal", "media", "rotate", "home_page", "input", "calibrate"}

func (k ActKind) String() string { return actNames[k] }

func (k ActKind) isLayer() bool { return k >= actHold && k <= actTo }

// Action は組み立て済みの割り当て。
type Action struct {
	Kind         ActKind
	Combo        Combo       // actKey のとき
	Mouse        mouseAction // actMouse のとき
	Media        mediaAction // actMedia のとき
	Layer        int         // layer_* の行き先
	Rotate       int         // actRotate：向き（0、90、180、270）。rotateNext なら 90 度回す。actHomePage：-1 か +1
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
		return a.Widget.handlesTouch()
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
	// PGrid は縦向きのときの格子。touch_portrait か、Grid から作ったもの。Grid が nil なら nil
	PGrid *Grid
	Soft  map[string]*Action
	// StatusBar は、このレイヤーで状態の帯を出すか（nil なら下のレイヤーに任せる）
	StatusBar *bool
	home      bool // ホーム画面（設定の home から作ったレイヤー）
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
	// Home はホーム画面。nil ならホーム画面を使わない
	Home *HomeDef
	// Bar は状態の帯の中身と、書いていないレイヤーで出すか
	Bar BarDef
	// Rotate は display.rotate（設定に書いた向き。保存した向きがなければ、これで始める）
	Rotate int
	// cfgPortrait は、レイヤーごとに touch_portrait を書いたか（画像の大きさの確かめに使う）
	cfgPortrait []bool
	// OfflineHome は offline.home（PC から外れたらホーム画面に切り替える）
	OfflineHome bool
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
	cfg.Terminal.validate(fail)
	cfg.IME.validate(fail)
	cfg.Backlight.validate(fail)

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
		case s.Media != "":
			a.Kind = actMedia
			m, err := parseMedia(s.Media)
			if err != nil {
				fail(path, "%s: %v", where, err)
			}
			a.Media = m
		case s.UsbMode != "":
			a.Kind = actUSB
			switch s.UsbMode {
			case usbKeyboard, usbMouse, usbToggle:
			default:
				fail(path, "%s: usb_mode must be %s, %s or %s", where, usbKeyboard, usbMouse, usbToggle)
			}
		case s.Terminal != "":
			a.Kind = actTerminal
			switch s.Terminal {
			case "on", "off", "toggle", termTargetPC, termTargetBrain:
			default:
				fail(path, "%s: terminal must be on, off, toggle, pc or brain", where)
			}
		case s.Rotate != "":
			a.Kind = actRotate
			r, ok := parseRotate(s.Rotate)
			if !ok {
				fail(path, "%s: rotate must be next, 0, 90, 180 or 270", where)
			}
			a.Rotate = r
		case s.Input != "":
			a.Kind = actInput
			if !inputKinds[s.Input] {
				fail(path, "%s: input must be todo or clock", where)
			}
		case s.Calibrate != "":
			a.Kind = actCalibrate
			if _, ok := calibrateKinds[s.Calibrate]; !ok {
				fail(path, "%s: calibrate must be touch (5 points) or touch9 (9 points)", where)
			}
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

	var baseGrid, basePGrid *Grid
	var monthWarns []string
	portraitMode := ""
	if cfg.Display != nil {
		portraitMode = cfg.Display.PortraitGrid
		km.Rotate = cfg.Display.Rotate
	}
	cfg.StatusBar.validate(fail)
	km.Bar = cfg.StatusBar.compile()
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
		// grid は、格子 gc を組み立てる。def は cols と rows を省略したときの大きさ（nil なら書かなければ誤り）
		grid := func(gc *GridConfig, tp, what string, def *Grid) *Grid {
			g := &Grid{Cols: gc.Cols, Rows: gc.Rows, Cells: map[cellPos]*Action{}, Background: gc.Background}
			if g.Background != "" && !validImageID(g.Background) {
				fail(tp+"/background", "layer %q %s background must be the 16-digit lowercase hex id of an image (as the settings GUI writes it), got %q", lc.Name, what, g.Background)
			}
			if (g.Cols == 0) != (g.Rows == 0) {
				fail(tp, "layer %q %s: write both cols and rows, or neither", lc.Name, what)
			}
			if g.Cols == 0 && def != nil {
				g.Cols, g.Rows = def.Cols, def.Rows
			}
			if g.Cols <= 0 || g.Rows <= 0 || g.Cols > 16 || g.Rows > 16 {
				fail(tp, "layer %q %s: cols and rows must be 1..16", lc.Name, what)
				g.Cols, g.Rows = max(g.Cols, 1), max(g.Rows, 1)
			}
			for k, s := range gc.Cells {
				path := tp + "/cells/" + pointerEscape(k)
				where := fmt.Sprintf("layer %q %s cell %q", lc.Name, what, k)
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
			monthWarns = append(monthWarns, linkFollows(g, tp, lc.Name, fail)...)
			monthWarns = append(monthWarns, calcWarns(g, lc.Name)...)
			return g
		}
		if lc.Touch != nil {
			if cfg.Touch == nil {
				fail("/touch", "layer %q has touch, but the touch section (device and calibration) is missing", lc.Name)
			}
			l.Grid = grid(lc.Touch, lp+"/touch", "touch", baseGrid)
			if li == 0 {
				baseGrid = l.Grid
			}
		} else if li == 0 && cfg.Touch != nil {
			fail(lp+"/touch", "base layer %q needs touch cols and rows", lc.Name)
		}
		if lc.TouchPortrait != nil {
			tp := lp + "/touch_portrait"
			switch {
			case lc.Touch == nil:
				fail(tp, "layer %q: touch_portrait needs touch (the landscape grid) in the same layer", lc.Name)
			default:
				l.PGrid = grid(lc.TouchPortrait, tp, "touch_portrait", basePGrid)
			}
		} else if l.Grid != nil {
			l.PGrid = portraitOf(l.Grid, portraitMode)
		}
		if li == 0 {
			basePGrid = l.PGrid
		}
		l.StatusBar = lc.StatusBar
		km.cfgPortrait = append(km.cfgPortrait, lc.TouchPortrait != nil)
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
	homeWarns := compileHome(cfg, km, index, fail)
	if cfg.Offline != nil && cfg.Offline.Home {
		km.OfflineHome = true
		if cfg.Home == nil {
			homeWarns = append(homeWarns, "offline.home has no effect without home (the home screen)")
		}
	}
	cfg.Battery.validate(fail)
	cfg.Clock.validate(fail)
	cfg.Photo.validate(fail)
	cfg.Files.validate(defaultDataDir, fail)
	cfg.Lid.validate(fail)
	if len(errs) > 0 {
		errs.sort()
		return nil, nil, errs
	}

	if err := km.checkWayBack(); err != nil {
		return nil, nil, err
	}
	warns = append(km.unreachable(), monthWarns...)
	warns = append(warns, homeWarns...)
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
	if km.Home != nil {
		good[km.Home.Layer] = true // ホーム画面へは、どのレイヤーからも HOME で戻れる。ホーム画面が出発点になる
	}
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
	if h := km.Home; h != nil {
		used[h.Layer] = true
		for _, a := range h.Apps {
			used[a.Layer] = true
		}
	}
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
		a.PageReset != "" || a.Stale != "" || a.Calendars != nil || a.Follow != "" || a.WeekStart != "" || a.Work != "" || a.Break != "" || a.States != nil || a.hasPadFields()
}

// hasPadFields は、トラックパッドにだけ書ける項目があるか。
func (a ActionSpec) hasPadFields() bool {
	return a.Speed != nil || a.Accel != nil || a.ScrollWidth != nil || a.ScrollDirection != "" || a.ScrollStep != nil ||
		a.SettleMS != nil || a.Smooth != nil || a.Deadzone != nil || a.MinPressure != nil || a.TapMS != nil ||
		a.TapMove != nil || a.DragMS != nil || a.LongPress != ""
}

// usesMedia は、設定のどこかでメディアキーを使うか。
func (km *Keymap) usesMedia() bool {
	for _, l := range km.Layers {
		for _, a := range km.allActions(l) {
			if a.Kind == actMedia {
				return true
			}
		}
	}
	return false
}

// usesMouse は、設定のどこかでマウスを使うか（mouse: か widget: trackpad）。
func (km *Keymap) usesMouse() bool {
	for _, l := range km.Layers {
		for _, a := range km.allActions(l) {
			if a.Kind == actMouse || (a.Widget != nil && (a.Widget.Kind == widgetPad || a.Widget.Kind == widgetPenTab)) {
				return true
			}
		}
	}
	return false
}
