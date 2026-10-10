package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilesConfigValidate(t *testing.T) {
	for _, c := range []struct {
		root FilesRoot
		bad  string
	}{
		{FilesRoot{Name: "a", Path: "/home/user", Delete: true}, ""},
		{FilesRoot{Name: "a", Path: "/var/log"}, ""},
		{FilesRoot{Name: "a", Path: "/var/lib/lefthand/photos", Delete: true}, ""},
		{FilesRoot{Name: "a", Path: "/etc", Delete: true}, "cannot be deleted"},
		{FilesRoot{Name: "a", Path: "/boot"}, "cannot be shown"},
		{FilesRoot{Name: "a", Path: "/boot", Delete: true}, "cannot be"},
		{FilesRoot{Name: "a", Path: "/", Delete: true}, "cannot be"},
		{FilesRoot{Name: "a", Path: "/proc/1"}, "cannot be shown"},
		{FilesRoot{Name: "a", Path: "/home/"}, "clean absolute"},
		{FilesRoot{Name: "a", Path: "/home", Delete: true}, "cannot be deleted"},
		{FilesRoot{Name: "a", Path: "/var/lib/lefthand", Delete: true}, "cannot be deleted"},
		{FilesRoot{Name: "", Path: "/home/user"}, "name"},
	} {
		var got []string
		(&FilesConfig{Roots: []FilesRoot{c.root}}).validate("/var/lib/lefthand", func(_, f string, a ...any) {
			got = append(got, fmt.Sprintf(f, a...))
		})
		msg := strings.Join(got, "; ")
		if (c.bad == "") != (msg == "") || !strings.Contains(msg, c.bad) {
			t.Errorf("%+v: %q, want %q", c.root, msg, c.bad)
		}
	}
}

func filesTestService(t *testing.T) (*FilesService, string, string) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	ro := filepath.Join(base, "ro")
	data := filepath.Join(base, "data")
	for _, d := range []string{home + "/sub/deep", home + "/.ssh", ro, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(home+"/a.txt", []byte("hello"), 0o644)
	os.WriteFile(home+"/sub/deep/big.bin", make([]byte, 5000), 0o644)
	os.WriteFile(home+"/.ssh/id", []byte("secret"), 0o600)
	os.WriteFile(ro+"/keep.txt", []byte("keep"), 0o644)
	os.Symlink(ro, home+"/link")
	s := NewFilesService(data, OpenPhotoStore(filepath.Join(data, "photos")), nil, nil)
	// t.TempDir は /tmp の下なので、delete: true が書ける
	s.SetConfig(&FilesConfig{Roots: []FilesRoot{{Name: "ホーム", Path: home, Delete: true}, {Name: "読むだけ", Path: ro}}})
	return s, home, ro
}

