package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- ファイル（見られる場所と消せる場所） ----------
//
// デーモンは root で動いているので、ファイルを見る・消す・出し入れする場所を決めておく（設定の files.roots）。
// 既定は、/home/user（見る、消す、PC から足す）と、brain-deck のデータのうち写真、手書きメモ、読み物（見る、消す）。
// システムのフォルダは見せない。場所は設定で変えられるが、消せる場所（delete: true）は /home、/media、/mnt、/tmp の下と、
// データの写真、メモ、読み物だけ。/boot、/etc などを消せる設定は受け付けない（見るだけなら書ける）。
// 写真、メモ、読み物のフォルダのファイルは、それぞれの係（PhotoStore、MemoService、ReadStore）を通して消す
// （index.json などの、ほかのファイルは消さない）。PC から足すのは、ふつうのフォルダ（データのフォルダでない）だけ。
// シンボリックリンクはたどらない（場所の外へ出ないため）。

const (
	filesDataPhotos = "photos"
	filesDataMemo   = "memo"
	filesDataReader = "reader"
	filesListMax    = 2000
	filesChunkMax   = 96 << 10
	filesPutMax     = 64 << 20
	filesDuLimit    = 8 * time.Second
)

// FilesConfig は設定の files。
type FilesConfig struct {
	Roots []FilesRoot `yaml:"roots,omitempty" json:"roots,omitempty"`
}

// FilesRoot は、見られる場所 1 つ。
type FilesRoot struct {
	Name   string `yaml:"name" json:"name"`
	Path   string `yaml:"path" json:"path"`
	Delete bool   `yaml:"delete,omitempty" json:"delete,omitempty"` // 消せる（と、PC から足せる）
}

// 消せる場所に書けるフォルダ（の下）
var filesDeletable = []string{"/home/", "/media/", "/mnt/", "/tmp/"}

// filesNever は、見る場所にも書けないフォルダ（中身を見せると危ない、意味がない）。
var filesNever = []string{"/proc", "/sys", "/dev", "/run"}

// filesHidden は、場所の中にあっても、出さず、読ませず、消させないもの（鍵とパスワード、起動の領域）。
var (
	filesHiddenPaths = []string{"/boot", "/root", "/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-", "/etc/sudoers", "/etc/sudoers.d", "/etc/ssh"}
	filesHiddenNames = map[string]bool{".ssh": true, ".gnupg": true}
)

func filesHiddenPath(path string) bool {
	for _, h := range filesHiddenPaths {
		if path == h || strings.HasPrefix(path, h+"/") {
			return true
		}
	}
	for _, part := range strings.Split(path, "/") {
		if filesHiddenNames[part] {
			return true
		}
	}
	return false
}

func defaultFilesRoots(dataDir string) []FilesRoot {
	return []FilesRoot{
		{Name: "ホーム", Path: "/home/user", Delete: true},
		{Name: "写真", Path: filepath.Join(dataDir, filesDataPhotos), Delete: true},
		{Name: "メモ", Path: filepath.Join(dataDir, filesDataMemo), Delete: true},
		{Name: "読み物", Path: filepath.Join(dataDir, filesDataReader), Delete: true},
	}
}

// validate は files の誤りを返す。
func (c *FilesConfig) validate(dataDir string, fail func(path, format string, args ...any)) {
	if c == nil {
		return
	}
	seen := map[string]bool{}
	for i, r := range c.Roots {
		p := fmt.Sprintf("/files/roots/%d", i)
		if strings.TrimSpace(r.Name) == "" || seen[r.Name] {
			fail(p, "files.roots[%d]: name must be distinct and not empty", i)
		}
		seen[r.Name] = true
		if !filepath.IsAbs(r.Path) || filepath.Clean(r.Path) != r.Path {
			fail(p, "files.roots[%d]: path must be a clean absolute path, got %q", i, r.Path)
			continue
		}
		never := r.Path == "/" || filesHiddenPath(r.Path)
		for _, n := range filesNever {
			never = never || r.Path == n || strings.HasPrefix(r.Path, n+"/")
		}
		if never {
			fail(p, "files.roots[%d]: %s cannot be shown", i, r.Path)
		}
		if r.Delete && !filesDeleteOK(r.Path, dataDir) {
			fail(p, "files.roots[%d]: files under %s cannot be deleted (delete: true is allowed only under %s and the photos, memo and reader data)",
				i, r.Path, strings.Join(filesDeletable, ", "))
		}
	}
}

