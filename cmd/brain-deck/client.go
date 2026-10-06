package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// ---------- Brain のデーモン（lefthand）との通信 ----------
//
// 形式は docs/protocol.md。1 行に 1 つの JSON。設定 GUI と同じ。

// 接続の失敗の種類。終了コードを決めるのに使う
var (
	errNotFound   = errors.New("brain not found")
	errNoReply    = errors.New("no reply")
	errPermission = errors.New("permission denied")
)

// busyError は、ほかのプロセスがポートを使っていること。
type busyError struct {
	port    string
	holders []string // 分かれば、開いているプロセス
	deck    bool     // ほかの brain-deck が終わらなかった
}

func (e *busyError) Error() string { return "busy: " + e.port }

// brainError は、Brain のデーモンがエラーを返したこと。
type brainError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *brainError) Error() string { return e.Code + ": " + e.Message }

// Client は、開いたポート 1 つでの通信。
type Client struct {
	f       *os.File
	Port    string
	rd      *bufio.Reader
	next    int
	verbose bool
	Hello   helloResult
}

type helloResult struct {
	Protocol int      `json:"protocol"`
	Daemon   string   `json:"daemon"`
	Version  string   `json:"version"`
	MaxLine  int      `json:"max_line"`
	Commands []string `json:"commands"`
}

func (h helloResult) has(cmd string) bool {
	for _, c := range h.Commands {
		if c == cmd {
			return true
		}
	}
	return false
}

func (c *Client) Close() error { return closePort(c.f) }

func (c *Client) logf(format string, args ...any) {
	if c.verbose {
		fmt.Fprintf(os.Stderr, "brain-deck: "+format+"\n", args...)
	}
}

