package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- バックアップと復元（brain-deck backup、brain-deck restore） ----------
//
// 設定ファイル（config.yaml）と、Brain で作ったデータ（Todo、ウィジェットの状態、上書きの値、メモ、読み物、写真、
// 背景画像など）と、日本語入力の学習（Anthy の ~/.anthy）を、1 つの tar.gz にまとめて PC に渡す。
// 取り直せるもの（予定、天気、天気のアイコン、時刻の記録、PC が送るテキストと QR とグラフ、電池の記録、更新の記録）は入れない。
//
// ファイルの形（tar.gz）：
//   config/config.yaml        設定ファイル（コメントもそのまま）
//   data/<名前>               データの置き場所のファイルとフォルダ（backupItems のものだけ）
//   anthy/<名前>              日本語入力の学習（ime.user の ~/.anthy。lock-file は入れない）
//   manifest.json             最後に置く。形の版、作った時刻、デーモンの版、Brain の名前、ファイルごとの大きさと SHA-256
// manifest.json を最後に置くのは、ファイルを読みながら SHA-256 を数えるため（作っているあいだにデータが書き換わっても、
// manifest の値は入れた中身と合う）。
//
// 復元は、受け取ったファイルを確かめ（形、版、SHA-256、名前、設定ファイルが今のデーモンで使えるか）、今の状態を
// <data-dir>/backup/before-restore.tar.gz に残してから、更新の係（updatehelper.go）がデーモンを止めて入れ替える。
// 新しいデーモンが起動して PC がつなぎ直せたら終わり。起動しなければ、入れ替えたものを元に戻して起動し直す。

const (
	backupFormat     = 1
	backupKindName   = "brain-deck-backup"
	backupDirName    = "backup"                // <data-dir>/backup
	backupBeforeName = "before-restore.tar.gz" // 復元する前の状態（brain-deck restore --undo で戻す）
	backupManifest   = "manifest.json"
	backupMaxBytes   = 192 << 20 // 受け取るバックアップの上限（gzip）
	backupMaxRaw     = 512 << 20 // 戻した中身の合計の上限
	backupMaxFiles   = 20000
	backupChunk      = 96 << 10
	backupIdle       = 10 * time.Minute // 作ったバックアップを、読まれなくなってから消すまで
	backupNewDir     = ".restore-new"   // 係が展開する場所（backup の下）
	backupOldDir     = ".restore-old"   // 係が入れ替えたときに、前のものを置く場所
)

// backupItem は、データの置き場所のうち、バックアップに入れるもの 1 つ。
type backupItem struct {
	Name  string
	Dir   bool
	Label string
}

// backupItems は、バックアップに入れるデータ（並ぶ順）。復元すると、これらはバックアップのとおりになる
// （バックアップにないものは、今のものを消す。消したものは before-restore.tar.gz に残る）。
var backupItems = []backupItem{
	{todoStoreName + ".json", false, "Todo"},
	{gadgetStoreName + ".json", false, "ウィジェットの状態"},
	{calcStoreName + ".json", false, "電卓"},
	{displayStoreName + ".json", false, "画面の向き"},
	{overridesName, false, "上書きの値"},
	{touchCalName, false, "タッチの調整"},
	{profileStateName, false, "PC ごとの設定の状態"},
	{"memo", true, "手書きメモ"},
	{"reader", true, "読み物"},
	{"photos", true, "写真"},
	{"images", true, "背景画像"},
	{appsDirName, true, "自分で足したアプリ"},
	{appsDataDirName, true, "アプリのデータ"},
}

// BackupManifest は manifest.json。
type BackupManifest struct {
	Format  int          `json:"format"`
	Kind    string       `json:"kind"`
	Created time.Time    `json:"created"`
	Version string       `json:"version"`           // 作ったデーモンの版
	Host    string       `json:"host"`              // Brain の名前（hostname）
	Profile string       `json:"profile,omitempty"` // 作ったときの PC ごとの設定
	Files   []BackupFile `json:"files"`
}

