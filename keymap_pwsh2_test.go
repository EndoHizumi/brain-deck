package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	evdev "github.com/holoplot/go-evdev"
)

// GUI は、接続する前（ファイルの編集）に使うため、get_keymap と同じ表を同梱している。
// 表を変えたら LEFTHAND_UPDATE_KEYMAP=1 go test -run KeymapJSON で書き直す。
const guiKeymapJSON = "gui/src/keymap-pwsh2.json"

func TestKeymapJSONInSync(t *testing.T) {
	want, _ := json.MarshalIndent(pwsh2Keymap(), "", "  ")
	want = append(want, '\n')
	if os.Getenv("LEFTHAND_UPDATE_KEYMAP") == "1" {
		if err := os.WriteFile(guiKeymapJSON, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(guiKeymapJSON)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	json.Unmarshal(got, &a)
	json.Unmarshal(want, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s is out of date; run LEFTHAND_UPDATE_KEYMAP=1 go test -run KeymapJSON", guiKeymapJSON)
	}
}

func TestKeymapCodesAreKnown(t *testing.T) {
	ids := map[string]bool{}
	for _, k := range pwsh2Keymap().Keys {
		if ids[k.ID] {
			t.Errorf("duplicate id %s", k.ID)
		}
		ids[k.ID] = true
		for _, c := range []string{k.Code, k.Symbol} {
			if _, ok := evdev.KEYFromString[c]; c != "" && !ok {
				t.Errorf("%s: unknown code %s", k.ID, c)
			}
		}
	}
}
