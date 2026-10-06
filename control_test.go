package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 行の組み立て ----------

func TestLineSplitter(t *testing.T) {
	var lines []string
	tooLong := 0
	sp := lineSplitter{max: 10}
	feed := func(s string) {
		sp.feed([]byte(s), func(l []byte) { lines = append(lines, string(l)) }, func() { tooLong++ })
	}
	feed(`{"a"`)
	feed(`:1}` + "\n\n" + `{"b":2}` + "\r\n" + `{"c"`) // 途中で分かれた行、空行、CRLF
	if !sp.pending() {
		t.Fatal("expected a pending line")
	}
	feed(`:3}` + "\n")
	feed(`0123456789AB`) // 上限を超えた
	feed(`CDEF` + "\n" + `{"d":4}` + "\n")
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`, `{"d":4}`}
	if !reflect.DeepEqual(lines, want) || tooLong != 1 {
		t.Fatalf("lines %q tooLong %d", lines, tooLong)
	}
	if sp.pending() {
		t.Fatal("nothing should be pending")
	}
	feed("0123456789") // ちょうど上限は通る
	feed("\n")
	if lines[len(lines)-1] != "0123456789" {
		t.Fatalf("lines %q", lines)
	}
}

// ---------- テスト用のデーモン ----------

type testDaemon struct {
	t       *testing.T
	path    string // 設定ファイル
	hid     string
	engine  *Engine
	store   *configStore
	monitor *Monitor
	clock   *TimeService
	ctl     *Controller
	conn    net.Conn // GUI 側
	rd      *bufio.Reader
	served  chan error
	nextID  int
}

const testConfig = `# 手で書いたコメント
hid_device: /dev/hidg0
touch:
  min_x: 0
  max_x: 400
  min_y: 0
  max_y: 300
  soft_areas:
    home: { x: [3950, 4095], y: [2000, 4095] }
layers:
  - name: base
    label: "基本"
    keys:
      KEY_Q: B
      KEY_LEFTALT: { layer_hold: edit }
      KEY_ESC: { layer_to: base }
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { key: B, label: "ブラシ" }
  - name: edit
    label: "編集"
    keys:
      KEY_Q: LCTRL+C
`

func newTestDaemon(t *testing.T, src string) *testDaemon {
	t.Helper()
	dir := t.TempDir()
	d := &testDaemon{t: t, path: filepath.Join(dir, "config.yaml"), hid: filepath.Join(dir, "hidg"), served: make(chan error, 1)}
	os.WriteFile(d.path, []byte(src), 0o644)
	os.WriteFile(d.hid, nil, 0o600)
	cfg, err := loadConfig(d.path)
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.engine = NewEngine(km, &State{hid: NewHIDWriter(d.hid), active: map[string]Combo{}})
	d.monitor = &Monitor{}
	d.engine.SetOnStatus(d.monitor.LayerChanged)
	d.store = &configStore{path: d.path, cfg: cfg, km: km, apply: reloader(d.engine)}
	d.clock = NewTimeService(OpenStore(filepath.Join(dir, "data")))
	d.ctl = &Controller{store: d.store, engine: d.engine, monitor: d.monitor, clock: d.clock, started: time.Now()}
	daemonSide, gui := net.Pipe()
	d.conn, d.rd = gui, bufio.NewReaderSize(gui, 1<<20)
	go func() { d.served <- d.ctl.Serve(daemonSide) }()
	t.Cleanup(func() { gui.Close(); daemonSide.Close() })
	return d
}

func (d *testDaemon) write(s string) {
	d.t.Helper()
	d.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := d.conn.Write([]byte(s)); err != nil {
		d.t.Fatal(err)
	}
}

// readMsg は次の 1 行を読む。
func (d *testDaemon) readMsg() map[string]any {
	d.t.Helper()
	d.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := d.rd.ReadBytes('\n')
	if err != nil {
		d.t.Fatalf("read: %v (got %q)", err, line)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		d.t.Fatalf("daemon sent a broken line %q: %v", line, err)
	}
	return m
}

// call はリクエストを送り、同じ id のレスポンスを返す（途中の通知は捨てる）。
func (d *testDaemon) call(cmd string, params map[string]any) map[string]any {
	d.t.Helper()
	d.nextID++
	req := map[string]any{"id": d.nextID, "cmd": cmd}
	for k, v := range params {
		req[k] = v
	}
	b, _ := json.Marshal(req)
	d.write(string(b) + "\n")
	for {
		m := d.readMsg()
		if id, ok := m["id"].(float64); ok && int(id) == d.nextID {
			return m
		}
	}
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	if m["ok"] != true {
		t.Fatalf("not ok: %v", m)
	}
	return m["result"].(map[string]any)
}

// currentJSON は get_config の結果（GUI が編集する形）を返す。
func (d *testDaemon) currentJSON() map[string]any {
	return result(d.t, d.call("get_config", nil))["config"].(map[string]any)
}

// ---------- プロトコル ----------

func TestProtocolHelloAndBasics(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	r := result(t, d.call("hello", nil))
	if r["protocol"] != float64(protocolVersion) || r["daemon"] != "lefthand" || r["version"] == "" {
		t.Fatalf("hello %v", r)
	}
	if r["max_line"] != float64(maxLineBytes) {
		t.Fatalf("max_line %v", r["max_line"])
	}

	cfg := d.currentJSON()
	layers := cfg["layers"].([]any)
	if len(layers) != 2 || layers[0].(map[string]any)["label"] != "基本" {
		t.Fatalf("config %v", cfg)
	}
	km := result(t, d.call("get_keymap", nil))
	if len(km["keys"].([]any)) < 40 || km["max_rollover"] != float64(3) {
		t.Fatalf("keymap %v", km)
	}
	st := result(t, d.call("get_status", nil))["status"].(map[string]any)
	if st["layer"] != "base" || st["mode"] != "base" || st["cols"] != float64(4) {
		t.Fatalf("status %v", st)
	}
	// 文字列の id もそのまま返す
	d.write(`{"id":"abc","cmd":"hello"}` + "\n")
	if m := d.readMsg(); m["id"] != "abc" || m["ok"] != true {
		t.Fatalf("string id: %v", m)
	}
	if m := d.call("nope", nil); errCode(m) != errUnknownCmd {
		t.Fatalf("unknown: %v", m)
	}
}

func TestProtocolBrokenInput(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	for _, tc := range []struct{ line, code string }{
		{`{"id":1,"cmd":"hello"`, errParse},             // 閉じていない
		{`not json`, errParse},                          //
		{"\xff\xfe{}", errParse},                        // UTF-8 でない
		{`{"cmd":"hello"}`, errBadRequest},              // id がない
		{`{"id":{"x":1},"cmd":"hello"}`, errBadRequest}, // id がオブジェクト
		{`[1,2,3]`, errParse},                           // オブジェクトでない
	} {
		d.write(tc.line + "\n")
		m := d.readMsg()
		if errCode(m) != tc.code || m["id"] != nil || m["ok"] != false {
			t.Errorf("%q -> %v, want %s", tc.line, m, tc.code)
		}
	}
	// そのあとも普通に使える
	if r := d.call("hello", nil); r["ok"] != true {
		t.Fatal(r)
	}
}

func TestProtocolTooLarge(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	big := `{"id":1,"cmd":"validate","text":"` + strings.Repeat("x", maxLineBytes) + `"}` + "\n"
	go d.write(big) // net.Pipe は相手が読むまで書き込みが返らない
	m := d.readMsg()
	if errCode(m) != errTooLarge || m["id"] != nil {
		t.Fatalf("got %v", m)
	}
	// 捨てた行のあとから、普通に使える
	if r := d.call("hello", nil); r["ok"] != true {
		t.Fatal(r)
	}
}

func TestProtocolIncompleteLine(t *testing.T) {
	old := partialTimeout
	partialTimeout = 50 * time.Millisecond
	defer func() { partialTimeout = old }()
	d := newTestDaemon(t, testConfig)
	d.write(`{"id":1,"cmd":"hel`) // 途中で切れた（GUI が閉じた、ケーブルが抜けたなど）
	m := d.readMsg()
	if errCode(m) != errIncomplete {
		t.Fatalf("got %v", m)
	}
	// 次の行が前の切れ端とつながらない
	if r := d.call("hello", nil); r["ok"] != true {
		t.Fatal(r)
	}
	// 切れ端のあとにすぐ改行が来たときは、壊れた JSON として 1 つのエラーになる
	d.write(`{"id":9,"cm` + "\n")
	if m := d.readMsg(); errCode(m) != errParse {
		t.Fatalf("got %v", m)
	}
}

// 大きな設定も、1 行で途中が欠けずに届く
func TestProtocolLargeConfig(t *testing.T) {
	var b strings.Builder
	b.WriteString(testConfig)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, "  - name: l%d\n    label: \"%s\"\n    touch:\n      cols: 16\n      rows: 16\n      cells:\n", i, strings.Repeat("長", 20))
		for c := 0; c < 16; c++ {
			for r := 0; r < 16; r++ {
				fmt.Fprintf(&b, "        \"%d,%d\": { key: LCTRL+LSHIFT+F%d, label: \"セル%d-%d\" }\n", c, r, c%12+1, c, r)
			}
		}
	}
	d := newTestDaemon(t, b.String())
	cfg := d.currentJSON()
	if n := len(cfg["layers"].([]any)); n != 12 {
		t.Fatalf("layers %d", n)
	}
	raw, _ := json.Marshal(cfg)
	if len(raw) < 100<<10 || len(raw) > maxLineBytes-1000 {
		t.Fatalf("test config too small: %d", len(raw))
	}
	// 同じものを送り返して検証できる（上限 256KiB に収まる）
	if r := result(t, d.call("validate", map[string]any{"config": cfg})); r["valid"] != true {
		t.Fatalf("validate %v", r)
	}
}

