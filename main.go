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
//	lefthand -check [config.yaml]          設定を検証する
//
// 動作中は /dev/ttyGS1（USB シリアル）で設定 GUI のリクエストを受ける（control.go）。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	evdev "github.com/holoplot/go-evdev"
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
	// テンキー。PC の配列（US/日本語）によらず同じ文字になるので、Ctrl++ などに使える
	"KPSLASH": 0x54, "KPASTERISK": 0x55, "KPMINUS": 0x56, "KPPLUS": 0x57,
	"KPENTER": 0x58, "KPDOT": 0x63,
}

func init() {
	for i := 0; i < 26; i++ {
		hidUsage[string(rune('A'+i))] = byte(0x04 + i)
	}
	for i := 1; i <= 9; i++ {
		hidUsage[fmt.Sprint(i)] = byte(0x1e + i - 1)
	}
	hidUsage["0"] = 0x27
	for i := 1; i <= 9; i++ {
		hidUsage[fmt.Sprintf("KP%d", i)] = byte(0x59 + i - 1)
	}
	hidUsage["KP0"] = 0x62
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

func runKeyboard(dev *evdev.InputDevice, e *Engine, mon *Monitor) {
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Printf("read keyboard: %v", err)
			e.out.shutdown(1)
		}
		if ev.Type == evdev.EV_SYN || ev.Type == evdev.EV_MSC {
			continue
		}
		if ev.Type != evdev.EV_KEY {
			vlogf("kbd: %s", evString(ev))
			continue
		}
		switch ev.Value {
		case 1:
			vlogf("kbd: %s", evString(ev))
			if mon.KeyPressed(e, ev.Code) {
				continue // 学習モード：設定 GUI に知らせるだけで、PC には送らない
			}
			e.PressKey(ev.Code)
		case 0:
			vlogf("kbd: %s", evString(ev))
			e.ReleaseKey(ev.Code)
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

// touchCell はタッチの生座標から cols×rows の格子のセルを求める。画面の枠も同じ境界で描く（cellSpan）。
func touchCell(tc *TouchConfig, cols, rows int, x, y int32) (col, row int) {
	if tc.SwapXY {
		x, y = y, x
	}
	return cellOf(x, tc.MinX, tc.MaxX, cols), cellOf(y, tc.MinY, tc.MaxY, rows)
}

// touchPoint は、タッチの生座標を W×H の画面のドットにする。セルの境界（touchCell）と同じ割り方。
func touchPoint(tc *TouchConfig, x, y int32, W, H int) image.Point {
	if tc.SwapXY {
		x, y = y, x
	}
	return image.Pt(cellOf(x, tc.MinX, tc.MaxX, W), cellOf(y, tc.MinY, tc.MaxY, H))
}

func evTime(ev *evdev.InputEvent) time.Time {
	return time.Unix(int64(ev.Time.Sec), int64(ev.Time.Usec)*1000)
}

func runTouch(dev *evdev.InputDevice, e *Engine, disp *Display, mon *Monitor) {
	var x, y int32
	down, pressed := false, false
	var lit *TouchHit // ハイライト中のセル
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Printf("read touch: %v", err)
			e.out.shutdown(1)
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
				e.Release("t")
				pressed = false
				if lit != nil {
					disp.SetPressed(lit.Gen, lit.Col, lit.Row, false)
					lit = nil
				}
			}
		case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT:
			// 触れた瞬間のセルで確定（スライドしても変えない）
			if down && !pressed {
				if mon.TouchPressed(e, x, y) {
					// 学習モード：設定 GUI に知らせるだけ。離したときもエンジンには何も残っていない
					vlogf("touch: x=%d y=%d -> settings GUI only", x, y)
					pressed = true
					continue
				}
				h := e.PressTouch(x, y)                 // HID を先に送り、描画はそのあと
				if h.Mapped && h.Soft == "" && !h.Own { // Todo は押した行を自分で描く
					disp.SetPressed(h.Gen, h.Col, h.Row, true)
					lit = &h
				}
				where := fmt.Sprintf("cell %d,%d", h.Col, h.Row)
				if h.Soft != "" {
					where = "soft key " + h.Soft
				}
				vlogf("touch: x=%d y=%d -> %s (mapped=%v) %v after event",
					x, y, where, h.Mapped, time.Since(evTime(ev)).Round(100*time.Microsecond))
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

// ---------- 画面の内容 ----------

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
	"KPSLASH": "/", "KPASTERISK": "*", "KPMINUS": "-", "KPPLUS": "+", "KPENTER": "Enter", "KPDOT": ".",
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

// 画面に出す、レイヤー切り替えの種類
var layerVerb = map[ActKind]string{
	actHold: "押す間", actToggle: "切替", actOneshot: "1回", actTo: "移動",
}

// cellView は割り当てから、セルに描く内容を作る。
func cellView(km *Keymap, a *Action) CellView {
	if a == nil {
		return CellView{}
	}
	label := a.Spec.Label
	if a.Widget != nil {
		// ウィジェットのセル。label は上に小さく出す見出し。タップで送るキーは出さない
		return CellView{Mapped: true, Layer: a.Kind.isLayer(), Label: label, Widget: a.Widget, Background: a.Spec.Background}
	}
	var sub string
	if a.Kind.isLayer() {
		dest := km.Layers[a.Layer].title()
		sub = layerVerb[a.Kind]
		if label == "" {
			label = dest
		} else {
			sub += ":" + dest
		}
		return CellView{Mapped: true, Layer: true, Label: label, Sub: sub, Background: a.Spec.Background}
	}
	keys := prettyCombo(a.Spec.Key)
	v := CellView{Mapped: true, Label: label, Sub: keys, Background: a.Spec.Background}
	if v.Label == "" || v.Label == keys {
		v.Label, v.Sub = keys, ""
	}
	return v
}

// buildLayout は今の重なりから、画面に描くセルの内容を作る。
func buildLayout(km *Keymap, v *View) *Layout {
	l := &Layout{Cols: v.Cols, Rows: v.Rows, Cells: make([]CellView, len(v.Cells)),
		Gen: v.Gen, Title: km.Layers[v.Top].title(), Mode: v.Mode, Press: km.Press, Wallpaper: v.Wallpaper}
	for i, a := range v.Cells {
		switch anc := v.Anchor[i]; {
		case anc == i:
			l.Cells[i] = cellView(km, a)
			l.Cells[i].SpanW, l.Cells[i].SpanH = a.SpanW, a.SpanH
		case anc >= 0:
			l.Cells[i] = CellView{Covered: true}
		}
	}
	return l
}

// renderPNG は実機なしで画面の見た目を PNG に書き出す（確認用）。
// layer を指定すると、そのレイヤーを base の上に重ねた画面を描く（hold なら一時的な色）。
// env はウィジェットを描くときの時刻など（-render-time、-render-unsynced）。
// images は背景画像の読み込み先（-render-images。省略すると -data-dir の images）。
func renderPNG(cfg *Config, km *Keymap, out, layer, pressedSpec, pressStyle string, w, h int, env WidgetEnv, images ImageSource) error {
	if cfg.Touch == nil {
		return errors.New("config has no touch section")
	}
	switch pressStyle {
	case "":
	case pressBorder, pressFill:
		km.Press = pressStyle
	default:
		return fmt.Errorf("-render-press-style %q: must be border or fill", pressStyle)
	}
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	if layer != "" {
		name, kind, _ := strings.Cut(layer, ":")
		a := &Action{Kind: actToggle}
		switch kind {
		case "", "toggle":
		case "hold":
			a.Kind = actHold
		default:
			return fmt.Errorf("-render-layer %q: mode must be toggle or hold", layer)
		}
		a.Layer = -1
		for i, l := range km.Layers {
			if l.Name == name {
				a.Layer = i
			}
		}
		if a.Layer < 0 {
			return fmt.Errorf("-render-layer: unknown layer %q", name)
		}
		if a.Layer != 0 {
			e.press("render", a)
		}
	}
	l := buildLayout(km, e.View())
	cv := NewCanvas(w, h, w*rgb565.Bpp, rgb565, cfg.Display.Rotate)
	l.W, l.H, l.Env, l.Images = cv.W, cv.H, env, images
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

// ---------- main ----------

func main() {
	calibrate := flag.Bool("calibrate", false, "タッチの生座標を表示し、四隅から座標範囲を求める")
	flag.BoolVar(&verbose, "v", false, "受け取ったイベントと送ったHIDレポートを標準エラーに出す")
	restore := flag.Bool("restore-console", false, "異常終了で残った専用 VT を元に戻して終わる（systemd の ExecStopPost 用）")
	check := flag.Bool("check", false, "設定を検証して終わる")
	dumpJSON := flag.Bool("dump-json", false, "設定を検証し、layers の形にそろえた JSON を標準出力に書いて終わる")
	pngOut := flag.String("render-png", "", "画面の見た目を PNG に書き出して終わる（実機不要）")
	pngLayer := flag.String("render-layer", "", "-render-png で base に重ねるレイヤー（例: edit、edit:hold）")
	pngPressed := flag.String("render-pressed", "", "-render-png で押下中として描くセル（例: \"0,0 2,1\"）")
	pngPress := flag.String("render-press-style", "", "-render-png で、設定の display.press_style の代わりに使う見せ方（border、fill）")
	pngSize := flag.String("render-size", "800x480", "-render-png の画面サイズ")
	pngTime := flag.String("render-time", "", "-render-png で時計に出す時刻（RFC 3339。例: 2026-10-06T09:41:00+09:00）。省略すると今")
	pngUnsynced := flag.Bool("render-unsynced", false, "-render-png で、時刻を一度も合わせていないときの時計を描く")
	pngTexts := flag.String("render-texts", "", "-render-png で、テキストのタイルに出す中身（text.json の形のファイル）")
	pngTodo := flag.String("render-todo", "", "-render-png で、Todo のセルに出す項目（todo.json の形のファイル）")
	pngTodoPage := flag.Int("render-todo-page", 1, "-render-png で、Todo のセルに出すページ（1 から）")
	pngCal := flag.String("render-calendar", "", "-render-png で、カレンダーのセルに出す予定（calendar.json の形のファイル）")
	pngCalPage := flag.Int("render-calendar-page", 0, "-render-png で、カレンダーのセルに出すページ（1 から。0 なら触っていないときのページ）")
	pngImages := flag.String("render-images", "", "-render-png と -check で、背景画像を探すディレクトリ（省略すると -data-dir の images）")
	dataDir := flag.String("data-dir", defaultDataDir, "ウィジェットのデータ、背景画像、時刻合わせの記録を置くディレクトリ")
	serialPath := flag.String("serial", "/dev/ttyGS1", "設定 GUI と通信するシリアル。空なら使わない")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-v] [-calibrate] [-check] [-dump-json] [-restore-console] [-render-png out.png] [config.yaml]\n", os.Args[0])
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

	km, warns, err := compileKeymap(cfg)
	if err != nil {
		var msgs []string
		for _, p := range asProblems(err) {
			msg := p.Message
			if p.Path != "" {
				msg += "  [" + p.Path + "]"
			}
			msgs = append(msgs, msg)
		}
		log.Fatalf("%s:\n%s", cfgPath, strings.Join(msgs, "\n"))
	}
	for _, w := range warns {
		log.Printf("warning: %s", w)
	}

	imagesDir := filepath.Join(*dataDir, "images")
	if *pngImages != "" {
		imagesDir = *pngImages
	}
	switch {
	case *check:
		logLayers(km)
		w, h := screenSize(cfg)
		for _, p := range imageProblems(km, NewImageStore(imagesDir), w, h) {
			log.Printf("warning: %s  [%s]", p.Message, p.Path)
		}
		log.Printf("%s: ok", cfgPath)
		return
	case *dumpJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(cfg); err != nil {
			log.Fatal(err)
		}
		return
	case *pngOut != "":
		var w, h int
		if _, err := fmt.Sscanf(*pngSize, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
			log.Fatalf("bad -render-size %q", *pngSize)
		}
		env := WidgetEnv{Now: time.Now(), TimeSynced: !*pngUnsynced}
		if *pngTime != "" {
			t, err := time.Parse(time.RFC3339, *pngTime)
			if err != nil {
				log.Fatalf("bad -render-time %q: %v", *pngTime, err)
			}
			env.Now = t
		}
		if *pngTexts != "" {
			b, err := os.ReadFile(*pngTexts)
			if err != nil {
				log.Fatal(err)
			}
			var f textFile
			if err := json.Unmarshal(b, &f); err != nil {
				log.Fatalf("%s: %v", *pngTexts, err)
			}
			env.Texts = f.Texts
		}
		if *pngTodo != "" {
			b, err := os.ReadFile(*pngTodo)
			if err != nil {
				log.Fatal(err)
			}
			var f todoFile
			if err := json.Unmarshal(b, &f); err != nil {
				log.Fatalf("%s: %v", *pngTodo, err)
			}
			env.Todo = TodoList{Rev: f.Rev, Items: f.Items}
		}
		if *pngCal != "" {
			b, err := os.ReadFile(*pngCal)
			if err != nil {
				log.Fatal(err)
			}
			var d CalendarData
			if err := json.Unmarshal(b, &d); err != nil {
				log.Fatalf("%s: %v", *pngCal, err)
			}
			if err := d.prepare(); err != nil {
				log.Fatalf("%s: %v", *pngCal, err)
			}
			env.Calendar = &d
		}
		for _, l := range km.Layers {
			if l.Grid == nil {
				continue
			}
			for _, a := range l.Grid.Cells {
				switch {
				case a.Widget == nil:
				case a.Widget.Kind == widgetTodo && *pngTodoPage > 1:
					a.Widget.pager.setPage(*pngTodoPage-1, env.Now)
				case a.Widget.Kind == widgetCal && *pngCalPage > 0:
					a.Widget.pager.setPage(*pngCalPage-1, env.Now)
				}
			}
		}
		images := NewImageStore(imagesDir)
		w2, h2 := screenSize(cfg)
		for _, p := range imageProblems(km, images, w2, h2) {
			log.Printf("warning: %s  [%s]", p.Message, p.Path)
		}
		if err := renderPNG(cfg, km, *pngOut, *pngLayer, *pngPressed, *pngPress, w, h, env, images); err != nil {
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
	e := NewEngine(km, s)
	store := OpenStore(*dataDir)
	clock := NewTimeService(store)
	if !clock.Synced() {
		log.Printf("time: not set since boot (the clock widget shows %q until the settings GUI connects)", unsyncedText)
	}
	texts := NewTextService(store)
	todos := NewTodoService(store)
	cals := NewCalendarService(store)
	images := OpenImageStore(imagesDir)
	if ps := imageProblems(km, images, 800, 480); len(ps) > 0 {
		for _, p := range ps {
			log.Printf("warning: %s", p.Message)
		}
	}
	widgetRT := NewWidgetRT(todos)
	e.SetWidgets(widgetRT)
	widgetEnv := func() WidgetEnv {
		env := clock.Env()
		env.Texts = texts.Snapshot()
		env.Todo = todos.Snapshot()
		env.Calendar = cals.Snapshot()
		return env
	}
	widgetRT.SetEnv(widgetEnv)

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
	log.Printf("keyboard: %s", describe(kbd))
	logLayers(km)
	var disp *Display
	if touch != nil {
		if err := touch.Grab(); err != nil {
			log.Printf("grab touch: %v", err)
		}
		log.Printf("touch: %s, %d soft keys", describe(touch), len(km.Areas))
		// 画面が使えなくても入力は動かす
		if cfg.displayEnabled() {
			first := buildLayout(km, e.View())
			d, err := StartDisplay(cfg.Display, first, widgetEnv, images)
			if err != nil {
				log.Printf("display disabled: %v", err)
			} else {
				disp = d
				addAtExit(d.Close)
				clock.SetOnChange(d.Poke)
				texts.SetOnChange(d.Poke)
				todos.SetOnChange(d.Poke)
				cals.SetOnChange(d.Poke)
				images.SetOnChange(d.Invalidate)
				images.Preload(preloadOrder(cfg))
				widgetRT.SetScreen(first.W, first.H, d.Poke)
				// レイヤーが変わったり、設定を差し替えたりしたら描き直す。SetLayout は待たずに返る
				e.SetOnView(func(v *View) { d.SetLayout(buildLayout(v.km, v)) }, first.Gen)
			}
		}
	}

	// 設定 GUI（USB シリアル）。通信が止まっても、入力の処理は待たない
	mon := &Monitor{}
	e.SetOnStatus(mon.LayerChanged)
	if *serialPath != "" {
		ctl := &Controller{
			store: &configStore{path: cfgPath, cfg: cfg, km: km, apply: func(c *Config, k *Keymap) error {
				if err := reloader(e)(c, k); err != nil {
					return err
				}
				images.Preload(preloadOrder(c)) // 新しい設定で使う画像を先に読む
				return nil
			}},
			engine:  e,
			monitor: mon,
			clock:   clock,
			texts:   texts,
			todos:   todos,
			cals:    cals,
			images:  images,
			started: time.Now(),
		}
		todos.SetOnNotify(mon.TodoChanged)
		go ctl.runSerial(*serialPath)
	}

	if touch != nil {
		go runTouch(touch, e, disp, mon)
	}
	runKeyboard(kbd, e, mon)
}

// reloader は、保存した設定を動いているエンジンに反映する関数を返す。
// 画面の描き直しは、エンジンの onView から行われる。
func reloader(e *Engine) func(*Config, *Keymap) error {
	return func(cfg *Config, km *Keymap) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic while applying: %v", r)
			}
		}()
		e.Reload(km)
		return nil
	}
}
