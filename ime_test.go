package main

import (
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 日本語の入力（ime.go） ----------

func TestIMEInstalledAndChoose(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "installed-modules.scm")
	os.WriteFile(p, []byte(";; generated\n(define installed-im-module-list '(\"anthy-utf8\" \"skk\"))\n(define installed-im-list '(anthy-utf8 skk))\n(define enabled-im-list installed-im-list)\n"), 0o644)
	got := imeInstalled(p)
	if strings.Join(got, ",") != "anthy-utf8,skk" {
		t.Fatalf("installed = %q", got)
	}
	if imeInstalled(filepath.Join(dir, "none")) != nil {
		t.Fatal("missing file should give nil")
	}
	for _, c := range []struct {
		method string
		inst   []string
		want   string
	}{
		{imeAuto, []string{"skk", "anthy-utf8"}, "anthy-utf8"},
		{imeAuto, []string{"skk", "anthy"}, "anthy"},
		{imeAuto, []string{"skk"}, "skk"},
		{imeAuto, nil, ""},
		{imeOff, []string{"anthy-utf8"}, ""},
		{"skk", []string{"anthy-utf8"}, "skk"},
		{"", []string{"anthy-utf8"}, "anthy-utf8"},
	} {
		if got := imeChoose(c.method, c.inst); got != c.want {
			t.Errorf("choose(%q, %v) = %q, want %q", c.method, c.inst, got, c.want)
		}
	}
}

func TestIMEModeFromStatus(t *testing.T) {
	for _, c := range []struct {
		s      string
		mode   string
		status bool
	}{
		{"anthy-utf8[An- Ｒ]", "英数", true},
		{"anthy-utf8[Anあ Ｒ]", "かな", true},
		{"anthy-utf8[Anア か]", "カナ", true},
		{"skk[-]", "英数", true},
		{"skk[あ]", "かな", true},
		{"1:日本語 2:にほんご 3:ニホンゴ [2/3]", "", false},
		{"", "", false},
	} {
		mode, isStatus := imeMode(c.s)
		if mode != c.mode || isStatus != c.status {
			t.Errorf("imeMode(%q) = %q %v, want %q %v", c.s, mode, isStatus, c.mode, c.status)
		}
	}
}

