package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAppManifest(t *testing.T) {
	files := map[string]bool{"app.yaml": true, "main.sh": true, "run.py": true}
	d, err := parseAppManifest([]byte("title: サイコロ\nevery: 30s\nkeys: [left, space, a, \"1\"]\n"), "dice", files)
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "サイコロ" || d.Run != "main.sh" || d.Every != 30*time.Second || d.Timeout != appDefaultTimeout || d.MemMB != appDefaultMemMB || !d.Keys["a"] || !d.Keys["1"] {
		t.Errorf("%+v", d)
	}
	d, err = parseAppManifest(nil, "x", files) // 空の app.yaml でもよい
	if err != nil || d.Title != "x" || d.Stream {
		t.Fatalf("%+v %v", d, err)
	}
	d, err = parseAppManifest([]byte("mode: stream\nrun: run.py\n"), "load", files)
	if err != nil || !d.Stream || d.Run != "run.py" {
		t.Fatalf("%+v %v", d, err)
	}
	d, _ = parseAppManifest([]byte("every: off\n"), "x", files)
	if d.Every != 0 {
		t.Error("every off")
	}
	for _, bad := range []string{
		"run: ../x.sh\n", "run: /bin/sh\n", "run: other.sh\n", "run: app.yaml\n", "mode: daemon\n", "every: 1s\n", "timeout: 2m\n",
		"memory_mb: 4\n", "keys: [f1]\n", "name: other\n", "mode: stream\nevery: 5s\n", "unknown: 1\n", "icon: nope\n",
	} {
		if _, err := parseAppManifest([]byte(bad), "x", files); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	for _, name := range []string{"", "Dice", "../x", "a/b", ".x", strings.Repeat("a", 33)} {
		if _, err := parseAppManifest(nil, name, files); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestParseAppOutput(t *testing.T) {
	sc := parseAppOutput([]byte("@title SD カード\nふつうの行\n@big 42%\n@color red\n赤い行\n@color\n@row 使用 | 3.1 GB\n@bar 30 60 半分\n@item a 一つ目\n@sep\n@button roll 振る\n@input name 名前 | 名前を入れる\n@@at\n@nope x\n"))
	if sc.Title != "SD カード" {
		t.Errorf("title %q", sc.Title)
	}
	kinds := ""
	for _, l := range sc.Lines {
		kinds += string(l.Kind)
	}
	if kinds != "tbtrpistt" {
		t.Fatalf("kinds %q (%+v)", kinds, sc.Lines)
	}
	if !sc.Lines[2].HasInk || sc.Lines[2].Ink != colInkError || sc.Lines[3].HasInk {
		t.Error("color")
	}
	if sc.Lines[3].Text != "使用" || sc.Lines[3].Right != "3.1 GB" {
		t.Errorf("row %+v", sc.Lines[3])
	}
	if l := sc.Lines[4]; l.Value != 30 || l.Max != 60 || l.Text != "半分" {
		t.Errorf("bar %+v", l)
	}
	if l := sc.Lines[5]; l.ID != "a" || l.Text != "一つ目" {
		t.Errorf("item %+v", l)
	}
	if sc.Lines[7].Text != "@at" || !sc.Lines[8].Unparsed {
		t.Errorf("escape/unknown %+v %+v", sc.Lines[7], sc.Lines[8])
	}
	if len(sc.Buttons) != 2 || sc.Buttons[0].ID != "roll" || sc.Buttons[0].Input || !sc.Buttons[1].Input || sc.Buttons[1].Prompt != "名前を入れる" || sc.Buttons[1].Label != "名前" {
		t.Errorf("buttons %+v", sc.Buttons)
	}
	if sc := parseAppOutput([]byte("a\n@clear\nb\n")); len(sc.Lines) != 1 || sc.Lines[0].Text != "b" {
		t.Errorf("clear %+v", sc.Lines)
	}
	// 制御文字は捨てる、行の数は上限まで
	if sc := parseAppOutput([]byte("a\x1b[31mb\n")); sc.Lines[0].Text != "a[31mb" {
		t.Errorf("control %q", sc.Lines[0].Text)
	}
	if sc := parseAppOutput([]byte(strings.Repeat("x\n", appMaxScreen+50))); len(sc.Lines) != appMaxScreen {
		t.Errorf("lines %d", len(sc.Lines))
	}
}

// mkAppTar は、名前と中身の tar.gz を作る。
func mkAppTar(t *testing.T, files map[string]string, extra ...*tar.Header) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func TestAppInstallRejectsUnsafe(t *testing.T) {
	dir := t.TempDir()
	s := OpenAppStore(dir)
	os.MkdirAll(s.dir, 0o755)
	ok := map[string]string{"app.yaml": "title: T\n", "main.sh": "echo hi\n"}
	if _, err := s.install("good", bytes.NewReader(mkAppTar(t, ok))); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get("good"); d == nil || d.Title != "T" {
		t.Fatal("not installed")
	}
	cases := map[string][]byte{
		"traversal": mkAppTar(t, map[string]string{"app.yaml": "", "main.sh": "", "../evil": "x"}),
		"subdir":    mkAppTar(t, map[string]string{"app.yaml": "", "main.sh": "", "a/b": "x"}),
		"symlink":   mkAppTar(t, map[string]string{"app.yaml": "", "main.sh": ""}, &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"}),
		"absolute":  mkAppTar(t, map[string]string{"app.yaml": "", "main.sh": "", "/etc/x": "x"}),
		"no-yaml":   mkAppTar(t, map[string]string{"main.sh": ""}),
		"no-run":    mkAppTar(t, map[string]string{"app.yaml": "run: x.sh\n"}),
		"big":       mkAppTar(t, map[string]string{"app.yaml": "", "main.sh": strings.Repeat("x", appMaxFileBytes+1)}),
		"not-gzip":  []byte("hello"),
	}
	for name, b := range cases {
		_, err := s.install("bad", bytes.NewReader(b))
		if err == nil {
			t.Errorf("%s: installed", name)
		} else if name != "not-gzip" && !errors.Is(err, errAppBad) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "bad")); err == nil {
		t.Error("bad app left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Error("wrote outside")
	}
	// 一時のフォルダは残らない
	ents, _ := os.ReadDir(s.dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left %s", e.Name())
		}
	}
	// 置き場所に手で置いたリンクは使わない
	os.MkdirAll(filepath.Join(s.dir, "linked"), 0o755)
	os.WriteFile(filepath.Join(s.dir, "linked", "app.yaml"), nil, 0o644)
	os.Symlink("/bin/sh", filepath.Join(s.dir, "linked", "main.sh"))
	s.Reload()
	if d, bad := s.Get("linked"); d != nil || bad == "" {
		t.Errorf("symlinked app usable: %v %q", d, bad)
	}
	// 入れ替えと消す（データも消す）
	os.MkdirAll(filepath.Join(dir, appsDataDirName, "good"), 0o755)
	if _, err := s.install("good", bytes.NewReader(mkAppTar(t, map[string]string{"app.yaml": "title: T2\n", "main.sh": ""}))); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get("good"); d.Title != "T2" {
		t.Error("not replaced")
	}
	if err := s.Remove("good"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, appsDataDirName, "good")); err == nil {
		t.Error("data left")
	}
	if err := s.Remove("../x"); !errors.Is(err, errAppMissing) {
		t.Errorf("remove ../x: %v", err)
	}
}

// newTestApps は、スクリプトを自分の権限で動かす係を作る。
func newTestApps(t *testing.T, apps map[string]map[string]string) *AppService {
	t.Helper()
	dir := t.TempDir()
	for name, files := range apps {
		p := filepath.Join(dir, appsDirName, name)
		os.MkdirAll(p, 0o755)
		for n, c := range files {
			os.WriteFile(filepath.Join(p, n), []byte(c), 0o644)
		}
	}
	s := NewAppService(OpenAppStore(dir), nil)
	s.runAsMe = true
	t.Cleanup(s.Shutdown)
	return s
}

// waitApp は、cond が真になるまで待つ。
func waitApp(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func screenText(v *AppView) string {
	if v == nil || v.Screen == nil {
		return ""
	}
	var out []string
	for _, l := range v.Screen.Lines {
		out = append(out, l.Text)
	}
	return strings.Join(out, "|")
}

func TestAppIntervalRunsAndEvents(t *testing.T) {
	s := newTestApps(t, map[string]map[string]string{
		"echo": {"app.yaml": "every: off\n", "main.sh": `echo "args=$*"; echo "ev=$BRAIN_EVENT"; [ "$1" = input ] && read x && echo "in=$x"; pwd; echo "$HOME"`},
	})
	s.Seen("echo")
	s.tick()
	waitApp(t, "first run", func() bool { return strings.HasPrefix(screenText(s.View("echo")), "args=|") })
	v := s.View("echo")
	if !strings.HasSuffix(screenText(v), filepath.Join(s.store.dataDir, "echo")+"|"+filepath.Join(s.store.dataDir, "echo")) {
		t.Errorf("cwd/home: %q", screenText(v))
	}
	s.Event("echo", appEvent{args: []string{"button", "roll"}})
	waitApp(t, "button", func() bool { return strings.HasPrefix(screenText(s.View("echo")), "args=button roll|ev=button roll") })
	s.Event("echo", appEvent{args: []string{"input", "name"}, stdin: "日本語"})
	waitApp(t, "input", func() bool { return strings.Contains(screenText(s.View("echo")), "in=日本語") })
	// 入力の欄の種類
	if n, id, ok := parseAppInputKind(appInputKind("echo", "name")); !ok || n != "echo" || id != "name" {
		t.Error("input kind")
	}
}

func TestAppLimits(t *testing.T) {
	s := newTestApps(t, map[string]map[string]string{
		"slow":  {"app.yaml": "every: off\ntimeout: 1s\n", "main.sh": "echo before; sleep 30 & sleep 30; echo after"},
		"loud":  {"app.yaml": "every: off\n", "main.sh": "yes 0123456789"},
		"fail":  {"app.yaml": "every: off\n", "main.sh": "echo out; echo oops >&2; exit 3"},
		"mem":   {"app.yaml": "every: off\nmemory_mb: 16\n", "main.sh": `x=$(head -c 40000000 /dev/zero | tr '\0' a); echo ${#x}`},
		"shell": {"app.yaml": "every: off\n", "main.sh": "#!/no/such/interp\necho x"},
	})
	for _, n := range []string{"slow", "loud", "fail", "mem", "shell"} {
		s.Seen(n)
	}
	t0 := time.Now()
	go s.runInterval("slow", s.states["slow"], appEvent{})
	go s.runInterval("loud", s.states["loud"], appEvent{})
	go s.runInterval("fail", s.states["fail"], appEvent{})
	waitApp(t, "slow stopped", func() bool { return strings.Contains(s.View("slow").Status, "時間の上限") })
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("timeout took %v", d)
	}
	if screenText(s.View("slow")) != "before" {
		t.Errorf("slow screen %q", screenText(s.View("slow")))
	}
	waitApp(t, "loud stopped", func() bool { return strings.Contains(s.View("loud").Status, "出力が") })
	waitApp(t, "fail", func() bool { return s.View("fail").Status != "" })
	if v := s.View("fail"); v.Status != "終了コード 3：oops" || v.Level != noticeWarn || screenText(v) != "out" {
		t.Errorf("fail %+v %q", v, screenText(v))
	}
	go s.runInterval("mem", s.states["mem"], appEvent{})
	waitApp(t, "mem", func() bool { v := s.View("mem"); return !v.Running && !v.LastRun.IsZero() })
	if v := s.View("mem"); screenText(v) == "40000000" {
		t.Errorf("memory limit not applied: %+v", v)
	}
	go s.runInterval("shell", s.states["shell"], appEvent{})
	waitApp(t, "shell", func() bool { return strings.Contains(s.View("shell").Status, "動かせません") })
	waitApp(t, "slots freed", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.slots == 0 })
}

func TestAppConcurrencyLimit(t *testing.T) {
	apps := map[string]map[string]string{}
	for _, n := range []string{"a", "b", "c", "d"} {
		apps[n] = map[string]string{"app.yaml": "every: off\n", "main.sh": "sleep 1; echo done"}
	}
	s := newTestApps(t, apps)
	for _, n := range []string{"a", "b", "c", "d"} {
		s.View(n) // 状態を作る（Seen と違って、時間で動かさない）
	}
	s.mu.Lock()
	sts := []*appState{s.states["a"], s.states["b"], s.states["c"], s.states["d"]}
	s.mu.Unlock()
	for i, n := range []string{"a", "b", "c"} {
		go s.runInterval(n, sts[i], appEvent{})
	}
	waitApp(t, "3 running", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.slots == 3 })
	s.runInterval("d", sts[3], appEvent{})
	if v := s.View("d"); !strings.Contains(v.Status, "待っています") || v.Running {
		t.Errorf("d: %+v", v)
	}
	waitApp(t, "done", func() bool { return screenText(s.View("a")) == "done" && screenText(s.View("c")) == "done" })
}

