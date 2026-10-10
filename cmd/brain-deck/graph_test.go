package main

import (
	"strings"
	"testing"
)

func TestGraphAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	f.handler = func(req map[string]any) (any, bool) {
		switch req["cmd"] {
		case "add_graph":
			var unknown []string
			for _, p := range req["points"].([]any) {
				if n := p.(map[string]any)["name"]; n != "cpu" && n != "temp" {
					unknown = append(unknown, n.(string))
				}
			}
			return map[string]any{"added": len(req["points"].([]any)), "unknown": unknown}, true
		case "clear_graph":
			return map[string]any{"name": req["name"], "cleared": req["name"] == "cpu"}, true
		case "get_graph":
			return map[string]any{"ids": []string{"cpu", "temp"}, "series": map[string]any{
				"cpu": map[string]any{"count": 3, "points": []any{}, "last": map[string]any{"t": "2026-10-10T03:04:05Z", "v": 12.5}},
				"gpu": map[string]any{"count": 1, "points": []any{}, "last": map[string]any{"t": "2026-10-10T03:04:05Z", "v": -1}}}}, true
		}
		return nil, false
	}
	go f.serve([]string{"hello", "get_status", "set_time", "add_graph", "clear_graph", "get_graph"})
	code, out, errs := runCLI(t, "--port", f.slave, "graph", "cpu", "12.5", "temp", "-3")
	if code != exitOK || out != "cpu: 12.5\ntemp: -3\n" || errs != "" {
		t.Fatalf("graph: %d %q %q", code, out, errs)
	}
	var add map[string]any
	for add == nil {
		if r := <-f.got; r["cmd"] == "add_graph" {
			add = r
		}
	}
	pts := add["points"].([]any)
	if len(pts) != 2 || pts[1].(map[string]any)["value"] != -3.0 || add["source"] != "brain-deck" {
		t.Fatalf("add_graph = %v", add)
	}
	if code, _, errs := runCLI(t, "--port", f.slave, "graph", "gpu", "1"); code != exitOK || !strings.Contains(errs, "widget: graph") {
		t.Errorf("unknown id: %d %q", code, errs)
	}
	if code, out, _ := runCLI(t, "--port", f.slave, "graph", "cpu", "--clear"); code != exitOK || out != "cpu: 値を消しました\n" {
		t.Errorf("clear: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "--port", f.slave, "graph", "--list"); code != exitOK || !strings.Contains(out, "cpu\t3\t12.5\t") ||
		!strings.Contains(out, "gpu\t1\t-1\t") || !strings.Contains(out, "セルなし") || !strings.Contains(out, "temp\t0\t-\t-") {
		t.Errorf("list: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "--port", f.slave, "graph", "--list", "--json"); code != exitOK || !strings.HasPrefix(out, "{") {
		t.Errorf("list --json: %d %q", code, out)
	}
	for _, args := range [][]string{{"graph"}, {"graph", "cpu"}, {"graph", "cpu", "abc"}, {"graph", "cpu", "1", "mem"}, {"graph", "cpu", "NaN"},
		{"graph", "a", "b", "--clear"}, {"graph", "cpu", "1", "--json"}} {
		if code, _, _ := runCLI(t, append([]string{"--port", f.slave}, args...)...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
