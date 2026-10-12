package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- 自分で足すアプリ（フェーズ K5）：スクリプトを動かす係 ----------
//
// スクリプトは、Brain の user（terminal.brain_user、既定 user）として、/bin/sh の ulimit を掛けてから動かす：
// 仮想メモリ（memory_mb）、CPU の時間（interval は timeout + 1 秒）、書けるファイルの大きさ（16 MiB）、開けるファイル（64）、
// user のプロセスの数（今の数 + 32。RLIMIT_NPROC は user のプロセスの合計を数えるので）。プロセスのグループごと動かし、優先度は nice 15（入力と描画を先にする）。
//
// interval：画面にあるあいだ、every ごとに（タップ、ボタン、キー、文字を入れたときはすぐに）動かし、標準出力を描く。
// timeout を超えたら止める。出力は output_kb まで（既定 32 KiB。超えたら止める）。標準エラーは最後の 4 KiB を、失敗したときに出す。
// stream：画面にあるあいだ動かし続け、1 行ごとに描き直す（0.2 秒に 1 回まで）。止まったら 5 秒、10 秒…と待って動かし直す
// （最大 5 分。1 分動いていれば元に戻す）。画面から消えて 30 秒たったら止める。出力は 10 秒に output_kb の 2 倍まで。
// 同時に動かすのは、すべてのアプリで設定の apps.max_running（既定 3）まで（stream は動いているあいだ 1 つ使う）。
// 時間、メモリ、出力は app.yaml で変えられ、設定の apps の上限を超える値のアプリは使えない（apps.go）。
// どれも、デーモンの入力と描画は待たない（別の goroutine で動かし、描くのは写しだけ）。

const (
	appMaxStderr     = 4 << 10
	appMaxLineBytes  = 1024
	appMaxScreen     = 200 // 1 つの画面の行の数
	appStreamWindow  = 10 * time.Second // stream の出力は、この時間に output_kb の 2 倍まで
	appStreamHide    = 30 * time.Second // 画面から消えて、これだけたったら stream を止める
	appVisibleFor    = 5 * time.Second  // 最後に描いてから、これだけは画面にあるとみなす
	appRedrawGap     = 200 * time.Millisecond
	appRestartMin    = 5 * time.Second
	appRestartMax    = 5 * time.Minute
	appRestartReset  = time.Minute
	appNice          = 15
	appTick          = time.Second
	appKillWait      = 2 * time.Second
	appMaxEventQueue = 8
)

// appState は、アプリ 1 つの動き。
type appState struct {
	def      *AppDef
	screen   *AppScreen
	status   string // 「止めました：…」など。空ならふつう
	level    int    // noticeInfo、noticeWarn、noticeError
	running  bool
	lastRun  time.Time
	nextRun  time.Time
	seen     time.Time // 最後に画面に描いた時刻
	gen      uint64
	pending  []appEvent // interval：動いているあいだに来たイベント
	proc     *appProc   // stream：動いているもの
	backoff  time.Duration
	building *AppScreen // stream：@clear のあとに作っている画面
	lastDraw time.Time
	drawSoon bool
	outWin   time.Time
	outBytes int
}

// appEvent は、スクリプトに渡すもの（引数と、標準入力に書く文字）。
type appEvent struct {
	args  []string
	stdin string
}

// appProc は、動いているスクリプト。
type appProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
	why   string // 止めた理由（上限を超えたなど）
}

// AppService は、アプリを動かす係（1 つのデーモンに 1 つ）。
type AppService struct {
	store    *AppStore
	userName func() string
	mu       sync.Mutex
	states   map[string]*appState
	slots    int
	onChange func()
	input    func(kind, title, hint string) // 文字を入れる欄を開く（prompt.go）
	stop     chan struct{}
	now      func() time.Time
	runAsMe  bool // テスト：user にならずに動かす
}

// NewAppService は、係を作る（Start で動かし始める）。
func NewAppService(store *AppStore, userName func() string) *AppService {
	s := &AppService{store: store, userName: userName, states: map[string]*appState{}, now: time.Now, stop: make(chan struct{})}
	store.mu.Lock()
	store.onChange = s.Forget
	store.mu.Unlock()
	return s
}

func (s *AppService) SetOnChange(f func()) { s.mu.Lock(); s.onChange = f; s.mu.Unlock() }