// BackupFile は、バックアップの中のファイル 1 つ。
type BackupFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// backupSources は、バックアップを作るときに読む場所。
type backupSources struct {
	DataDir  string
	CfgPath  string // 空なら設定ファイルを入れない
	AnthyDir string // 空なら日本語入力の学習を入れない
	Version  string
	Host     string
	Profile  string
	Now      time.Time
}

// anthyFileRE は、~/.anthy のうち、バックアップに入れるファイル（学習の記録）。
var anthyFileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func anthyFileOK(name string) bool {
	return anthyFileRE.MatchString(name) && !strings.HasPrefix(name, "lock-file") && len(name) <= 100
}

// createBackup は、src のファイルを tar.gz にして w に書く。
func createBackup(w io.Writer, src backupSources) (*BackupManifest, error) {
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed) // ARM926 では、圧縮の強さより時間を選ぶ
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	m := &BackupManifest{Format: backupFormat, Kind: backupKindName, Created: src.Now.UTC().Truncate(time.Second),
		Version: src.Version, Host: src.Host, Profile: src.Profile, Files: []BackupFile{}}
	add := func(name, file string) error {
		f, err := os.Open(file)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // 読むあいだに消えた
			}
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return err
		}
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: st.Size(), ModTime: st.ModTime().Truncate(time.Second), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, st.Size()))
		if err != nil {
			return err
		}
		if n != st.Size() { // 読むあいだに短くなった：tar の大きさに合わせて 0 で埋める（manifest には入れた中身の値）
			pad := make([]byte, st.Size()-n)
			tw.Write(pad)
			h.Write(pad)
			n = st.Size()
		}
		m.Files = append(m.Files, BackupFile{Path: name, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	}
	if src.CfgPath != "" {
		if err := add("config/config.yaml", src.CfgPath); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
	}
	for _, it := range backupItems {
		p := filepath.Join(src.DataDir, it.Name)
		if !it.Dir {
			if err := add("data/"+it.Name, p); err != nil {
				return nil, fmt.Errorf("%s: %w", it.Name, err)
			}
			continue
		}
		var files []string
		err := filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if strings.HasPrefix(d.Name(), ".") && fp != p { // 一時ファイル（.upload-*.part、.*.tmp*）
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				files = append(files, fp)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", it.Name, err)
		}
		sort.Strings(files)
		for _, fp := range files {
			rel, _ := filepath.Rel(src.DataDir, fp)
			if err := add("data/"+filepath.ToSlash(rel), fp); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
		}
	}
	if src.AnthyDir != "" {
		ents, _ := os.ReadDir(src.AnthyDir)
		for _, e := range ents {
			if e.Type().IsRegular() && anthyFileOK(e.Name()) {
				if err := add("anthy/"+e.Name(), filepath.Join(src.AnthyDir, e.Name())); err != nil {
					return nil, fmt.Errorf("anthy: %w", err)
				}
			}
		}
	}
	mb, _ := json.MarshalIndent(m, "", " ")
	if err := tw.WriteHeader(&tar.Header{Name: backupManifest, Mode: 0o644, Size: int64(len(mb)), ModTime: m.Created, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(mb); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return m, nil
}

// backupItemOf は、バックアップの中の data/ のファイルが、どの backupItem か（なければ nil）。
func backupItemOf(rel string) *backupItem {
	top, rest, sub := strings.Cut(rel, "/")
	for i := range backupItems {
		it := &backupItems[i]
		if it.Name == top && it.Dir == sub && (!sub || rest != "") {
			return it
		}
	}
	return nil
}

// validBackupPath は、バックアップの中のファイルの名前として使えるか（展開しても置き場所の外に出ないか）。
func validBackupPath(p string) bool {
	if p == "" || len(p) > 255 || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	switch top, rest, _ := strings.Cut(p, "/"); top {
	case "config":
		return rest == "config.yaml"
	case "anthy":
		return anthyFileOK(rest)
	case "data":
		return backupItemOf(rest) != nil
	}
	return false
}

// errBackupBad は、バックアップのファイルとして使えないこと。
var errBackupBad = errors.New("not a usable backup")

// readBackup は、バックアップ（tar.gz）を読み、ファイルごとに visit を呼ぶ（visit は中身を読み切らなくてよい）。
// 名前、大きさ、SHA-256 を manifest.json と比べ、合わなければ errBackupBad を返す。
func readBackup(r io.Reader, visit func(f BackupFile, data io.Reader) error) (*BackupManifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("%w: not a gzip file (%v)", errBackupBad, err)
	}
	tr := tar.NewReader(gz)
	got := map[string]BackupFile{}
	var order []string
	var m *BackupManifest
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errBackupBad, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%w: %s is not a regular file", errBackupBad, hdr.Name)
		}
		if m != nil {
			return nil, fmt.Errorf("%w: %s comes after manifest.json", errBackupBad, hdr.Name)
		}
		if hdr.Name == backupManifest {
			if hdr.Size > 8<<20 {
				return nil, fmt.Errorf("%w: manifest.json is too large", errBackupBad)
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", errBackupBad, err)
			}
			m = &BackupManifest{}
			if err := json.Unmarshal(b, m); err != nil {
				return nil, fmt.Errorf("%w: manifest.json: %v", errBackupBad, err)
			}
			continue
		}
		if !validBackupPath(hdr.Name) {
			return nil, fmt.Errorf("%w: unexpected file %q", errBackupBad, hdr.Name)
		}
		if _, dup := got[hdr.Name]; dup {
			return nil, fmt.Errorf("%w: %s appears twice", errBackupBad, hdr.Name)
		}
		if total += hdr.Size; total > backupMaxRaw || len(got) >= backupMaxFiles {
			return nil, fmt.Errorf("%w: too large (more than %d files or %d MiB)", errBackupBad, backupMaxFiles, backupMaxRaw>>20)
		}
		h := sha256.New()
		cr := &countingReader{r: io.TeeReader(tr, h)}
		f := BackupFile{Path: hdr.Name, Bytes: hdr.Size}
		if visit != nil {
			if err := visit(f, cr); err != nil {
				return nil, err
			}
		}
		if _, err := io.Copy(io.Discard, cr); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", errBackupBad, hdr.Name, err)
		}
		f.Bytes, f.SHA256 = cr.n, hex.EncodeToString(h.Sum(nil))
		got[hdr.Name] = f
		order = append(order, hdr.Name)
	}
	if m == nil {
		return nil, fmt.Errorf("%w: manifest.json is missing (the file may be cut short)", errBackupBad)
	}
	if m.Kind != backupKindName {
		return nil, fmt.Errorf("%w: manifest.json is not a brain-deck backup (kind %q)", errBackupBad, m.Kind)
	}
	if m.Format < 1 || m.Format > backupFormat {
		return nil, fmt.Errorf("%w: format %d is not supported by this lefthand (%d); update lefthand first", errBackupBad, m.Format, backupFormat)
	}
	if len(m.Files) != len(got) {
		return nil, fmt.Errorf("%w: manifest.json lists %d files, the archive has %d", errBackupBad, len(m.Files), len(got))
	}
	for _, f := range m.Files {
		g, ok := got[f.Path]
		if !ok {
			return nil, fmt.Errorf("%w: %s is listed in manifest.json but missing", errBackupBad, f.Path)
		}
		if g.Bytes != f.Bytes || g.SHA256 != f.SHA256 {
			return nil, fmt.Errorf("%w: %s does not match manifest.json (damaged?)", errBackupBad, f.Path)
		}
	}
	return m, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// BackupSummary は、バックアップの中身のまとめ（確かめた結果。PC に見せる）。
