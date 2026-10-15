package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------- run（PC の brain-deck run から、Brain でコマンドを動かす） ----------
//
// ssh が使えないとき（USB のネットワークの名前が変わった、PC の設定が壊れたなど）でも、シリアルだけで Brain の
// コマンドを動かせるようにする。root ではなく Brain のふつうのユーザー（user）で動かす（root が要るときは
// コマンドの中で sudo を使う）。動かしたコマンドと結果は、ジャーナル（journalctl -u lefthand）に残す。
// シェルは通さない（argv をそのまま渡す）。シェルの書き方を使うなら `sh -c '...'`。標準入力は /dev/null。
// 時間の上限（既定 1 分、最大 10 分）を過ぎたら、プロセスグループごとに SIGTERM、3 秒後に SIGKILL を送る。
// 出力は標準出力と標準エラーをそれぞれ 64 KiB まで返し、超えた分は捨てる（truncated）。
// 返事を待つあいだ、この接続のほかの要求は待たせる（brain-deck はポートを独り占めしているので困らない）。

const (
	runUser         = "user"
	runDefaultLimit = time.Minute
	runMaxLimit     = 10 * time.Minute
	runOutputMax    = 64 << 10
	runKillGrace    = 3 * time.Second
	runExitTimeout  = 124 // timeout(1) と同じ
	runExitNotFound = 127 // シェルと同じ
)

// runAs は、コマンドを動かすユーザー。デーモンが root でないとき（テスト、PC での試し）は、自分のまま動かす。
var runAs = runUser

// RunResult は run の結果。
type RunResult struct {
	Exit      int    `json:"exit"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	TookMS    int64  `json:"took_ms"`
	User      string `json:"user"`
}

// capWriter は、max バイトまで覚え、あとは捨てる。
type capWriter struct {
	buf bytes.Buffer
	max int
	cut bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room < len(p) {
		w.cut = true
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// runUserCommand は argv を runAs のユーザーで動かす。limit が 0 なら既定、最大を超えたら最大にする。
func runUserCommand(argv []string, limit time.Duration) (RunResult, error) {
	if len(argv) == 0 || argv[0] == "" {
		return RunResult{}, errors.New(`"argv" (the command and its arguments) is required`)
	}
	if limit <= 0 {
		limit = runDefaultLimit
	}
	limit = min(limit, runMaxLimit)
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	who, home := "", "/"
	if u, err := user.Current(); err == nil {
		who, home = u.Username, u.HomeDir
	}
	if os.Geteuid() == 0 && runAs != "" {
		u, err := user.Lookup(runAs)
		if err != nil {
			return RunResult{}, fmt.Errorf("the user %s to run commands as: %v", runAs, err)
		}
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		if gs, err := u.GroupIds(); err == nil {
			for _, g := range gs {
				if n, err := strconv.ParseUint(g, 10, 32); err == nil {
					cred.Groups = append(cred.Groups, uint32(n))
				}
			}
		}
		cmd.SysProcAttr.Credential = cred
		who, home = u.Username, u.HomeDir
	}
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		home = "/"
	}
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "USER=" + who, "LOGNAME=" + who, "SHELL=/bin/bash", "TERM=dumb",
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin", "LANG=C.UTF-8"}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = runKillGrace
	out, errOut := &capWriter{max: runOutputMax}, &capWriter{max: runOutputMax}
	cmd.Stdout, cmd.Stderr = out, errOut

	log.Printf("run: as %s: %s (limit %v)", who, quoteArgv(argv), limit)
	start := time.Now()
	err := cmd.Run()
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // 残った子も止める（時間切れのとき、出力をつないだままのもの）
	}
	r := RunResult{TookMS: time.Since(start).Milliseconds(), User: who}
	var ee *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		r.TimedOut, r.Exit = true, runExitTimeout
	case err == nil:
	case errors.As(err, &ee):
		r.Exit = ee.ExitCode()
		if r.Exit < 0 { // シグナルで止まった
			r.Exit = 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
		}
	case cmd.Process == nil: // 動かせなかった（見つからない、権限がない）
		r.Exit = runExitNotFound
		fmt.Fprintf(errOut, "%v\n", err)
	default:
		r.Exit = 1
		fmt.Fprintf(errOut, "%v\n", err)
	}
	r.Stdout, r.Stderr, r.Truncated = out.buf.String(), errOut.buf.String(), out.cut || errOut.cut
	log.Printf("run: exit %d after %v (stdout %d bytes, stderr %d bytes%s%s)", r.Exit, time.Since(start).Round(time.Millisecond),
		out.buf.Len(), errOut.buf.Len(), map[bool]string{true: ", truncated"}[r.Truncated], map[bool]string{true: ", timed out"}[r.TimedOut])
	return r, nil
}

// quoteArgv は、ジャーナルに書くコマンドの形（空白や記号を含む引数は引用符で囲む）。
func quoteArgv(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`;&|<>()*?[]#~") {
			q[i] = strconv.Quote(a)
		} else {
			q[i] = a
		}
	}
	return strings.Join(q, " ")
}
