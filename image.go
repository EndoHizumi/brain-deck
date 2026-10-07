package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- 背景画像（/var/lib/lefthand/images） ----------
//
// 画像の読み込み、切り抜き、縮小、RGB565 への変換、ディザリングは設定 GUI（PC）で行う。
// Brain は変換済みのファイルを受け取って保存し、描くときに画面へ写すだけにする。
//
// ファイルの形（<id>.565）：
//
//	0..3   "LHI1"
//	4..5   幅（リトルエンディアンの uint16）
//	6..7   高さ（同）
//	8..    画素。左上から行の順に、1 画素 2 バイトの RGB565（リトルエンディアン）
//
// id はファイル全体の SHA-256 の先頭 16 文字（16 進の小文字）。設定からは `background: <id>` で参照する。
// 元のファイル名などの名前は、index.json に id ごとに残す（名前を変えても中身は同じなので、ハッシュには入れない）。

const (
	imageMagic     = "LHI1"
	imageHeader    = 8
	imageExt       = ".565"
	imageIDLen     = 16
	imageMaxSide   = 800                            // 幅と高さの上限（回転した画面の 480x800 も入る）
	imageMaxPixels = 800 * 480                      // 画面いっぱいまで
	imageMaxBytes  = imageHeader + imageMaxPixels*2 // 1 枚の上限 768,008 バイト
	imageChunkMax  = 96 << 10                       // image_chunk の 1 回の大きさ（base64 で 128 KiB。1 行の上限に収まる）
	imageIndexFile = "index.json"                   //
	imagePartGlob  = ".upload-*.part"               // 受け取っている途中のファイル
	imageNameMax   = 100                            // 名前の長さ（文字）
)

// 上限。テストで小さくするので変数にしてある
var (
	imageQuota      int64 = 16 << 20         // images の合計の上限
	imageReserve    int64 = 64 << 20         // 保存したあとも、ファイルシステムに残しておく空き
	imageCacheBytes       = 4 << 20          // 読み込んだ画像をメモリに置いておく上限
	imageUploadIdle       = 60 * time.Second // image_chunk が来ないまま、これだけたつと受け取りをやめる
	imageFreeSpace        = statfsFree       // ファイルシステムの空き（テストで差し替える）
)

var imageIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func validImageID(s string) bool { return imageIDRE.MatchString(s) }

// エラーの種類。control.go で、プロトコルのエラーの code にする
var (
	errImageBad      = errors.New("bad image request")
	errImageNotFound = errors.New("image not found")
	errImageQuota    = errors.New("image quota exceeded")
	errImageNoSpace  = errors.New("not enough free space")
	errImageHash     = errors.New("image hash mismatch")
	errImageUpload   = errors.New("no such upload")
)

func imageErr(kind error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", kind, fmt.Sprintf(format, args...))
}

// Image は読み込んだ画像。Pix は RGB565（リトルエンディアン）で W*H*2 バイト。
type Image struct {
	W, H int
	Pix  []byte
}

func (im *Image) bytes() int { return len(im.Pix) + imageHeader }

// parseImage はファイルの中身を読む。
func parseImage(b []byte) (*Image, error) {
	if len(b) < imageHeader || string(b[:4]) != imageMagic {
		return nil, errors.New("not a lefthand image (LHI1)")
	}
	w, h := int(binary.LittleEndian.Uint16(b[4:])), int(binary.LittleEndian.Uint16(b[6:]))
	if err := checkImageSize(w, h); err != nil {
		return nil, err
	}
	if len(b) != imageHeader+w*h*2 {
		return nil, fmt.Errorf("%dx%d needs %d bytes, got %d", w, h, imageHeader+w*h*2, len(b))
	}
	return &Image{W: w, H: h, Pix: b[imageHeader:]}, nil
}

func checkImageSize(w, h int) error {
	if w < 1 || h < 1 || w > imageMaxSide || h > imageMaxSide || w*h > imageMaxPixels {
		return fmt.Errorf("image size %dx%d is out of range (each side 1..%d, at most %d pixels)", w, h, imageMaxSide, imageMaxPixels)
	}
	return nil
}

