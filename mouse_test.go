package main

import (
	"strings"
	"testing"

	evdev "github.com/holoplot/go-evdev"
)

const mouseKeysConfig = `
touch: { min_x: 226, max_x: 3938, min_y: 3803, max_y: 384 }
layers:
  - name: base
    keys:
      KEY_A: { mouse: left }
      KEY_S: { layer_hold: other }
      KEY_D: { mouse: scroll_down }
    touch:
      cols: 4
      rows: 3
      cells:
        "3,1": { mouse: right, label: 右 }
  - name: other
    keys:
      KEY_Q: { mouse: middle }
`

func mouseEngine(t *testing.T) (*Engine, *hidRec, *Keymap) {
	t.Helper()
	cfg, err := parseConfig([]byte(mouseKeysConfig))
	if err != nil {
		t.Fatal(err)
	}
	km, _, err := compileKeymap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &hidRec{}
	return NewEngine(km, &State{hid: h, active: map[string]Combo{}, kbdID: hidKeyboardID, mouse: NewMouse(h, hidMouseID)}), h, km
}

// TestEngineMouseKeysRelease は、キーのマウスのボタンを、レイヤーの切り替え、設定の再読み込み、終了で離すことを確かめる。
func TestEngineMouseKeysRelease(t *testing.T) {
	e, _, km := mouseEngine(t)
	mouse := e.out.mouse
	e.PressKey(evdev.KEY_A)
	if mouse.Held() != mouseLeft {
		t.Fatal("KEY_A should press the left button")
	}
	e.PressKey(evdev.KEY_S) // layer_hold other
	if mouse.Held() != 0 {
		t.Fatal("layer change must release mouse buttons")
	}
	e.ReleaseKey(evdev.KEY_A)
	// 上のレイヤーで押したボタンは、そのレイヤーを抜けると離す
	e.PressKey(evdev.KEY_Q)
	if mouse.Held() != mouseMiddle {
		t.Fatal("KEY_Q in layer other should press middle")
	}
	e.ReleaseKey(evdev.KEY_S)
	if mouse.Held() != 0 {
		t.Fatal("leaving the layer must release middle")
	}
	e.ReleaseKey(evdev.KEY_Q)

	e.PressKey(evdev.KEY_A)
	e.Reload(km)
	if mouse.Held() != 0 {
		t.Fatal("reload must release")
	}
	e.ReleaseKey(evdev.KEY_A)
	e.PressKey(evdev.KEY_A)
	e.out.releaseAll() // shutdown が呼ぶもの
	if mouse.Held() != 0 {
		t.Fatal("releaseAll must release the mouse")
	}
}

func TestEngineMouseCellAndScrollKey(t *testing.T) {
	e, h, _ := mouseEngine(t)
	h.take()
	// セル 3,1（右クリック）を押しているあいだ、右ボタンを押す
	hit := e.PressTouch(3500, 2000)
	if hit.Col != 3 || hit.Row != 1 || !hit.Mapped || e.out.mouse.Held() != mouseRight {
		t.Fatalf("hit %+v, held %d", hit, e.out.mouse.Held())
	}
	e.Release("t")
	e.PressKey(evdev.KEY_D)
	e.ReleaseKey(evdev.KEY_D)
	var reps [][]byte
	for _, r := range h.take() {
		if r[0] == hidMouseID {
			reps = append(reps, r)
		}
	}
	if len(reps) != 3 || reps[0][1] != mouseRight || reps[1][1] != 0 || reps[2][4] != 0xff {
		t.Fatalf("got % x", reps)
	}
	l := buildLayout(e.km, e.View())
	if v := l.Cells[1*4+3]; v.Label != "右" || v.Sub != "右クリック" {
		t.Errorf("cell view %+v", v)
	}
}

func TestMouseConfigErrors(t *testing.T) {
	for cell, want := range map[string]string{
		`{ mouse: wheel }`:        "unknown mouse action",
		`{ mouse: left, key: A }`: "exactly one",
	} {
		y := "touch: { min_x: 0, max_x: 100, min_y: 0, max_y: 100 }\nlayers:\n  - name: base\n    touch:\n      cols: 2\n      rows: 2\n      cells:\n        \"0,0\": " + cell + "\n"
		cfg, err := parseConfig([]byte(y))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", cell, err, want)
		}
	}
}
