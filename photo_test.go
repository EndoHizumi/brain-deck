package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lefthand/internal/imgconv"
)

func TestPhotoConfig(t *testing.T) {
	p, err := (*PhotoConfig)(nil).compile()
	if err != nil || p.interval != time.Minute || p.random || !p.night || p.nightFrom != 23*60 || p.nightTo != 7*60 || p.level != 4 || p.max != 10 {
		t.Fatalf("defaults %+v %v", p, err)
	}
	five := 5
	p, err = (&PhotoConfig{Interval: "10s", Order: "random", Night: "off", Level: &five, Max: &five}).compile()
	if err != nil || p.interval != 10*time.Second || !p.random || p.night || p.level != 5 || p.max != 5 {
		t.Fatalf("set %+v %v", p, err)
	}
	for _, c := range []PhotoConfig{{Interval: "1s"}, {Order: "shuffle"}, {Night: "23-7"}, {Night: "25:00-07:00"}} {
		if _, err := c.compile(); err == nil {
			t.Errorf("%+v: no error", c)
		}
	}
	// 夜間の時間帯：日をまたぐ、またがない
	p, _ = (&PhotoConfig{}).compile()
	at := func(h, m int) time.Time { return time.Date(2026, 10, 11, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		h, m  int
		night bool
	}{{22, 59, false}, {23, 0, true}, {3, 0, true}, {6, 59, true}, {7, 0, false}, {12, 0, false}} {
		if got := p.atNight(at(c.h, c.m)); got != c.night {
			t.Errorf("%02d:%02d night=%v", c.h, c.m, got)
		}
	}
	p, _ = (&PhotoConfig{Night: "01:00-05:30"}).compile()
	if !p.atNight(at(5, 29)) || p.atNight(at(5, 30)) || p.atNight(at(0, 59)) {
		t.Error("01:00-05:30")
	}
	// backlight.photo は dim_after が要る
	if _, err := (&BacklightConfig{OffAfter: "1m", Photo: ptr(true)}).compile(); err == nil || !strings.Contains(err.Error(), "photo needs dim_after") {
		t.Errorf("photo without dim_after: %v", err)
	}
	bp, err := (&BacklightConfig{DimAfter: "1m", Photo: ptr(true), Offline: &BacklightOffline{Photo: ptr(false)}}).compile()
	if err != nil || !bp.photo || bp.offline.photo {
		t.Errorf("offline.photo: %+v %v", bp, err)
	}
}

// testPhoto は、模様の写真（自分で作ったもの）を w×h の LHI1 にする。
func testPhoto(w, h, seed int) []byte {
	src := imgconv.RGBA{W: 64, H: 48, Pix: make([]byte, 64*48*4)}
	for i := 0; i < 64*48; i++ {
		src.Pix[i*4], src.Pix[i*4+1], src.Pix[i*4+2], src.Pix[i*4+3] = byte(i*seed), byte(i/64*5), byte(seed*40), 255
	}
	pix, _ := imgconv.Convert(src, w, h, imgconv.Params{CX: -1, CY: -1, Dither: true})
	return imgconv.Encode(w, h, pix)
}

// uploadPhoto は、upload_* と同じ手順で写真を受け取らせる。
func uploadPhoto(t *testing.T, u *Uploads, b []byte, name, of string) (string, error) {
	t.Helper()
	s := sha256.Sum256(b)
	st, err := u.Begin(UploadBegin{Kind: "photo", SHA256: hex.EncodeToString(s[:]), Bytes: int64(len(b)), Name: name, Source: "test", Of: of})
	if err != nil {
		return "", err
	}
	for off := 0; off < len(b); off += st.ChunkBytes {
		if _, err := u.Chunk(st.Upload, int64(off), b[off:min(off+st.ChunkBytes, len(b))]); err != nil {
			return "", err
		}
	}
	res, err := u.End(st.Upload)
	if err != nil {
		return "", err
	}
	return res.(map[string]any)["id"].(string), nil
}

func TestPhotoStore(t *testing.T) {
	old := imageFreeSpace
	imageFreeSpace = func(string) (int64, int64, error) { return 1 << 30, 3 << 30, nil }
	defer func() { imageFreeSpace = old }()
	dir := filepath.Join(t.TempDir(), "photos")
	s := OpenPhotoStore(dir)
	s.SetMax(3)
	u := NewUploads()
	u.Register("photo", s.UploadKind())

	a, err := uploadPhoto(t, u, testPhoto(800, 480, 1), "海", "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	b, err := uploadPhoto(t, u, testPhoto(800, 480, 2), "山", "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	// a を縦向きに切り抜き直したもの
	ap, err := uploadPhoto(t, u, testPhoto(480, 800, 3), "海（縦）", a)
	if err != nil {
		t.Fatal(err)
	}
	// 上限（3 枚）
	if _, err := uploadPhoto(t, u, testPhoto(480, 800, 4), "x", ""); err == nil || !strings.Contains(err.Error(), "photo.max") {
		t.Errorf("over max: %v", err)
	}
	// 大きさが画面と違う、of を横向きに付ける
	s.SetMax(10)
	if _, err := uploadPhoto(t, u, testPhoto(400, 240, 5), "x", ""); err == nil || !strings.Contains(err.Error(), "800x480") {
		t.Errorf("bad size: %v", err)
	}
	if _, err := uploadPhoto(t, u, testPhoto(800, 480, 6), "x", a); err == nil || !strings.Contains(err.Error(), "portrait") {
		t.Errorf("of on landscape: %v", err)
	}
	if got := s.slides(false); strings.Join(got, ",") != a+","+b {
		t.Errorf("landscape slides %v", got)
	}
	if got := s.slides(true); strings.Join(got, ",") != ap+","+b {
		t.Errorf("portrait slides %v (want the crop for a, b letterboxed)", got)
	}
	l := s.List()
	if len(l.Photos) != 3 || l.Photos[0].Name != "海" || l.Photos[2].Of != a || l.Max != 10 {
		t.Errorf("list %+v", l)
	}
	// 開き直しても同じ（index.json）
	if s2 := OpenPhotoStore(dir); s2.Count() != 3 || strings.Join(s2.slides(true), ",") != ap+","+b {
		t.Error("reopen")
	}
	// 読む
	data, total, err := s.Read(a, 0, 100)
	if err != nil || total != 768008 || string(data[:4]) != "LHI1" {
		t.Errorf("read %v %v", total, err)
	}
	// もとの写真を消すと、縦向きの写真は縦向きだけの写真として出す
	if err := s.Delete(a); err != nil {
		t.Fatal(err)
	}
	if got := s.slides(false); strings.Join(got, ",") != b+","+ap {
		t.Errorf("after delete %v", got)
	}
	if err := s.Delete(a); err == nil {
		t.Error("delete twice")
	}
	if _, err := os.Stat(filepath.Join(dir, a+".565")); !os.IsNotExist(err) {
		t.Error("file remains")
	}
	// SD カードの空き
	imageFreeSpace = func(string) (int64, int64, error) { return 64<<20 + 500<<10, 3 << 30, nil }
	if _, err := uploadPhoto(t, u, testPhoto(800, 480, 7), "x", ""); err == nil || !strings.Contains(err.Error(), "kept for the system") {
		t.Errorf("no space: %v", err)
	}
}

func TestPhotoForScreen(t *testing.T) {
	b := testPhoto(800, 480, 1)
	img, _ := parseImage(b)
	if photoForScreen(img, 800, 480) != img {
		t.Error("same size is not copied")
	}
	p := photoForScreen(img, 480, 800)
	_, _, want, _ := imgconv.Decode(b)
	fit := imgconv.Fit565(want, 800, 480, 480, 800)
	if p.W != 480 || p.H != 800 || string(p.Pix) != string(imgconv.Encode(480, 800, fit)[8:]) {
		t.Error("letterbox differs from imgconv.Fit565")
	}
	// 上下の帯は黒
	if p.Pix[0] != 0 || p.Pix[len(p.Pix)-1] != 0 {
		t.Error("bands are not black")
	}
}

type fakeShower struct {
	mu    sync.Mutex
	shown []*Image
}

func (f *fakeShower) SetPhoto(img *Image) {
	f.mu.Lock()
	f.shown = append(f.shown, img)
	f.mu.Unlock()
}

func (f *fakeShower) last() (*Image, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.shown) == 0 {
		return nil, 0
	}
	return f.shown[len(f.shown)-1], len(f.shown)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDimmerPhoto(t *testing.T) {
	old := imageFreeSpace
	imageFreeSpace = func(string) (int64, int64, error) { return 1 << 30, 3 << 30, nil }
	defer func() { imageFreeSpace = old }()
	fb := newFakeBacklight(t, 7)
	store := OpenPhotoStore(filepath.Join(t.TempDir(), "photos"))
	u := NewUploads()
	u.Register("photo", store.UploadKind())
	for i := 1; i <= 2; i++ {
		if _, err := uploadPhoto(t, u, testPhoto(800, 480, i), "p", ""); err != nil {
			t.Fatal(err)
		}
	}
	pp, _ := (&PhotoConfig{Night: "off"}).compile()
	pp.interval = 30 * time.Millisecond
	frame := NewPhotoFrame(store, pp)
	sh := &fakeShower{}
	frame.SetDisplay(sh)
	allowed := true
	var amu sync.Mutex
	d := NewDimmer(nil)
	defer d.Close()
	d.SetPhotoFrame(frame, func() bool { amu.Lock(); defer amu.Unlock(); return allowed })
	d.Apply(&dimParams{level: -1, dimLevel: 1, wakeOnKey: true, dimAfter: 40 * time.Millisecond, offAfter: 400 * time.Millisecond, photo: true})

	// 暗くする時間で、写真を明るさ 4 で出す。順に替わる
	fb.waitLevel(4)
	if d.Info().State != "photo" {
		t.Fatalf("state %q", d.Info().State)
	}
	waitFor(t, "two photos", func() bool { _, n := sh.last(); return n >= 2 })
	if frame.Showing() == "" {
		t.Error("showing is empty")
	}
	// タッチ：写真をやめて明るくするだけ（タッチは捨てる）
	if !d.TouchDown() {
		t.Error("the touch on a photo is not swallowed")
	}
	waitFor(t, "photo off", func() bool { img, _ := sh.last(); return img == nil })
	fb.waitLevel(7)
	d.TouchUp()
	// 写真のあとは消える
	fb.waitLevel(4)
	fb.waitLevel(0)
	waitFor(t, "photo off after off_after", func() bool { img, _ := sh.last(); return img == nil })
	if d.Info().State != "off" {
		t.Errorf("state %q", d.Info().State)
	}
	// 出してはいけないとき（時刻が合っていない、タイマー）は、暗くするだけ
	amu.Lock()
	allowed = false
	amu.Unlock()
	d.Key()
	fb.waitLevel(7)
	fb.waitLevel(1)
	if d.Info().State != "dim" {
		t.Errorf("state %q, want dim", d.Info().State)
	}
	// 夜間は、写真を出さずに消す
	amu.Lock()
	allowed = true
	amu.Unlock()
	np, _ := (&PhotoConfig{Night: "00:00-23:59"}).compile()
	np.interval = 30 * time.Millisecond
	frame.SetParams(np)
	d.Key()
	fb.waitLevel(7)
	fb.waitLevel(0)
	if d.Info().State != "off" {
		t.Errorf("night: state %q", d.Info().State)
	}
	// 写真の段階のあいだに夜間になったら消す
	frame.SetParams(pp)
	d.Key()
	fb.waitLevel(4)
	frame.SetParams(np)
	fb.waitLevel(0)
	waitFor(t, "photo off at night", func() bool { img, _ := sh.last(); return img == nil })
}