// encodeImage はファイルの中身を作る（テストと例の画像用）。
func encodeImage(w, h int, pix []byte) []byte {
	b := make([]byte, imageHeader, imageHeader+len(pix))
	copy(b, imageMagic)
	binary.LittleEndian.PutUint16(b[4:], uint16(w))
	binary.LittleEndian.PutUint16(b[6:], uint16(h))
	return append(b, pix...)
}

// imageIDOf はファイルの中身の id と、SHA-256（16 進）を返す。
func imageIDOf(b []byte) (id, sum string) {
	s := sha256.Sum256(b)
	sum = hex.EncodeToString(s[:])
	return sum[:imageIDLen], sum
}

// imageMeta は index.json に残す、画像 1 枚の情報。
type imageMeta struct {
	Name   string    `json:"name,omitempty"`
	W      int       `json:"w"`
	H      int       `json:"h"`
	Bytes  int64     `json:"bytes"`
	SHA256 string    `json:"sha256,omitempty"`
	Added  time.Time `json:"added"`
	Source string    `json:"source,omitempty"`
}

type imageIndex struct {
	Images map[string]imageMeta `json:"images"`
}

// ImageStore は背景画像の置き場所と、読み込んだ画像のキャッシュ。
// 描画の goroutine（Image）と通信の goroutine（受け取り、一覧、削除）の両方から使う。
type ImageStore struct {
	dir string

	mu       sync.Mutex
	cache    map[string]*cachedImage
	tick     uint64 // キャッシュの使った順
	cached   int    // キャッシュの合計バイト
	missing  map[string]bool
	up       *imageUpload
	onChange func() // 画像が増えた・減ったとき（画面を描き直す）
}

type cachedImage struct {
	img  *Image
	used uint64
}

// NewImageStore は dir を画像の置き場所として使う。ディレクトリは作らない（-check、-render-png 用）。
func NewImageStore(dir string) *ImageStore {
	return &ImageStore{dir: dir, cache: map[string]*cachedImage{}, missing: map[string]bool{}}
}

// OpenImageStore は dir を（なければ作って）使い、前に受け取りの途中で止まったファイルを消す。
func OpenImageStore(dir string) *ImageStore {
	s := NewImageStore(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("images: %v (background images cannot be saved)", err)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, imagePartGlob))
	for _, p := range parts {
		os.Remove(p)
		log.Printf("images: removed an incomplete upload %s", filepath.Base(p))
	}
	return s
}

// SetOnChange は、画像を足したり消したりしたときに呼ぶ関数を設定する。
func (s *ImageStore) SetOnChange(f func()) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

func (s *ImageStore) path(id string) string { return filepath.Join(s.dir, id+imageExt) }

// Image は id の画像を返す。キャッシュになければ読み込む。ない、読めないときは nil。
// ないことは、id ごとに 1 回だけログに出す。描画の goroutine から呼ぶ。
func (s *ImageStore) Image(id string) *Image {
	if s == nil || !validImageID(id) {
		return nil
	}
	s.mu.Lock()
	s.tick++
	if c, ok := s.cache[id]; ok {
		c.used = s.tick
		s.mu.Unlock()
		return c.img
	}
	s.mu.Unlock()
	// SD カードから読むあいだは、ロックを持たない（ほかの goroutine の受け取りや先読みを待たせない）
	t := time.Now()
	b, err := os.ReadFile(s.path(id))
	var img *Image
	if err == nil {
		img, err = parseImage(b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[id]; ok { // 読んでいるあいだに、ほかの goroutine が読んだ
		c.used = s.tick
		return c.img
	}
	if err != nil {
		if !s.missing[id] {
			s.missing[id] = true
			if errors.Is(err, os.ErrNotExist) {
				log.Printf("images: background %s is not on this Brain (%s); drawing without it", id, s.dir)
			} else {
				log.Printf("images: background %s: %v; drawing without it", id, err)
			}
		}
		return nil
	}
	s.cache[id] = &cachedImage{img: img, used: s.tick}
	s.cached += img.bytes()
	s.evictLocked(id)
	vlogf("images: loaded %s (%dx%d) in %v, cache %d KiB", id, img.W, img.H, time.Since(t), s.cached>>10)
	return img
}

// Preload は、ids の画像を、優先度を下げた別の goroutine で先に読み込む（キャッシュの上限まで）。
// レイヤーを初めて切り替えたときに、SD カードからの読み込みを待たないようにするため。待たずに返る。
func (s *ImageStore) Preload(ids []string) {
	if s == nil || len(ids) == 0 {
		return
	}
	go func() {
		runtime.LockOSThread()
		syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), 10)
		t := time.Now()
		n := 0
		for _, id := range ids {
			if s.CacheBytes() >= imageCacheBytes {
				break
			}
			if s.Image(id) != nil {
				n++
			}
		}
		vlogf("images: preloaded %d of %d images in %v", n, len(ids), time.Since(t))
	}()
}

