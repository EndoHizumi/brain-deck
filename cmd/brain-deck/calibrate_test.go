package main

import (
	"strings"
	"testing"
)

func TestCalibrateAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	polls := 0
	var started, reset string
	f.handler = func(req map[string]any) (any, bool) {
		cfg := map[string]any{"source": "config", "config": map[string]any{"min_x": 226, "max_x": 3938, "min_y": 3803, "max_y": 384}}
		switch req["cmd"] {
		case "hello":
			return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "commands": []string{"hello", "calibrate"}}, true
		case "calibrate":
			switch req["mode"] {
			case "start":
				started = req["kind"].(string)
				return map[string]any{"status": map[string]any{"state": "target", "point": 1, "points": 9, "gen": 2}, "calibration": cfg}, true
			case "reset":
				reset = "yes"
				return map[string]any{"reset": true, "calibration": cfg}, true
			}
			if started == "" {
				return map[string]any{"status": map[string]any{"state": "off", "gen": 1}, "calibration": cfg}, true
			}
			polls++
			st := map[string]any{"state": "target", "point": 3, "points": 9, "gen": 2}
			if polls >= 2 {
				st = map[string]any{"state": "off", "result": "saved", "points": 9, "gen": 2,
					"before": map[string]any{"mean": 12.5, "max": 30}, "after": map[string]any{"mean": 1.5, "max": 3.2}}
			}
			return map[string]any{"status": st, "calibration": map[string]any{"source": "calibration",
				"file": map[string]any{"points": []any{1, 2, 3, 4, 5, 6, 7, 8, 9}, "before": map[string]any{"mean": 12.5, "max": 30}, "after": map[string]any{"mean": 1.5, "max": 3.2}}}}, true
		}
		return nil, false
	}
	go f.serve(nil)
	code, out, errs := runCLI(t, "--port", f.slave, "calibrate", "--points", "9")
	if code != exitOK || started != "touch9" || !strings.Contains(out, "前 平均 12.5、最大 30.0 ドット → 後 平均 1.5、最大 3.2 ドット") || !strings.Contains(errs, "十字 3/9") {
		t.Fatalf("calibrate: %d %q %q", code, out, errs)
	}
	if code, out, _ := runCLI(t, "--port", f.slave, "calibrate", "--reset"); code != exitOK || reset != "yes" || !strings.Contains(out, "設定の min と max に戻しました") {
		t.Fatalf("reset: %d %q", code, out)
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "calibrate", "--points", "7"); code != exitUsage {
		t.Errorf("--points 7: %d", code)
	}
}
