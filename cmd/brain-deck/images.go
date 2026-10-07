package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- images（背景画像） ----------
//
// 画像の変換（切り抜き、縮小、RGB565、ディザリング）は設定 GUI で行う。brain-deck は、Brain にある画像の一覧、
// 使っていない画像の削除、変換済みのファイル（.565。config/background-images など）を送ることだけをする。

const imagesUsage = `images のあとには、list（省略可）、put <ファイル.565>...、prune [--dry-run] を書きます`

// imagePutTimeout は、images put で --timeout を書かなかったときの時間の上限（画面いっぱいの画像は 1 MB ほど送る）。
const imagePutTimeout = 2 * time.Minute

type imageInfo struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	W      int       `json:"w"`
	H      int       `json:"h"`
	Bytes  int64     `json:"bytes"`
	Added  time.Time `json:"added"`
	Source string    `json:"source"`
	Refs   []string  `json:"refs"`
}

type imageList struct {
	Images     []imageInfo `json:"images"`
	TotalBytes int64       `json:"total_bytes"`
	LimitBytes int64       `json:"limit_bytes"`
	FreeBytes  int64       `json:"free_bytes"`
	Reserve    int64       `json:"reserve_bytes"`
	Missing    []string    `json:"missing"`
}

func imagesCommand(o *options) (func(*Client) (string, error), error) {
	a := o.args[1:]
	sub := "list"
	if len(a) > 0 {
		sub, a = a[0], a[1:]
	}
	if o.json && sub != "list" && sub != "ls" {
		return nil, usageError("--json は images list だけで使えます")
	}
	if o.dryRun && sub != "prune" {
		return nil, usageError("--dry-run は images prune と calendar sync で使います")
	}
	switch sub {
	case "list", "ls":
		if len(a) != 0 {
			return nil, usageError("images list は引数を取りません")
		}
		return func(c *Client) (string, error) { return listImages(c, o.json) }, nil
	case "put":
		if len(a) == 0 {
			return nil, usageError("images put のあとに、送るファイル（.565。設定 GUI か tools/mkbg が作ったもの）か、それを入れたディレクトリを書きます")
		}
		files := make([]imageFile, 0, len(a))
		for _, p := range a {
			paths := []string{p}
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				// ディレクトリなら、中の .565 をすべて送る（config/background-images など）
				paths, _ = filepath.Glob(filepath.Join(p, "*.565"))
				if len(paths) == 0 {
					return nil, usageError(p + " に .565 のファイルがありません")
				}
			}
			for _, q := range paths {
				f, err := readImageFile(q)
				if err != nil {
					return nil, usageError(err.Error())
				}
				files = append(files, f)
			}
		}
		if o.timeout == defaultTimeout {
			o.timeout = imagePutTimeout
		}
		return func(c *Client) (string, error) { return putImages(c, files) }, nil
	case "prune":
		if len(a) != 0 {
			return nil, usageError("images prune は引数を取りません")
		}
		return func(c *Client) (string, error) { return pruneImages(c, o.dryRun) }, nil
	}
	return nil, usageError(imagesUsage)
}

func listImages(c *Client, asJSON bool) (string, error) {
	if err := need(c, "list_images"); err != nil {
		return "", err
	}
	raw, err := c.Call("list_images", nil, callTimeout())
	if err != nil {
		return "", err
	}
	if asJSON {
		return string(raw), nil
	}
	var l imageList
	if err := json.Unmarshal(raw, &l); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, im := range l.Images {
		used := "未使用"
		if len(im.Refs) > 0 {
			used = fmt.Sprintf("使用 %d", len(im.Refs))
		}
		fmt.Fprintf(&b, "%s  %3dx%-3d  %5s  %-7s %s\n", im.ID, im.W, im.H, kb(im.Bytes), used, im.Name)
	}
	fmt.Fprintf(&b, "%d 枚、%s（上限 %s）。SD カードの空き %s（%s は残す）", len(l.Images), kb(l.TotalBytes), kb(l.LimitBytes), kb(l.FreeBytes), kb(l.Reserve))
	if len(l.Missing) > 0 {
		fmt.Fprintf(&b, "\n設定で使っているのに Brain にない画像：%s（背景なしで描いています）", strings.Join(l.Missing, " "))
	}
	return b.String(), nil
}