type BackupSummary struct {
	Created  time.Time       `json:"created"`
	Version  string          `json:"version"`
	Host     string          `json:"host"`
	Profile  string          `json:"profile,omitempty"`
	Config   bool            `json:"config"`             // 設定ファイルが入っている
	Anthy    int             `json:"anthy"`              // 日本語入力の学習のファイルの数
	Items    []BackupItemSum `json:"items"`              // データ（backupItems の順。入っていないものも）
	Bytes    int64           `json:"bytes"`              // 中身の合計
	Problems []string        `json:"problems,omitempty"` // 設定ファイルが今のデーモンで使えないなど（config を戻せない）
}

// BackupItemSum は、データ 1 つのまとめ。
type BackupItemSum struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

func summarize(m *BackupManifest) BackupSummary {
	s := BackupSummary{Created: m.Created, Version: m.Version, Host: m.Host, Profile: m.Profile}
	idx := map[string]int{}
	for _, it := range backupItems {
		idx[it.Name] = len(s.Items)
		s.Items = append(s.Items, BackupItemSum{Name: it.Name, Label: it.Label})
	}
	for _, f := range m.Files {
		s.Bytes += f.Bytes
		top, rest, _ := strings.Cut(f.Path, "/")
		switch top {
		case "config":
			s.Config = true
		case "anthy":
			s.Anthy++
		case "data":
			if it := backupItemOf(rest); it != nil {
				i := idx[it.Name]
				s.Items[i].Files++
				s.Items[i].Bytes += f.Bytes
			}
		}
	}
	return s
}

