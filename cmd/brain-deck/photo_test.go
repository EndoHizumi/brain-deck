//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lefthand/internal/imgconv"
)

// photoFake は、Brain の写真の置き場所の代わり。
type photoFake struct {
	files map[string][]byte
	order []string
	of    map[string]string
	cur   []byte
	name  string
	curOf string
}

func (p *photoFake) handle(req map[string]any) (any, bool) {
	switch req["cmd"] {
	case "hello":
		return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "upload_kinds": []string{"photo"},
			"commands": []string{"hello", "list_photos", "get_photo", "delete_photo", "upload_begin"}}, true
	case "upload_begin":
		p.cur, p.name = nil, req["name"].(string)
		p.curOf, _ = req["of"].(string)
		return map[string]any{"upload": "u1", "chunk_bytes": 50000}, true
	case "upload_chunk":
		b, _ := base64.StdEncoding.DecodeString(req["data"].(string))
		p.cur = append(p.cur, b...)
		return map[string]any{"received": len(p.cur)}, true
	case "upload_end":
		id := "a00000000000000" + string(rune('0'+len(p.order)))
		p.files[id] = p.cur
		p.order = append(p.order, id)
		p.of[id] = p.curOf
		return map[string]any{"id": id, "count": len(p.order), "max": 10}, true
	case "list_photos":
		var l []any
		for _, id := range p.order {
			w, h, _, _ := imgconv.Decode(p.files[id])
			l = append(l, map[string]any{"id": id, "name": "写真", "w": w, "h": h, "bytes": len(p.files[id]), "added": "2026-10-11T00:00:00Z", "of": p.of[id]})
		}
		return map[string]any{"photos": l, "max": 10, "total_bytes": 1, "free_bytes": 100 << 20, "reserve_bytes": 64 << 20, "showing": ""}, true
	case "get_photo":
		b := p.files[req["photo"].(string)]
		off := int(req["offset"].(float64))
		end := min(off+300000, len(b))
		return map[string]any{"total": len(b), "data": base64.StdEncoding.EncodeToString(b[off:end])}, true
	case "delete_photo":
		id := req["photo"].(string)
		delete(p.files, id)
		for i, x := range p.order {
			if x == id {
				p.order = append(p.order[:i], p.order[i+1:]...)
			}
		}
		return map[string]any{"deleted": id}, true
	}
	return nil, false
}

func TestPhotoAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	pf := &photoFake{files: map[string][]byte{}, of: map[string]string{}}
	f.handler = pf.handle
	go f.serve(nil)
	dir := t.TempDir()
	// 自分で描いた模様の PNG（1200×700）
	src := image.NewNRGBA(image.Rect(0, 0, 1200, 700))
	for y := 0; y < 700; y++ {
		for x := 0; x < 1200; x++ {
			src.Set(x, y, color.NRGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	p := filepath.Join(dir, "pattern.png")
	var buf bytes.Buffer
	png.Encode(&buf, src)
	os.WriteFile(p, buf.Bytes(), 0o644)

	code, out, errs := runCLI(t, "--port", f.slave, "photo", "add", p)
	if code != exitOK || !strings.Contains(out, "800x480") || !strings.Contains(out, "1 枚 / 上限 10 枚") {
		t.Fatalf("add: %d %s %s", code, out, errs)
	}
	rgba := imgconv.RGBA{W: 1200, H: 700, Pix: src.Pix}
	want, _ := imgconv.Convert(rgba, 800, 480, imgconv.Params{CX: -1, CY: -1, ZoomMilli: 1000, Dither: true})
	if !bytes.Equal(pf.files[pf.order[0]], imgconv.Encode(800, 480, want)) {
		t.Fatal("the uploaded photo differs from imgconv.Convert")
	}
	// 縦向きに切り抜き直す
	code, out, errs = runCLI(t, "--port", f.slave, "photo", "add", p, "--of", "1", "--center", "300,350", "--zoom", "1.5", "--brightness", "-20")
	if code != exitOK || !strings.Contains(out, "480x800") || pf.of[pf.order[1]] != pf.order[0] {
		t.Fatalf("add --of: %d %s %s %v", code, out, errs, pf.of)
	}
	want, _ = imgconv.Convert(rgba, 480, 800, imgconv.Params{CX: 300, CY: 350, ZoomMilli: 1500, Brightness: -20, Dither: true})
	if !bytes.Equal(pf.files[pf.order[1]], imgconv.Encode(480, 800, want)) {
		t.Fatal("the portrait crop differs")
	}
	if code, out, _ = runCLI(t, "--port", f.slave, "photo"); code != exitOK || !strings.Contains(out, "（1 を縦向きに切り抜いたもの）") || !strings.Contains(out, "2 枚（上限 10 枚）") {
		t.Fatalf("list: %d %s", code, out)
	}
	png2 := filepath.Join(dir, "got.png")
	if code, out, errs = runCLI(t, "--port", f.slave, "photo", "get", "1", "-o", png2); code != exitOK {
		t.Fatalf("get: %d %s %s", code, out, errs)
	}
	if fh, err := os.Open(png2); err != nil {
		t.Fatal(err)
	} else if img, err := png.Decode(fh); err != nil || img.Bounds().Dx() != 800 {
		t.Fatalf("got png %v", err)
	}
	if code, out, _ = runCLI(t, "--port", f.slave, "photo", "rm", "2"); code != exitOK || len(pf.order) != 1 {
		t.Fatalf("rm: %d %s", code, out)
	}
	if code, _, errs = runCLI(t, "--port", f.slave, "photo", "rm", "5"); code != exitUsage || !strings.Contains(errs, "番号 5 はありません") {
		t.Fatalf("rm 5: %d %s", code, errs)
	}
	for _, bad := range [][]string{{"photo", "add", p, "--zoom", "9"}, {"photo", "list", "--portrait"}, {"todo", "--portrait"}, {"photo", "get", "1"}} {
		if code, _, _ := runCLI(t, append([]string{"--port", f.slave}, bad...)...); code != exitUsage {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}

func TestExifOrientation(t *testing.T) {
	src := imgconv.RGBA{W: 3, H: 2, Pix: make([]byte, 24)}
	for i := 0; i < 6; i++ {
		src.Pix[i*4] = byte(i)
	}
	at := func(r imgconv.RGBA, x, y int) byte { return r.Pix[(y*r.W+x)*4] }
	r6 := orient(src, 6) // 右に 90 度回して見る
	if r6.W != 2 || r6.H != 3 || at(r6, 1, 0) != 0 || at(r6, 0, 0) != 3 || at(r6, 0, 2) != 5 {
		t.Errorf("orientation 6: %v", r6.Pix)
	}
	r3 := orient(src, 3)
	if at(r3, 0, 0) != 5 || at(r3, 2, 1) != 0 {
		t.Error("orientation 3")
	}
	// EXIF の読み取り（Motorola の並び、Orientation 6）
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	jpg := append([]byte{0xff, 0xd8, 0xff, 0xe1, byte((len(app1) + 2) >> 8), byte(len(app1) + 2)}, app1...)
	jpg = append(jpg, 0xff, 0xda, 0, 2)
	if o := exifOrientation(jpg); o != 6 {
		t.Errorf("exif orientation %d", o)
	}
	if exifOrientation([]byte("not a jpeg")) != 1 {
		t.Error("non-jpeg")
	}
}

func TestMemoAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	doc := `{"id":"m3","created":"2026-10-11T00:00:00Z","updated":"2026-10-11T00:00:00Z","pages":[{"w":40,"h":30,"strokes":[{"w":4,"p":[5,5,35,25]}]},{"w":40,"h":30,"strokes":[]}]}`
	deleted := ""
	f.handler = func(req map[string]any) (any, bool) {
		switch req["cmd"] {
		case "hello":
			return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "commands": []string{"hello", "list_memos", "get_memo", "delete_memo"}}, true
		case "list_memos":
			return map[string]any{"memos": []any{map[string]any{"id": "m3", "pages": 2, "strokes": 1, "updated": "2026-10-11T00:00:00Z", "bytes": len(doc)}}}, true
		case "get_memo":
			off := int(req["offset"].(float64))
			end := min(off+50, len(doc))
			return map[string]any{"total": len(doc), "data": base64.StdEncoding.EncodeToString([]byte(doc[off:end]))}, true
		case "delete_memo":
			deleted = req["memo"].(string)
			return map[string]any{"deleted": deleted}, true
		}
		return nil, false
	}
	go f.serve(nil)
	if code, out, _ := runCLI(t, "--port", f.slave, "memo"); code != exitOK || !strings.Contains(out, "m3") || !strings.Contains(out, "2 ページ、1 本") {
		t.Fatalf("list: %d %s", code, out)
	}
	p := filepath.Join(t.TempDir(), "m.png")
	if code, out, errs := runCLI(t, "--port", f.slave, "memo", "get", "1", "-o", p); code != exitOK || !strings.Contains(out, "2 ページ") {
		t.Fatalf("get: %d %s %s", code, out, errs)
	}
	fh, _ := os.Open(p)
	img, err := png.Decode(fh)
	if err != nil || img.Bounds().Dx() != 40 || img.Bounds().Dy() != 30+30+8 {
		t.Fatalf("png %v %v", img.Bounds(), err)
	}
	if r, _, _, _ := img.At(20, 15).RGBA(); r>>8 != 0x10 {
		t.Errorf("the line is not drawn: %x", r>>8)
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "memo", "get", "1", "-o", p, "--page", "3"); code != exitUsage {
		t.Errorf("--page 3: %d", code)
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "memo", "rm", "m3"); code != exitOK || deleted != "m3" {
		t.Errorf("rm: %d %q", code, deleted)
	}
}
