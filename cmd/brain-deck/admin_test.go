//go:build linux

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUpdater は、Brain の更新の係の代わり。update_apply のあと、版を変え、update_confirm で終わる。
type fakeUpdater struct {
	version   string
	newVer    string
	state     map[string]any
	received  []byte
	fail      bool // 入れ替えに失敗し、前の版に戻したことにする
	confirmed int
	logsCalls int
	config    string
}

func (u *fakeUpdater) handle(req map[string]any) (any, bool) {
	switch req["cmd"] {
	case "hello":
		return map[string]any{"protocol": 1, "daemon": "lefthand", "version": u.version, "upload_kinds": []string{"update", "image"},
			"commands": []string{"hello", "get_status", "set_text", "update_apply", "update_status", "update_confirm", "update_discard",
				"update_rollback", "upload_begin", "restart", "power", "get_logs", "get_config_text", "put_config_text"}}, true
	case "upload_begin":
		u.received = nil
		return map[string]any{"upload": "u1", "chunk_bytes": 1000}, true
	case "upload_chunk":
		b, _ := base64.StdEncoding.DecodeString(req["data"].(string))
		u.received = append(u.received, b...)
		return map[string]any{"received": len(u.received)}, true
	case "upload_end":
		return map[string]any{"stage": "aaaaaaaaaaaa", "action": "update", "version": "v2", "current_version": u.version,
			"bin_changed": true, "has_gadget": true, "gadget_changed": true, "rebind": "yes", "rebind_reasons": []string{"the HID report descriptor differs"}, "warnings": []string{}}, true
	case "update_rollback":
		return map[string]any{"stage": "bbbbbbbbbbbb", "action": "rollback", "version": "v0", "current_version": u.version, "has_gadget": true, "rebind": "no", "warnings": []string{}}, true
	case "update_discard":
		return map[string]any{"discarded": true}, true
	case "update_apply", "restart":
		stage, _ := req["stage"].(string)
		if req["cmd"] == "restart" {
			stage = "cccccccccccc"
		}
		if u.fail {
			u.state = map[string]any{"stage": stage, "phase": "rolled_back", "done": true, "ok": false, "from": u.version, "message": "the new lefthand did not become ready"}
			return map[string]any{"stage": stage}, true
		}
		u.state = map[string]any{"stage": stage, "phase": "waiting_confirm", "rebound": true}
		if req["cmd"] == "restart" {
			u.state = map[string]any{"stage": stage, "phase": "done", "done": true, "ok": true, "rebound": req["usb"] == true}
		} else {
			u.version = u.newVer
		}
		return map[string]any{"stage": stage}, true
	case "update_confirm":
		u.confirmed++
		u.state["phase"], u.state["done"], u.state["ok"] = "done", true, true
		return map[string]any{"confirmed": true}, true
	case "update_status":
		return map[string]any{"version": u.version, "state": u.state, "has_prev": true, "unit": "lefthand.service"}, true
	case "get_logs":
		u.logsCalls++
		if req["cursor"] != nil {
			return map[string]any{"text": "", "cursor": "c1"}, true
		}
		return map[string]any{"text": "line 1\nline 2", "cursor": "c1"}, true
	case "get_config_text":
		return map[string]any{"text": "# comment\nlayers: []\n"}, true
	case "put_config_text":
		t := req["text"].(string)
		switch {
		case strings.Contains(t, "NOPE"):
			return fakeErr{Code: "invalid_config", Message: "the config is not valid", Problems: []map[string]any{{"path": "/layers/0/keys/KEY_Q", "message": "unknown key NOPE"}}}, true
		case strings.Contains(t, "hidg1") && req["restart"] != true && req["dry_run"] != true:
			return fakeErr{Code: "needs_restart", Message: "x", Problems: []map[string]any{{"path": "/hid_device"}}}, true
		case req["dry_run"] == true:
			return map[string]any{"valid": true, "needs_restart": strings.Contains(t, "hidg1")}, true
		case strings.Contains(t, "hidg1"):
			u.config = t
			u.state = map[string]any{"stage": "dddddddddddd", "phase": "done", "done": true, "ok": true}
			return map[string]any{"saved": "/etc/lefthand/config.yaml", "restarting": true, "stage": "dddddddddddd"}, true
		}
		u.config = t
		return map[string]any{"saved": "/etc/lefthand/config.yaml", "restarting": false, "warnings": []string{}}, true
	case "power":
		return map[string]any{"action": req["action"]}, true
	}
	return nil, false
}

func newAdminFake(t *testing.T) (*fakeBrain, *fakeUpdater) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	old := helperSettle
	helperSettle = 10 * time.Millisecond
	t.Cleanup(func() { helperSettle = old })
	f := newFakeBrain(t)
	u := &fakeUpdater{version: "v1", newVer: "v2"}
	f.handler = u.handle
	go f.serve(nil)
	return f, u
}

func gzipFile(t *testing.T) string {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(bytes.Repeat([]byte("lefthand"), 1000))
	zw.Close()
	p := filepath.Join(t.TempDir(), "x.update.tar.gz")
	os.WriteFile(p, b.Bytes(), 0o644)
	return p
}

