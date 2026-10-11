package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- 日本語の入力（uim-fep） ----------
//
// Brain（Brainux）には uim-fep と uim-anthy（anthy-utf8）が入っている。uim-fep は端末の中で動く仕組みなので、
// brain-deck の文字を入れる欄（prompt.go）では、欄を「2 行だけの小さな端末」にして、その中で uim-fep と、
// 1 行を読むだけのプログラム（bash の read -e。readline）を動かす。uim-fep が描く変換中の文字（下線）、
// 文節（反転）、候補の一覧（2 行目）を、端末の描き方（term.go）でそのまま受け取り、欄に描く。
//
//   lefthand ──PTY──▶ uim-fep -u anthy-utf8 -s lastline -e bash line.sh      （Brain の user として動かす。root ではない）
//                       1 行目：readline の行（打った文字、変換中の文字）
//                       2 行目：uim-fep の状態の行（入力方式とモード、変換の候補）
//   line.sh は、決定（Enter）で ESC ] 7777 ; ok ; <文字> BEL を、やめる（Ctrl+]）で ESC ] 7777 ; cancel BEL を書く。
//   lefthand は Term.OnOSC でそれを受け取り、欄の結果にする。
//
// キー（uim.scm で決める。Brain のキーボードにある Ctrl と組み合わせる）：
//   Ctrl+Space  かな と 英数 を切り替える（本体の「文字切り替え」を 1 回押すのと同じ。lefthand が NUL を送る）
//   Ctrl+^      英数にする（時刻の欄を開くときなど、かなになっていても英数から始めたいときに lefthand が送る）
//   Ctrl+]      変換中ならやめる、でなければ欄をやめる（readline のマクロで、行を消して結果を返す）
//   Space       変換、次の候補。Enter 確定。← → 文節の移動、Shift+← → 文節の長さ、Esc 変換をやめる（uim の既定）
//
// uim-fep は起動に 3 秒ほどかかる（ARM926）ので、一度起動したら欄を閉じても止めず、しばらく（ime.idle、既定 10 分）
// 使わなければ止めてメモリを空ける。起動が終わるまでに打ったキーはためておいて、終わってから送る。
// uim の設定は ~/.uim を書き換えず、LIBUIM_USER_SCM_FILE で lefthand が作る uim.scm（~/.uim があれば最後に読む）を使う。
// Brain 自身のシェル（端末モードの Brain）でも、同じ入力方式と uim.scm で uim-fep を通す（termmode.go、contrib/brain/lefthand-ime.sh）。

// IMEConfig は設定の ime。
type IMEConfig struct {
	Method   string `yaml:"method,omitempty" json:"method,omitempty"`     // uim の入力方式。auto（既定。入っているものから選ぶ）、off（使わない）、anthy-utf8 など
	User     string `yaml:"user,omitempty" json:"user,omitempty"`         // uim-fep を動かすユーザー（既定 terminal.brain_user、なければ user。root は不可）
	Idle     string `yaml:"idle,omitempty" json:"idle,omitempty"`         // 使わなくなってから止めるまで（既定 10m。0 で止めない）
	Terminal *bool  `yaml:"terminal,omitempty" json:"terminal,omitempty"` // Brain 自身のシェル（端末モードの Brain）でも使う（既定 true）
}

const (
	imeAuto        = "auto"
	imeOff         = "off"
	defaultIMEIdle = 10 * time.Minute
	imeOSC         = "7777"
	imeRows        = 2
	imeStartFor    = 20 * time.Second // 起動を待つ上限

	imeKeyToggle = 0x00 // Ctrl+Space
	imeKeyLatin  = 0x1e // Ctrl+^
	imeKeyCancel = 0x1d // Ctrl+]
)

// imeModulesFile は、uim に登録されている入力方式の一覧（uim-module-manager が書く）。
var imeModulesFile = "/usr/share/uim/installed-modules.scm"

func (c *IMEConfig) method() string {
	if c == nil || c.Method == "" {
		return imeAuto
	}
	return c.Method
}

