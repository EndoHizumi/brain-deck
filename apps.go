package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------- 自分で足すアプリ（フェーズ K5）：置き場所と説明のファイル ----------
//
// /var/lib/lefthand/apps/<名前>/ に、説明のファイル app.yaml とスクリプトを置くと、
// セル（widget: app、name: <名前>）にスクリプトの出力を描く。PC からは brain-deck app install <フォルダ>（upload の kind: app）。
// フォルダとファイルは root のもの（0755 と 0644）で、スクリプトは Brain の user として動かす（appsrun.go）。
// スクリプトが書けるのは /var/lib/lefthand/apps-data/<名前>/（user のもの。BRAIN_APP_DATA）。
// 名前とファイルの名前は英数字と _ . - だけで、フォルダの中にはふつうのファイルだけを置く（リンクやフォルダは置かない）。
// 出力の決まりと、引数と標準入力で渡すものは appwidget.go と docs/apps.md。

const (
	appsDirName     = "apps"
	appsDataDirName = "apps-data"
	appManifestName = "app.yaml"
	appDefaultRun   = "main.sh"

	appMaxFiles     = 32
	appMaxFileBytes = 256 << 10
	appMaxBytes     = 1 << 20  // 1 つのアプリのファイルの合計
	appMaxUpload    = 2 << 20  // 送る tar.gz
	appMaxApps      = 32       // 置けるアプリの数
	appMaxDataBytes = 16 << 20 // スクリプトが書けるファイルの大きさ（ulimit -f）

	appModeInterval = "interval" // 決まった間隔で動かして、出力を描く
	appModeStream   = "stream"   // 動き続け、行ごとに描き直す

	appDefaultEvery   = time.Minute
	appMinEvery       = 2 * time.Second
	appMaxEvery       = 24 * time.Hour
	appDefaultTimeout = 10 * time.Second
	appMaxTimeout     = 60 * time.Second
	appDefaultMemMB   = 64
	appMinMemMB       = 16
	appMaxMemMB       = 256
)

var (
	appNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	appFileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

	errAppBad     = errors.New("not a usable app")
	errAppMissing = errors.New("no such app")
)

