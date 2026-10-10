package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 分割して送られる大きなファイルの受け取り（共通） ----------
//
// 1 行の上限（256 KiB）より大きなファイル（背景画像、更新のファイルなど）は、送る側が分けて base64 で送る。
// 受け取りの手順はどれも同じなので、ここにまとめる。
//
//  1. 始める：全体の大きさと SHA-256 を受け取り、一時ファイル（<dir>/.upload-<token>.part）を作る
//  2. 書く：offset は、それまでに受け取った大きさと同じでなければ断る（受け取りは続く）。言った大きさを超えたら捨てる
//  3. 終える：大きさと SHA-256 を確かめ、fsync する。合わなければ一時ファイルを消す。
//     確かめたファイルの使い方（rename、展開など）は、種類ごとに決める
//  4. やめる：一時ファイルを消す。別の受け取りを始めた、接続が切れた、idle のあいだ何も来ない、のどれでもやめる
//
// プロトコルは、背景画像の image_*（前からあるもの）と、種類を選べる upload_*（docs/protocol.md）。

// 受け取りのエラー。control.go で、プロトコルのエラーの code にする
var (
	errTransferBad    = errors.New("bad upload request") // bad_request
	errTransferHash   = errors.New("hash mismatch")      // hash_mismatch
	errTransferUpload = errors.New("no such upload")     // bad_request
)

// transferPartGlob は、受け取っている途中のファイルの名前。
const transferPartGlob = ".upload-*.part"

// Receiver は、分割して送られる 1 つのファイルの受け取り。呼ぶ側が排他する。
type Receiver struct {
	Token    string
	path     string
	f        *os.File
	want     int64
	got      int64
	sum      string // 送る側が言った SHA-256（16 進の小文字）
	h        hash.Hash
	maxChunk int
	idleFor  time.Duration
	timer    *time.Timer
}

// newToken は、受け取りの名前（12 桁の 16 進）を作る。
func newToken() string {
	var rnd [6]byte
	rand.Read(rnd[:])
	return hex.EncodeToString(rnd[:])
}

