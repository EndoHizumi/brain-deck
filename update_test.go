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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScriptDescriptorMatchesGo(t *testing.T) {
	b, err := os.ReadFile("gadget-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"KBD_DESC": hidDescKeyboard, "COMBO_DESC": hidDescCombo} {
		got, err := scriptDescriptor(b, name)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: % x %v", name, got, err)
		}
		// bash の printf で展開したもの（hid_test.go）とも同じ
		if bash := gadgetDesc(t, name); !bytes.Equal(got, bash) {
			t.Errorf("%s differs from bash printf", name)
		}
	}
	if err := checkGadgetScript("gadget-setup.sh"); err != nil {
		t.Fatal(err)
	}
	bad := bytes.Replace(b, []byte(`\x25\x73\x05\x07\x19\x00\x29\x73`), []byte(`\x25\x65\x05\x07\x19\x00\x29\x65`), 1)
	p := filepath.Join(t.TempDir(), "g.sh")
	os.WriteFile(p, bad, 0o755)
	if err := checkGadgetScript(p); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a changed descriptor was accepted: %v", err)
	}
}

func TestUnitOf(t *testing.T) {
	if u := unitOf([]byte("0::/system.slice/lefthand.service\n")); u != "lefthand.service" {
		t.Errorf("unit %q", u)
	}
	if u := unitOf([]byte("0::/user.slice/user-1000.slice/session-3.scope\n")); u != "" {
		t.Errorf("scope gave %q", u)
	}
}

// ---------- 偽の systemd と、新しい lefthand ----------

// fakeSystem は systemctl、systemd-run、コマンドの代わり。lefthand の「実行ファイル」は、版を書いたテキスト。
// start すると、入っている実行ファイルの版で起動の知らせを書く（中身が broken なら書かない）。
type fakeSystem struct {
	mu       sync.Mutex
	pid      int
	nextPID  int
	bin      string
	ready    string
	calls    []string
	gadget   [][]string // gadget-setup.sh を実行したときの環境変数
	runs     [][]string // systemd-run
	helperOn bool
	journal  string
}

func (f *fakeSystem) ops() sysOps {
	return sysOps{
		systemctl: func(args ...string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, strings.Join(args, " "))
			switch args[0] {
			case "stop":
				f.pid = 0
			case "start":
				f.nextPID++
				f.pid = 100 + f.nextPID
				v, _ := os.ReadFile(f.bin)
				if s := strings.TrimSpace(string(v)); s != "broken" {
					b, _ := json.Marshal(readyInfo{Version: s, PID: f.pid})
					os.WriteFile(f.ready, b, 0o644)
				}
			case "show":
				return strconv.Itoa(f.pid), nil
			case "is-active":
				if f.helperOn {
					return "active", nil
				}
				return "inactive", nil
			}
			return "", nil
		},
		systemdRun: func(unit string, argv ...string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.runs = append(f.runs, append([]string{unit}, argv...))
			return nil
		},
		command: func(env []string, name string, args ...string) (string, int, error) {
			switch {
			case name == "journalctl":
				f.mu.Lock()
				defer f.mu.Unlock()
				f.calls = append(f.calls, "journalctl "+strings.Join(args, " "))
				return f.journal, 0, nil
			case name == "bash" && len(args) == 2 && args[0] == "-n":
				return runCommand(env, name, args...)
			case name == "bash":
				f.mu.Lock()
				f.gadget = append(f.gadget, env)
				f.mu.Unlock()
				for _, e := range env {
					if e == "GADGET_CHECK=1" {
						return "rebind: the HID report descriptor or attributes differ (want keyboard)\n", 10, nil
					}
				}
				return "done\n", 0, nil
			}
			// 新しい lefthand（中身は版のテキスト）
			b, err := os.ReadFile(name)
			if err != nil {
				return "", -1, err
			}
			v := strings.TrimSpace(string(b))
			switch args[0] {
			case "-version":
				if v == "legacy" {
					return "flag provided but not defined: -version", 2, nil
				}
				vi := versionInfo{Version: v, Update: 1}
				if strings.HasPrefix(v, "k7") { // 読み物の文字を別のファイルに置く版（フェーズ K7）
					vi.ReaderFonts = updateFontNames()
				}
				j, _ := json.Marshal(vi)
				return string(j) + "\n", 0, nil
			case "-check-fonts":
				if err := checkReadFonts(args[1]); err != nil {
					return err.Error(), 1, nil
				}
				return "ok", 0, nil
			case "-check-gadget":
				if err := checkGadgetScript(args[1]); err != nil {
					return err.Error(), 1, nil
				}
				return "ok", 0, nil
			case "-check":
				if v == "strict" {
					return "config.yaml: unknown field", 1, nil
				}
				cfg, _ := os.ReadFile(args[len(args)-1])
				if _, _, _, err := checkConfig(cfg); err != nil {
					return err.Error(), 1, nil
				}
				return "2026/10/09 warning: something minor\n", 0, nil
			}
			return "", 2, nil
		},
	}
}