// 実機の uim-fep 1.9.6（anthy-utf8、-s lastline、50 桁 × 2 行）が出したバイト列（2026-10-11 に記録）。
func TestTermUimFepSequences(t *testing.T) {
	tm := NewTerm(50, 2, 0, nil)
	tm.Write([]byte("\x1b[6n\n\x1b[1;2H\x1b[6n\x1b[1;1H\x1b[6n\x1b[1;1r\x1b[2;1H\x1b[39;49manthy-utf8[An- \xef\xbc\xb2]\x1b[K\x1b[1;1H"))
	if got := tm.LineText(1); got != "anthy-utf8[An- Ｒ]" {
		t.Fatalf("status = %q", got)
	}
	tm.Write([]byte("\x1b(B\x1b[ma"))                                                                                   // a（英数）
	tm.Write([]byte("\x1b[6n\x1b[2;14H\x1b[39;49m\xe3\x81\x82\x1b[1;1H"))                                               // Ctrl+Space：モードが あ に
	tm.Write([]byte("\x1b[4mn\x1b[1;1H\xe3\x81\xabh\x1b[1;3H\xe3\x81\xbbn\x1b[1;5H\xe3\x82\x93g\x1b[1;7H\xe3\x81\x94")) // nihongo
	if got := tm.LineText(1); got != "anthy-utf8[AnあＲ]" {                                                               // あ が「- 」の 2 桁に重なる
		t.Fatalf("status after toggle = %q", got)
	}
	// 上書きされて "a" は消え、1 行目は変換中の にほんご（下線）
	if got := tm.LineText(0); got != "にほんご" {
		t.Fatalf("line = %q", got)
	}
	if c := tm.line(0)[0]; c.r != 'に' || c.w != 2 || c.at&attrUnderline == 0 {
		t.Fatalf("cell = %+v", c)
	}
	// スペース：日本語（反転）。もう一度：候補の一覧が 2 行目に
	tm.Write([]byte("\x1b[1;1H\x1b(B\x1b[m\x1b[7;39;49m\xe6\x97\xa5\xe6\x9c\xac\xe8\xaa\x9e\x1b(B\x1b[m\x1b[39;49m  \x1b[1;1H"))
	if got := tm.LineText(0); got != "日本語" {
		t.Fatalf("converted = %q", got)
	}
	if c := tm.line(0)[0]; c.at&attrReverse == 0 || c.at&attrUnderline != 0 {
		t.Fatalf("cell = %+v", c)
	}
	tm.Write([]byte("\x1b[2;1H1:\xe6\x97\xa5\xe6\x9c\xac\xe8\xaa\x9e 2:\x1b[7m\xe3\x81\xab\xe3\x81\xbb\xe3\x82\x93\xe3\x81\x94\x1b(B\x1b[m\x1b[39;49m 3:\xe3\x83\x8b\xe3\x83\x9b\xe3\x83\xb3\xe3\x82\xb4 [2/3]\x1b[1;1H\x1b[7m\xe3\x81\xab\xe3\x81\xbb\xe3\x82\x93\xe3\x81\x94\x1b[1;1H"))
	if got := tm.LineText(1); got != "1:日本語 2:にほんご 3:ニホンゴ [2/3]" {
		t.Fatalf("candidates = %q", got)
	}
	if mode, isStatus := imeMode(tm.LineText(1)); isStatus || mode != "" {
		t.Fatal("candidates taken as the status line")
	}
	cells := imeCells(tm.line(1), false)
	if len(cells) != 36 || !cells[11].Reverse || cells[0].Reverse {
		t.Fatalf("candidate cells = %d %+v", len(cells), cells[11])
	}
	// Enter：確定。状態の行に戻る。1 行目は にほんご（飾りなし）
	tm.Write([]byte("\x1b[2;1H\x1b(B\x1b[m\x1b[39;49manthy-utf8[An\xe3\x81\x82\xef\xbc\xb2]\x1b[K\x1b[1;1H        \x1b[1;1H\x1b(B\x1b[m\xe3\x81\xab\xe3\x81\xbb\xe3\x82\x93\xe3\x81\x94"))
	if got := tm.LineText(0); got != "にほんご" {
		t.Fatalf("committed = %q", got)
	}
	if c := tm.line(0)[0]; c.at != 0 {
		t.Fatalf("committed cell has attrs: %+v", c)
	}
	if x, _ := tm.Cursor(); x != 8 {
		t.Fatalf("cursor = %d", x)
	}
	// 結果（line.sh の OSC）は OnOSC で受け取り、端末には出さない
	var got []string
	tm.OnOSC = func(num, rest string) bool { got = append(got, num+"|"+rest); return num == imeOSC }
	tm.Write([]byte("\r\n\x1b]7777;ok;a\xe3\x81\xab\xe3\x81\xbb\xe3\x82\x93\xe3\x81\x94\x07\x1b(B\x1b[m\x1b[6n\x1b[1;2r\x1b[2;1H\x1b[K\x1b[1;1H"))
	if len(got) != 1 || got[0] != "7777|ok;aにほんご" {
		t.Fatalf("osc = %q", got)
	}
	tm.Write([]byte("\x1b]2;title\x07"))
	if tm.Title != "title" {
		t.Fatalf("other OSC not handled: %q", tm.Title)
	}
}