func (c *IMEConfig) idle() time.Duration {
	if c == nil || c.Idle == "" {
		return defaultIMEIdle
	}
	d, err := time.ParseDuration(c.Idle)
	if err != nil || d < 0 {
		return defaultIMEIdle
	}
	return d
}

func (c *IMEConfig) terminal() bool { return c == nil || c.Terminal == nil || *c.Terminal }

// validate は ime の誤りを返す。
func (c *IMEConfig) validate(fail func(path, format string, args ...any)) {
	if c == nil {
		return
	}
	if m := c.Method; m != "" && m != imeAuto && m != imeOff && !imeMethodName.MatchString(m) {
		fail("/ime/method", "ime.method must be auto, off, or a uim input method name such as anthy-utf8")
	}
	if c.User == "root" || c.User == "0" {
		fail("/ime/user", "ime.user must not be root")
	} else if c.User != "" && (strings.ContainsAny(c.User, " /:\t") || strings.HasPrefix(c.User, "-")) {
		fail("/ime/user", "ime.user must be a user name")
	}
	if c.Idle != "" {
		if d, err := time.ParseDuration(c.Idle); err != nil || d < 0 {
			fail("/ime/idle", "ime.idle must be a duration such as 10m (0 to keep running)")
		}
	}
}

var imeMethodName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// imeInstalled は、uim に登録されている入力方式（installed-modules.scm の installed-im-list）。
func imeInstalled(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	i := bytes.Index(b, []byte("(define installed-im-list '("))
	if i < 0 {
		return nil
	}
	rest := b[i+len("(define installed-im-list '("):]
	j := bytes.IndexByte(rest, ')')
	if j < 0 {
		return nil
	}
	var out []string
	for _, f := range strings.Fields(string(rest[:j])) {
		if imeMethodName.MatchString(f) {
			out = append(out, f)
		}
	}
	return out
}

// imeChoose は、設定の method と入っている方式から、使う方式を決める（空なら使わない）。auto は anthy-utf8、anthy、
// それ以外の順。
func imeChoose(method string, installed []string) string {
	switch method {
	case imeOff:
		return ""
	case imeAuto, "":
		for _, p := range []string{"anthy-utf8", "anthy"} {
			for _, m := range installed {
				if m == p {
					return m
				}
			}
		}
		if len(installed) > 0 {
			return installed[0]
		}
		return ""
	}
	return method
}

// imeModeLabels は、uim-fep の状態の行の、モードを表す文字（anthy の [Anあ Ｒ] の「あ」など）と、画面に出す名前。
var imeModeLabels = map[rune]string{'-': "英数", 'あ': "かな", 'ア': "カナ", 'ｱ': "半角カナ", 'a': "英数", 'Ａ': "全角英数", 'A': "全角英数"}

// imeStatusRe は、uim-fep の状態の行（<入力方式>[<モード>]）。候補を出しているあいだは、この形ではない。
var imeStatusRe = regexp.MustCompile(`^([a-z0-9_-]+)\[(.*)\]$`)

// imeMode は、状態の行から、モードの名前（かな、英数など）を取る。状態の行でなければ空。
func imeMode(status string) (mode string, isStatus bool) {
	m := imeStatusRe.FindStringSubmatch(strings.TrimSpace(status))
	if m == nil {
		return "", false
	}
	label := []rune(m[2])
	// anthy：An + モード + 空白 + 入力の形（Ｒ ローマ字、か かな）。ほかの方式は、見つかった最初の文字で決める
	if len(label) >= 3 && string(label[:2]) == "An" {
		label = label[2:3]
	}
	for _, r := range label {
		if s, ok := imeModeLabels[r]; ok {
			return s, true
		}
	}
	return "", true
}

// imeMethodLabel は、設定の値を画面に出す形にする。
func imeMethodLabel(m string) string {
	switch m {
	case imeOff, "":
		return "使わない"
	case imeAuto:
		return "自動"
	case "anthy-utf8", "anthy":
		return "Anthy（" + m + "）"
	case "skk":
		return "SKK（skk）"
	}
	return m
}