func (f *fakeSystem) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "|")
}

// updateEnv は、入れ替え先（bin、gadget）と、デーモンを持つテスト。
type updateEnv struct {
	*testDaemon
	sys    *fakeSystem
	upd    *Updater
	bin    string
	gadget string
}

func newUpdateEnv(t *testing.T) *updateEnv {
	e := &updateEnv{}
	root := t.TempDir()
	e.bin, e.gadget = filepath.Join(root, "bin", "lefthand"), filepath.Join(root, "sbin", "gadget-setup")
	os.MkdirAll(filepath.Dir(e.bin), 0o755)
	os.MkdirAll(filepath.Dir(e.gadget), 0o755)
	os.WriteFile(e.bin, []byte("v1\n"), 0o755)
	os.WriteFile(e.bin+".prev", []byte("v0\n"), 0o755)
	script, _ := os.ReadFile("gadget-setup.sh")
	os.WriteFile(e.gadget, script, 0o755)
	e.sys = &fakeSystem{bin: e.bin, ready: filepath.Join(root, "ready")}
	e.testDaemon = newTestDaemon(t, testConfig, func(d *testDaemon) {
		e.upd = NewUpdater(d.dataDir, e.bin, e.gadget, "lefthand.service", d.path, filepath.Join(d.dataDir, "images"), e.sys.ready, nil, nil)
		e.upd.sys = e.sys.ops()
		e.upd.Helper = "/usr/local/bin/lefthand"
		d.ctl.updater = e.upd
		d.ctl.uploads = NewUploads()
		d.ctl.uploads.Register("update", e.upd.UploadKind())
		d.ctl.uploads.Register("image", d.images.UploadKind())
	})
	return e
}

