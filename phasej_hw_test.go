package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// フェーズ J の実機のテスト（ファイルエクスプローラ：一覧と du の時間、画像を開く時間、テキストのページを数える時間）。
// 画面と入力には触れないので、本番のサービスは動かしたままでよい。一覧は /home/user を読むだけ。
// 画像とテキストはテストの一時ディレクトリに作る（本番のデータには触れない）。
//
//	LEFTHAND_HW_TEST=1 timeout 300 nice ./lefthand.test -test.run HWFiles -test.v
func TestHWFiles(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	tmp := t.TempDir()
	s := NewFilesService(tmp, nil, nil, nil)
	s.SetConfig(&FilesConfig{Roots: []FilesRoot{{Name: "ホーム", Path: "/home/user"}, {Name: "試し", Path: tmp, Delete: true}}})
	for _, du := range []bool{false, true} {
		t0 := time.Now()
		l, err := s.List("/home/user", du, du)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("list /home/user du=%v: %d entries, partial %v, in %v", du, len(l.Entries), l.Partial, time.Since(t0).Round(time.Millisecond))
	}
	area := image.Rect(0, 0, 776, 410)
	for _, sz := range [][2]int{{1600, 1200}, {2400, 1600}} {
		img := image.NewNRGBA(image.Rect(0, 0, sz[0], sz[1]))
		for y := 0; y < sz[1]; y++ {
			for x := 0; x < sz[0]; x++ {
				img.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
			}
		}
		p := filepath.Join(tmp, "g.png")
		f, _ := os.Create(p)
		png.Encode(f, img)
		f.Close()
		fi, _ := os.Stat(p)
		u := &FilesUI{svc: s, held: -1}
		e := FileEntry{Name: "g.png", Path: p, Size: fi.Size()}
		u.sel = &e
		t0 := time.Now()
		u.loadImage(e, area)
		v := u.Snapshot()
		if v.img == nil {
			t.Fatalf("%v: %s", sz, v.note)
		}
		t.Logf("open %dx%d png (%d KB): %v → %dx%d", sz[0], sz[1], fi.Size()>>10, time.Since(t0).Round(time.Millisecond), v.img.W, v.img.H)
		os.Remove(p)
	}
	txt := strings.Repeat("これはファイルエクスプローラの試しの文です。The quick brown fox jumps over the lazy dog.\n", 3000)
	p := filepath.Join(tmp, "t.txt")
	os.WriteFile(p, []byte(txt), 0o644)
	u := &FilesUI{svc: s, held: -1}
	e := FileEntry{Name: "t.txt", Path: p, Size: int64(len(txt))}
	u.sel = &e
	t0 := time.Now()
	u.loadText(e, area)
	v := u.Snapshot()
	t.Logf("open text %d KB: %d pages in %v", len(txt)>>10, len(v.starts), time.Since(t0).Round(time.Millisecond))
	if err := s.Delete(p); err != nil {
		t.Fatal(err)
	}
}