// ---------- デーモンの係 ----------

// BackupService は、バックアップを作って渡すことと、復元を受け取って係に渡すことを受け持つ。
type BackupService struct {
	mu      sync.Mutex
	dataDir string
	cfgPath string
	store   *configStore
	updater *Updater
	flush   func()        // まだ書いていないデータを書く（メモの線、しおりなど）
	anthy   func() string // 日本語入力の学習の場所（~/.anthy。分からなければ空）
	host    func() string // Brain の名前
	profile func() string // 今の PC ごとの設定
	stopIME func()        // 復元の前に uim-fep を止める（学習を書き換えるので）
	now     func() time.Time
	out     *backupOut
	staged  map[string]*backupStaged
}

type backupOut struct {
	token string
	path  string
	bytes int64
	sum   string
	timer *time.Timer
}

type backupStaged struct {
	path string
	sum  BackupSummary
}

func NewBackupService(dataDir, cfgPath string) *BackupService {
	return &BackupService{dataDir: dataDir, cfgPath: cfgPath, now: time.Now, staged: map[string]*backupStaged{},
		host: func() string { h, _ := os.Hostname(); return h }}
}

func (b *BackupService) dir() string { return filepath.Join(b.dataDir, backupDirName) }

func (b *BackupService) sources() backupSources {
	src := backupSources{DataDir: b.dataDir, CfgPath: b.cfgPath, Version: daemonVersion(), Now: b.now()}
	if b.host != nil {
		src.Host = b.host()
	}
	if b.anthy != nil {
		src.AnthyDir = b.anthy()
	}
	if b.profile != nil {
		src.Profile = b.profile()
	}
	return src
}

// writeArchive は、今の状態のバックアップを path に作る（一時ファイルに書いてから置き換える）。
func (b *BackupService) writeArchive(p string) (*BackupManifest, int64, string, error) {
	if b.flush != nil {
		b.flush()
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, 0, "", err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".backup-*.tmp")
	if err != nil {
		return nil, 0, "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	os.Chmod(tmp, 0o600) // 個人のデータが入る
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h)}
	m, err := createBackup(cw, b.sources())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, 0, "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return nil, 0, "", err
	}
	return m, cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Create は、バックアップを作って、読み出せるようにする（backup_create）。前に作ったものは消す。
func (b *BackupService) Create() (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropOutLocked()
	if free, size, err := statfsFree(b.dataDir); err == nil {
		// 中身の合計（gzip する前）より空きが少なければ断る（背景画像と写真はほとんど縮まない）
		var used int64
		filepath.WalkDir(b.dataDir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if in, err := d.Info(); err == nil {
					used += in.Size()
				}
			}
			return nil
		})
		if reserve := min(int64(64<<20), size/10); free-used < reserve {
			return nil, fmt.Errorf("%w: %s has %s free; making a backup of about %s would leave less than %s",
				errImageNoSpace, b.dataDir, kib(free), kib(used), kib(reserve))
		}
	}
	t0 := b.now()
	token := newToken()
	p := filepath.Join(b.dir(), ".out-"+token+".tar.gz")
	m, n, sum, err := b.writeArchive(p)
	if err != nil {
		os.Remove(p)
		return nil, err
	}
	o := &backupOut{token: token, path: p, bytes: n, sum: sum}
	o.timer = time.AfterFunc(backupIdle, func() {
		b.mu.Lock()
		if b.out == o {
			b.dropOutLocked()
		}
		b.mu.Unlock()
	})
	b.out = o
	s := summarize(m)
	log.Printf("backup: made %s (%d files, %s, %v)", token, len(m.Files), kib(n), b.now().Sub(t0).Round(100*time.Millisecond))
	return map[string]any{"backup": token, "bytes": n, "sha256": sum, "chunk_bytes": backupChunk, "summary": s,
		"name": "brain-" + m.Created.Local().Format("20060102-150405") + ".tar.gz"}, nil
}

