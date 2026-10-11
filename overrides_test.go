package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- 上書きの値（フェーズ K1） ----------

func rawJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// 上書きを重ねる：設定ファイルの値は書き換えない。消えるまでの時間は、暗くなるまでより長くそろえる。
func TestApplyOverrides(t *testing.T) {
	file := mustCfg(t, `backlight: {dim_after: 3m, off_after: 15m, photo: true, offline: {dim_after: 1m, off_after: 5m, photo: true}}`)
	eff, infos := applyOverrides(file, map[string]any{"backlight.level": 5, "backlight.dim_after": "30m", "lid.close": "none"})
	if *eff.Backlight.Level != 5 || eff.Backlight.DimAfter != "30m" || eff.Backlight.OffAfter != "42m" || eff.Lid.action() != lidNone {
		t.Errorf("eff = %+v lid %+v", eff.Backlight, eff.Lid)
	}
	if file.Backlight.Level != nil || file.Backlight.DimAfter != "3m" || file.Lid != nil {
		t.Errorf("the file config was changed: %+v", file.Backlight)
	}
	if len(infos) != 3 || infos[0].Key != "backlight.level" || infos[0].File != "起動したときの明るさ" || infos[1].Shown != "30分" || infos[1].File != "3分" || !infos[2].Used {
		t.Errorf("infos = %+v", infos)
	}
	// 外しているときの時間：消える時間は外しているときの間隔（4 分）だけ延ばす
	eff, _ = applyOverrides(file, map[string]any{"backlight.offline.dim_after": "10m"})
	if o := eff.Backlight.Offline; o.DimAfter != "10m" || o.OffAfter != "14m" || file.Backlight.Offline.DimAfter != "1m" {
		t.Errorf("offline = %+v", o)
	}
	p, err := eff.Backlight.compile()
	if err != nil || p.offline.dimAfter != 10*time.Minute || p.offline.offAfter != 14*time.Minute || p.dimAfter != 3*time.Minute {
		t.Errorf("compile = %+v %v", p, err)
	}
	// 暗くしない：写真も出さない（写真は暗くする段階の代わり）
	eff, _ = applyOverrides(file, map[string]any{"backlight.dim_after": "off", "backlight.photo": true})
	p, err = eff.Backlight.compile()
	if err != nil || p.dimAfter != 0 || p.photo || !p.offline.photo {
		t.Errorf("dim off: %+v %v", p, err)
	}
	// 写真を消す：外しているときも
	eff, _ = applyOverrides(file, map[string]any{"backlight.photo": false})
	p, _ = eff.Backlight.compile()
	if p.photo || p.offline.photo {
		t.Errorf("photo off: %+v", p)
	}
	// 設定ファイルに backlight がない：明るさだけ（自動減光はしない）。ふただけなら backlight は nil のまま
	bare := mustCfg(t, "")
	eff, _ = applyOverrides(bare, map[string]any{"backlight.level": 3})
	if p, _ := eff.Backlight.compile(); p == nil || p.level != 3 || p.dimAfter != 0 {
		t.Errorf("bare level: %+v", p)
	}
	eff, _ = applyOverrides(bare, map[string]any{"lid.close": "none"})
	if eff.Backlight != nil || eff.Lid.action() != lidNone {
		t.Errorf("bare lid: %+v %+v", eff.Backlight, eff.Lid)
	}
	// 合わない上書きは使わない：写真を出すのに暗くしない設定
	nodim := mustCfg(t, `backlight: {off_after: 10m}`)
	eff, infos = applyOverrides(nodim, map[string]any{"backlight.photo": true})
	if eff.Backlight.Photo != nil && *eff.Backlight.Photo || infos[0].Used || !strings.Contains(infos[0].Note, "photo needs dim_after") {
		t.Errorf("photo without dim: %+v %+v", eff.Backlight, infos)
	}
	// 上書きがなければ、そのもの
	if e, in := applyOverrides(file, nil); e != file || in != nil {
		t.Error("no overrides must return the file config")
	}
}

