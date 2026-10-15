package main

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- 設定のアプリ（フェーズ K1） ----------

// 設定のアプリ：選ぶと上書きに置き、設定ファイルと同じ値なら上書きを消す。上書きの行には札。
func TestSettingsUIChoose(t *testing.T) {
	newFakeBacklight(t, 7)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("x"), 0o644)
	file := mustCfg(t, "backlight: {dim_after: 3m, off_after: 15m}")
	km, _, _ := compileKeymap(file)
	s := &configStore{path: path, cfg: file, km: km, ov: OpenOverrides(dir), light: func(*Config) {}}
	var rot, size atomic.Int32 // 選んだものは別の goroutine で変える
	size.Store(24)
	u := NewSettingsUI(SettingsDeps{Store: s, Rotation: func() int { return int(rot.Load()) }, Rotate: func(r int) { rot.Store(int32(r)) },
		ReaderSize: func() int { return int(size.Load()) }, SetReaderSize: func(n int) { size.Store(int32(n)) }, BrightLevel: func() int { return 7 }})
	cell := image.Rect(0, 0, 800, 480)
	area := cell.Inset(cellGap + textMargin)
	g := settingsGeometry(area, 0)
	tapRow := func(id string) {
		for pi, ids := range settingsPages(g.per) {
			for i, rid := range ids {
				if rid == id {
					pt := g.rows[i].Min.Add(image.Pt(10, 10))
					u.mu.Lock()
					u.page = pi
					u.mu.Unlock()
					u.touchDown(cell, pt)
					u.touchUp(false)
				}
			}
		}
	}
	pick := func(label string) {
		v := u.Snapshot()
		gg := settingsGeometry(area, len(v.Choices))
		for i, c := range v.Choices {
			if c.Label == label {
				u.touchDown(cell, gg.choice[i].Min.Add(image.Pt(5, 5)))
				u.touchUp(false)
				return
			}
		}
		t.Fatalf("no choice %q in %+v", label, v.Choices)
	}
	wait := func(f func() bool) {
		t.Helper()
		for i := 0; i < 200 && !f(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		if !f() {
			t.Fatal("timed out")
		}
	}
	tapRow("level")
	if v := u.Snapshot(); v.Choice != "level" || v.Current != 6 {
		t.Fatalf("level choices: %+v", v)
	}
	pick("4")
	wait(func() bool { return s.Current().Backlight.Level != nil && *s.Current().Backlight.Level == 4 })
	if v := u.Snapshot(); !v.Rows[0].Override || v.Rows[0].Value != "4 / 7" || v.Overs != 1 {
		t.Errorf("rows = %+v", v.Rows[0])
	}
	// 設定ファイルの値に戻す
	tapRow("level")
	pick("設定ファイルの値に戻す（起動したときの明るさ）")
	wait(func() bool { return s.Current().Backlight.Level == nil })
	// 設定ファイルと同じ値を選んだら、上書きを置かない
	tapRow("dim")
	pick("1分")
	wait(func() bool { return s.Current().Backlight.DimAfter == "1m" })
	tapRow("dim")
	pick("3分")
	wait(func() bool { return len(s.Overrides()) == 0 })
	// 画面の向きと読み物の文字は、Brain の状態を変える（上書きではない）
	tapRow("rotate")
	pick("縦向き")
	wait(func() bool { return rot.Load() == 90 })
	tapRow("reader")
	pick("大きい（32）")
	wait(func() bool { return size.Load() == 32 })
	if len(s.Overrides()) != 0 {
		t.Error("rotation and reader size must not be overrides")
	}
	// ふた。すべて戻す（聞いてから）
	tapRow("lid")
	pick("何もしない")
	wait(func() bool { return s.Current().Lid.action() == lidNone })
	u.Snapshot() // 描き直し（タッチの処理は、最後に描いたときの上書きの数を使う）
	tapRow("reset")
	if !u.Snapshot().Confirm {
		t.Fatal("reset must ask")
	}
	u.touchDown(cell, g.yes.Min.Add(image.Pt(5, 5)))
	u.touchUp(false)
	wait(func() bool { return len(s.Overrides()) == 0 })
	// 選ぶ画面の外をタップしたら閉じる
	tapRow("photo")
	u.touchDown(cell, image.Pt(area.Min.X+1, area.Max.Y-2))
	u.touchUp(false)
	if u.Snapshot().Choice != "" {
		t.Error("tapping outside must close")
	}
}

// 電源：2 秒押し続けたときだけ行う。
func TestSettingsPowerHold(t *testing.T) {
	got := make(chan string, 2)
	u := NewSettingsUI(SettingsDeps{Power: func(a string) error { got <- a; return nil }})
	u.tab = setTabPower
	cell := image.Rect(0, 0, 800, 480)
	g := settingsGeometry(cell.Inset(cellGap+textMargin), 0)
	u.touchDown(cell, g.power[0].Min.Add(image.Pt(5, 5)))
	time.Sleep(50 * time.Millisecond)
	u.touchUp(false)
	if v := u.Snapshot(); v.Note == "" || len(got) != 0 {
		t.Errorf("a short press must not reboot: %+v", v)
	}
	u.touchDown(cell, g.power[1].Min.Add(image.Pt(5, 5)))
	select {
	case a := <-got:
		if a != "poweroff" {
			t.Errorf("action = %s", a)
		}
	case <-time.After(setPowerHold + time.Second):
		t.Fatal("holding for 2 s did nothing")
	}
	u.touchUp(false)
	if v := u.Snapshot(); v.Power != "poweroff" {
		t.Errorf("view = %+v", v)
	}
	// 使えないとき
	u2 := NewSettingsUI(SettingsDeps{})
	u2.tab = setTabPower
	u2.touchDown(cell, g.power[0].Min.Add(image.Pt(5, 5)))
	if v := u2.Snapshot(); !strings.Contains(v.Note, "使えません") || v.PowerOK {
		t.Errorf("no power: %+v", v)
	}
}

