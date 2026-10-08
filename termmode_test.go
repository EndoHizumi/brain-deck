package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// termRig は、端末モードを、Brain の getty、USB の付け直し、シリアルを偽物にして動かす。
// 起きたことを順に ev に記録する。
type termRig struct {
	t     *testing.T
	e     *Engine
	m     *TermMode
	hid   *hidRec
	mu    sync.Mutex
	ev    []string
	pcOut *io.PipeWriter // PC が Brain に送る
	sent  strings.Builder
	conn  *fakeTermConn
}

type fakeTermConn struct {
	r      *io.PipeReader
	rig    *termRig
	closed bool
}

func (c *fakeTermConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *fakeTermConn) Write(b []byte) (int, error) {
	c.rig.mu.Lock()
	defer c.rig.mu.Unlock()
	c.rig.sent.Write(b)
	return len(b), nil
}
func (c *fakeTermConn) Close() error {
	c.rig.log("close ttyGS0")
	c.r.CloseWithError(io.ErrClosedPipe)
	return nil
}
func (c *fakeTermConn) Kind() string { return "serial" }

func (r *termRig) log(s string) {
	r.mu.Lock()
	r.ev = append(r.ev, s)
	r.mu.Unlock()
}

func (r *termRig) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ev...)
}

func (r *termRig) sentText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent.String()
}

