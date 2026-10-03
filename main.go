// 左手デバイス変換デーモン
// Brainのキーボード/タッチパネルの evdev 入力を読み、config.yaml に従って
// /dev/hidg0 に HID キーボードレポートを書き込む。
//
// ビルド:
//
//	go mod init lefthand && go mod tidy
//	GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -o lefthand
//
// 使い方:
//
//	lefthand [-v] [config.yaml]            通常動作
//	lefthand -calibrate [config.yaml]      タッチの生座標を表示して範囲を求める
package main

import (
	"errors"
	"flag"
	"fmt"
	"image/png"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	evdev "github.com/holoplot/go-evdev"
	"gopkg.in/yaml.v3"
)

// ---------- 設定 ----------

type Config struct {
	HIDDevice string            `yaml:"hid_device"`
	Keyboard  string            `yaml:"keyboard"` // "/dev/input/eventN" またはデバイス名
	Keys      map[string]string `yaml:"keys"`     // "KEY_A": "LCTRL+Z"
	Touch     *TouchConfig      `yaml:"touch"`
	Display   *DisplayConfig    `yaml:"display"`
}

type TouchConfig struct {
	Device string              `yaml:"device"` // "/dev/input/eventN" またはデバイス名
	Cols   int                 `yaml:"cols"`
	Rows   int                 `yaml:"rows"`
	SwapXY bool                `yaml:"swap_xy"` // パネルのX軸が画面の縦方向のとき
	MinX   int32               `yaml:"min_x"`
	MaxX   int32               `yaml:"max_x"`
	MinY   int32               `yaml:"min_y"`
	MaxY   int32               `yaml:"max_y"`
	Cells  map[string]CellSpec `yaml:"cells"` // "col,row": "LCTRL+S" または { key: ..., label: ... }
}

// CellSpec はタッチセル 1 つの設定。
// `"0,0": B` と `"0,0": { key: B, label: "ブラシ" }` のどちらでも書ける。
type CellSpec struct {
	Key   string `yaml:"key"`
	Label string `yaml:"label"`
}

func (c *CellSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Key = n.Value
		return nil
	}
	type plain CellSpec // UnmarshalYAML を持たない型で中身を読む
	if err := n.Decode((*plain)(c)); err != nil {
		return err
	}
	if c.Key == "" {
		return fmt.Errorf("line %d: touch cell needs key", n.Line)
	}
	return nil
}

// DisplayConfig は画面表示の設定。display を省略してもタッチがあれば表示する。
type DisplayConfig struct {
	Enabled *bool  `yaml:"enabled"` // false で画面を使わない
	Device  string `yaml:"device"`  // フレームバッファ
	VT      int    `yaml:"vt"`      // 使う VT の番号。0 なら tty8 以降の空きを使う
	Rotate  int    `yaml:"rotate"`  // 画面の回転（0, 90, 180, 270）
}

const (
	defaultHID      = "/dev/hidg0"
	defaultKeyboard = "brain-kbd-i2c"
	defaultTouch    = "mxs-lradc-ts"
	defaultFB       = "/dev/fb0"
)

var verbose bool

func vlogf(format string, args ...any) {
	if verbose {
		log.Printf(format, args...)
	}
}

// ---------- HID ----------

type Combo struct {
	Mods byte
	Keys []byte
}

var modBits = map[string]byte{
	"LCTRL": 0x01, "LSHIFT": 0x02, "LALT": 0x04, "LGUI": 0x08,
	"RCTRL": 0x10, "RSHIFT": 0x20, "RALT": 0x40, "RGUI": 0x80,
}

var hidUsage = map[string]byte{
	"ENTER": 0x28, "ESC": 0x29, "BACKSPACE": 0x2a, "TAB": 0x2b, "SPACE": 0x2c,
	"MINUS": 0x2d, "EQUAL": 0x2e, "LEFTBRACE": 0x2f, "RIGHTBRACE": 0x30,
	"BACKSLASH": 0x31, "SEMICOLON": 0x33, "APOSTROPHE": 0x34, "GRAVE": 0x35,
	"COMMA": 0x36, "DOT": 0x37, "SLASH": 0x38,
	"INSERT": 0x49, "HOME": 0x4a, "PAGEUP": 0x4b, "DELETE": 0x4c,
	"END": 0x4d, "PAGEDOWN": 0x4e,
	"RIGHT": 0x4f, "LEFT": 0x50, "DOWN": 0x51, "UP": 0x52,
}