// preloadOrder は、設定で使っている画像の id を、base のものから順に返す。
func preloadOrder(cfg *Config) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, l := range cfg.Layers {
		if l.Touch == nil {
			continue
		}
		add(l.Touch.Background)
		keys := make([]string, 0, len(l.Touch.Cells))
		for k := range l.Touch.Cells {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(l.Touch.Cells[k].Background)
		}
	}
	return out
}

// evictLocked は、キャッシュが上限を超えたら、使ってから長い順に捨てる（keep は捨てない）。
// 1 つのレイヤーで上限を超える画像を使うときは、描くたびに読み直すことになるが、描けなくはならない。
func (s *ImageStore) evictLocked(keep string) {
	for s.cached > imageCacheBytes {
		old, oldest := "", uint64(0)
		for id, c := range s.cache {
			if id != keep && (old == "" || c.used < oldest) {
				old, oldest = id, c.used
			}
		}
		if old == "" {
			return
		}
		s.cached -= s.cache[old].img.bytes()
		delete(s.cache, old)
	}
}

// CacheBytes は、キャッシュに置いている画像の合計バイト。
func (s *ImageStore) CacheBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached
}

// Has は id の画像があるか。
func (s *ImageStore) Has(id string) bool {
	if s == nil || !validImageID(id) {
		return false
	}
	_, err := os.Stat(s.path(id))
	return err == nil
}

// Size は id の画像の幅と高さ（ヘッダだけを読む）。
func (s *ImageStore) Size(id string) (w, h int, err error) {
	f, err := os.Open(s.path(id))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var b [imageHeader]byte
	if _, err := f.ReadAt(b[:], 0); err != nil || string(b[:4]) != imageMagic {
		return 0, 0, errors.New("not a lefthand image")
	}
	return int(binary.LittleEndian.Uint16(b[4:])), int(binary.LittleEndian.Uint16(b[6:])), nil
}

func (s *ImageStore) loadIndexLocked() imageIndex {
	var x imageIndex
	if b, err := os.ReadFile(filepath.Join(s.dir, imageIndexFile)); err == nil {
		json.Unmarshal(b, &x)
	}
	if x.Images == nil {
		x.Images = map[string]imageMeta{}
	}
	return x
}

func (s *ImageStore) saveIndexLocked(x imageIndex) error {
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, imageIndexFile), append(b, '\n'), false)
}

// files は、置いてある画像の id と大きさ（バイト）を返す。
func (s *ImageStore) files() (map[string]int64, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]int64{}, nil
		}
		return nil, err
	}
	out := map[string]int64{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), imageExt)
		if !ok || !validImageID(id) || !e.Type().IsRegular() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out[id] = fi.Size()
		}
	}
	return out, nil
}

func statfsFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// ImageInfo は list_images で返す、画像 1 枚の情報。
type ImageInfo struct {
	ID string `json:"id"`
	imageMeta
	Refs []string `json:"refs"` // 今の設定で、この画像を使っている場所（JSON Pointer）
}

