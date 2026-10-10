package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"lefthand/internal/imgconv"
)

// ---------- フォトフレーム（設定の photo と backlight.photo） ----------
//
// 自動減光の「暗くする」段階を、写真を順に出すことに置き換える（backlight.photo: true。外しているあいだは
// backlight.offline.photo）。写真は /var/lib/lefthand/photos/ に、背景画像と同じ形（LHI1、RGB565）で置く。
// 変換（切り抜き、縮小、RGB565、ディザリング）は PC（設定 GUI か brain-deck photo add）で行い、Brain は写すだけ。
//
// - 写真の大きさは、画面と同じ 800×480（横向き）か 480×800（縦向き）。縦向きの写真は、横向きの写真を
//   縦向きに切り抜き直したもの（of にもとの写真の id）でも、縦向きだけの写真でもよい。
// - 縦向きの画面で、縦向きの写真がない横向きの写真は、縮めて上下に黒い帯を入れて出す（imgconv.Fit565）。
//   横向きの画面での縦向きだけの写真も、同じく左右に帯を入れる。縦向きに切り抜き直した写真は、横向きの画面では出さない
//   （もとの写真を出す）。
// - 写真を出さないとき（今までどおり暗くするだけ）：時刻が合っていない、プレゼン用タイマーかポモドーロが動いている、
//   端末モード、写真がない。夜間（photo.night。既定 23:00〜7:00）は、写真を出さずに画面を消す。
// - 写真を出しているときにタッチしたら、写真をやめて元の画面に戻すだけで、セルは押さない（暗いときと同じ）。
// - 枚数の上限（photo.max、既定 10 枚）は背景画像とは別に数える。1 つのファイルを 1 枚と数える
//   （縦向きに切り抜き直したものも 1 枚）。SD カードの空きが 64 MiB を下回るときは保存を断る（背景画像と同じ）。

const (
	photoExt         = ".565"
	photoIndexFile   = "index.json"
	defaultPhotoMax  = 10
	maxPhotoMax      = 100
	defaultPhotoLvl  = 4
	defaultPhotoInt  = time.Minute
	defaultPhotoNite = "23:00-07:00"
	photoMaxBytes    = imageHeader + 800*480*2
)

// PhotoConfig は設定の photo。
type PhotoConfig struct {
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"` // 1 枚を出す時間（既定 1m。5s〜24h）
	Order    string `yaml:"order,omitempty" json:"order,omitempty"`       // sequential（既定。足した順）か random
	Night    string `yaml:"night,omitempty" json:"night,omitempty"`       // 写真を出さずに消す時間帯（既定 23:00-07:00）。off で時間帯なし
	Level    *int   `yaml:"level,omitempty" json:"level,omitempty"`       // 写真を出すときの明るさ（既定 4。0〜max_brightness）
	Max      *int   `yaml:"max,omitempty" json:"max,omitempty"`           // 置ける枚数（既定 10）
}

// photoParams は、組み立て済みの photo。
type photoParams struct {
	interval  time.Duration
	random    bool
	night     bool
	nightFrom int // 分（0〜1439）
	nightTo   int
	level     int
	max       int
}

func defaultPhotoParams() *photoParams {
	p, _ := (&PhotoConfig{}).compile()
	return p
}

