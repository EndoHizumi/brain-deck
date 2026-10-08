package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 端末モード ----------
//
// Brain の画面とキーボードを、PC のコンソールの端末にする。2 つのつなぎ方がある。
//
// シリアル（既定）：1 つ目の ACM（Brain の /dev/ttyGS0、PC の -if03）を逆向きに使い、PC の getty にログインする。
//   ふだんは Brain の getty（serial-getty@ttyGS0）が ttyGS0 でログイン画面を出している。両端の getty が同時に動くと、
//   互いのログイン画面をユーザー名として読み合い、ログインの失敗が続く。そこで次の順に切り替える。
//     入る：Brain の getty を止める → USB を付け直し、構成の名前を「NCM+HID+ACM+ACM terminal」にする
//           → PC の udev がその名前を見て、-if03 で getty を起動する（contrib/udev/71-brain-terminal.rules）
//           → lefthand が ttyGS0 を開く
//     抜ける：ttyGS0 を閉じる → USB を付け直し、構成の名前を元に戻す（PC の getty は、デバイスが消えて止まる。
//           ログインしていたシェルはハングアップで終わる）→ Brain の getty を起動し直す
//   ふだんの構成の名前では、PC は何も起動しないので、設定 GUI のコンソールのタブは今までどおり使える。
//   PC 側は、ACM の DCD（制御線）を当てにしない。このカーネルの f_acm は、ttyGS0 を開いたときに一度だけ
//   SERIAL_STATE を送り、PC がポートを開く前だと届かない（PC で調べると、Brain の getty が開いていても DCD は 0）。
//
// コマンド（terminal.command）：ssh などのコマンドを、Brain の上で疑似端末（PTY）につないで動かす。
//   getty も USB も触らない。端末の大きさは PTY でそのまま伝わる。
//
// 途中で lefthand が落ちたときは、/run/lefthand-terminal の印を見て、lefthand -restore-console（ExecStopPost）と
// 次の起動が、USB の構成の名前と Brain の getty を元に戻す。

const (
	termOff = iota
	termEntering
	termOn
	termLeaving
)

const (
	defaultTermPort   = "/dev/ttyGS0"
	defaultTermGetty  = "serial-getty@ttyGS0.service"
	defaultScrollback = 1000
	termNoReply       = 5 * time.Second // これより長く PC から何も届かなければ、そう出す
)

// termMarker は、端末モードに入ったことを残すファイル（落ちたときの後始末のため。テストで差し替える）。
var termMarker = "/run/lefthand-terminal"

// TerminalConfig は設定の terminal。どれも省略できる。端末モードに入るときに読む。
type TerminalConfig struct {
	Port       string   `yaml:"port,omitempty" json:"port,omitempty"`             // シリアル。既定 /dev/ttyGS0
	Getty      string   `yaml:"getty,omitempty" json:"getty,omitempty"`           // 端末モードのあいだ止める Brain の getty。none で止めない
	Command    []string `yaml:"command,omitempty" json:"command,omitempty"`       // シリアルの代わりに PTY で動かすコマンド（ssh など）
	User       string   `yaml:"user,omitempty" json:"user,omitempty"`             // command を動かすユーザー
	Font       string   `yaml:"font,omitempty" json:"font,omitempty"`             // wide（100 桁、既定）か narrow（200 桁）
	Scrollback int      `yaml:"scrollback,omitempty" json:"scrollback,omitempty"` // 履歴の行数。既定 1000
}

// termConn は、端末の相手（シリアルか PTY）。
type termConn interface {
	io.ReadWriteCloser
	Kind() string // 帯に出す名前
}

// TermMode は端末モードの状態と、入る・抜けるの手順。
type TermMode struct {
	mu sync.Mutex

	cfg    func() *Config
	usb    *USBMode
	out    *State
	disp   *Display
	script string // gadget-setup.sh（落ちたあとの後始末用）

	want     bool
	state    int
	active   atomic.Bool // 入力を端末に回すか（入る途中から、抜けると決めるまで）
	worker   bool
	onChange []func()

	tc       TerminalConfig // 入ったときの設定
	geom     termGeom
	term     *Term
	keys     termKeys
	page     int
	pressed  int // 押しているタッチのキー（-1 なし）
	conn     termConn
	gen      int        // conn を開き直すたびに増える
	wmu      sync.Mutex // writeCh を守る（端末の答えは m.mu を持ったまま送るので、別のロックにする）
	writeCh  chan []byte
	status   string
	level    int
	lastErr  string
	openedAt time.Time
	lastRx   time.Time
	gettyOn  bool // 止めた Brain の getty（抜けるときに起動し直す）

	// テストで差し替える
	systemctl func(args ...string) error
	openPort  func(path string) (termConn, error)
	startCmd  func(tc TerminalConfig, cols, rows int) (termConn, error)
}

