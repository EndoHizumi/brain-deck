package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------- backup、restore（Brain の設定とデータを、PC に 1 つのファイルで残す・戻す） ----------
//
// バックアップのファイル（tar.gz）は lefthand の backup.go が作る：config/config.yaml、data/…、anthy/…、最後に manifest.json。
// 中身には個人のデータ（Todo、メモ、読み物、写真、日本語入力の学習）が入るので、置き場所は自分だけが読めるようにする
// （フォルダ 0700、ファイル 0600）。既定の置き場所は $XDG_DATA_HOME/brain-deck/backups（なければ ~/.local/share/brain-deck/backups）。

const (
	backupCreateTimeout = 3 * time.Minute // Brain が tar.gz を作る（写真が多いと 10 秒を超える）
	syncBackupPrefix    = "brain-sync-"   // brain-deck sync が取ったもの（古いものから消す）
)

// backupDir は、バックアップの既定の置き場所。
func backupDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "brain-deck", "backups")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "brain-deck", "backups")
}

type backupManifest struct {
	Format  int       `json:"format"`
	Kind    string    `json:"kind"`
	Created time.Time `json:"created"`
	Version string    `json:"version"`
	Host    string    `json:"host"`
	Profile string    `json:"profile"`
	Files   []struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// backupSummary は、lefthand の BackupSummary。
type backupSummary struct {
	Created  time.Time `json:"created"`
	Version  string    `json:"version"`
	Host     string    `json:"host"`
	Profile  string    `json:"profile"`
	Config   bool      `json:"config"`
	Anthy    int       `json:"anthy"`
	Bytes    int64     `json:"bytes"`
	Problems []string  `json:"problems"`
	Items    []struct {
		Name  string `json:"name"`
		Label string `json:"label"`
		Files int    `json:"files"`
		Bytes int64  `json:"bytes"`
	} `json:"items"`
}

func (s backupSummary) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s に %s（lefthand %s）で作ったもの", s.Created.Local().Format("2006-01-02 15:04:05"), or(s.Host, "Brain"), s.Version)
	if s.Profile != "" {
		fmt.Fprintf(&b, "、PC ごとの設定 %s", s.Profile)
	}
	var parts []string
	if s.Config {
		parts = append(parts, "設定ファイル")
	}
	for _, it := range s.Items {
		if it.Files > 0 {
			p := it.Label
			if it.Files > 1 {
				p += fmt.Sprintf(" %d 個", it.Files)
			}
			parts = append(parts, p)
		}
	}
	if s.Anthy > 0 {
		parts = append(parts, "日本語入力の学習")
	}
	fmt.Fprintf(&b, "\n中身：%s（合計 %s）", strings.Join(parts, "、"), humanBytes(s.Bytes))
	return b.String()
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d バイト", n)
}

// inspectBackup は、PC にあるバックアップのファイルを読み、中身が manifest.json と合うかを確かめる。
func inspectBackup(data []byte) (*backupManifest, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("brain-deck のバックアップとして読めません："+format, args...)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, bad("gzip ではありません")
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	var m *backupManifest
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, bad("%v（ファイルが途中で切れている？）", err)
		}
		if h.Name == "manifest.json" {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, bad("%v", err)
			}
			m = &backupManifest{}
			if err := json.Unmarshal(b, m); err != nil {
				return nil, bad("manifest.json：%v", err)
			}
			continue
		}
		hs := sha256.New()
		if _, err := io.Copy(hs, tr); err != nil {
			return nil, bad("%s：%v", h.Name, err)
		}
		got[h.Name] = hex.EncodeToString(hs.Sum(nil))
	}
	if m == nil || m.Kind != "brain-deck-backup" {
		return nil, bad("manifest.json がない（途中で切れている？）")
	}
	if len(m.Files) != len(got) {
		return nil, bad("manifest.json と中身のファイルの数が違います")
	}
	for _, f := range m.Files {
		if got[f.Path] != f.SHA256 {
			return nil, bad("%s が manifest.json と合いません（壊れている？）", f.Path)
		}
	}
	return m, nil
}