// filesDeleteOK は、path の下を消してよいか。
func filesDeleteOK(path, dataDir string) bool {
	for _, d := range filesDeletable {
		if strings.HasPrefix(path+"/", d) && path+"/" != d {
			return true
		}
	}
	for _, d := range []string{filesDataPhotos, filesDataMemo, filesDataReader} {
		if path == filepath.Join(dataDir, d) {
			return true
		}
	}
	return false
}

// FileEntry は、フォルダの中の 1 つ。
type FileEntry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Dir   bool      `json:"dir,omitempty"`
	Link  bool      `json:"link,omitempty"` // シンボリックリンク（たどらない）
	Size  int64     `json:"size"`           // ファイルの大きさ。du のときはフォルダの中の合計
	MTime time.Time `json:"mtime"`
}

// FileList は list_files の結果。
type FileList struct {
	Path      string      `json:"path"`             // 空なら場所の一覧
	Root      string      `json:"root,omitempty"`   // 場所の名前
	Delete    bool        `json:"delete,omitempty"` // 消せる
	Put       bool        `json:"put,omitempty"`    // PC から足せる
	Parent    string      `json:"parent,omitempty"`
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
	Partial   bool        `json:"partial,omitempty"` // du が時間内に終わらなかった
	Free      int64       `json:"free_bytes"`
	Total     int64       `json:"total_bytes"`
}

var (
	errFilesOutside = errors.New("outside the allowed places")
	errFilesDenied  = errors.New("not allowed here")
	errFilesMissing = errors.New("no such file")
)

// FilesService は、見られる場所と、写真・メモ・読み物の係。
type FilesService struct {
	mu      sync.Mutex
	roots   []FilesRoot
	dataDir string
	photos  *PhotoStore
	memos   *MemoService
	reads   *ReadStore
	changed func()
}

func NewFilesService(dataDir string, photos *PhotoStore, memos *MemoService, reads *ReadStore) *FilesService {
	return &FilesService{roots: defaultFilesRoots(dataDir), dataDir: dataDir, photos: photos, memos: memos, reads: reads}
}

// SetConfig は、設定の files.roots にする（なければ既定）。
func (s *FilesService) SetConfig(c *FilesConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil || len(c.Roots) == 0 {
		s.roots = defaultFilesRoots(s.dataDir)
		return
	}
	s.roots = append([]FilesRoot(nil), c.Roots...)
}

// SetChanged は、PC から足したり消したりしたときに呼ぶ関数（画面の一覧を読み直す）。
func (s *FilesService) SetChanged(f func()) { s.mu.Lock(); s.changed = f; s.mu.Unlock() }

func (s *FilesService) Roots() []FilesRoot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]FilesRoot(nil), s.roots...)
}

// resolve は、path がどの場所の中かを決める（シンボリックリンクはたどらない。.. は使えない）。
func (s *FilesService) resolve(path string) (FilesRoot, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "\x00") {
		return FilesRoot{}, "", fmt.Errorf("%w: %q is not a clean absolute path", errFilesOutside, path)
	}
	best := FilesRoot{}
	for _, r := range s.Roots() {
		if (path == r.Path || strings.HasPrefix(path, r.Path+"/")) && len(r.Path) > len(best.Path) {
			best = r
		}
	}
	if best.Path == "" || filesHiddenPath(path) {
		return FilesRoot{}, "", fmt.Errorf("%w: %s", errFilesOutside, path)
	}
	// 途中のフォルダにシンボリックリンクがあれば、場所の外へ出るかもしれないので断る
	rel, _ := filepath.Rel(best.Path, path)
	cur := best.Path
	if rel != "." {
		for _, part := range strings.Split(rel, "/") {
			cur = filepath.Join(cur, part)
			if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 && cur != path {
				return FilesRoot{}, "", fmt.Errorf("%w: %s goes through a symbolic link", errFilesOutside, path)
			}
		}
	}
	return best, rel, nil
}

func (s *FilesService) isData(path string) string {
	for _, d := range []string{filesDataPhotos, filesDataMemo, filesDataReader} {
		if filepath.Dir(path) == filepath.Join(s.dataDir, d) {
			return d
		}
	}
	return ""
}

