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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

var verbose bool

// userActivity は、本体キーかタッチがあったときに呼ぶ（電池が少なくて電源を切ろうとしているのを取りやめる）。
var userActivity = func() {}

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
	// F13〜F24。PC の OS のショートカットに、コマンドを割り当てて使う（キーボードにないので、ほかと重ならない）
	for i := 13; i <= 24; i++ {
		hidUsage[fmt.Sprintf("F%d", i)] = byte(0x68 + i - 13)
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
	mu       sync.Mutex // キーボードとマウスが同じデバイスに書く
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

// Close はデバイスを閉じる（USB を付け直す前）。次に書くときに開き直す。
func (w *HIDWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fd >= 0 {
		syscall.Close(w.fd)
		w.fd = -1
	}
	w.lastOpen = time.Time{}
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
	w.mu.Lock()
	defer w.mu.Unlock()
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
	hid    hidOut
	active map[string]Combo // 入力元ID -> 出力
	dirty  bool             // 最後の送信が失敗し、現在の状態がPCに届いていない
	kbdID  byte             // キーボードのレポート ID。0 なら付けない（キーボードだけの形。hid.go）
	mouse  *Mouse           // nil ならマウスはない
	media  *Media           // nil ならメディアキーはない
	pen    *Pen             // nil ならペンはない
}

func (s *State) press(id string, c Combo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[id] = c
	s.send()
	hostWaker.Request() // PC がスリープしていれば起こす（wakeup.go）
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

// releaseAll は、キーとマウスのボタンをすべて離す（終了、設定の再読み込み）。
func (s *State) releaseAll() {
	s.mu.Lock()
	s.active = map[string]Combo{}
	s.send()
	s.mu.Unlock()
	s.mouse.ReleaseAll()
	s.media.ReleaseAll()
	s.pen.Lift()
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
		s.mouse.retry()
		s.media.retry()
		s.pen.retry()
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
	if s.kbdID != 0 {
		r = append([]byte{s.kbdID}, r...)
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

func runKeyboard(dev *evdev.InputDevice, e *Engine, mon *Monitor, dim *Dimmer, lid *Lid) {
	kf := &keyFilter{lid: lid}
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Printf("read keyboard: %v", err)
			e.out.shutdown(1)
		}
		if kf.drop(ev) {
			continue // タブレットの形：本体のキーを使わない（lid.go）
		}
		if ev.Type == evdev.EV_SYN || ev.Type == evdev.EV_MSC {
			continue
		}
		if ev.Type == evdev.EV_SW { // ふたの開け閉め、タブレットの形（lid.go）
			vlogf("kbd: %s", evString(ev))
			lid.Switch(ev.Code, ev.Value)
			continue
		}
		if ev.Type != evdev.EV_KEY {
			vlogf("kbd: %s", evString(ev))
			continue
		}
		switch ev.Value {
		case 1:
			vlogf("kbd: %s", evString(ev))
			dim.Key() // キーは暗いときもそのまま PC に送る（明るくするかは設定の wake_on_key）
			userActivity()
			if mon.KeyPressed(e, ev.Code) {
				continue // 学習モード：設定 GUI に知らせるだけで、PC には送らない
			}
			e.PressKey(ev.Code)
		case 0:
			vlogf("kbd: %s", evString(ev))
			e.ReleaseKey(ev.Code)
		case 2: // オートリピート。HID では無視（PC 側でリピートする）。端末モードでは文字を繰り返す
			e.RepeatKey(ev.Code)
		}
	}
}

// keyFilter は、タブレットの形のあいだ（Lid.KeysBlocked）、本体のキーを捨てる。捨て始める前に押していたキーの離しは通し
// （押しっぱなしにしない）、捨てているあいだに押したキーは、離しもリピートも捨てる（戻したあとに離しだけが届かないように）。
type keyFilter struct {
	lid     *Lid
	dropped map[evdev.EvCode]bool
}

func (f *keyFilter) drop(ev *evdev.InputEvent) bool {
	if ev.Type != evdev.EV_KEY {
		return false
	}
	if f.dropped[ev.Code] {
		if ev.Value == 0 {
			delete(f.dropped, ev.Code)
		}
		return true
	}
	if ev.Value == 1 && f.lid.KeysBlocked() {
		if f.dropped == nil {
			f.dropped = map[evdev.EvCode]bool{}
		}
		f.dropped[ev.Code] = true
		vlogf("kbd: %s ignored (tablet mode)", evString(ev))
		return true
	}
	return false
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
// キャリブレーション（touchcal.go）があれば、そちらで画面のドットにしてから割る。
func touchCell(tc *TouchConfig, cols, rows int, x, y int32) (col, row int) {
	if c := touchCalNow.Load(); c != nil {
		X, Y, _ := calPhys(x, y, c.W, c.H)
		return calCell(X, c.W, cols), calCell(Y, c.H, rows)
	}
	if tc.SwapXY {
		x, y = y, x
	}
	return cellOf(x, tc.MinX, tc.MaxX, cols), cellOf(y, tc.MinY, tc.MaxY, rows)
}

// touchPoint は、タッチの生座標を W×H の画面のドットにする。セルの境界（touchCell）と同じ割り方。
func touchPoint(tc *TouchConfig, x, y int32, W, H int) image.Point {
	if X, Y, ok := calPhys(x, y, W, H); ok {
		return image.Pt(calCell(X, W, W), calCell(Y, H, H))
	}
	if tc.SwapXY {
		x, y = y, x
	}
	return image.Pt(cellOf(x, tc.MinX, tc.MaxX, W), cellOf(y, tc.MinY, tc.MaxY, H))
}

func evTime(ev *evdev.InputEvent) time.Time {
	return time.Unix(int64(ev.Time.Sec), int64(ev.Time.Usec)*1000)
}

func runTouch(dev *evdev.InputDevice, e *Engine, disp *Display, mon *Monitor, dim *Dimmer) {
	tp := &touchProc{e: e, disp: disp, mon: mon, dim: dim}
	for {
		ev, err := dev.ReadOne()
		if err != nil {
			log.Printf("read touch: %v", err)
			e.out.shutdown(1)
		}
		tp.event(ev, time.Now())
	}
}

// touchProc は、タッチのイベントを 1 つずつ処理する。実機（runTouch）と、記録したタッチの再生（replayTouch）で同じものを使う。
// t はイベントを受け取った時刻（再生では記録した時刻）。トラックパッドの判定に使う。
type touchProc struct {
	e    *Engine
	disp *Display
	mon  *Monitor
	dim  *Dimmer // 自動減光。nil なら使わない

	x, y, p       int32
	down, pressed bool
	pad           bool        // トラックパッドのセルを押している
	stream        touchStream // ペンタブレット、手書きメモのセルを押している
	lit           *TouchHit   // ハイライト中のセル
	swallowed     bool        // 暗いときに触れたので、明るくしただけ。離すまで何もしない
	soft          string      // キャリブレーションのあいだに押したソフトキー
	calFirst      bool        // キャリブレーションのあいだ：押してから最初の読み
}

func (tp *touchProc) event(ev *evdev.InputEvent, t time.Time) {
	e := tp.e
	switch {
	case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_X:
		tp.x = ev.Value
	case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_Y:
		tp.y = ev.Value
	case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_PRESSURE:
		tp.p = ev.Value
	case ev.Type == evdev.EV_KEY && ev.Code == evdev.BTN_TOUCH:
		vlogf("touch: %s", evString(ev))
		tp.down = ev.Value == 1
		if tp.calibTouch(true) {
			return
		}
		if !tp.down && tp.pressed && tp.swallowed {
			tp.pressed, tp.swallowed = false, false
			tp.dim.TouchUp()
			return
		}
		if !tp.down && tp.pressed {
			tp.dim.TouchUp()
			if tp.stream != nil {
				tp.stream.Up(t)
				tp.stream = nil
			}
			if tp.pad {
				e.pad.Up(t)
				tp.pad = false
			}
			e.Release("t")
			tp.pressed = false
			if tp.lit != nil {
				tp.disp.SetPressed(tp.lit.Gen, tp.lit.Col, tp.lit.Row, false)
				tp.lit = nil
			}
		}
	case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT:
		if tp.calibTouch(false) {
			return
		}
		if tp.down && tp.pressed && tp.pad {
			e.pad.Sample(tp.x, tp.y, tp.p, t)
			return
		}
		if tp.down && tp.pressed && tp.stream != nil {
			tp.stream.Sample(tp.x, tp.y, tp.p, t)
			return
		}
		// 触れた瞬間のセルで確定（スライドしても変えない）
		if tp.down && !tp.pressed {
			x, y := tp.x, tp.y
			userActivity()
			if tp.dim.TouchDown() {
				// 暗かった：明るくするだけで、セルもソフトキーも押さない（設定 GUI の学習モードにも知らせない）
				vlogf("touch: x=%d y=%d -> woke the backlight only", x, y)
				tp.pressed, tp.swallowed = true, true
				return
			}
			if tp.mon.TouchPressed(e, x, y) {
				// 学習モード：設定 GUI に知らせるだけ。離したときもエンジンには何も残っていない
				vlogf("touch: x=%d y=%d -> settings GUI only", x, y)
				tp.pressed = true
				return
			}
			h := e.PressTouchAt(x, y, t)            // HID を先に送り、描画はそのあと
			if h.Mapped && h.Soft == "" && !h.Own { // Todo とトラックパッドは押したことを自分で描く（描かない）
				tp.disp.SetPressed(h.Gen, h.Col, h.Row, true)
				tp.lit = &h
			}
			if h.Pad {
				tp.pad = true
				e.pad.Sample(x, y, tp.p, t)
			}
			if h.Stream != nil {
				tp.stream = h.Stream
				tp.stream.Sample(x, y, tp.p, t)
			}
			where := fmt.Sprintf("cell %d,%d", h.Col, h.Row)
			if h.Soft != "" {
				where = "soft key " + h.Soft
			}
			vlogf("touch: x=%d y=%d -> %s (mapped=%v) %v after event",
				x, y, where, h.Mapped, time.Since(evTime(ev)).Round(100*time.Microsecond))
			tp.pressed = true
		}
	}
}

// calibTouch は、タッチのキャリブレーション（calibui.go）のあいだ、生のタッチをそちらに回す。回したら true。
// 始める前から押していたタッチ（「タッチ調整」のセル）は、離すまでいつもどおりに扱う。
// ソフトキーの帯は生の座標で決めるので、キャリブレーションの最中も、戻る（やめる）と決定（保存）に使える。
// BTN_TOUCH の押し下げは、その回の座標の前に来るので、ソフトキーかどうかは最初の SYN_REPORT で決める。
func (tp *touchProc) calibTouch(btn bool) bool {
	c := tp.e.calib
	if tp.pressed || !c.Active() {
		return false
	}
	if btn {
		if tp.down {
			userActivity()
			tp.dim.TouchDown()
			tp.calFirst, tp.soft = true, ""
			return true
		}
		tp.dim.TouchUp()
		if tp.soft != "" {
			soft := tp.soft
			tp.soft = ""
			c.SoftKey(soft)
			return true
		}
		c.Touch(false, tp.x, tp.y)
		return true
	}
	if !tp.down {
		return true
	}
	if tp.calFirst {
		tp.calFirst = false
		if s, ok := tp.e.km.hitSoft(tp.x, tp.y); ok {
			tp.soft = s
		}
	}
	if tp.soft == "" {
		c.Touch(true, tp.x, tp.y)
	}
	return true
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
		return CellView{Mapped: true, Layer: a.Kind.isLayer(), Label: label, Widget: a.Widget, Background: a.Spec.Background,
			NeedsPC: a.needsPC() || a.Widget.fromPC()}
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
	if a.Kind == actMouse {
		keys = a.Mouse.Pretty
	}
	if a.Kind == actMedia {
		keys = a.Media.Pretty
	}
	if a.Kind == actUSB {
		keys = usbLabels[a.Spec.UsbMode]
	}
	if a.Kind == actTerminal {
		keys = termLabels[a.Spec.Terminal]
	}
	if a.Kind == actRotate {
		keys = rotateLabels[a.Spec.Rotate]
	}
	if a.Kind == actInput {
		keys = inputLabels[a.Spec.Input]
	}
	if a.Kind == actCalibrate {
		keys = calibrateLabels[a.Spec.Calibrate]
	}
	v := CellView{Mapped: true, Label: label, Sub: keys, Background: a.Spec.Background, Media: a.Kind == actMedia, NeedsPC: a.needsPC()}
	if v.Label == "" || v.Label == keys {
		v.Label, v.Sub = keys, ""
	}
	return v
}

// buildLayout は今の重なりから、画面に描くセルの内容を作る。
func buildLayout(km *Keymap, v *View) *Layout {
	l := &Layout{Cols: v.Cols, Rows: v.Rows, Cells: make([]CellView, len(v.Cells)),
		Gen: v.Gen, Title: km.Layers[v.Top].title(), Mode: v.Mode, Press: km.Press, Wallpaper: v.Wallpaper,
		Rotate: v.Rot, Bar: v.Bar, BarItems: km.Bar.Items, Offline: v.Offline}
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
// w×h は物理の画面の大きさで、rot の向きに回して描く。
// homePage はホーム画面に出すページ（0 から）。
// offline なら、PC から外れたときの画面（PC に送るセルを休みと描く）。
func renderPNG(cfg *Config, km *Keymap, out, layer, pressedSpec, pressStyle string, w, h, rot, homePage int, offline bool, env WidgetEnv, images ImageSource) error {
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
	e.SetRotation(rot, w, h, nil)
	if km.Home != nil {
		km.Home.SetPage(homePage)
	}
	if offline {
		e.mu.Lock()
		e.offline = true
		e.view = nil
		e.refresh()
		e.mu.Unlock()
	}
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
		if km.Home == nil || a.Layer != km.Home.Layer {
			e.mu.Lock()
			e.stack = nil // ホーム画面から始まる設定でも、base の上に重ねる
			e.view = nil
			e.refresh()
			e.mu.Unlock()
		}
		if a.Layer != 0 && (km.Home == nil || a.Layer != km.Home.Layer || !km.Home.Start) {
			e.press("render", a)
		}
	}
	l := buildLayout(km, e.View())
	cv := NewCanvas(w, h, w*rgb565.Bpp, rgb565, l.Rotate)
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
	pngMonthOffset := flag.Int("render-month-offset", 0, "-render-png で、月の表示に出す月（今月からいくつ送ったか。-1 で先月）")
	pngMonthSelect := flag.String("render-month-select", "", "-render-png で、月の表示で選んだ日（YYYY-MM-DD）。follow のカレンダーにもその日の予定が出る")
	pngWeather := flag.String("render-weather", "", "-render-png で、天気のセルに出す予報（weather.json の形のファイル）")
	pngIcons := flag.String("render-weather-icons", "", "-render-png で、天気アイコンを探すディレクトリ（省略すると -data-dir の weather-icons）")
	pngImages := flag.String("render-images", "", "-render-png と -check で、背景画像を探すディレクトリ（省略すると -data-dir の images）")
	dataDir := flag.String("data-dir", defaultDataDir, "ウィジェットのデータ、背景画像、時刻合わせの記録を置くディレクトリ")
	serialPath := flag.String("serial", "/dev/ttyGS1", "設定 GUI と通信するシリアル。空なら使わない")
	gadgetSetup := flag.String("gadget-setup", defaultGadgetSetup, "USB の形（キーボードだけ、キーボードとマウス）を切り替えるときに実行する gadget-setup.sh")
	pngMouseOff := flag.Bool("render-mouse-off", false, "-render-png で、USB にマウスがないとき（キーボードだけの形）のトラックパッドを描く")
	pngBattery := flag.String("render-battery", "", "-render-png で、電池のウィジェットに出す状態（87、87,charging、87,plugged、87,charger-off（USB 給電で充電していない）。省略すると読めないとき）")
	pngGadgets := flag.String("render-widgets", "", "-render-png で、ストップウォッチ、ポモドーロ、カウンター、在席ランプの状態（widgets.json の形のファイル）")
	pngCalc := flag.String("render-calc", "", "-render-png で、電卓のセルに出す状態（{\"<id>\": CalcState} の JSON。get_widgets の calculator と同じ形）")
	pngCalcPressed := flag.String("render-calc-pressed", "", "-render-png で、電卓で押しているボタン（行,列。例: 4,4 は =）")
	pngGraph := flag.String("render-graph", "", "-render-png で、グラフのセルに出す値（graph.json の形のファイル）")
	pngRotate := flag.Int("render-rotate", -1, "-render-png で、画面の向き（0、90、180、270。-render-size は物理の大きさ）。省略すると設定の display.rotate")
	pngHomePage := flag.Int("render-home-page", 1, "-render-png で、ホーム画面に出すページ（1 から）")
	pngLink := flag.String("render-link", "configured", "-render-png で、PC とのつながり（UDC の state か power。configured、suspended、power、not attached、空なら出さない）。configured と suspended のほかでは、PC に送るセルを休みと描く")
	pngLastSync := flag.String("render-last-sync", "", "-render-png で、状態の帯に出す最後に同期した時刻（RFC 3339。省略すると未同期）")
	pngRead := flag.String("render-read", "", "-render-png で、読み物のアプリのセルに、この読み物（brain-deck read add -o の .json.gz）を開いて出す")
	pngReadPage := flag.Int("render-read-page", 1, "-render-read で出すページ（1 から）")
	pngFiles := flag.String("render-files", "", "-render-png で、widget: files にこのフォルダの一覧を出す（「/」なら場所の一覧。ファイルなら中を見ている画面）")
	pngFilesSize := flag.Bool("render-files-by-size", false, "-render-files を大きい順に")
	pngReadSize := flag.Int("render-read-size", 24, "-render-read の文字の大きさ（24 か 32）")
	pngSettings := flag.String("render-settings", "", "-render-png で、widget: settings に設定のアプリのこのタブを描く（settings、settings2（設定のタブの 2 ページ目）、info、power）")
	pngSettingsChoice := flag.String("render-settings-choice", "", "-render-settings で、この行の選ぶ画面を出す（level、dim、dim_offline、photo、rotate、reader、lid、tablet、ime）")
	pngHelp := flag.Int("render-help", 0, "-render-png で、widget: help にヘルプのこのページ（1 から）を描く。-render-read-size で文字の大きさ")
	pngReadH := flag.Bool("render-read-horizontal", false, "-render-read を横書きで出す")
	pngMemo := flag.String("render-memo", "", "-render-png で、手書きメモのセルに出すメモ（memo の <id>.json の形のファイル。最後のページを出す）")
	pngPhoto := flag.String("render-photo", "", "-render-png で、格子の代わりに、この写真（.565）をフォトフレームと同じく描く（向きが違えば縮めて帯を入れる）")
	pngQR := flag.String("render-qr", "", "-render-png で、QR コードのウィジェットに出す中身（qr.json の形のファイル）")
	recOut := flag.String("record-touch", "", "タッチパネルの生のイベントをこのファイルに記録して終わる（lefthand.service を止めてから使う）")
	recGesture := flag.String("record-gesture", "", "-record-touch で、見出しに書く動きの名前（slow、tap など）")
	recNote := flag.String("record-note", "", "-record-touch で、見出しに書くメモ")
	recLayer := flag.String("record-layer", "", "-record-touch で、画面に出すレイヤー（省略するとトラックパッドのある最初のレイヤー）")
	recFor := flag.Duration("record-for", time.Minute, "-record-touch で、記録する時間（最大 10m）")
	replay := flag.Bool("replay-touch", false, "引数の記録（.touch）を再生し、トラックパッドの判定の結果を書いて終わる（実機不要）")
	replayCfg := flag.String("replay-config", "", "-replay-touch で、トラックパッドの設定を読む設定ファイル（省略すると既定値）")
	replayParams := flag.String("replay-params", "", `-replay-touch で、上書きするトラックパッドの項目（JSON。例: {"speed":1.5,"smooth":4}）`)
	replayOps := flag.Bool("replay-ops", false, "-replay-touch で、送るマウスの操作を 1 つずつ書く")
	replayPenF := flag.Bool("replay-pen", false, "引数の記録（.touch）を、ペンタブレットと手書きメモの線の読み取りに通し、結果を書いて終わる（実機不要）。-replay-params で項目を変える")
	replayPNG := flag.String("replay-png", "", "-replay-pen で、最後の記録から読み取った線を描いた PNG を書く")
	showVersion := flag.Bool("version", false, "版を JSON で書いて終わる（brain-deck update が、新しい lefthand を確かめるのに使う）")
	checkGadget := flag.String("check-gadget", "", "この gadget-setup.sh のディスクリプタが、この lefthand のものと同じかを確かめて終わる")
	updateApply := flag.String("update-apply", "", "更新・再起動の係として、plan.json のとおりに入れ替えて終わる（デーモンが systemd-run で起動する）")
	updateBin := flag.String("update-bin", "/usr/local/bin/lefthand", "brain-deck update で入れ替える lefthand の場所")
	updateGadget := flag.String("update-gadget", defaultGadgetSetup, "brain-deck update で入れ替える gadget-setup.sh の場所")
	readyFile := flag.String("ready-file", defaultReadyFile, "起動を終えたことを書くファイル（更新の係が、新しい版が動いたことを確かめる）。空なら書かない")
	unitName := flag.String("unit", "", "自分の systemd のサービスの名前（省略すると /proc/self/cgroup から読む。再起動と更新に使う）")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-v] [-calibrate] [-check] [-dump-json] [-restore-console] [-render-png out.png] [config.yaml]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	switch {
	case *showVersion:
		printVersion()
		return
	case *checkGadget != "":
		if err := checkGadgetScript(*checkGadget); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok: the descriptors in", *checkGadget, "match", daemonVersion())
		return
	case *updateApply != "":
		os.Exit(runUpdateHelper(*updateApply))
	}
	if *restore {
		restoreConsole()
		restoreBacklight()
		restoreTerminal(*gadgetSetup)
		return
	}
	if *replayPenF {
		if err := runReplayPen(os.Stdout, flag.Args(), *replayParams, *replayPNG); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *replay {
		if err := runReplay(flag.Args(), *replayCfg, *replayParams, *replayOps); err != nil {
			log.Fatal(err)
		}
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
	iconsDir := filepath.Join(*dataDir, "weather-icons")
	if *pngIcons != "" {
		iconsDir = *pngIcons
	}
	switch {
	case *recOut != "":
		if *recFor <= 0 || *recFor > 10*time.Minute {
			log.Fatalf("-record-for must be 1s..10m")
		}
		err := runRecord(cfg, km, recordOptions{Out: *recOut, Gesture: *recGesture, Note: *recNote, Layer: *recLayer, For: *recFor})
		if err != nil {
			log.Fatal(err)
		}
		return
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
		env := WidgetEnv{Now: time.Now(), TimeSynced: !*pngUnsynced, MouseOff: *pngMouseOff, Link: *pngLink}
		offline := false
		if *pngLastSync != "" {
			t, err := time.Parse(time.RFC3339, *pngLastSync)
			if err != nil {
				log.Fatalf("bad -render-last-sync %q: %v", *pngLastSync, err)
			}
			env.LastSync = &t
		}
		if *pngRotate != -1 && !validRotate(*pngRotate) {
			log.Fatalf("bad -render-rotate %d (0, 90, 180 or 270)", *pngRotate)
		}

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
		if *pngBattery != "" {
			b, err := parseRenderBattery(*pngBattery)
			if err != nil {
				log.Fatalf("bad -render-battery %q: %v", *pngBattery, err)
			}
			env.Battery = b
		}
		if *pngGadgets != "" {
			b, err := os.ReadFile(*pngGadgets)
			if err != nil {
				log.Fatal(err)
			}
			if err := json.Unmarshal(b, &env.Gadgets); err != nil {
				log.Fatalf("%s: %v", *pngGadgets, err)
			}
		}
		if *pngQR != "" {
			b, err := os.ReadFile(*pngQR)
			if err != nil {
				log.Fatal(err)
			}
			var f textFile
			if err := json.Unmarshal(b, &f); err != nil {
				log.Fatalf("%s: %v", *pngQR, err)
			}
			env.QR = f.Texts
		}
		if *pngCalc != "" {
			b, err := os.ReadFile(*pngCalc)
			if err != nil {
				log.Fatal(err)
			}
			if err := json.Unmarshal(b, &env.Calc); err != nil {
				log.Fatalf("%s: %v", *pngCalc, err)
			}
		}
		if *pngCalcPressed != "" {
			var r, c int
			if _, err := fmt.Sscanf(*pngCalcPressed, "%d,%d", &r, &c); err != nil || r < 0 || r >= calcRows || c < 0 || c >= calcCols {
				log.Fatalf("bad -render-calc-pressed %q (row,col)", *pngCalcPressed)
			}
			for _, l := range km.Layers {
				if l.Grid == nil {
					continue
				}
				for _, a := range l.Grid.Cells {
					if a.Widget != nil && a.Widget.Kind == widgetCalc {
						a.Widget.calc.pressed = r*calcCols + c
					}
				}
			}
		}
		if *pngGraph != "" {
			b, err := os.ReadFile(*pngGraph)
			if err != nil {
				log.Fatal(err)
			}
			var f graphFile
			if err := json.Unmarshal(b, &f); err != nil {
				log.Fatalf("%s: %v", *pngGraph, err)
			}
			env.Graph = f.Series
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
		if *pngWeather != "" {
			b, err := os.ReadFile(*pngWeather)
			if err != nil {
				log.Fatal(err)
			}
			var d WeatherData
			if err := json.Unmarshal(b, &d); err != nil {
				log.Fatalf("%s: %v", *pngWeather, err)
			}
			env.Weather = &d
		}
		env.WeatherIcons = NewIconStore(iconsDir)
		if *pngRead != "" {
			dir, err := os.MkdirTemp("", "lefthand-read")
			if err != nil {
				log.Fatal(err)
			}
			defer os.RemoveAll(dir)
			b, err := os.ReadFile(*pngRead)
			if err == nil {
				err = os.WriteFile(filepath.Join(dir, "0000000000000001.json.gz"), b, 0o644)
			}
			if err != nil {
				log.Fatal(err)
			}
			st := OpenReadStore(dir)
			doc, err := st.Load("0000000000000001")
			if err != nil {
				log.Fatal(err)
			}
			st.idx["0000000000000001"] = readMeta{Title: doc.Title, Author: doc.Author}
			rs := &ReaderService{store: st, mode: readRead, docID: "0000000000000001", doc: doc, size: *pngReadSize, vertical: !*pngReadH}
			// 寸法を決めて、ページを数え終えてから、指定のページを出す
			area := renderReaderArea(cfg, km, *pngLayer, w0(*pngSize), rotOf(cfg, *pngRotate))
			rs.mu.Lock()
			m := rs.metricsLocked(area)
			rs.mu.Unlock()
			t0 := time.Now()
			starts := paginate(doc, m)
			log.Printf("render: %q: %d pages in %v", doc.Title, len(starts), time.Since(t0))
			rs.mu.Lock()
			rs.gen++ // 数えている goroutine の結果は使わない
			rs.starts, rs.counting = starts, false
			rs.pos = starts[min(max(*pngReadPage-1, 0), len(starts)-1)]
			rs.mu.Unlock()
			env.Reader = rs.Snapshot()
		}
		if *pngMemo != "" {
			dir, err := os.MkdirTemp("", "lefthand-memo")
			if err != nil {
				log.Fatal(err)
			}
			b, err := os.ReadFile(*pngMemo)
			if err == nil {
				err = os.WriteFile(filepath.Join(dir, "m1.json"), b, 0o644)
			}
			if err != nil {
				log.Fatal(err)
			}
			env.Memo = OpenMemoService(dir).Snapshot()
			os.RemoveAll(dir)
		}
		if *pngFiles != "" {
			svc := NewFilesService(*dataDir, nil, nil, nil)
			svc.SetConfig(cfg.Files)
			area := renderWidgetArea(cfg, km, *pngLayer, w0(*pngSize), rotOf(cfg, *pngRotate), widgetFiles)
			v, err := renderFilesView(svc, *pngFiles, *pngFilesSize, area)
			if err != nil {
				log.Fatal(err)
			}
			env.Files = v
		}
		if *pngSettings != "" {
			v, err := renderSettingsView(cfg, *pngSettings, *pngSettingsChoice, rotOf(cfg, *pngRotate), *pngReadSize, env)
			if err != nil {
				log.Fatal(err)
			}
			env.Settings = v
		}
		if *pngHelp > 0 {
			area := renderWidgetArea(cfg, km, *pngLayer, w0(*pngSize), rotOf(cfg, *pngRotate), widgetHelp)
			env.Help = renderHelpView(area, *pngHelp, *pngReadSize)
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
		if *pngMonthOffset != 0 || *pngMonthSelect != "" {
			if err := monthRenderState(km, *pngMonthOffset, *pngMonthSelect, env.Now); err != nil {
				log.Fatal(err)
			}
		}
		images := NewImageStore(imagesDir)
		w2, h2 := screenSize(cfg)
		for _, p := range imageProblems(km, images, w2, h2) {
			log.Printf("warning: %s  [%s]", p.Message, p.Path)
		}
		rot := cfg.Display.Rotate
		if !linkOnline(*pngLink) {
			offline = true
		}
		if *pngRotate != -1 {
			rot = *pngRotate
		}
		if *pngPhoto != "" {
			if err := renderPhotoPNG(*pngPhoto, *pngOut, w, h, rot); err != nil {
				log.Fatal(err)
			}
			return
		}
		if err := renderPNG(cfg, km, *pngOut, *pngLayer, *pngPressed, *pngPress, w, h, rot, *pngHomePage-1, offline, env, images); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Brain の設定のアプリで変えた上書きの値（overrides.go）を、設定ファイルの値に重ねて使う
	overrides := OpenOverrides(*dataDir)
	fileCfg := cfg
	// PC ごとの設定（profile.go）：前に名乗った PC か、手で選んだプロファイルを重ねる
	profiles := NewProfileService(*dataDir)
	base := fileCfg
	if name := profiles.Resolve(fileCfg); name != "" {
		c, err := applyProfile(fileCfg, name)
		var k *Keymap
		if err == nil {
			k, _, err = compileKeymap(c)
		}
		if err != nil {
			log.Printf("profile: %q: %v (using the config without the profile)", name, err)
		} else {
			base, cfg, km = c, c, k
			log.Printf("profile: using %q", name)
		}
	}
	if vals := overrides.Values(); len(vals) > 0 {
		eff, infos := applyOverrides(base, vals)
		if k, _, err := compileKeymap(eff); err != nil {
			log.Printf("overrides: %v (using the config file without overrides)", err)
		} else {
			cfg, km = eff, k
		}
		for _, in := range infos {
			if !in.Used {
				log.Printf("overrides: %s = %v is not used: %s", in.Key, in.Value, in.Note)
			}
		}
	}

	// CPU が 1 つでも、描画中に入力の goroutine がすぐ動けるようにする
	// （描画スレッドは nice 10 に下げてあるので、OS が入力側を優先する）
	if runtime.GOMAXPROCS(0) < 2 {
		runtime.GOMAXPROCS(2)
	}

	// 時刻：起動したら、なるべく早く内蔵 RTC か最後に分かっていた時刻で合わせる（rtc.go）
	store := OpenStore(*dataDir)
	clock := NewTimeService(store)
	clock.BootFromRTC(cfg.Clock.rtcMode())

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

	// 前に端末モードのまま終わっていたら、USB の構成の名前と Brain の getty を戻す（/dev/hidg0 を開く前に）
	restoreTerminal(*gadgetSetup)

	hid := NewHIDWriter(cfg.HIDDevice)
	hostWaker = NewWaker() // スリープ中の PC をキーで起こす
	hl := detectHID(cfg.HIDDevice)
	log.Printf("hid: %s", hl.Source)
	s := &State{hid: hid, active: map[string]Combo{}, kbdID: hl.KeyboardID, mouse: NewMouse(hid, hl.MouseID), media: NewMedia(hid, hl.MediaID), pen: NewPen(hid, hl.PenID)}
	if hl.MouseID == 0 && km.usesMouse() {
		log.Printf("mouse: the config uses the mouse (mouse: or widget: trackpad), but it is off (boot keyboard); turn it on with usb_mode, brain-deck usb-mode mouse, or HID_MOUSE=1 in /etc/lefthand/gadget.env")
	}
	logMediaOff(km, hl)
	e := NewEngine(km, s)
	pad := NewPad(s.mouse, true)
	e.SetPad(pad)
	usb := NewUSBMode(s, hid, cfg.HIDDevice, *gadgetSetup)
	usb.OnChange(pad.Cancel)
	e.SetUSB(usb)
	var curCfg atomic.Pointer[Config] // 今の設定（端末モードに入るときに terminal を読む）
	curCfg.Store(cfg)
	term := NewTermMode(curCfg.Load, usb, s, *gadgetSetup)
	e.SetTerm(term)
	// 日本語入力（ime.go）。文字を入れる欄と、Brain のシェルで uim-fep を通す
	ime := NewIME(*dataDir, curCfg.Load)
	term.ime = ime
	addAtExit(ime.Stop)
	if !clock.Synced() {
		log.Printf("time: not set since boot (the clock widget shows %q until the settings GUI connects)", unsyncedText)
	}
	go clock.KeepKnown()
	addAtExit(clock.SaveKnown)
	addAtExit(clock.Wait) // 時刻を合わせた直後に止めるときも、clock.json を書き終える
	texts := NewTextService(store)
	qrs := NewQRService(store)
	todos := NewTodoService(store)
	addAtExit(todos.Flush) // まだ書いていない変更を書く（電池が少なくて電源を切るときも、ここを通る）
	cals := NewCalendarService(store)
	weather := NewWeatherService(store)
	graphs := NewGraphService(store)
	calc := NewCalcService(store)
	addAtExit(graphs.Flush) // 間引いて、まだ書いていない値を書く
	icons := OpenIconStore(iconsDir)
	gadgets := NewGadgetService(store)
	addAtExit(gadgets.Shutdown)
	battery := NewBatteryService("/sys")
	dimP, _ := cfg.Backlight.compile() // 検証済み
	dim := NewDimmer(dimP)
	addAtExit(dim.Close)
	lid := NewLid(dim, cfg.Lid)
	batP, _ := cfg.Battery.compile() // 検証済み
	battery.SetCurve(batP.curve)
	guard := NewBatteryGuard(batP, *dataDir)
	guard.wake = dim.Wake
	guard.backlight = func() int {
		if in := dim.Info(); in.Enabled {
			return in.Level
		}
		return currentBacklight() // 自動減光を使っていなければ、今の明るさを読む
	}
	var powerOff func() error // 電池が少ないときに電源を切る（更新の係ができたら決める）
	guard.poweroff = func() error {
		if powerOff == nil {
			return errors.New("not running as a systemd service")
		}
		return powerOff()
	}
	battery.OnPoll(guard.Sample)
	if in := battery.Info(); in != nil {
		guard.Sample(*in) // 起動したときの 1 行（記録の「start」）
	}
	userActivity = guard.Activity
	prompt := NewPrompt(nil)
	prompt.SetIME(ime)
	e.SetInput(prompt)
	e.SetNotify(func(msg string) { prompt.Notify(msg, noticeWarn) })
	// タッチのキャリブレーション（touchcal.go、calibui.go）。保存したものがあれば、設定の min と max より優先する
	if f, err := loadTouchCal(*dataDir); err != nil {
		log.Printf("touch: calibration: %v (using touch.min_x etc. in the config)", err)
	} else if f != nil {
		log.Printf("touch: using the calibration from %s (%d points, %s)", f.Created.Format(time.DateTime), len(f.Points), f.After)
	}
	calib := NewCalibUI(*dataDir)
	calib.tc = func() *TouchConfig { return curCfg.Load().Touch } // e.mu は使わない（エンジンは e.mu を取ったまま calib を呼ぶので）
	calib.notify = func(msg string, level int) { prompt.Notify(msg, level) }
	calib.wake = dim.Wake
	e.calib = calib
	guard.notify, guard.clear = prompt.NotifyFor, prompt.ClearNotice
	gadgets.SetOnAlert(dim.Wake)
	gadgets.SetOnBusy(dim.SetBusy) // プレゼン用タイマーが動いているあいだは暗くしない
	photos := OpenPhotoStore(filepath.Join(*dataDir, "photos"))
	photoP, _ := cfg.Photo.compile() // 検証済み
	frame := NewPhotoFrame(photos, photoP)
	photos.SetMax(photoP.max)
	frame.SetSize(func() (int, int) { return rotatedSize(e.Rotation(), 800, 480) })
	// 時刻が合っていない（再起動の直後など）、プレゼン用タイマーかポモドーロが動いているあいだは、写真を出さない
	dim.SetPhotoFrame(frame, func() bool { return clock.Synced() && !gadgets.TimerRunning() })
	images := OpenImageStore(imagesDir)
	if ps := imageProblems(km, images, 800, 480); len(ps) > 0 {
		for _, p := range ps {
			log.Printf("warning: %s", p.Message)
		}
	}
	link := NewLinkMonitor("/sys")
	setOnline := func(on bool) {
		e.SetOffline(!on)
		dim.SetOffline(!on)
		term.LinkChanged(on)     // PC の端末の途中で外れたら、画面に出す（Brain のシェルはそのまま）
		profiles.LinkChanged(on) // つなぎ直したら、PC が名乗るのを待つ（名乗らなければ既定に戻す）
	}
	term.SetOnline(link.Online)
	link.OnOnline(setOnline)
	if !link.Online() {
		setOnline(false)
	}
	rotStore := NewRotateStore(store)
	e.SetRotation(rotStore.Initial(km.Rotate), 800, 480, rotStore)
	widgetRT := NewWidgetRT(todos)
	widgetRT.gadgets = gadgets
	widgetRT.calc = calc
	widgetRT.pen = s.pen
	memos := OpenMemoService(filepath.Join(*dataDir, "memo"))
	addAtExit(memos.Flush) // まだ書いていない線を書く
	widgetRT.memos = memos
	reads := OpenReadStore(filepath.Join(*dataDir, "reader"))
	addAtExit(reads.FlushState) // しおり
	reader := NewReaderService(reads)
	syncs := NewSyncService(store)
	widgetRT.reader = reader
	files := NewFilesService(*dataDir, photos, memos, reads)
	files.SetConfig(cfg.Files)
	filesUI := NewFilesUI(files)
	filesUI.photos, filesUI.images, filesUI.reads, filesUI.added = photos, images, reads, reader.Added
	filesUI.SetNotify(func(msg string) { prompt.Notify(msg, noticeWarn) })
	widgetRT.files = filesUI
	files.SetChanged(filesUI.reload)
	helpUI := NewHelpUI(reader.Size, reader.SetSize)
	widgetRT.help = helpUI
	// 設定のアプリ。設定の置き場所（cstore）と電源（更新の係）は、あとで決める
	settingsUI := NewSettingsUI(SettingsDeps{
		Rotation:      e.Rotation,
		Rotate:        func(r int) { dim.Wake(); e.Rotate(r) },
		ReaderSize:    reader.Size,
		SetReaderSize: reader.SetSize,
		OpenClock:     func() { prompt.Open(inputClock) },
		BrightLevel: func() int {
			if in := dim.Info(); in.Enabled && in.State == dimNames[dimBright] {
				return in.Level
			}
			if v := currentBacklight(); v > 0 {
				return v
			}
			return 7
		},
		Lid: lid,
		IME: ime.Info,
	})
	widgetRT.settings = settingsUI
	memos.SetNotify(func(msg string) { prompt.Notify(msg, noticeWarn) })
	reader.SetNotify(func(msg string) { prompt.Notify(msg, noticeWarn) })
	syncs.SetNotify(func(msg string, ok bool) {
		if ok {
			prompt.Notify(msg, noticeInfo)
		} else {
			prompt.Notify(msg, noticeWarn)
		}
	})
	e.SetWidgets(widgetRT)
	widgetEnv := func() WidgetEnv {
		env := clock.Env()
		env.Texts = texts.Snapshot()
		env.QR = qrs.Snapshot()
		env.Todo = todos.Snapshot()
		env.Calendar = cals.Snapshot()
		env.Weather = weather.Snapshot()
		env.WeatherIcons = icons
		env.Graph = graphs.Snapshot()
		env.Calc = calc.Snapshot()
		env.Memo = memos.Snapshot()
		env.Reader = reader.Snapshot()
		env.Files = filesUI.Snapshot()
		env.Settings = settingsUI.Snapshot()
		env.Help = helpUI.Snapshot()
		env.MouseOff = !s.mouse.Available()
		env.Gadgets = gadgets.Snapshot()
		env.Battery = battery.Info()
		env.Link = link.State()
		if ti := clock.Info(); ti.LastPCSet != nil {
			env.LastSync = ti.LastPCSet
		}
		// brain-deck sync の結果があれば、そちらを「同期」に出す（時刻だけを合わせたときより新しければ、そちら）
		running, last, failed := syncs.envFields()
		env.Syncing = running
		if last != nil && (env.LastSync == nil || !last.Before(*env.LastSync)) {
			env.LastSync, env.SyncFailed = last, failed
		}
		return env
	}
	widgetRT.SetEnv(widgetEnv)
	calc.SetTyper(func(text string) error {
		if !link.Online() {
			return errOffline
		}
		return typeOnHID(s, text)
	})
	prompt.SetHandlers(func(kind, text string) (string, error) {
		switch kind {
		case inputTodo:
			if strings.TrimSpace(text) == "" {
				return "", inputError("何も入っていません")
			}
			it, _, err := todos.Add(text, 0, time.Now(), sourceBrain)
			if err != nil {
				return "", inputError(err.Error())
			}
			return "Todo に足しました：" + it.Text, nil
		case inputClock:
			t, err := parseClockInput(text, time.Local)
			if err != nil {
				return "", err
			}
			res, err := clock.Set(t, sourceBrain)
			if err != nil {
				return "", inputError(err.Error())
			}
			afterClockSet(res, texts, qrs, gadgets, graphs)
			return "時刻を合わせました：" + t.Format(clockInputForm), nil
		}
		return "", nil
	}, func(kind string) string {
		if kind == inputClock {
			return clock.Estimate().Format(clockInputForm)
		}
		return ""
	})

	// 終了時に押しっぱなしを防ぐ
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		got := <-sig
		log.Printf("%v received, releasing all keys", got)
		s.shutdown(0)
	}()
	go s.retryLoop()
	addAtExit(term.Shutdown) // 端末モードなら抜けてから（画面を戻す前に）

	// 起動直後の状態を揃える（前回異常終了したときの押しっぱなしも解除される）
	s.releaseAll()

	if err := kbd.Grab(); err != nil {
		log.Printf("grab keyboard: %v", err)
	}
	lid.Init(kbd) // デーモンを起動し直したときに、ふたが閉じていれば
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
				calib.show = d.SetFullScreen
				term.SetDisplay(d)
				frame.SetDisplay(d)
				clock.SetOnChange(d.Poke)
				texts.SetOnChange(d.Poke)
				qrs.SetOnChange(d.Poke)
				gadgets.SetOnChange(d.Poke)
				battery.SetOnChange(d.Poke)
				todos.SetOnChange(d.Poke)
				cals.SetOnChange(d.Poke)
				weather.SetOnChange(d.Poke)
				graphs.SetOnChange(d.Poke)
				calc.SetOnChange(d.Poke)
				link.SetOnChange(d.Poke)
				prompt.SetWidth(first.W)
				e.SetOnRotate(func(r int) { // 向きが変わったら、欄の桁数も変える（表示が変わる前に呼ばれるので、向きから決める）
					pw, ph := d.PhysSize()
					if r == 90 || r == 270 {
						pw = ph
					}
					prompt.SetWidth(pw)
				})
				prompt.SetShow(d.SetOverlay)
				if pw, ph := d.PhysSize(); pw != 800 || ph != 480 {
					e.SetRotation(e.Rotation(), pw, ph, rotStore)
				}
				icons.SetOnChange(d.Poke)
				images.SetOnChange(d.Invalidate)
				usb.OnChange(d.Invalidate) // トラックパッドの「マウスはオフ」と、メディアキーのセルを描き直す
				images.Preload(preloadOrder(cfg))
				widgetRT.SetScreen(first.W, first.H, d.Poke)
				widgetRT.SetInk(d.Ink)
				memos.SetOnChange(d.Poke)
				reader.SetOnChange(d.Poke)
				settingsUI.SetOnChange(d.Poke)
				helpUI.SetOnChange(d.Poke)
				lid.SetOnChange(d.Poke)
				lid.SetDisplay(d) // 閉じているあいだは描き直さない
				syncs.SetOnChange(d.Poke)
				// レイヤーが変わったり、設定を差し替えたりしたら描き直す。SetLayout は待たずに返る
				e.SetOnView(func(v *View) { d.SetLayout(buildLayout(v.km, v)) }, first.Gen)
			}
		}
	}

	// 設定 GUI（USB シリアル）。通信が止まっても、入力の処理は待たない
	mon := &Monitor{}
	e.SetOnStatus(mon.LayerChanged)
	term.OnChange(func() {
		mon.TerminalChanged(term.Info())
		dim.SetTerminal(term.Active())
	})
	// 設定の置き場所。設定 GUI と brain-deck（シリアル）と、Brain の設定のアプリから使う
	cstore := &configStore{path: cfgPath, file: fileCfg, cfg: cfg, km: km, ov: overrides, prof: profiles, profName: cfg.profile, apply: func(c *Config, k *Keymap) error {
		if err := reloader(e)(c, k); err != nil {
			return err
		}
		curCfg.Store(c)
		images.Preload(preloadOrder(c)) // 新しい設定で使う画像を先に読む
		p, _ := c.Backlight.compile()   // 検証済み
		dim.Apply(p)
		lid.SetConfig(c.Lid)
		bp, _ := c.Battery.compile() // 検証済み
		guard.Apply(bp)
		battery.SetCurve(bp.curve)
		clock.SetRTCMode(c.Clock.rtcMode())
		pp, _ := c.Photo.compile() // 検証済み
		frame.SetParams(pp)
		files.SetConfig(c.Files)
		go filesUI.reload()
		ime.ConfigChanged()
		settingsUI.Changed()
		return nil
	}, light: func(c *Config) {
		// 上書きだけが変わった：上書きで変わる項目（自動減光、ふた、日本語入力）だけを反映する（割り当ては変わらない）
		curCfg.Store(c)
		p, _ := c.Backlight.compile() // applyOverrides が確かめてある
		dim.Apply(p)
		lid.SetConfig(c.Lid)
		ime.ConfigChanged()
	}, onOverrides: settingsUI.Changed}
	profiles.SetOnChange(cstore.Reprofile)
	// バックアップと復元（backup.go）。復元は更新の係が行う（あとで決める）
	backups := NewBackupService(*dataDir, cfgPath)
	backups.store = cstore
	backups.flush = func() { todos.Flush(); memos.Flush(); reads.FlushState() }
	backups.anthy = func() string { return anthyDirOf(ime.userName()) }
	backups.profile = cstore.Profile
	backups.stopIME = ime.Stop
	settingsUI.SetDeps(func(d *SettingsDeps) {
		d.Store = cstore
		d.Profile = func() ProfileInfo {
			in := profiles.Info(cstore.File())
			in.SetActive(cstore.Profile())
			return in
		}
		d.UseProfile = func(name string) error { return profiles.Use(name, cstore.File()) }
		d.Info = func() []SettingsInfo {
			in := settingsInfoIn{Version: daemonVersion(), Battery: battery.Info(), Link: link.State(), Synced: clock.Synced(),
				Lid: lid.Info(), Overrides: cstore.Overrides(), LastSync: syncs.Info().Last, LastPCSet: clock.Info().LastPCSet}
			ii := ime.Info()
			in.IME = &ii
			pi := profiles.Info(cstore.File())
			pi.SetActive(cstore.Profile())
			in.Profile = &pi
			in.Free, in.Size, in.DiskErr = statfsFree(*dataDir)
			return in.rows()
		}
	})
	if *serialPath != "" {
		ctl := &Controller{
			store:   cstore,
			backup:  backups,
			profile: profiles,
			lid:     lid,
			ime:     ime,
			engine:  e,
			monitor: mon,
			clock:   clock,
			texts:   texts,
			qrs:     qrs,
			gadgets: gadgets,
			battery: battery,
			dim:     dim,
			todos:   todos,
			cals:    cals,
			weather: weather,
			graphs:  graphs,
			calc:    calc,
			icons:   icons,
			images:  images,
			usb:     usb,
			term:    term,
			link:    link,
			guard:   guard,
			photos:  photos,
			frame:   frame,
			memos:   memos,
			reads:   reads,
			sync:    syncs,
			reader:  reader,
			files:   files,
			calib:   calib,
			dataDir: *dataDir,
			started: time.Now(),
		}
		upd := NewUpdater(*dataDir, *updateBin, *updateGadget, *unitName, cfgPath, imagesDir, *readyFile, usb, term)
		ctl.updater = upd
		backups.updater = upd
		if upd.Unit != "" {
			powerOff = func() error { return upd.Power("poweroff") }
			settingsUI.SetDeps(func(d *SettingsDeps) { d.Power = upd.Power })
		}
		ctl.uploads = NewUploads()
		ctl.uploads.Register("update", upd.UploadKind())
		ctl.uploads.Register("image", images.UploadKind())
		ctl.uploads.Register("photo", photos.UploadKind())
		rk := reads.UploadKind()
		done := rk.Done
		rk.Done = func(b UploadBegin, f *os.File, part string) (any, error) {
			res, err := done(b, f, part)
			if err == nil {
				reader.Added()
			}
			return res, err
		}
		ctl.uploads.Register("read", rk)
		ctl.uploads.Register("file", files.UploadKind(filepath.Join(*dataDir, "files-tmp")))
		ctl.uploads.Register("backup", backups.UploadKind())
		ctl.uploads.Register(iconUploadKnd, icons.UploadKind())
		icons.SetWanted(ctl.iconBoxes) // 上限を超えたら、今の設定で使わない大きさのアイコンから消す
		if upd.Unit == "" {
			log.Printf("update: not running as a systemd service; brain-deck update and restart are unavailable")
		}
		todos.SetOnNotify(mon.TodoChanged)
		gadgets.SetOnNotify(mon.WidgetsChanged)
		go ctl.runSerial(*serialPath)
	}

	if touch != nil {
		go runTouch(touch, e, disp, mon, dim)
	}
	writeReady(*readyFile) // 更新の係は、これで新しい版が起動したことを知る
	runKeyboard(kbd, e, mon, dim, lid)
}

// w0 は、-render-size の大きさ。
func w0(size string) image.Point {
	var w, h int
	fmt.Sscanf(size, "%dx%d", &w, &h)
	return image.Pt(w, h)
}

func rotOf(cfg *Config, r int) int {
	if r != -1 {
		return r
	}
	return cfg.Display.Rotate
}

// renderReaderArea は、-render-read で、レイヤーの読み物のアプリのセルの中の範囲（ページを数えるため）。
func renderReaderArea(cfg *Config, km *Keymap, layer string, size image.Point, rot int) image.Rectangle {
	return renderWidgetArea(cfg, km, layer, size, rot, widgetReader)
}

// renderWidgetArea は、-render-png のレイヤーで、kind のウィジェットを描く範囲。
func renderWidgetArea(cfg *Config, km *Keymap, layer string, size image.Point, rot int, kind string) image.Rectangle {
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	e.SetRotation(rot, size.X, size.Y, nil)
	name, _, _ := strings.Cut(layer, ":")
	for i, l := range km.Layers {
		if l.Name == name && i != 0 {
			e.mu.Lock()
			e.stack = []stackEntry{{layer: i, kind: actTo}}
			e.view = nil
			e.refresh()
			e.mu.Unlock()
		}
	}
	l := buildLayout(km, e.View())
	l.W, l.H = rotatedSize(rot, size.X, size.Y)
	for i, c := range l.Cells {
		if c.Widget != nil && c.Widget.Kind == kind && !c.Covered {
			inner := l.rect(i%l.Cols, i/l.Cols).Inset(cellGap + textMargin)
			_, _, area := widgetCaption(inner, c.Label)
			return area
		}
	}
	return image.Rect(0, 0, l.W, l.H)
}

// renderPhotoPNG は、写真を出している画面を PNG に書く（-render-photo）。
func renderPhotoPNG(in, out string, w, h, rot int) error {
	b, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	img, err := parseImage(b)
	if err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}
	cv := NewCanvas(w, h, w*rgb565.Bpp, rgb565, rot)
	full := image.Rect(0, 0, cv.W, cv.H)
	img = photoForScreen(img, cv.W, cv.H)
	cv.fill(full, colBG)
	cv.blitImage(img, centered(img, full).dst, full)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, cv.Image())
}

// runReplay は、記録を再生して結果を書く（-replay-touch）。
func runReplay(files []string, cfgPath, params string, ops bool) error {
	if len(files) == 0 {
		return errors.New("-replay-touch: give one or more .touch files")
	}
	var km *Keymap
	if cfgPath != "" {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			return err
		}
		if km, _, err = compileKeymap(cfg); err != nil {
			return err
		}
	}
	for _, f := range files {
		rec, err := LoadRecording(f)
		if err != nil {
			return err
		}
		p, err := padParamsFor(km, rec.Header.Layer, params)
		if err != nil {
			return err
		}
		printReplay(os.Stdout, f, rec, replayTouch(rec, p), ops)
	}
	return nil
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

// parseRenderBattery は -render-battery の値（87、87,charging、87,plugged、none）を読む。
func parseRenderBattery(s string) (*BatteryInfo, error) {
	if s == "none" {
		return nil, nil
	}
	pct, rest, _ := strings.Cut(s, ",")
	n, err := strconv.Atoi(pct)
	if err != nil || n < 0 || n > 100 {
		return nil, errors.New("percent must be 0..100")
	}
	b := &BatteryInfo{Percent: n, Source: "render"}
	switch rest {
	case "":
	case "plugged":
		b.Plugged = true
	case "charging":
		b.Plugged, b.Charging = true, true
	case "charger-off":
		b.Plugged, b.ChargerOff = true, true
	default:
		return nil, errors.New("use 87, 87,plugged, 87,charging or 87,charger-off")
	}
	return b, nil
}
