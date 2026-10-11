//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testBackup は、lefthand の backup.go と同じ形のバックアップ（manifest.json が最後）を作る。
func testBackup(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	m := map[string]any{"format": 1, "kind": "brain-deck-backup", "created": "2026-10-12T01:00:00Z", "version": "v1", "host": "brain"}
	var list []map[string]any
	for _, n := range []string{"config/config.yaml", "data/todo.json", "data/memo/m1.json", "anthy/last-record2_default.utf8"} {
		b, ok := files[n]
		if !ok {
			continue
		}
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write([]byte(b))
		s := sha256.Sum256([]byte(b))
		list = append(list, map[string]any{"path": n, "bytes": len(b), "sha256": hex.EncodeToString(s[:])})
	}
	m["files"] = list
	mb, _ := json.Marshal(m)
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg})
	tw.Write(mb)
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

var testBackupFiles = map[string]string{"config/config.yaml": "# c\nlayers: []\n", "data/todo.json": "{}", "data/memo/m1.json": "{}", "anthy/last-record2_default.utf8": "x"}

// fakeBackups は、Brain のバックアップの係の代わり。
type fakeBackups struct {
	fakeUpdater
	archive   []byte
	reads     int
	done      bool
	applied   map[string]any
	problems  []string
	pcName    string
	profile   string
	manual    string
	backupCmd bool // backup_create に対応している
	lastSync  string
}

func (b *fakeBackups) handle(req map[string]any) (any, bool) {
	summary := map[string]any{"created": "2026-10-12T01:00:00Z", "version": "v1", "host": "brain", "config": true, "anthy": 1, "bytes": 30,
		"items": []map[string]any{{"name": "todo.json", "label": "Todo", "files": 1}, {"name": "memo", "label": "手書きメモ", "files": 1}}, "problems": b.problems}
	switch req["cmd"] {
	case "hello":
		cmds := []string{"hello", "get_status", "update_status", "update_confirm", "upload_begin", "restore_apply", "set_pc", "get_profile", "set_profile"}
		if b.backupCmd {
			cmds = append(cmds, "backup_create", "backup_read", "backup_done")
		}
		return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "v1", "upload_kinds": []string{"backup"}, "commands": cmds}, true
	case "backup_create":
		s := sha256.Sum256(b.archive)
		return map[string]any{"backup": "b1", "bytes": len(b.archive), "sha256": hex.EncodeToString(s[:]), "summary": summary}, true
	case "backup_read":
		b.reads++
		off := int(req["offset"].(float64))
		end := min(off+100, len(b.archive))
		return map[string]any{"offset": off, "total": len(b.archive), "data": base64.StdEncoding.EncodeToString(b.archive[off:end])}, true
	case "backup_done":
		b.done = true
		return map[string]any{"removed": true}, true
	case "upload_end":
		return map[string]any{"restore": "r1", "summary": summary}, true
	case "restore_apply":
		b.applied = req
		b.state = map[string]any{"stage": "eeeeeeeeeeee", "phase": "waiting_confirm"}
		if b.fail {
			b.state = map[string]any{"stage": "eeeeeeeeeeee", "phase": "rolled_back", "done": true, "ok": false, "message": "the new lefthand did not become ready"}
		}
		return map[string]any{"stage": "eeeeeeeeeeee", "action": "restore", "summary": summary}, true
	case "set_pc":
		b.pcName = req["name"].(string)
		b.profile = map[bool]string{true: "mac", false: ""}[b.pcName == "macbook"]
		return b.profileInfo(), true
	case "set_profile":
		switch n := req["name"].(string); n {
		case "auto":
			b.manual = ""
		case "nosuch":
			return fakeErr{Code: "bad_request", Message: `no profile "nosuch" in the config`}, true
		default:
			b.manual = n
		}
		return b.profileInfo(), true
	case "get_profile":
		return b.profileInfo(), true
	case "get_status":
		if b.lastSync != "" {
			return map[string]any{"sync": map[string]any{"running": false, "last": map[string]any{"at": b.lastSync}}}, true
		}
	}
	return b.fakeUpdater.handle(req)
}

func (b *fakeBackups) profileInfo() map[string]any {
	active := b.profile
	if b.manual != "" {
		active = map[bool]string{true: "", false: b.manual}[b.manual == "none"]
	}
	return map[string]any{"active": active, "pc": b.pcName, "manual": b.manual,
		"profiles": []map[string]any{{"name": "mac", "label": "Mac", "pcs": []string{"macbook"}}, {"name": "work", "pcs": []string{"work"}}}}
}

