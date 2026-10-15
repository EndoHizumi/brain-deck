package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ---------- run（Brain でコマンドを動かす。ssh を使わない） ----------
//
//	brain-deck run [--limit 1m] -- <コマンド> [引数...]
//
// Brain の lefthand が、ふつうのユーザー（user）でコマンドを動かし、標準出力と標準エラーを返す（それぞれ 64 KiB まで）。
// シェルは通さないので、パイプなどは `sh -c '...'` で書く。root が要るときは sudo を付ける。
// 動かしたコマンドは Brain のジャーナル（brain-deck logs、journalctl -u lefthand）に残る。
// 終了コードはコマンドのもの（時間切れは 124、見つからないは 127）。brain-deck 自身の失敗は、いつもの終了コード。

const (
	runDefaultLimit = time.Minute
	runMaxLimit     = 10 * time.Minute
)

func runRun(o *options, stdout, stderr io.Writer) int {
	if o.dashAt != 1 || len(o.args) < 2 {
		return report(stderr, usageError("コマンドの前に -- を書きます（brain-deck run -- uptime、brain-deck run -- sh -c 'ip -br addr'）"))
	}
	argv := o.args[1:]
	limit := o.limit
	if limit == 0 {
		limit = runDefaultLimit
	}
	if limit > runMaxLimit {
		return report(stderr, usageError("--limit は 10m までです"))
	}
	s, err := openSession(o, stderr, limit+30*time.Second)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	if err := need(s.c, "run"); err != nil {
		return report(stderr, err)
	}
	raw, err := s.call("run", map[string]any{"argv": argv, "limit_ms": limit.Milliseconds()}, limit+15*time.Second)
	if err != nil {
		return report(stderr, err)
	}
	var r struct {
		Exit      int    `json:"exit"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		Truncated bool   `json:"truncated"`
		TimedOut  bool   `json:"timed_out"`
		User      string `json:"user"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return report(stderr, err)
	}
	io.WriteString(stdout, r.Stdout)
	io.WriteString(stderr, r.Stderr)
	if r.Truncated {
		fmt.Fprintln(stderr, "brain-deck: 出力が 64 KiB を超えたので、そこまでにしました")
	}
	if r.TimedOut {
		fmt.Fprintf(stderr, "brain-deck: 時間切れ（%v）で止めました。長いコマンドは --limit（10m まで）\n", limit)
	}
	return r.Exit
}
