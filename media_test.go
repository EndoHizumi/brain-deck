package main

import (
	"bytes"
	"strings"
	"testing"

	evdev "github.com/holoplot/go-evdev"
)

const mediaKeysConfig = `
touch: { min_x: 226, max_x: 3938, min_y: 3803, max_y: 384 }
layers:
  - name: base
    keys:
      KEY_A: { media: volume_up }
      KEY_S: { media: mute }
      KEY_D: { layer_hold: other }
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { media: play_pause }
        "1,0": { media: next, label: 次 }
  - name: other
    keys:
      KEY_Q: { media: previous }
`

func mediaEngine(t *testing.T, id byte) (*Engine, *hidRec, *Keymap) {
	t.Helper()
	cfg, err := parseConfig([]byte(mediaKeysConfig))
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, kbdID: hidKeyboardID, mouse: NewMouse(h, hidMouseID), media: NewMedia(h, id)}
	return NewEngine(km, s), h, km
}

func mediaReps(reps [][]byte) [][]byte {
	var out [][]byte
	for _, r := range reps {
		if r[0] == hidMediaID {
			out = append(out, r)
		}
	}
	return out
}

func TestEngineMediaKeys(t *testing.T) {
	e, h, _ := mediaEngine(t, hidMediaID)
	e.PressKey(evdev.KEY_A) // 音量+（0xe9）
	e.PressKey(evdev.KEY_S) // ミュート（0xe2）。新しく押したものが先
	e.ReleaseKey(evdev.KEY_A)
	e.PressKey(evdev.KEY_D) // layer_hold。押しているメディアキーは離さない（離すのはそのキーを離したとき）
	e.ReleaseKey(evdev.KEY_S)
	e.ReleaseKey(evdev.KEY_D)
	want := [][]byte{
		{hidMediaID, 0xe9, 0, 0, 0},
		{hidMediaID, 0xe2, 0, 0xe9, 0},
		{hidMediaID, 0xe2, 0, 0, 0},
		{hidMediaID, 0, 0, 0, 0},
	}
	got := mediaReps(h.take())
	if len(got) != len(want) {
		t.Fatalf("reports:\n% x\nwant\n% x", got, want)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("report %d: % x, want % x", i, got[i], want[i])
		}
	}
	// 3 つ押すと、新しい 2 つだけを送る。番号が 0xff より大きいもの（電卓 0x192）も 16 ビットで送る
	m := e.out.media
	m.Press("a", mediaActions["mute"])
	m.Press("b", mediaActions["volume_up"])
	m.Press("c", mediaActions["calculator"])
	reps := mediaReps(h.take())
	if last := reps[len(reps)-1]; !bytes.Equal(last, []byte{hidMediaID, 0x92, 0x01, 0xe9, 0}) {
		t.Errorf("3 keys: % x", last)
	}
	m.ReleaseAll()
	if reps := mediaReps(h.take()); len(reps) != 1 || !bytes.Equal(reps[0], []byte{hidMediaID, 0, 0, 0, 0}) {
		t.Errorf("release all: % x", reps)
	}
}

// TestMediaBootKeyboard は、ブートキーボードの形（メディアキーがない）では何も送らないことを確かめる。
func TestMediaBootKeyboard(t *testing.T) {
	e, h, _ := mediaEngine(t, 0)
	e.PressKey(evdev.KEY_A)
	e.ReleaseKey(evdev.KEY_A)
	e.out.releaseAll()
	e.out.media.retry()
	if reps := mediaReps(h.take()); len(reps) != 0 {
		t.Errorf("sent media reports without media keys: % x", reps)
	}
	if e.out.media.Available() {
		t.Error("Available with id 0")
	}
}

// TestMediaRetry は、送れなかった状態を送り直すことを確かめる（離したのが届かず押しっぱなしにならない）。
func TestMediaRetry(t *testing.T) {
	h := &hidRec{}
	m := NewMedia(h, hidMediaID)
	m.Press("k", mediaActions["volume_down"])
	h.mu.Lock()
	h.fail = true
	h.mu.Unlock()
	m.Release("k")
	h.mu.Lock()
	h.fail = false
	h.mu.Unlock()
	m.retry()
	reps := h.take()
	if len(reps) != 2 || !bytes.Equal(reps[1], []byte{hidMediaID, 0, 0, 0, 0}) {
		t.Errorf("reports: % x", reps)
	}
	m.retry()
	if reps := h.take(); len(reps) != 0 {
		t.Errorf("retry after success sent % x", reps)
	}
}

func TestMediaConfig(t *testing.T) {
	_, _, km := mediaEngine(t, hidMediaID)
	if !km.usesMedia() {
		t.Error("usesMedia")
	}
	v := cellView(km, km.Layers[0].Grid.Cells[cellPos{0, 0}])
	if !v.Media || v.Label != "再生/停止" || v.Sub != "" {
		t.Errorf("play_pause cell: %+v", v)
	}
	v = cellView(km, km.Layers[0].Grid.Cells[cellPos{1, 0}])
	if !v.Media || v.Label != "次" || v.Sub != "次の曲" {
		t.Errorf("labeled cell: %+v", v)
	}
	for _, bad := range []string{
		"layers: [{ name: base, keys: { KEY_A: { media: louder } } }]",
		"layers: [{ name: base, keys: { KEY_A: { media: mute, key: A } } }]",
	} {
		cfg, err := parseConfig([]byte(bad))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	cfg, _ := parseConfig([]byte("layers: [{ name: base, keys: { KEY_A: { media: louder } } }]"))
	if _, _, err := compileKeymap(cfg); err == nil || !strings.Contains(err.Error(), "play_pause") {
		t.Errorf("error should list the names: %v", err)
	}
	for _, n := range mediaActionNames {
		if _, ok := mediaActions[n]; !ok {
			t.Errorf("%s is not in mediaActions", n)
		}
	}
	if len(mediaActionNames) != len(mediaActions) {
		t.Error("mediaActionNames and mediaActions differ")
	}
}