func init() {
	for i := 0; i < 26; i++ {
		hidUsage[string(rune('A'+i))] = byte(0x04 + i)
	}
	for i := 1; i <= 9; i++ {
		hidUsage[fmt.Sprint(i)] = byte(0x1e + i - 1)
	}
	hidUsage["0"] = 0x27
	for i := 1; i <= 12; i++ {
		hidUsage[fmt.Sprintf("F%d", i)] = byte(0x3a + i - 1)
	}
}

func parseCombo(s string) (Combo, error) {
	var c Combo
	for _, p := range strings.Split(s, "+") {
		p = strings.ToUpper(strings.TrimSpace(p))
		if m, ok := modBits[p]; ok {
			c.Mods |= m
		} else if u, ok := hidUsage[p]; ok {
			c.Keys = append(c.Keys, u)
		} else {
			return c, fmt.Errorf("unknown key %q in %q", p, s)
		}
	}
	return c, nil
}

// HIDWriter は /dev/hidg0 に非ブロッキングで書き込む。
// ブロッキングで開くと、PCがレポートを取りに来ない間（スリープ中など）
// write が永久に待ち、終了時の空レポートも送れなくなる。
type HIDWriter struct {
	path     string
	fd       int
	lastOpen time.Time
	failing  bool
}

const (
	hidRetryFor  = 50 * time.Millisecond // EAGAIN（前のレポートが未送信）を待つ上限
	hidReopenGap = time.Second
)

func NewHIDWriter(path string) *HIDWriter {
	w := &HIDWriter{path: path, fd: -1}
	w.open()
	return w
}

func (w *HIDWriter) open() error {
	w.lastOpen = time.Now()
	fd, err := syscall.Open(w.path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	w.fd = fd
	return nil
}

// Write は失敗してもエラーを返すだけで、呼び出し側を止めない。
func (w *HIDWriter) Write(r []byte) error {
	err := w.write(r)
	if err != nil && !w.failing {
		log.Printf("hid write failed (PC未接続/スリープ中?): %v", err)
	} else if err == nil && w.failing {
		log.Printf("hid write recovered")
	}
	w.failing = err != nil
	return err
}

func (w *HIDWriter) write(r []byte) error {
	if w.fd < 0 {
		if time.Since(w.lastOpen) < hidReopenGap {
			return errors.New("hid device not open")
		}
		if err := w.open(); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(hidRetryFor)
	for {
		_, err := syscall.Write(w.fd, r)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR):
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(time.Millisecond)
		case errors.Is(err, syscall.ESHUTDOWN):
			// USB 未接続/未構成。fd はそのまま使える
			return err
		default:
			syscall.Close(w.fd)
			w.fd = -1
			return err
		}
	}
}

// ---------- 押下状態とレポート送信 ----------

type State struct {
	mu     sync.Mutex
	hid    *HIDWriter
	active map[string]Combo // 入力元ID -> 出力
	dirty  bool             // 最後の送信が失敗し、現在の状態がPCに届いていない
}

func (s *State) press(id string, c Combo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[id] = c
	s.send()
}

func (s *State) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.active[id]; !ok {
		return
	}
	delete(s.active, id)
	s.send()
}

func (s *State) releaseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = map[string]Combo{}
	s.send()
}

// retryLoop は送信に失敗した状態を定期的に送り直す。
// 離した瞬間のレポートが落ちても、キーが押しっぱなしにならないようにする。
func (s *State) retryLoop() {
	for range time.Tick(200 * time.Millisecond) {
		s.mu.Lock()
		if s.dirty {
			s.send()
		}
		s.mu.Unlock()
	}
}