func TestUpdateAgainstFakeBrain(t *testing.T) {
	f, u := newAdminFake(t)
	p := gzipFile(t)
	code, out, errs := runCLI(t, "--port", f.slave, "update", p)
	if code != exitOK || !strings.Contains(out, "更新しました：lefthand v2") || !strings.Contains(out, "USB を付け直しました") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if !strings.Contains(errs, "USB の付け直しが要ります") || u.confirmed != 1 {
		t.Fatalf("stderr %s, confirmed %d", errs, u.confirmed)
	}
	data, _ := os.ReadFile(p)
	if !bytes.Equal(u.received, data) {
		t.Fatal("the file was not sent as is")
	}

	// 失敗したら、前の版に戻したと知らせる
	u.fail, u.version = true, "v1"
	code, _, errs = runCLI(t, "--port", f.slave, "update", p)
	if code != exitFailed || !strings.Contains(errs, "前の版（v1）に戻しました") {
		t.Fatalf("fail: exit %d %s", code, errs)
	}
	u.fail = false

	// 確かめるだけ
	code, out, _ = runCLI(t, "--port", f.slave, "update", p, "--dry-run")
	if code != exitOK || !strings.Contains(out, "入れ替えずに消しました") {
		t.Fatalf("dry-run %d %s", code, out)
	}
	// 前の版に戻す
	u.newVer = "v0"
	code, out, errs = runCLI(t, "--port", f.slave, "update", "--rollback")
	if code != exitOK || !strings.Contains(out, "前の版に戻しました：lefthand v0") {
		t.Fatalf("rollback %d %s %s", code, out, errs)
	}
	// 使い方の誤り
	for _, args := range [][]string{{"update"}, {"update", "--rollback", p}, {"update", "/etc/hostname"}, {"restart", "--rollback"}} {
		if code, _, _ := runCLI(t, append([]string{"--port", f.slave}, args...)...); code != exitUsage {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	code, out, _ = runCLI(t, "--port", f.slave, "update", "--status")
	if code != exitOK || !strings.Contains(out, "前の版\tあり") {
		t.Fatalf("status %d %s", code, out)
	}
}

func TestRestartLogsConfigAgainstFakeBrain(t *testing.T) {
	f, u := newAdminFake(t)
	code, out, errs := runCLI(t, "--port", f.slave, "restart", "--usb")
	if code != exitOK || !strings.Contains(out, "再起動しました") || !strings.Contains(out, "USB を付け直しました") {
		t.Fatalf("restart %d %s %s", code, out, errs)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "logs", "-n", "20")
	if code != exitOK || out != "line 1\nline 2\n" {
		t.Fatalf("logs %d %q", code, out)
	}

	code, out, _ = runCLI(t, "--port", f.slave, "config", "get")
	if code != exitOK || out != "# comment\nlayers: []\n" {
		t.Fatalf("config get %q", out)
	}
	dir := t.TempDir()
	put := func(name, text string, args ...string) (int, string, string) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(text), 0o644)
		return runCLI(t, append([]string{"--port", f.slave, "config", "put", p}, args...)...)
	}
	code, out, _ = put("ok.yaml", "# keep me\nlayers: []\n")
	if code != exitOK || !strings.Contains(out, "反映しました") || u.config != "# keep me\nlayers: []\n" {
		t.Fatalf("put %d %s %q", code, out, u.config)
	}
	code, _, errs = put("bad.yaml", "KEY_Q: NOPE\n")
	if code != exitBrainError || !strings.Contains(errs, "/layers/0/keys/KEY_Q：unknown key NOPE") {
		t.Fatalf("invalid %d %s", code, errs)
	}
	code, _, errs = put("re.yaml", "hid_device: /dev/hidg1\n")
	if code != exitBrainError || !strings.Contains(errs, "--restart") || !strings.Contains(errs, "/hid_device") {
		t.Fatalf("needs restart %d %s", code, errs)
	}
	code, out, _ = put("re.yaml", "hid_device: /dev/hidg1\n", "--dry-run")
	if code != exitOK || !strings.Contains(out, "再起動が要ります") {
		t.Fatalf("dry %d %s", code, out)
	}
	code, out, errs = put("re.yaml", "hid_device: /dev/hidg1\n", "--restart")
	if code != exitOK || !strings.Contains(out, "再起動しました") {
		t.Fatalf("restart put %d %s %s", code, out, errs)
	}
}

func TestPoweroffWaitsForThePortToGo(t *testing.T) {
	f, _ := newAdminFake(t)
	go func() {
		for req := range f.got {
			if req["cmd"] == "power" {
				time.Sleep(300 * time.Millisecond)
				f.master.Close() // Brain が止まると、ポートが消える
				return
			}
		}
	}()
	code, out, errs := runCLI(t, "--port", f.slave, "poweroff")
	if code != exitOK || !strings.Contains(out, "電源を切りました") {
		t.Fatalf("poweroff %d %s %s", code, out, errs)
	}
}

