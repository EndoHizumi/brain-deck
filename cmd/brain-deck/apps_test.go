//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type appFake struct {
	apps    map[string][]string // 名前 → ファイルの名前
	cur     []byte
	name    string
	deleted []string
}

func (a *appFake) handle(req map[string]any) (any, bool) {
	switch req["cmd"] {
	case "hello":
		return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "upload_kinds": []string{"app"},
			"commands": []string{"hello", "list_apps", "delete_app", "upload_begin"}}, true
	case "upload_begin":
		if req["kind"] != "app" {
			return nil, false
		}
		a.cur, a.name = nil, req["name"].(string)
		return map[string]any{"upload": "u1", "chunk_bytes": 50000}, true
	case "upload_chunk":
		b, _ := base64.StdEncoding.DecodeString(req["data"].(string))
		a.cur = append(a.cur, b...)
		return map[string]any{"received": len(a.cur)}, true
	case "upload_end":
		zr, err := gzip.NewReader(bytes.NewReader(a.cur))
		if err != nil {
			return nil, false
		}
		tr := tar.NewReader(zr)
		var names []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil || h.Typeflag != tar.TypeReg || strings.Contains(h.Name, "/") {
				return nil, false
			}
			names = append(names, h.Name)
		}
		a.apps[a.name] = names
		return map[string]any{"name": a.name, "title": "T", "icon": "counter", "bytes": len(a.cur), "files": len(names)}, true
	case "list_apps":
		var l []any
		keys := []string{}
		for k := range a.apps {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			l = append(l, map[string]any{"name": k, "title": "T", "mode": "interval", "every": "off", "bytes": 10, "files": len(a.apps[k])})
		}
		l = append(l, map[string]any{"name": "broken", "error": "not a usable app: app.yaml is missing", "bytes": 0, "files": 1})
		return map[string]any{"apps": l, "dir": "/var/lib/lefthand/apps"}, true
	case "delete_app":
		n := req["app"].(string)
		if _, ok := a.apps[n]; !ok {
			return nil, false
		}
		delete(a.apps, n)
		a.deleted = append(a.deleted, n)
		return map[string]any{"deleted": n}, true
	}
	return nil, false
}

func TestAppAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	af := &appFake{apps: map[string][]string{}}
	f.handler = af.handle
	go f.serve(nil)

	code, out, errs := runCLI(t, "--port", f.slave, "app", "install", "../../contrib/apps/dice", "../../contrib/apps/load")
	if code != exitOK || !strings.Contains(out, "dice を Brain に置きました") || !strings.Contains(out, "{widget: app, name: load}") {
		t.Fatalf("install: %d %s %s", code, out, errs)
	}
	if strings.Join(af.apps["dice"], ",") != "app.yaml,main.sh" {
		t.Errorf("dice files %v", af.apps["dice"])
	}
	if code, out, _ = runCLI(t, "--port", f.slave, "app"); code != exitOK || !strings.Contains(out, "dice") || !strings.Contains(out, "broken") || !strings.Contains(out, "使えません") {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, out, _ = runCLI(t, "--port", f.slave, "app", "rm", "dice"); code != exitOK || len(af.deleted) != 1 {
		t.Fatalf("rm: %d %s", code, out)
	}

	// 送る前に断るもの
	dir := t.TempDir()
	mk := func(name string, files map[string]string) string {
		p := filepath.Join(dir, name)
		os.MkdirAll(p, 0o755)
		for n, c := range files {
			os.WriteFile(filepath.Join(p, n), []byte(c), 0o644)
		}
		return p
	}
	sub := mk("withdir", map[string]string{"app.yaml": "", "main.sh": ""})
	os.MkdirAll(filepath.Join(sub, "lib"), 0o755)
	link := mk("withlink", map[string]string{"app.yaml": "", "main.sh": ""})
	os.Symlink("/etc/passwd", filepath.Join(link, "passwd"))
	for _, p := range []string{
		sub, link,
		mk("noyaml", map[string]string{"main.sh": ""}),
		mk("norun", map[string]string{"app.yaml": "run: go.sh\n"}),
		mk("BadName", map[string]string{"app.yaml": "", "main.sh": ""}),
		mk("named", map[string]string{"app.yaml": "name: ../x\n", "main.sh": ""}),
		mk("big", map[string]string{"app.yaml": "", "main.sh": strings.Repeat("x", 300<<10)}),
	} {
		if code, _, errs := runCLI(t, "--port", f.slave, "app", "install", p); code != exitUsage {
			t.Errorf("%s: exit %d %s", filepath.Base(p), code, errs)
		}
	}
	// .DS_Store などは送らない
	ok := mk("okapp", map[string]string{"app.yaml": "", "main.sh": "echo", ".DS_Store": "x"})
	if code, _, errs := runCLI(t, "--port", f.slave, "app", "install", ok); code != exitOK || strings.Join(af.apps["okapp"], ",") != "app.yaml,main.sh" {
		t.Errorf("okapp: %d %s %v", code, errs, af.apps["okapp"])
	}
}