// 呼び出し側でロック済みであること
func (s *State) send() {
	r := make([]byte, 8)
	n := 0
	ids := make([]string, 0, len(s.active))
	for id := range s.active {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 6キーを超えたときにどれが残るかを毎回同じにする
	for _, id := range ids {
		c := s.active[id]
		r[0] |= c.Mods
	next:
		for _, k := range c.Keys {
			for _, e := range r[2 : 2+n] {
				if e == k {
					continue next
				}
			}
			if n < 6 {
				r[2+n] = k
				n++
			}
		}
	}
	err := s.hid.Write(r)
	s.dirty = err != nil
	if err != nil {
		vlogf("hid report % x (not sent: %v)", r, err)
	} else {
		vlogf("hid report % x", r)
	}
}

var (
	cleanupOnce sync.Once
	atExitMu    sync.Mutex
	atExit      []func() // 終了時に実行する（画面の後始末など）
)

func addAtExit(f func()) {
	atExitMu.Lock()
	atExit = append(atExit, f)
	atExitMu.Unlock()
}

// shutdown は空レポートを送り、後始末をしてから終了する。
// 複数の goroutine から同時に呼ばれても、後始末は一度だけ行う。
func (s *State) shutdown(code int) {
	cleanupOnce.Do(func() {
		s.releaseAll()
		atExitMu.Lock()
		defer atExitMu.Unlock()
		for _, f := range atExit {
			f()
		}
	})
	os.Exit(code)
}

// ---------- 入力 ----------

// openInput は "/" で始まればパスとして、そうでなければデバイス名として開く。
func openInput(spec string) (*evdev.InputDevice, error) {
	if strings.HasPrefix(spec, "/") {
		return evdev.Open(spec)
	}
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range paths {
		if p.Name == spec {
			return evdev.Open(p.Path)
		}
		names = append(names, fmt.Sprintf("%s=%q", p.Path, p.Name))
	}
	return nil, fmt.Errorf("input device %q not found (available: %s)", spec, strings.Join(names, ", "))
}

func describe(dev *evdev.InputDevice) string {
	name, _ := dev.Name()
	return fmt.Sprintf("%s (%s)", dev.Path(), name)
}

func evString(ev *evdev.InputEvent) string {
	return fmt.Sprintf("%s %s %d", ev.TypeName(), ev.CodeName(), ev.Value)
}

func runKeyboard(dev *evdev.InputDevice, keymap map[evdev.EvCode]Combo, s *State) {
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Printf("read keyboard: %v", err)
			s.shutdown(1)
		}
		if ev.Type == evdev.EV_SYN || ev.Type == evdev.EV_MSC {
			continue
		}
		vlogf("kbd: %s", evString(ev))
		if ev.Type != evdev.EV_KEY {
			continue
		}
		c, ok := keymap[ev.Code]
		if !ok {
			continue
		}
		id := fmt.Sprintf("k:%d", ev.Code)
		switch ev.Value {
		case 1:
			s.press(id, c)
		case 0:
			s.release(id)
		} // 2 = オートリピートは無視（PC側でリピートする）
	}
}

func cellOf(v, min, max int32, n int) int {
	if max == min || n <= 0 {
		return 0
	}
	i := int(int64(v-min) * int64(n) / int64(max-min)) // min>maxなら反転も扱える
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}

// touchCell はタッチの生座標からセルを求める。画面の枠も同じ境界で描く（cellSpan）。
func touchCell(tc *TouchConfig, x, y int32) (col, row int) {
	if tc.SwapXY {
		x, y = y, x
	}
	return cellOf(x, tc.MinX, tc.MaxX, tc.Cols), cellOf(y, tc.MinY, tc.MaxY, tc.Rows)
}

func evTime(ev *evdev.InputEvent) time.Time {
	return time.Unix(int64(ev.Time.Sec), int64(ev.Time.Usec)*1000)
}

func runTouch(dev *evdev.InputDevice, tc *TouchConfig, cells map[string]Combo, s *State, disp *Display) {
	var x, y int32
	down, pressed := false, false
	litCol, litRow := -1, -1 // ハイライト中のセル
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Printf("read touch: %v", err)
			s.shutdown(1)
		}
		switch {
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_X:
			x = ev.Value
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_Y:
			y = ev.Value
		case ev.Type == evdev.EV_KEY && ev.Code == evdev.BTN_TOUCH:
			vlogf("touch: %s", evString(ev))
			down = ev.Value == 1
			if !down && pressed {
				s.release("t")
				pressed = false
				if litCol >= 0 {
					disp.SetPressed(litCol, litRow, false)
					litCol, litRow = -1, -1
				}
			}
		case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT:
			// 触れた瞬間のセルで確定（スライドしても変えない）
			if down && !pressed {
				col, row := touchCell(tc, x, y)
				key := fmt.Sprintf("%d,%d", col, row)
				c, ok := cells[key]
				if ok {
					s.press("t", c) // HID を先に送り、描画はそのあと
					disp.SetPressed(col, row, true)
					litCol, litRow = col, row
				}
				vlogf("touch: x=%d y=%d -> cell %s (mapped=%v) %v after event",
					x, y, key, ok, time.Since(evTime(ev)).Round(100*time.Microsecond))
				pressed = true
			}
		}
	}
}

