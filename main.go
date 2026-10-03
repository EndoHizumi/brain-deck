// 左手デバイス変換デーモン（雛形）
// Brainのキーボード/タッチパネルの evdev 入力を読み、config.yaml に従って
// /dev/hidg0 に HID キーボードレポートを書き込む。
//
// ビルド:
//   go mod init lefthand && go mod tidy
//   GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -o lefthand
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	evdev "github.com/holoplot/go-evdev"
	"gopkg.in/yaml.v3"
)

// ---------- 設定 ----------

type Config struct {
	HIDDevice string            `yaml:"hid_device"`
	Keyboard  string            `yaml:"keyboard"`
	Keys      map[string]string `yaml:"keys"` // "KEY_A": "LCTRL+Z"
	Touch     *TouchConfig      `yaml:"touch"`
}

type TouchConfig struct {
	Device string            `yaml:"device"`
	Cols   int               `yaml:"cols"`
	Rows   int               `yaml:"rows"`
	MinX   int32             `yaml:"min_x"`
	MaxX   int32             `yaml:"max_x"`
	MinY   int32             `yaml:"min_y"`
	MaxY   int32             `yaml:"max_y"`
	Cells  map[string]string `yaml:"cells"` // "col,row": "LCTRL+S"
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

// ---------- 押下状態とレポート送信 ----------

type State struct {
	mu     sync.Mutex
	hid    *os.File
	active map[string]Combo // 入力元ID -> 出力
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

// 呼び出し側でロック済みであること
func (s *State) send() {
	r := make([]byte, 8)
	n := 0
	for _, c := range s.active {
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
	if _, err := s.hid.Write(r); err != nil {
		// PC未接続時などはエラーになるのでログだけ
		log.Printf("hid write: %v", err)
	}
}

// ---------- 入力 ----------

func runKeyboard(path string, keymap map[evdev.EvCode]Combo, s *State) {
	dev, err := evdev.Open(path)
	if err != nil {
		log.Fatalf("open keyboard: %v", err)
	}
	if err := dev.Grab(); err != nil {
		log.Printf("grab keyboard: %v", err)
	}
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Fatalf("read keyboard: %v", err)
		}
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

func runTouch(tc *TouchConfig, cells map[string]Combo, s *State) {
	dev, err := evdev.Open(tc.Device)
	if err != nil {
		log.Fatalf("open touch: %v", err)
	}
	if err := dev.Grab(); err != nil {
		log.Printf("grab touch: %v", err)
	}
	var x, y int32
	down, pressed := false, false
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Fatalf("read touch: %v", err)
		}
		switch {
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_X:
			x = ev.Value
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_Y:
			y = ev.Value
		case ev.Type == evdev.EV_KEY && ev.Code == evdev.BTN_TOUCH:
			down = ev.Value == 1
			if !down && pressed {
				s.release("t")
				pressed = false
			}
		case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT:
			// 触れた瞬間のセルで確定（スライドしても変えない）
			if down && !pressed {
				key := fmt.Sprintf("%d,%d",
					cellOf(x, tc.MinX, tc.MaxX, tc.Cols),
					cellOf(y, tc.MinY, tc.MaxY, tc.Rows))
				if c, ok := cells[key]; ok {
					s.press("t", c)
				}
				pressed = true
			}
		}
	}
}

// ---------- main ----------

func main() {
	cfgPath := "/etc/lefthand/config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.HIDDevice == "" {
		cfg.HIDDevice = "/dev/hidg0"
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

	hid, err := os.OpenFile(cfg.HIDDevice, os.O_WRONLY, 0)
	if err != nil {
		log.Fatalf("open hid: %v", err)
	}
	s := &State{hid: hid, active: map[string]Combo{}}

	// 終了時に押しっぱなしを防ぐ
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		s.releaseAll()
		os.Exit(0)
	}()

	if cfg.Touch != nil {
		cells := map[string]Combo{}
		for k, v := range cfg.Touch.Cells {
			c, err := parseCombo(v)
			if err != nil {
				log.Fatal(err)
			}
			cells[k] = c
		}
		go runTouch(cfg.Touch, cells, s)
	}
	runKeyboard(cfg.Keyboard, keymap, s)
}