// ---------- 検証と保存 ----------

func TestProtocolValidate(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	cfg := d.currentJSON()
	base := cfg["layers"].([]any)[0].(map[string]any)
	base["keys"].(map[string]any)["KEY_W"] = map[string]any{"key": "LCTRL+NOPE"}
	base["touch"].(map[string]any)["cells"].(map[string]any)["9,9"] = map[string]any{"key": "A"}
	r := result(t, d.call("validate", map[string]any{"config": cfg}))
	if r["valid"] != false {
		t.Fatalf("expected invalid: %v", r)
	}
	paths := map[string]bool{}
	for _, e := range r["errors"].([]any) {
		paths[e.(map[string]any)["path"].(string)] = true
	}
	if !paths["/layers/0/keys/KEY_W"] || !paths["/layers/0/touch/cells/9,9"] {
		t.Fatalf("paths %v", paths)
	}
	// 保存したファイルは変わらない
	if b, _ := os.ReadFile(d.path); string(b) != testConfig {
		t.Fatal("validate must not write the file")
	}

	// ファイルの読み込み：YAML の文字列も検証でき、layers の形にそろえて返す
	r = result(t, d.call("validate", map[string]any{"text": "keys: { KEY_Q: B }\n" +
		"touch: { min_x: 0, max_x: 1, min_y: 0, max_y: 1, cols: 2, rows: 1, cells: { \"1,0\": A } }\n"}))
	if r["valid"] != true || len(r["config"].(map[string]any)["layers"].([]any)) != 1 {
		t.Fatalf("legacy text: %v", r)
	}
	r = result(t, d.call("validate", map[string]any{"text": "layers: [ {name: base, keys: {KEY_Q: B}"}))
	if r["valid"] != false || len(r["errors"].([]any)) == 0 {
		t.Fatalf("broken yaml: %v", r)
	}
	// 動作中には変えられない項目
	cfg = d.currentJSON()
	cfg["hid_device"] = "/etc/passwd"
	r = result(t, d.call("validate", map[string]any{"config": cfg}))
	if r["valid"] != false || r["errors"].([]any)[0].(map[string]any)["path"] != "/hid_device" {
		t.Fatalf("restart field: %v", r)
	}
	if m := d.call("validate", nil); errCode(m) != errBadRequest {
		t.Fatalf("no config: %v", m)
	}
}