// ---------- キャリブレーション ----------

type sample struct{ x, y int32 }

// runCalibrate はタッチごとの平均座標を表示し、四隅から範囲を求める。
// 抵抗膜は触れ始め/離す瞬間の値が暴れるので、生の極値ではなく
// タッチごとの平均を使う。
func runCalibrate(spec string) {
	dev, err := openInput(spec)
	if err != nil {
		log.Fatalf("open touch: %v", err)
	}
	log.Printf("calibrate: %s", describe(dev))
	if infos, err := dev.AbsInfos(); err == nil {
		for _, code := range []evdev.EvCode{evdev.ABS_X, evdev.ABS_Y, evdev.ABS_PRESSURE} {
			if ai, ok := infos[code]; ok {
				log.Printf("  %s: driver range %d..%d", evdev.CodeName(evdev.EV_ABS, code), ai.Minimum, ai.Maximum)
			}
		}
	}
	fmt.Fprintln(os.Stderr, "四隅を 左上 → 右上 → 右下 → 左下 の順に、1か所ずつしっかり押して離してください。")
	fmt.Fprintln(os.Stderr, "終わったら Ctrl-C（または timeout で終了）。")

	var (
		mu                   sync.Mutex
		touches              []sample
		rawMinX, rawMaxX     int32 = 1<<31 - 1, -1 << 31
		rawMinY, rawMaxY     int32 = 1<<31 - 1, -1 << 31
		x, y                 int32
		down                 bool
		sumX, sumY, nSamples int64
	)

	report := func() {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(os.Stderr, "\n=== calibration result (%d touches) ===\n", len(touches))
		if len(touches) == 0 {
			return
		}
		fmt.Fprintf(os.Stderr, "raw sample extremes: x %d..%d, y %d..%d\n", rawMinX, rawMaxX, rawMinY, rawMaxY)
		ax, bx, ay, by := touches[0].x, touches[0].x, touches[0].y, touches[0].y
		for _, t := range touches {
			ax, bx = min(ax, t.x), max(bx, t.x)
			ay, by = min(ay, t.y), max(by, t.y)
		}
		fmt.Fprintf(os.Stderr, "per-touch average extremes: x %d..%d, y %d..%d\n", ax, bx, ay, by)
		if len(touches) < 4 {
			fmt.Fprintln(os.Stderr, "四隅が揃っていないので向きは判定できません")
			return
		}
		c := touches[len(touches)-4:] // 最後の4回を 左上, 右上, 右下, 左下 とみなす
		tl, tr, br, bl := c[0], c[1], c[2], c[3]
		absd := func(v int32) int32 {
			if v < 0 {
				return -v
			}
			return v
		}
		swap := absd(tr.x-tl.x) < absd(bl.x-tl.x)
		get := func(s sample) (int32, int32) {
			if swap {
				return s.y, s.x
			}
			return s.x, s.y
		}
		tlx, tly := get(tl)
		trx, try := get(tr)
		brx, bry := get(br)
		blx, bly := get(bl)
		// 見出しも値と同じ stdout に出す（stderr と分けると ssh 経由で順序が入れ替わる）。
		// コメントにしておけば stdout をそのまま YAML として使える
		fmt.Fprintf(os.Stdout, "  # suggested touch config (最後の4タッチ = 左上,右上,右下,左下)\n"+
			"  swap_xy: %v\n  min_x: %d\n  max_x: %d\n  min_y: %d\n  max_y: %d\n",
			swap, (tlx+blx)/2, (trx+brx)/2, (tly+try)/2, (bly+bry)/2)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		report()
		os.Exit(0)
	}()

	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Printf("read touch: %v", err)
			report()
			os.Exit(1)
		}
		switch {
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_X:
			x = ev.Value
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_Y:
			y = ev.Value
		case ev.Type == evdev.EV_KEY && ev.Code == evdev.BTN_TOUCH:
			mu.Lock()
			if ev.Value == 1 {
				down = true
				sumX, sumY, nSamples = 0, 0, 0
			} else if down {
				down = false
				if nSamples > 0 {
					t := sample{int32(sumX / nSamples), int32(sumY / nSamples)}
					touches = append(touches, t)
					fmt.Fprintf(os.Stderr, "touch #%d: avg x=%d y=%d (%d samples)\n", len(touches), t.x, t.y, nSamples)
				}
			}
			mu.Unlock()
		case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT:
			mu.Lock()
			if down {
				sumX += int64(x)
				sumY += int64(y)
				nSamples++
				rawMinX, rawMaxX = min(rawMinX, x), max(rawMaxX, x)
				rawMinY, rawMaxY = min(rawMinY, y), max(rawMaxY, y)
				if verbose {
					fmt.Fprintf(os.Stderr, "raw x=%d y=%d\n", x, y)
				}
			}
			mu.Unlock()
		}
	}
}