// ImageList は list_images の結果。
type ImageList struct {
	Images       []ImageInfo `json:"images"`
	TotalBytes   int64       `json:"total_bytes"`
	LimitBytes   int64       `json:"limit_bytes"`
	MaxImage     int         `json:"max_image_bytes"`
	MaxW         int         `json:"max_side"`
	MaxPixels    int         `json:"max_pixels"`
	FreeBytes    int64       `json:"free_bytes"`
	ReserveBytes int64       `json:"reserve_bytes"`
	ChunkBytes   int         `json:"chunk_bytes"`
	Missing      []string    `json:"missing"` // 設定で使っているのに、Brain にない id
}

// List は置いてある画像の一覧を返す。refs は、id ごとの設定での使い道（imageRefs）。
func (s *ImageStore) List(refs map[string][]string) (ImageList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.files()
	if err != nil {
		return ImageList{}, err
	}
	x := s.loadIndexLocked()
	out := ImageList{Images: []ImageInfo{}, LimitBytes: imageQuota, MaxImage: imageMaxBytes, MaxW: imageMaxSide,
		MaxPixels: imageMaxPixels, ReserveBytes: imageReserve, ChunkBytes: imageChunkMax, Missing: []string{}}
	for id, n := range files {
		m, ok := x.Images[id]
		if !ok { // index.json がない、壊れたとき。大きさはヘッダから読む
			m.W, m.H, _ = s.Size(id)
		}
		m.Bytes = n
		r := refs[id]
		if r == nil {
			r = []string{}
		}
		out.Images = append(out.Images, ImageInfo{ID: id, imageMeta: m, Refs: r})
		out.TotalBytes += n
	}
	sort.Slice(out.Images, func(i, j int) bool {
		a, b := out.Images[i], out.Images[j]
		if !a.Added.Equal(b.Added) {
			return a.Added.Before(b.Added)
		}
		return a.ID < b.ID
	})
	for id := range refs {
		if _, ok := files[id]; !ok {
			out.Missing = append(out.Missing, id)
		}
	}
	sort.Strings(out.Missing)
	out.FreeBytes, _ = imageFreeSpace(s.dir)
	return out, nil
}

// Read は id の画像のファイルの offset から最大 n バイトを返す（get_image）。
func (s *ImageStore) Read(id string, offset int64, n int) (data []byte, total int64, sum string, err error) {
	if !validImageID(id) {
		return nil, 0, "", imageErr(errImageBad, "image must be a 16-digit lowercase hex id")
	}
	if n <= 0 || n > imageChunkMax {
		n = imageChunkMax
	}
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, "", imageErr(errImageNotFound, "image %s is not on this Brain", id)
	}
	if err != nil {
		return nil, 0, "", err
	}
	if offset < 0 || offset > int64(len(b)) {
		return nil, 0, "", imageErr(errImageBad, "offset %d is outside the image (%d bytes)", offset, len(b))
	}
	_, sum = imageIDOf(b)
	end := min(offset+int64(n), int64(len(b)))
	return b[offset:end], int64(len(b)), sum, nil
}

// Prune は、keep にない画像（今の設定で使っていない画像）を消す。dry なら消さずに、消すものを返す。
func (s *ImageStore) Prune(keep map[string]bool, dry bool) (removed []string, freed int64, err error) {
	s.mu.Lock()
	files, err := s.files()
	if err != nil {
		s.mu.Unlock()
		return nil, 0, err
	}
	removed = []string{}
	for id, n := range files {
		if keep[id] {
			continue
		}
		if !dry {
			if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.mu.Unlock()
				return removed, freed, err
			}
			if c, ok := s.cache[id]; ok {
				s.cached -= c.img.bytes()
				delete(s.cache, id)
			}
		}
		removed = append(removed, id)
		freed += n
	}
	sort.Strings(removed)
	if !dry && len(removed) > 0 {
		x := s.loadIndexLocked()
		for _, id := range removed {
			delete(x.Images, id)
		}
		if err := s.saveIndexLocked(x); err != nil {
			log.Printf("images: index: %v", err)
		}
		if d, err := os.Open(s.dir); err == nil {
			d.Sync()
			d.Close()
		}
	}
	f := s.onChange
	s.mu.Unlock()
	if !dry && len(removed) > 0 && f != nil {
		f()
	}
	return removed, freed, nil
}

