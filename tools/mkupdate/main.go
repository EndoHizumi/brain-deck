// mkupdate は、brain-deck update で Brain に送る「更新のファイル」（tar.gz）を作る。
//
// 中身は manifest.json、lefthand（ARMv5 の実行ファイル）、gadget-setup.sh の 3 つ。デーモンと gadget-setup.sh は
// 組で入れ替える（ディスクリプタが同じでなければならない）ので、いつも両方を入れる。
//
//	go run ./tools/mkupdate                       # ARMv5 向けにビルドして lefthand-<版>.update.tar.gz を作る
//	go run ./tools/mkupdate -bin lefthand -o x.tar.gz   # ビルド済みの lefthand を使う
//
// 版は、lefthand に埋め込まれた git のリビジョン（-ldflags の -X main.version があればそちら）から読む。
// Brain のデーモンは、新しい lefthand を -version で実行し、ここに書いた版と同じかを確かめる。
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type manifestFile struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type manifest struct {
	Format  int                     `json:"format"`
	Version string                  `json:"version"`
	Built   string                  `json:"built"`
	Files   map[string]manifestFile `json:"files"`
}

func main() {
	bin := flag.String("bin", "", "ビルド済みの lefthand（省略すると、ARMv5 向けにビルドする）")
	gadget := flag.String("gadget", "gadget-setup.sh", "gadget-setup.sh")
	out := flag.String("o", "", "書き出すファイル（省略すると lefthand-<版>.update.tar.gz）")
	src := flag.String("src", ".", "-bin を省略したときに、ビルドするディレクトリ（lefthand のリポジトリ）")
	flag.Parse()
	if err := run(*bin, *gadget, *out, *src); err != nil {
		fmt.Fprintln(os.Stderr, "mkupdate:", err)
		os.Exit(1)
	}
}

func run(bin, gadget, out, src string) error {
	if bin == "" {
		tmp, err := os.MkdirTemp("", "mkupdate")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		bin = filepath.Join(tmp, "lefthand")
		cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm", "GOARM=5", "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		fmt.Fprintln(os.Stderr, "mkupdate: GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -trimpath")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go build: %w", err)
		}
	}
	ver, err := binaryVersion(bin)
	if err != nil {
		return err
	}
	if out == "" {
		out = "lefthand-" + strings.ReplaceAll(ver, "+", "-") + ".update.tar.gz"
	}
	m := manifest{Format: 1, Version: ver, Built: time.Now().UTC().Format(time.RFC3339), Files: map[string]manifestFile{}}
	files := []struct{ name, path string }{{"lefthand", bin}, {"gadget-setup.sh", gadget}}
	data := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if err != nil {
			return err
		}
		s := sha256.Sum256(b)
		m.Files[f.name] = manifestFile{SHA256: hex.EncodeToString(s[:]), Bytes: int64(len(b))}
		data[f.name] = b
	}
	mb, _ := json.MarshalIndent(m, "", "  ")

	o, err := os.Create(out)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(o, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	add := func(name string, b []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	err = add("manifest.json", mb, 0o644)
	for _, f := range files {
		if err == nil {
			err = add(f.name, data[f.name], 0o755)
		}
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = zw.Close()
	}
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
		return err
	}
	st, _ := os.Stat(out)
	fmt.Printf("%s（lefthand %s、%.1f MB）\n", out, ver, float64(st.Size())/(1<<20))
	return nil
}

// binaryVersion は、lefthand が -version で答える版を、実行ファイルから読む（main.go の daemonVersion と同じ規則）。
func binaryVersion(path string) (string, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w (not a Go executable?)", path, err)
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "-ldflags":
			if m := regexp.MustCompile(`-X[= ]main\.version=(\S+)`).FindStringSubmatch(s.Value); m != nil {
				return strings.Trim(m[1], `'"`), nil
			}
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev", nil
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev, nil
}