// makeBundle は更新のファイル（tar.gz）を作る。edit で manifest を書き換えられる。
func makeBundle(t *testing.T, files map[string][]byte, edit func(*updateManifest), extra ...string) []byte {
	t.Helper()
	m := updateManifest{Format: 1, Version: strings.TrimSpace(string(files["lefthand"])), Files: map[string]manifestFile{}}
	for n, b := range files {
		s := sha256.Sum256(b)
		m.Files[n] = manifestFile{SHA256: hex.EncodeToString(s[:]), Bytes: int64(len(b))}
	}
	if edit != nil {
		edit(&m)
	}
	mb, _ := json.Marshal(m)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(n string, b []byte) {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	add("manifest.json", mb)
	for n, b := range files {
		add(n, b)
	}
	for _, n := range extra {
		add(n, []byte("x"))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// send は upload_* で data を送り、upload_end の返事を返す。
func (d *testDaemon) send(kind string, data []byte) map[string]any {
	d.t.Helper()
	s := sha256.Sum256(data)
	r := d.call("upload_begin", map[string]any{"kind": kind, "sha256": hex.EncodeToString(s[:]), "bytes": len(data), "name": "x", "source": "test"})
	if r["ok"] != true {
		return r
	}
	res := r["result"].(map[string]any)
	tok, chunk := res["upload"], int(res["chunk_bytes"].(float64))
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		result(d.t, d.call("upload_chunk", map[string]any{"upload": tok, "offset": off, "data": base64.StdEncoding.EncodeToString(data[off:end])}))
	}
	return d.call("upload_end", map[string]any{"upload": tok})
}

func goodFiles(t *testing.T, ver string) map[string][]byte {
	script, err := os.ReadFile("gadget-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"lefthand": []byte(ver + "\n"), "gadget-setup.sh": script}
}

func TestUpdateUploadVerify(t *testing.T) {
	e := newUpdateEnv(t)
	hello := result(t, e.call("hello", nil))
	if !strings.Contains(strings.Join(func() []string {
		var s []string
		for _, x := range hello["upload_kinds"].([]any) {
			s = append(s, x.(string))
		}
		return s
	}(), ","), "update") {
		t.Fatalf("upload_kinds %v", hello["upload_kinds"])
	}

	// 同じ gadget-setup.sh なら、付け直さない
	info := result(t, e.send("update", makeBundle(t, goodFiles(t, "v2"), nil)))
	if info["version"] != "v2" || info["gadget_changed"] != false || info["rebind"] != "no" || info["bin_changed"] != true {
		t.Fatalf("info %v", info)
	}
	if w := info["warnings"].([]any); len(w) != 1 || w[0] != "something minor" {
		t.Fatalf("warnings %v", w)
	}
	stage := info["stage"].(string)
	if _, err := os.Stat(filepath.Join(e.upd.Dir, stage, "lefthand")); err != nil {
		t.Fatal("not unpacked:", err)
	}
	if parts(t, e.upd.Dir) != 0 {
		t.Fatal("the bundle file was left")
	}

	// gadget-setup.sh が変われば、GADGET_CHECK で調べる
	f := goodFiles(t, "v3")
	f["gadget-setup.sh"] = append(f["gadget-setup.sh"], []byte("\n# changed\n")...)
	info = result(t, e.send("update", makeBundle(t, f, nil)))
	if info["gadget_changed"] != true || info["rebind"] != "yes" || len(info["rebind_reasons"].([]any)) != 1 {
		t.Fatalf("info %v", info)
	}
	if _, err := os.Stat(filepath.Join(e.upd.Dir, stage)); err == nil {
		t.Fatal("the earlier stage was not removed")
	}

	// 断るもの。何も残さない
	bad := map[string][]byte{}
	for k, v := range goodFiles(t, "v4") {
		bad[k] = v
	}
	badDesc := bytes.Replace(bad["gadget-setup.sh"], []byte(`\x29\x73\x81\x00\xc0'`), []byte(`\x29\x65\x81\x00\xc0'`), 1)
	cases := map[string][]byte{
		"version":  makeBundle(t, goodFiles(t, "v4"), func(m *updateManifest) { m.Version = "v5" }),
		"sha":      makeBundle(t, goodFiles(t, "v4"), func(m *updateManifest) { m.Files["lefthand"] = manifestFile{SHA256: strings.Repeat("0", 64), Bytes: 3} }),
		"extra":    makeBundle(t, goodFiles(t, "v4"), nil, "evil.sh"),
		"nogadget": makeBundle(t, map[string][]byte{"lefthand": []byte("v4\n")}, nil),
		"config":   makeBundle(t, goodFiles(t, "strict"), nil),
		"pair":     makeBundle(t, map[string][]byte{"lefthand": []byte("v4\n"), "gadget-setup.sh": badDesc}, nil),
		"notgz":    []byte("this is not gzip"),
	}
	for name, b := range cases {
		if c := errCode(e.send("update", b)); c != errRejected {
			t.Errorf("%s: %s", name, c)
		}
	}
	ents, _ := os.ReadDir(e.upd.Dir)
	for _, x := range ents {
		if x.IsDir() {
			t.Errorf("a rejected update left %s", x.Name())
		}
	}
	if b, _ := os.ReadFile(e.bin); string(b) != "v1\n" {
		t.Fatal("the binary changed")
	}
}

func TestUpdateApplyStartsHelper(t *testing.T) {
	e := newUpdateEnv(t)
	info := result(t, e.send("update", makeBundle(t, goodFiles(t, "v2"), nil)))
	stage := info["stage"].(string)
	if c := errCode(e.call("update_apply", map[string]any{"stage": "0123456789ab"})); c != errNotFound {
		t.Fatalf("unknown stage: %s", c)
	}
	e.sys.helperOn = true
	if c := errCode(e.call("update_apply", map[string]any{"stage": stage})); c != errConflict {
		t.Fatalf("while a helper runs: %s", c)
	}
	e.sys.helperOn = false
	r := result(t, e.call("update_apply", map[string]any{"stage": stage}))
	if r["version"] != "v2" || r["confirm"] != true {
		t.Fatalf("apply %v", r)
	}
	if len(e.sys.runs) != 1 || e.sys.runs[0][0] != updateHelperUnit || e.sys.runs[0][2] != "-update-apply" {
		t.Fatalf("systemd-run %v", e.sys.runs)
	}
	var p updatePlan
	b, _ := os.ReadFile(e.sys.runs[0][3])
	json.Unmarshal(b, &p)
	if p.Unit != "lefthand.service" || p.Bin != e.bin || p.Gadget != e.gadget || p.Version != "v2" || p.StagedGad == "" || !p.Confirm {
		t.Fatalf("plan %+v", p)
	}
	st := result(t, e.call("update_status", nil))
	if st["state"].(map[string]any)["phase"] != "starting_helper" || st["has_prev"] != true {
		t.Fatalf("status %v", st)
	}
	// 入れ替えたあと、新しいデーモンが update_confirm を受ける
	result(t, e.call("update_confirm", map[string]any{"stage": stage, "source": "test"}))
	if _, err := os.Stat(filepath.Join(e.upd.Dir, stage, updateConfirmFile)); err != nil {
		t.Fatal(err)
	}
	// 捨てる（--dry-run）
	info = result(t, e.send("update", makeBundle(t, goodFiles(t, "v3"), nil)))
	if r := result(t, e.call("update_discard", map[string]any{"stage": info["stage"]})); r["discarded"] != true {
		t.Fatal(r)
	}
}

func TestUpdateRollbackPrepare(t *testing.T) {
	e := newUpdateEnv(t)
	info := result(t, e.call("update_rollback", nil))
	if info["action"] != "rollback" || info["version"] != "v0" || info["legacy"] == true {
		t.Fatalf("rollback %v", info)
	}
	// .prev はそのまま残る（リンクで置いたので）
	if b, _ := os.ReadFile(e.bin + ".prev"); string(b) != "v0\n" {
		t.Fatal(".prev changed")
	}
	// -version に答えない古い版にも戻せる（起動の知らせを待たない）
	os.WriteFile(e.bin+".prev", []byte("legacy\n"), 0o755)
	info = result(t, e.call("update_rollback", nil))
	if info["legacy"] != true {
		t.Fatalf("legacy %v", info)
	}
	os.Remove(e.bin + ".prev")
	if c := errCode(e.call("update_rollback", nil)); c != errNotFound {
		t.Fatalf("no prev: %s", c)
	}
}

// ---------- 係 ----------

func helperTimeouts(t *testing.T) {
	old := []time.Duration{updateReadyTimeout, updateConfirmTimeout, updateLegacyStable, updateStable, updateHelperDelay}
	updateReadyTimeout, updateConfirmTimeout, updateLegacyStable, updateStable, updateHelperDelay =
		500*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond, 0
	t.Cleanup(func() {
		updateReadyTimeout, updateConfirmTimeout, updateLegacyStable, updateStable, updateHelperDelay = old[0], old[1], old[2], old[3], old[4]
	})
}

// stageFor は、係に渡す plan を作る（受け取りと確かめは済んだことにする）。
func (e *updateEnv) stageFor(t *testing.T, ver string, script []byte) updatePlan {
	stage := newToken()
	dir := filepath.Join(e.upd.Dir, stage)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "lefthand"), []byte(ver+"\n"), 0o755)
	p := updatePlan{Stage: stage, Action: "update", Dir: e.upd.Dir, Unit: "lefthand.service", Bin: e.bin, Gadget: e.gadget,
		StagedBin: filepath.Join(dir, "lefthand"), Version: ver, From: "v1", Confirm: true, ReadyFile: e.sys.ready}
	if script != nil {
		os.WriteFile(filepath.Join(dir, "gadget-setup.sh"), script, 0o755)
		p.StagedGad = filepath.Join(dir, "gadget-setup.sh")
	}
	return p
}

// confirmWhenAsked は、係が update_confirm を待ち始めたら、新しいデーモンの代わりに書く。
func (e *updateEnv) confirmWhenAsked(stage string) {
	go func() {
		for i := 0; i < 200; i++ {
			if st, err := loadUpdateState(e.upd.Dir); err == nil && st.Phase == "waiting_confirm" {
				e.sys.mu.Lock()
				pid := e.sys.pid
				e.sys.mu.Unlock()
				b, _ := json.Marshal(map[string]any{"pid": pid})
				os.WriteFile(filepath.Join(e.upd.Dir, stage, updateConfirmFile), b, 0o644)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func (e *updateEnv) runHelper(p updatePlan) (bool, updateState) {
	h := &helper{p: p, sys: e.sys.ops(), sleep: time.Sleep, now: time.Now, logf: func(string, ...any) {}}
	ok := h.run()
	st, _ := loadUpdateState(e.upd.Dir)
	return ok, st
}

func readFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "(none)"
	}
	return strings.TrimSpace(string(b))
}

func TestHelperUpdateSucceeds(t *testing.T) {
	helperTimeouts(t)
	e := newUpdateEnv(t)
	script, _ := os.ReadFile("gadget-setup.sh")
	p := e.stageFor(t, "v2", append(script, []byte("\n# new\n")...))
	e.confirmWhenAsked(p.Stage)
	ok, st := e.runHelper(p)
	if !ok || st.Phase != "done" || !st.OK || st.Rebound { // スクリプトは何も変えなかった（偽の出力は done だけ）
		t.Fatalf("ok=%v state %+v", ok, st)
	}
	if readFile(t, e.bin) != "v2" || readFile(t, e.bin+".prev") != "v1" {
		t.Fatalf("bin %s prev %s", readFile(t, e.bin), readFile(t, e.bin+".prev"))
	}
	if !strings.HasSuffix(readFile(t, e.gadget), "# new") || strings.HasSuffix(readFile(t, e.gadget+".prev"), "# new") {
		t.Fatal("gadget-setup.sh was not replaced as a pair")
	}
	if len(e.sys.gadget) != 1 || !strings.Contains(strings.Join(e.sys.gadget[0], " "), "HID_MOUSE=0") {
		t.Fatalf("gadget runs %v", e.sys.gadget)
	}
	if c := e.sys.callList(); !strings.HasPrefix(c, "stop lefthand.service|start lefthand.service") {
		t.Fatalf("calls %s", c)
	}
	if _, err := os.Stat(filepath.Join(e.upd.Dir, p.Stage)); err == nil {
		t.Fatal("the stage was not removed")
	}
}

func TestHelperRollsBackWithoutConfirm(t *testing.T) {
	helperTimeouts(t)
	e := newUpdateEnv(t)
	p := e.stageFor(t, "v2", nil)
	ok, st := e.runHelper(p) // brain-deck がつなぎ直さない
	if ok || st.Phase != "rolled_back" || !strings.Contains(st.Message, "did not reconnect") {
		t.Fatalf("ok=%v state %+v", ok, st)
	}
	if readFile(t, e.bin) != "v1" || readFile(t, e.bin+".prev") != "v0" {
		t.Fatalf("not restored: bin %s prev %s", readFile(t, e.bin), readFile(t, e.bin+".prev"))
	}
	if !strings.HasSuffix(e.sys.callList(), "stop lefthand.service|start lefthand.service") || e.sys.pid == 0 {
		t.Fatalf("not restarted: %s", e.sys.callList())
	}
}

func TestHelperRollsBackWhenNotReady(t *testing.T) {
	helperTimeouts(t)
	e := newUpdateEnv(t)
	script, _ := os.ReadFile("gadget-setup.sh")
	p := e.stageFor(t, "broken", append(script, '\n'))
	p.Version = "broken"
	ok, st := e.runHelper(p)
	if ok || st.Phase != "rolled_back" || !strings.Contains(st.Message, "did not become ready") {
		t.Fatalf("ok=%v state %+v", ok, st)
	}
	if readFile(t, e.bin) != "v1" || readFile(t, e.gadget) != strings.TrimSpace(string(script)) {
		t.Fatal("not restored")
	}
	// 変わった gadget-setup.sh を実行したので、戻したあと前のものを実行し直す
	if len(e.sys.gadget) != 2 {
		t.Fatalf("gadget runs %d", len(e.sys.gadget))
	}
	// 版が違うデーモンが起動したときも戻す
	e2 := newUpdateEnv(t)
	p = e2.stageFor(t, "v2", nil)
	p.Version = "v9"
	if ok, st := e2.runHelper(p); ok || !strings.Contains(st.Message, "not v9") {
		t.Fatalf("version mismatch: %+v", st)
	}
}

func TestHelperRollbackAndLegacy(t *testing.T) {
	helperTimeouts(t)
	e := newUpdateEnv(t)
	info, err := e.upd.PrepareRollback()
	if err != nil {
		t.Fatal(err)
	}
	p := updatePlan{Stage: info.Stage, Action: "rollback", Dir: e.upd.Dir, Unit: "lefthand.service", Bin: e.bin, Gadget: e.gadget,
		StagedBin: filepath.Join(e.upd.Dir, info.Stage, "lefthand"), Version: "v0", Confirm: true, ReadyFile: e.sys.ready}
	e.confirmWhenAsked(p.Stage)
	if ok, st := e.runHelper(p); !ok {
		t.Fatalf("%+v", st)
	}
	// 戻したあと、今の版（v1）が .prev になる（もう一度 --rollback すれば v1 に戻る）
	if readFile(t, e.bin) != "v0" || readFile(t, e.bin+".prev") != "v1" {
		t.Fatalf("bin %s prev %s", readFile(t, e.bin), readFile(t, e.bin+".prev"))
	}
	// 起動を知らせない古い版：動き続ければよい
	os.WriteFile(e.bin+".prev", []byte("legacy\n"), 0o755)
	info, _ = e.upd.PrepareRollback()
	p = updatePlan{Stage: info.Stage, Action: "rollback", Dir: e.upd.Dir, Unit: "lefthand.service", Bin: e.bin, Gadget: e.gadget,
		StagedBin: filepath.Join(e.upd.Dir, info.Stage, "lefthand"), Legacy: true, ReadyFile: e.sys.ready}
	if ok, st := e.runHelper(p); !ok || readFile(t, e.bin) != "legacy" {
		t.Fatalf("%+v", st)
	}
}

func TestHelperRestart(t *testing.T) {
	helperTimeouts(t)
	e := newUpdateEnv(t)
	p := updatePlan{Stage: newToken(), Action: "restart", Dir: e.upd.Dir, Unit: "lefthand.service", Bin: e.bin, Gadget: e.gadget,
		Rebind: true, HIDMouse: true, ReadyFile: e.sys.ready}
	os.MkdirAll(filepath.Join(e.upd.Dir, p.Stage), 0o755)
	ok, st := e.runHelper(p)
	if !ok || !st.Rebound || len(e.sys.gadget) != 2 {
		t.Fatalf("ok=%v %+v gadget %v", ok, st, e.sys.gadget)
	}
	if env := strings.Join(e.sys.gadget[0], " "); !strings.Contains(env, "GADGET_UNBIND_ONLY=1") || !strings.Contains(env, "HID_MOUSE=1") {
		t.Fatalf("unbind env %s", env)
	}
	if readFile(t, e.bin) != "v1" {
		t.Fatal("restart changed the binary")
	}

	// 設定を書いて再起動し、起動しなければ前の設定に戻す
	os.WriteFile(e.bin, []byte("broken\n"), 0o755)
	os.WriteFile(e.path+".prev", []byte("old: config\n"), 0o644)
	os.WriteFile(e.path, []byte("new: config\n"), 0o644)
	p = updatePlan{Stage: newToken(), Action: "restart", Dir: e.upd.Dir, Unit: "lefthand.service", Bin: e.bin, Gadget: e.gadget,
		ReadyFile: e.sys.ready, Config: e.path}
	ok, st = e.runHelper(p)
	if ok || st.Phase != "rolled_back" || readFile(t, e.path) != "old: config" {
		t.Fatalf("config restore: ok=%v %+v %s", ok, st, readFile(t, e.path))
	}
}

// ---------- 設定のテキスト、再起動、電源、ログ、画像の共通の受け取り ----------

func TestConfigText(t *testing.T) {
	e := newUpdateEnv(t)
	r := result(t, e.call("get_config_text", nil))
	if r["text"] != testConfig || r["path"] != e.path {
		t.Fatalf("get %v", r)
	}
	// コメントを残したまま置き換え、すぐ反映する
	text := strings.Replace(testConfig, `"ブラシ" }`, `"ペン" }  # 変えた`, 1)
	r = result(t, e.call("put_config_text", map[string]any{"text": text, "source": "test"}))
	if r["restarting"] != false || readFile(t, e.path) != strings.TrimSpace(text) || readFile(t, e.path+".prev") != strings.TrimSpace(testConfig) {
		t.Fatalf("put %v", r)
	}
	if !strings.Contains(string(mustJSON(e.currentJSON())), "ペン") {
		t.Fatal("not applied")
	}
	if p := result(t, e.call("get_config_text", map[string]any{"prev": true})); p["text"] != testConfig {
		t.Fatal("prev")
	}
	// 誤りは書かない
	m := e.call("put_config_text", map[string]any{"text": "layers: [{name: base, keys: {KEY_Q: NOPE}}]"})
	if errCode(m) != errInvalid || readFile(t, e.path) != strings.TrimSpace(text) {
		t.Fatalf("invalid: %v", m)
	}
	// 再起動が要る項目
	re := strings.Replace(text, "hid_device: /dev/hidg0", "hid_device: /dev/hidg1", 1)
	if m := e.call("put_config_text", map[string]any{"text": re}); errCode(m) != errRestart || readFile(t, e.path) != strings.TrimSpace(text) {
		t.Fatalf("needs restart: %v", m)
	}
	if r := result(t, e.call("put_config_text", map[string]any{"text": re, "dry_run": true})); r["needs_restart"] != true || r["valid"] != true {
		t.Fatalf("dry run %v", r)
	}
	r = result(t, e.call("put_config_text", map[string]any{"text": re, "restart": true}))
	if r["restarting"] != true || !strings.Contains(readFile(t, e.path), "hidg1") || len(e.sys.runs) != 1 {
		t.Fatalf("restart %v runs %v", r, e.sys.runs)
	}
	var p updatePlan
	b, _ := os.ReadFile(e.sys.runs[0][3])
	json.Unmarshal(b, &p)
	if p.Action != "restart" || p.Config != e.path {
		t.Fatalf("plan %+v", p)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestRestartPowerLogs(t *testing.T) {
	e := newUpdateEnv(t)
	r := result(t, e.call("restart", map[string]any{"usb": true}))
	if r["usb"] != true || len(e.sys.runs) != 1 {
		t.Fatalf("restart %v", r)
	}
	old := powerDelay
	powerDelay = 0
	defer func() { powerDelay = old }()
	if c := errCode(e.call("power", map[string]any{"action": "explode"})); c != errBadRequest {
		t.Fatal(c)
	}
	result(t, e.call("power", map[string]any{"action": "reboot"}))
	for i := 0; i < 100 && !strings.Contains(e.sys.callList(), "--no-block reboot"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(e.sys.callList(), "--no-block reboot") {
		t.Fatalf("calls %s", e.sys.callList())
	}

	e.sys.journal = "2026-10-09T10:00:00+0900 lefthand[1]: a\n2026-10-09T10:00:01+0900 lefthand[1]: b\n-- cursor: s=abc\n"
	r = result(t, e.call("get_logs", map[string]any{"lines": 50}))
	if r["text"] != "2026-10-09T10:00:00+0900 lefthand[1]: a\n2026-10-09T10:00:01+0900 lefthand[1]: b" || r["cursor"] != "s=abc" {
		t.Fatalf("logs %v", r)
	}
	e.sys.journal = ""
	r = result(t, e.call("get_logs", map[string]any{"cursor": "s=abc"}))
	if r["text"] != "" || r["cursor"] != "s=abc" || !strings.Contains(e.sys.callList(), "--after-cursor=s=abc") {
		t.Fatalf("follow %v %s", r, e.sys.callList())
	}
}

func TestUploadKindImage(t *testing.T) {
	e := newUpdateEnv(t)
	b := testImage(40, 30, 7)
	id, _ := imageIDOf(b)
	r := result(t, e.send("image", b))
	if r["id"] != id || !e.images.Has(id) {
		t.Fatalf("image %v", r)
	}
	if c := errCode(e.send("image", []byte("LHI1 not an image"))); c != errBadRequest {
		t.Fatalf("bad image: %s", c)
	}
	if c := errCode(e.call("upload_begin", map[string]any{"kind": "font", "sha256": strings.Repeat("a", 64), "bytes": 3})); c != errBadRequest {
		t.Fatal(c)
	}
	if parts(t, e.images.dir) != 0 {
		t.Fatal("part files left")
	}
}
