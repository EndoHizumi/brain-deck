//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"text", "build", "ビルド 成功", "--style", "ok", "--ttl=10m", "-q", "--port", "/dev/x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(o.args, "|") != "text|build|ビルド 成功" || o.style != "ok" || o.ttl != 10*time.Minute || !o.quiet || o.port != "/dev/x" {
		t.Errorf("options = %+v", o)
	}
	o, _ = parseArgs([]string{"text", "x", "--", "--not-a-flag"})
	if strings.Join(o.args, "|") != "text|x|--not-a-flag" {
		t.Errorf("after -- : %q", o.args)
	}
	for _, bad := range [][]string{{"--style"}, {"--nope"}, {"--quiet=1"}, {"--ttl", "-1m"}, {"--ttl", "soon"}, {"--timeout", "0s"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if d, err := parseTTL("1.5d"); err != nil || d != 36*time.Hour {
		t.Errorf("1.5d = %v %v", d, err)
	}
}

func TestUsageExitCodes(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"time"}, {"text"}, {"text", "build"}, {"text", "build", "x", "--clear"}, {"status", "x"}} {
		var out, errb bytes.Buffer
		if code := run(args, nil, &out, &errb); code != exitUsage {
			t.Errorf("%q: exit %d, want %d (%s)", args, code, exitUsage, errb.String())
		}
	}
	var out bytes.Buffer
	if code := run([]string{"--help"}, nil, &out, &out); code != exitOK || !strings.Contains(out.String(), "終了コード") {
		t.Errorf("--help: %d %s", code, out.String())
	}
}

func TestReportExitCodes(t *testing.T) {
	cases := map[error]int{
		&busyError{port: "/dev/x", holders: []string{"chromium (pid 1)"}}: exitBusy,
		&busyError{port: "/tmp/l", deck: true}:                            exitBusy,
		fmt.Errorf("x: %w", errPermission):                                exitPermission,
		errNotFound:                                                       exitNoBrain,
		fmt.Errorf("%w (timeout)", errNoReply):                            exitNoBrain,
		&brainError{Code: "bad_request", Message: "m"}:                    exitBrainError,
		fmt.Errorf("other"):                                               exitInternal,
	}
	for err, want := range cases {
		var b bytes.Buffer
		if got := report(&b, err); got != want {
			t.Errorf("%v: exit %d, want %d", err, got, want)
		}
	}
	var b bytes.Buffer
	report(&b, &busyError{port: "/dev/x", holders: []string{"chromium (pid 1)"}})
	if !strings.Contains(b.String(), "設定 GUI が接続中です") || !strings.Contains(b.String(), "chromium (pid 1)") {
		t.Errorf("busy message: %s", b.String())
	}
}

// ---------- PTY で、デーモンの代わりをする ----------

// fakeBrain は PTY のマスター側で、lefthand の代わりに返事をする。
type fakeBrain struct {
	master  *os.File
	slave   string
	got     chan map[string]any
	ignore  int // 最初のこの数の hello に答えない（デーモンの再起動直後を真似る）
	todo    todoList
	removed int
	cals    any // set_calendar で受け取った calendars
}

func newFakeBrain(t *testing.T) *fakeBrain {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	var n uint32
	unlock := int32(0)
	rc, _ := master.SyscallConn()
	rc.Control(func(fd uintptr) {
		ioctl(int(fd), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
		ioctl(int(fd), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n)))
	})
	f := &fakeBrain{master: master, slave: fmt.Sprintf("/dev/pts/%d", n), got: make(chan map[string]any, 32)}
	t.Cleanup(func() { master.Close() })
	return f
}

func (f *fakeBrain) serve(commands []string) {
	rd := bufio.NewReader(f.master)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			// スレーブを全部閉じたあいだは EIO になる。開き直されるのを待つ
			if errors.Is(err, os.ErrClosed) {
				return
			}
			time.Sleep(20 * time.Millisecond)
			rd.Reset(f.master)
			continue
		}
		var req map[string]any
		if json.Unmarshal(bytes.TrimSpace(line), &req) != nil {
			continue
		}
		f.got <- req
		var res any
		switch req["cmd"] {
		case "hello":
			if f.ignore > 0 {
				f.ignore--
				continue
			}
			res = map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "commands": commands}
		case "get_status":
			res = map[string]any{"time": map[string]any{"now": time.Now().Format(time.RFC3339Nano), "synced": true}}
		case "set_text":
			if req["style"] == "pink" {
				b, _ := json.Marshal(map[string]any{"id": req["id"], "ok": false, "error": map[string]any{"code": "bad_request", "message": "unknown style"}})
				f.master.Write(append(b, '\n'))
				continue
			}
			res = map[string]any{"name": req["name"], "cleared": false, "shown": true}
		case "get_todo", "todo_add", "todo_update", "todo_delete", "todo_clear_done":
			if code := f.todoCmd(req); code != "" {
				b, _ := json.Marshal(map[string]any{"id": req["id"], "ok": false, "error": map[string]any{"code": code, "message": code}})
				f.master.Write(append(b, '\n'))
				continue
			}
			res = f.todo
			if req["cmd"] == "todo_clear_done" {
				res = map[string]any{"rev": f.todo.Rev, "items": f.todo.Items, "removed": f.removed}
			}
		case "set_calendar":
			f.cals = req["calendars"]
			res = map[string]any{"rev": 1, "shown": true}
		case "get_calendar":
			res = map[string]any{"rev": 1, "shown": true, "calendars": f.cals}
		default:
			res = map[string]any{}
		}
		// 通知と、ほかの id の返事が混ざっても読み飛ばす
		f.master.Write([]byte(`{"event":"layer","layer":"base"}` + "\n" + `{"id":null,"ok":false,"error":{"code":"incomplete_line","message":"x"}}` + "\n"))
		b, _ := json.Marshal(map[string]any{"id": req["id"], "ok": true, "result": res})
		f.master.Write(append(b, '\n'))
	}
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader("標準入力の\nテキスト\n"), &out, &errb)
	return code, out.String(), errb.String()
}

func TestTextAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	go f.serve([]string{"hello", "get_status", "set_time", "set_text", "get_text"})
	code, out, errs := runCLI(t, "--port", f.slave, "text", "build", "ビルド成功", "--style", "ok", "--ttl", "10m")
	if code != exitOK || !strings.Contains(out, "build: ビルド成功") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	var setText map[string]any
	for len(f.got) > 0 {
		if r := <-f.got; r["cmd"] == "set_text" {
			setText = r
		}
	}
	if setText["name"] != "build" || setText["text"] != "ビルド成功" || setText["style"] != "ok" || setText["ttl_sec"] != float64(600) {
		t.Errorf("set_text = %v", setText)
	}
	code, _, _ = runCLI(t, "--port", f.slave, "text", "build", "-")
	for len(f.got) > 0 {
		if r := <-f.got; r["cmd"] == "set_text" && r["text"] != "標準入力の\nテキスト" {
			t.Errorf("stdin text = %q", r["text"])
		}
	}
	if code != exitOK {
		t.Errorf("stdin: exit %d", code)
	}
	if code, _, errs := runCLI(t, "--port", f.slave, "text", "build", "x", "--style", "pink"); code != exitBrainError || !strings.Contains(errs, "unknown style") {
		t.Errorf("brain error: exit %d %s", code, errs)
	}
}

func TestOldDaemon(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	go f.serve([]string{"hello", "get_status", "set_time"})
	if code, _, errs := runCLI(t, "--port", f.slave, "text", "build", "x"); code != exitBrainError || !strings.Contains(errs, "新しく") {
		t.Errorf("exit %d %s", code, errs)
	}
}

// デーモンの再起動直後に、最初の hello に返事がないことがある。開き直して、もう一度試す
func TestRetryAfterNoReply(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	f.ignore = 1
	go f.serve([]string{"hello", "get_status"})
	start := time.Now()
	code, _, errs := runCLI(t, "--port", f.slave, "status")
	if code != exitOK {
		t.Fatalf("exit %d %s", code, errs)
	}
	if d := time.Since(start); d < helloTimeout {
		t.Errorf("should have retried after %v, took %v", helloTimeout, d)
	}
}

func TestNoReplyTimesOut(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	f.ignore = 100
	go f.serve(nil)
	start := time.Now()
	code, _, errs := runCLI(t, "--port", f.slave, "--timeout", "2s", "status")
	if code != exitNoBrain || !strings.Contains(errs, "返事がありません") {
		t.Errorf("exit %d %s", code, errs)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v with --timeout 2s", d)
	}
}

// ほかのプロセスが TIOCEXCL で開いている（Chrome の WebSerial と同じ）と、すぐに「接続中」で終わる
func TestBusyWhenExclusive(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores TIOCEXCL")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	go f.serve([]string{"hello"})
	g, err := openPort(f.slave)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	start := time.Now()
	code, _, errs := runCLI(t, "--port", f.slave, "status")
	if code != exitBusy || !strings.Contains(errs, "設定 GUI が接続中です") {
		t.Errorf("exit %d %s", code, errs)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("busy should fail fast, took %v", d)
	}
}

// TIOCEXCL を使わないプログラムが開いているときも、通信が混ざらないよう使わない（Linux）
func TestBusyWhenOpenedWithoutExcl(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	go f.serve([]string{"hello"})
	// 自分のプロセスは holders から除くので、子プロセスに開かせる
	p, err := os.StartProcess("/bin/sleep", []string{"sleep", "5"}, &os.ProcAttr{Files: []*os.File{nil, nil, nil, mustOpen(t, f.slave)}})
	if err != nil {
		t.Skip(err)
	}
	defer p.Kill()
	time.Sleep(100 * time.Millisecond)
	code, _, errs := runCLI(t, "--port", f.slave, "status")
	if code != exitBusy || !strings.Contains(errs, "sleep") {
		t.Errorf("exit %d %s", code, errs)
	}
}