// ---------- exec ----------

func TestExecWritesTheTile(t *testing.T) {
	f, _ := newAdminFake(t)
	var texts []map[string]any
	f.handler = func(req map[string]any) (any, bool) {
		if req["cmd"] == "set_text" {
			texts = append(texts, req)
			return map[string]any{"shown": true}, true
		}
		return (&fakeUpdater{version: "v1"}).handle(req)
	}
	code, out, errs := runCLI(t, "--port", f.slave, "exec", "--result", "build", "--", "sh", "-c", "echo building; printf '\\033[32mall 3 passed\\033[0m\\n'")
	if code != 0 || !strings.Contains(out, "all 3 passed") {
		t.Fatalf("exit %d %q %s", code, out, errs)
	}
	f.mu.Lock()
	if len(texts) != 2 || !strings.HasPrefix(texts[0]["text"].(string), "実行中 ") || texts[1]["style"] != "ok" ||
		!strings.HasSuffix(texts[1]["text"].(string), "\nall 3 passed") || !strings.HasPrefix(texts[1]["text"].(string), "成功 ") {
		t.Fatalf("texts %v", texts)
	}
	texts = nil
	f.mu.Unlock()

	code, _, _ = runCLI(t, "--port", f.slave, "exec", "--result", "build", "--", "sh", "-c", "echo 'error: x' >&2; exit 3")
	f.mu.Lock()
	if code != 3 || texts[1]["style"] != "error" || !strings.Contains(texts[1]["text"].(string), "失敗(3)") || !strings.HasSuffix(texts[1]["text"].(string), "error: x") {
		t.Fatalf("exit %d %v", code, texts)
	}
	texts = nil
	f.mu.Unlock()

	code, _, _ = runCLI(t, "--port", f.slave, "exec", "--result", "build", "--limit", "300ms", "--", "sleep", "5")
	f.mu.Lock()
	if code != execExitTimeout || !strings.Contains(texts[1]["text"].(string), "時間切れ") {
		t.Fatalf("timeout %d %v", code, texts)
	}
	f.mu.Unlock()

	if code, _, _ := runCLI(t, "--port", f.slave, "exec", "--result", "build", "--", "no-such-command-xyz"); code != 127 {
		t.Fatalf("not found: %d", code)
	}
	for _, args := range [][]string{{"exec", "--", "true"}, {"exec", "--result", "build", "true"}, {"exec", "--result", "a/b", "--", "true"}} {
		if code, _, _ := runCLI(t, append([]string{"--port", f.slave}, args...)...); code != exitUsage {
			t.Errorf("%q: %d", args, code)
		}
	}
}

func TestExecWithoutBrainAndWhileRunning(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// Brain がなくても、コマンドは実行し、終了コードを返す
	code, out, errs := runCLI(t, "--port", "/dev/nonexistent-brain", "exec", "--result", "build", "--", "sh", "-c", "echo hi; exit 4")
	if code != 4 || out != "hi\n" || !strings.Contains(errs, "Brain が見つかりません") {
		t.Fatalf("exit %d %q %s", code, out, errs)
	}
	// 同じ id で実行中なら、重ねない
	unlock, ok := execLock("build")
	if !ok {
		t.Fatal("lock")
	}
	code, _, errs = runCLI(t, "--port", "/dev/nonexistent-brain", "exec", "--result", "build", "--", "true")
	unlock()
	if code != execExitBusy || !strings.Contains(errs, "まだ実行中") {
		t.Fatalf("busy %d %s", code, errs)
	}
	// 別の id は動く
	if code, _, _ := runCLI(t, "--port", "/dev/nonexistent-brain", "exec", "--result", "deploy", "--", "true"); code != 0 {
		t.Fatal(code)
	}
}

func TestTailLastLine(t *testing.T) {
	var w tailWriter
	w.Write([]byte("first\nprogress 10%\rprogress 100%\r\n\x1b[1;31mFAILED\x1b[0m\tcase\n\n"))
	if l := w.lastLine(); l != "FAILED case" {
		t.Fatalf("%q", l)
	}
	w = tailWriter{}
	w.Write([]byte("a\nprogress 10%\rprogress 100%\n"))
	if l := w.lastLine(); l != "progress 100%" {
		t.Fatalf("%q", l)
	}
	w.Write(bytes.Repeat([]byte("x"), 10000))
	if len(w.buf) != execTailBytes {
		t.Fatal(len(w.buf))
	}
}

func TestClipWidth(t *testing.T) {
	for in, want := range map[string]string{
		"ok: 42 tests passed":          "ok: 42 tests passed",
		"systemctl --user restart api": "systemctl --user r…",
		"ビルドに失敗しました（make）": "ビルドに失敗しまし…",
		"失敗(2) 18:15 12秒":           "失敗(2) 18:15 12秒",
	} {
		if got := clip(in, execLineMax); got != want {
			t.Errorf("clip(%q) = %q, want %q", in, got, want)
		}
	}
}
