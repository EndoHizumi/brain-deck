package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ---------- 天気アイコン（/var/lib/lefthand/weather-icons） ----------
//
// 気象庁の天気アイコン（SVG）は、PC の brain-deck が、セルに合う大きさで透明度付きの RGB565 にして送る
// （SVG を描くのも、ディザリングも PC。Brain は受け取って、描くときに重ねるだけ）。
// 大きさはセルの大きさで決まるので、ファイルは天気の番号と、置く箱の大きさごとに持つ（<番号>-<幅>x<高さ>.lha）。
// 一度受け取ったものは残し、set_weather の返事で、まだないもの（icons_missing）を PC に知らせる。
//
// ファイルの形（.lha）：
//
//	0..3   "LHA1"
//	4..5   幅（リトルエンディアンの uint16）
//	6..7   高さ（同）
//	8..    画素。左上から行の順に、1 画素 2 バイトの RGB565（リトルエンディアン）
//	..     そのあとに、1 画素 1 バイトの不透明度（0 透明 〜 255 不透明）
//
// 画像は箱より小さくてよい（縦横比を保って箱に収めたもの）。描くときは箱の中央に置く。

const (
	iconMagic     = "LHA1"
	iconHeader    = 8
	iconExt       = ".lha"
	iconMaxSide   = 480
	iconMaxPixels = 480 * 320
	iconMaxBytes  = iconHeader + iconMaxPixels*3
	iconUploadKnd = "weather_icon"
)

// 上限。テストで小さくするので変数にしてある
var (
	iconQuota      int64 = 2 << 20 // アイコンの合計の上限
	iconCacheBytes       = 1 << 20 // 読み込んだアイコンをメモリに置いておく上限
)

// iconNameRE は、アイコンの名前（<番号>-<幅>x<高さ>）。
var iconNameRE = regexp.MustCompile(`^([0-9]{3})-([0-9]{1,3})x([0-9]{1,3})$`)

// iconName は、天気の番号 code を、box の大きさの箱に置くアイコンの名前。
func iconName(code string, box image.Point) string {
	return fmt.Sprintf("%s-%dx%d", code, box.X, box.Y)
}

// parseIconName は、名前から番号と箱の大きさを読む。
func parseIconName(name string) (string, image.Point, bool) {
	m := iconNameRE.FindStringSubmatch(name)
	if m == nil {
		return "", image.Point{}, false
	}
	w, _ := strconv.Atoi(m[2])
	h, _ := strconv.Atoi(m[3])
	if w < 1 || h < 1 || w > iconMaxSide || h > iconMaxSide || w*h > iconMaxPixels {
		return "", image.Point{}, false
	}
	return m[1], image.Pt(w, h), true
}

// Icon は読み込んだアイコン。Pix は RGB565（リトルエンディアン）、A は不透明度。
type Icon struct {
	W, H int
	Pix  []byte
	A    []byte
}

func (ic *Icon) bytes() int { return iconHeader + len(ic.Pix) + len(ic.A) }

// parseIcon はファイルの中身を読む。
func parseIcon(b []byte) (*Icon, error) {
	if len(b) < iconHeader || string(b[:4]) != iconMagic {
		return nil, errors.New("not a lefthand icon (LHA1)")
	}
	w, h := int(binary.LittleEndian.Uint16(b[4:])), int(binary.LittleEndian.Uint16(b[6:]))
	if w < 1 || h < 1 || w > iconMaxSide || h > iconMaxSide || w*h > iconMaxPixels {
		return nil, fmt.Errorf("icon size %dx%d is out of range (each side 1..%d, at most %d pixels)", w, h, iconMaxSide, iconMaxPixels)
	}
	if len(b) != iconHeader+w*h*3 {
		return nil, fmt.Errorf("%dx%d needs %d bytes, got %d", w, h, iconHeader+w*h*3, len(b))
	}
	return &Icon{W: w, H: h, Pix: b[iconHeader : iconHeader+w*h*2], A: b[iconHeader+w*h*2:]}, nil
}

