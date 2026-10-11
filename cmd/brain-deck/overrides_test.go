package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOverridesAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	values := map[string]any{"backlight.level": 5.0}
	info := func() []any {
		var out []any
		if v, ok := values["backlight.level"]; ok {
			out = append(out, map[string]any{"key": "backlight.level", "label": "明るさ", "shown": jsonString(v), "file": "起動したときの明るさ", "used": true})
		}
		if v, ok := values["lid.close"]; ok {
			out = append(out, map[string]any{"key": "lid.close", "label": "ふたを閉じたとき", "shown": v, "file": "画面を消す", "used": true})
		}
		return out
	}
	var lastSet map[string]any
	f.handler = func(req map[string]any) (any, bool) {
		switch req["cmd"] {
		case "hello":
			return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "commands": []string{"hello", "get_status", "get_overrides", "set_override", "clear_overrides"}}, true
		case "get_overrides":
			return map[string]any{"overrides": info(), "keys": []string{"backlight.level", "lid.close"}}, true
		case "set_override":
			lastSet = req
			values[req["key"].(string)] = req["value"]
			return map[string]any{"overrides": info()}, true
		case "clear_overrides":
			var done []string
			keys, _ := req["keys"].([]any)
			if len(keys) == 0 {
				for k := range values {
					done = append(done, k)
				}
				values = map[string]any{}
			}
			for _, k := range keys {
				delete(values, k.(string))
				done = append(done, k.(string))
			}
			return map[string]any{"cleared": done, "overrides": info()}, true
		case "get_status":
			return map[string]any{"status": map[string]any{"label": "ホーム"}, "time": map[string]any{"now": "2026-10-11T15:00:00+09:00", "synced": true},
				"lid": map[string]any{"closed": true, "close": "off"}, "overrides": info()}, true
		}
		return nil, false
	}
	go f.serve(nil)
	code, out, _ := runCLI(t, "--port", f.slave, "overrides")
	if code != exitOK || !strings.Contains(out, "backlight.level\t明るさ：5（設定ファイル：起動したときの明るさ）") {
		t.Fatalf("list: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "status")
	if code != exitOK || !strings.Contains(out, "ふた\t閉じている。画面を消している") || !strings.Contains(out, "上書き\t明るさ：5") {
		t.Fatalf("status: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "overrides", "set", "lid.close", "none")
	if code != exitOK || lastSet["value"] != "none" || !strings.Contains(out, "ふたを閉じたとき：none") {
		t.Fatalf("set: %d %q %v", code, out, lastSet)
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "overrides", "set", "backlight.level", "3"); code != exitOK || lastSet["value"] != 3.0 {
		t.Fatalf("set number: %d %v", code, lastSet)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "overrides", "clear", "lid.close")
	if code != exitOK || !strings.Contains(out, "lid.close") {
		t.Fatalf("clear: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "overrides", "clear", "all")
	if code != exitOK || !strings.Contains(out, "backlight.level") {
		t.Fatalf("clear all: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "--port", f.slave, "status")
	if code != exitOK || !strings.Contains(out, "上書き\tなし") {
		t.Fatalf("status after clear: %d %q", code, out)
	}
	for _, args := range [][]string{{"overrides", "clear"}, {"overrides", "set", "x"}, {"overrides", "nope"}} {
		if code, _, _ := runCLI(t, append([]string{"--port", f.slave}, args...)...); code != exitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