// Call はリクエストを送り、同じ id のレスポンスの result を返す。timeout までに返事がなければ errNoReply。
// 途中に届く通知や、ほかの id の返事（前の接続の残り）は読み捨てる。
func (c *Client) Call(cmd string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	c.next++
	id := fmt.Sprintf("bd-%d-%d", os.Getpid(), c.next)
	req := map[string]any{"id": id, "cmd": cmd}
	for k, v := range params {
		req[k] = v
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	c.f.SetWriteDeadline(deadline)
	c.logf("%s → %s", c.Port, b)
	if _, err := c.f.Write(append(b, '\n')); err != nil {
		return nil, ioErr(err)
	}
	c.f.SetReadDeadline(deadline)
	for {
		line, err := c.rd.ReadBytes('\n')
		if err != nil {
			return nil, ioErr(err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
			Error  *brainError     `json:"error"`
		}
		if json.Unmarshal(line, &resp) != nil || string(resp.ID) != `"`+id+`"` {
			c.logf("%s ← (ignored) %.200s", c.Port, line)
			continue
		}
		c.logf("%s ← %.400s", c.Port, line)
		if !resp.OK {
			if resp.Error == nil {
				resp.Error = &brainError{Code: "unknown", Message: string(line)}
			}
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// ioErr は、読み書きの失敗を、返事がないこととして扱う（タイムアウト、切断）。
func ioErr(err error) error {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("%w (timeout)", errNoReply)
	case errors.Is(err, io.EOF), errors.Is(err, syscall.EIO), errors.Is(err, syscall.ENXIO):
		return fmt.Errorf("%w (disconnected: %v)", errNoReply, err)
	}
	return fmt.Errorf("%w (%v)", errNoReply, err)
}

// connectOptions は接続のしかた。
type connectOptions struct {
	port     string        // 空なら探す
	timeout  time.Duration // 接続から終わりまでの全体の時間
	verbose  bool
	deadline time.Time
}

// helloTimeout は、1 つのポートで hello の返事を待つ時間。
const helloTimeout = 1500 * time.Millisecond

// retryDelay は、つながらなかったときに開き直すまでの時間。
// デーモンを再起動した直後、最初に開いたときに一度だけ切断扱いになることがあるため、1 回だけ開き直す。
const retryDelay = 300 * time.Millisecond

// connect は Brain の設定用のポートを探して開き、hello を交わす。
func connect(o connectOptions) (*Client, func(), error) {
	unlock, err := acquireLock(o.deadline)
	if err != nil {
		return nil, nil, &busyError{port: lockPath(), deck: true}
	}
	cands := []string{o.port}
	if o.port == "" {
		cands = candidatePorts()
		if len(cands) == 0 {
			unlock()
			return nil, nil, errNotFound
		}
	}
	var lastErr error = errNoReply
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if time.Until(o.deadline) < retryDelay+200*time.Millisecond {
				break
			}
			if o.verbose {
				fmt.Fprintf(os.Stderr, "brain-deck: no reply, reopening (%v)\n", lastErr)
			}
			time.Sleep(retryDelay)
		}
		for _, p := range cands {
			c, err := tryPort(p, o)
			if err == nil {
				return c, unlock, nil
			}
			var be *busyError
			switch {
			case errors.As(err, &be), errors.Is(err, errPermission):
				// ほかのプロセスが使っているときは、残りのポート（コンソール用）には書かない
				unlock()
				return nil, nil, err
			}
			lastErr = err
			if time.Until(o.deadline) <= 0 {
				break
			}
		}
	}
	unlock()
	return nil, nil, lastErr
}

func tryPort(p string, o connectOptions) (*Client, error) {
	f, err := openPort(p)
	switch {
	case errors.Is(err, errBusy):
		return nil, &busyError{port: p, holders: holders(p)}
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return nil, fmt.Errorf("%s: %w", p, errPermission)
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.ENODEV):
		return nil, fmt.Errorf("%s: %w (%v)", p, errNotFound, err)
	case err != nil:
		return nil, fmt.Errorf("%w (%v)", errNoReply, err)
	}
	// TIOCEXCL を使わないプログラムや root のプログラムが開いていれば、通信が混ざるので使わない
	if hs := holders(p); len(hs) > 0 {
		closePort(f)
		return nil, &busyError{port: p, holders: hs}
	}
	c := &Client{f: f, Port: p, rd: bufio.NewReaderSize(f, 1<<20), verbose: o.verbose}
	// 前の接続の切れ端が Brain 側に残っていても、新しいリクエストとつながらないよう、まず空行を送る
	f.SetWriteDeadline(time.Now().Add(helloTimeout))
	if _, err := f.Write([]byte("\n")); err != nil {
		closePort(f)
		return nil, ioErr(err)
	}
	wait := min(helloTimeout, max(time.Until(o.deadline), 100*time.Millisecond))
	raw, err := c.Call("hello", map[string]any{"client": "brain-deck/" + versionString()}, wait)
	if err == nil {
		err = json.Unmarshal(raw, &c.Hello)
	}
	if err == nil && c.Hello.Daemon != "lefthand" {
		err = fmt.Errorf("%w (%s answered as %q)", errNoReply, p, c.Hello.Daemon)
	}
	if err != nil {
		closePort(f)
		var be *brainError
		if errors.As(err, &be) {
			return nil, fmt.Errorf("%w (%s: hello failed: %v)", errNoReply, p, err)
		}
		return nil, err
	}
	return c, nil
}

// describeBusy は、ほかのプロセスが使っているときの説明。
func describeBusy(e *busyError) string {
	if e.deck {
		return "ほかの brain-deck が実行中で、終わるのを待てませんでした（" + e.port + "）"
	}
	msg := "設定 GUI が接続中です（ほかのプログラムが " + e.port + " を開いています）"
	if len(e.holders) > 0 {
		msg = "設定 GUI が接続中です（" + strings.Join(e.holders, "、") + " が " + e.port + " を開いています）"
	}
	return msg + "\n設定 GUI で「切断」するか、タブを閉じてから、もう一度実行してください"
}