func TestAppStream(t *testing.T) {
	s := newTestApps(t, map[string]map[string]string{
		"st": {"app.yaml": "mode: stream\n", "main.sh": `echo one; echo two; while read ev; do echo @clear; echo "got $ev"; done`},
	})
	s.Seen("st")
	s.tick()
	waitApp(t, "lines", func() bool { return screenText(s.View("st")) == "one|two" })
	s.Event("st", appEvent{args: []string{"button", "x"}})
	waitApp(t, "event", func() bool { return screenText(s.View("st")) == "got button x" })
	s.Event("st", appEvent{args: []string{"input", "q"}, stdin: "a\nb"})
	waitApp(t, "input", func() bool { return screenText(s.View("st")) == "got input q a b" })
	// 画面から消えて時間がたったら止める
	s.mu.Lock()
	s.states["st"].seen = time.Now().Add(-appStreamHide - time.Second)
	s.mu.Unlock()
	s.tick()
	waitApp(t, "stopped", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.states["st"].proc == nil && s.slots == 0 })
}

func TestAppStreamRestartsAndLimitsOutput(t *testing.T) {
	s := newTestApps(t, map[string]map[string]string{
		"flood": {"app.yaml": "mode: stream\n", "main.sh": `while :; do echo 0123456789012345678901234567890123456789; done`},
		"quit":  {"app.yaml": "mode: stream\n", "main.sh": `echo bye; exit 1`},
	})
	s.Seen("flood")
	s.Seen("quit")
	s.tick()
	waitApp(t, "flood stopped", func() bool { return strings.Contains(s.View("flood").Status, "出力が") })
	waitApp(t, "quit", func() bool { return strings.Contains(s.View("quit").Status, "動かし直します") })
	s.mu.Lock()
	next := s.states["quit"].nextRun
	s.mu.Unlock()
	if d := time.Until(next); d < 3*time.Second || d > appRestartMin {
		t.Errorf("restart in %v", d)
	}
}