// ---------- main ----------

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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
		return nil, fmt.Errorf("%s: display.rotate must be 0, 90, 180 or 270", path)
	}
	return &cfg, nil
}

// displayEnabled は画面を使うかどうか。タッチがなければ描くものがない。
func (c *Config) displayEnabled() bool {
	return c.Touch != nil && (c.Display.Enabled == nil || *c.Display.Enabled)
}

// 画面に出すキー名。設定の書き方より短く、読みやすくする
var prettyKey = map[string]string{
	"LCTRL": "Ctrl", "RCTRL": "Ctrl", "LSHIFT": "Shift", "RSHIFT": "Shift",
	"LALT": "Alt", "RALT": "Alt", "LGUI": "Win", "RGUI": "Win",
	"ENTER": "Enter", "ESC": "Esc", "BACKSPACE": "BS", "TAB": "Tab", "SPACE": "Space",
	"MINUS": "-", "EQUAL": "=", "LEFTBRACE": "[", "RIGHTBRACE": "]",
	"BACKSLASH": "\\", "SEMICOLON": ";", "APOSTROPHE": "'", "GRAVE": "`",
	"COMMA": ",", "DOT": ".", "SLASH": "/",
	"INSERT": "Ins", "HOME": "Home", "PAGEUP": "PgUp", "DELETE": "Del",
	"END": "End", "PAGEDOWN": "PgDn",
	"RIGHT": "→", "LEFT": "←", "DOWN": "↓", "UP": "↑",
}

func prettyCombo(s string) string {
	parts := strings.Split(s, "+")
	for i, p := range parts {
		p = strings.ToUpper(strings.TrimSpace(p))
		if q, ok := prettyKey[p]; ok {
			p = q
		}
		parts[i] = p
	}
	return strings.Join(parts, "+")
}

// buildLayout はタッチの設定から、画面に描くセルの内容を作る。
func buildLayout(tc *TouchConfig) *Layout {
	l := &Layout{Cols: tc.Cols, Rows: tc.Rows, Cells: make([]CellView, tc.Cols*tc.Rows)}
	for k, spec := range tc.Cells {
		var col, row int
		fmt.Sscanf(k, "%d,%d", &col, &row) // 範囲は main で検査済み
		keys := prettyCombo(spec.Key)
		v := CellView{Mapped: true, Label: spec.Label, Sub: keys}
		if v.Label == "" || v.Label == keys {
			v.Label, v.Sub = keys, ""
		}
		l.Cells[row*tc.Cols+col] = v
	}
	return l
}

// renderPNG は実機なしで画面の見た目を PNG に書き出す（確認用）。
func renderPNG(cfg *Config, out string, pressedSpec string, w, h int) error {
	if cfg.Touch == nil {
		return errors.New("config has no touch section")
	}
	l := buildLayout(cfg.Touch)
	cv := NewCanvas(w, h, w*rgb565.Bpp, rgb565, cfg.Display.Rotate)
	l.W, l.H = cv.W, cv.H
	pressed := make([]bool, len(l.Cells))
	for _, p := range strings.Fields(strings.ReplaceAll(pressedSpec, ";", " ")) {
		var c, r int
		if _, err := fmt.Sscanf(p, "%d,%d", &c, &r); err == nil && c < l.Cols && r < l.Rows {
			pressed[r*l.Cols+c] = true
		}
	}
	drawAll(cv, l, pressed)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, cv.Image())
}