// encodeIcon はファイルの中身を作る（テスト用）。
func encodeIcon(w, h int, pix, alpha []byte) []byte {
	b := make([]byte, iconHeader, iconHeader+len(pix)+len(alpha))
	copy(b, iconMagic)
	binary.LittleEndian.PutUint16(b[4:], uint16(w))
	binary.LittleEndian.PutUint16(b[6:], uint16(h))
	b = append(b, pix...)
	return append(b, alpha...)
}

// iconMeta は、置いてあるアイコン 1 つ。
type iconMeta struct {
	Name  string    `json:"name"`
	W     int       `json:"w"`
	H     int       `json:"h"`
	Bytes int64     `json:"bytes"`
	Added time.Time `json:"added"`
}

// IconStore は天気アイコンの置き場所と、読み込んだアイコンのキャッシュ。
// 描画の goroutine（Icon）と通信の goroutine（受け取り、一覧）の両方から使う。
type IconStore struct {
	dir string

	mu       sync.Mutex
	files    map[string]iconMeta
	cache    map[string]*Icon
	cached   int
	gen      uint64 // アイコンが増えた・減ったたびに増やす（描き直しの判断に使う）
	wanted   func() map[image.Point]bool
	onChange func()
}

// NewIconStore は dir を読む（なければ空）。ディレクトリは作らない（-render-png 用）。
func NewIconStore(dir string) *IconStore {
	s := &IconStore{dir: dir, files: map[string]iconMeta{}, cache: map[string]*Icon{}}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		n := e.Name()
		if filepath.Ext(n) != iconExt {
			continue
		}
		name := n[:len(n)-len(iconExt)]
		if _, _, ok := parseIconName(name); !ok {
			continue
		}
		if info, err := e.Info(); err == nil {
			w, h := iconFileSize(filepath.Join(dir, n))
			s.files[name] = iconMeta{Name: name, W: w, H: h, Bytes: info.Size(), Added: info.ModTime().UTC()}
		}
	}
	return s
}

// OpenIconStore は dir を（なければ作って）使い、前に受け取りの途中で止まったファイルを消す。
func OpenIconStore(dir string) *IconStore {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("weather icons: %v (icons cannot be saved)", err)
	}
	for _, p := range removeParts(dir) {
		log.Printf("weather icons: removed an incomplete upload %s", filepath.Base(p))
	}
	return NewIconStore(dir)
}

// iconFileSize は、ファイルの先頭から幅と高さを読む。読めなければ 0。
func iconFileSize(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var b [iconHeader]byte
	if _, err := f.Read(b[:]); err != nil || string(b[:4]) != iconMagic {
		return 0, 0
	}
	return int(binary.LittleEndian.Uint16(b[4:])), int(binary.LittleEndian.Uint16(b[6:]))
}

// SetOnChange は、アイコンが増えた・減ったときに呼ぶ関数（画面の描き直し）を設定する。
func (s *IconStore) SetOnChange(f func()) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

// SetWanted は、今の設定で使う箱の大きさを返す関数を設定する（上限を超えたとき、使わない大きさから消す）。
func (s *IconStore) SetWanted(f func() map[image.Point]bool) {
	s.mu.Lock()
	s.wanted = f
	s.mu.Unlock()
}

// Gen は、アイコンの増減の回数。
func (s *IconStore) Gen() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// Has は、名前のアイコンがあるか。
func (s *IconStore) Has(name string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.files[name]
	return ok
}

// List は、置いてあるアイコン（名前の順）と合計の大きさ。
func (s *IconStore) List() ([]iconMeta, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]iconMeta, 0, len(s.files))
	var total int64
	for _, m := range s.files {
		out = append(out, m)
		total += m.Bytes
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, total
}