// imeRowValue は、設定のアプリの行に出す値（設定の値と、実際に使う方式）。
func imeRowValue(setting string, info func() IMEInfo) string {
	if setting == imeOff {
		return "使わない"
	}
	if info == nil {
		return imeMethodLabel(setting) + "・使えません"
	}
	in := info()
	switch {
	case !in.UimFep:
		return imeMethodLabel(setting) + "・uim-fep がない"
	case in.Method == "":
		return imeMethodLabel(setting) + "・方式がない"
	case setting == imeAuto:
		return "自動：" + imeMethodLabel(in.Method)
	}
	return imeMethodLabel(in.Method)
}

// imeInfoLabel は、情報のタブに出す 1 行。
func imeInfoLabel(in IMEInfo) string {
	switch {
	case !in.UimFep:
		return "uim-fep が入っていない"
	case in.Method == "":
		return "使わない（" + imeMethodLabel(in.Setting) + "）"
	}
	s := imeMethodLabel(in.Method)
	if in.Running {
		s += "、uim-fep は動いている"
	} else {
		s += "、uim-fep は止まっている"
	}
	if in.Terminal {
		s += "、Brain のシェルでも使う"
	}
	if in.Error != "" {
		s += "、" + in.Error
	}
	return s
}

// IMECell は、欄に描く 1 桁（端末の桁と同じ。全角は 2 桁で、右の桁は Width 0）。
type IMECell struct {
	R         rune
	Width     int
	Underline bool // 変換中の文字
	Reverse   bool // 選んでいる文節、候補
}

// IMEView は、欄に描くもの。
type IMEView struct {
	Line       []IMECell // 1 行目（打った文字と変換中の文字）
	Cursor     int       // カーソルの桁
	Candidates []IMECell // 2 行目に候補が出ているとき。出ていなければ nil
	Mode       string    // かな、英数など（分からなければ空）
	Busy       string    // 起動中などの知らせ（空なら使える）
}

// IME は、uim-fep を動かし、文字を入れる欄に 1 行の端末を貸す係。
type IME struct {
	mu         sync.Mutex
	cfg        func() *Config
	dir        string // uim.scm、inputrc、line.sh を置くところ（データの置き場所の ime）
	user       string
	method     string
	cols       int
	conn       *ptyConn
	term       *Term
	gen        int
	ready      bool
	starting   time.Time
	queue      []byte
	open       bool
	skipCanc   int // 欄を閉じたときに送った Ctrl+] の数（その分の cancel の結果は捨てる）
	idle       *time.Timer
	lastErr    string
	onResult   func(ok bool, text string)
	onChange   func()
	writeCh    chan []byte
	pending    []imeResult // term.Write のあいだに届いた結果（ロックを放してから onResult に渡す）
	mode       string      // 最後に分かったモード（候補を出しているあいだは状態の行がないので覚えておく）
	lastStatus string      // 状態の行（変わったときに -v で記録する）

	// テストで差し替える
	start     func(user string, argv, env []string, dir string, cols, rows int) (*ptyConn, error)
	lookPath  func(string) (string, error)
	installed func() []string
	now       func() time.Time
}

// NewIME は係を作る。dataDir の ime に、uim-fep に渡すファイルを作る。
func NewIME(dataDir string, cfg func() *Config) *IME {
	return &IME{cfg: cfg, dir: filepath.Join(dataDir, "ime"), start: startUserPTY, lookPath: exec.LookPath,
		installed: func() []string { return imeInstalled(imeModulesFile) }, now: time.Now}
}

// SetHandlers は、結果（確定、やめる）と、描くものが変わったときに呼ぶ関数を設定する。どちらも待たずに返ること。
func (m *IME) SetHandlers(result func(ok bool, text string), change func()) {
	m.mu.Lock()
	m.onResult, m.onChange = result, change
	m.mu.Unlock()
}

// IMEInfo は、get_status と設定のアプリに出す状態。
type IMEInfo struct {
	Method    string   `json:"method"`              // 使う入力方式（空なら使わない）
	Setting   string   `json:"setting"`             // 設定の値（auto、off、方式の名前）
	Installed []string `json:"installed,omitempty"` // uim に登録されている方式
	UimFep    bool     `json:"uim_fep"`             // uim-fep が入っているか
	Running   bool     `json:"running"`             // uim-fep が動いているか
	Terminal  bool     `json:"terminal"`            // Brain のシェルでも使うか
	Error     string   `json:"error,omitempty"`     // 最後に起動できなかった理由
}