func TestSetDimAfter(t *testing.T) {
	for _, c := range []struct{ v, off, fd, fo, want string }{
		{"5m", "15m", "3m", "15m", "15m"},  // 消える時間のほうが長い：そのまま
		{"30m", "15m", "3m", "15m", "42m"}, // 間隔 12 分だけ延ばす
		{"20m", "10m", "", "10m", "30m"},   // 暗くしていなかった：消える時間だけ延ばす
		{"10m", "", "", "", ""},            // 消さない
		{"24h", "15m", "3m", "15m", "off"}, // 上限
	} {
		dim, off := "", c.off
		setDimAfter(&dim, &off, c.v, c.fd, c.fo)
		if dim != c.v || off != c.want {
			t.Errorf("%+v: dim %q off %q", c, dim, off)
		}
	}
	if fmtDur(90*time.Minute) != "1h30m" || fmtDur(2*time.Hour) != "2h" || fmtDur(45*time.Second) != "45s" || fmtDur(3*time.Minute) != "3m" {
		t.Error("fmtDur")
	}
	if durLabel("1h30m") != "1時間30分" || durLabel("off") != "しない" || durLabel("30s") != "30秒" {
		t.Error("durLabel")
	}
}

// 上書きの保存：消せる、壊れたファイルや知らない項目は使わない、値を確かめる。
func TestOverrideStore(t *testing.T) {
	dir := t.TempDir()
	s := OpenOverrides(dir)
	if len(s.Values()) != 0 {
		t.Fatal("empty")
	}
	if err := s.Set("backlight.level", rawJSON(9), "test"); err == nil || !strings.Contains(err.Error(), "1..7") {
		t.Errorf("bad level: %v", err)
	}
	if err := s.Set("display.rotate", rawJSON(90), "test"); err == nil {
		t.Error("unknown key accepted")
	}
	if err := s.Set("backlight.dim_after", rawJSON("2s"), "test"); err == nil {
		t.Error("too short accepted")
	}
	for k, v := range map[string]any{"backlight.level": 5, "backlight.dim_after": "180s", "lid.close": "none", "backlight.photo": false} {
		if err := s.Set(k, rawJSON(v), "test"); err != nil {
			t.Fatal(k, err)
		}
	}
	again := OpenOverrides(dir).Values()
	if again["backlight.level"] != 5 || again["backlight.dim_after"] != "3m" || again["lid.close"] != "none" || again["backlight.photo"] != false {
		t.Errorf("reloaded = %v", again)
	}
	done, err := s.Clear([]string{"lid.close"}, "test")
	if err != nil || len(done) != 1 || OpenOverrides(dir).Values()["lid.close"] != nil {
		t.Errorf("clear one: %v %v", done, err)
	}
	if _, err := s.Clear([]string{"nope"}, "test"); err == nil {
		t.Error("clearing an unknown key must fail")
	}
	done, _ = s.Clear(nil, "test")
	if len(done) != 3 {
		t.Errorf("clear all: %v", done)
	}
	if _, err := os.Stat(filepath.Join(dir, overridesName)); !os.IsNotExist(err) {
		t.Error("the file must be removed when empty")
	}
	// 壊れたファイル、知らない項目、範囲の外の値
	os.WriteFile(filepath.Join(dir, overridesName), []byte(`{"format":1,"values":{"backlight.level":4,"x.y":1,"lid.close":"sleep"}}`), 0o644)
	if v := OpenOverrides(dir).Values(); len(v) != 1 || v["backlight.level"] != 4 {
		t.Errorf("partly bad file = %v", v)
	}
	os.WriteFile(filepath.Join(dir, overridesName), []byte(`{`), 0o644)
	if v := OpenOverrides(dir).Values(); len(v) != 0 {
		t.Errorf("broken file = %v", v)
	}
}