// fakeIME は、偽の uim-fep（testdata/ime/uim-fep。bash の readline で行を読む本物の line.sh を動かす）で IME を作る。
func fakeIME(t *testing.T, cfg *Config) (*IME, chan [2]string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	fake, _ := filepath.Abs("testdata/ime/uim-fep")
	dir := t.TempDir()
	m := NewIME(dir, func() *Config { return cfg })
	m.lookPath = func(s string) (string, error) { return fake, nil }
	m.installed = func() []string { return []string{"anthy-utf8"} }
	m.start = func(user string, argv, env []string, _ string, cols, rows int) (*ptyConn, error) {
		if user != "user" || argv[0] != "uim-fep" || argv[2] != "anthy-utf8" {
			t.Errorf("start %s %q", user, argv)
		}
		ptmx, pts, _, err := openPTY(cols, rows)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("sh", append([]string{fake}, argv[1:]...)...)
		cmd.Env = append(env, "HOME="+dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		if err := cmd.Start(); err != nil {
			pts.Close()
			ptmx.Close()
			return nil, err
		}
		pts.Close()
		p := &ptyConn{File: ptmx, cmd: cmd, done: make(chan struct{})}
		go func() { p.err = cmd.Wait(); close(p.done) }()
		return p, nil
	}
	results := make(chan [2]string, 16)
	m.SetHandlers(func(ok bool, text string) {
		results <- [2]string{map[bool]string{true: "ok", false: "cancel"}[ok], text}
	}, func() {})
	t.Cleanup(m.Stop)
	return m, results
}

func waitResult(t *testing.T, ch chan [2]string, want [2]string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("result = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no result (want %q)", want)
	}
}

func TestIMEFieldLifecycle(t *testing.T) {
	cfg := &Config{IME: &IMEConfig{Idle: "300ms"}}
	m, results := fakeIME(t, cfg)
	if in := m.Info(); in.Method != "anthy-utf8" || !in.UimFep || in.Running || in.Setting != imeAuto {
		t.Fatalf("info = %+v", in)
	}
	// 開く前に打ったキーは、起動が終わってから届く
	if !m.Open(40, "", imeStartLatin) {
		t.Fatal("open failed")
	}
	m.Send([]byte("abc\r"))
	waitResult(t, results, [2]string{"ok", "abc"})
	if m.Active() {
		t.Fatal("still open after the result")
	}
	for _, name := range []string{"uim.scm", "inputrc", "line.sh"} {
		if st, err := os.Stat(filepath.Join(m.dir, name)); err != nil || st.Mode().Perm() != 0o644 {
			t.Fatalf("%s: %v %v", name, err, st)
		}
	}
	// 最初の文字（英数から始める）：Ctrl+^ は readline では何もしない
	m.Open(40, "2026-10-11 23:00", imeStartLatin)
	waitFor(t, "ready", func() bool { v := m.View(); return v != nil && v.Busy == "" })
	waitFor(t, "initial text", func() bool { return strings.TrimSpace(imeText(m.View().Line)) == "2026-10-11 23:00" })
	if v := m.View(); v.Mode != "英数" || v.Cursor != 16 || v.Candidates != nil {
		t.Fatalf("view = %+v", v)
	}
	m.Send([]byte("\x7f\x7f55\r"))
	waitResult(t, results, [2]string{"ok", "2026-10-11 23:55"})
	// やめる：Ctrl+] のマクロ
	m.Open(40, "", imeStartLatin)
	m.Send([]byte("xyz"))
	m.Cancel()
	waitResult(t, results, [2]string{"cancel", ""})
	// 結果を待たずに閉じる：行は消え、その cancel は捨てる。次に開いたときはまっさら
	m.Open(40, "", imeStartLatin)
	m.Send([]byte("left"))
	m.Close()
	m.Open(40, "", imeStartLatin)
	m.Send([]byte("q\r"))
	waitResult(t, results, [2]string{"ok", "q"})
	select {
	case r := <-results:
		t.Fatalf("stray result %q", r)
	case <-time.After(200 * time.Millisecond):
	}
	// 使わなくなったら止まる（idle 300ms）
	waitFor(t, "idle stop", func() bool { return !m.Info().Running })
	// 方式を off にしたら開けない
	cfg.IME.Method = imeOff
	if m.Open(40, "", imeStartLatin) {
		t.Fatal("opened with ime off")
	}
}

