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
	"log"
	"os"
	"os/signal"
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
}

type TouchConfig struct {
	Device string            `yaml:"device"` // "/dev/input/eventN" またはデバイス名
	Cols   int               `yaml:"cols"`
	Rows   int               `yaml:"rows"`
	SwapXY bool              `yaml:"swap_xy"` // パネルのX軸が画面の縦方向のとき
	MinX   int32             `yaml:"min_x"`
	MaxX   int32             `yaml:"max_x"`
	MinY   int32             `yaml:"min_y"`
	MaxY   int32             `yaml:"max_y"`
	Cells  map[string]string `yaml:"cells"` // "col,row": "LCTRL+S"
}

const (
	defaultHID      = "/dev/hidg0"
	defaultKeyboard = "brain-kbd-i2c"
	defaultTouch    = "mxs-lradc-ts"
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

// shutdown は空レポートを送ってから終了する。
func (s *State) shutdown(code int) {
	s.releaseAll()
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

func runTouch(dev *evdev.InputDevice, tc *TouchConfig, cells map[string]Combo, s *State) {
	var x, y int32
	down, pressed := false, false
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
			}
		case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT:
			// 触れた瞬間のセルで確定（スライドしても変えない）
			if down && !pressed {
				px, py := x, y
				if tc.SwapXY {
					px, py = y, x
				}
				key := fmt.Sprintf("%d,%d",
					cellOf(px, tc.MinX, tc.MaxX, tc.Cols),
					cellOf(py, tc.MinY, tc.MaxY, tc.Rows))
				c, ok := cells[key]
				vlogf("touch: x=%d y=%d -> cell %s (mapped=%v)", x, y, key, ok)
				if ok {
					s.press("t", c)
				}
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
	return &cfg, nil
}

func main() {
	calibrate := flag.Bool("calibrate", false, "タッチの生座標を表示し、四隅から座標範囲を求める")
	flag.BoolVar(&verbose, "v", false, "受け取ったイベントと送ったHIDレポートを標準エラーに出す")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-v] [-calibrate] [config.yaml]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

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
			c, err := parseCombo(v)
			if err != nil {
				log.Fatal(err)
			}
			cells[fmt.Sprintf("%d,%d", col, row)] = c
		}
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
	if touch != nil {
		if err := touch.Grab(); err != nil {
			log.Printf("grab touch: %v", err)
		}
		log.Printf("touch: %s, %dx%d grid, %d cells mapped", describe(touch), cfg.Touch.Cols, cfg.Touch.Rows, len(cells))
		go runTouch(touch, cfg.Touch, cells, s)
	}
	runKeyboard(kbd, keymap, s)
}