func TestSetConfigAppliesWithoutRestart(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	// 押したまま保存する
	d.engine.PressKey(evdev.KEY_Q)
	d.engine.PressKey(evdev.KEY_LEFTALT) // edit を重ねる
	if d.engine.Status().Layer != "edit" {
		t.Fatal("setup")
	}
	cfg := d.currentJSON()
	layers := cfg["layers"].([]any)
	base := layers[0].(map[string]any)
	base["keys"].(map[string]any)["KEY_Q"] = map[string]any{"key": "LCTRL+Z"}
	base["label"] = "新しい基本"
	base["touch"].(map[string]any)["cols"] = 2
	base["touch"].(map[string]any)["rows"] = 2

	r := result(t, d.call("set_config", map[string]any{"config": cfg}))
	if r["saved"] != d.path {
		t.Fatalf("set_config %v", r)
	}
	// ファイル：YAML で保存し、前の版を .prev に残す
	saved, _ := os.ReadFile(d.path)
	prev, _ := os.ReadFile(d.path + ".prev")
	if string(prev) != testConfig {
		t.Fatalf(".prev = %q", prev)
	}
	if !strings.Contains(string(saved), "KEY_Q: LCTRL+Z") || !strings.Contains(string(saved), "label: 新しい基本") ||
		!strings.Contains(string(saved), `"0,0": {key: B, label: ブラシ}`) {
		t.Fatalf("saved YAML:\n%s", saved)
	}
	if strings.Contains(string(saved), "手で書いたコメント") || !strings.Contains(string(saved), "コメントは消える") {
		t.Fatal("comment handling")
	}
	if _, _, _, err := checkConfig(saved); err != nil {
		t.Fatalf("saved file does not pass -check: %v", err)
	}
	if fs, _ := filepath.Glob(filepath.Join(filepath.Dir(d.path), ".*tmp*")); len(fs) > 0 {
		t.Fatalf("temporary files left: %v", fs)
	}

	// 動作：押していたキーは離され（空のレポート）、重なりは base に戻り、新しい割り当てになる
	st := d.engine.Status()
	if st.Layer != "base" || st.Label != "新しい基本" || st.Cols != 2 {
		t.Fatalf("status %+v", st)
	}
	hid, _ := os.ReadFile(d.hid)
	if last := hid[len(hid)-8:]; string(last) != string(make([]byte, 8)) {
		t.Fatalf("last report % x, want empty", last)
	}
	d.engine.ReleaseKey(evdev.KEY_Q)       // 差し替え前に押したキーは、離しても何も送らない
	d.engine.ReleaseKey(evdev.KEY_LEFTALT) //
	if after, _ := os.ReadFile(d.hid); len(after) != len(hid) {
		t.Fatalf("release after reload sent % x", after[len(hid):])
	}
	d.engine.PressKey(evdev.KEY_Q)
	hid2, _ := os.ReadFile(d.hid)
	if got := hid2[len(hid2)-8:]; got[0] != 0x01 || got[2] != 0x1d {
		t.Fatalf("new mapping sent % x, want Ctrl+Z", got)
	}

	// get_config は新しい設定を返す
	if l := d.currentJSON()["layers"].([]any)[0].(map[string]any)["label"]; l != "新しい基本" {
		t.Fatalf("get_config label %v", l)
	}
}