// SetInput は、文字を入れる欄を開く関数を設定する。
func (s *AppService) SetInput(f func(kind, title, hint string)) {
	s.mu.Lock()
	s.input = f
	s.mu.Unlock()
}

func (s *AppService) changedLocked() {
	if f := s.onChange; f != nil {
		go f()
	}
}

// Start は、時間で動かす goroutine を始める。
func (s *AppService) Start() {
	go func() {
		t := time.NewTicker(appTick)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.tick()
			}
		}
	}()
}

// Shutdown は、動いているものをすべて止める（デーモンを止めるとき）。
func (s *AppService) Shutdown() {
	if s == nil {
		return
	}
	s.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	var procs []*appProc
	for _, st := range s.states {
		if st.proc != nil {
			procs = append(procs, st.proc)
		}
	}
	s.mu.Unlock()
	for _, p := range procs {
		p.kill("デーモンを止めました")
	}
}

// Forget は、アプリを入れ替えたり消したりしたときに、動いているものを止めて、状態を捨てる。
func (s *AppService) Forget(name string) {
	s.mu.Lock()
	st := s.states[name]
	delete(s.states, name)
	s.mu.Unlock()
	if st != nil && st.proc != nil {
		st.proc.kill("入れ替えました")
	}
	s.mu.Lock()
	s.changedLocked()
	s.mu.Unlock()
}

// stateLocked は、名前のアプリの状態（なければ作る。アプリがなければ nil）。
func (s *AppService) stateLocked(name string) *appState {
	d, _ := s.store.Get(name)
	st := s.states[name]
	if d == nil {
		return nil
	}
	if st == nil || st.def != d {
		if st != nil && st.proc != nil {
			go st.proc.kill("入れ替えました")
		}
		st = &appState{def: d}
		s.states[name] = st
	}
	return st
}

// Seen は、アプリのセルを描いたとき（描画の goroutine から）。画面にあるあいだだけ動かす。
func (s *AppService) Seen(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	first := false
	if st := s.stateLocked(name); st != nil {
		first = st.seen.IsZero() || s.now().Sub(st.seen) > appVisibleFor
		st.seen = s.now()
	}
	s.mu.Unlock()
	if first {
		go s.tick() // 画面に出たら、待たずに動かす
	}
}

// tick は、画面にあるアプリを、決まった時刻に動かす。
func (s *AppService) tick() {
	now := s.now()
	type job struct {
		name string
		st   *appState
	}
	var start []job
	var stop []*appProc
	s.mu.Lock()
	for name, st := range s.states {
		visible := !st.seen.IsZero() && now.Sub(st.seen) <= appVisibleFor
		if st.def.Stream {
			switch {
			case st.proc != nil && !st.seen.IsZero() && now.Sub(st.seen) > appStreamHide:
				stop = append(stop, st.proc)
				st.proc.why = ""
			case st.proc == nil && !st.running && visible && !now.Before(st.nextRun):
				start = append(start, job{name, st})
			}
			continue
		}
		if visible && !st.running && (st.lastRun.IsZero() || (st.def.Every > 0 && !now.Before(st.nextRun))) {
			start = append(start, job{name, st})
		}
	}
	s.mu.Unlock()
	for _, p := range stop {
		p.kill("")
	}
	for _, j := range start {
		if j.st.def.Stream {
			s.startStream(j.name, j.st)
		} else {
			s.runInterval(j.name, j.st, appEvent{})
		}
	}
}