// ---------- 受け取り（image_begin、image_chunk、image_end、image_abort） ----------
//
// 一時ファイル（.upload-*.part）に順に書き、最後に大きさと SHA-256 を確かめてから、fsync して
// <id>.565 に rename する。確かめる前に <id>.565 ができることはない。
// 途中で切れたとき（image_abort、別の image_begin、接続の終わり、imageUploadIdle のあいだ chunk が来ない、
// デーモンの再起動）は、一時ファイルを消す。受け取りは同時に 1 つだけ。

type imageUpload struct {
	token string
	f     *os.File
	path  string
	want  int64
	got   int64
	sum   string // 送る側が言った SHA-256
	h     hash.Hash
	meta  imageMeta
	timer *time.Timer
}

// ImageBegin は image_begin の引数。
type ImageBegin struct {
	SHA256 string
	Bytes  int64
	W, H   int
	Name   string
	Source string
}

// BeginResult は image_begin の結果。Exists なら、もうあるので送らなくてよい。
type BeginResult struct {
	ID         string `json:"id"`
	Exists     bool   `json:"exists"`
	Upload     string `json:"upload,omitempty"`
	ChunkBytes int    `json:"chunk_bytes,omitempty"`
}

// Begin は受け取りを始める。前の受け取りが途中なら、やめる。
func (s *ImageStore) Begin(b ImageBegin, now time.Time) (BeginResult, error) {
	b.SHA256 = strings.ToLower(b.SHA256)
	if len(b.SHA256) != 64 || strings.Trim(b.SHA256, "0123456789abcdef") != "" {
		return BeginResult{}, imageErr(errImageBad, `"sha256" must be the 64-digit hex SHA-256 of the file`)
	}
	if err := checkImageSize(b.W, b.H); err != nil {
		return BeginResult{}, imageErr(errImageBad, "%v", err)
	}
	if want := int64(imageHeader + b.W*b.H*2); b.Bytes != want {
		return BeginResult{}, imageErr(errImageBad, `"bytes" must be %d for a %dx%d image (8-byte header + 2 bytes per pixel), got %d`, want, b.W, b.H, b.Bytes)
	}
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(b.Name))
	if r := []rune(name); len(r) > imageNameMax {
		name = string(r[:imageNameMax])
	}
	id := b.SHA256[:imageIDLen]

	s.mu.Lock()
	defer s.mu.Unlock()
	s.abortLocked("")
	if _, err := os.Stat(s.path(id)); err == nil {
		// 同じ中身はもうある。名前がなければ足す
		x := s.loadIndexLocked()
		if m, ok := x.Images[id]; ok && m.Name == "" && name != "" {
			m.Name = name
			x.Images[id] = m
			s.saveIndexLocked(x)
		}
		return BeginResult{ID: id, Exists: true}, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return BeginResult{}, err
	}
	files, err := s.files()
	if err != nil {
		return BeginResult{}, err
	}
	var total int64
	for _, n := range files {
		total += n
	}
	if total+b.Bytes > imageQuota {
		return BeginResult{}, imageErr(errImageQuota,
			"the images would take %s, over the limit of %s (now %s in %d images). Remove unused images first (prune_images)",
			kib(total+b.Bytes), kib(imageQuota), kib(total), len(files))
	}
	free, err := imageFreeSpace(s.dir)
	if err != nil {
		return BeginResult{}, fmt.Errorf("checking free space: %w", err)
	}
	if free-b.Bytes < imageReserve {
		return BeginResult{}, imageErr(errImageNoSpace,
			"the SD card has %s free; saving %s would leave less than %s, which is kept for the system",
			kib(free), kib(b.Bytes), kib(imageReserve))
	}
	var rnd [6]byte
	rand.Read(rnd[:])
	token := hex.EncodeToString(rnd[:])
	path := filepath.Join(s.dir, ".upload-"+token+".part")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return BeginResult{}, err
	}
	u := &imageUpload{token: token, f: f, path: path, want: b.Bytes, sum: b.SHA256, h: sha256.New(),
		meta: imageMeta{Name: name, W: b.W, H: b.H, Bytes: b.Bytes, SHA256: b.SHA256, Added: now.UTC(), Source: b.Source}}
	u.timer = time.AfterFunc(imageUploadIdle, func() { s.expire(token) })
	s.up = u
	vlogf("images: receiving %s %dx%d (%d bytes) %q", id, b.W, b.H, b.Bytes, name)
	return BeginResult{ID: id, Upload: token, ChunkBytes: imageChunkMax}, nil
}