// Info は今の状態。
func (m *IME) Info() IMEInfo {
	if m == nil {
		return IMEInfo{Setting: imeOff}
	}
	var c *IMEConfig
	if cfg := m.cfg(); cfg != nil {
		c = cfg.IME
	}
	installed := m.installed()
	_, err := m.lookPath("uim-fep")
	in := IMEInfo{Setting: c.method(), Installed: installed, UimFep: err == nil, Terminal: c.terminal()}
	if in.UimFep {
		in.Method = imeChoose(c.method(), installed)
	}
	m.mu.Lock()
	in.Running = m.conn != nil
	in.Error = m.lastErr
	m.mu.Unlock()
	return in
}

// Method は、使う入力方式（空なら使わない）。
func (m *IME) Method() string {
	if m == nil {
		return ""
	}
	return m.Info().Method
}

// TerminalEnv は、Brain 自身のシェル（端末モードの Brain）に渡す環境変数。使わないなら nil。
func (m *IME) TerminalEnv() []string {
	if m == nil {
		return nil
	}
	in := m.Info()
	if in.Method == "" || !in.Terminal {
		return nil
	}
	if err := m.writeFiles(); err != nil {
		log.Printf("ime: %v", err)
		return nil
	}
	return []string{"BRAIN_IME=" + in.Method, "LIBUIM_USER_SCM_FILE=" + filepath.Join(m.dir, "uim.scm")}
}

func (m *IME) userName() string {
	cfg := m.cfg()
	if cfg != nil && cfg.IME != nil && cfg.IME.User != "" {
		return cfg.IME.User
	}
	if cfg != nil && cfg.Terminal != nil && cfg.Terminal.BrainUser != "" {
		return cfg.Terminal.BrainUser
	}
	return defaultBrainUser
}

// imeScm は、uim に読ませる設定（キー）。
const imeScm = `;; lefthand（brain-deck）が作る uim の設定。uim-fep に LIBUIM_USER_SCM_FILE で渡す。手で直しても、次に作り直す。
;; 自分の設定は ~/.uim に書く（最後に読む）。
(define-key generic-on-key? '("<Control> " "zenkaku-hankaku" "<Shift> "))
(define-key generic-off-key? '("<Control> " "<Control>^" "zenkaku-hankaku" "<Shift> "))
(define-key anthy-on-key? '("<Control> " "zenkaku-hankaku" "<Shift> "))
(define-key anthy-off-key? '("<Control> " "<Control>^" "zenkaku-hankaku" "<Shift> "))
(define-key generic-cancel-key? '("escape" "<Control>[" "<Control>]"))
(define-key anthy-cancel-key? '("escape" "<Control>[" "<Control>]"))
(define-key skk-cancel-key? '("escape" "<Control>[" "<Control>]" "<Control>g"))
(let ((home (getenv "HOME")))
  (if home
      (try-load (string-append home "/.uim"))))
`

// imeInputrc は、欄の readline の設定。Ctrl+] は行を消して、先頭に Ctrl+] を入れた行として確定する（line.sh が cancel にする）。
const imeInputrc = `# lefthand（brain-deck）が作る。文字を入れる欄の readline の設定
set enable-bracketed-paste off
set horizontal-scroll-mode on
set bell-style none
set echo-control-characters off
"\C-]": "\C-a\C-k\C-v\C-]\C-j"
`

// imeLineSh は、1 行を読んで結果を OSC で返すプログラム。欄を閉じても終わらず、次の行を待つ。
// 行を読み始めるとき、プロンプトとして「1 行目の先頭に戻って消す」を出す（前の行の改行で 2 行目に下りているので）。
const imeLineSh = `# lefthand（brain-deck）が作る。文字を入れる欄の 1 行を readline で読み、結果を ESC ] 7777 ; … BEL で返す
while :; do
  IFS= read -e -r -p $'\001\033[1;1H\033[K\002' l || exit 0
  case "$l" in
    $'\035'*) printf '\033]7777;cancel\a' ;;
    *) printf '\033]7777;ok;%s\a' "$l" ;;
  esac
done
`

