package main

import (
	"strings"
	"testing"
	"time"
)

// TestRunUserCommand は、run の結果（出力、終了コード、見つからない、時間切れ、出力の上限）を確かめる。
// テストは root でないので、自分のユーザーで動く。
func TestRunUserCommand(t *testing.T) {
	r, err := runUserCommand([]string{"sh", "-c", "echo out; echo err >&2; pwd; echo $HOME; exit 3"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if r.Exit != 3 || len(lines) != 3 || lines[0] != "out" || lines[1] != lines[2] || r.Stderr != "err\n" || r.User == "" || r.TimedOut {
		t.Fatalf("result %+v", r)
	}
	if r, _ := runUserCommand([]string{"no-such-command-xyz"}, 0); r.Exit != runExitNotFound || !strings.Contains(r.Stderr, "no-such-command-xyz") {
		t.Fatalf("not found %+v", r)
	}
	// 時間切れ：子のプロセスが出力をつないだままでも、上限と猶予のうちに返る
	start := time.Now()
	r, _ = runUserCommand([]string{"sh", "-c", "sleep 30 & sleep 30"}, 300*time.Millisecond)
	if r.Exit != runExitTimeout || !r.TimedOut || time.Since(start) > runKillGrace+2*time.Second {
		t.Fatalf("timeout %+v after %v", r, time.Since(start))
	}
	// 出力は 64 KiB まで
	r, _ = runUserCommand([]string{"sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x"}, 0)
	if len(r.Stdout) != runOutputMax || !r.Truncated || r.Exit != 0 {
		t.Fatalf("truncated: %d bytes %+v", len(r.Stdout), r.Truncated)
	}
	if _, err := runUserCommand(nil, 0); err == nil {
		t.Fatal("empty argv must fail")
	}
	if q := quoteArgv([]string{"ls", "-l", "a b", "$X", ""}); q != `ls -l "a b" "$X" ""` {
		t.Errorf("quote %s", q)
	}
}

func TestControlRun(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	r := result(t, d.call("run", map[string]any{"argv": []string{"echo", "hello"}}))
	if r["exit"] != float64(0) || r["stdout"] != "hello\n" {
		t.Fatalf("run %v", r)
	}
	if m := d.call("run", map[string]any{}); errCode(m) != errBadRequest {
		t.Fatalf("no argv: %v", m)
	}
}