// validSHA256 は、64 桁の 16 進か。
func validSHA256(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// newReceiver は dir に一時ファイルを作って受け取りを始める。idle のあいだ Write がなければ expire(token) を呼ぶ。
func newReceiver(dir string, want int64, sum string, maxChunk int, idle time.Duration, expire func(token string)) (*Receiver, error) {
	sum = strings.ToLower(sum)
	if !validSHA256(sum) {
		return nil, fmt.Errorf(`%w: "sha256" must be the 64-digit hex SHA-256 of the file`, errTransferBad)
	}
	if want <= 0 {
		return nil, fmt.Errorf(`%w: "bytes" must be positive`, errTransferBad)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	token := newToken()
	path := filepath.Join(dir, ".upload-"+token+".part")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	r := &Receiver{Token: token, path: path, f: f, want: want, sum: sum, h: sha256.New(), maxChunk: maxChunk, idleFor: idle}
	if expire != nil {
		r.timer = time.AfterFunc(idle, func() { expire(token) })
	}
	return r, nil
}

// Want は、全体の大きさ。Got は、受け取った大きさ。
func (r *Receiver) Want() int64 { return r.want }
func (r *Receiver) Got() int64  { return r.got }

// Sum は、送る側が言った SHA-256。
func (r *Receiver) Sum() string { return r.sum }

// Write は offset から data を書く。言った大きさを超えたら一時ファイルを消す（discarded が true）。
func (r *Receiver) Write(offset int64, data []byte) (got int64, discarded bool, err error) {
	switch {
	case offset != r.got:
		return r.got, false, fmt.Errorf("%w: offset %d: expected %d (send the chunks in order)", errTransferBad, offset, r.got)
	case len(data) == 0 || len(data) > r.maxChunk:
		return r.got, false, fmt.Errorf("%w: a chunk must be 1..%d bytes, got %d", errTransferBad, r.maxChunk, len(data))
	case r.got+int64(len(data)) > r.want:
		r.Abort()
		return 0, true, fmt.Errorf("%w: the data exceeds the announced %d bytes; the upload was discarded", errTransferBad, r.want)
	}
	if _, err := r.f.Write(data); err != nil {
		r.Abort()
		return 0, true, fmt.Errorf("writing the upload: %w (the upload was discarded)", err)
	}
	r.h.Write(data)
	r.got += int64(len(data))
	if r.timer != nil {
		r.timer.Reset(r.idleFor)
	}
	return r.got, false, nil
}

// Finish は、大きさと SHA-256 を確かめて fsync し、開いたままのファイルとその名前を返す。
// 呼ぶ側は、使い終えたら閉じて、rename するか消す。合わなければ一時ファイルを消してエラーを返す。
func (r *Receiver) Finish() (*os.File, string, error) {
	r.stop()
	fail := func(err error) (*os.File, string, error) {
		r.f.Close()
		os.Remove(r.path)
		return nil, "", err
	}
	if r.got != r.want {
		return fail(fmt.Errorf("%w: received %d of %d bytes; the upload was discarded", errTransferBad, r.got, r.want))
	}
	if got := hex.EncodeToString(r.h.Sum(nil)); got != r.sum {
		return fail(fmt.Errorf("%w: the received data has SHA-256 %s, not %s; the upload was discarded", errTransferHash, got[:16], r.sum[:16]))
	}
	if err := r.f.Sync(); err != nil {
		return fail(err)
	}
	return r.f, r.path, nil
}

// Abort は受け取りをやめて、一時ファイルを消す。
func (r *Receiver) Abort() {
	r.stop()
	r.f.Close()
	os.Remove(r.path)
}

func (r *Receiver) stop() {
	if r.timer != nil {
		r.timer.Stop()
	}
}

// removeParts は、前に受け取りの途中で止まったファイル（デーモンの再起動など）を消し、消した名前を返す。
func removeParts(dir string) []string {
	parts, _ := filepath.Glob(filepath.Join(dir, transferPartGlob))
	for _, p := range parts {
		os.Remove(p)
	}
	return parts
}

// ---------- 種類を選べる受け取り（upload_begin、upload_chunk、upload_end、upload_abort） ----------

// UploadKind は、upload_* で受け取れるファイルの種類。
type UploadKind struct {
	// Dir は一時ファイルを置くディレクトリ。終えたあとに rename するなら、置き先と同じファイルシステムにする
	Dir string
	// MaxBytes は 1 つのファイルの大きさの上限
	MaxBytes int64
	// Check は、始める前に断るか決める（空きが足りないなど）。nil なら断らない
	Check func(b UploadBegin) error
	// Done は、大きさと SHA-256 を確かめたファイル（開いたまま）を受け取り、結果を返す。
	// f は Done の中で閉じ、path は rename するか消す
	Done func(b UploadBegin, f *os.File, path string) (any, error)
}

// UploadBegin は upload_begin の引数。
type UploadBegin struct {
	Kind   string
	SHA256 string
	Bytes  int64
	Name   string
	Source string
	Of     string // photo：縦向きに切り抜き直した写真の、もとの写真の id
}

// uploadChunkMax は upload_chunk の 1 回の大きさ（base64 で 128 KiB。1 行の上限に収まる）。
const uploadChunkMax = 96 << 10

// uploadIdle は、upload_chunk が来ないまま、これだけたつと受け取りをやめる時間。テストで短くする。
var uploadIdle = 60 * time.Second

// Uploads は upload_* の受け取り。受け取りは同時に 1 つだけ（別の upload_begin は前のものをやめる）。
type Uploads struct {
	mu    sync.Mutex
	kinds map[string]UploadKind
	cur   *Receiver
	begin UploadBegin
}

func NewUploads() *Uploads { return &Uploads{kinds: map[string]UploadKind{}} }

// Register は種類を足す。
func (u *Uploads) Register(name string, k UploadKind) {
	u.mu.Lock()
	u.kinds[name] = k
	u.mu.Unlock()
	if k.Dir != "" {
		removeParts(k.Dir)
	}
}

// Kinds は受け取れる種類の名前。
func (u *Uploads) Kinds() []string {
	if u == nil {
		return []string{}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := []string{}
	for k := range u.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UploadStart は upload_begin の結果。
type UploadStart struct {
	Upload     string `json:"upload"`
	ChunkBytes int    `json:"chunk_bytes"`
}

// Begin は受け取りを始める。前の受け取りが途中なら、やめる。
func (u *Uploads) Begin(b UploadBegin) (UploadStart, error) {
	if u == nil {
		return UploadStart{}, fmt.Errorf("%w: uploads are not available", errTransferBad)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	k, ok := u.kinds[b.Kind]
	if !ok {
		var names []string
		for n := range u.kinds {
			names = append(names, n)
		}
		return UploadStart{}, fmt.Errorf(`%w: unknown "kind" %q (this daemon accepts: %s)`, errTransferBad, b.Kind, strings.Join(names, ", "))
	}
	if b.Bytes > k.MaxBytes {
		return UploadStart{}, fmt.Errorf("%w: %s files must be at most %d bytes, got %d", errTransferBad, b.Kind, k.MaxBytes, b.Bytes)
	}
	u.abortLocked("")
	if k.Check != nil {
		if err := k.Check(b); err != nil {
			return UploadStart{}, err
		}
	}
	r, err := newReceiver(k.Dir, b.Bytes, b.SHA256, uploadChunkMax, uploadIdle, func(token string) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.cur != nil && u.cur.Token == token {
			vlogf("upload: no data for %v; gave up the %s upload", uploadIdle, u.begin.Kind)
			u.abortLocked("")
		}
	})
	if err != nil {
		return UploadStart{}, err
	}
	b.SHA256 = r.Sum()
	u.cur, u.begin = r, b
	vlogf("upload: receiving %s %q (%d bytes)", b.Kind, b.Name, b.Bytes)
	return UploadStart{Upload: r.Token, ChunkBytes: uploadChunkMax}, nil
}

func (u *Uploads) lookupLocked(token string) (*Receiver, error) {
	if u.cur == nil || token == "" || u.cur.Token != token {
		return nil, fmt.Errorf("%w: upload %q is not in progress (it was finished, aborted, timed out, or replaced by another upload_begin); start again with upload_begin", errTransferUpload, token)
	}
	return u.cur, nil
}

// Chunk は offset から data を書く。
func (u *Uploads) Chunk(token string, offset int64, data []byte) (int64, error) {
	if u == nil {
		return 0, fmt.Errorf("%w: uploads are not available", errTransferUpload)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	r, err := u.lookupLocked(token)
	if err != nil {
		return 0, err
	}
	got, discarded, err := r.Write(offset, data)
	if discarded {
		u.cur = nil
	}
	return got, err
}

// End は受け取りを終え、種類ごとの Done の結果を返す。
func (u *Uploads) End(token string) (any, error) {
	if u == nil {
		return nil, fmt.Errorf("%w: uploads are not available", errTransferUpload)
	}
	u.mu.Lock()
	r, err := u.lookupLocked(token)
	if err != nil {
		u.mu.Unlock()
		return nil, err
	}
	b, k := u.begin, u.kinds[u.begin.Kind]
	u.cur = nil
	u.mu.Unlock()
	f, path, err := r.Finish()
	if err != nil {
		return nil, err
	}
	return k.Done(b, f, path) // 展開などで時間がかかることがあるので、ロックの外で行う
}

// Abort は受け取りをやめる。token が空なら、どの受け取りでもやめる。やめるものがあったかを返す。
func (u *Uploads) Abort(token string) bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.abortLocked(token)
}

func (u *Uploads) abortLocked(token string) bool {
	if u.cur == nil || (token != "" && u.cur.Token != token) {
		return false
	}
	vlogf("upload: %s upload aborted (%d of %d bytes)", u.begin.Kind, u.cur.Got(), u.cur.Want())
	u.cur.Abort()
	u.cur = nil
	return true
}