func (r *termRig) waitState(state string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.m.Info().State != state {
		if time.Now().After(deadline) {
			r.t.Fatalf("terminal state = %s, want %s (events %v)", r.m.Info().State, state, r.events())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const termYAML = layerYAML + `
terminal:
  font: wide
`

func newTermRig(t *testing.T) *termRig {
	old := usbSwitchDelay
	usbSwitchDelay = 0
	t.Cleanup(func() { usbSwitchDelay = old })
	oldMarker := termMarker
	termMarker = filepath.Join(t.TempDir(), "lefthand-terminal")
	t.Cleanup(func() { termMarker = oldMarker })

	src := strings.Replace(termYAML, "      KEY_INSERT: { layer_to: num }\n", "      KEY_INSERT: { layer_to: num }\n      KEY_DELETE: { terminal: toggle }\n", 1)
	cfg, err := parseConfig([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &termRig{t: t, hid: &hidRec{}}
	s := &State{hid: r.hid, active: map[string]Combo{}, mouse: NewMouse(r.hid, 0)}
	r.e = NewEngine(km, s)
	u := NewUSBMode(s, NewHIDWriter(os.DevNull), "/dev/null", "gadget-setup")
	u.run = func(ctx context.Context, script string, mouse, terminal bool) error {
		r.log(map[bool]string{true: "usb terminal", false: "usb normal"}[terminal])
		return nil
	}
	u.detect = func(string) HIDLayout { return HIDLayout{Source: "fake"} }
	r.m = NewTermMode(func() *Config { return cfg }, u, s, "gadget-setup")
	r.m.systemctl = func(args ...string) error {
		r.log("systemctl " + strings.Join(args, " "))
		return nil // is-active も成功（Brain の getty は動いている）
	}
	r.m.openPort = func(path string) (termConn, error) {
		r.log("open " + path)
		pr, pw := io.Pipe()
		r.mu.Lock()
		r.pcOut = pw
		r.conn = &fakeTermConn{r: pr, rig: r}
		c := r.conn
		r.mu.Unlock()
		return c, nil
	}
	r.e.SetTerm(r.m)
	return r
}

func (r *termRig) pc(s string) {
	r.mu.Lock()
	w := r.pcOut
	r.mu.Unlock()
	w.Write([]byte(s))
}

func TestTerminalEnterExit(t *testing.T) {
	r := newTermRig(t)
	r.hid.take()
	// 端末モードに入る（KEY_DELETE に terminal: toggle）
	r.e.PressKey(evdev.KEY_DELETE)
	r.e.ReleaseKey(evdev.KEY_DELETE)
	r.waitState("on")
	if !r.m.Active() {
		t.Fatal("not active")
	}
	if _, err := os.Stat(termMarker); err != nil {
		t.Errorf("marker: %v", err)
	}
	// PC のログイン画面
	r.pc("\r\nUbuntu 22.04\r\n\r\npc login: ")
	deadline := time.Now().Add(2 * time.Second)
	for r.m.Info().Status != "PC のログイン画面" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := r.m.Info().Status; st != "PC のログイン画面" {
		t.Errorf("status = %q", st)
	}
	// 本体キーは端末に回り、HID には何も送らない。base の KEY_Q（B）ではなく q
	r.hid.take()
	press := func(codes ...evdev.EvCode) {
		for _, c := range codes {
			r.e.PressKey(c)
		}
		for i := len(codes) - 1; i >= 0; i-- {
			r.e.ReleaseKey(codes[i])
		}
	}
	press(evdev.KEY_Q)
	press(evdev.KEY_LEFTSHIFT, evdev.KEY_BACKSLASH)
	press(evdev.KEY_LEFTCTRL, evdev.KEY_C)
	r.e.RepeatKey(evdev.KEY_W)
	// タッチのキー：1 ページ目の左上は |
	g := r.m.geometry()
	r.e.PressTouchAt(10, int32(300*(g.Pad.Min.Y+5)/g.H), time.Now())
	r.e.Release("t")
	deadline = time.Now().Add(2 * time.Second)
	for r.sentText() != "q|\x03w|" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.sentText(); got != "q|\x03w|" {
		t.Errorf("sent to PC = %q", got)
	}
	if reps := r.hid.take(); len(reps) > 0 {
		t.Errorf("HID reports in terminal mode: %v", reps)
	}
	// HOME のソフトキーで抜ける（layerYAML の home は x 3950〜、y 2000〜）
	r.e.PressTouchAt(4000, 3000, time.Now())
	r.e.Release("t")
	r.waitState("off")
	if r.m.Active() {
		t.Error("still active")
	}
	if _, err := os.Stat(termMarker); err == nil {
		t.Error("marker left")
	}
	// 抜けたら、本体キーはふだんどおり HID に（KEY_Q は B）
	r.hid.take()
	press(evdev.KEY_Q)
	reps := r.hid.take()
	if len(reps) == 0 || reps[0][2] != hidUsage["B"] {
		t.Errorf("after exit, KEY_Q sent %v", reps)
	}
}

// Brain の getty と PC の getty が、同時に動くときがないこと。
// PC の getty は、USB の構成の名前が端末モードのときだけ動く（contrib/udev/71-brain-terminal.rules）。
func TestTerminalGettyNeverBoth(t *testing.T) {
	r := newTermRig(t)
	for i := 0; i < 3; i++ {
		r.m.Request("on")
		r.waitState("on")
		r.m.Request("off")
		r.waitState("off")
	}
	// 入るのと抜けるのを、待たずに続けても
	r.m.Request("on")
	r.m.Request("off")
	r.m.Request("toggle")
	r.waitState("on")
	r.m.Request("off")
	r.waitState("off")

	brain, pc, open := true, false, false
	for i, ev := range r.events() {
		switch {
		case ev == "systemctl stop serial-getty@ttyGS0.service":
			brain = false
		case ev == "systemctl start serial-getty@ttyGS0.service":
			brain = true
		case ev == "usb terminal":
			pc = true
		case ev == "usb normal":
			pc = false
		case strings.HasPrefix(ev, "open "):
			if brain {
				t.Errorf("event %d: lefthand opened ttyGS0 while the Brain getty runs", i)
			}
			open = true
		case ev == "close ttyGS0":
			open = false
		}
		if brain && pc {
			t.Fatalf("event %d (%s): the Brain getty and the PC getty run at the same time\n%s", i, ev, strings.Join(r.events(), "\n"))
		}
		if brain && open {
			t.Fatalf("event %d (%s): the Brain getty and lefthand share ttyGS0", i, ev)
		}
	}
	if !brain || pc || open {
		t.Errorf("at the end: brain getty %v, pc getty %v, ttyGS0 open %v", brain, pc, open)
	}
}

// 端末モードに入る前に押していたキーは、押したときの割り当てで離す（押しっぱなしにしない）
func TestTerminalReleasesHeldKeys(t *testing.T) {
	r := newTermRig(t)
	r.e.PressKey(evdev.KEY_LEFTALT) // layer_hold: edit
	r.e.PressKey(evdev.KEY_Q)       // edit の Ctrl+C を押したまま
	r.hid.take()
	r.m.Request("on")
	r.waitState("on")
	reps := r.hid.take()
	if len(reps) == 0 || reps[len(reps)-1][0] != 0 || reps[len(reps)-1][2] != 0 {
		t.Errorf("keys not released on entering: %v", reps)
	}
	r.e.ReleaseKey(evdev.KEY_Q)
	r.e.ReleaseKey(evdev.KEY_LEFTALT)
	if st := r.e.Status(); st.Layer != "base" {
		t.Errorf("layer = %s, want base (the hold key was released)", st.Layer)
	}
	if got := r.sentText(); got != "" {
		t.Errorf("released keys reached the terminal: %q", got)
	}
	r.m.Request("off")
	r.waitState("off")
}

func TestTerminalConfigValidate(t *testing.T) {
	for src, want := range map[string]string{
		"terminal: { font: big }":                                  "terminal.font",
		"terminal: { command: [ssh, pc] }":                         "terminal.user is required",
		"terminal: { command: [ssh, pc], user: root }":             "must not be root",
		"terminal: { port: ttyGS0 }":                               "terminal.port",
		"terminal: { getty: getty }":                               "terminal.getty",
		"terminal: { scrollback: -1 }":                             "terminal.scrollback",
		"layers: [{name: base, keys: {KEY_A: {terminal: maybe}}}]": "terminal must be on, off or toggle",
	} {
		if !strings.Contains(src, "layers") {
			src += "\nlayers: [{name: base}]"
		}
		_, _, err := compileYAML(t, src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	if _, _, err := compileYAML(t, "terminal: { command: [ssh, me@192.168.7.1], user: user, font: narrow }\nlayers: [{name: base, keys: {KEY_A: {terminal: toggle, label: 端末}}}]"); err != nil {
		t.Errorf("valid config: %v", err)
	}
}