// writeFiles は、uim-fep に渡すファイルを作る（user が読めるように 0644。置き場所は 0755）。
func (m *IME) writeFiles() error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	os.Chmod(m.dir, 0o755)
	for name, body := range map[string]string{"uim.scm": imeScm, "inputrc": imeInputrc, "line.sh": imeLineSh} {
		p := filepath.Join(m.dir, name)
		if b, err := os.ReadFile(p); err == nil && string(b) == body {
			os.Chmod(p, 0o644)
			continue
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
		os.Chmod(p, 0o644)
	}
	return nil
}

// Open は欄を開く。initial は最初に入れておく文字（英数字だけ）。latin なら英数から始める。
// uim-fep が使えなければ false（欄は prompt.go の英数字だけの入れ方になる）。
func (m *IME) Open(cols int, initial string, latin bool) bool {
	if m == nil {
		return false
	}
	in := m.Info()
	if in.Method == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idle != nil {
		m.idle.Stop()
		m.idle = nil
	}
	if m.conn != nil && (m.cols != cols || m.method != in.Method) {
		m.stopLocked()
	}
	if m.conn == nil {
		if err := m.startLocked(in.Method, cols); err != nil {
			log.Printf("ime: %v", err)
			m.lastErr = err.Error()
			return false
		}
		m.lastErr = ""
	}
	m.open = true
	var b []byte
	if latin || initial != "" {
		b = append(b, imeKeyLatin)
	}
	b = append(b, []byte(initial)...)
	m.sendLocked(b)
	return true
}

func (m *IME) startLocked(method string, cols int) error {
	if err := m.writeFiles(); err != nil {
		return err
	}
	user := m.userName()
	if user == "root" {
		return errors.New("ime.user must not be root")
	}
	env := []string{"TERM=xterm-256color", "LANG=en_US.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin", "SHELL=/bin/bash",
		"LIBUIM_USER_SCM_FILE=" + filepath.Join(m.dir, "uim.scm"), "INPUTRC=" + filepath.Join(m.dir, "inputrc")}
	argv := []string{"uim-fep", "-u", method, "-s", "lastline", "-e", "bash", "--norc", "--noprofile", filepath.Join(m.dir, "line.sh")}
	c, err := m.start(user, argv, env, "", cols, imeRows)
	if err != nil {
		return fmt.Errorf("start uim-fep: %w", err)
	}
	m.gen++
	gen := m.gen
	ch := make(chan []byte, 64)
	m.writeCh = ch
	m.conn, m.method, m.cols = c, method, cols
	m.ready, m.queue, m.skipCanc = false, nil, 0
	m.starting = m.now()
	m.term = NewTerm(cols, imeRows, 0, func(b []byte) {
		select {
		case ch <- append([]byte(nil), b...):
		default:
		}
	})
	m.term.OnOSC = func(num, rest string) bool { return m.oscLocked(num, rest) }
	log.Printf("ime: started uim-fep -u %s as %s (%d cols)", method, user, cols)
	go m.writer(c, ch)
	go m.reader(c, gen)
	return nil
}

// imeChunkGap は、制御のキー（Ctrl+Space など）の前後に置く間。uim-fep は、1 回の読み取りで制御の文字と文字が一緒に
// 届くと、知らないエスケープシーケンスとみなして、そのまま readline に渡してしまう（実機の uim-fep 1.9.6 で、
// Ctrl+^ と文字を一度に書いたら Ctrl+^ が効かなかった）。制御の文字は 1 つずつ、間を置いて書く
const imeChunkGap = 30 * time.Millisecond

func (m *IME) writer(c *ptyConn, ch chan []byte) {
	for b := range ch {
		for _, chunk := range imeSplit(b) {
			if _, err := c.Write(chunk); err != nil {
				return
			}
			if chunk[0] < 0x20 || chunk[0] == 0x7f {
				time.Sleep(imeChunkGap)
			}
		}
	}
}

