package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- exec（コマンドのショートカットの結果をタイルに出す） ----------
//
// PC の OS のショートカット（F13〜F18 など。Brain のセルが送る）から呼ばれる。
//
//	brain-deck exec --result build -- make -j4
//
//  1. 同じ id で実行中なら、何もせずに終わる（終了コード 75）。id ごとのロックファイルの flock で分かる
//  2. タイルに「実行中」を出す
//  3. コマンドを実行する。出力は、そのまま brain-deck の標準出力と標準エラーにも出す（スクリプトのログに残る）
//  4. 終わったら、成功（終了コード 0）は ok、失敗は error の色で、時刻、かかった時間、出力の最後の行を出す
//
// Brain がない、設定 GUI が接続中などでタイルに書けなくても、コマンドは実行し、コマンドの終了コードを返す。
// タイルに書くあいだだけポートを開く（コマンドの中で brain-deck text を使ってもよい）。

const (
	execDefaultLimit = 10 * time.Minute
	execExitTimeout  = 124 // timeout(1) と同じ
	execExitBusy     = 75  // EX_TEMPFAIL：同じ id で実行中
	execTextTimeout  = 3 * time.Second
	execKillGrace    = 5 * time.Second // 時間切れで SIGTERM を送ってから SIGKILL まで
	execTailBytes    = 4096
	execLineMax      = 120 // タイルに出す最後の行の長さ（文字）
)

func runExec(o *options, stdout, stderr io.Writer) int {
	if o.result == "" {
		return report(stderr, usageError("exec には --result <id>（結果を出すテキストのタイル）が要ります"))
	}
	if !validTileID(o.result) {
		return report(stderr, usageError("--result の id は、英数字と _ . - の 32 文字までです"))
	}
	if o.dashAt != 1 || len(o.args) < 2 {
		return report(stderr, usageError("コマンドの前に -- を書きます（brain-deck exec --result build -- make -j4）"))
	}
	argv := o.args[1:]
	limit := o.limit
	if limit == 0 {
		limit = execDefaultLimit
	}

	unlock, ok := execLock(o.result)
	if !ok {
		fmt.Fprintf(stderr, "brain-deck: %s はまだ実行中です。重ねて実行しません\n", o.result)
		return execExitBusy
	}
	defer unlock()

	name := filepath.Base(argv[0])
	start := time.Now()
	tileText(o, stderr, fmt.Sprintf("実行中… %s\n%s", start.Format("15:04"), clip(strings.Join(append([]string{name}, argv[1:]...), " "), execLineMax)), "normal")

	tail := &tailWriter{}
	code, timedOut, runErr := runCommandTee(argv, limit, io.MultiWriter(stdout, tail), io.MultiWriter(stderr, tail))
	took := time.Since(start)

	style, head := "ok", "成功"
	switch {
	case timedOut:
		style, head = "error", fmt.Sprintf("失敗 時間切れ（%v）", limit)
		fmt.Fprintf(stderr, "brain-deck: %s が %v で終わらなかったので止めました\n", name, limit)
	case runErr != nil:
		style, head = "error", "失敗 実行できません"
		fmt.Fprintf(stderr, "brain-deck: %v\n", runErr)
	case code != 0:
		style, head = "error", fmt.Sprintf("失敗 終了コード %d", code)
	}
	last := tail.lastLine()
	if runErr != nil {
		last = runErr.Error()
	}
	text := fmt.Sprintf("%s %s（%s）", head, time.Now().Format("15:04"), shortDuration(took))
	if last != "" {
		text += "\n" + clip(last, execLineMax)
	}
	tileText(o, stderr, text, style)
	return code
}

// validTileID は、テキストのタイルの id として使えるか（lefthand の validTextID と同じ）。
var tileIDRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

func validTileID(s string) bool { return tileIDRE.MatchString(s) }

// execLock は、id ごとのロックファイルを、待たずに取る。取れなければ false（同じ id で実行中）。
func execLock(id string) (func(), bool) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	p := filepath.Join(dir, fmt.Sprintf("brain-deck-exec-%d-%s.lock", os.Getuid(), id))
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return func() {}, true // ロックを作れない環境でも、コマンドは実行する
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true
}

// tileText は、タイルにテキストを書く。書けなくても（Brain がない、設定 GUI が接続中）、注意を出すだけ。
func tileText(o *options, stderr io.Writer, text, style string) {
	t := o.timeout
	if t == defaultTimeout {
		t = execTextTimeout
	}
	deadline := time.Now().Add(t)
	c, unlock, err := connect(connectOptions{port: o.port, timeout: t, verbose: o.verbose, deadline: deadline})
	if err == nil {
		defer unlock()
		defer c.Close()
		params := map[string]any{"name": o.result, "text": text, "style": style, "source": "brain-deck exec"}
		if o.ttl > 0 {
			params["ttl_sec"] = o.ttl.Seconds()
		}
		callTimeout = func() time.Duration { return max(time.Until(deadline), 100*time.Millisecond) }
		if err = need(c, "set_text"); err == nil {
			if o.ttl > 0 && !o.noTimeSync {
				err = syncIfNeeded(c)
			}
			if err == nil {
				_, err = c.Call("set_text", params, callTimeout())
			}
		}
	}
	if err != nil && !o.quiet {
		msg := err.Error()
		var be *busyError
		switch {
		case errors.As(err, &be):
			msg = "設定 GUI が接続中"
		case errors.Is(err, errNotFound):
			msg = "Brain が見つかりません"
		case errors.Is(err, errNoReply):
			msg = "Brain から返事がありません"
		}
		fmt.Fprintf(stderr, "brain-deck: タイル %s に書けませんでした（%s）。コマンドは実行します\n", o.result, msg)
	}
}

// runCommandTee は argv を実行する。limit を過ぎたら、プロセスのグループに SIGTERM、5 秒後に SIGKILL を送る。
// brain-deck が SIGINT や SIGTERM を受けたら、コマンドにも送る。
func runCommandTee(argv []string, limit time.Duration, stdout, stderr io.Writer) (code int, timedOut bool, err error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		code = 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		return code, false, err
	}
	pgid := cmd.Process.Pid
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	var kill <-chan time.Time
	for {
		select {
		case werr := <-done:
			if timedOut {
				return execExitTimeout, true, nil
			}
			return exitCodeOf(werr), false, nil
		case s := <-sigs:
			syscall.Kill(-pgid, s.(syscall.Signal))
		case <-timer.C:
			timedOut = true
			syscall.Kill(-pgid, syscall.SIGTERM)
			kill = time.After(execKillGrace)
		case <-kill:
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

// tailWriter は、書かれたものの最後の部分を覚える（標準出力と標準エラーをまとめて）。
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > execTailBytes {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-execTailBytes:]...)
	}
	return len(p), nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// lastLine は、空でない最後の行を返す。色などのエスケープシーケンスを除き、\r で書き直した行は最後のものにする。
func (t *tailWriter) lastLine() string {
	t.mu.Lock()
	s := string(t.buf)
	t.mu.Unlock()
	s = strings.ToValidUTF8(ansiRE.ReplaceAllString(s, ""), "")
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if j := strings.LastIndex(strings.TrimRight(l, "\r"), "\r"); j >= 0 {
			l = l[j+1:]
		}
		l = strings.Map(func(r rune) rune {
			switch {
			case r == '\t':
				return ' '
			case r < 0x20 || r == 0x7f:
				return -1
			}
			return r
		}, l)
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1f秒", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%d分%02d秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d時間%02d分", int(d.Hours()), int(d.Minutes())%60)
}