func TestAppInterpreter(t *testing.T) {
	dir := t.TempDir()
	w := func(s string) string {
		p := filepath.Join(dir, "s")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	if a, err := appInterpreter(w("echo")); err != nil || strings.Join(a, " ") != "/bin/sh" {
		t.Error(a, err)
	}
	if a, err := appInterpreter(w("#!/bin/sh -e\necho")); err != nil || strings.Join(a, " ") != "/bin/sh -e" {
		t.Error(a, err)
	}
	if a, err := appInterpreter(w("#!/usr/bin/env sh\n")); err != nil || !strings.HasSuffix(a[0], "/sh") {
		t.Error(a, err)
	}
	for _, bad := range []string{"#!sh\n", "#!/no/such\n", "#!/bin/sh a b\n", "#!/usr/bin/env no-such-interp-x\n"} {
		if _, err := appInterpreter(w(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAppWidgetDraw(t *testing.T) {
	if _, err := compileWidget(ActionSpec{Widget: widgetApp, Name: "dice"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ActionSpec{{Widget: widgetApp}, {Widget: widgetApp, Name: "../x"}, {Widget: widgetClock, Name: "x"}, {Widget: widgetApp, Name: "x", Key: "A"}} {
		if _, err := compileWidget(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	w := &WidgetDef{Kind: widgetApp, ID: "dice"}
	sc := parseAppOutput([]byte("@title T\n@big 6\n@item a 行\n@bar 1 2\n" + strings.Repeat("長い行 ", 200) + "\n@button roll 振る\n@input n 名前\n"))
	d := &AppDef{Name: "dice", Title: "サイコロ"}
	for _, size := range []image.Point{{800, 480}, {200, 140}, {80, 60}} {
		cv := NewCanvas(size.X, size.Y, size.X*rgb565.Bpp, rgb565, 0)
		area := image.Rect(0, 0, size.X, size.Y)
		for _, v := range []*AppView{nil, {Bad: "x"}, {}, {Def: d}, {Def: d, Screen: sc, Status: "止めました：x", Level: noticeError}} {
			drawApp(cv, area, w, WidgetEnv{AppViews: map[string]*AppView{"dice": v}})
		}
	}
	g := appLayout(image.Rect(0, 0, 400, 300), sc, "", false)
	if len(g.buttons) != 2 || g.scale != 2 || !g.more {
		t.Errorf("layout %+v", g)
	}
	for i := 1; i < len(g.lines); i++ {
		if g.lines[i].r.Min.Y < g.lines[i-1].r.Max.Y || g.lines[i].r.Max.Y > g.body.Max.Y {
			t.Errorf("line %d overlaps %v %v", i, g.lines[i-1].r, g.lines[i].r)
		}
	}
	// stream は最後の行を残す
	long := parseAppOutput([]byte(strings.Repeat("x\n", 50) + "last\n"))
	gt := appLayout(image.Rect(0, 0, 400, 300), long, "", true)
	if gt.lines[len(gt.lines)-1].text != "last" {
		t.Error("tail")
	}
	if appCaption(w, "", WidgetEnv{AppViews: map[string]*AppView{"dice": {Def: d, Screen: sc}}}) != "T" ||
		appCaption(w, "L", WidgetEnv{}) != "L" || appCaption(w, "", WidgetEnv{AppViews: map[string]*AppView{"dice": {Def: d}}}) != "サイコロ" {
		t.Error("caption")
	}
	if appKeyName(appKeyCodes["q"]) != "q" || appKeyName(appKeyCodes["5"]) != "5" || appKeyName(appKeyCodes["left"]) != "left" {
		t.Error("key names")
	}
}

// contrib/apps の例を、自分の権限で動かしてみる（出力に知らない指示がないこと）。
func TestContribApps(t *testing.T) {
	dir := t.TempDir()
	s := OpenAppStore(dir)
	os.MkdirAll(s.dir, 0o755)
	for _, name := range []string{"sdcard", "dice", "load"} {
		files := map[string]string{}
		ents, _ := os.ReadDir(filepath.Join("contrib/apps", name))
		for _, e := range ents {
			b, _ := os.ReadFile(filepath.Join("contrib/apps", name, e.Name()))
			files[e.Name()] = string(b)
		}
		if _, err := s.install(name, bytes.NewReader(mkAppTar(t, files))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	a := NewAppService(s, nil)
	a.runAsMe = true
	defer a.Shutdown()
	check := func(name string, v *AppView) {
		t.Helper()
		if v.Screen == nil || len(v.Screen.Lines) == 0 {
			t.Fatalf("%s: no output (%q)", name, v.Status)
		}
		for _, l := range v.Screen.Lines {
			if l.Unparsed {
				t.Errorf("%s: unknown directive %q", name, l.Text)
			}
		}
		if v.Status != "" {
			t.Errorf("%s: status %q", name, v.Status)
		}
	}
	a.View("sdcard")
	a.runInterval("sdcard", a.states["sdcard"], appEvent{})
	check("sdcard", a.View("sdcard"))
	a.View("dice")
	a.runInterval("dice", a.states["dice"], appEvent{args: []string{"input", "sides"}, stdin: "20"})
	a.runInterval("dice", a.states["dice"], appEvent{args: []string{"button", "two"}})
	v := a.View("dice")
	check("dice", v)
	if v.Screen.Title != "サイコロ（20 面）" || v.Screen.Lines[0].Kind != 'b' || len(strings.Fields(v.Screen.Lines[0].Text)) != 2 {
		t.Errorf("dice: %q %+v", v.Screen.Title, v.Screen.Lines[0])
	}
	a.Seen("load")
	a.tick()
	waitApp(t, "load", func() bool { v := a.View("load"); return v.Screen != nil && len(v.Screen.Buttons) == 3 })
	a.Event("load", appEvent{args: []string{"button", "fast"}})
	waitApp(t, "load fast", func() bool { v := a.View("load"); return v.Screen != nil && strings.Contains(v.Screen.Title, "1 秒") })
	check("load", a.View("load"))
}