// imeSplit は、送るバイト列を、uim-fep が 1 回に読んでよい単位に分ける：ふつうの文字の並び、制御の文字 1 つ、
// エスケープシーケンス（ESC [ … 最後の文字、ESC O 文字、ESC + 1 文字）1 つ。
func imeSplit(b []byte) [][]byte {
	var out [][]byte
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			j := i + 1
			switch {
			case j < len(b) && b[j] == '[':
				j++
				for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
					j++
				}
				if j < len(b) {
					j++
				}
			case j < len(b):
				j++
			}
			out = append(out, b[i:j])
			i = j
		case c < 0x20 || c == 0x7f:
			out = append(out, b[i:i+1])
			i++
		default:
			j := i + 1
			for j < len(b) && b[j] >= 0x20 && b[j] != 0x7f {
				j++
			}
			out = append(out, b[i:j])
			i = j
		}
	}
	return out
}

// results は、reader が受け取った結果（ロックを放してから渡す）。
type imeResult struct {
	ok   bool
	text string
}

func (m *IME) reader(c *ptyConn, gen int) {
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		var results []imeResult
		var change func()
		m.mu.Lock()
		if m.gen != gen {
			m.mu.Unlock()
			return
		}
		if n > 0 {
			m.pending = nil
			m.term.Write(buf[:n])
			results = m.pending
			m.pending = nil
			if st := m.term.LineText(1); st != m.lastStatus {
				vlogf("ime: status line %q", st)
				m.lastStatus = st
			}
			if !m.ready && m.term.LineText(1) != "" {
				m.ready = true
				log.Printf("ime: ready (%v)", m.now().Sub(m.starting).Round(100*time.Millisecond))
				if len(m.queue) > 0 {
					m.sendLocked(m.queue)
					m.queue = nil
				}
			}
			change = m.onChange
		}
		if err != nil {
			reason := err.Error()
			if c.exitText() != "" {
				reason = c.exitText()
			}
			log.Printf("ime: uim-fep stopped %s", reason)
			wasOpen := m.open
			m.stopLocked()
			if wasOpen {
				m.lastErr = "uim-fep が止まりました " + reason
				results = append(results, imeResult{ok: false, text: m.lastErr})
			}
			change = m.onChange
		}
		onResult := m.onResult
		m.mu.Unlock()
		for _, r := range results {
			if onResult != nil {
				onResult(r.ok, r.text)
			}
		}
		if change != nil {
			change()
		}
		if err != nil {
			return
		}
	}
}

// oscLocked は、line.sh からの結果（Term.OnOSC。m.mu を持ったまま呼ばれる）。
func (m *IME) oscLocked(num, rest string) bool {
	if num != imeOSC {
		return false
	}
	kind, text, _ := cut(rest, ';')
	switch kind {
	case "ok":
		if m.open {
			m.open = false
			m.pending = append(m.pending, imeResult{ok: true, text: text})
			m.idleLocked()
		}
	case "cancel":
		if m.skipCanc > 0 {
			m.skipCanc--
		} else if m.open {
			m.open = false
			m.pending = append(m.pending, imeResult{ok: false, text: ""})
			m.idleLocked()
		}
	}
	return true
}

// sendLocked は、uim-fep に送る（起動が終わっていなければためておく）。
func (m *IME) sendLocked(b []byte) {
	if len(b) == 0 || m.conn == nil {
		return
	}
	if !m.ready {
		m.queue = append(m.queue, b...)
		return
	}
	select {
	case m.writeCh <- append([]byte(nil), b...):
	default:
		log.Printf("ime: write queue full, dropping %d bytes", len(b))
	}
}

// Send は、キーをバイト列で送る（欄が開いているあいだ）。
func (m *IME) Send(b []byte) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.open {
		m.sendLocked(b)
	}
	m.mu.Unlock()
}

// Cancel は、変換中ならそれをやめ、でなければ欄をやめる（結果は onResult に cancel で届く）。
func (m *IME) Cancel() { m.Send([]byte{imeKeyCancel}) }