func TestSettingsInfoRows(t *testing.T) {
	at := time.Date(2026, 10, 11, 15, 4, 0, 0, time.Local)
	rows := settingsInfoIn{Version: "abc", Battery: &BatteryInfo{Percent: 47, VoltageMV: 3760, Plugged: true, ChargerOff: true},
		Free: 11 << 30, Size: 14 << 30, Link: "configured", LastSync: &SyncRecord{At: at, OK: true}, Synced: true,
		Lid: LidInfo{Close: lidOff}, Overrides: []OverrideInfo{{Label: "明るさ"}}}.rows()
	want := map[string]string{"版": "abc", "電池": "47%（3.76 V） USB 給電、充電していない", "SD カードの空き": "11.0G（全体 14.0G）", "PC": "つながっている",
		"最後の同期": "10月11日 15:04（成功）", "時刻": "合わせてある", "ふた": "開いている、閉じたら画面を消す", "上書き": "1 個：明るさ"}
	for _, r := range rows {
		if w, ok := want[r.Label]; !ok || w != r.Value {
			t.Errorf("%s = %q, want %q", r.Label, r.Value, w)
		}
	}
	if len(rows) != len(want) {
		t.Errorf("%d rows", len(rows))
	}
}

func TestSettingsWidget(t *testing.T) {
	if _, err := compileWidget(mustSpec(t, `{widget: settings}`)); err != nil {
		t.Error(err)
	}
	if _, err := compileWidget(mustSpec(t, `{widget: settings, key: A}`)); err == nil {
		t.Error("settings with key accepted")
	}
}

// 設定の置き場所が、ロックを持ったまま設定を反映している（エンジンを待っている）あいだも、設定のアプリの
// 描き直しの知らせ、描画、タッチが止まらないこと（2026-10-11 に本番で config put のあとに止まった）。
func TestSettingsNoDeadlockWhileApplying(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	src := "touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }\nlayers: [{name: base, touch: {cols: 1, rows: 1}}]\n"
	os.WriteFile(path, []byte(src), 0o644)
	file := mustCfg(t, "")
	km, _, _ := compileKeymap(file)
	s := &configStore{path: path, cfg: file, km: km, ov: OpenOverrides(dir)}
	u := NewSettingsUI(SettingsDeps{Store: s, BrightLevel: func() int { return 7 }})
	u.SetOnChange(func() { u.Snapshot() })
	entered, release := make(chan struct{}), make(chan struct{})
	s.apply = func(*Config, *Keymap) error {
		u.Changed() // main の apply と同じ
		close(entered)
		<-release // エンジンのロックを待っているつもり
		return nil
	}
	go s.PutText([]byte(src+"lid: {close: none}\n"), false, false)
	<-entered
	go u.Snapshot() // 描画の goroutine：設定の置き場所を待つ（設定のアプリのロックは持たない）
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		cell := image.Rect(0, 0, 800, 480)
		g := settingsGeometry(cell.Inset(cellGap+textMargin), 0)
		u.touchDown(cell, g.rows[0].Min.Add(image.Pt(10, 10))) // エンジンのロックの中で呼ばれる。選ぶ画面を開く
		u.touchUp(false)
		u.touchDown(cell, g.rows[0].Min.Add(image.Pt(10, 10))) // まだ描いていないので、外のタップとして閉じる
		u.touchUp(false)
		u.touchDown(cell, g.tabs[1].Min.Add(image.Pt(5, 5)))
		u.touchUp(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("touches blocked while the config was being applied")
	}
	close(release)
	for i := 0; i < 200 && s.Current().Lid.action() != lidNone; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Current().Lid.action() != lidNone {
		t.Error("the config was not applied")
	}
	if v := u.Snapshot(); v.Tab != setTabInfo {
		t.Errorf("tab = %d", v.Tab)
	}
}

// 設定のタブの 1 ページ目はよく変えるもの（明るさ、暗くなるまで、フォトフレーム、画面の向き、日本語入力）、2 ページ目はそのほか。
// 1 ページに入らない画面では、組を続くページに分ける。
func TestSettingsPages(t *testing.T) {
	g := settingsGeometry(image.Rect(0, 0, 800, 480).Inset(cellGap+textMargin), 0)
	pl := settingsPages(g.per)
	if len(pl) != 2 || strings.Join(pl[0], ",") != "level,dim,dim_offline,photo,rotate,ime" || strings.Join(pl[1], ",") != "reader,lid,profile,clock,reset" {
		t.Fatalf("pages (per %d) = %q", g.per, pl)
	}
	pl = settingsPages(4)
	if len(pl) != 4 || len(pl[0]) != 4 || len(pl[1]) != 2 || pl[2][0] != "reader" {
		t.Fatalf("pages (per 4) = %q", pl)
	}
	n := 0
	for _, p := range pl {
		n += len(p)
	}
	if n != len(setRowIDs) {
		t.Fatalf("%d rows in pages, want %d", n, len(setRowIDs))
	}
}
