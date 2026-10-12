package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- PC ごとの設定（フェーズ K3） ----------

const profileConfig = testConfig + `  - name: code
    label: "コード"
    keys:
      KEY_W: LCTRL+LSHIFT+P
start_layer: base
profiles:
  - name: mac
    label: "Mac"
    pcs: [MacBook, imac]
    start_layer: code
    modifiers: { ctrl: gui, gui: ctrl }
    layers:
      - name: base
        keys:
          KEY_Q: LGUI+Q
        touch:
          cells:
            "1,0": { key: C, label: "シー" }
  - name: work
`

func TestApplyProfile(t *testing.T) {
	file, _, _, err := checkConfig([]byte(profileConfig))
	if err != nil {
		t.Fatal(err)
	}
	eff, err := applyProfile(file, "mac")
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(eff)
	if err != nil {
		t.Fatal(err)
	}
	if km.Profile != "mac" || !km.HasStart || km.Layers[km.Start].Name != "code" {
		t.Fatalf("profile %q start %v %d", km.Profile, km.HasStart, km.Start)
	}
	// base の KEY_Q はプロファイルの LGUI+Q を、修飾キーを入れ替えて LCTRL+Q に
	if a := km.lookupKey([]int{0}, evdev.KEY_Q); a == nil || a.Combo.Mods != modBits["LCTRL"] {
		t.Fatalf("KEY_Q = %+v", a)
	}
	// edit の LCTRL+C は LGUI+C に、code の LCTRL+LSHIFT+P は LGUI+LSHIFT+P に
	if a := km.lookupKey([]int{0, 1}, evdev.KEY_Q); a == nil || a.Combo.Mods != modBits["LGUI"] {
		t.Fatalf("edit KEY_Q = %+v", a)
	}
	if a := km.lookupKey([]int{0, 2}, evdev.KEY_W); a == nil || a.Combo.Mods != modBits["LGUI"]|modBits["LSHIFT"] {
		t.Fatalf("code KEY_W = %+v", a)
	}
	// セルは重ねる（0,0 は残り、1,0 が増える）
	g := km.Layers[0].Grid
	if g.Cells[cellPos{0, 0}] == nil || g.Cells[cellPos{1, 0}] == nil || g.Cells[cellPos{1, 0}].Spec.Label != "シー" {
		t.Fatalf("cells %v", g.Cells)
	}
	// 設定ファイルの値は変えない
	if file.Layers[0].Keys["KEY_Q"].Key != "B" || file.Layers[1].Keys["KEY_Q"].Key != "LCTRL+C" || len(file.Layers[0].Touch.Cells) != 1 {
		t.Fatal("applyProfile changed the file config")
	}
	fk, _, _ := compileKeymap(file)
	if fk.Layers[fk.Start].Name != "base" || fk.Profile != "" {
		t.Fatal("start_layer of the file")
	}
	// 中身のないプロファイル
	if eff, err := applyProfile(file, "work"); err != nil || eff.profile != "work" {
		t.Fatalf("work: %v", err)
	}
	if _, err := applyProfile(file, "none"); err == nil {
		t.Fatal("unknown profile")
	}
	if remapComboMods("RCTRL+LGUI+A", map[string]string{"ctrl": "gui", "gui": "ctrl"}) != "RGUI+LCTRL+A" {
		t.Fatal("remapComboMods")
	}
	if c := (&Config{Profiles: file.Profiles}); c.profileForPC("macbook") == nil || c.profileForPC("mac") != nil || c.profileForPC("WORK") == nil {
		t.Fatal("pcs match")
	}
}

func TestProfileValidate(t *testing.T) {
	base := "  - name: code\n    keys: { KEY_W: A }\n"
	for src, want := range map[string]string{
		"profiles: [{name: a}, {name: a}]":                                          "defined twice",
		"profiles: [{name: none}]":                                                  "must be letters",
		"profiles: [{name: a, pcs: [x]}, {name: b, pcs: [X]}]":                      "used by both",
		"profiles: [{name: a, modifiers: {ctrl: cmd}}]":                             "modifiers",
		"profiles: [{name: a, start_layer: nosuch}]":                                "no layer named",
		"profiles: [{name: a, layers: [{name: nosuch}]}]":                           "only change existing layers",
		"profiles: [{name: a, layers: [{name: code, keys: {KEY_W: NOSUCH}}]}]":      "unknown key",
		"start_layer: nosuch":                                                       "no layer named",
		"start_layer: code\nprofiles: [{name: a, pcs: [my-pc], start_layer: base}]": "",
	} {
		_, _, _, err := checkConfig([]byte(testConfig + base + src + "\n"))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
}

func TestProfileService(t *testing.T) {
	dir := t.TempDir()
	cfg := mustCfg(t, "profiles: [{name: mac, pcs: [macbook]}, {name: work}]")
	s := NewProfileService(dir)
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() { changes <- struct{}{} })
	if s.Resolve(cfg) != "" {
		t.Fatal("default")
	}
	s.SetPC("MacBook")
	if s.Resolve(cfg) != "mac" {
		t.Fatal("pc macbook")
	}
	// 手で選ぶ：別の PC が名乗るまで続く
	if err := s.Use("work", cfg); err != nil || s.Resolve(cfg) != "work" {
		t.Fatalf("use work: %v", err)
	}
	s.SetPC("macbook") // 同じ PC（大文字と小文字は区別しない）
	if s.Resolve(cfg) != "work" {
		t.Fatal("manual must stay on the same PC")
	}
	s.Use(profileNone, cfg)
	if s.Resolve(cfg) != "" || s.Info(cfg).Manual != profileNone {
		t.Fatal("none")
	}
	s.SetPC("other")
	if s.Resolve(cfg) != "" || s.Info(cfg).Manual != "" {
		t.Fatal("a different PC clears the manual choice")
	}
	if err := s.Use("nosuch", cfg); err == nil {
		t.Fatal("unknown profile accepted")
	}
	if err := s.SetPC("bad name"); err == nil {
		t.Fatal("bad PC name accepted")
	}
	// 覚えている（デーモンを起動し直しても同じ PC）
	s.SetPC("work")
	if NewProfileService(dir).Resolve(cfg) != "work" {
		t.Fatal("state not saved")
	}
	// つなぎ直して名乗らなければ既定
	s.waitFor = 30 * time.Millisecond
	s.LinkChanged(true)
	if !s.Info(cfg).Waiting {
		t.Fatal("waiting")
	}
	time.Sleep(80 * time.Millisecond)
	if s.Resolve(cfg) != "" || s.Info(cfg).Waiting {
		t.Fatal("not reset after the wait")
	}
	// 名乗れば待つのをやめる
	s.LinkChanged(true)
	s.SetPC("work")
	time.Sleep(80 * time.Millisecond)
	if s.Resolve(cfg) != "work" {
		t.Fatal("reset although the PC announced itself")
	}
	if len(changes) == 0 {
		t.Fatal("no change notifications")
	}
	if _, err := os.Stat(filepath.Join(dir, profileStateName)); err != nil {
		t.Fatal(err)
	}
}