// Close は、結果を待たずに欄を閉じる（ほかの欄に切り替えたときなど）。readline の行は消しておく。
func (m *IME) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.open && m.conn != nil {
		m.open = false
		m.skipCanc += 2 // 変換中なら 1 つ目で変換をやめ、2 つ目で行を消す。変換中でなければ、2 つとも cancel が返る
		m.sendLocked([]byte{imeKeyCancel, imeKeyCancel})
		m.idleLocked()
	}
	m.mu.Unlock()
}

// Open のあと、結果が返るまでの状態。
func (m *IME) Active() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open
}

// idleLocked は、欄を閉じたあと、しばらく使わなければ止めるタイマーを掛ける。
func (m *IME) idleLocked() {
	if m.idle != nil {
		m.idle.Stop()
	}
	var d time.Duration = defaultIMEIdle
	if cfg := m.cfg(); cfg != nil {
		d = cfg.IME.idle()
	}
	if d == 0 {
		return
	}
	m.idle = time.AfterFunc(d, func() {
		m.mu.Lock()
		if !m.open && m.conn != nil {
			log.Printf("ime: idle for %v, stopping uim-fep", d)
			m.stopLocked()
		}
		m.mu.Unlock()
	})
}

func (m *IME) stopLocked() {
	if m.conn != nil {
		m.conn.Close()
		if p := m.conn.cmd.Process; p != nil {
			go func(p *os.Process, done chan struct{}) {
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					syscall.Kill(-p.Pid, syscall.SIGKILL)
				}
			}(p, m.conn.done)
		}
	}
	if m.writeCh != nil {
		close(m.writeCh)
		m.writeCh = nil
	}
	m.conn, m.term, m.ready, m.queue, m.open = nil, nil, false, nil, false
	m.gen++
}

// Stop は uim-fep を止める（終了のとき、設定の反映で方式が変わったとき）。
func (m *IME) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.idle != nil {
		m.idle.Stop()
		m.idle = nil
	}
	m.stopLocked()
	m.mu.Unlock()
}

// ConfigChanged は、設定の反映で呼ぶ。方式やユーザーが変わっていれば、次に開くときに起動し直す（開いていなければ今止める）。
func (m *IME) ConfigChanged() {
	if m == nil {
		return
	}
	in := m.Info()
	m.mu.Lock()
	if m.conn != nil && !m.open && (in.Method == "" || in.Method != m.method) {
		m.stopLocked()
	}
	m.mu.Unlock()
}

// View は、欄に描くもの。uim-fep が動いていなければ nil。
func (m *IME) View() *IMEView {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil || m.term == nil {
		return nil
	}
	v := &IMEView{}
	if !m.ready {
		v.Busy = "日本語入力を準備しています…"
		if m.now().Sub(m.starting) > imeStartFor {
			v.Busy = "日本語入力の起動に時間がかかっています"
		}
	}
	v.Line = imeCells(m.term.line(0), true)
	v.Cursor, _ = m.term.Cursor()
	status := m.term.LineText(1)
	mode, isStatus := imeMode(status)
	switch {
	case isStatus:
		v.Mode = mode
		m.mode = mode
	case status == "": // 確定した直後は状態の行が消えている（次のキーで描き直される）。最後に分かったモードを出す
		v.Mode = m.mode
	default:
		v.Mode = m.mode
		v.Candidates = imeCells(m.term.line(1), false)
	}
	return v
}

// imeCells は、端末の行を欄の桁にする（右の空白は除く）。
func imeCells(line []tcell, keepTrailing bool) []IMECell {
	out := make([]IMECell, 0, len(line))
	last := -1
	for i, c := range line {
		cell := IMECell{R: c.r, Width: int(c.w), Underline: c.at&attrUnderline != 0, Reverse: c.at&attrReverse != 0}
		if cell.R == 0 && cell.Width > 0 {
			cell.R = ' '
		}
		out = append(out, cell)
		if c.r != 0 && c.r != ' ' || c.at&(attrUnderline|attrReverse) != 0 {
			last = i
		}
	}
	if keepTrailing {
		return out
	}
	return out[:last+1]
}
