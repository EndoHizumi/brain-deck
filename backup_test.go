package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------- バックアップと復元（フェーズ K3） ----------

// backupFixture は、データの置き場所（入れるものと入れないもの）、設定ファイル、Anthy の学習を作る。
func backupFixture(t *testing.T, data map[string]string) (dataDir, cfgPath, anthy string) {
	t.Helper()
	root := t.TempDir()
	dataDir, cfgPath, anthy = filepath.Join(root, "data"), filepath.Join(root, "config.yaml"), filepath.Join(root, "home", ".anthy")
	for name, body := range data {
		p := filepath.Join(dataDir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(cfgPath, []byte("# コメント\n"+testConfig[strings.Index(testConfig, "\n")+1:]), 0o644)
	os.MkdirAll(anthy, 0o700)
	os.WriteFile(filepath.Join(anthy, "last-record2_default.utf8"), []byte("学習"), 0o600)
	os.WriteFile(filepath.Join(anthy, "lock-file_default"), nil, 0o600)
	return
}

var backupData = map[string]string{
	"todo.json":                   `{"items":[{"text":"牛乳"}]}`,
	"widgets.json":                `{"counter":{}}`,
	"overrides.json":              `{"format":1,"values":{"backlight.level":3}}`,
	"memo/m1.json":                `{"id":"m1"}`,
	"memo/.m2.json.tmp1":          `half`, // 書いている途中の一時ファイル：入れない
	"reader/index.json":           `{}`,
	"reader/abc.json.gz":          "gz",
	"images/0123456789abcdef.565": "IMG",
	"photos/.upload-x.part":       "partial",
	// 取り直せるもの：入れない
	"weather.json":          `{}`,
	"calendar.json":         `{}`,
	"clock.json":            `{}`,
	"battery-log.csv":       "t,mv\n",
	"weather-icons/101.lha": "icon",
	"update/state.json":     `{}`,
	"text.json":             `{}`,
}

func backupPaths(m *BackupManifest) []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

func TestBackupRoundTrip(t *testing.T) {
	dataDir, cfgPath, anthy := backupFixture(t, backupData)
	var buf bytes.Buffer
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	m, err := createBackup(&buf, backupSources{DataDir: dataDir, CfgPath: cfgPath, AnthyDir: anthy, Version: "v1", Host: "brain", Profile: "mac", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"anthy/last-record2_default.utf8", "config/config.yaml", "data/images/0123456789abcdef.565", "data/memo/m1.json",
		"data/overrides.json", "data/reader/abc.json.gz", "data/reader/index.json", "data/todo.json", "data/widgets.json"}
	if got := backupPaths(m); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("files\n got %q\nwant %q", got, want)
	}
	contents := map[string]string{}
	m2, err := readBackup(bytes.NewReader(buf.Bytes()), func(f BackupFile, r io.Reader) error {
		b, _ := io.ReadAll(r)
		contents[f.Path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Host != "brain" || m2.Version != "v1" || m2.Profile != "mac" || !m2.Created.Equal(now) || len(m2.Files) != len(want) {
		t.Fatalf("manifest %+v", m2)
	}
	if contents["data/todo.json"] != backupData["todo.json"] || contents["anthy/last-record2_default.utf8"] != "学習" ||
		!strings.HasPrefix(contents["config/config.yaml"], "# コメント\n") {
		t.Fatalf("contents %q", contents)
	}
	s := summarize(m2)
	if !s.Config || s.Anthy != 1 || s.Items[0].Name != "todo.json" || s.Items[0].Files != 1 {
		t.Fatalf("summary %+v", s)
	}
	for _, it := range s.Items {
		if it.Name == "memo" && it.Files != 1 || it.Name == "photos" && it.Files != 0 || it.Name == "reader" && it.Files != 2 {
			t.Errorf("item %+v", it)
		}
	}
	// manifest.json は最後
	gz, _ := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	tr := tar.NewReader(gz)
	var last string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		last = h.Name
	}
	if last != backupManifest {
		t.Errorf("the last entry is %q", last)
	}
}

// 壊れたもの、知らない名前、置き場所の外に出る名前、新しすぎる形は断る。
func TestBackupRejects(t *testing.T) {
	build := func(files map[string]string, edit func(*BackupManifest)) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		m := BackupManifest{Format: 1, Kind: backupKindName}
		var names []string
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			b := []byte(files[n])
			tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
			tw.Write(b)
			m.Files = append(m.Files, BackupFile{Path: n, Bytes: int64(len(b)), SHA256: sha256hex(b)})
		}
		if edit != nil {
			edit(&m)
		}
		mb, _ := json.Marshal(m)
		tw.WriteHeader(&tar.Header{Name: backupManifest, Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg})
		tw.Write(mb)
		tw.Close()
		zw.Close()
		return buf.Bytes()
	}
	ok := map[string]string{"data/todo.json": "{}"}
	if _, err := readBackup(bytes.NewReader(build(ok, nil)), nil); err != nil {
		t.Fatalf("a good backup was rejected: %v", err)
	}
	for name, b := range map[string][]byte{
		"not gzip":       []byte("hello"),
		"traversal":      build(map[string]string{"data/../../etc/passwd": "x"}, nil),
		"unknown item":   build(map[string]string{"data/weather.json": "{}"}, nil),
		"hidden":         build(map[string]string{"data/memo/.x": "{}"}, nil),
		"anthy lock":     build(map[string]string{"anthy/lock-file_default": ""}, nil),
		"absolute":       build(map[string]string{"/data/todo.json": "{}"}, nil),
		"damaged":        build(ok, func(m *BackupManifest) { m.Files[0].SHA256 = strings.Repeat("0", 64) }),
		"missing file":   build(ok, func(m *BackupManifest) { m.Files = append(m.Files, BackupFile{Path: "data/calc.json"}) }),
		"newer format":   build(ok, func(m *BackupManifest) { m.Format = 2 }),
		"other kind":     build(ok, func(m *BackupManifest) { m.Kind = "x" }),
		"no manifest":    func() []byte { b := build(ok, nil); return b[:len(b)/2] }(),
		"file as a dir":  build(map[string]string{"data/todo.json/x": "{}"}, nil),
		"dir item alone": build(map[string]string{"data/memo": "{}"}, nil),
	} {
		if _, err := readBackup(bytes.NewReader(b), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// 入れ替え：バックアップのとおりにし（ないものは今のものを動かす）、設定ファイルは .prev を残し、学習を書く。元に戻せば前のまま。
func TestRestoreApplyAndUndo(t *testing.T) {
	src, srcCfg, srcAnthy := backupFixture(t, map[string]string{"todo.json": `{"v":"backup"}`, "memo/m1.json": `{"id":"m1"}`})
	os.WriteFile(srcCfg, []byte("# backup config\n"+testConfig), 0o644)
	var buf bytes.Buffer
	if _, err := createBackup(&buf, backupSources{DataDir: src, CfgPath: srcCfg, AnthyDir: srcAnthy, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dataDir, cfgPath, anthy := backupFixture(t, map[string]string{"todo.json": `{"v":"now"}`, "photos/index.json": `{"p":1}`, "weather.json": `{"keep":1}`})
	os.WriteFile(filepath.Join(anthy, "last-record2_default.utf8"), []byte("今の学習"), 0o600)
	before := snapshotDir(t, filepath.Dir(dataDir))
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	os.WriteFile(archive, buf.Bytes(), 0o600)
	p := restorePlan{Archive: archive, DataDir: dataDir, CfgPath: cfgPath, AnthyDir: anthy, UID: os.Getuid(), GID: os.Getgid(),
		RestoreOptions: RestoreOptions{Config: true, Data: true, Anthy: true}}
	u, err := applyRestore(p)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dataDir, "todo.json")) != `{"v":"backup"}` || readFile(t, filepath.Join(dataDir, "memo", "m1.json")) != `{"id":"m1"}` {
		t.Fatal("data not restored")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "photos")); err == nil {
		t.Fatal("photos (not in the backup) must be moved away")
	}
	if readFile(t, filepath.Join(dataDir, "weather.json")) != `{"keep":1}` {
		t.Fatal("weather.json is not part of a backup and must stay")
	}
	if !strings.HasPrefix(readFile(t, cfgPath), "# backup config") || !strings.HasPrefix(readFile(t, cfgPath+".prev"), "# コメント") {
		t.Fatal("config not restored with .prev")
	}
	if readFile(t, filepath.Join(anthy, "last-record2_default.utf8")) != "学習" {
		t.Fatal("anthy not restored")
	}
	// 新しいデーモンが起動しなかった：元に戻す
	if err := u.undo(); err != nil {
		t.Fatal(err)
	}
	after := snapshotDir(t, filepath.Dir(dataDir))
	delete(after, "config.yaml.prev") // .prev は残る（中身は前の設定）
	if !equalMaps(before, after) {
		t.Fatalf("undo did not restore the previous state\nbefore %v\nafter  %v", before, after)
	}
	// 設定ファイルを戻さない
	p.Config, p.Anthy = false, false
	u, err = applyRestore(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(readFile(t, cfgPath), "# backup config") || readFile(t, filepath.Join(anthy, "last-record2_default.utf8")) != "今の学習" {
		t.Fatal("config or anthy restored although not asked")
	}
	u.finish()
	for _, d := range []string{backupNewDir, backupOldDir} {
		if _, err := os.Stat(filepath.Join(dataDir, backupDirName, d)); err == nil {
			t.Errorf("%s left behind", d)
		}
	}
	// 途中で止まった復元の残りがあれば断る
	os.MkdirAll(p.oldDir(), 0o700)
	if _, err := applyRestore(p); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v", err)
	}
}

func snapshotDir(t *testing.T, root string) map[string]string {
	out := map[string]string{}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			b, _ := os.ReadFile(p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// プロトコル：backup_create → backup_read → backup_done、upload（kind: backup）→ restore_apply → 係が入れ替える。
func TestBackupProtocolAndHelper(t *testing.T) {
	helperTimeouts(t)
	var bs *BackupService
	e := newUpdateEnv(t)
	bs = NewBackupService(e.dataDir, e.path)
	bs.store, bs.updater = e.store, e.upd
	bs.host = func() string { return "brain" }
	bs.flush = e.todos.Flush
	e.ctl.backup = bs
	e.ctl.uploads.Register("backup", bs.UploadKind())
	result(t, e.call("todo_add", map[string]any{"text": "バックアップ前"}))
	os.MkdirAll(filepath.Join(e.dataDir, "memo"), 0o755)
	os.WriteFile(filepath.Join(e.dataDir, "memo", "m1.json"), []byte(`{"id":"m1"}`), 0o644)

	res := result(t, e.call("backup_create", nil))
	tok := res["backup"].(string)
	total := int(res["bytes"].(float64))
	var got []byte
	for len(got) < total {
		r := result(t, e.call("backup_read", map[string]any{"backup": tok, "offset": len(got), "length": 100}))
		b, _ := base64.StdEncoding.DecodeString(r["data"].(string))
		got = append(got, b...)
	}
	if sha256hex(got) != res["sha256"] {
		t.Fatal("sha256 mismatch")
	}
	if r := result(t, e.call("backup_done", map[string]any{"backup": tok})); r["removed"] != true {
		t.Fatalf("done %v", r)
	}
	if r := e.call("backup_read", map[string]any{"backup": tok, "offset": 0}); r["ok"] == true {
		t.Fatal("read after done")
	}
	// 状態を変えてから、復元を送る
	result(t, e.call("todo_add", map[string]any{"text": "バックアップのあと"}))
	os.Remove(filepath.Join(e.dataDir, "memo", "m1.json"))
	r := e.send("backup", got)
	end := result(t, r)
	sum := end["summary"].(map[string]any)
	if sum["config"] != true || sum["host"] != "brain" || sum["problems"] != nil {
		t.Fatalf("summary %v", sum)
	}
	// 壊れたもの
	bad := append([]byte(nil), got...)
	bad[len(bad)/2] ^= 0xff
	if r := e.send("backup", bad); r["ok"] == true {
		t.Fatal("a damaged backup was accepted")
	}
	r = e.send("backup", got)
	end = result(t, r)
	ap := result(t, e.call("restore_apply", map[string]any{"restore": end["restore"]}))
	stage := ap["stage"].(string)
	if len(e.sys.runs) != 1 || !strings.Contains(strings.Join(e.sys.runs[0], " "), "-update-apply") {
		t.Fatalf("runs %v", e.sys.runs)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, backupDirName, backupBeforeName)); err != nil {
		t.Fatal("before-restore.tar.gz was not made")
	}
	var plan updatePlan
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(e.upd.Dir, stage, updatePlanFile))), &plan); err != nil || plan.Restore == nil || !plan.Confirm {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// 係：起動しなかったら元に戻す
	os.WriteFile(e.bin, []byte("broken\n"), 0o755)
	ok, st := e.runHelper(plan)
	if ok || st.Phase != "rolled_back" || !strings.Contains(readFile(t, filepath.Join(e.dataDir, "todo.json")), "バックアップのあと") {
		t.Fatalf("rollback: ok=%v %+v", ok, st)
	}
	// 起動したら入れ替えたまま
	os.WriteFile(e.bin, []byte("v1\n"), 0o755)
	os.MkdirAll(filepath.Join(e.upd.Dir, stage), 0o755)
	os.WriteFile(filepath.Join(e.upd.Dir, stage, "backup.tar.gz"), got, 0o600)
	e.confirmWhenAsked(stage)
	ok, st = e.runHelper(plan)
	todo := readFile(t, filepath.Join(e.dataDir, "todo.json"))
	if !ok || st.Phase != "done" || strings.Contains(todo, "バックアップのあと") || !strings.Contains(todo, "バックアップ前") {
		t.Fatalf("restore: ok=%v %+v todo %s", ok, st, todo)
	}
	if readFile(t, filepath.Join(e.dataDir, "memo", "m1.json")) != `{"id":"m1"}` {
		t.Fatal("memo not restored")
	}
	// 復元の前の状態に戻す（undo）：今の状態を残してから
	e.sys.runs = nil
	ap = result(t, e.call("restore_apply", map[string]any{"undo": true}))
	if ap["action"] != "restore" || len(e.sys.runs) != 1 {
		t.Fatalf("undo %v runs %v", ap, e.sys.runs)
	}
	if r := e.call("restore_apply", map[string]any{"restore": "000000000000"}); r["ok"] == true {
		t.Fatal("unknown restore accepted")
	}
}

// 設定ファイルが今のデーモンで使えないバックアップは、設定ファイルを戻せない（データだけなら戻せる）。
func TestBackupConfigProblems(t *testing.T) {
	e := newUpdateEnv(t)
	bs := NewBackupService(e.dataDir, e.path)
	bs.store, bs.updater = e.store, e.upd
	e.ctl.backup = bs
	e.ctl.uploads.Register("backup", bs.UploadKind())
	src, cfgPath, _ := backupFixture(t, map[string]string{"todo.json": "{}"})
	os.WriteFile(cfgPath, []byte("layers: [{name: base, keys: {KEY_A: NOSUCHKEY}}]\n"), 0o644)
	var buf bytes.Buffer
	createBackup(&buf, backupSources{DataDir: src, CfgPath: cfgPath, Now: time.Now()})
	end := result(t, e.send("backup", buf.Bytes()))
	if ps := end["summary"].(map[string]any)["problems"]; ps == nil {
		t.Fatal("problems expected")
	}
	if r := e.call("restore_apply", map[string]any{"restore": end["restore"]}); r["ok"] == true {
		t.Fatal("restoring an unusable config must be refused")
	}
	r := e.call("restore_apply", map[string]any{"restore": end["restore"], "items": map[string]any{"data": true}})
	if r["ok"] != true {
		t.Fatalf("data only: %v", r)
	}
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