// Icon は名前のアイコンを返す。ない、読めないときは nil。描画の goroutine から呼ぶ。
func (s *IconStore) Icon(name string) *Icon {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if ic, ok := s.cache[name]; ok {
		s.mu.Unlock()
		return ic
	}
	_, ok := s.files[name]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.dir, name+iconExt))
	if err != nil {
		log.Printf("weather icons: %v", err)
		return nil
	}
	ic, err := parseIcon(b)
	if err != nil {
		log.Printf("weather icons: %s: %v", name, err)
		return nil
	}
	s.mu.Lock()
	if s.cached+ic.bytes() > iconCacheBytes {
		s.cache, s.cached = map[string]*Icon{}, 0 // 小さいので、溢れたら全部捨てて読み直す
	}
	s.cache[name] = ic
	s.cached += ic.bytes()
	s.mu.Unlock()
	return ic
}

// Read は、ファイルの中身を返す（get_weather_icon。設定 GUI のプレビュー用）。
func (s *IconStore) Read(name string) ([]byte, error) {
	s.mu.Lock()
	_, ok := s.files[name]
	s.mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(s.dir, name+iconExt))
}

// UploadKind は、upload_*（kind: weather_icon）で受け取る種類。name は <番号>-<幅>x<高さ>。
func (s *IconStore) UploadKind() UploadKind {
	return UploadKind{
		Dir:      s.dir,
		MaxBytes: iconMaxBytes,
		Check: func(b UploadBegin) error {
			if _, _, ok := parseIconName(b.Name); !ok {
				return fmt.Errorf(`%w: "name" must be <icon>-<w>x<h> (such as 100-87x58), got %q`, errTransferBad, b.Name)
			}
			if err := os.MkdirAll(s.dir, 0o755); err != nil {
				return err
			}
			// 空きの確かめ（背景画像と同じ。reserveFor）
			free, size, err := imageFreeSpace(s.dir)
			if err == nil {
				reserve := reserveFor(size)
				if free-b.Bytes < reserve {
					return fmt.Errorf("%w: %d bytes free on %s; keeping %d bytes free", errImageNoSpace, free, s.dir, reserve)
				}
			}
			return nil
		},
		Done: func(b UploadBegin, f *os.File, path string) (any, error) {
			return s.add(b.Name, f, path)
		},
	}
}

// add は、受け取って確かめたファイルを、名前の場所に置く。上限を超えるなら、今の設定で使わない大きさから消す。
func (s *IconStore) add(name string, f *os.File, path string) (any, error) {
	defer os.Remove(path)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	b := make([]byte, st.Size())
	_, err = f.ReadAt(b, 0)
	f.Close()
	if err != nil {
		return nil, err
	}
	ic, err := parseIcon(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errTransferBad, err)
	}
	_, box, _ := parseIconName(name)
	if ic.W > box.X || ic.H > box.Y {
		return nil, fmt.Errorf("%w: the icon is %dx%d, larger than its box %dx%d", errTransferBad, ic.W, ic.H, box.X, box.Y)
	}
	s.mu.Lock()
	wanted := s.wanted
	s.mu.Unlock()
	var want map[image.Point]bool
	if wanted != nil {
		want = wanted()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for n, m := range s.files {
		if n != name {
			total += m.Bytes
		}
	}
	var removed []string
	if total+int64(len(b)) > iconQuota {
		// 使わない大きさのものを、古い順に消す
		var old []iconMeta
		for n, m := range s.files {
			if _, bx, ok := parseIconName(n); ok && !want[bx] && n != name {
				old = append(old, m)
			}
		}
		sort.Slice(old, func(i, j int) bool { return old[i].Added.Before(old[j].Added) })
		for _, m := range old {
			if total+int64(len(b)) <= iconQuota {
				break
			}
			if err := os.Remove(filepath.Join(s.dir, m.Name+iconExt)); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
			delete(s.files, m.Name)
			if c, ok := s.cache[m.Name]; ok {
				s.cached -= c.bytes()
				delete(s.cache, m.Name)
			}
			total -= m.Bytes
			removed = append(removed, m.Name)
		}
		if total+int64(len(b)) > iconQuota {
			s.gen++
			return nil, fmt.Errorf("%w: weather icons of the current cells use %d bytes (limit %d)", errImageQuota, total, iconQuota)
		}
	}
	dst := filepath.Join(s.dir, name+iconExt)
	if err := os.Rename(path, dst); err != nil {
		return nil, err
	}
	syncDir(s.dir)
	s.files[name] = iconMeta{Name: name, W: ic.W, H: ic.H, Bytes: int64(len(b)), Added: time.Now().UTC()}
	if c, ok := s.cache[name]; ok {
		s.cached -= c.bytes()
		delete(s.cache, name)
	}
	s.gen++
	cb := s.onChange
	if cb != nil {
		go cb()
	}
	vlogf("weather icons: saved %s (%dx%d, %d bytes)", name, ic.W, ic.H, len(b))
	res := map[string]any{"name": name, "w": ic.W, "h": ic.H, "bytes": len(b)}
	if len(removed) > 0 {
		res["removed"] = removed
	}
	return res, nil
}