func kb(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%dKB", (n+1023)>>10)
}

// imageFile は、送る画像のファイル。
type imageFile struct {
	path, name string
	data       []byte
	w, h       int
}

// readImageFile は .565 のファイルを読み、ヘッダと大きさを確かめる。
// 名前は、同じディレクトリの index.json（tools/mkbg と Brain の形）にあればそれを、なければファイル名を使う。
func readImageFile(p string) (imageFile, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return imageFile{}, err
	}
	if len(b) < 8 || string(b[:4]) != "LHI1" {
		return imageFile{}, fmt.Errorf("%s は lefthand の画像（.565）ではありません。PNG などは設定 GUI で変換して送ります", p)
	}
	w, h := int(binary.LittleEndian.Uint16(b[4:])), int(binary.LittleEndian.Uint16(b[6:]))
	if len(b) != 8+w*h*2 {
		return imageFile{}, fmt.Errorf("%s：%dx%d なら %d バイトのはずが、%d バイトです", p, w, h, 8+w*h*2, len(b))
	}
	f := imageFile{path: p, data: b, w: w, h: h, name: filepath.Base(p)}
	var idx struct {
		Images map[string]struct {
			Name string `json:"name"`
		} `json:"images"`
	}
	if j, err := os.ReadFile(filepath.Join(filepath.Dir(p), "index.json")); err == nil && json.Unmarshal(j, &idx) == nil {
		if m, ok := idx.Images[strings.TrimSuffix(filepath.Base(p), ".565")]; ok && m.Name != "" {
			f.name = m.Name
		}
	}
	return f, nil
}

func putImages(c *Client, files []imageFile) (string, error) {
	if err := need(c, "image_begin"); err != nil {
		return "", err
	}
	var out []string
	for _, f := range files {
		s := sha256.Sum256(f.data)
		sum := hex.EncodeToString(s[:])
		raw, err := c.Call("image_begin", map[string]any{"sha256": sum, "bytes": len(f.data), "w": f.w, "h": f.h,
			"name": f.name, "source": "brain-deck"}, callTimeout())
		if err != nil {
			return strings.Join(out, "\n"), err
		}
		var r struct {
			ID     string `json:"id"`
			Exists bool   `json:"exists"`
			Upload string `json:"upload"`
			Chunk  int    `json:"chunk_bytes"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		if r.Exists {
			out = append(out, fmt.Sprintf("%s  もうあります（%s）", r.ID, f.name))
			continue
		}
		t0 := time.Now()
		for off := 0; off < len(f.data); off += r.Chunk {
			end := min(off+r.Chunk, len(f.data))
			if _, err := c.Call("image_chunk", map[string]any{"upload": r.Upload, "offset": off,
				"data": base64.StdEncoding.EncodeToString(f.data[off:end])}, callTimeout()); err != nil {
				c.Call("image_abort", map[string]any{"upload": r.Upload}, time.Second)
				return strings.Join(out, "\n"), err
			}
		}
		if _, err := c.Call("image_end", map[string]any{"upload": r.Upload}, callTimeout()); err != nil {
			return strings.Join(out, "\n"), err
		}
		out = append(out, fmt.Sprintf("%s  送りました（%s、%dx%d、%s、%.1f 秒）", r.ID, f.name, f.w, f.h, kb(int64(len(f.data))), time.Since(t0).Seconds()))
	}
	return strings.Join(out, "\n"), nil
}

func pruneImages(c *Client, dry bool) (string, error) {
	if err := need(c, "prune_images"); err != nil {
		return "", err
	}
	raw, err := c.Call("prune_images", map[string]any{"dry_run": dry}, callTimeout())
	if err != nil {
		return "", err
	}
	var r struct {
		Removed []string `json:"removed"`
		Freed   int64    `json:"freed_bytes"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	switch {
	case len(r.Removed) == 0:
		return "使っていない画像はありません", nil
	case dry:
		return fmt.Sprintf("消す画像（%d 枚、%s）：%s", len(r.Removed), kb(r.Freed), strings.Join(r.Removed, " ")), nil
	}
	return fmt.Sprintf("使っていない画像を %d 枚消しました（%s）：%s", len(r.Removed), kb(r.Freed), strings.Join(r.Removed, " ")), nil
}