func TestSetConfigInvalidChangesNothing(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	cfg := d.currentJSON()
	cfg["layers"].([]any)[0].(map[string]any)["keys"].(map[string]any)["KEY_Q"] = map[string]any{"layer_hold": "nope"}
	m := d.call("set_config", map[string]any{"config": cfg})
	if errCode(m) != errInvalid {
		t.Fatalf("got %v", m)
	}
	p := m["error"].(map[string]any)["problems"].([]any)[0].(map[string]any)
	if p["path"] != "/layers/0/keys/KEY_Q" {
		t.Fatalf("problem %v", p)
	}
	if b, _ := os.ReadFile(d.path); string(b) != testConfig {
		t.Fatal("file changed")
	}
	if _, err := os.Stat(d.path + ".prev"); err == nil {
		t.Fatal(".prev should not be written")
	}
	if m := d.call("set_config", map[string]any{"text": testConfig}); errCode(m) != errBadRequest {
		t.Fatalf("text: %v", m)
	}
}

func TestSetConfigRollbackOnApplyFailure(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	real := d.store.apply
	calls := 0
	d.store.apply = func(c *Config, km *Keymap) error {
		calls++
		if calls == 1 {
			return errors.New("simulated failure")
		}
		return real(c, km) // 2 回目は前の設定を戻す
	}
	cfg := d.currentJSON()
	cfg["layers"].([]any)[0].(map[string]any)["label"] = "変更"
	m := d.call("set_config", map[string]any{"config": cfg})
	if errCode(m) != errApply || !strings.Contains(m["error"].(map[string]any)["message"].(string), "simulated failure") {
		t.Fatalf("got %v", m)
	}
	if b, _ := os.ReadFile(d.path); string(b) != testConfig {
		t.Fatalf("file was not restored:\n%s", b)
	}
	if calls != 2 || d.engine.Status().Label != "基本" {
		t.Fatalf("calls %d status %+v", calls, d.engine.Status())
	}
	if l := d.currentJSON()["layers"].([]any)[0].(map[string]any)["label"]; l != "基本" {
		t.Fatalf("get_config label %v", l)
	}

	// 反映中の panic も失敗として扱い、前の設定に戻す
	d.store.apply = reloader(nil) // nil の Engine で panic する
	if m := d.call("set_config", map[string]any{"config": cfg}); errCode(m) != errInternal && errCode(m) != errApply {
		t.Fatalf("panic: %v", m)
	}
	if b, _ := os.ReadFile(d.path); string(b) != testConfig {
		t.Fatal("file was not restored after panic")
	}
}