func mustOpen(t *testing.T, p string) *os.File {
	f, err := os.OpenFile(p, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestNotFound(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if code, _, errs := runCLI(t, "--port", "/dev/no-such-brain", "status"); code != exitNoBrain || !strings.Contains(errs, "見つかりません") {
		t.Errorf("exit %d %s", code, errs)
	}
}

// todoCmd は、lefthand の Todo のコマンドを簡単に真似る。誤りならエラーの種類を返す。
func (f *fakeBrain) todoCmd(req map[string]any) string {
	l := &f.todo
	find := func() int {
		for i, it := range l.Items {
			if it.ID == req["item"] {
				if rev, ok := req["rev"].(float64); ok && uint64(rev) != it.Rev {
					return -2
				}
				return i
			}
		}
		return -1
	}
	switch req["cmd"] {
	case "get_todo":
		return ""
	case "todo_add":
		l.Rev++
		it := todoItem{ID: fmt.Sprintf("t%d", l.Rev), Text: req["text"].(string), Rev: l.Rev, Source: fmt.Sprint(req["source"])}
		if i, ok := req["index"].(float64); ok {
			l.Items = append(l.Items[:int(i)], append([]todoItem{it}, l.Items[int(i):]...)...)
		} else {
			l.Items = append(l.Items, it)
		}
	case "todo_update", "todo_delete":
		i := find()
		switch i {
		case -1:
			return "not_found"
		case -2:
			return "conflict"
		}
		l.Rev++
		if req["cmd"] == "todo_delete" {
			l.Items = append(l.Items[:i], l.Items[i+1:]...)
			return ""
		}
		if d, ok := req["done"].(bool); ok {
			l.Items[i].Done = d
		}
		if t, ok := req["text"].(string); ok {
			l.Items[i].Text = t
		}
		l.Items[i].Rev = l.Rev
	case "todo_clear_done":
		var kept []todoItem
		for _, it := range l.Items {
			if !it.Done {
				kept = append(kept, it)
			}
		}
		f.removed = len(l.Items) - len(kept)
		l.Items = kept
		l.Rev++
	}
	return ""
}

func TestTodoAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	go f.serve([]string{"hello", "get_status", "get_todo", "todo_add", "todo_update", "todo_delete", "todo_clear_done"})
	cli := func(want int, args ...string) string {
		t.Helper()
		code, out, errs := runCLI(t, append([]string{"--port", f.slave, "todo"}, args...)...)
		if code != want {
			t.Fatalf("todo %q: exit %d, want %d: %s %s", args, code, want, out, errs)
		}
		return out + errs
	}
	if out := cli(exitOK, "add", "牛乳を買う"); !strings.Contains(out, "追加しました：牛乳を買う（未完了 1 件、完了 0 件）") {
		t.Errorf("add: %s", out)
	}
	cli(exitOK, "add", "-") // 標準入力の 2 行
	cli(exitOK, "add", "急ぎ", "--top")
	if out := cli(exitOK, "list"); !strings.Contains(out, "  1 [ ] 急ぎ\n  2 [ ] 牛乳を買う\n  3 [ ] 標準入力の\n  4 [ ] テキスト\n未完了 4 件") {
		t.Errorf("list: %s", out)
	}
	// 番号は画面の順（完了は下）
	cli(exitOK, "done", "2", "1")
	if out := cli(exitOK); !strings.Contains(out, "  1 [ ] 標準入力の\n  2 [ ] テキスト\n  3 [x] 急ぎ\n  4 [x] 牛乳を買う") {
		t.Errorf("after done: %s", out)
	}
	cli(exitOK, "undo", "4") // 牛乳を買う
	cli(exitOK, "edit", "1", "牛乳と卵")
	cli(exitOK, "rm", "t2")
	if out := cli(exitOK, "clear-done"); !strings.Contains(out, "1 件消しました（未完了 2 件") {
		t.Errorf("clear-done: %s", out)
	}
	out := cli(exitOK, "list", "--json")
	var l todoList
	if err := json.Unmarshal([]byte(out), &l); err != nil || len(l.Items) != 2 || l.Items[0].Text != "牛乳と卵" || l.Items[1].Text != "テキスト" {
		t.Errorf("json: %v %s", err, out)
	}
	cli(exitUsage, "done", "9")
	cli(exitUsage, "done", "t99")
	cli(exitUsage, "nope")
	cli(exitUsage, "edit", "1")
	cli(exitUsage, "list", "--top")
	if code, _, _ := runCLI(t, "--port", f.slave, "text", "--list", "--json"); code != exitUsage {
		t.Errorf("text --json: exit %d", code)
	}
}

func TestTodoConflictMessage(t *testing.T) {
	var b bytes.Buffer
	if code := report(&b, &brainError{Code: "conflict", Message: "item t1 was changed"}); code != exitBrainError || !strings.Contains(b.String(), "todo list で確かめて") {
		t.Errorf("conflict: %d %s", code, b.String())
	}
}