func TestFilesListReadDelete(t *testing.T) {
	s, home, ro := filesTestService(t)
	l, err := s.List("", false, false)
	if err != nil || len(l.Entries) != 2 || l.Entries[0].Name != "ホーム" {
		t.Fatalf("roots %+v %v", l, err)
	}
	l, err = s.List(home, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	// フォルダが先、.ssh は出さない、リンクは出す（たどらない）
	if strings.Join(names, ",") != "sub,a.txt,link" || !l.Entries[2].Link || !l.Delete || !l.Put || l.Parent != "" {
		t.Errorf("list %v %+v", names, l)
	}
	l, _ = s.List(home, true, true)
	if l.Entries[0].Name != "sub" || l.Entries[0].Size != 5000 {
		t.Errorf("by size %+v", l.Entries)
	}
	if l, _ := s.List(home+"/sub", false, false); l.Parent != home {
		t.Errorf("parent %q", l.Parent)
	}
	b, total, err := s.Read(home+"/a.txt", 1, 3)
	if err != nil || string(b) != "ell" || total != 5 {
		t.Errorf("read %q %d %v", b, total, err)
	}
	for _, p := range []string{home + "/.ssh/id", home + "/link/keep.txt", home + "/../ro/keep.txt", "/etc/passwd", "rel"} {
		if _, _, err := s.Read(p, 0, 10); !errors.Is(err, errFilesOutside) {
			t.Errorf("read %s: %v", p, err)
		}
	}
	if _, _, err := s.Read(home+"/link", 0, 10); !errors.Is(err, errFilesDenied) {
		t.Errorf("read link itself: %v", err)
	}
	if err := s.Delete(ro + "/keep.txt"); !errors.Is(err, errFilesDenied) {
		t.Errorf("delete in read-only place: %v", err)
	}
	if err := s.Delete(home); !errors.Is(err, errFilesDenied) {
		t.Errorf("delete the place: %v", err)
	}
	if err := s.Delete(home + "/sub"); err == nil {
		t.Errorf("deleted a folder that is not empty")
	}
	// リンクを消しても、リンク先は残る
	if err := s.Delete(home + "/link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ro + "/keep.txt"); err != nil {
		t.Errorf("link target: %v", err)
	}
	if err := s.Delete(home + "/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home + "/a.txt"); !os.IsNotExist(err) {
		t.Errorf("a.txt is still there")
	}
}

func TestFilesDeleteData(t *testing.T) {
	old := imageFreeSpace
	imageFreeSpace = func(string) (int64, int64, error) { return 1 << 30, 3 << 30, nil }
	defer func() { imageFreeSpace = old }()
	data := filepath.Join(t.TempDir(), "data")
	photos := OpenPhotoStore(filepath.Join(data, "photos"))
	u := NewUploads()
	u.Register("photo", photos.UploadKind())
	id, err := uploadPhoto(t, u, testPhoto(800, 480, 1), "海", "")
	if err != nil {
		t.Fatal(err)
	}
	s := NewFilesService(data, photos, nil, nil) // 既定の場所
	if err := s.Delete(filepath.Join(data, "photos", photoIndexFile)); !errors.Is(err, errFilesDenied) {
		t.Errorf("deleted the index: %v", err)
	}
	if err := s.Delete(filepath.Join(data, "photos", id+photoExt)); err != nil {
		t.Fatal(err)
	}
	if photos.Count() != 0 {
		t.Errorf("photo still listed")
	}
}

func TestFilesPut(t *testing.T) {
	old := imageFreeSpace
	imageFreeSpace = func(string) (int64, int64, error) { return 1 << 30, 3 << 30, nil }
	defer func() { imageFreeSpace = old }()
	s, home, ro := filesTestService(t)
	u := NewUploads()
	u.Register("file", s.UploadKind(filepath.Join(t.TempDir(), "tmp")))
	put := func(dir, name string, b []byte) error {
		h := sha256.Sum256(b)
		st, err := u.Begin(UploadBegin{Kind: "file", SHA256: hex.EncodeToString(h[:]), Bytes: int64(len(b)), Name: name, Path: dir, Source: "test"})
		if err != nil {
			return err
		}
		if _, err := u.Chunk(st.Upload, 0, b); err != nil {
			return err
		}
		_, err = u.End(st.Upload)
		return err
	}
	if err := put(home+"/sub", "n.txt", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(home + "/sub/n.txt"); string(b) != "new" {
		t.Errorf("put %q", b)
	}
	for _, c := range [][2]string{{home + "/sub", "n.txt"}, {ro, "x.txt"}, {home, "../x"}, {home + "/.ssh", "k"}, {home + "/nofolder", "x"}} {
		if err := put(c[0], c[1], []byte("x")); err == nil {
			t.Errorf("put %v: no error", c)
		}
	}
}

func TestFilesUI(t *testing.T) {
	s, home, _ := filesTestService(t)
	os.WriteFile(home+"/b.txt", []byte("一行目\n二行目\n"), 0o644)
	u := NewFilesUI(s)
	cell := image.Rect(0, 24, 800, 480)
	area := cell.Inset(cellGap + textMargin)
	g := filesGeometry(area)
	tap := func(p image.Point) { u.touchDown(cell, p); u.touchUp(false) }
	wait := func(f func(v *FilesView) bool) *FilesView {
		for i := 0; i < 200; i++ {
			if v := u.Snapshot(); f(v) {
				return v
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timeout: %+v", u.Snapshot())
		return nil
	}
	mid := func(r image.Rectangle) image.Point { return r.Min.Add(r.Size().Div(2)) }
	wait(func(v *FilesView) bool { return len(v.list.Entries) == 2 })
	tap(mid(g.rows[0])) // ホーム
	v := wait(func(v *FilesView) bool { return v.list.Path == home })
	k := -1
	for i, e := range v.list.Entries {
		if e.Name == "b.txt" {
			k = i
		}
	}
	tap(mid(g.rows[k]))
	v = wait(func(v *FilesView) bool { return v.doc != nil && len(v.starts) > 0 })
	if v.mode != filesText || len(v.doc.Blocks) < 2 {
		t.Errorf("text view %+v", v)
	}
	tap(mid(area)) // 道具
	if v := u.Snapshot(); v.mode != filesMenu || v.menuItems()[0] != "読み物に足す" {
		t.Errorf("menu %+v", v.menuItems())
	}
	tap(mid(g.menu[1])) // 一覧に戻る
	if v := u.Snapshot(); v.mode != filesList {
		t.Errorf("back: mode %d", v.mode)
	}
	// 長押しで、消すかを聞く
	u.touchDown(cell, mid(g.rows[k]))
	time.Sleep(filesHold + 100*time.Millisecond)
	u.touchUp(false)
	if v := u.Snapshot(); v.mode != filesConfirm || v.sel == nil || v.sel.Name != "b.txt" {
		t.Fatalf("confirm %+v", v)
	}
	tap(mid(g.no))
	if _, err := os.Stat(home + "/b.txt"); err != nil {
		t.Fatal("deleted after やめる")
	}
	u.touchDown(cell, mid(g.rows[k]))
	time.Sleep(filesHold + 100*time.Millisecond)
	u.touchUp(false)
	tap(mid(g.yes))
	wait(func(v *FilesView) bool {
		_, err := os.Stat(home + "/b.txt")
		return os.IsNotExist(err) && len(v.list.Entries) == 3
	})
	// 描いても落ちない
	cv := NewCanvas(800, 480, 1600, rgb565, 0)
	for _, m := range []int{filesList, filesConfirm} {
		u.mu.Lock()
		u.mode = m
		u.sel = &FileEntry{Name: "x"}
		u.mu.Unlock()
		drawFiles(cv, area, WidgetEnv{Files: u.Snapshot()})
	}
}

func TestFilesAddTo(t *testing.T) {
	old := imageFreeSpace
	imageFreeSpace = func(string) (int64, int64, error) { return 1 << 30, 3 << 30, nil }
	defer func() { imageFreeSpace = old }()
	s, home, _ := filesTestService(t)
	data := t.TempDir()
	u := NewFilesUI(s)
	u.photos = OpenPhotoStore(filepath.Join(data, "photos"))
	u.reads = OpenReadStore(filepath.Join(data, "reader"))
	added := 0
	u.added = func() { added++ }
	// PNG の写真
	img := image.NewNRGBA(image.Rect(0, 0, 300, 200))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	f, _ := os.Create(home + "/p.png")
	png.Encode(f, img)
	f.Close()
	e := &FileEntry{Name: "p.png", Path: home + "/p.png"}
	if _, err := u.addTo("写真に足す", e, nil); err != nil {
		t.Fatal(err)
	}
	if l := u.photos.List(); len(l.Photos) != 1 || l.Photos[0].Name != "p" {
		t.Errorf("photos %+v", l)
	}
	doc := textDoc("t.txt", []byte("あ\nい"))
	if _, err := u.addTo("読み物に足す", &FileEntry{Name: "t.txt"}, doc); err != nil {
		t.Fatal(err)
	}
	if l := u.reads.List(); len(l) != 1 || added != 1 {
		t.Errorf("reads %+v added %d", l, added)
	}
}