// compile は photo を検証する。nil でも既定値を返す。
func (c *PhotoConfig) compile() (*photoParams, error) {
	if c == nil {
		c = &PhotoConfig{}
	}
	p := &photoParams{interval: defaultPhotoInt, level: defaultPhotoLvl, max: defaultPhotoMax}
	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil || d < 5*time.Second || d > 24*time.Hour {
			return nil, fmt.Errorf("interval must be a duration from 5s to 24h (such as 1m), got %q", c.Interval)
		}
		p.interval = d
	}
	switch c.Order {
	case "", "sequential":
	case "random":
		p.random = true
	default:
		return nil, fmt.Errorf("order must be sequential or random, got %q", c.Order)
	}
	night := c.Night
	if night == "" {
		night = defaultPhotoNite
	}
	if night != durationOff {
		from, to, ok := parseTimeRange(night)
		if !ok {
			return nil, fmt.Errorf(`night must be "HH:MM-HH:MM" (such as 23:00-07:00) or off, got %q`, c.Night)
		}
		p.night, p.nightFrom, p.nightTo = from != to, from, to
	}
	if c.Level != nil {
		if *c.Level < 0 || *c.Level > 1000 {
			return nil, fmt.Errorf("level must be 0..max_brightness (0..7 on the PW-SH2)")
		}
		p.level = *c.Level
	}
	if c.Max != nil {
		if *c.Max < 1 || *c.Max > maxPhotoMax {
			return nil, fmt.Errorf("max must be 1..%d", maxPhotoMax)
		}
		p.max = *c.Max
	}
	return p, nil
}

func (c *PhotoConfig) validate(fail func(path, format string, args ...any)) {
	if _, err := c.compile(); err != nil {
		fail("/photo", "photo: %v", err)
	}
}

// parseTimeRange は "23:00-07:00" を、分の組にする。
func parseTimeRange(s string) (from, to int, ok bool) {
	a, b, found := strings.Cut(strings.ReplaceAll(s, " ", ""), "-")
	if !found {
		return 0, 0, false
	}
	hm := func(t string) (int, bool) {
		var h, m int
		if n, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil || n != 2 || h < 0 || h > 24 || m < 0 || m > 59 || h == 24 && m != 0 {
			return 0, false
		}
		return (h*60 + m) % 1440, true
	}
	from, ok1 := hm(a)
	to, ok2 := hm(b)
	return from, to, ok1 && ok2
}

// atNight は、t が夜間の時間帯にあるか。
func (p *photoParams) atNight(t time.Time) bool {
	if p == nil || !p.night {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if p.nightFrom < p.nightTo {
		return m >= p.nightFrom && m < p.nightTo
	}
	return m >= p.nightFrom || m < p.nightTo
}

// ---------- 置き場所 ----------

// photoMeta は index.json に残す、写真 1 枚の情報。
type photoMeta struct {
	Name   string    `json:"name,omitempty"`
	W      int       `json:"w"`
	H      int       `json:"h"`
	Bytes  int64     `json:"bytes"`
	SHA256 string    `json:"sha256,omitempty"`
	Added  time.Time `json:"added"`
	Source string    `json:"source,omitempty"`
	Of     string    `json:"of,omitempty"` // 縦向きに切り抜き直した写真なら、もとの（横向きの）写真の id
}

type photoIndex struct {
	Photos map[string]photoMeta `json:"photos"`
}

// PhotoInfo は list_photos の 1 枚。
type PhotoInfo struct {
	ID string `json:"id"`
	photoMeta
}

// PhotoList は list_photos の結果。
type PhotoList struct {
	Photos       []PhotoInfo `json:"photos"`
	Max          int         `json:"max"`
	TotalBytes   int64       `json:"total_bytes"`
	FreeBytes    int64       `json:"free_bytes"`
	ReserveBytes int64       `json:"reserve_bytes"`
	Showing      string      `json:"showing,omitempty"` // 今出している写真
}

var errPhotoNotFound = errors.New("photo not found")

// PhotoStore は写真の置き場所。
type PhotoStore struct {
	dir string
	mu  sync.Mutex
	idx photoIndex
	max int
}

// OpenPhotoStore は dir を使う（ディレクトリは写真を足すときに作る）。
func OpenPhotoStore(dir string) *PhotoStore {
	s := &PhotoStore{dir: dir, max: defaultPhotoMax}
	removeParts(dir)
	s.mu.Lock()
	s.loadLocked()
	s.mu.Unlock()
	return s
}

func (s *PhotoStore) path(id string) string { return filepath.Join(s.dir, id+photoExt) }

// SetMax は、置ける枚数を変える（設定の photo.max）。
func (s *PhotoStore) SetMax(n int) {
	s.mu.Lock()
	s.max = n
	s.mu.Unlock()
}

// loadLocked は index.json を読み、ファイルのないものを外し、index にないファイルを足す。
func (s *PhotoStore) loadLocked() {
	var x photoIndex
	if b, err := os.ReadFile(filepath.Join(s.dir, photoIndexFile)); err == nil {
		json.Unmarshal(b, &x)
	}
	if x.Photos == nil {
		x.Photos = map[string]photoMeta{}
	}
	ents, _ := os.ReadDir(s.dir)
	have := map[string]bool{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), photoExt)
		if !ok || !validImageID(id) {
			continue
		}
		have[id] = true
		if _, ok := x.Photos[id]; !ok {
			fi, _ := e.Info()
			m := photoMeta{Added: fi.ModTime().UTC()}
			if fi != nil {
				m.Bytes = fi.Size()
			}
			if f, err := os.Open(s.path(id)); err == nil {
				var hd [imageHeader]byte
				f.ReadAt(hd[:], 0)
				f.Close()
				m.W, m.H = int(binary.LittleEndian.Uint16(hd[4:])), int(binary.LittleEndian.Uint16(hd[6:]))
			}
			x.Photos[id] = m
		}
	}
	for id := range x.Photos {
		if !have[id] {
			delete(x.Photos, id)
		}
	}
	s.idx = x
}