// プロトコル：set_pc で割り当てと最初のレイヤーが変わり、get_status と get_profile に出る。設定を入れ直しても続く。
func TestProfileProtocol(t *testing.T) {
	var prof *ProfileService
	d := newTestDaemon(t, profileConfig, func(d *testDaemon) {
		prof = NewProfileService(d.dataDir)
		d.store.prof = prof
		d.ctl.profile = prof
		prof.SetOnChange(d.store.Reprofile)
	})
	if d.engine.top() != "base" {
		t.Fatalf("start %s", d.engine.top())
	}
	res := result(t, d.call("set_pc", map[string]any{"name": "imac", "source": "test"}))
	if res["active"] != "mac" || res["label"] != "Mac" || res["pc"] != "imac" {
		t.Fatalf("set_pc %v", res)
	}
	if d.engine.top() != "code" {
		t.Fatalf("start layer of the profile: %s", d.engine.top())
	}
	if a := d.store.Keymap().lookupKey([]int{0}, evdev.KEY_Q); a.Combo.Mods != modBits["LCTRL"] {
		t.Fatal("keys of the profile")
	}
	st := result(t, d.call("get_status", nil))
	if st["profile"].(map[string]any)["active"] != "mac" {
		t.Fatalf("get_status %v", st["profile"])
	}
	// get_config は設定ファイルの値（プロファイルを重ねる前）
	cfg := result(t, d.call("get_config", nil))["config"].(map[string]any)
	if cfg["layers"].([]any)[0].(map[string]any)["keys"].(map[string]any)["KEY_Q"].(map[string]any)["key"] != "B" {
		t.Fatalf("get_config %v", cfg["layers"])
	}
	// 設定を入れ直しても、プロファイルは重なったまま
	text, _ := os.ReadFile(d.path)
	result(t, d.call("put_config_text", map[string]any{"text": string(text)}))
	if d.store.Profile() != "mac" || d.store.Keymap().lookupKey([]int{0}, evdev.KEY_Q).Combo.Mods != modBits["LCTRL"] {
		t.Fatal("the profile was lost by put_config_text")
	}
	// 手で既定に
	res = result(t, d.call("set_profile", map[string]any{"name": "none"}))
	if res["active"] != "" || d.engine.top() != "base" || d.store.Keymap().lookupKey([]int{0}, evdev.KEY_Q).Combo.Mods != 0 {
		t.Fatalf("set_profile none %v top %s", res, d.engine.top())
	}
	if r := d.call("set_profile", map[string]any{"name": "nosuch"}); r["ok"] == true {
		t.Fatal("unknown profile")
	}
	res = result(t, d.call("set_profile", map[string]any{"name": "auto"}))
	if res["active"] != "mac" {
		t.Fatalf("auto %v", res)
	}
	res = result(t, d.call("get_profile", nil))
	if len(res["profiles"].([]any)) != 2 {
		t.Fatalf("profiles %v", res["profiles"])
	}
}

func TestBarShowsProfileOnlyWhenNotDefault(t *testing.T) {
	env := WidgetEnv{Now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local), TimeSynced: true}
	items := barItems([]string{barTime}, env)
	if len(items) != 1 || items[0].kind != barTime {
		t.Fatalf("default: %+v", items)
	}
	env.Profile = "会社"
	items = barItems([]string{barTime}, env)
	if len(items) != 2 || items[0].kind != barProfile || items[0].text != "会社" {
		t.Fatalf("profile: %+v", items)
	}
	cfg := &Config{Profiles: []ProfileConfig{{Name: "work", Label: "会社"}, {Name: "home"}}}
	if got := cfg.profileTitle(); got != "" {
		t.Errorf("no profile: %q", got)
	}
	cfg.profile = "work"
	if got := cfg.profileTitle(); got != "会社" {
		t.Errorf("label: %q", got)
	}
	cfg.profile = "home"
	if got := cfg.profileTitle(); got != "home" {
		t.Errorf("name: %q", got)
	}
	// 重ねた設定（Profiles は空になる）でも label を出す
	cfg.profile = ""
	eff, err := applyProfile(cfg, "work")
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.profileTitle(); got != "会社" {
		t.Errorf("applied: %q", got)
	}
}
