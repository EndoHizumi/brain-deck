package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	content := strings.Repeat("あいう", 100)
	var deleted, putPath, putName string
	var putData []byte
	f.handler = func(req map[string]any) (any, bool) {
		switch req["cmd"] {
		case "hello":
			return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "commands": []string{"hello", "list_files", "get_file", "delete_file"}}, true
		case "list_files":
			if req["path"] == "" {
				return map[string]any{"path": "", "entries": []any{map[string]any{"name": "ホーム", "path": "/home/user", "dir": true, "size": 0, "mtime": "2026-10-11T00:00:00Z"}}, "free_bytes": 1 << 30}, true
			}
			return map[string]any{"path": req["path"], "root": "ホーム", "delete": true, "entries": []any{
				map[string]any{"name": "sub", "path": "/home/user/sub", "dir": true, "size": 4096, "mtime": "2026-10-11T00:00:00Z"},
				map[string]any{"name": "a.txt", "path": "/home/user/a.txt", "size": len(content), "mtime": "2026-10-11T00:00:00Z"}}, "free_bytes": 1 << 30}, true
		case "get_file":
			off := int(req["offset"].(float64))
			end := min(off+100, len(content))
			return map[string]any{"total": len(content), "data": base64.StdEncoding.EncodeToString([]byte(content[off:end]))}, true
		case "upload_begin":
			putPath, putName = req["path"].(string), req["name"].(string)
			return map[string]any{"upload": "u1", "chunk_bytes": 4}, true
		case "upload_chunk":
			b, _ := base64.StdEncoding.DecodeString(req["data"].(string))
			putData = append(putData, b...)
			return map[string]any{}, true
		case "upload_end":
			return map[string]any{"path": putPath + "/" + putName}, true
		case "delete_file":
			deleted = req["path"].(string)
			return map[string]any{"deleted": deleted}, true
		}
		return nil, false
	}
	go f.serve(nil)
	if code, out, _ := runCLI(t, "--port", f.slave, "files"); code != exitOK || !strings.Contains(out, "/home/user") {
		t.Fatalf("roots: %d %s", code, out)
	}
	if code, out, _ := runCLI(t, "--port", f.slave, "files", "ls", "/home/user", "--sort", "size"); code != exitOK || !strings.Contains(out, "sub/") || !strings.Contains(out, "a.txt") {
		t.Fatalf("ls: %d %s", code, out)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if code, out, errs := runCLI(t, "--port", f.slave, "files", "get", "/home/user/a.txt", "-o", p); code != exitOK {
		t.Fatalf("get: %d %s %s", code, out, errs)
	}
	if b, _ := os.ReadFile(p); string(b) != content {
		t.Errorf("got %d bytes", len(b))
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "files", "get", "/home/user/a.txt", "-o", p); code != exitUsage {
		t.Errorf("overwrite without --force: %d", code)
	}
	src := filepath.Join(dir, "n.bin")
	os.WriteFile(src, []byte("0123456789"), 0o644)
	if code, out, errs := runCLI(t, "--port", f.slave, "files", "put", src, "/home/user/sub"); code != exitOK || putPath != "/home/user/sub" || putName != "n.bin" || string(putData) != "0123456789" {
		t.Fatalf("put: %d %s %s %q %q %q", code, out, errs, putPath, putName, putData)
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "files", "rm", "/home/user/a.txt"); code != exitOK || deleted != "/home/user/a.txt" {
		t.Errorf("rm: %d %q", code, deleted)
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "files", "ls", "--sort", "big"); code != exitUsage {
		t.Errorf("bad sort: %d", code)
	}
}