func (s *PhotoStore) saveLocked() error {
	b, err := json.MarshalIndent(s.idx, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, photoIndexFile), append(b, '\n'), false)
}

// sorted は、足した順の写真。
func (s *PhotoStore) sortedLocked() []PhotoInfo {
	out := make([]PhotoInfo, 0, len(s.idx.Photos))
	for id, m := range s.idx.Photos {
		out = append(out, PhotoInfo{ID: id, photoMeta: m})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Added.Equal(out[j].Added) {
			return out[i].Added.Before(out[j].Added)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// List は list_photos の結果。
func (s *PhotoStore) List() PhotoList {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := PhotoList{Photos: s.sortedLocked(), Max: s.max}
	for _, p := range l.Photos {
		l.TotalBytes += p.Bytes
	}
	free, size, _ := imageFreeSpace(filepath.Dir(s.dir))
	if f, sz, err := imageFreeSpace(s.dir); err == nil {
		free, size = f, sz
	}
	l.FreeBytes, l.ReserveBytes = free, reserveFor(size)
	return l
}

// Count は写真の数。
func (s *PhotoStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.idx.Photos)
}

// Delete は id の写真を消す。
func (s *PhotoStore) Delete(id string) error {
	if !validImageID(id) {
		return fmt.Errorf("%w: photo must be a 16-digit lowercase hex id", errTransferBad)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idx.Photos[id]; !ok {
		return fmt.Errorf("%w: %s is not on this Brain", errPhotoNotFound, id)
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(s.idx.Photos, id)
	if err := s.saveLocked(); err != nil {
		log.Printf("photos: index: %v", err)
	}
	log.Printf("photos: removed %s", id)
	return nil
}

// Read は id の写真のファイルの offset から最大 n バイトを返す（get_photo）。
func (s *PhotoStore) Read(id string, offset int64, n int) ([]byte, int64, error) {
	if !validImageID(id) {
		return nil, 0, fmt.Errorf("%w: photo must be a 16-digit lowercase hex id", errTransferBad)
	}
	if n <= 0 || n > imageChunkMax {
		n = imageChunkMax
	}
	f, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, fmt.Errorf("%w: %s is not on this Brain", errPhotoNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if offset < 0 || offset > st.Size() {
		return nil, 0, fmt.Errorf("%w: offset %d is outside the photo (%d bytes)", errTransferBad, offset, st.Size())
	}
	b := make([]byte, min(int64(n), st.Size()-offset))
	if _, err := f.ReadAt(b, offset); err != nil {
		return nil, 0, err
	}
	return b, st.Size(), nil
}

// load は id の写真を読む。
func (s *PhotoStore) load(id string) (*Image, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	return parseImage(b)
}

// checkPhotoSize は、写真の大きさが画面と同じかを確かめる。
func checkPhotoSize(w, h int) error {
	if !(w == 800 && h == 480 || w == 480 && h == 800) {
		return fmt.Errorf("a photo must be 800x480 (landscape) or 480x800 (portrait), got %dx%d", w, h)
	}
	return nil
}

// roomLocked は、もう 1 枚置けるかを確かめる。
func (s *PhotoStore) roomLocked(bytes int64) error {
	if len(s.idx.Photos) >= s.max {
		return imageErr(errImageQuota, "there are already %d photos (photo.max is %d); remove one first (brain-deck photo rm)", len(s.idx.Photos), s.max)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	free, size, err := imageFreeSpace(s.dir)
	if err != nil {
		return fmt.Errorf("checking free space: %w", err)
	}
	if reserve := reserveFor(size); free-bytes < reserve {
		return imageErr(errImageNoSpace, "the SD card has %s free; saving %s would leave less than %s, which is kept for the system",
			kib(free), kib(bytes), kib(reserve))
	}
	return nil
}

// UploadKind は、upload_* の kind: photo。of（upload_begin の of）で、縦向きに切り抜き直したもとの写真を指す。
func (s *PhotoStore) UploadKind() UploadKind {
	return UploadKind{Dir: s.dir, MaxBytes: photoMaxBytes,
		Check: func(b UploadBegin) error {
			if b.Of != "" && !validImageID(b.Of) {
				return fmt.Errorf(`%w: "of" must be the id of the landscape photo`, errTransferBad)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if id := strings.ToLower(b.SHA256); len(id) >= imageIDLen {
				if _, ok := s.idx.Photos[id[:imageIDLen]]; ok {
					return nil // 同じ写真はもうある（受け取ったあと、置き換えるだけ）
				}
			}
			return s.roomLocked(b.Bytes)
		},
		Done: func(b UploadBegin, f *os.File, part string) (any, error) {
			var hd [imageHeader]byte
			_, err := f.ReadAt(hd[:], 0)
			w, h := int(binary.LittleEndian.Uint16(hd[4:])), int(binary.LittleEndian.Uint16(hd[6:]))
			if err == nil && (string(hd[:4]) != imageMagic || b.Bytes != int64(imageHeader+w*h*2)) {
				err = fmt.Errorf("not an LHI1 image of %d bytes", b.Bytes)
			}
			if err == nil {
				err = checkPhotoSize(w, h)
			}
			if err == nil && b.Of != "" && h != 800 {
				err = errors.New(`"of" is for a portrait (480x800) crop of a landscape photo`)
			}
			if err != nil {
				f.Close()
				os.Remove(part)
				return nil, imageErr(errImageBad, "%v; the upload was discarded", err)
			}
			if err := f.Close(); err != nil {
				os.Remove(part)
				return nil, err
			}
			id := b.SHA256[:imageIDLen]
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := os.Rename(part, s.path(id)); err != nil {
				os.Remove(part)
				return nil, err
			}
			if d, err := os.Open(s.dir); err == nil {
				d.Sync()
				d.Close()
			}
			s.idx.Photos[id] = photoMeta{Name: cleanImageName(b.Name), W: w, H: h, Bytes: b.Bytes, SHA256: b.SHA256,
				Added: time.Now().UTC(), Source: b.Source, Of: b.Of}
			if err := s.saveLocked(); err != nil {
				log.Printf("photos: index: %v (the photo itself was saved)", err)
			}
			log.Printf("photos: saved %s %dx%d %q from %s", id, w, h, b.Name, b.Source)
			return map[string]any{"id": id, "count": len(s.idx.Photos), "max": s.max}, nil
		}}
}

// slides は、向き（portrait なら縦向きの画面）で出す写真の id を、足した順に返す。
func (s *PhotoStore) slides(portrait bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.sortedLocked()
	crop := map[string]string{} // 横向きの写真 → 縦向きに切り抜き直した写真
	for _, p := range all {
		if p.Of != "" {
			if _, ok := s.idx.Photos[p.Of]; ok {
				crop[p.Of] = p.ID
			}
		}
	}
	var out []string
	for _, p := range all {
		land := p.W > p.H
		switch {
		case portrait && land && crop[p.ID] != "":
			out = append(out, crop[p.ID])
		case portrait && !land && p.Of != "" && s.idx.Photos[p.Of].W > 0:
			// もとの写真のところで出す
		case !portrait && !land && p.Of != "" && s.idx.Photos[p.Of].W > 0:
			// 横向きの画面では、もとの写真を出す
		default:
			out = append(out, p.ID)
		}
	}
	return out
}

// photoForScreen は、写真を W×H の画面に合わせる（向きが違えば、縮めて帯を入れる）。
func photoForScreen(img *Image, W, H int) *Image {
	if img.W == W && img.H == H {
		return img
	}
	src := unsafe.Slice((*uint16)(unsafe.Pointer(&img.Pix[0])), img.W*img.H)
	if !littleEndian() {
		src = make([]uint16, img.W*img.H)
		for i := range src {
			src[i] = binary.LittleEndian.Uint16(img.Pix[i*2:])
		}
	}
	out := imgconv.Fit565(src, img.W, img.H, W, H)
	pix := make([]byte, len(out)*2)
	for i, v := range out {
		binary.LittleEndian.PutUint16(pix[i*2:], v)
	}
	return &Image{W: W, H: H, Pix: pix}
}

func littleEndian() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}

// ---------- 写真を順に出す ----------

// photoShower は、写真を画面に出す側（*Display）。nil で元の画面に戻す。
type photoShower interface {
	SetPhoto(*Image)
}

// PhotoFrame は、写真を順に出す。Dimmer が写真の段階に入ったとき Start、出たとき Stop を呼ぶ。
type PhotoFrame struct {
	store *PhotoStore
	mu    sync.Mutex
	p     *photoParams
	disp  photoShower
	size  func() (int, int) // 今の画面の論理の大きさ（向き）
	night func()            // 夜間になったときに呼ぶ（画面を消す）
	on    bool
	gen   uint64
	order []string
	pos   int
	cur   string
	// fitted は、向きに合わせて縮めた写真（帯を入れたもの）。縮めるのは重いので、新しい 2 枚だけを覚えておく
	fitted []fittedPhoto
}

type fittedPhoto struct {
	key string
	img *Image
}

// fitCached は、写真 id を w×h の画面に合わせたもの。縮めたものは覚えておく。
func (f *PhotoFrame) fitCached(id string, img *Image, w, h int) *Image {
	if img.W == w && img.H == h {
		return img
	}
	key := fmt.Sprintf("%s %dx%d", id, w, h)
	f.mu.Lock()
	for _, c := range f.fitted {
		if c.key == key {
			f.mu.Unlock()
			return c.img
		}
	}
	f.mu.Unlock()
	out := photoForScreen(img, w, h)
	f.mu.Lock()
	f.fitted = append(f.fitted, fittedPhoto{key, out})
	if len(f.fitted) > 2 {
		f.fitted = f.fitted[1:]
	}
	f.mu.Unlock()
	return out
}

// NewPhotoFrame は、store の写真を出す PhotoFrame を作る。
func NewPhotoFrame(store *PhotoStore, p *photoParams) *PhotoFrame {
	if p == nil {
		p = defaultPhotoParams()
	}
	return &PhotoFrame{store: store, p: p, size: func() (int, int) { return 800, 480 }}
}

// SetDisplay は、写真を出す画面を設定する。
func (f *PhotoFrame) SetDisplay(d photoShower) {
	f.mu.Lock()
	f.disp = d
	f.mu.Unlock()
}

// SetSize は、今の画面の論理の大きさ（向き）を返す関数を設定する。
func (f *PhotoFrame) SetSize(size func() (int, int)) {
	f.mu.Lock()
	f.size = size
	f.mu.Unlock()
}

// SetParams は設定の photo を差し替える。
func (f *PhotoFrame) SetParams(p *photoParams) {
	if f == nil || p == nil {
		return
	}
	f.mu.Lock()
	f.p = p
	f.mu.Unlock()
	f.store.SetMax(p.max)
}

func (f *PhotoFrame) params() *photoParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.p
}

// Level は、写真を出すときの明るさ。
func (f *PhotoFrame) Level() int { return f.params().level }

// AtNight は、今が夜間の時間帯か。
func (f *PhotoFrame) AtNight(now time.Time) bool { return f != nil && f.params().atNight(now) }

// Has は、今の向きで出せる写真があるか。
func (f *PhotoFrame) Has() bool {
	if f == nil || f.store == nil {
		return false
	}
	w, h := f.size()
	return len(f.store.slides(h > w)) > 0
}

// Showing は、今出している写真の id（出していなければ空）。
func (f *PhotoFrame) Showing() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.on {
		return ""
	}
	return f.cur
}

// Start は写真を出し始める。待たずに返る（SD カードから読むのは別の goroutine）。
func (f *PhotoFrame) Start() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.on {
		f.mu.Unlock()
		return
	}
	f.on = true
	f.gen++
	gen := f.gen
	w, h := f.size()
	f.order = f.store.slides(h > w)
	if f.p.random {
		rand.Shuffle(len(f.order), func(i, j int) { f.order[i], f.order[j] = f.order[j], f.order[i] })
	} else if f.cur != "" {
		// 前に出していた写真の次から
		for i, id := range f.order {
			if id == f.cur {
				f.pos = i + 1
			}
		}
	}
	if f.pos >= len(f.order) || f.p.random {
		f.pos = 0
	}
	f.mu.Unlock()
	go f.run(gen)
}

// Stop は写真をやめて、元の画面に戻す。
func (f *PhotoFrame) Stop() {
	if f == nil {
		return
	}
	// SetPhoto は待たずに返る。ロックを持ったまま呼び、run が読み終えた写真を、やめたあとに出さないようにする
	f.mu.Lock()
	defer f.mu.Unlock()
	was := f.on
	f.on = false
	f.gen++
	if was && f.disp != nil {
		f.disp.SetPhoto(nil)
	}
}

func (f *PhotoFrame) run(gen uint64) {
	// 写真を読んで縮めるのは重い（ARMv5 で 0.5〜0.7 秒）ので、入力と描画の goroutine を先に走らせる
	runtime.LockOSThread()
	syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), 15)
	w0, h0 := f.size()
	for {
		f.mu.Lock()
		if f.gen != gen || !f.on {
			f.mu.Unlock()
			return
		}
		p := f.p
		if p.atNight(time.Now()) {
			night := f.night
			f.mu.Unlock()
			if night != nil {
				night() // 画面を消す（Dimmer が Stop を呼ぶ）
			}
			return
		}
		if len(f.order) == 0 {
			f.mu.Unlock()
			return
		}
		if f.pos >= len(f.order) {
			f.pos = 0
			if p.random {
				rand.Shuffle(len(f.order), func(i, j int) { f.order[i], f.order[j] = f.order[j], f.order[i] })
			}
		}
		id := f.order[f.pos]
		f.pos++
		disp := f.disp
		f.mu.Unlock()
		w, h := f.size()
		if w != w0 || h != h0 {
			return // 画面の向きが変わった（明るくして写真をやめるので、ここでは何もしない）
		}
		t := time.Now()
		img, err := f.store.load(id)
		if err != nil {
			log.Printf("photos: %s: %v", id, err)
		} else {
			img = f.fitCached(id, img, w, h)
			f.mu.Lock()
			ok := f.gen == gen && f.on
			if ok {
				f.cur = id
				if disp != nil {
					disp.SetPhoto(img) // Stop と同じロックの中で（やめたあとに出さない）
				}
			}
			f.mu.Unlock()
			if !ok {
				return
			}
			vlogf("photos: showing %s (%dx%d) in %v", id, w, h, time.Since(t))
		}
		time.Sleep(time.Until(t.Add(p.interval)))
	}
}