func (b *BackupService) dropOutLocked() {
	if b.out != nil {
		b.out.timer.Stop()
		os.Remove(b.out.path)
		b.out = nil
	}
	// 前のデーモンが残したもの
	if ents, err := os.ReadDir(b.dir()); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".out-") || strings.HasPrefix(e.Name(), ".backup-") {
				os.Remove(filepath.Join(b.dir(), e.Name()))
			}
		}
	}
}

// Read は、作ったバックアップの offset から読む（backup_read）。
func (b *BackupService) Read(token string, offset int64, length int) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o := b.out
	if o == nil || o.token != token {
		return nil, fmt.Errorf("%w: backup %q is not available (make it again with backup_create)", errUpdateGone, token)
	}
	if length <= 0 || length > backupChunk {
		length = backupChunk
	}
	if offset < 0 || offset > o.bytes {
		return nil, fmt.Errorf("%w: offset %d is outside 0..%d", errUpdateBad, offset, o.bytes)
	}
	f, err := os.Open(o.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, min(int64(length), o.bytes-offset))
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, err
	}
	o.timer.Reset(backupIdle)
	return map[string]any{"backup": token, "offset": offset, "total": o.bytes, "data": base64.StdEncoding.EncodeToString(buf)}, nil
}

// Done は、読み終えたバックアップを消す（backup_done）。
func (b *BackupService) Done(token string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.out == nil || b.out.token != token {
		return false
	}
	b.dropOutLocked()
	return true
}

// UploadKind は、upload_* の kind: backup（復元するバックアップを受け取る）。
func (b *BackupService) UploadKind() UploadKind {
	return UploadKind{Dir: b.dir(), MaxBytes: backupMaxBytes,
		Check: func(ub UploadBegin) error {
			if b.updater != nil {
				if err := b.updater.canChange(); err != nil {
					return err
				}
			}
			if free, size, err := statfsFree(b.dataDir); err == nil {
				// 受け取ったファイル、展開したもの（gzip のおよそ 1〜2 倍）、復元の前の状態が同時に置かれる
				if reserve := min(int64(64<<20), size/10); free-ub.Bytes*4 < reserve {
					return fmt.Errorf("%w: %s has %s free; restoring a backup of %s needs about %s",
						errImageNoSpace, b.dataDir, kib(free), kib(ub.Bytes), kib(ub.Bytes*4+reserve))
				}
			}
			return nil
		},
		Done: b.receive}
}