// NewTermMode は端末モードを作る。cfg は今の設定を返す関数。
func NewTermMode(cfg func() *Config, usb *USBMode, out *State, script string) *TermMode {
	return &TermMode{cfg: cfg, usb: usb, out: out, script: script, pressed: -1,
		systemctl: runSystemctl, openPort: openTermSerial, startCmd: startPTY}
}

// SetDisplay は画面を設定する（画面がなければ呼ばない）。
func (m *TermMode) SetDisplay(d *Display) {
	m.mu.Lock()
	m.disp = d
	m.mu.Unlock()
}

// OnChange は、入った・抜けた・状態が変わったときに呼ぶ関数を足す（設定 GUI への知らせ）。
func (m *TermMode) OnChange(f func()) {
	m.mu.Lock()
	m.onChange = append(m.onChange, f)
	m.mu.Unlock()
}

// Active は、入力を端末に回すか。nil でもよい。
func (m *TermMode) Active() bool { return m != nil && m.active.Load() }

// TermInfo は設定 GUI と brain-deck に返す状態。
type TermInfo struct {
	Active    bool   `json:"active"`              // 端末モード（入る途中を含む）
	State     string `json:"state"`               // off、entering、on、leaving
	Transport string `json:"transport,omitempty"` // serial、command
	Status    string `json:"status,omitempty"`    // 帯に出している状態
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

var termStateNames = [...]string{termOff: "off", termEntering: "entering", termOn: "on", termLeaving: "leaving"}

func (m *TermMode) Info() TermInfo {
	if m == nil {
		return TermInfo{State: "off"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	in := TermInfo{Active: m.want || m.state != termOff, State: termStateNames[m.state]}
	if m.state != termOff {
		in.Transport = "serial"
		if len(m.tc.Command) > 0 {
			in.Transport = "command"
		}
		in.Status, in.Cols, in.Rows = m.status, m.geom.Cols, m.geom.Rows
	}
	return in
}

// Request は、mode（on、off、toggle）へ切り替える。待たずに返る。入るかどうか（切り替えたあと）を返す。
func (m *TermMode) Request(mode string) (bool, error) {
	if m == nil {
		return false, errors.New("the terminal mode is not available")
	}
	m.mu.Lock()
	var want bool
	switch mode {
	case "on":
		want = true
	case "off":
		want = false
	case "toggle":
		want = !(m.want)
	default:
		m.mu.Unlock()
		return false, fmt.Errorf(`mode must be "on", "off" or "toggle"`)
	}
	m.setWantLocked(want)
	m.mu.Unlock()
	return want, nil
}

func (m *TermMode) setWantLocked(want bool) {
	if want && !m.want && m.state == termOff {
		// 入る：押しているキーとマウスのボタンを離し、入力を端末に回す
		m.out.releaseAll()
		m.keys.reset()
		m.active.Store(true)
	}
	if !want {
		m.active.Store(false)
		m.pressed = -1
	}
	m.want = want
	if !m.worker {
		m.worker = true
		go m.run()
	}
}

// run は、want になるまで入る・抜けるを進める。
func (m *TermMode) run() {
	for {
		m.mu.Lock()
		want, state := m.want, m.state
		if (want && state == termOn) || (!want && state == termOff) {
			m.worker = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		if want {
			m.enter()
		} else {
			m.leave()
		}
	}
}

func (m *TermMode) notify() {
	m.mu.Lock()
	fs := append([]func(){}, m.onChange...)
	d := m.disp
	m.mu.Unlock()
	d.Poke()
	for _, f := range fs {
		f()
	}
}

func (m *TermMode) setStatus(s string, level int) {
	m.mu.Lock()
	m.status, m.level = s, level
	m.mu.Unlock()
	m.notify()
}

// enter は端末モードに入る。
func (m *TermMode) enter() {
	cfg := m.cfg()
	tc := TerminalConfig{}
	if cfg != nil && cfg.Terminal != nil {
		tc = *cfg.Terminal
	}
	tc = tc.withDefaults()
	W, H := 800, 480
	m.mu.Lock()
	if m.disp != nil {
		W, H = m.disp.Size()
	}
	m.tc = tc
	m.geom = termLayout(W, H, tc.Font)
	m.term = NewTerm(m.geom.Cols, m.geom.Rows, tc.Scrollback, m.send)
	m.state = termEntering
	m.page, m.pressed = 0, -1
	m.status, m.level, m.lastErr = "準備中…", 1, ""
	ch := make(chan []byte, 256)
	d := m.disp
	m.mu.Unlock()
	m.wmu.Lock()
	m.writeCh = ch
	m.wmu.Unlock()
	log.Printf("terminal: entering (%s)", tc.describe())
	if d != nil {
		d.SetTerminal(m)
	}
	m.notify()
	go m.writer(ch)

	if len(tc.Command) > 0 {
		m.setStatus("起動中…", 1)
		c, err := m.startCmd(tc, m.geom.Cols, m.geom.Rows)
		if err != nil {
			log.Printf("terminal: %v", err)
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
			m.feedNote("起動できません：" + err.Error())
			m.finishEnter(nil, "起動できません", 2)
			return
		}
		m.finishEnter(c, "", 0)
		return
	}

	// シリアル：USB を切り離しているあいだに Brain の getty を止め、構成の名前を変えて付け直してから開く。
	// 切り離しているあいだなら、getty が閉じるときに u_serial が送っていない出力を待たずに捨てる
	// （つながっていると最大 15 秒待ち、getty のログイン画面の残りが PC に届くことがある）
	m.gettyOn = false
	m.setStatus("USB を付け直しています…", 1)
	time.Sleep(usbSwitchDelay) // 設定 GUI や brain-deck への返事を送り終えてから
	// ttyGS0 も、切り離しているあいだに開いて生のモードにする。Brain の getty が残した設定（エコーあり）のまま
	// つながると、開いてから生のモードにするまでのあいだに届いた PC の getty のログイン画面を ttyGS0 が
	// 送り返し、PC の getty がそれをユーザー名として読んで、PC にログインの失敗が残る（実機で 4 回に 1 回起きた）。
	// u_serial は設定を開き直しても保つので、あとで開き直すとき（USB の付け直しのあと）は、初めから生のモード
	var c termConn
	var openErr error
	err := m.usb.SetTerminal(true, func() {
		if tc.Getty != "none" && m.systemctl("is-active", "--quiet", tc.Getty) == nil {
			if err := m.systemctl("stop", tc.Getty); err != nil {
				log.Printf("terminal: stop %s: %v", tc.Getty, err)
			}
			m.gettyOn = true
			writeTermMarker(tc.Getty, true)
		}
		for i := 0; i < 20; i++ {
			if c, openErr = m.openPort(tc.Port); openErr == nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	})
	if !m.gettyOn {
		writeTermMarker(tc.Getty, false)
	}
	if err != nil {
		log.Printf("terminal: %v", err)
		m.feedNote("USB の構成の名前を変えられませんでした（PC の getty が起動しないかもしれません）：" + err.Error())
	}
	if c == nil {
		if openErr == nil {
			openErr = errors.New("not opened")
		}
		log.Printf("terminal: open %s: %v", tc.Port, openErr)
		m.feedNote("開けません：" + openErr.Error())
		m.finishEnter(nil, "シリアルを開けません", 2)
		return
	}
	m.finishEnter(c, "", 0)
}

// finishEnter は、入る手順の終わり。c が nil なら、相手なしで端末モードのまま（HOME で抜ける）。
func (m *TermMode) finishEnter(c termConn, status string, level int) {
	m.mu.Lock()
	m.state = termOn
	if c != nil {
		m.conn = c
		m.gen++
		m.openedAt, m.lastRx = time.Now(), time.Time{}
		go m.reader(c, m.gen)
		go m.watch(m.gen)
		status, level = m.connStatusLocked()
	}
	m.status, m.level = status, level
	m.mu.Unlock()
	log.Printf("terminal: on (%s)", status)
	m.notify()
}

// leave は端末モードを抜ける。
func (m *TermMode) leave() {
	m.mu.Lock()
	m.state = termLeaving
	m.status, m.level = "抜けています…", 1
	c := m.conn
	m.conn = nil
	m.gen++
	tc := m.tc
	m.mu.Unlock()
	m.wmu.Lock()
	if m.writeCh != nil {
		close(m.writeCh)
		m.writeCh = nil
	}
	m.wmu.Unlock()
	m.notify()
	if len(tc.Command) > 0 {
		if c != nil {
			c.Close()
		}
	} else {
		// 切り離しているあいだに ttyGS0 を閉じる（送っていない出力を待たずに捨てる）。付け直してから Brain の getty を起動する
		time.Sleep(usbSwitchDelay)
		err := m.usb.SetTerminal(false, func() {
			if c != nil {
				c.Close()
			}
		})
		if err != nil {
			log.Printf("terminal: %v", err)
		}
		if m.gettyOn {
			if err := m.systemctl("start", tc.Getty); err != nil {
				log.Printf("terminal: start %s: %v", tc.Getty, err)
			}
		}
		os.Remove(termMarker)
	}
	m.mu.Lock()
	m.state = termOff
	m.keys.reset()
	m.status = ""
	d := m.disp
	m.mu.Unlock()
	if d != nil {
		d.SetTerminal(nil)
	}
	log.Printf("terminal: off")
	m.notify()
}

// Shutdown は、デーモンが終わるときに端末モードを抜ける（待つ）。
func (m *TermMode) Shutdown() {
	if m == nil {
		return
	}
	m.mu.Lock()
	on := m.want || m.state != termOff
	m.mu.Unlock()
	if !on {
		return
	}
	m.Request("off")
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		done := m.state == termOff && !m.worker
		m.mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	log.Printf("terminal: did not finish leaving; lefthand -restore-console will clean up")
}

// feedNote は、端末の画面に lefthand からの知らせを 1 行書く（PC の出力と区別できるよう色を変える）。
func (m *TermMode) feedNote(s string) {
	m.mu.Lock()
	if m.term != nil {
		m.term.Write([]byte("\r\n\x1b[0;33m[lefthand] " + s + "\x1b[0m\r\n"))
	}
	m.mu.Unlock()
	m.disp.Poke()
}

// ---------- 読み書き ----------

// send は PC に送る（キーと、端末の問い合わせへの答え）。待たない。
func (m *TermMode) send(b []byte) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if len(b) == 0 || m.writeCh == nil {
		return
	}
	select {
	case m.writeCh <- append([]byte(nil), b...):
	default: // PC が読んでいない。捨てる
	}
}

func (m *TermMode) writer(ch chan []byte) {
	for b := range ch {
		m.mu.Lock()
		c := m.conn
		m.mu.Unlock()
		if c == nil {
			continue
		}
		if _, err := c.Write(b); err != nil {
			vlogf("terminal: write: %v", err)
		}
	}
}

func (m *TermMode) reader(c termConn, gen int) {
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			m.mu.Lock()
			if m.gen != gen {
				m.mu.Unlock()
				return
			}
			m.term.Write(buf[:n])
			m.lastRx = time.Now()
			old := m.status
			m.status, m.level = m.rxStatusLocked()
			d := m.disp
			m.mu.Unlock()
			if old != m.status {
				m.notify()
			} else {
				d.Poke()
			}
		}
		if err != nil || n == 0 {
			m.closed(c, gen, err)
			return
		}
	}
}

// closed は、相手が切れたとき。シリアルなら開き直す（USB の付け直しやケーブルの抜き差しでハングアップする）。
func (m *TermMode) closed(c termConn, gen int, err error) {
	m.mu.Lock()
	if m.gen != gen {
		m.mu.Unlock()
		return
	}
	c.Close()
	m.conn = nil
	tc := m.tc
	m.mu.Unlock()
	if len(tc.Command) > 0 {
		code := ""
		if p, ok := c.(*ptyConn); ok {
			code = p.exitText()
		}
		m.feedNote("コマンドが終わりました" + code + "。決定（Enter）でもう一度動かす。HOME で抜ける")
		m.setStatus("コマンドが終わりました", 2)
		return
	}
	vlogf("terminal: %s closed (%v), reopening", tc.Port, err)
	m.setStatus("USB が切れました。つながるのを待っています…", 2)
	for {
		time.Sleep(500 * time.Millisecond)
		m.mu.Lock()
		stop := m.gen != gen || !m.want
		m.mu.Unlock()
		if stop {
			return
		}
		nc, err := m.openPort(tc.Port)
		if err != nil {
			continue
		}
		m.mu.Lock()
		if m.gen != gen || !m.want {
			m.mu.Unlock()
			nc.Close()
			return
		}
		m.conn = nc
		m.gen++
		m.openedAt, m.lastRx = time.Now(), time.Time{}
		go m.reader(nc, m.gen)
		go m.watch(m.gen)
		m.status, m.level = m.connStatusLocked()
		m.mu.Unlock()
		m.notify()
		return
	}
}

// restartCommand は、終わったコマンドをもう一度動かす（決定を押したとき）。
func (m *TermMode) restartCommand() {
	m.mu.Lock()
	if m.state != termOn || m.conn != nil || len(m.tc.Command) == 0 {
		m.mu.Unlock()
		return
	}
	tc := m.tc
	m.mu.Unlock()
	c, err := m.startCmd(tc, m.geom.Cols, m.geom.Rows)
	if err != nil {
		m.feedNote("起動できません：" + err.Error())
		return
	}
	m.mu.Lock()
	m.conn = c
	m.gen++
	m.openedAt, m.lastRx = time.Now(), time.Time{}
	go m.reader(c, m.gen)
	m.status, m.level = m.connStatusLocked()
	m.mu.Unlock()
	m.notify()
}

// watch は、PC から何も届かないまま時間がたったら、帯の表示を変える。
func (m *TermMode) watch(gen int) {
	time.Sleep(termNoReply)
	m.mu.Lock()
	if m.gen != gen {
		m.mu.Unlock()
		return
	}
	m.status, m.level = m.connStatusLocked()
	m.mu.Unlock()
	m.notify()
}

// connStatusLocked は、つないだあとの帯の表示。
func (m *TermMode) connStatusLocked() (string, int) {
	if len(m.tc.Command) > 0 {
		if m.conn == nil {
			return "コマンドが終わりました", 2
		}
		return "動作中", 3
	}
	if udcState() != "configured" {
		return "USB がつながっていません", 2
	}
	if m.lastRx.IsZero() {
		if time.Since(m.openedAt) >= termNoReply {
			return "PC から応答がありません（決定で再表示。PC の設定は README）", 2
		}
		return "PC の応答を待っています…", 1
	}
	return m.rxStatusLocked()
}

// rxStatusLocked は、PC から何か届いたあとの帯の表示。
func (m *TermMode) rxStatusLocked() (string, int) {
	if len(m.tc.Command) > 0 {
		return "動作中", 3
	}
	if loginPrompt(m.term) {
		return "PC のログイン画面", 3
	}
	return "PC とつながっています", 3
}

// loginPrompt は、カーソルの行が getty のログイン画面（「… login: 」か「Password: 」）か。
func loginPrompt(t *Term) bool {
	if t == nil {
		return false
	}
	if v, _ := t.View(); v > 0 {
		return false
	}
	_, y := t.Cursor()
	line := strings.TrimRight(t.LineText(y), " ")
	return strings.HasSuffix(line, " login:") || strings.HasSuffix(line, "Password:")
}

func udcState() string {
	b, err := os.ReadFile("/sys/class/udc/ci_hdrc.0/state")
	if err != nil {
		return "configured" // 分からないときは、つながっているとみなす（PC でのテスト）
	}
	return strings.TrimSpace(string(b))
}

// ---------- 入力 ----------

// KeyEvent は、端末モードのあいだの本体キー（value：1 押す、0 離す、2 リピート）。
func (m *TermMode) KeyEvent(code evdev.EvCode, value int32) {
	m.mu.Lock()
	if m.state == termOff && !m.want {
		m.mu.Unlock()
		return
	}
	app := m.term != nil && m.term.AppCursor()
	r := m.keys.event(code, value, app)
	var poke bool
	if r.scroll != 0 && m.term != nil {
		poke = m.term.Scroll(r.scroll * max(m.geom.Rows-1, 1))
	}
	if len(r.out) > 0 && m.term != nil {
		if v, _ := m.term.View(); v > 0 { // 打ったら、今の画面に戻る
			m.term.Scroll(-v)
			poke = true
		}
	}
	restart := len(r.out) == 1 && r.out[0] == '\r' && m.conn == nil && m.state == termOn && len(m.tc.Command) > 0
	if m.keys.oneCtrl || m.keys.oneAlt || value == 1 {
		poke = true // 帯の [Ctrl] を消す
	}
	m.mu.Unlock()
	if r.exit {
		log.Printf("terminal: leaving (Alt+Esc)")
		m.Request("off")
		return
	}
	if restart {
		go m.restartCommand()
		return
	}
	m.send(r.out)
	if poke {
		m.disp.Poke()
	}
}

// softActions は、端末モードのあいだの、画面右のソフトキーの働き。
var softActions = map[string]string{
	"home": "exit", "up": "scroll_up", "down": "scroll_down", "right": "page_next", "left": "page_prev",
	"enter": "enter", "back": "esc", "menu": "ctrl",
}

// Touch は、端末モードのあいだのタッチ（押した瞬間）。x、y はパネルの生の座標。
// soft は、その座標にあるソフトキーの区画の名前（なければ空）。
func (m *TermMode) Touch(soft string, p image.Point) {
	m.mu.Lock()
	if m.state == termOff && !m.want {
		m.mu.Unlock()
		return
	}
	var out []byte
	act := ""
	if soft != "" {
		act = softActions[soft]
	} else if col, row, ok := m.geom.padKeyAt(p); ok {
		m.pressed = row*termPadCols + col
		key := termPadPages[m.page%len(termPadPages)].Keys[row][col]
		switch key.Mod {
		case "ctrl":
			m.keys.oneCtrl = !m.keys.oneCtrl
		case "alt":
			m.keys.oneAlt = !m.keys.oneAlt
		default:
			out = m.keys.padBytes(key, m.term != nil && m.term.AppCursor())
		}
	}
	if m.term == nil && act != "exit" { // 入る手順の途中（端末をまだ作っていない）
		m.mu.Unlock()
		return
	}
	rows := max(m.geom.Rows-1, 1)
	switch act {
	case "scroll_up":
		m.term.Scroll(rows)
	case "scroll_down":
		m.term.Scroll(-rows)
	case "page_next":
		m.page = (m.page + 1) % len(termPadPages)
	case "page_prev":
		m.page = (m.page + len(termPadPages) - 1) % len(termPadPages)
	case "enter":
		out = []byte{'\r'}
	case "esc":
		out = []byte{0x1b}
	case "ctrl":
		m.keys.oneCtrl = !m.keys.oneCtrl
	}
	if len(out) > 0 && m.term != nil {
		if v, _ := m.term.View(); v > 0 {
			m.term.Scroll(-v)
		}
	}
	restart := act == "enter" && m.conn == nil && m.state == termOn && len(m.tc.Command) > 0
	m.mu.Unlock()
	switch {
	case act == "exit":
		log.Printf("terminal: leaving (HOME)")
		m.Request("off")
		return
	case restart:
		go m.restartCommand()
	default:
		m.send(out)
	}
	m.disp.Poke()
}

// TouchUp は、タッチを離したとき。
func (m *TermMode) TouchUp() {
	m.mu.Lock()
	changed := m.pressed >= 0
	m.pressed = -1
	m.mu.Unlock()
	if changed {
		m.disp.Poke()
	}
}

// ---------- 画面 ----------

// takeFrame は、描く側（Display）が呼ぶ。端末の変わった行を写し、帯の状態を返す。
func (m *TermMode) takeFrame(f *termFrame) termStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.term != nil {
		m.term.TakeFrame(f)
	}
	st := termStatus{State: m.status, Level: m.level, Ctrl: m.keys.oneCtrl || m.keys.ctrl > 0,
		Alt: m.keys.oneAlt, Page: m.page, Pressed: m.pressed, Exitable: m.hasHome()}
	if len(m.tc.Command) > 0 {
		st.Transport = "コマンド"
	} else {
		st.Transport = "シリアル"
	}
	if m.state == termLeaving {
		st.ExitingMsg = "抜けています…（USB を付け直しています）"
	}
	return st
}

func (m *TermMode) hasHome() bool {
	cfg := m.cfg()
	if cfg == nil || cfg.Touch == nil {
		return false
	}
	_, ok := cfg.Touch.SoftAreas["home"]
	return ok
}

// geometry は、今の画面の配置。
func (m *TermMode) geometry() termGeom {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.geom
}

// ---------- 設定 ----------

// termLabels は、terminal のセルに出す名前。
var termLabels = map[string]string{"on": "端末", "off": "端末を出る", "toggle": "端末"}

func (tc TerminalConfig) withDefaults() TerminalConfig {
	if tc.Port == "" {
		tc.Port = defaultTermPort
	}
	if tc.Getty == "" {
		tc.Getty = defaultTermGetty
	}
	if tc.Font == "" {
		tc.Font = defaultTermFont
	}
	if tc.Scrollback == 0 {
		tc.Scrollback = defaultScrollback
	}
	return tc
}

func (tc TerminalConfig) describe() string {
	if len(tc.Command) > 0 {
		return fmt.Sprintf("command %q as %s", strings.Join(tc.Command, " "), tc.User)
	}
	return "serial " + tc.Port + ", getty " + tc.Getty
}

// validate は terminal の誤りを返す。
func (tc *TerminalConfig) validate(fail func(path, format string, args ...any)) {
	if tc == nil {
		return
	}
	if tc.Port != "" && !strings.HasPrefix(tc.Port, "/dev/") {
		fail("/terminal/port", "terminal.port must be a device path such as /dev/ttyGS0")
	}
	if tc.Getty != "" && tc.Getty != "none" && !strings.HasSuffix(tc.Getty, ".service") {
		fail("/terminal/getty", "terminal.getty must be a systemd service name (serial-getty@ttyGS0.service) or none")
	}
	if tc.Font != "" && termFontWidths[tc.Font] == 0 {
		fail("/terminal/font", "terminal.font must be wide or narrow")
	}
	if tc.Scrollback < 0 || tc.Scrollback > 10000 {
		fail("/terminal/scrollback", "terminal.scrollback must be 0..10000")
	}
	if len(tc.Command) > 0 && tc.User == "" {
		fail("/terminal/user", "terminal.user is required with terminal.command (the command does not run as root)")
	}
	if len(tc.Command) > 0 && tc.Command[0] == "" {
		fail("/terminal/command", "terminal.command must start with the program")
	}
	if tc.User == "root" {
		fail("/terminal/user", "terminal.user must not be root")
	}
}

// ---------- シリアル ----------

type serialConn struct {
	*os.File
}

func (serialConn) Kind() string { return "serial" }

func openTermSerial(path string) (termConn, error) {
	f, err := openSerial(path)
	if err != nil {
		return nil, err
	}
	return serialConn{f}, nil
}

// ---------- getty と、落ちたあとの後始末 ----------

func runSystemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	// --no-ask-password：systemctl が起こすパスワードの問い合わせのエージェントが出力を握り、戻りが遅れないように
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--no-ask-password"}, args...)...).CombinedOutput()
	if err != nil && len(out) > 0 {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return err
}

// writeTermMarker は、端末モードに入ったことを /run に残す（落ちたときの後始末のため）。
func writeTermMarker(getty string, gettyOn bool) {
	on := 0
	if gettyOn {
		on = 1
	}
	if err := os.WriteFile(termMarker, []byte(fmt.Sprintf("getty=%s active=%d\n", getty, on)), 0644); err != nil {
		log.Printf("terminal: %v", err)
	}
}

// restoreTerminal は、端末モードのまま lefthand が終わったときに、USB の構成の名前と Brain の getty を元に戻す。
// lefthand -restore-console（ExecStopPost）と、デーモンの起動時に呼ぶ。印がなければ何もしない。
func restoreTerminal(script string) {
	b, err := os.ReadFile(termMarker)
	if err != nil {
		return
	}
	var getty string
	var on int
	fmt.Sscanf(strings.TrimSpace(string(b)), "getty=%s active=%d", &getty, &on)
	log.Printf("terminal: restoring after an unclean exit (getty %s, was active %v)", getty, on == 1)
	mouse := detectHID("/dev/hidg0").MouseID != 0
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := runGadgetSetup(ctx, script, mouse, false); err != nil {
		log.Printf("terminal: %v", err)
	}
	cancel()
	if on == 1 && getty != "" && getty != "none" {
		if err := runSystemctl("start", getty); err != nil {
			log.Printf("terminal: start %s: %v", getty, err)
		}
	}
	os.Remove(termMarker)
}