func kib(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)>>10)
}

func (s *ImageStore) expire(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.up != nil && s.up.token == token {
		log.Printf("images: no data for %v; gave up the upload of %s", imageUploadIdle, s.up.sum[:imageIDLen])
		s.abortLocked("")
	}
}

// uploadLocked は token の受け取りを返す。
func (s *ImageStore) uploadLocked(token string) (*imageUpload, error) {
	if s.up == nil || s.up.token != token || token == "" {
		return nil, imageErr(errImageUpload, "upload %q is not in progress (it was finished, aborted, timed out, or replaced by another image_begin); start again with image_begin", token)
	}
	return s.up, nil
}

// Chunk は offset から data を書く。offset は、それまでに受け取った大きさと同じでなければならない。
func (s *ImageStore) Chunk(token string, offset int64, data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.uploadLocked(token)
	if err != nil {
		return 0, err
	}
	switch {
	case offset != u.got:
		return u.got, imageErr(errImageBad, "offset %d: expected %d (send the chunks in order)", offset, u.got)
	case len(data) == 0 || len(data) > imageChunkMax:
		return u.got, imageErr(errImageBad, "a chunk must be 1..%d bytes, got %d", imageChunkMax, len(data))
	case u.got+int64(len(data)) > u.want:
		s.abortLocked("")
		return 0, imageErr(errImageBad, "the data exceeds the announced %d bytes; the upload was discarded", u.want)
	}
	if _, err := u.f.Write(data); err != nil {
		s.abortLocked("")
		return 0, fmt.Errorf("writing the image: %w (the upload was discarded)", err)
	}
	u.h.Write(data)
	u.got += int64(len(data))
	u.timer.Reset(imageUploadIdle)
	return u.got, nil
}

// End は受け取りを終える。大きさ、SHA-256、ヘッダを確かめてから、<id>.565 にする。
// 合わなければ一時ファイルを消す。
func (s *ImageStore) End(token string) (string, error) {
	s.mu.Lock()
	u, err := s.uploadLocked(token)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.up = nil
	u.timer.Stop()
	fail := func(err error) (string, error) {
		u.f.Close()
		os.Remove(u.path)
		s.mu.Unlock()
		return "", err
	}
	if u.got != u.want {
		return fail(imageErr(errImageBad, "received %d of %d bytes; the upload was discarded", u.got, u.want))
	}
	if got := hex.EncodeToString(u.h.Sum(nil)); got != u.sum {
		return fail(imageErr(errImageHash, "the received data has SHA-256 %s, not %s; the upload was discarded", got[:imageIDLen], u.sum[:imageIDLen]))
	}
	var hd [imageHeader]byte
	if _, err := u.f.ReadAt(hd[:], 0); err != nil {
		return fail(err)
	}
	if string(hd[:4]) != imageMagic || int(binary.LittleEndian.Uint16(hd[4:])) != u.meta.W || int(binary.LittleEndian.Uint16(hd[6:])) != u.meta.H {
		return fail(imageErr(errImageBad, "the file header is not LHI1 %dx%d; the upload was discarded", u.meta.W, u.meta.H))
	}
	if err := u.f.Sync(); err != nil {
		return fail(err)
	}
	if err := u.f.Close(); err != nil {
		os.Remove(u.path)
		s.mu.Unlock()
		return "", err
	}
	id := u.sum[:imageIDLen]
	if err := os.Rename(u.path, s.path(id)); err != nil {
		os.Remove(u.path)
		s.mu.Unlock()
		return "", err
	}
	if d, err := os.Open(s.dir); err == nil {
		d.Sync()
		d.Close()
	}
	x := s.loadIndexLocked()
	x.Images[id] = u.meta
	if err := s.saveIndexLocked(x); err != nil {
		log.Printf("images: index: %v (the image itself was saved)", err)
	}
	delete(s.missing, id)
	f := s.onChange
	s.mu.Unlock()
	log.Printf("images: saved %s %dx%d %q from %s", id, u.meta.W, u.meta.H, u.meta.Name, u.meta.Source)
	if f != nil {
		f()
	}
	return id, nil
}