func (s *FilesService) dataRoot(path string) bool {
	for _, d := range []string{filesDataPhotos, filesDataMemo, filesDataReader} {
		if path == filepath.Join(s.dataDir, d) || strings.HasPrefix(path, filepath.Join(s.dataDir, d)+"/") {
			return true
		}
	}
	return false
}

func fsFree(path string) (free, total int64) {
	for p := path; ; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize)
		}
		if p == "/" {
			return 0, 0
		}
	}
}

// List は、path の中（空なら場所の一覧）。byName でなければ大きい順。du ならフォルダの中の合計も数える。
func (s *FilesService) List(path string, bySize, du bool) (FileList, error) {
	if path == "" {
		l := FileList{Entries: []FileEntry{}}
		for _, r := range s.Roots() {
			e := FileEntry{Name: r.Name, Path: r.Path, Dir: true}
			if fi, err := os.Stat(r.Path); err == nil {
				e.MTime = fi.ModTime()
			}
			l.Entries = append(l.Entries, e)
		}
		l.Free, l.Total = fsFree("/")
		return l, nil
	}
	root, rel, err := s.resolve(path)
	if err != nil {
		return FileList{}, err
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileList{}, fmt.Errorf("%w: %s", errFilesMissing, path)
		}
		return FileList{}, err
	}
	l := FileList{Path: path, Root: root.Name, Delete: root.Delete, Put: root.Delete && !s.dataRoot(path), Entries: []FileEntry{}}
	if rel != "." {
		l.Parent = filepath.Dir(path)
	} else {
		l.Parent = "" // 場所の一覧へ
	}
	deadline := time.Now().Add(filesDuLimit)
	for i, e := range ents {
		if i >= filesListMax {
			l.Truncated = true
			break
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if filesHiddenPath(filepath.Join(path, e.Name())) {
			continue
		}
		fe := FileEntry{Name: e.Name(), Path: filepath.Join(path, e.Name()), Dir: e.IsDir(), Link: fi.Mode()&os.ModeSymlink != 0, Size: fi.Size(), MTime: fi.ModTime()}
		if fe.Dir {
			fe.Size = 0
			if du {
				n, ok := dirSize(fe.Path, deadline)
				fe.Size = n
				if !ok {
					l.Partial = true
				}
			}
		}
		l.Entries = append(l.Entries, fe)
	}
	sort.SliceStable(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if bySize && a.Size != b.Size {
			return a.Size > b.Size
		}
		if a.Dir != b.Dir && !bySize {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	l.Free, l.Total = fsFree(path)
	return l, nil
}

// dirSize は、フォルダの中のファイルの大きさの合計（シンボリックリンクはたどらない）。deadline を過ぎたら途中まで。
func dirSize(path string, deadline time.Time) (int64, bool) {
	var n int64
	ok := true
	filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			ok = false
			return filepath.SkipAll
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, ok
}

// Read は、ファイルの offset から最大 n バイト（get_file）。
func (s *FilesService) Read(path string, offset int64, n int) ([]byte, int64, error) {
	if _, _, err := s.resolve(path); err != nil {
		return nil, 0, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s", errFilesMissing, path)
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: %s is not a regular file", errFilesDenied, path)
	}
	if n <= 0 || n > filesChunkMax {
		n = filesChunkMax
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if offset < 0 || offset > fi.Size() {
		return nil, 0, fmt.Errorf("%w: offset %d is outside the file (%d bytes)", errTransferBad, offset, fi.Size())
	}
	b := make([]byte, min(int64(n), fi.Size()-offset))
	if _, err := io.ReadFull(io.NewSectionReader(f, offset, int64(len(b))), b); err != nil {
		return nil, 0, err
	}
	return b, fi.Size(), nil
}

// Delete は、path を消す（フォルダは空のときだけ）。写真、メモ、読み物は、その係を通して消す。
func (s *FilesService) Delete(path string) error {
	root, rel, err := s.resolve(path)
	if err != nil {
		return err
	}
	if !root.Delete || !filesDeleteOK(root.Path, s.dataDir) {
		return fmt.Errorf("%w: files in %s cannot be deleted", errFilesDenied, root.Name)
	}
	if rel == "." {
		return fmt.Errorf("%w: the place itself cannot be deleted", errFilesDenied)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %s", errFilesMissing, path)
	}
	name := filepath.Base(path)
	switch s.isData(path) {
	case filesDataPhotos:
		id, ok := strings.CutSuffix(name, photoExt)
		if !ok || !validImageID(id) || s.photos == nil {
			return fmt.Errorf("%w: only photos (<id>.565) can be deleted here", errFilesDenied)
		}
		err = s.photos.Delete(id)
	case filesDataMemo:
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !validMemoID(id) || s.memos == nil {
			return fmt.Errorf("%w: only memos (m<n>.json) can be deleted here", errFilesDenied)
		}
		err = s.memos.Delete(id)
	case filesDataReader:
		id, ok := strings.CutSuffix(name, readExt)
		if !ok || !validImageID(id) || s.reads == nil {
			return fmt.Errorf("%w: only readings (<id>.json.gz) can be deleted here", errFilesDenied)
		}
		err = s.reads.Delete(id)
	default:
		if s.dataRoot(path) {
			return fmt.Errorf("%w: the folders in the data cannot be deleted", errFilesDenied)
		}
		if fi.IsDir() {
			err = os.Remove(path) // 空のフォルダだけ
		} else {
			err = os.Remove(path)
		}
	}
	if err == nil {
		log.Printf("files: deleted %s", path)
		s.notifyChanged()
	}
	return err
}

func (s *FilesService) notifyChanged() {
	s.mu.Lock()
	f := s.changed
	s.mu.Unlock()
	if f != nil {
		go f()
	}
}

// UploadKind は、upload_* の kind: file（path にフォルダ、name にファイルの名前）。
func (s *FilesService) UploadKind(dir string) UploadKind {
	return UploadKind{Dir: dir, MaxBytes: filesPutMax,
		Check: func(b UploadBegin) error {
			_, err := s.putTarget(b)
			if err != nil {
				return err
			}
			// 一時ファイルの置き場所は、初めて足すときにはまだない。空きは置き先の SD カードで数える
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			free, size, err := imageFreeSpace(dir)
			if err != nil {
				return err
			}
			if free-b.Bytes < reserveFor(size) {
				return imageErr(errImageNoSpace, "the SD card has %s free; saving %s would leave less than %s", kib(free), kib(b.Bytes), kib(reserveFor(size)))
			}
			return nil
		},
		Done: func(b UploadBegin, f *os.File, part string) (any, error) {
			f.Close()
			dst, err := s.putTarget(b)
			if err != nil {
				os.Remove(part)
				return nil, err
			}
			// 一時ファイルはデータの場所にあり、置き先と別のファイルシステムのことがあるので、写してから消す
			if err := copyNewFile(part, dst); err != nil {
				os.Remove(part)
				return nil, err
			}
			os.Remove(part)
			if st, err := os.Stat(filepath.Dir(dst)); err == nil {
				if sys, ok := st.Sys().(*syscall.Stat_t); ok {
					os.Chown(dst, int(sys.Uid), int(sys.Gid)) // フォルダの持ち主のものにする（user のホーム）
				}
			}
			log.Printf("files: put %s (%d bytes) from %s", dst, b.Bytes, b.Source)
			s.notifyChanged()
			return map[string]any{"path": dst, "bytes": b.Bytes}, nil
		}}
}

// putTarget は、PC から足すファイルの置き先（既にあれば断る）。
func (s *FilesService) putTarget(b UploadBegin) (string, error) {
	name := b.Name
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return "", fmt.Errorf("%w: \"name\" must be a file name, got %q", errTransferBad, name)
	}
	root, _, err := s.resolve(b.Path)
	if err != nil {
		return "", err
	}
	if !root.Delete || s.dataRoot(b.Path) {
		return "", fmt.Errorf("%w: files cannot be added to %s (use brain-deck photo add or read add for photos and readings)", errFilesDenied, b.Path)
	}
	if fi, err := os.Lstat(b.Path); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: %s is not a folder", errFilesMissing, b.Path)
	}
	dst := filepath.Join(b.Path, name)
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("%w: %s already exists", errFilesDenied, dst)
	}
	return dst, nil
}

func copyNewFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