// Event は、タップ、ボタン、キー、文字（入力の goroutine から。待たずに返る）。
func (s *AppService) Event(name string, ev appEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	st := s.stateLocked(name)
	if st == nil {
		s.mu.Unlock()
		return
	}
	st.seen = s.now()
	if st.def.Stream {
		p := st.proc
		s.mu.Unlock()
		if p == nil {
			go s.tick()
			return
		}
		line := strings.Join(ev.args, " ")
		if ev.stdin != "" {
			line += " " + strings.NewReplacer("\n", " ", "\r", " ").Replace(ev.stdin)
		}
		go func() {
			p.stdin.Write([]byte(line + "\n")) // 読まなくて詰まっても、入力は待たない（この goroutine が待つ）
		}()
		return
	}
	if st.running {
		if len(st.pending) < appMaxEventQueue {
			st.pending = append(st.pending, ev)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	go s.runInterval(name, st, ev)
}

// takeSlot は、同時に動かす数の枠を 1 つ取る。取れなければ false。
func (s *AppService) takeSlotLocked() bool {
	if s.slots >= s.store.Limits().MaxRunning {
		return false
	}
	s.slots++
	return true
}

func (s *AppService) freeSlot() {
	s.mu.Lock()
	s.slots--
	s.mu.Unlock()
}

// setStatusLocked は、アプリの下に出す知らせを決める。
func (st *appState) setStatusLocked(msg string, level int) {
	st.status, st.level = msg, level
	st.gen++
}

// runInterval は、interval のアプリを 1 回動かし、出力を画面にする。
func (s *AppService) runInterval(name string, st *appState, ev appEvent) {
	s.mu.Lock()
	if st.running || s.states[name] != st {
		s.mu.Unlock()
		return
	}
	if !s.takeSlotLocked() {
		st.setStatusLocked(fmt.Sprintf("ほかのアプリが %d つ動いているので待っています", s.slots), noticeInfo)
		st.nextRun = s.now().Add(appTick)
		if ev.args != nil && len(st.pending) < appMaxEventQueue {
			st.pending = append(st.pending, ev)
		}
		s.changedLocked()
		s.mu.Unlock()
		return
	}
	st.running = true
	st.gen++
	def := st.def
	s.changedLocked()
	s.mu.Unlock()

	t0 := s.now()
	out, errOut, why, code, err := s.runOnce(def, ev)
	s.freeSlot()

	s.mu.Lock()
	defer s.mu.Unlock()
	st.running = false
	st.lastRun = s.now()
	if def.Every > 0 {
		st.nextRun = t0.Add(def.Every)
	}
	switch {
	case why != "":
		st.setStatusLocked("止めました："+why, noticeError)
		if len(out) > 0 {
			st.screen = parseAppOutput(out)
		}
	case err != nil:
		st.setStatusLocked("動かせません："+err.Error(), noticeError)
		log.Printf("apps: %s: %v", name, err)
	default:
		st.screen = parseAppOutput(out)
		if code != 0 {
			msg := fmt.Sprintf("終了コード %d", code)
			if l := lastErrLine(errOut); l != "" {
				msg += "：" + l
			}
			st.setStatusLocked(msg, noticeWarn)
		} else {
			st.setStatusLocked("", noticeInfo)
		}
	}
	vlogf("apps: %s %q: %d bytes in %v (exit %d)", name, ev.args, len(out), s.now().Sub(t0).Round(time.Millisecond), code)
	s.changedLocked()
	if len(st.pending) > 0 && s.states[name] == st {
		next := st.pending[0]
		st.pending = st.pending[1:]
		go s.runInterval(name, st, next)
	}
}

// lastErrLine は、最後の空でない行。
func lastErrLine(b []byte) string {
	ls := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}

// runOnce は、interval のスクリプトを 1 回動かす。why は上限を超えて止めた理由。
func (s *AppService) runOnce(def *AppDef, ev appEvent) (out, errOut []byte, why string, code int, err error) {
	cmd, err := s.command(def, ev.args, false)
	if err != nil {
		return nil, nil, "", -1, err
	}
	in := ev.stdin
	if in != "" && !strings.HasSuffix(in, "\n") {
		in += "\n" // read で読めるように
	}
	cmd.Stdin = strings.NewReader(in)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, "", -1, err
	}
	var stderr tailBuffer
	stderr.max = appMaxStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, "", -1, err
	}
	setNice(cmd.Process.Pid)
	p := &appProc{cmd: cmd, done: make(chan struct{})}
	timer := time.AfterFunc(def.Timeout, func() { p.kill(fmt.Sprintf("時間の上限（%v）を超えました", def.Timeout)) })
	var buf bytes.Buffer
	maxOut := def.OutputBytes
	_, rerr := io.Copy(&buf, io.LimitReader(stdout, int64(maxOut)+1))
	if rerr == nil && buf.Len() > maxOut {
		p.kill(fmt.Sprintf("出力が %s を超えました", kib(int64(maxOut))))
		io.Copy(io.Discard, stdout)
	}
	werr := cmd.Wait()
	timer.Stop()
	close(p.done)
	out = buf.Bytes()
	if len(out) > maxOut {
		out = out[:maxOut]
	}
	procMu.Lock()
	why = p.why
	procMu.Unlock()
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && why == "" {
				switch ws.Signal() {
				case syscall.SIGXCPU:
					why = "CPU の時間の上限を超えました"
				case syscall.SIGXFSZ:
					why = "書けるファイルの大きさの上限を超えました"
				case syscall.SIGKILL, syscall.SIGSEGV, syscall.SIGABRT:
					why = fmt.Sprintf("止まりました（%v。メモリの上限 %d MB を超えたかもしれません）", ws.Signal(), def.MemMB)
				default:
					why = fmt.Sprintf("止まりました（%v）", ws.Signal())
				}
			}
		} else {
			return out, stderr.Bytes(), why, -1, werr
		}
	}
	return out, stderr.Bytes(), why, code, nil
}