// ---------- 入力の通知 ----------

func TestSubscribeInput(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	// 購読していなければ、何もせずエンジンに渡す
	if d.monitor.KeyPressed(d.engine, evdev.KEY_Q) {
		t.Fatal("not subscribed: must not suppress")
	}
	result(t, d.call("subscribe_input", map[string]any{"enable": true}))
	if d.monitor.KeyPressed(d.engine, evdev.KEY_Q) {
		t.Fatal("without suppress, input goes to the engine too")
	}
	m := d.readMsg()
	if m["event"] != "input" || m["type"] != "key" || m["code"] != "KEY_Q" || m["layer"] != "base" {
		t.Fatalf("event %v", m)
	}
	// 学習モード（suppress）：PC には送らない
	result(t, d.call("subscribe_input", map[string]any{"enable": true, "suppress": true}))
	if !d.monitor.TouchPressed(d.engine, 399, 0) {
		t.Fatal("suppress")
	}
	m = d.readMsg()
	if m["type"] != "touch" || m["col"] != float64(3) || m["row"] != float64(0) || m["suppressed"] != true {
		t.Fatalf("touch event %v", m)
	}
	if !d.monitor.TouchPressed(d.engine, 4000, 3000) {
		t.Fatal("suppress")
	}
	if m = d.readMsg(); m["soft"] != "home" {
		t.Fatalf("soft event %v", m)
	}
	// レイヤーが変わったことも知らせる
	d.engine.PressKey(evdev.KEY_LEFTALT)
	if m = d.readMsg(); m["event"] != "layer" || m["layer"] != "edit" || m["mode"] != "temp" {
		t.Fatalf("layer event %v", m)
	}
	d.engine.ReleaseKey(evdev.KEY_LEFTALT)
	d.readMsg()
	result(t, d.call("subscribe_input", map[string]any{"enable": false}))
	if d.monitor.KeyPressed(d.engine, evdev.KEY_Q) || d.monitor.subscribed() {
		t.Fatal("unsubscribed")
	}
}

// 学習モードは、GUI が何も言わなくなったら自動で解ける（Brain が使えなくならない）
func TestSuppressLeaseExpires(t *testing.T) {
	m := &Monitor{}
	ch := make(chan []byte, 4)
	m.subscribe(ch, true)
	if !m.suppressing() {
		t.Fatal("should suppress")
	}
	m.mu.Lock()
	m.suppressUntil = time.Now().Add(-time.Second)
	m.mu.Unlock()
	if m.suppressing() {
		t.Fatal("lease should have expired")
	}
	if !m.subscribed() {
		t.Fatal("still subscribed (events only)")
	}
}