// fetchBackup は、Brain にバックアップを作らせ、path に書く（一時ファイルに書いて、確かめてから置き換える）。
func fetchBackup(c *Client, path string) (backupSummary, error) {
	var sum backupSummary
	if err := need(c, "backup_create"); err != nil {
		return sum, err
	}
	raw, err := c.Call("backup_create", nil, max(callTimeout(), backupCreateTimeout))
	if err != nil {
		return sum, err
	}
	var r struct {
		Backup  string        `json:"backup"`
		Bytes   int64         `json:"bytes"`
		SHA256  string        `json:"sha256"`
		Summary backupSummary `json:"summary"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Backup == "" {
		return sum, fmt.Errorf("backup_create の返事が読めません")
	}
	defer c.Call("backup_done", map[string]any{"backup": r.Backup}, 10*time.Second)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return sum, err
	}
	f, err := os.CreateTemp(dir, ".brain-backup-*.part")
	if err != nil {
		return sum, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	os.Chmod(tmp, 0o600)
	h := sha256.New()
	var off int64
	for off < r.Bytes {
		raw, err := c.Call("backup_read", map[string]any{"backup": r.Backup, "offset": off}, max(callTimeout(), 30*time.Second))
		if err != nil {
			f.Close()
			return sum, err
		}
		var chunk struct {
			Data string `json:"data"`
		}
		json.Unmarshal(raw, &chunk)
		b, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil || len(b) == 0 {
			f.Close()
			return sum, fmt.Errorf("backup_read の返事が読めません（%d バイト目）", off)
		}
		f.Write(b)
		h.Write(b)
		off += int64(len(b))
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return sum, err
	}
	f.Close()
	if hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		return sum, errors.New("受け取ったバックアップの SHA-256 が合いません（もう一度取ってください）")
	}
	if err := os.Rename(tmp, path); err != nil {
		return sum, err
	}
	os.Chmod(path, 0o600)
	return r.Summary, nil
}

// backupName は、バックアップのファイルの名前（prefix-年月日-時分秒.tar.gz）。
func backupName(prefix string, t time.Time) string {
	return prefix + t.Format("20060102-150405") + ".tar.gz"
}

func runBackup(o *options, stdout, stderr io.Writer) int {
	if err := o.checkSub("o"); err != nil {
		return report(stderr, err)
	}
	a := o.args[1:]
	if len(a) == 1 && a[0] == "list" {
		return listBackups(stdout, stderr)
	}
	if len(a) == 2 && a[0] == "show" {
		data, err := os.ReadFile(a[1])
		if err != nil {
			return report(stderr, usageError(err.Error()))
		}
		m, err := inspectBackup(data)
		if err != nil {
			return report(stderr, usageError(err.Error()))
		}
		fmt.Fprintln(stdout, describeManifest(m))
		return exitOK
	}
	if len(a) != 0 {
		return report(stderr, usageError("backup は引数を取りません（backup [-o <ファイル>]、backup list、backup show <ファイル>）"))
	}
	path := o.vals["o"]
	if path == "" {
		path = filepath.Join(backupDir(), backupName("brain-", time.Now()))
	}
	if !o.quiet {
		fmt.Fprintln(stderr, "brain-deck: Brain でバックアップを作っています（写真が多いと数十秒かかります）")
	}
	s, err := openSession(o, stderr, backupCreateTimeout+5*time.Minute)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	sum, err := fetchBackup(s.c, path)
	if err != nil {
		return report(stderr, err)
	}
	if !o.quiet {
		st, _ := os.Stat(path)
		fmt.Fprintf(stdout, "%s に保存しました（%s）\n%s\n個人のデータが入っています。置き場所に気をつけてください（README の「バックアップと復元」）\n",
			path, humanBytes(st.Size()), sum.describe())
	}
	return exitOK
}

// describeManifest は、PC で読んだ manifest.json のまとめ。
func describeManifest(m *backupManifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "作った時刻\t%s\nBrain\t%s（lefthand %s）\n", m.Created.Local().Format("2006-01-02 15:04:05"), m.Host, m.Version)
	if m.Profile != "" {
		fmt.Fprintf(&b, "PC ごとの設定\t%s\n", m.Profile)
	}
	groups := map[string][2]int64{}
	var order []string
	for _, f := range m.Files {
		key := f.Path
		if parts := strings.SplitN(f.Path, "/", 3); len(parts) == 3 {
			key = parts[0] + "/" + parts[1] + "/"
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		g := groups[key]
		groups[key] = [2]int64{g[0] + 1, g[1] + f.Bytes}
	}
	for _, k := range order {
		g := groups[k]
		if strings.HasSuffix(k, "/") {
			fmt.Fprintf(&b, "%s\t%d 個、%s\n", k, g[0], humanBytes(g[1]))
		} else {
			fmt.Fprintf(&b, "%s\t%s\n", k, humanBytes(g[1]))
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// listBackups は、既定の置き場所のバックアップを新しい順に並べる。
func listBackups(stdout, stderr io.Writer) int {
	dir := backupDir()
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return report(stderr, err)
	}
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "brain-") && strings.HasSuffix(e.Name(), ".tar.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) == 0 {
		fmt.Fprintf(stdout, "%s にバックアップはありません\n", dir)
		return exitOK
	}
	fmt.Fprintln(stdout, dir)
	for _, n := range names {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		kind := "手で取ったもの"
		if strings.HasPrefix(n, syncBackupPrefix) {
			kind = "sync で取ったもの"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", n, humanBytes(st.Size()), kind)
	}
	return exitOK
}

func runRestore(o *options, stdout, stderr io.Writer) int {
	if err := o.checkSub("no-config no-data no-anthy undo"); err != nil {
		return report(stderr, err)
	}
	a := o.args[1:]
	undo := o.flags["undo"]
	switch {
	case undo && len(a) != 0:
		return report(stderr, usageError("restore --undo はファイルを取りません"))
	case !undo && len(a) != 1:
		return report(stderr, usageError("restore のあとに、バックアップのファイル（brain-deck backup が作った .tar.gz）を 1 つ書きます"))
	}
	items := map[string]bool{"config": !o.flags["no-config"], "data": !o.flags["no-data"], "anthy": !o.flags["no-anthy"]}
	if !items["config"] && !items["data"] && !items["anthy"] {
		return report(stderr, usageError("--no-config、--no-data、--no-anthy をすべて書くと、戻すものがありません"))
	}
	var data []byte
	if !undo {
		var err error
		if data, err = os.ReadFile(a[0]); err != nil {
			return report(stderr, usageError(err.Error()))
		}
		m, err := inspectBackup(data)
		if err != nil {
			return report(stderr, usageError(a[0]+"："+err.Error()))
		}
		if !o.quiet {
			fmt.Fprintf(stderr, "brain-deck: %s を確かめました（%s、%d 個のファイル）\n", a[0], m.Created.Local().Format("2006-01-02 15:04:05"), len(m.Files))
		}
	}
	s, err := openSession(o, stderr, updateTimeout)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	if err := need(s.c, "restore_apply"); err != nil {
		return report(stderr, err)
	}
	params := map[string]any{"items": items}
	if undo {
		params["undo"] = true
	} else {
		raw, err := s.upload("backup", data, filepath.Base(a[0]))
		if err != nil {
			return report(stderr, err)
		}
		var r struct {
			Restore string        `json:"restore"`
			Summary backupSummary `json:"summary"`
		}
		json.Unmarshal(raw, &r)
		s.note("Brain で確かめました：%s", r.Summary.describe())
		if len(r.Summary.Problems) > 0 && r.Summary.Config {
			msg := "バックアップの設定ファイルは、今の lefthand では使えません：" + strings.Join(r.Summary.Problems, "；")
			if items["config"] {
				return report(stderr, usageError(msg+"\n設定ファイルのほかを戻すときは --no-config を付けます"))
			}
			s.note("%s（--no-config なので戻しません）", msg)
		}
		if o.dryRun {
			fmt.Fprintln(stdout, "--dry-run なので、戻さずに終わります")
			return exitOK
		}
		params["restore"] = r.Restore
	}
	if o.dryRun {
		fmt.Fprintln(stdout, "--dry-run なので、戻さずに終わります")
		return exitOK
	}
	raw, err := s.call("restore_apply", params, 3*time.Minute)
	if err != nil {
		return report(stderr, err)
	}
	var ap struct {
		Stage   string        `json:"stage"`
		Summary backupSummary `json:"summary"`
	}
	json.Unmarshal(raw, &ap)
	if undo {
		s.note("復元の前の状態に戻します：%s", ap.Summary.describe())
	}
	s.note("今の状態を Brain に残しました。lefthand を止めて入れ替えるので、シリアルが一度切れます（つなぎ直して確かめます）")
	st, err := s.waitHelper(ap.Stage, true, "")
	if err != nil {
		return report(stderr, err)
	}
	if !st.OK {
		fmt.Fprintf(stderr, "brain-deck: 戻せなかったので、元の状態に戻しました：%s\n詳しくは brain-deck logs で見られます\n", st.Message)
		return exitFailed
	}
	if !o.quiet {
		var what []string
		for _, k := range []string{"config", "data", "anthy"} {
			if items[k] {
				what = append(what, map[string]string{"config": "設定ファイル", "data": "データ", "anthy": "日本語入力の学習"}[k])
			}
		}
		msg := "戻しました（" + strings.Join(what, "、") + "）。戻す前の状態は Brain に残してあります（brain-deck restore --undo で戻せます）"
		if undo {
			msg = "復元の前の状態に戻しました（もう一度 restore --undo で、戻す前の状態に戻せます）"
		}
		fmt.Fprintln(stdout, msg)
	}
	return exitOK
}

// ---------- sync で取る ----------

// syncBackupConfig は、sync.yaml の backup。
type syncBackupConfig struct {
	Keep  *int   `yaml:"keep"`  // 残す数（既定 7。0 で取らない）
	Every string `yaml:"every"` // 前に取ってからこれだけたっていれば取る（既定 20h。0 なら毎回）
	Dir   string `yaml:"dir"`   // 置き場所（既定 ~/.local/share/brain-deck/backups）
}

const (
	defaultSyncBackupKeep  = 7
	defaultSyncBackupEvery = 20 * time.Hour // 毎日つないでも、1 日 1 回になるように少し短く
)

// syncBackup は、sync の段階「バックアップ」。取らなかったときは skip が true。
func syncBackup(c *Client, bc syncBackupConfig, now time.Time) (note string, skip bool, err error) {
	keep := defaultSyncBackupKeep
	if bc.Keep != nil {
		keep = *bc.Keep
	}
	if keep <= 0 {
		return "sync.yaml で backup.keep: 0", true, nil
	}
	every := defaultSyncBackupEvery
	if bc.Every != "" {
		d, err := time.ParseDuration(bc.Every)
		if err != nil || d < 0 {
			return "", false, fmt.Errorf("sync.yaml の backup.every は 24h のような時間です")
		}
		every = d
	}
	dir := backupDir()
	if bc.Dir != "" {
		dir = expandHome(bc.Dir)
	}
	old := syncBackups(dir)
	if len(old) > 0 && every > 0 {
		if st, err := os.Stat(filepath.Join(dir, old[0])); err == nil && now.Sub(st.ModTime()) < every {
			return fmt.Sprintf("前に取ってから %v たっていない（%s）", every, old[0]), true, nil
		}
	}
	if err := need(c, "backup_create"); err != nil {
		return "Brain の lefthand がバックアップに対応していない", true, nil
	}
	path := filepath.Join(dir, backupName(syncBackupPrefix, now))
	if _, err := fetchBackup(c, path); err != nil {
		return "", false, err
	}
	removed := 0
	for _, n := range syncBackups(dir)[min(keep, len(syncBackups(dir))):] {
		if os.Remove(filepath.Join(dir, n)) == nil {
			removed++
		}
	}
	st, _ := os.Stat(path)
	note = fmt.Sprintf("%s（%s）", path, humanBytes(st.Size()))
	if removed > 0 {
		note += fmt.Sprintf("。古いものを %d 個消した（%d 個残す）", removed, keep)
	}
	return note, false, nil
}

// syncBackups は、dir にある sync のバックアップ（新しい順）。
func syncBackups(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), syncBackupPrefix) && strings.HasSuffix(e.Name(), ".tar.gz") && e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}