// procMu は、appProc の why を守る（数が少ないので 1 つで足りる）。
var procMu sync.Mutex

// kill は、プロセスのグループを止める。why が空でなければ、画面に出す理由として覚える。
func (p *appProc) kill(why string) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	procMu.Lock()
	if why != "" && p.why == "" {
		p.why = why
	}
	procMu.Unlock()
	pid := p.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		select {
		case <-p.done:
		case <-time.After(appKillWait):
		}
		syscall.Kill(-pid, syscall.SIGKILL) // 子が残っていれば
	}()
}

// startStream は、stream のアプリを動かし始める。
func (s *AppService) startStream(name string, st *appState) {
	s.mu.Lock()
	if st.proc != nil || st.running || s.states[name] != st {
		s.mu.Unlock()
		return
	}
	if !s.takeSlotLocked() {
		st.setStatusLocked(fmt.Sprintf("ほかのアプリが %d つ動いているので待っています", s.slots), noticeInfo)
		st.nextRun = s.now().Add(appTick)
		s.changedLocked()
		s.mu.Unlock()
		return
	}
	st.running = true
	def := st.def
	s.mu.Unlock()

	fail := func(err error) {
		s.freeSlot()
		s.mu.Lock()
		st.running = false
		st.nextRun = s.now().Add(s.backoffLocked(st))
		st.setStatusLocked("動かせません："+err.Error(), noticeError)
		s.changedLocked()
		s.mu.Unlock()
	}
	cmd, err := s.command(def, nil, true)
	if err != nil {
		fail(err)
		return
	}
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	if err := errors.Join(err1, err2); err != nil {
		fail(err)
		return
	}
	var stderr tailBuffer
	stderr.max = appMaxStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		fail(err)
		return
	}
	setNice(cmd.Process.Pid)
	p := &appProc{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	started := s.now()
	s.mu.Lock()
	st.proc = p
	st.lastRun = started
	st.building = &AppScreen{}
	st.outWin, st.outBytes = started, 0
	st.setStatusLocked("", noticeInfo)
	s.changedLocked()
	s.mu.Unlock()

	go func() {
		r := bufio.NewReaderSize(stdout, 4096)
		for {
			line, err := readAppLine(r)
			if line != "" || err == nil {
				if !s.streamLine(name, st, p, line) {
					p.kill(fmt.Sprintf("出力が %v に %s を超えました", appStreamWindow, kib(int64(2*def.OutputBytes))))
				}
			}
			if err != nil {
				break
			}
		}
		werr := cmd.Wait()
		close(p.done)
		stdin.Close()
		s.freeSlot()
		s.mu.Lock()
		defer s.mu.Unlock()
		st.running = false
		if st.proc == p {
			st.proc = nil
		}
		if st.building != nil && len(st.building.Lines) > 0 {
			st.screen = st.building
		}
		procMu.Lock()
		why := p.why
		procMu.Unlock()
		ran := s.now().Sub(started)
		if ran >= appRestartReset {
			st.backoff = 0
		}
		wait := s.backoffLocked(st)
		st.nextRun = s.now().Add(wait)
		switch {
		case why != "":
			st.setStatusLocked("止めました："+why, noticeError)
		case werr != nil:
			msg := "止まりました（" + werr.Error() + "）"
			if l := lastErrLine(stderr.Bytes()); l != "" {
				msg += "：" + l
			}
			st.setStatusLocked(msg+fmt.Sprintf("。%v 後に動かし直します", wait), noticeWarn)
		default:
			if !st.seen.IsZero() && s.now().Sub(st.seen) <= appVisibleFor {
				st.setStatusLocked(fmt.Sprintf("終わりました。%v 後に動かし直します", wait), noticeInfo)
			}
		}
		vlogf("apps: %s: stream ended after %v (%v)", name, ran.Round(time.Second), werr)
		s.changedLocked()
	}()
}