// configStore：get_config は設定ファイルの値、動くのは上書きを重ねたもの。設定ファイルを変えても上書きは残る。
func TestConfigStoreOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	src := "touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers: [{name: base, touch: {cols: 1, rows: 1}}]\nbacklight: {dim_after: 3m, off_after: 15m}\n"
	os.WriteFile(path, []byte(src), 0o644)
	file := mustCfg(t, "backlight: {dim_after: 3m, off_after: 15m}")
	km, _, _ := compileKeymap(file)
	var lit []*Config
	s := &configStore{path: path, cfg: file, km: km, ov: OpenOverrides(dir),
		apply: func(c *Config, k *Keymap) error { lit = append(lit, c); return nil },
		light: func(c *Config) { lit = append(lit, c) }}
	if err := s.SetOverride("backlight.level", rawJSON(2), "test"); err != nil {
		t.Fatal(err)
	}
	if s.File().Backlight.Level != nil || *s.Current().Backlight.Level != 2 || len(lit) != 1 || *lit[0].Backlight.Level != 2 {
		t.Errorf("after set: file %+v cur %+v", s.File().Backlight, s.Current().Backlight)
	}
	// 設定ファイルを変える（put_config_text）：上書きはそのまま重なる
	src2 := strings.Replace(src, "dim_after: 3m", "dim_after: 2m", 1)
	if _, _, err := s.PutText([]byte(src2), false, false); err != nil {
		t.Fatal(err)
	}
	if s.File().Backlight.DimAfter != "2m" || s.Current().Backlight.DimAfter != "2m" || *s.Current().Backlight.Level != 2 {
		t.Errorf("after put: file %+v cur %+v", s.File().Backlight, s.Current().Backlight)
	}
	if b, _ := os.ReadFile(path); string(b) != src2 {
		t.Error("PutText must write the text as is (no overrides)")
	}
	ov := s.Overrides()
	if len(ov) != 1 || ov[0].Key != "backlight.level" || ov[0].Shown != "2" {
		t.Errorf("overrides = %+v", ov)
	}
	// 上書きで直る誤りでも、設定ファイルの誤りとして断る
	if _, _, err := s.PutText([]byte(strings.Replace(src2, "dim_after: 2m, ", "photo: true, ", 1)), false, false); err == nil {
		t.Error("an invalid file was accepted")
	}
	done, err := s.ClearOverrides(nil, "test")
	if err != nil || len(done) != 1 || s.Current().Backlight.Level != nil {
		t.Errorf("clear: %v %v %+v", done, err, s.Current().Backlight)
	}
	// 上書きを使わない（ov が nil）
	s2 := &configStore{path: path, cfg: file, km: km}
	if err := s2.SetOverride("backlight.level", rawJSON(2), "test"); err != errNoOverrides {
		t.Errorf("no store: %v", err)
	}
	if len(s2.Overrides()) != 0 {
		t.Error("no store: overrides")
	}
}

// プロトコル：get_config は設定ファイルの値と上書き、set_override、clear_overrides、get_status の lid と overrides。
func TestControlOverrides(t *testing.T) {
	d := newTestDaemon(t, testConfig, func(d *testDaemon) {
		d.store.ov = OpenOverrides(t.TempDir())
		d.ctl.lid = NewLid(nil, lidOff)
		d.ctl.battery = NewBatteryService(t.TempDir())
		d.ctl.term = NewTermMode(func() *Config { return d.store.Current() }, nil, nil, "")
		d.ctl.usb = NewUSBMode(nil, nil, "", "")
	})
	result(t, d.call("set_override", map[string]any{"key": "lid.close", "value": "none", "source": "test"}))
	if m := d.call("set_override", map[string]any{"key": "backlight.level", "value": 0}); errCode(m) != errBadRequest {
		t.Errorf("bad value: %v", m)
	}
	if m := d.call("set_override", map[string]any{"key": "nope", "value": 1}); errCode(m) != errBadRequest {
		t.Errorf("bad key: %v", m)
	}
	if m := d.call("set_override", map[string]any{"key": "lid.close"}); errCode(m) != errBadRequest {
		t.Errorf("no value: %v", m)
	}
	r := result(t, d.call("get_config", nil))
	if _, ok := r["config"].(map[string]any)["lid"]; ok {
		t.Error("get_config must return the config file (without overrides)")
	}
	ov := r["overrides"].([]any)
	if len(ov) != 1 || ov[0].(map[string]any)["shown"] != "何もしない" || ov[0].(map[string]any)["file"] != "画面を消す" {
		t.Errorf("overrides = %v", ov)
	}
	if d.store.Current().Lid.action() != lidNone {
		t.Error("the override is not used")
	}
	st := result(t, d.call("get_status", nil))
	if lid := st["lid"].(map[string]any); lid["close"] != lidOff || len(st["overrides"].([]any)) != 1 {
		t.Errorf("get_status lid %v overrides %v", st["lid"], st["overrides"])
	}
	r = result(t, d.call("get_overrides", nil))
	if len(r["overrides"].([]any)) != 1 || len(r["keys"].([]any)) != len(overrideKeys) {
		t.Errorf("get_overrides = %v", r)
	}
	r = result(t, d.call("clear_overrides", map[string]any{"keys": []string{}}))
	if cl := r["cleared"].([]any); len(cl) != 1 || cl[0] != "lid.close" {
		t.Errorf("clear = %v", r)
	}
	if m := d.call("clear_overrides", map[string]any{"keys": []string{"nope"}}); errCode(m) != errBadRequest {
		t.Errorf("clear unknown: %v", m)
	}
}
