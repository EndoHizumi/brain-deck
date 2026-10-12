package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// フェーズ K7：読み物の本文の文字（24、32 ドット）を別のファイルに置き、brain-deck update で lefthand と組で入れ替える。

func realFonts(t *testing.T) map[string][]byte {
	t.Helper()
	m := map[string][]byte{}
	for _, n := range updateFontNames() {
		b, err := os.ReadFile(filepath.Join("font", n))
		if err != nil {
			t.Fatal(err)
		}
		m[n] = b
	}
	return m
}

func TestReadFontsExternal(t *testing.T) {
	old := readFontDir
	t.Cleanup(func() {
		readFontDir = old
		readFontsMu.Lock()
		readFonts = map[int]*ReadFont{}
		readFontsMu.Unlock()
	})
	reset := func(dir string) {
		readFontDir = dir
		readFontsMu.Lock()
		readFonts = map[int]*ReadFont{}
		readFontsMu.Unlock()
	}
	// ルビは埋め込み。本文は readFontDir（なければ今のディレクトリの font/）
	reset(t.TempDir())
	if f := readFontOf(12); f == nil || f.S != 12 {
		t.Fatal("embedded ruby font")
	}
	if f := readFontOf(24); f == nil || f.S != 24 {
		t.Fatal("font/ fallback in the repository")
	}
	dir := t.TempDir()
	for n, b := range realFonts(t) {
		os.WriteFile(filepath.Join(dir, n), b, 0o644)
	}
	reset(dir)
	if f := readFontOf(32); f == nil || f.S != 32 {
		t.Fatal("font in readFontDir")
	}
	if err := checkReadFonts(dir); err != nil {
		t.Fatal(err)
	}
	// 名前と大きさが違う、壊れている、ないときは -check-fonts が失敗する
	os.Rename(filepath.Join(dir, "reader-24.lrf.gz"), filepath.Join(dir, "x"))
	os.WriteFile(filepath.Join(dir, "reader-24.lrf.gz"), realFonts(t)["reader-32.lrf.gz"], 0o644)
	if err := checkReadFonts(dir); err == nil || !strings.Contains(err.Error(), "not 24") {
		t.Fatalf("wrong size: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "reader-24.lrf.gz"), []byte("x"), 0o644)
	if err := checkReadFonts(dir); err == nil {
		t.Fatal("broken font")
	}
	os.Remove(filepath.Join(dir, "reader-24.lrf.gz"))
	if err := checkReadFonts(dir); err == nil {
		t.Fatal("missing font")
	}
	// ないときは k8x12 で描く（nil）。リポジトリの外（Brain）では font/ もない
	wd, _ := os.Getwd()
	os.Chdir(t.TempDir())
	defer os.Chdir(wd)
	reset(dir)
	if readFontOf(24) != nil {
		t.Fatal("missing font must fall back to k8x12")
	}
}

// mkupdate が入れる名前と、デーモンが受け付ける名前が同じ。
func TestMkupdateFontNames(t *testing.T) {
	b, err := os.ReadFile("tools/mkupdate/main.go")
	if err != nil {
		t.Fatal(err)
	}
	want := `var fontNames = []string{"` + strings.Join(updateFontNames(), `", "`) + `"}`
	if !strings.Contains(string(b), want) {
		t.Fatalf("tools/mkupdate fontNames must be %s", want)
	}
}

func TestUpdateFontsVerify(t *testing.T) {
	e := newUpdateEnv(t)
	e.upd.FontDir = filepath.Join(t.TempDir(), "share")
	files := goodFiles(t, "k7a")
	for n, b := range realFonts(t) {
		files[n] = b
	}
	info := result(t, e.send("update", makeBundle(t, files, nil)))
	if f, _ := info["fonts"].([]any); len(f) != 2 || info["fonts_changed"] != true {
		t.Fatalf("info %v", info)
	}
	for _, w := range info["warnings"].([]any) {
		if strings.Contains(w.(string), "reader font") {
			t.Fatalf("unexpected warning %v", w)
		}
	}
	// 文字の入っていない更新のファイル：Brain にもなければ注意（受け付ける）
	info = result(t, e.send("update", makeBundle(t, goodFiles(t, "k7b"), nil)))
	if w := strings.Join(func() []string {
		var s []string
		for _, x := range info["warnings"].([]any) {
			s = append(s, x.(string))
		}
		return s
	}(), "\n"); strings.Count(w, "reader font") != 2 {
		t.Fatalf("warnings %q", w)
	}
	// 文字を埋め込む前の版（K7 より前）なら、注意しない
	info = result(t, e.send("update", makeBundle(t, goodFiles(t, "v2"), nil)))
	for _, w := range info["warnings"].([]any) {
		if strings.Contains(w.(string), "reader font") {
			t.Fatalf("old version: %v", w)
		}
	}
	// 断るもの：壊れた文字、manifest にない文字
	broken := goodFiles(t, "k7c")
	broken["reader-24.lrf.gz"] = []byte("x")
	broken["reader-32.lrf.gz"] = realFonts(t)["reader-32.lrf.gz"]
	if r := e.send("update", makeBundle(t, broken, nil)); r["error"] == nil {
		t.Fatalf("broken font: %v", r)
	}
	unlisted := goodFiles(t, "k7d")
	unlisted["reader-24.lrf.gz"] = realFonts(t)["reader-24.lrf.gz"]
	if r := e.send("update", makeBundle(t, unlisted, func(m *updateManifest) { delete(m.Files, "reader-24.lrf.gz") })); r["error"] == nil {
		t.Fatalf("unlisted font: %v", r)
	}
}

// 係は、文字を lefthand と組で入れ替え（初めてなら置き）、起動しなければ元に戻す（初めてなら消す）。
func TestHelperInstallsFonts(t *testing.T) {
	helperTimeouts(t)
	e := newUpdateEnv(t)
	share := filepath.Join(t.TempDir(), "share", "lefthand")
	e.upd.FontDir = share
	stageFonts := func(p *updatePlan, content string) {
		for _, n := range updateFontNames() {
			src := filepath.Join(e.upd.Dir, p.Stage, n)
			os.WriteFile(src, []byte(content+n), 0o755)
			p.Files = append(p.Files, [2]string{src, filepath.Join(share, n)})
		}
	}
	// 初めて置くとき、起動しなければ消す
	p := e.stageFor(t, "broken", nil)
	p.Version = "broken"
	stageFonts(&p, "a")
	if ok, _ := e.runHelper(p); ok {
		t.Fatal("should roll back")
	}
	if _, err := os.Stat(filepath.Join(share, "reader-24.lrf.gz")); err == nil {
		t.Fatal("a font installed for the first time must be removed when rolling back")
	}
	// 置く（フォルダも作る）。実行ファイルではないので 0644
	p = e.stageFor(t, "k7a", nil)
	stageFonts(&p, "a")
	e.confirmWhenAsked(p.Stage)
	if ok, st := e.runHelper(p); !ok {
		t.Fatalf("%+v", st)
	}
	f24 := filepath.Join(share, "reader-24.lrf.gz")
	if readFile(t, f24) != "areader-24.lrf.gz" {
		t.Fatal("font not installed")
	}
	if st, _ := os.Stat(f24); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	// 次の版：前のものは .prev。起動しなければ戻す
	p = e.stageFor(t, "broken", nil)
	p.Version = "broken"
	stageFonts(&p, "b")
	if ok, _ := e.runHelper(p); ok {
		t.Fatal("should roll back")
	}
	if readFile(t, f24) != "areader-24.lrf.gz" || readFile(t, e.bin) != "k7a" {
		t.Fatalf("not restored: %s %s", readFile(t, f24), readFile(t, e.bin))
	}
	p = e.stageFor(t, "k7b", nil)
	stageFonts(&p, "b")
	e.confirmWhenAsked(p.Stage)
	if ok, st := e.runHelper(p); !ok {
		t.Fatalf("%+v", st)
	}
	if readFile(t, f24) != "breader-24.lrf.gz" || readFile(t, f24+".prev") != "areader-24.lrf.gz" {
		t.Fatalf("font %s prev %s", readFile(t, f24), readFile(t, f24+".prev"))
	}
	// --rollback は、前の文字も一緒に戻す（新しい lefthand の -check-fonts で確かめるので、本物の文字にしておく）
	real := realFonts(t)
	for n, b := range real {
		os.WriteFile(filepath.Join(share, n)+".prev", b, 0o644)
	}
	info, err := e.upd.PrepareRollback()
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Fonts) != 2 {
		t.Fatalf("rollback fonts %v", info.Fonts)
	}
	plan, err := e.upd.Apply(info.Stage)
	if err != nil {
		t.Fatal(err)
	}
	e.confirmWhenAsked(plan.Stage)
	if ok, st := e.runHelper(*plan); !ok {
		t.Fatalf("%+v", st)
	}
	if b, _ := os.ReadFile(f24); readFile(t, e.bin) != "k7a" || string(b) != string(real["reader-24.lrf.gz"]) || readFile(t, f24+".prev") != "breader-24.lrf.gz" {
		t.Fatalf("after rollback: bin %s, font %d bytes, prev %s", readFile(t, e.bin), len(b), readFile(t, f24+".prev"))
	}
}