// backoffLocked は、stream を動かし直すまでの時間（5 秒から倍にして 5 分まで）。
func (s *AppService) backoffLocked(st *appState) time.Duration {
	if st.backoff == 0 {
		st.backoff = appRestartMin
	} else {
		st.backoff = min(st.backoff*2, appRestartMax)
	}
	return st.backoff
}

// streamLine は、stream の 1 行を画面に足す。出力が多すぎれば false。
func (s *AppService) streamLine(name string, st *appState, p *appProc, line string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.proc != p {
		return true
	}
	now := s.now()
	if now.Sub(st.outWin) > appStreamWindow {
		st.outWin, st.outBytes = now, 0
	}
	st.outBytes += len(line) + 1
	if st.outBytes > 2*st.def.OutputBytes {
		return false
	}
	if strings.TrimSpace(line) == "@clear" {
		st.building = &AppScreen{}
		return true
	}
	st.building.add(line)
	st.screen = st.building.copy()
	st.gen++
	if now.Sub(st.lastDraw) >= appRedrawGap {
		st.lastDraw = now
		s.changedLocked()
	} else if !st.drawSoon {
		st.drawSoon = true
		time.AfterFunc(appRedrawGap, func() {
			s.mu.Lock()
			st.drawSoon = false
			st.lastDraw = s.now()
			s.changedLocked()
			s.mu.Unlock()
		})
	}
	return true
}

// readAppLine は、1 行を読む（appMaxLineBytes より長い行は切る）。
func readAppLine(r *bufio.Reader) (string, error) {
	var b []byte
	for {
		part, isPrefix, err := r.ReadLine()
		if len(b) < appMaxLineBytes {
			b = append(b, part[:min(len(part), appMaxLineBytes-len(b))]...)
		}
		if err != nil {
			return string(b), err
		}
		if !isPrefix {
			return string(b), nil
		}
	}
}

// setNice は、スクリプトの優先度を下げる（入力と描画を先にする）。
func setNice(pid int) {
	syscall.Setpriority(syscall.PRIO_PROCESS, pid, appNice)
}

// appUlimit は、/bin/sh で上限を掛けてから exec する前置き。
func appUlimit(def *AppDef, stream bool, nproc int) string {
	cpu := "unlimited"
	if !stream {
		cpu = strconv.Itoa(int(def.Timeout/time.Second) + 1)
	}
	return fmt.Sprintf(`ulimit -v %d && ulimit -t %s && ulimit -f %d && ulimit -n 64 && { ulimit -p %d 2>/dev/null || ulimit -u %d 2>/dev/null || true; } && exec "$@"`,
		def.MemMB*1024, cpu, appMaxDataBytes/512, nproc, nproc)
}

// appProcExtra は、スクリプトが増やしてよいプロセスの数。
const appProcExtra = 32

// userProcs は、uid のスレッドの数（RLIMIT_NPROC と同じくスレッドも数える。/proc の持ち主で選ぶ）。
func userProcs(uid int) int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if c := e.Name()[0]; c < '0' || c > '9' {
			continue
		}
		if fi, err := os.Stat("/proc/" + e.Name()); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid {
				n++
				if b, err := os.ReadFile("/proc/" + e.Name() + "/status"); err == nil {
					if _, rest, ok := strings.Cut(string(b), "\nThreads:"); ok {
						f := strings.Fields(rest)
						if len(f) > 0 {
							if k, err := strconv.Atoi(f[0]); err == nil && k > 1 {
								n += k - 1
							}
						}
					}
				}
			}
		}
	}
	return n
}

// appInterpreter は、スクリプトの 1 行目の #! を読む（なければ /bin/sh）。絶対パスで、あるものだけを使う。
func appInterpreter(script string) ([]string, error) {
	f, err := os.Open(script)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, 128)
	n, _ := io.ReadFull(f, b)
	first, _, _ := strings.Cut(string(b[:n]), "\n")
	if !strings.HasPrefix(first, "#!") {
		return []string{"/bin/sh"}, nil
	}
	fs := strings.Fields(strings.TrimPrefix(first, "#!"))
	if len(fs) == 0 || len(fs) > 2 || !filepath.IsAbs(fs[0]) {
		return nil, fmt.Errorf("the first line of %s must be #!/absolute/path [one argument]", filepath.Base(script))
	}
	if fs[0] == "/usr/bin/env" && len(fs) == 2 {
		p, err := exec.LookPath(fs[1])
		if err != nil {
			return nil, fmt.Errorf("%s is not installed on the Brain", fs[1])
		}
		return []string{p}, nil
	}
	if _, err := os.Stat(fs[0]); err != nil {
		return nil, fmt.Errorf("%s is not installed on the Brain", fs[0])
	}
	return fs, nil
}