func newBackupFake(t *testing.T) (*fakeBrain, *fakeBackups) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := helperSettle
	helperSettle = 10 * time.Millisecond
	t.Cleanup(func() { helperSettle = old })
	f := newFakeBrain(t)
	b := &fakeBackups{fakeUpdater: fakeUpdater{version: "v1"}, archive: testBackup(testBackupFiles), backupCmd: true}
	f.handler = b.handle
	go f.serve(nil)
	return f, b
}

func TestBackupAgainstFakeBrain(t *testing.T) {
	f, b := newBackupFake(t)
	out := filepath.Join(t.TempDir(), "x", "b.tar.gz")
	code, stdout, errs := runCLI(t, "--port", f.slave, "backup", "-o", out)
	if code != exitOK || !strings.Contains(stdout, "に保存しました") || !strings.Contains(stdout, "設定ファイル、Todo、手書きメモ、日本語入力の学習") {
		t.Fatalf("backup %d %s %s", code, stdout, errs)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, b.archive) || !b.done || b.reads < 2 {
		t.Fatalf("archive differs or not finished (reads %d, done %v)", b.reads, b.done)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Dir(out)); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode())
	}
	// 既定の置き場所
	code, _, _ = runCLI(t, "--port", f.slave, "backup")
	ents, _ := os.ReadDir(backupDir())
	if code != exitOK || len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "brain-") {
		t.Fatalf("default dir %d %v", code, ents)
	}
	code, stdout, _ = runCLI(t, "backup", "list")
	if code != exitOK || !strings.Contains(stdout, "手で取ったもの") {
		t.Fatalf("list %s", stdout)
	}
	code, stdout, _ = runCLI(t, "backup", "show", out)
	if code != exitOK || !strings.Contains(stdout, "data/memo/\t1 個") || !strings.Contains(stdout, "config/config.yaml") {
		t.Fatalf("show %s", stdout)
	}
	// 壊れた（途中で切れた）ものは送る前に断る
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	os.WriteFile(bad, got[:len(got)-40], 0o600)
	if code, _, errs := runCLI(t, "--port", f.slave, "restore", bad); code != exitUsage || !strings.Contains(errs, "読めません") {
		t.Fatalf("damaged %d %s", code, errs)
	}
	// 戻す
	code, stdout, errs = runCLI(t, "--port", f.slave, "restore", out)
	if code != exitOK || !strings.Contains(stdout, "戻しました（設定ファイル、データ、日本語入力の学習）") || b.confirmed != 1 {
		t.Fatalf("restore %d %s %s", code, stdout, errs)
	}
	if !bytes.Equal(b.received, got) || b.applied["restore"] != "r1" {
		t.Fatalf("applied %v", b.applied)
	}
	// 設定ファイルを戻さない
	runCLI(t, "--port", f.slave, "restore", out, "--no-config")
	if items := b.applied["items"].(map[string]any); items["config"] != false || items["data"] != true {
		t.Fatalf("items %v", items)
	}
	// 失敗したら元に戻したと知らせる
	b.fail = true
	code, _, errs = runCLI(t, "--port", f.slave, "restore", out)
	if code != exitFailed || !strings.Contains(errs, "元の状態に戻しました") {
		t.Fatalf("fail %d %s", code, errs)
	}
	b.fail = false
	// 設定ファイルが使えない
	b.problems = []string{"/layers/0/keys/KEY_A: unknown key"}
	code, _, errs = runCLI(t, "--port", f.slave, "restore", out)
	if code != exitUsage || !strings.Contains(errs, "--no-config") {
		t.Fatalf("problems %d %s", code, errs)
	}
	b.problems = nil
	// 戻す前の状態に戻す
	code, stdout, _ = runCLI(t, "--port", f.slave, "restore", "--undo")
	if code != exitOK || b.applied["undo"] != true || !strings.Contains(stdout, "復元の前の状態に戻しました") {
		t.Fatalf("undo %d %s %v", code, stdout, b.applied)
	}
	for _, args := range [][]string{{"restore"}, {"restore", "--undo", out}, {"restore", out, "--no-config", "--no-data", "--no-anthy"}, {"backup", "x"}} {
		if code, _, _ := runCLI(t, append([]string{"--port", f.slave}, args...)...); code != exitUsage {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

// sync のバックアップ：間隔のうちは取らない、keep を超えたら古い sync のバックアップから消す（手で取ったものは消さない）。
func TestSyncBackup(t *testing.T) {
	f, b := newBackupFake(t)
	dir := t.TempDir()
	keep := 2
	bc := syncBackupConfig{Keep: &keep, Every: "1h", Dir: dir}
	os.WriteFile(filepath.Join(dir, "brain-20260101-000000.tar.gz"), []byte("manual"), 0o600)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	for i, old := range []string{"20261001-090000", "20261002-090000", "20261003-090000"} {
		p := filepath.Join(dir, syncBackupPrefix+old+".tar.gz")
		os.WriteFile(p, []byte("old"), 0o600)
		mt := now.Add(-time.Duration(3-i) * 24 * time.Hour)
		os.Chtimes(p, mt, mt)
	}
	c, unlock, err := connect(connectOptions{port: f.slave, timeout: 5 * time.Second, deadline: time.Now().Add(5 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	defer c.Close()
	callTimeout = func() time.Duration { return 5 * time.Second }
	note, skip, err := syncBackup(c, bc, now)
	if err != nil || skip || !strings.Contains(note, "古いものを 2 個消した") {
		t.Fatalf("note %q skip %v err %v", note, skip, err)
	}
	names := syncBackups(dir)
	if len(names) != 2 || names[0] != backupName(syncBackupPrefix, now) {
		t.Fatalf("left %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "brain-20260101-000000.tar.gz")); err != nil {
		t.Fatal("a manual backup was removed")
	}
	// 1 時間のうちは取らない
	os.Chtimes(filepath.Join(dir, names[0]), now, now)
	if note, skip, _ := syncBackup(c, bc, now.Add(30*time.Minute)); !skip || !strings.Contains(note, "たっていない") {
		t.Fatalf("every: %q %v", note, skip)
	}
	zero := 0
	if _, skip, _ := syncBackup(c, syncBackupConfig{Keep: &zero}, now); !skip {
		t.Fatal("keep: 0 must skip")
	}
	b.backupCmd = false
}

func TestProfileAgainstFakeBrain(t *testing.T) {
	f, b := newBackupFake(t)
	code, out, _ := runCLI(t, "--port", f.slave, "profile", "announce", "macbook")
	if code != exitOK || b.pcName != "macbook" || !strings.Contains(out, "mac。PC macbook が名乗った") {
		t.Fatalf("announce %d %s", code, out)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "profile", "list")
	if code != exitOK || !strings.Contains(out, "* mac\tMac\tPC：macbook") {
		t.Fatalf("list %s", out)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "profile", "use", "work")
	if code != exitOK || !strings.Contains(out, "work。手で選んだ") {
		t.Fatalf("use %d %s", code, out)
	}
	if code, _, errs := runCLI(t, "--port", f.slave, "profile", "use", "nosuch"); code != exitBrainError || !strings.Contains(errs, "nosuch") {
		t.Fatalf("unknown %d %s", code, errs)
	}
	runCLI(t, "--port", f.slave, "profile", "auto")
	if b.manual != "" {
		t.Fatal("auto")
	}
	// sync.yaml の pc_name
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "brain-deck"), 0o755)
	os.WriteFile(syncPath(), []byte("pc_name: macbook\n"), 0o644)
	b.pcName = ""
	if code, _, _ := runCLI(t, "--port", f.slave, "profile", "announce"); code != exitOK || b.pcName != "macbook" {
		t.Fatalf("announce from sync.yaml: %d %q", code, b.pcName)
	}
	os.WriteFile(syncPath(), []byte("pc_name: \"my pc\"\n"), 0o644)
	if code, _, _ := runCLI(t, "--port", f.slave, "profile", "announce"); code == exitOK {
		t.Fatal("a bad PC name was accepted")
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "profile", "use"); code != exitUsage {
		t.Fatal("usage")
	}
}

// sync --if-older で同期を飛ばすときも、PC の名前は知らせる（USB を付け直したあとに既定に戻らないように）。
func TestSyncIfOlderStillAnnounces(t *testing.T) {
	f, b := newBackupFake(t)
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "brain-deck"), 0o755)
	os.WriteFile(syncPath(), []byte("pc_name: macbook\n"), 0o644)
	b.lastSync = time.Now().Add(-time.Minute).Format(time.RFC3339)
	code, out, errs := runCLI(t, "--port", f.slave, "sync", "--if-older", "10m")
	if code != exitOK || !strings.Contains(out, "何もしません") || b.pcName != "macbook" {
		t.Fatalf("%d %q %q pc %q", code, out, errs, b.pcName)
	}
}