// receive は、受け取ったバックアップを確かめて、復元できるように置く。結果はまとめと、復元の名前（restore_apply に渡す）。
func (b *BackupService) receive(ub UploadBegin, f *os.File, p string) (any, error) {
	defer f.Close()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		os.Remove(p)
		return nil, err
	}
	var cfgRaw []byte
	m, err := readBackup(f, func(bf BackupFile, r io.Reader) error {
		switch {
		case bf.Path == "config/config.yaml":
			b, err := io.ReadAll(io.LimitReader(r, 4<<20))
			cfgRaw = b
			return err
		case strings.HasSuffix(bf.Path, ".json") && strings.HasPrefix(bf.Path, "data/") && bf.Bytes < 32<<20:
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			if !json.Valid(b) {
				return fmt.Errorf("%w: %s is not valid JSON", errBackupBad, bf.Path)
			}
		}
		return nil
	})
	if err != nil {
		os.Remove(p)
		return nil, fmt.Errorf("%w: %v", errUpdateCheck, err)
	}
	sum := summarize(m)
	if cfgRaw != nil && b.store != nil {
		_, eff, _, _, err := b.store.check(cfgRaw)
		if err == nil {
			if rp := restartProblems(b.store.Current(), eff); len(rp) > 0 {
				err = rp
			}
		}
		if err != nil {
			for _, pr := range asProblems(err) {
				sum.Problems = append(sum.Problems, strings.TrimPrefix(pr.Path+": ", ": ")+pr.Message)
			}
		}
	}
	token := newToken()
	dst := filepath.Join(b.dir(), "staged-"+token+".tar.gz")
	if err := os.Rename(p, dst); err != nil {
		os.Remove(p)
		return nil, err
	}
	os.Chmod(dst, 0o600)
	b.mu.Lock()
	for t, s := range b.staged { // 確かめた復元は 1 つだけ
		os.Remove(s.path)
		delete(b.staged, t)
	}
	b.staged[token] = &backupStaged{path: dst, sum: sum}
	b.mu.Unlock()
	log.Printf("backup: received a backup from %s (%d files, made %s by %s)", ub.Source, len(m.Files), m.Created.Local().Format(time.DateTime), m.Version)
	return map[string]any{"restore": token, "summary": sum}, nil
}

// RestoreOptions は、復元で戻すもの。
type RestoreOptions struct {
	Config bool `json:"config"`
	Data   bool `json:"data"`
	Anthy  bool `json:"anthy"`
}