// blendIcon は、アイコンを dst（箱）の中央に、不透明度で重ねる。clip の外は描かない。
// 色は 8 ビットに戻して混ぜ、画面の形にし直す（gui/src/weatherwidget.ts の blendIcon と同じ）。
func (cv *Canvas) blendIcon(ic *Icon, box, clip image.Rectangle) {
	x0 := box.Min.X + (box.Dx()-ic.W)/2
	y0 := box.Min.Y + (box.Dy()-ic.H)/2
	dst := image.Rect(x0, y0, x0+ic.W, y0+ic.H)
	r := dst.Intersect(clip).Intersect(image.Rect(0, 0, cv.W, cv.H))
	if cv.rot == 0 && cv.pf == rgb565 {
		cv.blendIcon565(ic, x0, y0, r)
		return
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			i := (y-y0)*ic.W + (x - x0)
			a := int(ic.A[i])
			if a == 0 {
				continue
			}
			u := rgb565.unpack(uint32(ic.Pix[i*2]) | uint32(ic.Pix[i*2+1])<<8)
			s := RGB{u.R, u.G, u.B}
			if a < 255 {
				d := cv.at(x, y)
				s = RGB{blend8(s.R, d.R, a), blend8(s.G, d.G, a), blend8(s.B, d.B, a)}
			}
			cv.set(x, y, s)
		}
	}
}

// expand5、expand6 は、5 ビット、6 ビットの値を 8 ビットに戻す表（fb.go の unpack と同じ x*255/最大）。
var expand5, expand6 = func() (a [32]uint8, b [64]uint8) {
	for i := range a {
		a[i] = uint8(i * 255 / 31)
	}
	for i := range b {
		b[i] = uint8(i * 255 / 63)
	}
	return
}()

// blendIcon565 は blendIcon の、回転なしの RGB565 の画面の場合。画面のバイト列を直接混ぜる（結果は blendIcon と同じ）。
func (cv *Canvas) blendIcon565(ic *Icon, x0, y0 int, r image.Rectangle) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		row := cv.pix[y*cv.stride:]
		i := (y-y0)*ic.W + (r.Min.X - x0)
		for x := r.Min.X; x < r.Max.X; x, i = x+1, i+1 {
			a := int(ic.A[i])
			if a == 0 {
				continue
			}
			o := x * 2
			if a == 255 {
				row[o], row[o+1] = ic.Pix[i*2], ic.Pix[i*2+1]
				continue
			}
			sv := int(ic.Pix[i*2]) | int(ic.Pix[i*2+1])<<8
			dv := int(row[o]) | int(row[o+1])<<8
			rr := blend8(expand5[sv>>11], expand5[dv>>11], a)
			gg := blend8(expand6[sv>>5&63], expand6[dv>>5&63], a)
			bb := blend8(expand5[sv&31], expand5[dv&31], a)
			v := int(rr>>3)<<11 | int(gg>>2)<<5 | int(bb>>3)
			row[o], row[o+1] = byte(v), byte(v>>8)
		}
	}
}

// blend8 は、不透明度 a（0〜255）で s を d の上に重ねた値（四捨五入）。
func blend8(s, d uint8, a int) uint8 {
	return uint8((int(s)*a + int(d)*(255-a) + 127) / 255)
}