// AppManifest は app.yaml。
type AppManifest struct {
	Name        string   `yaml:"name,omitempty" json:"name,omitempty"`               // 省略するとフォルダの名前。書くならフォルダの名前と同じ
	Title       string   `yaml:"title,omitempty" json:"title,omitempty"`             // 画面に出す名前。省略すると name
	Description string   `yaml:"description,omitempty" json:"description,omitempty"` // 一覧に出す説明
	Icon        string   `yaml:"icon,omitempty" json:"icon,omitempty"`               // ホーム画面に置くときの組み込みのアイコンの名前（brain-deck が勧める）
	Run         string   `yaml:"run,omitempty" json:"run,omitempty"`                 // 動かすスクリプト（フォルダの中のファイル）。既定 main.sh
	Mode        string   `yaml:"mode,omitempty" json:"mode,omitempty"`               // interval（既定）か stream
	Every       string   `yaml:"every,omitempty" json:"every,omitempty"`             // interval：動かす間隔（既定 1m、2s〜24h。off で、タップやボタンのときだけ）
	Timeout     string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`         // interval：1 回の時間の上限（既定 10s、1s〜60s）
	Memory      int      `yaml:"memory_mb,omitempty" json:"memory_mb,omitempty"`     // メモリの上限（MB。仮想メモリ。既定 64、16〜256）
	Keys        []string `yaml:"keys,omitempty" json:"keys,omitempty"`               // 受け取る本体キー（left、right、up、down、enter、space、a〜z、0〜9 など）
}

// AppDef は、確かめた説明のファイル。
type AppDef struct {
	Name, Title, Description, Icon string
	Run                            string
	Stream                         bool
	Every                          time.Duration // 0 なら時間では動かさない
	Timeout                        time.Duration
	MemMB                          int
	Keys                           map[string]bool
	Dir                            string
}

// parseAppManifest は app.yaml を読んで確かめる。dirName はフォルダの名前、files はフォルダの中のファイルの名前。
func parseAppManifest(b []byte, dirName string, files map[string]bool) (*AppDef, error) {
	var m AppManifest
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %v", appManifestName, err)
	}
	if !appNameRE.MatchString(dirName) {
		return nil, fmt.Errorf("app name %q must be 1-32 characters of a-z 0-9 _ - (starting with a letter or a digit)", dirName)
	}
	if m.Name != "" && m.Name != dirName {
		return nil, fmt.Errorf("%s: name %q is not the folder name %q", appManifestName, m.Name, dirName)
	}
	d := &AppDef{Name: dirName, Title: m.Title, Description: m.Description, Icon: m.Icon, Run: m.Run, MemMB: m.Memory,
		Every: appDefaultEvery, Timeout: appDefaultTimeout}
	if d.Title == "" {
		d.Title = dirName
	}
	if len([]rune(d.Title)) > 32 || len([]rune(d.Description)) > 200 {
		return nil, fmt.Errorf("%s: title is up to 32 characters and description up to 200", appManifestName)
	}
	if d.Icon != "" {
		if _, ok := builtinIcons[d.Icon]; !ok {
			return nil, fmt.Errorf("%s: icon %q is not a built-in icon", appManifestName, d.Icon)
		}
	}
	if d.Run == "" {
		d.Run = appDefaultRun
	}
	if !appFileRE.MatchString(d.Run) || d.Run == appManifestName {
		return nil, fmt.Errorf("%s: run must be a file name in the app folder (no / or ..), got %q", appManifestName, d.Run)
	}
	if files != nil && !files[d.Run] {
		return nil, fmt.Errorf("%s: run: %q is not in the app folder", appManifestName, d.Run)
	}
	switch m.Mode {
	case "", appModeInterval:
	case appModeStream:
		d.Stream = true
		if m.Every != "" || m.Timeout != "" {
			return nil, fmt.Errorf("%s: every and timeout are for mode: interval (a stream app keeps running)", appManifestName)
		}
	default:
		return nil, fmt.Errorf("%s: mode must be interval or stream, got %q", appManifestName, m.Mode)
	}
	switch m.Every {
	case "":
	case "off":
		d.Every = 0
	default:
		e, err := time.ParseDuration(m.Every)
		if err != nil || e < appMinEvery || e > appMaxEvery {
			return nil, fmt.Errorf("%s: every must be a duration from %v to %v (such as 30s), or off", appManifestName, appMinEvery, appMaxEvery)
		}
		d.Every = e
	}
	if m.Timeout != "" {
		t, err := time.ParseDuration(m.Timeout)
		if err != nil || t < time.Second || t > appMaxTimeout {
			return nil, fmt.Errorf("%s: timeout must be a duration from 1s to %v", appManifestName, appMaxTimeout)
		}
		d.Timeout = t
	}
	if d.MemMB == 0 {
		d.MemMB = appDefaultMemMB
	}
	if d.MemMB < appMinMemMB || d.MemMB > appMaxMemMB {
		return nil, fmt.Errorf("%s: memory_mb must be %d..%d", appManifestName, appMinMemMB, appMaxMemMB)
	}
	if len(m.Keys) > 0 {
		d.Keys = map[string]bool{}
		for _, k := range m.Keys {
			if _, ok := appKeyCodes[k]; !ok {
				return nil, fmt.Errorf("%s: keys: unknown key %q (%s)", appManifestName, k, strings.Join(appKeyNames(), " "))
			}
			d.Keys[k] = true
		}
	}
	return d, nil
}

// AppInfo は、一覧の 1 つ（list_apps の答え）。
type AppInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Every       string `json:"every,omitempty"`
	Bytes       int64  `json:"bytes"`
	Files       int    `json:"files"`
	Error       string `json:"error,omitempty"` // 使えないとき、その理由
	Status      string `json:"status,omitempty"`
	LastRun     string `json:"last_run,omitempty"`
}

// AppStore は、アプリの置き場所。
type AppStore struct {
	dir, dataDir string
	mu           sync.Mutex
	apps         map[string]*AppDef
	bad          map[string]string // 使えないアプリと理由
	sizes        map[string][2]int64
	onChange     func(name string) // 足した、入れ替えた、消したとき（動いているものを止める）
}

// OpenAppStore は、dataDir/apps を読む。
func OpenAppStore(dataDir string) *AppStore {
	s := &AppStore{dir: filepath.Join(dataDir, appsDirName), dataDir: filepath.Join(dataDir, appsDataDirName)}
	s.Reload()
	return s
}

// Reload は、フォルダを読み直す。
func (s *AppStore) Reload() {
	apps, bad, sizes := map[string]*AppDef{}, map[string]string{}, map[string][2]int64{}
	ents, err := os.ReadDir(s.dir)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("apps: %v", err)
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // 入れ替えの途中のもの
		}
		d, n, size, err := loadAppDir(filepath.Join(s.dir, name), name)
		sizes[name] = [2]int64{int64(n), size}
		if err != nil {
			bad[name] = err.Error()
			continue
		}
		apps[name] = d
	}
	s.mu.Lock()
	s.apps, s.bad, s.sizes = apps, bad, sizes
	s.mu.Unlock()
	if len(apps)+len(bad) > 0 {
		log.Printf("apps: %d apps in %s (%d unusable)", len(apps)+len(bad), s.dir, len(bad))
	}
}

// loadAppDir は、アプリのフォルダを確かめて読む。ファイルの数と大きさの合計も返す。
func loadAppDir(dir, name string) (*AppDef, int, int64, error) {
	if !appNameRE.MatchString(name) {
		return nil, 0, 0, fmt.Errorf("%w: the folder name %q is not a valid app name", errAppBad, name)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, 0, 0, err
	}
	if !fi.IsDir() {
		return nil, 0, 0, fmt.Errorf("%w: %s is not a folder", errAppBad, dir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, 0, err
	}
	files := map[string]bool{}
	var total int64
	for _, e := range ents {
		if !appFileRE.MatchString(e.Name()) || !e.Type().IsRegular() {
			return nil, len(ents), total, fmt.Errorf("%w: %s: only plain files with names of A-Z a-z 0-9 _ . - can be in an app folder", errAppBad, e.Name())
		}
		in, err := e.Info()
		if err != nil {
			return nil, len(ents), total, err
		}
		total += in.Size()
		files[e.Name()] = true
	}
	if len(ents) > appMaxFiles || total > appMaxBytes {
		return nil, len(ents), total, fmt.Errorf("%w: up to %d files and %s in an app", errAppBad, appMaxFiles, kib(appMaxBytes))
	}
	b, err := os.ReadFile(filepath.Join(dir, appManifestName))
	if err != nil {
		return nil, len(ents), total, fmt.Errorf("%w: %s is missing", errAppBad, appManifestName)
	}
	d, err := parseAppManifest(b, name, files)
	if err != nil {
		return nil, len(ents), total, fmt.Errorf("%w: %v", errAppBad, err)
	}
	d.Dir = dir
	return d, len(ents), total, nil
}

// Get は、名前のアプリ（なければ nil と、使えない理由）。
func (s *AppStore) Get(name string) (*AppDef, string) {
	if s == nil {
		return nil, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apps[name], s.bad[name]
}

// List は、置いてあるアプリ（名前の順）。
func (s *AppStore) List() []AppInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AppInfo
	for name, sz := range s.sizes {
		in := AppInfo{Name: name, Files: int(sz[0]), Bytes: sz[1]}
		if d := s.apps[name]; d != nil {
			in.Title, in.Description, in.Icon = d.Title, d.Description, d.Icon
			in.Mode = appModeInterval
			if d.Stream {
				in.Mode = appModeStream
			} else if d.Every > 0 {
				in.Every = d.Every.String()
			} else {
				in.Every = "off"
			}
		} else {
			in.Error = s.bad[name]
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *AppStore) changed(name string) {
	s.mu.Lock()
	f := s.onChange
	s.mu.Unlock()
	if f != nil {
		f(name)
	}
}

// Remove は、アプリと、そのアプリが書いたデータ（apps-data/<名前>）を消す。
func (s *AppStore) Remove(name string) error {
	if !appNameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", errAppMissing, name)
	}
	dir := filepath.Join(s.dir, name)
	if _, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("%w: %q", errAppMissing, name)
	}
	s.changed(name) // 動いていれば止める
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(s.dataDir, name))
	s.Reload()
	return nil
}

// UploadKind は、upload_* の kind: app（name はアプリの名前、中身はフォルダの中のファイルを並べた tar.gz）。
func (s *AppStore) UploadKind() UploadKind {
	return UploadKind{Dir: s.dir, MaxBytes: appMaxUpload,
		Check: func(b UploadBegin) error {
			if !appNameRE.MatchString(b.Name) {
				return fmt.Errorf("%w: app name %q must be 1-32 characters of a-z 0-9 _ - (starting with a letter or a digit)", errAppBad, b.Name)
			}
			s.mu.Lock()
			n := len(s.sizes)
			_, exists := s.sizes[b.Name]
			s.mu.Unlock()
			if !exists && n >= appMaxApps {
				return fmt.Errorf("%w: up to %d apps; remove one first (brain-deck app rm)", errAppBad, appMaxApps)
			}
			return os.MkdirAll(s.dir, 0o755)
		},
		Done: func(b UploadBegin, f *os.File, part string) (any, error) {
			defer os.Remove(part)
			defer f.Close()
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			d, err := s.install(b.Name, f)
			if err != nil {
				return nil, err
			}
			for _, in := range s.List() {
				if in.Name == d.Name {
					return in, nil
				}
			}
			return nil, errAppMissing
		}}
}

// install は、tar.gz の中身を確かめてから、アプリのフォルダを入れ替える（前のものは消す。データは残す）。
func (s *AppStore) install(name string, r io.Reader) (*AppDef, error) {
	tmp, err := os.MkdirTemp(s.dir, ".new-"+name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil {
		return nil, err
	}
	if err := extractApp(r, tmp); err != nil {
		return nil, err
	}
	d, _, _, err := loadAppDir(tmp, name)
	if err != nil {
		return nil, err
	}
	s.changed(name) // 動いていれば止める
	dst := filepath.Join(s.dir, name)
	old := ""
	if _, err := os.Lstat(dst); err == nil {
		old = filepath.Join(s.dir, fmt.Sprintf(".old-%s-%d", name, time.Now().UnixNano()))
		if err := os.Rename(dst, old); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		if old != "" {
			os.Rename(old, dst)
		}
		return nil, err
	}
	if old != "" {
		os.RemoveAll(old)
	}
	s.Reload()
	d.Dir = dst
	log.Printf("apps: installed %q (%s)", name, d.Title)
	return d, nil
}

// extractApp は、tar.gz のふつうのファイルだけを dir に書く（名前は appFileRE。フォルダ、リンク、ほかの場所を指す名前は断る）。
func extractApp(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %v", errAppBad, err)
	}
	tr := tar.NewReader(zr)
	n := 0
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errAppBad, err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir && (name == "" || name == ".") {
			continue
		}
		if h.Typeflag != tar.TypeReg || !appFileRE.MatchString(name) {
			return fmt.Errorf("%w: %q: only plain files with names of A-Z a-z 0-9 _ . - (no folders, links or paths) can be in an app", errAppBad, h.Name)
		}
		n++
		total += h.Size
		if n > appMaxFiles || h.Size > appMaxFileBytes || total > appMaxBytes {
			return fmt.Errorf("%w: up to %d files, %s each and %s in total", errAppBad, appMaxFiles, kib(appMaxFileBytes), kib(appMaxBytes))
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("%w: %q: %v", errAppBad, name, err)
		}
		_, err = io.Copy(f, io.LimitReader(tr, appMaxFileBytes+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if n == 0 {
		return fmt.Errorf("%w: the archive is empty", errAppBad)
	}
	return nil
}
