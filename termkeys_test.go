package main

import (
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// 本体キーの押し方（docs/keymap-pwsh2.md）で、シェルの記号がすべて打てること
// now は、テストのキーのイベントの時刻。呼ぶたびに 100 ミリ秒進む（人が打つ間隔）
var testClock = time.Unix(1e9, 0)

func now() time.Time {
	testClock = testClock.Add(100 * time.Millisecond)
	return testClock
}

// 記号を先に離したときの、カーネルの押し直し（KEY_1 を離して、すぐ KEY_Q を押す）は文字にしない
func TestTermKeysSymbolGhost(t *testing.T) {
	var k termKeys
	t0 := now()
	var got string
	ev := func(code evdev.EvCode, v int32, at time.Time) { got += string(k.event(code, v, false, at).out) }
	// 記号 + Q で 1、記号を離す（カーネル：1 を離して Q を押す）、Q を離す
	ev(evdev.KEY_1, 1, t0)
	ev(evdev.KEY_1, 0, t0.Add(200*time.Millisecond))
	ev(evdev.KEY_Q, 1, t0.Add(200*time.Millisecond+300*time.Microsecond))
	ev(evdev.KEY_Q, 0, t0.Add(300*time.Millisecond))
	// Q を押したまま記号を押す（Q を離して 1 を押す）
	ev(evdev.KEY_Q, 1, t0.Add(400*time.Millisecond))
	ev(evdev.KEY_Q, 0, t0.Add(500*time.Millisecond))
	ev(evdev.KEY_1, 1, t0.Add(500*time.Millisecond+200*time.Microsecond))
	ev(evdev.KEY_1, 0, t0.Add(600*time.Millisecond))
	// シフトと記号で |（G）。記号を先に離しても g は入らない
	ev(evdev.KEY_LEFTSHIFT, 1, t0.Add(700*time.Millisecond))
	ev(evdev.KEY_BACKSLASH, 1, t0.Add(750*time.Millisecond))
	ev(evdev.KEY_BACKSLASH, 0, t0.Add(800*time.Millisecond))
	ev(evdev.KEY_G, 1, t0.Add(800*time.Millisecond+100*time.Microsecond))
	ev(evdev.KEY_LEFTSHIFT, 0, t0.Add(850*time.Millisecond))
	ev(evdev.KEY_G, 0, t0.Add(900*time.Millisecond))
	// 人がふつうに 1 と q を続けて打つ（離してから 80 ミリ秒）のは、両方とも入る
	ev(evdev.KEY_1, 1, t0.Add(1000*time.Millisecond))
	ev(evdev.KEY_1, 0, t0.Add(1050*time.Millisecond))
	ev(evdev.KEY_Q, 1, t0.Add(1130*time.Millisecond))
	ev(evdev.KEY_Q, 0, t0.Add(1200*time.Millisecond))
	if got != "1q|1q" {
		t.Errorf("typed %q, want %q", got, "1q|1q")
	}
	if symbolPairs[evdev.KEY_Q] != evdev.KEY_1 || symbolPairs[evdev.KEY_SLASH] != evdev.KEY_MINUS || symbolPairs[evdev.KEY_G] != evdev.KEY_BACKSLASH {
		t.Errorf("symbol pairs: %v", symbolPairs)
	}
}

func TestTermKeysSymbols(t *testing.T) {
	type step struct {
		code  evdev.EvCode
		value int32
	}
	press := func(mods []evdev.EvCode, code evdev.EvCode) []step {
		var s []step
		for _, m := range mods {
			s = append(s, step{m, 1})
		}
		s = append(s, step{code, 1}, step{code, 0})
		for _, m := range mods {
			s = append(s, step{m, 0})
		}
		return s
	}
	S := []evdev.EvCode{evdev.KEY_LEFTSHIFT}
	cases := []struct {
		name  string
		steps []step
		want  string
	}{
		// 記号 + シフト + G は、カーネルが KEY_LEFTSHIFT + KEY_BACKSLASH で届ける
		{"| (記号+シフト+G)", press(S, evdev.KEY_BACKSLASH), "|"},
		{"~ (記号+シフト+D)", press(S, evdev.KEY_GRAVE), "~"},
		{"\\ (記号+G)", press(nil, evdev.KEY_BACKSLASH), "\\"},
		{"{ (記号+シフト+K)", press(S, evdev.KEY_LEFTBRACE), "{"},
		{"} (記号+シフト+L)", press(S, evdev.KEY_RIGHTBRACE), "}"},
		{"[ (記号+K)", press(nil, evdev.KEY_LEFTBRACE), "["},
		{"< (記号+シフト+N)", press(S, evdev.KEY_COMMA), "<"},
		{"> (記号+シフト+M)", press(S, evdev.KEY_DOT), ">"},
		{"& (記号+シフト+U)", press(S, evdev.KEY_7), "&"},
		{"; (記号+H)", press(nil, evdev.KEY_SEMICOLON), ";"},
		{"$ (記号+シフト+R)", press(S, evdev.KEY_4), "$"},
		{"^ (記号+シフト+Y)", press(S, evdev.KEY_6), "^"},
		{"* (記号+シフト+I)", press(S, evdev.KEY_8), "*"},
		{"# (記号+シフト+E)", press(S, evdev.KEY_3), "#"},
		{"? (記号+シフト+−)", press(S, evdev.KEY_SLASH), "?"},
		{"_ (シフト+−)", press(S, evdev.KEY_MINUS), "_"},
		{"\" (記号+シフト+J)", press(S, evdev.KEY_APOSTROPHE), "\""},
		{"Ctrl+C", press([]evdev.EvCode{evdev.KEY_LEFTCTRL}, evdev.KEY_C), "\x03"},
		{"Alt+b", press([]evdev.EvCode{evdev.KEY_LEFTALT}, evdev.KEY_B), "\x1bb"},
		{"Tab (国語)", press(nil, evdev.KEY_TAB), "\t"},
		{"Esc (戻る)", press(nil, evdev.KEY_ESC), "\x1b"},
		{"Enter (決定)", press(nil, evdev.KEY_ENTER), "\r"},
		{"BS", press(nil, evdev.KEY_BACKSPACE), "\x7f"},
		{"↑", press(nil, evdev.KEY_UP), "\x1b[A"},
		{"Ctrl+→", press([]evdev.EvCode{evdev.KEY_LEFTCTRL}, evdev.KEY_RIGHT), "\x1b[1;5C"},
		{"Del (マーカーテスト)", press(nil, evdev.KEY_DELETE), "\x1b[3~"},
		{"大文字", press(S, evdev.KEY_Q), "Q"},
	}
	for _, c := range cases {
		var k termKeys
		var got string
		for _, s := range c.steps {
			r := k.event(s.code, s.value, false, now())
			got += string(r.out)
			if r.exit {
				t.Errorf("%s: exits", c.name)
			}
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTermKeysExitAndScroll(t *testing.T) {
	var k termKeys
	k.event(evdev.KEY_LEFTALT, 1, false, now())
	if r := k.event(evdev.KEY_ESC, 1, false, now()); !r.exit || r.out != nil {
		t.Errorf("Alt+Esc = %+v, want exit", r)
	}
	k.event(evdev.KEY_ESC, 0, false, now())
	k.event(evdev.KEY_LEFTALT, 0, false, now())
	if r := k.event(evdev.KEY_ESC, 1, false, now()); r.exit {
		t.Error("Esc alone exits")
	}
	k.event(evdev.KEY_LEFTSHIFT, 1, false, now())
	if r := k.event(evdev.KEY_PAGEUP, 1, false, now()); r.scroll != 1 {
		t.Errorf("Shift+PgUp = %+v", r)
	}
	k.event(evdev.KEY_LEFTSHIFT, 0, false, now())
	if r := k.event(evdev.KEY_PAGEUP, 1, false, now()); string(r.out) != "\x1b[5~" {
		t.Errorf("PgUp = %q", r.out)
	}
	// カーネルのオートリピートは使わない（termmode.go が自分で繰り返す）
	if r := k.event(evdev.KEY_A, 2, false, now()); r.out != nil {
		t.Errorf("kernel repeat = %q", r.out)
	}
	// アプリケーションのカーソルキー
	if r := k.event(evdev.KEY_UP, 1, true, now()); string(r.out) != "\x1bOA" {
		t.Errorf("app cursor = %q", r.out)
	}
}

func TestTermPadSticky(t *testing.T) {
	var k termKeys
	k.oneCtrl = true
	if got := string(k.padBytes(termPadKey{Send: "["}, false)); got != "\x1b" {
		t.Errorf("Ctrl+[ = %q", got)
	}
	if got := string(k.padBytes(termPadKey{Send: "["}, false)); got != "[" {
		t.Errorf("sticky Ctrl stayed: %q", got)
	}
	k.oneCtrl = true
	if r := k.event(evdev.KEY_C, 1, false, now()); string(r.out) != "\x03" {
		t.Errorf("touch Ctrl + C key = %q", r.out)
	}
	k.oneAlt = true
	if got := string(k.padBytes(termPadKey{Send: "|"}, false)); got != "\x1b|" {
		t.Errorf("Alt+| = %q", got)
	}
	if got := string(k.padBytes(termPadKey{Send: "\x1b[A"}, true)); got != "\x1bOA" {
		t.Errorf("pad ↑ in app cursor mode = %q", got)
	}
	for _, p := range termPadPages {
		for _, row := range p.Keys {
			for _, key := range row {
				if key.Label == "" || (key.Send == "" && key.Mod == "") {
					t.Errorf("page %s: empty key %+v", p.Name, key)
				}
			}
		}
	}
}
