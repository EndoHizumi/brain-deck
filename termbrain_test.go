package main

import (
	"image"
	"io"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// fakeLogin は、/bin/login の代わり（パスワードは聞くだけ）。
type fakeLogin struct {
	r    *io.PipeReader
	w    *io.PipeWriter
	in   strings.Builder
	user string
}

func (f *fakeLogin) Read(b []byte) (int, error)  { return f.r.Read(b) }
func (f *fakeLogin) Write(b []byte) (int, error) { f.in.Write(b); return len(b), nil }
func (f *fakeLogin) Close() error                { f.r.CloseWithError(io.ErrClosedPipe); return nil }
func (f *fakeLogin) Kind() string                { return "command" }

func brainRig(t *testing.T) (*termRig, *bool, *[]*fakeLogin) {
	r := newTermRig(t)
	online := true
	r.m.SetOnline(func() bool { return online })
	var logins []*fakeLogin
	r.m.startLogin = func(user string, cols, rows int, env []string) (termConn, error) {
		pr, pw := io.Pipe()
		f := &fakeLogin{r: pr, w: pw, user: user}
		logins = append(logins, f)
		r.log("login " + user)
		go pw.Write([]byte("Password: "))
		return f, nil
	}
	return r, &online, &logins
}

func TestTerminalBrainOffline(t *testing.T) {
	r, online, logins := brainRig(t)
	*online = false
	r.e.SetOffline(true)
	// PC から外しているとき、terminal: toggle（KEY_DELETE）は Brain にログインする（休みにしない）
	r.e.PressKey(evdev.KEY_DELETE)
	r.e.ReleaseKey(evdev.KEY_DELETE)
	r.waitState("on")
	if in := r.m.Info(); in.Transport != "brain" || len(*logins) != 1 || (*logins)[0].user != defaultBrainUser {
		t.Fatalf("info %+v, logins %d", in, len(*logins))
	}
	// USB の構成は変えない（getty も触らない）
	for _, ev := range r.events() {
		if strings.HasPrefix(ev, "usb") || strings.HasPrefix(ev, "systemctl") || strings.HasPrefix(ev, "open") {
			t.Errorf("event %q while opening the Brain shell", ev)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for r.m.Info().Status != "Brain のログイン画面" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := r.m.Info().Status; st != "Brain のログイン画面" {
		t.Errorf("status %q", st)
	}
	// PC をつないでも、シェルは閉じない
	*online = true
	r.e.SetOffline(false)
	r.m.LinkChanged(true)
	time.Sleep(20 * time.Millisecond)
	if r.m.Info().State != "on" || r.m.Info().Transport != "brain" {
		t.Errorf("after connecting the PC: %+v", r.m.Info())
	}
	// PC を選ぶ terminal: pc は、外しているときは断る
	*online = false
	if _, err := r.m.Request("pc"); err == nil {
		t.Error("terminal: pc while offline")
	}
	r.m.Request("off")
	r.waitState("off")
	for _, ev := range r.events() {
		if strings.HasPrefix(ev, "usb") {
			t.Errorf("event %q while leaving the Brain shell", ev)
		}
	}
}

func TestTerminalChooser(t *testing.T) {
	r, _, logins := brainRig(t)
	// つないでいるとき：選ぶ画面。キーは PC に送らない
	r.hid.take()
	r.e.PressKey(evdev.KEY_DELETE)
	r.e.ReleaseKey(evdev.KEY_DELETE)
	r.waitState("choosing")
	if !r.m.Active() {
		t.Fatal("the chooser does not take the input")
	}
	r.e.PressKey(evdev.KEY_Q)
	r.e.ReleaseKey(evdev.KEY_Q)
	for _, rep := range r.hid.take() {
		for _, b := range rep {
			if b != 0 {
				t.Errorf("a key went to the PC while choosing: % x", rep)
			}
		}
	}
	// タッチで Brain を選ぶ
	_, brain := r.m.geometry().chooseRects()
	r.m.Touch("", image.Pt((brain.Min.X+brain.Max.X)/2, (brain.Min.Y+brain.Max.Y)/2))
	r.waitState("on")
	if r.m.Info().Transport != "brain" || len(*logins) != 1 {
		t.Fatalf("touch brain: %+v", r.m.Info())
	}
	r.m.Request("off")
	r.waitState("off")
	// キーの P で PC
	r.m.Request("on")
	r.waitState("choosing")
	r.m.KeyEvent(evdev.KEY_P, 1)
	r.waitState("on")
	if r.m.Info().Transport != "serial" {
		t.Fatalf("key P: %+v", r.m.Info())
	}
	// PC の端末の途中で外れたら、画面に出す
	r.m.LinkChanged(false)
	if st := r.m.Info().Status; st != "PC から外れました" {
		t.Errorf("status after unplugging %q", st)
	}
	r.m.Request("off")
	r.waitState("off")
	// 選ぶ画面は、戻る（BS）でやめる
	r.m.Request("on")
	r.waitState("choosing")
	r.m.KeyEvent(evdev.KEY_BACKSPACE, 1)
	r.waitState("off")
	if r.m.Active() {
		t.Error("still active after cancelling")
	}
}

func TestTerminalBrainUserNotRoot(t *testing.T) {
	cfg, err := parseConfig([]byte(layerYAML + "terminal:\n  brain_user: root\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := compileKeymap(cfg); err == nil || !strings.Contains(err.Error(), "must not be root") {
		t.Errorf("brain_user root: %v", err)
	}
	if _, err := startLoginPTY("root", 80, 24, nil); err == nil {
		t.Error("startLoginPTY opened root")
	}
}