// Abort は受け取りをやめて、一時ファイルを消す。token が空なら、どの受け取りでもやめる。
func (s *ImageStore) Abort(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abortLocked(token)
}

func (s *ImageStore) abortLocked(token string) bool {
	u := s.up
	if u == nil || (token != "" && u.token != token) {
		return false
	}
	s.up = nil
	u.timer.Stop()
	u.f.Close()
	os.Remove(u.path)
	vlogf("images: upload of %s aborted (%d of %d bytes)", u.sum[:imageIDLen], u.got, u.want)
	return true
}

// ---------- 設定との関係 ----------

// imageRefs は、設定で使っている画像の id ごとに、使っている場所（JSON Pointer）を返す。
func imageRefs(cfg *Config) map[string][]string {
	out := map[string][]string{}
	if cfg == nil {
		return out
	}
	for li, l := range cfg.Layers {
		if l.Touch == nil {
			continue
		}
		tp := fmt.Sprintf("/layers/%d/touch", li)
		if l.Touch.Background != "" {
			out[l.Touch.Background] = append(out[l.Touch.Background], tp+"/background")
		}
		keys := make([]string, 0, len(l.Touch.Cells))
		for k := range l.Touch.Cells {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if id := l.Touch.Cells[k].Background; id != "" {
				out[id] = append(out[id], tp+"/cells/"+pointerEscape(k))
			}
		}
	}
	return out
}

// imageProblems は、設定の背景画像について、Brain にない・大きさが合わないものを返す（警告。動作は続ける）。
// W、H は画面の大きさ（回転したあと）。
func imageProblems(km *Keymap, store *ImageStore, W, H int) Problems {
	var out Problems
	check := func(path, where, id string, w, h int) {
		iw, ih, err := store.Size(id)
		switch {
		case errors.Is(err, os.ErrNotExist):
			out = append(out, Problem{path, fmt.Sprintf("%s: background image %s is not on this Brain (%s); it is drawn without the image", where, id, store.dir)})
		case err != nil:
			out = append(out, Problem{path, fmt.Sprintf("%s: background image %s: %v", where, id, err)})
		case iw != w || ih != h:
			out = append(out, Problem{path, fmt.Sprintf("%s: background image %s is %dx%d, but the area is %dx%d; it is centered and cropped", where, id, iw, ih, w, h)})
		}
	}
	for li, l := range km.Layers {
		if l.Grid == nil {
			continue
		}
		tp := fmt.Sprintf("/layers/%d/touch", li)
		if l.Grid.Background != "" {
			check(tp+"/background", fmt.Sprintf("layer %q wallpaper", l.Name), l.Grid.Background, W, H)
		}
		for p, a := range l.Grid.Cells {
			if a.Spec.Background == "" {
				continue
			}
			box := cellRect(p.Col, p.Row, a.SpanW, a.SpanH, l.Grid.Cols, l.Grid.Rows, W, H).Inset(cellGap)
			check(fmt.Sprintf("%s/cells/%d,%d", tp, p.Col, p.Row), fmt.Sprintf("layer %q touch cell \"%d,%d\"", l.Name, p.Col, p.Row),
				a.Spec.Background, box.Dx(), box.Dy())
		}
	}
	out.sort()
	return out
}

// screenSize は、設定の display.rotate を反映した画面の大きさ（PW-SH2 の 800x480）。
func screenSize(cfg *Config) (int, int) {
	if cfg.Display != nil && (cfg.Display.Rotate == 90 || cfg.Display.Rotate == 270) {
		return 480, 800
	}
	return 800, 480
}