// Apply は、確かめたバックアップ（token）か、復元の前の状態（undo）を戻すよう、係に頼む。
// 今の状態を before-restore.tar.gz に残してから頼む。
func (b *BackupService) Apply(token string, undo bool, opt RestoreOptions) (*updatePlan, BackupSummary, error) {
	if b.updater == nil {
		return nil, BackupSummary{}, fmt.Errorf("%w: lefthand is not running as a systemd service", errNoService)
	}
	if err := b.updater.canChange(); err != nil {
		return nil, BackupSummary{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var src string
	var sum BackupSummary
	if undo {
		src = filepath.Join(b.dir(), backupBeforeName)
		f, err := os.Open(src)
		if err != nil {
			return nil, sum, fmt.Errorf("%w: there is no state saved before a restore (%v)", errUpdateGone, err)
		}
		m, err := readBackup(f, nil)
		f.Close()
		if err != nil {
			return nil, sum, fmt.Errorf("%w: %v", errUpdateCheck, err)
		}
		sum = summarize(m)
	} else {
		st, ok := b.staged[token]
		if !ok {
			return nil, sum, fmt.Errorf("%w: restore %q is not ready (send the backup again)", errUpdateGone, token)
		}
		src, sum = st.path, st.sum
	}
	if opt.Config && sum.Config && len(sum.Problems) > 0 {
		return nil, sum, fmt.Errorf("%w: the config in the backup cannot be used by this lefthand (%s); restore without the config", errUpdateCheck, strings.Join(sum.Problems, "; "))
	}
	if !opt.Config && !opt.Data && !opt.Anthy {
		return nil, sum, fmt.Errorf("%w: nothing to restore", errUpdateBad)
	}
	// 係が使う場所に写す（undo のときは、これから before-restore.tar.gz を作り直すので先に写す）
	stage := newToken()
	stageDir := filepath.Join(b.updater.Dir, stage)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return nil, sum, err
	}
	archive := filepath.Join(stageDir, "backup.tar.gz")
	if err := copyFile(src, archive, 0o600); err != nil {
		os.RemoveAll(stageDir)
		return nil, sum, err
	}
	// 今の状態を残す
	if _, _, _, err := b.writeArchive(filepath.Join(b.dir(), backupBeforeName)); err != nil {
		os.RemoveAll(stageDir)
		return nil, sum, fmt.Errorf("saving the current state before restoring: %w", err)
	}
	rp := &restorePlan{Archive: archive, DataDir: b.dataDir, CfgPath: b.cfgPath, RestoreOptions: opt}
	if b.anthy != nil {
		rp.AnthyDir = b.anthy()
	}
	if rp.AnthyDir != "" {
		if st, err := os.Stat(filepath.Dir(rp.AnthyDir)); err == nil {
			rp.UID, rp.GID = fileOwner(st)
		}
	}
	if b.stopIME != nil {
		b.stopIME()
	}
	p, err := b.updater.startRestore(stage, rp)
	if err != nil {
		os.RemoveAll(stageDir)
		return nil, sum, err
	}
	if !undo {
		os.Remove(src)
		delete(b.staged, token)
	}
	what := "backup " + token
	if undo {
		what = "the state before the last restore"
	}
	log.Printf("backup: restoring %s (config %v, data %v, anthy %v)", what, opt.Config, opt.Data, opt.Anthy)
	return p, sum, nil
}

// Info は get_status の backup：復元の前の状態があるか。
func (b *BackupService) Info() map[string]any {
	if b == nil {
		return nil
	}
	res := map[string]any{"items": backupItemNames()}
	if st, err := os.Stat(filepath.Join(b.dir(), backupBeforeName)); err == nil {
		res["before_restore"] = map[string]any{"at": st.ModTime(), "bytes": st.Size()}
	}
	return res
}

func backupItemNames() []string {
	var out []string
	for _, it := range backupItems {
		out = append(out, it.Name)
	}
	return out
}

// anthyDirOf は、user の ~/.anthy（ユーザーがいなければ空）。
func anthyDirOf(name string) string {
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir == "" || u.Uid == "0" {
		return ""
	}
	return filepath.Join(u.HomeDir, ".anthy")
}

// ---------- 係が行う入れ替え（デーモンを止めているあいだ） ----------

// restorePlan は、係（updatehelper.go）に渡す復元の指示。
type restorePlan struct {
	Archive  string `json:"archive"`
	DataDir  string `json:"data_dir"`
	CfgPath  string `json:"config_path"`
	AnthyDir string `json:"anthy_dir,omitempty"`
	UID      int    `json:"uid"`
	GID      int    `json:"gid"`
	RestoreOptions
}

// restoreUndo は、入れ替えたものを元に戻す手順。
type restoreUndo struct {
	p      restorePlan
	moved  []string          // データの置き場所の名前（.restore-old に動かしたもの、新しいものを入れたもの）
	oldCfg []byte            // 入れ替える前の設定ファイル（nil なら変えていない）
	anthy  map[string][]byte // 入れ替える前の学習のファイル（nil の値は、前はなかった）
}

func (p restorePlan) newDir() string { return filepath.Join(p.DataDir, backupDirName, backupNewDir) }
func (p restorePlan) oldDir() string { return filepath.Join(p.DataDir, backupDirName, backupOldDir) }

// applyRestore は、バックアップを展開して入れ替える。失敗したら、それまでに入れ替えたものを戻してから返す。
// 成功したら、元に戻す手順を返す（新しいデーモンが起動しなかったときに使う。起動したら finish で片付ける）。
func applyRestore(p restorePlan) (*restoreUndo, error) {
	nd, od := p.newDir(), p.oldDir()
	if _, err := os.Stat(od); err == nil {
		return nil, fmt.Errorf("%s exists (left over from an interrupted restore); check it and remove it first", od)
	}
	os.RemoveAll(nd)
	if err := os.MkdirAll(nd, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(p.Archive)
	if err != nil {
		return nil, err
	}
	_, err = readBackup(f, func(bf BackupFile, r io.Reader) error {
		dst := filepath.Join(nd, filepath.FromSlash(bf.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		if err == nil {
			err = w.Sync()
		}
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		return err
	})
	f.Close()
	if err != nil {
		os.RemoveAll(nd)
		return nil, err
	}
	u := &restoreUndo{p: p}
	fail := func(err error) (*restoreUndo, error) {
		if uerr := u.undo(); uerr != nil {
			err = fmt.Errorf("%v; undoing also failed: %v", err, uerr)
		}
		return nil, err
	}
	if err := os.MkdirAll(od, 0o700); err != nil {
		os.RemoveAll(nd)
		return nil, err
	}
	if p.Data {
		for _, it := range backupItems {
			cur := filepath.Join(p.DataDir, it.Name)
			next := filepath.Join(nd, "data", it.Name)
			_, curErr := os.Lstat(cur)
			_, nextErr := os.Lstat(next)
			if curErr != nil && nextErr != nil {
				continue // どちらにもない
			}
			u.moved = append(u.moved, it.Name)
			if curErr == nil {
				if err := os.Rename(cur, filepath.Join(od, it.Name)); err != nil {
					return fail(err)
				}
			}
			if nextErr == nil {
				if err := os.Rename(next, cur); err != nil {
					return fail(err)
				}
			}
		}
	}
	if p.Config && p.CfgPath != "" {
		if b, err := os.ReadFile(filepath.Join(nd, "config", "config.yaml")); err == nil {
			old, err := os.ReadFile(p.CfgPath)
			if err != nil {
				return fail(err)
			}
			u.oldCfg = old
			if err := writeFileAtomic(p.CfgPath, b, true); err != nil {
				return fail(err)
			}
		}
	}
	if p.Anthy && p.AnthyDir != "" {
		ents, _ := os.ReadDir(filepath.Join(nd, "anthy"))
		if len(ents) > 0 {
			if err := os.MkdirAll(p.AnthyDir, 0o700); err != nil {
				return fail(err)
			}
			os.Lchown(p.AnthyDir, p.UID, p.GID)
			u.anthy = map[string][]byte{}
			for _, e := range ents {
				dst := filepath.Join(p.AnthyDir, e.Name())
				old, err := os.ReadFile(dst)
				if err != nil {
					old = nil
				}
				u.anthy[e.Name()] = old
				b, err := os.ReadFile(filepath.Join(nd, "anthy", e.Name()))
				if err != nil {
					return fail(err)
				}
				if err := writeOwned(dst, b, p.UID, p.GID); err != nil {
					return fail(err)
				}
			}
		}
	}
	return u, nil
}

// writeOwned は、path を data にし、持ち主を uid、gid にする（日本語入力の学習は user のファイル）。
func writeOwned(p string, data []byte, uid, gid int) error {
	if err := writeFileAtomic(p, data, false); err != nil {
		return err
	}
	os.Chmod(p, 0o600)
	return os.Lchown(p, uid, gid)
}

// undo は、入れ替えたものを元に戻し、展開したものを消す。
func (u *restoreUndo) undo() error {
	var errs []string
	p := u.p
	od := p.oldDir()
	for i := len(u.moved) - 1; i >= 0; i-- {
		name := u.moved[i]
		cur := filepath.Join(p.DataDir, name)
		old := filepath.Join(od, name)
		if _, err := os.Lstat(cur); err == nil {
			if err := os.RemoveAll(cur); err != nil {
				errs = append(errs, err.Error())
				continue
			}
		}
		if _, err := os.Lstat(old); err == nil {
			if err := os.Rename(old, cur); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if u.oldCfg != nil {
		if err := writeFileAtomic(p.CfgPath, u.oldCfg, false); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for name, old := range u.anthy {
		dst := filepath.Join(p.AnthyDir, name)
		if old == nil {
			os.Remove(dst)
		} else if err := writeOwned(dst, old, p.UID, p.GID); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; ")) // .restore-old は残す（手で戻せるように）
	}
	os.RemoveAll(od)
	os.RemoveAll(p.newDir())
	return nil
}

// finish は、新しいデーモンが起動したあとに、前のものと展開したものを消す。
func (u *restoreUndo) finish() {
	os.RemoveAll(u.p.oldDir())
	os.RemoveAll(u.p.newDir())
}

// restoreItemsLabel は、戻したものの短い説明（ログと係の知らせ）。
func restoreItemsLabel(o RestoreOptions) string {
	var s []string
	if o.Config {
		s = append(s, "config")
	}
	if o.Data {
		s = append(s, "data")
	}
	if o.Anthy {
		s = append(s, "anthy")
	}
	return strings.Join(s, "+")
}

// fileOwner は、ファイルの持ち主の uid と gid。
func fileOwner(st os.FileInfo) (int, int) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid), int(s.Gid)
	}
	return 0, 0
}