// command は、スクリプトを user の権限と上限で動かすコマンドを作る（まだ起動しない）。
func (s *AppService) command(def *AppDef, args []string, stream bool) (*exec.Cmd, error) {
	script := filepath.Join(def.Dir, def.Run)
	fi, err := os.Lstat(script)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a plain file", def.Run)
	}
	interp, err := appInterpreter(script)
	if err != nil {
		return nil, err
	}
	data := filepath.Join(s.store.dataDir, def.Name)
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "BRAIN_APP=" + def.Name, "BRAIN_APP_DIR=" + def.Dir,
		"BRAIN_APP_DATA=" + data, "BRAIN_EVENT=" + strings.Join(args, " ")}
	attr := &syscall.SysProcAttr{Setpgid: true}
	uid, gid := -1, -1
	if !s.runAsMe {
		name := defaultBrainUser
		if s.userName != nil {
			name = s.userName()
		}
		u, err := user.Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("user %q: %v", name, err)
		}
		uid, _ = strconv.Atoi(u.Uid)
		gid, _ = strconv.Atoi(u.Gid)
		if uid == 0 {
			return nil, errors.New("apps never run as root (terminal.brain_user)")
		}
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		env = append(env, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
	} else {
		env = append(env, "HOME="+data)
	}
	pu := uid
	if pu < 0 {
		pu = os.Getuid()
	}
	argv := append([]string{"/bin/sh", "-c", appUlimit(def, stream, userProcs(pu)+appProcExtra), "brain-app"}, interp...)
	argv = append(argv, script)
	argv = append(argv, args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	if uid >= 0 {
		chownTree(data, uid, gid) // 復元したあとは root のものになっているので
	}
	cmd.Dir = data
	cmd.Env = env
	cmd.SysProcAttr = attr
	return cmd, nil
}

// tailBuffer は、最後の max バイトだけを覚える io.Writer。
type tailBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.b...)
}

// AppView は、描くときの写し（1 つのアプリ）。
type AppView struct {
	Def     *AppDef
	Bad     string // 使えない理由
	Screen  *AppScreen
	Status  string
	Level   int
	Running bool
	Gen     uint64
	LastRun time.Time
}

// View は、名前のアプリの写し（描画の goroutine から）。
func (s *AppService) View(name string) *AppView {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(name)
	if st == nil {
		_, bad := s.store.Get(name)
		return &AppView{Bad: bad}
	}
	return &AppView{Def: st.def, Screen: st.screen, Status: st.status, Level: st.level, Running: st.running, Gen: st.gen, LastRun: st.lastRun}
}

// Info は、list_apps の答え（置いてあるものと、動きの様子）。
func (s *AppService) Info() []AppInfo {
	list := s.store.List()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range list {
		if st := s.states[list[i].Name]; st != nil {
			switch {
			case st.running:
				list[i].Status = "動いています"
			case st.status != "":
				list[i].Status = st.status
			}
			if !st.lastRun.IsZero() {
				list[i].LastRun = st.lastRun.Format(time.RFC3339)
			}
		}
	}
	return list
}

// openInput は、@input のボタンを押したとき、文字を入れる欄を開く。
func (s *AppService) openInput(name, id, title string) {
	s.mu.Lock()
	f := s.input
	s.mu.Unlock()
	if f != nil {
		f(appInputKind(name, id), title, "")
	}
}

// appInputKind は、文字を入れる欄の種類（prompt.go の kind）。app:<名前>:<id>。
func appInputKind(name, id string) string { return "app:" + name + ":" + id }

// parseAppInputKind は、appInputKind の逆。
func parseAppInputKind(kind string) (name, id string, ok bool) {
	rest, ok := strings.CutPrefix(kind, "app:")
	if !ok {
		return "", "", false
	}
	name, id, ok = strings.Cut(rest, ":")
	return name, id, ok
}

// chownTree は、dir の持ち主が uid でなければ、中のものも含めて uid と gid にする（リンクはたどらない）。
func chownTree(dir string, uid, gid int) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid {
		return
	}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			os.Lchown(p, uid, gid)
		}
		return nil
	})
}