func TestIMESplit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"abc", []string{"abc"}},
		{"\x1e2026-10-11 23:37", []string{"\x1e", "2026-10-11 23:37"}},
		{"ab\r\x1d", []string{"ab", "\r", "\x1d"}},
		{"\x1b[1;2D\x1b[Ax\x1bb\x1b", []string{"\x1b[1;2D", "\x1b[A", "x", "\x1bb", "\x1b"}},
		{"\x7fq\x00", []string{"\x7f", "q", "\x00"}},
		{"", nil},
	} {
		var got []string
		for _, p := range imeSplit([]byte(c.in)) {
			got = append(got, string(p))
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("imeSplit(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func imeText(cells []IMECell) string {
	var b strings.Builder
	for _, c := range cells {
		if c.Width > 0 {
			b.WriteRune(c.R)
		}
	}
	return b.String()
}

func TestIMEProcessDies(t *testing.T) {
	m, results := fakeIME(t, &Config{})
	m.Open(40, "", imeStartLatin)
	waitFor(t, "ready", func() bool { v := m.View(); return v != nil && v.Busy == "" })
	m.Send([]byte("\x04")) // Ctrl+D：read が EOF で終わり、line.sh が終わる
	waitResult(t, results, [2]string{"cancel", "uim-fep が止まりました （終了コード 0）"})
	if m.Info().Running || m.View() != nil {
		t.Fatal("still running")
	}
	// もう一度開けば起動し直す
	m.Open(40, "", imeStartLatin)
	m.Send([]byte("again\r"))
	waitResult(t, results, [2]string{"ok", "again"})
}

// Prompt が uim-fep の欄を使うときの流れ（キー → バイト列、結果 → 確定、やめる）。
func TestPromptIME(t *testing.T) {
	m, _ := fakeIME(t, &Config{})
	var shown *Overlay
	p := NewPrompt(func(o *Overlay) { shown = o })
	p.after = func(time.Duration, func()) {}
	p.SetIME(m)
	var committed []string
	p.SetHandlers(func(kind, text string) (string, error) {
		committed = append(committed, kind+":"+text)
		if text == "" {
			return "", inputError("何も入っていません")
		}
		return "足しました", nil
	}, func(kind string) string { return "" })
	p.Open(inputTodo)
	if !p.Active() || shown == nil || shown.Input == nil || shown.Input.IME == nil || shown.Input.Title != inputTitleIME[inputTodo] {
		t.Fatalf("overlay = %+v", shown)
	}
	waitFor(t, "ready", func() bool { p.render(); return shown.Input != nil && shown.Input.IME.Busy == "" })
	// 文字切り替えを 1 回：かな/英数（NUL を送る。偽の uim-fep では readline がそのまま受けて何もしない）
	p.Key(evdev.KEY_LEFTALT, 1)
	p.Key(evdev.KEY_LEFTALT, 0)
	for _, c := range []evdev.EvCode{evdev.KEY_A, evdev.KEY_LEFTSHIFT, evdev.KEY_B} {
		p.Key(c, 1)
		if c != evdev.KEY_LEFTSHIFT {
			p.Key(c, 0)
		}
	}
	p.Key(evdev.KEY_LEFTSHIFT, 0)
	waitFor(t, "typed", func() bool { p.render(); return strings.TrimSpace(imeText(shown.Input.IME.Line)) == "aB" })
	if shown.Input.IME.Cursor != 2 {
		t.Fatalf("cursor = %d", shown.Input.IME.Cursor)
	}
	p.Key(evdev.KEY_ENTER, 1)
	waitFor(t, "committed", func() bool { return !p.Active() })
	if len(committed) != 1 || committed[0] != "todo:aB" || shown.Notice != "足しました" {
		t.Fatalf("committed %q notice %q", committed, shown.Notice)
	}
	// 空で確定：エラーで開いたまま（赤い知らせ）
	p.Open(inputTodo)
	p.Key(evdev.KEY_ENTER, 1)
	waitFor(t, "error", func() bool { p.render(); return shown.Input != nil && shown.Input.Err != "" })
	if !p.Active() || shown.Input.Err != "何も入っていません" {
		t.Fatalf("after empty commit: %+v", shown.Input)
	}
	// 戻る：やめる
	p.Key(evdev.KEY_ESC, 1)
	waitFor(t, "cancelled", func() bool { return !p.Active() })
	if shown != nil && shown.Input != nil {
		t.Fatal("input still shown")
	}
	// 時刻の欄：最初の文字が入る
	p.SetHandlers(func(kind, text string) (string, error) {
		committed = append(committed, kind+":"+text)
		return "ok", nil
	},
		func(kind string) string { return "2026-10-11 23:00" })
	p.Open(inputClock)
	waitFor(t, "initial", func() bool { p.render(); return strings.TrimSpace(imeText(shown.Input.IME.Line)) == "2026-10-11 23:00" })
	p.Close()
	if p.Active() || m.Active() {
		t.Fatal("close did not close")
	}
}

// 欄の絵（日本語入力）。/tmp/lefthand-ime-overlay.png に書く（目で確かめる用）。
func TestDrawIMEOverlay(t *testing.T) {
	line := []IMECell{{R: 'a', Width: 1}}
	for _, r := range "日本語" {
		line = append(line, IMECell{R: r, Width: 2, Reverse: true}, IMECell{})
	}
	for _, r := range "のにゅうりょく" {
		line = append(line, IMECell{R: r, Width: 2, Underline: true}, IMECell{})
	}
	var cands []IMECell
	for i, r := range "1:日本語 2:にほんご 3:ニホンゴ [2/3]" {
		w := runeWidth(r)
		cands = append(cands, IMECell{R: r, Width: w, Reverse: i >= 6 && i < 11})
		if w == 2 {
			cands = append(cands, IMECell{})
		}
	}
	for _, rot := range []int{0, 90} {
		cv := NewCanvas(800, 480, 1600, rgb565, rot)
		o := &Overlay{Input: &InputView{Title: inputTitleIME[inputTodo], Hint: inputHintIME[inputTodo],
			IME: &IMEView{Line: line, Cursor: 1, Candidates: cands, Mode: "かな"}}}
		r := drawOverlay(cv, o)
		_, in := overlayRects(o, cv.W, cv.H)
		if r != in {
			t.Fatalf("rot %d: rect %v != %v", rot, r, in)
		}
		if rot == 0 {
			f, _ := os.Create("/tmp/lefthand-ime-overlay.png")
			png.Encode(f, cv.Image())
			f.Close()
		}
	}
	if imeColsFor(800) != 95 || imeColsFor(480) != 55 {
		t.Fatalf("cols = %d %d", imeColsFor(800), imeColsFor(480))
	}
}

// 欄の種類ごとの、開いたときのかなと英数（ime.start）。かなは「英数にしてから切り替える」ので、前の状態によらない。
func TestIMEStartMode(t *testing.T) {
	var none *IMEConfig
	if none.startMode(inputTodo) != imeStartKana || none.startMode(inputClock) != imeStartLatin || none.startMode("other") != imeStartLatin {
		t.Fatal("defaults: todo kana, clock latin, others latin")
	}
	c := &IMEConfig{Start: map[string]string{inputTodo: imeStartLast}}
	if c.startMode(inputTodo) != imeStartLast || c.startMode(inputClock) != imeStartLatin {
		t.Fatal("ime.start must override only the kinds it names")
	}
	for _, k := range []struct {
		initial, start, want string
	}{
		{"", imeStartKana, "\x1e\x00"},
		{"", imeStartLatin, "\x1e"},
		{"", imeStartLast, ""},
		{"2026-10-12 01:00", imeStartLatin, "\x1e2026-10-12 01:00"},
		{"12:00", imeStartLast, "\x1e12:00"},
		{"abc", imeStartKana, "\x1eabc\x00"},
	} {
		if got := string(imeOpenKeys(k.initial, k.start)); got != k.want {
			t.Errorf("imeOpenKeys(%q, %s) = %q, want %q", k.initial, k.start, got, k.want)
		}
	}
	// 制御の文字は 1 つずつ書く（uim-fep の癖）
	if got := imeSplit(imeOpenKeys("abc", imeStartKana)); len(got) != 3 {
		t.Errorf("split = %q", got)
	}
	for src, want := range map[string]string{
		"ime: {start: {todo: kana, clock: latin}}": "",
		"ime: {start: {todo: last}}":               "",
		"ime: {start: {todo: hiragana}}":           "ime.start.todo must be kana, latin or last",
		"ime: {start: {memo: kana}}":               "unknown input field",
	} {
		_, err := checkText(src)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
}