func main() {
	calibrate := flag.Bool("calibrate", false, "タッチの生座標を表示し、四隅から座標範囲を求める")
	flag.BoolVar(&verbose, "v", false, "受け取ったイベントと送ったHIDレポートを標準エラーに出す")
	restore := flag.Bool("restore-console", false, "異常終了で残った専用 VT を元に戻して終わる（systemd の ExecStopPost 用）")
	pngOut := flag.String("render-png", "", "画面の見た目を PNG に書き出して終わる（実機不要）")
	pngPressed := flag.String("render-pressed", "", "-render-png で押下中として描くセル（例: \"0,0 2,1\"）")
	pngSize := flag.String("render-size", "800x480", "-render-png の画面サイズ")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-v] [-calibrate] [-restore-console] [-render-png out.png] [config.yaml]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *restore {
		restoreConsole()
		return
	}

	cfgPath := "/etc/lefthand/config.yaml"
	if flag.NArg() > 0 {
		cfgPath = flag.Arg(0)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	if *calibrate {
		spec := defaultTouch
		if cfg.Touch != nil {
			spec = cfg.Touch.Device
		}
		runCalibrate(spec)
		return
	}

	keymap := map[evdev.EvCode]Combo{}
	for name, target := range cfg.Keys {
		code, ok := evdev.KEYFromString[name]
		if !ok {
			log.Fatalf("unknown source key %q", name)
		}
		c, err := parseCombo(target)
		if err != nil {
			log.Fatal(err)
		}
		keymap[code] = c
	}

	var cells map[string]Combo
	if cfg.Touch != nil {
		cells = map[string]Combo{}
		for k, v := range cfg.Touch.Cells {
			var col, row int
			if _, err := fmt.Sscanf(k, "%d,%d", &col, &row); err != nil ||
				col < 0 || col >= cfg.Touch.Cols || row < 0 || row >= cfg.Touch.Rows {
				log.Fatalf("touch cell %q is out of %dx%d grid", k, cfg.Touch.Cols, cfg.Touch.Rows)
			}
			c, err := parseCombo(v.Key)
			if err != nil {
				log.Fatal(err)
			}
			norm := fmt.Sprintf("%d,%d", col, row)
			if _, dup := cells[norm]; dup {
				log.Fatalf("touch cell %q is defined twice", k)
			}
			cells[norm] = c
		}
	}

	if *pngOut != "" {
		var w, h int
		if _, err := fmt.Sscanf(*pngSize, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
			log.Fatalf("bad -render-size %q", *pngSize)
		}
		if err := renderPNG(cfg, *pngOut, *pngPressed, w, h); err != nil {
			log.Fatal(err)
		}
		return
	}

	// CPU が 1 つでも、描画中に入力の goroutine がすぐ動けるようにする
	// （描画スレッドは nice 10 に下げてあるので、OS が入力側を優先する）
	if runtime.GOMAXPROCS(0) < 2 {
		runtime.GOMAXPROCS(2)
	}

	// 入力デバイスを先に開く。失敗したら HID に何も送らずに終わる
	kbd, err := openInput(cfg.Keyboard)
	if err != nil {
		log.Fatalf("open keyboard: %v", err)
	}
	var touch *evdev.InputDevice
	if cfg.Touch != nil {
		if touch, err = openInput(cfg.Touch.Device); err != nil {
			log.Fatalf("open touch: %v", err)
		}
	}

	s := &State{hid: NewHIDWriter(cfg.HIDDevice), active: map[string]Combo{}}

	// 終了時に押しっぱなしを防ぐ
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		got := <-sig
		log.Printf("%v received, releasing all keys", got)
		s.shutdown(0)
	}()
	go s.retryLoop()

	// 起動直後の状態を揃える（前回異常終了したときの押しっぱなしも解除される）
	s.releaseAll()

	if err := kbd.Grab(); err != nil {
		log.Printf("grab keyboard: %v", err)
	}
	log.Printf("keyboard: %s, %d keys mapped", describe(kbd), len(keymap))
	var disp *Display
	if touch != nil {
		if err := touch.Grab(); err != nil {
			log.Printf("grab touch: %v", err)
		}
		log.Printf("touch: %s, %dx%d grid, %d cells mapped", describe(touch), cfg.Touch.Cols, cfg.Touch.Rows, len(cells))
		// 画面が使えなくても入力は動かす
		if cfg.displayEnabled() {
			d, err := StartDisplay(cfg.Display, buildLayout(cfg.Touch))
			if err != nil {
				log.Printf("display disabled: %v", err)
			} else {
				disp = d
				addAtExit(d.Close)
			}
		}
		go runTouch(touch, cfg.Touch, cells, s, disp)
	}
	runKeyboard(kbd, keymap, s)
}