// PC が読まなくても、入力の処理は待たない。接続が切れたら購読も学習モードも解ける
func TestInputNeverBlocks(t *testing.T) {
	old := writeTimeout
	writeTimeout = 100 * time.Millisecond
	defer func() { writeTimeout = old }()
	d := newTestDaemon(t, testConfig)
	result(t, d.call("subscribe_input", map[string]any{"enable": true, "suppress": true}))
	// ここから GUI は何も読まない
	start := time.Now()
	for i := 0; i < 1000; i++ {
		d.monitor.KeyPressed(d.engine, evdev.KEY_Q)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("input was blocked for %v", el)
	}
	select {
	case err := <-d.served:
		if err == nil {
			t.Fatal("Serve should return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not give up on the stuck writer")
	}
	if d.monitor.subscribed() || d.monitor.suppressing() {
		t.Fatal("subscription must end with the connection")
	}
	if d.monitor.KeyPressed(d.engine, evdev.KEY_Q) {
		t.Fatal("input must go to the engine again")
	}
}

// 通知とレスポンスが混ざらない：どの行も完全な JSON
func TestNoInterleaving(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	result(t, d.call("subscribe_input", map[string]any{"enable": true}))
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				d.monitor.TouchPressed(d.engine, 10, 10)
			}
		}
	}()
	for i := 0; i < 10; i++ {
		d.call("get_keymap", nil) // readMsg が、壊れた行で失敗する
	}
	close(stop)
}

// ---------- ファイル ----------

func TestMarshalConfigRoundTrip(t *testing.T) {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// 送れるキーの名前をすべて使い、YAML で特別な意味を持つ値（y、no、1 など）も試す
	keys := map[string]ActionSpec{}
	i := 0
	for name := range hidUsage {
		keys[fmt.Sprintf("KEY_F%d", i%24+1)] = ActionSpec{Key: name}
		cfg.Layers[1].Keys = keys
		out, err := marshalConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		again, _, _, err := checkConfig(out)
		if err != nil || !reflect.DeepEqual(again, cfg) {
			t.Fatalf("%s: round trip failed (%v):\n%s", name, err, out)
		}
		i++
	}
	cfg.Layers[0].Touch.Cells["0,0"] = ActionSpec{Key: "B", Label: "二行の\nラベル: #x"}
	out, _ := marshalConfig(cfg)
	if again, _, _, err := checkConfig(out); err != nil || !reflect.DeepEqual(again, cfg) {
		t.Fatalf("label round trip failed (%v):\n%s", err, out)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("v1"), 0o640)
	if err := writeFileAtomic(p, []byte("v2"), true); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("v3"), true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	prev, _ := os.ReadFile(p + ".prev")
	st, _ := os.Stat(p)
	if string(b) != "v3" || string(prev) != "v2" || st.Mode().Perm() != 0o640 {
		t.Fatalf("%q %q %v", b, prev, st.Mode())
	}
	// 書けないディレクトリでは、元のファイルはそのまま
	if err := writeFileAtomic("/nonexistent/dir/c.yaml", []byte("x"), true); err == nil {
		t.Fatal("expected error")
	}
}

// 実際の tty（pty）で、生のモードになっていること：エコーしない、改行を変えない、4096 バイトを超える行も届く
func TestSerialRawModeOnPTY(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	defer master.Close()
	var n uint32
	unlock := int32(0)
	rc, _ := master.SyscallConn()
	rc.Control(func(fd uintptr) {
		ioctl(int(fd), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
		ioctl(int(fd), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n)))
	})
	slave, err := openSerial(fmt.Sprintf("/dev/pts/%d", n))
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()

	d := newTestDaemon(t, testConfig)
	go d.ctl.Serve(slave)
	label := strings.Repeat("あ", 3000) // 9000 バイトの行
	req := fmt.Sprintf(`{"id":7,"cmd":"validate","config":{"layers":[{"name":"base","label":%q}]}}`, label)
	go master.Write([]byte(req + "\n"))
	rd := bufio.NewReader(master)
	master.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := rd.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("reply is not one JSON line (echo?): %.200q", line)
	}
	if m["id"] != float64(7) || m["ok"] != true {
		t.Fatalf("reply %v", m)
	}
}
