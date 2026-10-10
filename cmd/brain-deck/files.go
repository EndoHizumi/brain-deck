package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ---------- files（Brain のファイル） ----------
//
// Brain のファイルエクスプローラ（widget: files）と同じ、見られる場所（設定の files.roots）のファイルの一覧、取り出し、
// 足す、消す。場所の外、/boot や /etc のパスワードなどは Brain が断る。足すのと消すのは delete: true の場所だけ。
// 送るのは get_file と upload_*（kind: file）で、96 KiB ずつ。

const filesUsage = `files のあとには、ls [<Brain のフォルダ>] [--sort size]、get <Brain のファイル> [-o <ファイル>] [--force]、` +
	`put <ファイル>... <Brain のフォルダ>、rm <Brain のファイル>... を書きます`

type fileEntry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Dir   bool      `json:"dir,omitempty"`
	Link  bool      `json:"link,omitempty"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

type fileList struct {
	Path      string      `json:"path"`
	Root      string      `json:"root,omitempty"`
	Delete    bool        `json:"delete,omitempty"`
	Put       bool        `json:"put,omitempty"`
	Parent    string      `json:"parent,omitempty"`
	Entries   []fileEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
	Partial   bool        `json:"partial,omitempty"`
	Free      int64       `json:"free_bytes"`
	Total     int64       `json:"total_bytes"`
}

func filesCommand(o *options) (func(*Client) (string, error), error) {
	a := o.args[1:]
	sub := "ls"
	if len(a) > 0 {
		sub, a = a[0], a[1:]
	}
	switch sub {
	case "ls", "list":
		if err := o.checkSub("sort"); err != nil {
			return nil, err
		}
		if s := o.vals["sort"]; s != "" && s != "size" && s != "name" {
			return nil, usageError("--sort は size か name です")
		}
		if len(a) > 1 {
			return nil, usageError("files ls のあとには、フォルダを 1 つだけ書きます")
		}
		dir := ""
		if len(a) == 1 && a[0] != "/" {
			dir = a[0]
		}
		return func(c *Client) (string, error) { return listFiles(c, dir, o.vals["sort"] == "size", o.json) }, nil
	case "get":
		if err := o.checkSub("o force"); err != nil {
			return nil, err
		}
		if len(a) != 1 {
			return nil, usageError("files get <Brain のファイル> [-o <ファイル>] と書きます")
		}
		out := o.vals["o"]
		if out == "" {
			out = path.Base(a[0])
		}
		return func(c *Client) (string, error) { return getFile(c, a[0], out, o.flags["force"]) }, nil
	case "put":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if len(a) < 2 {
			return nil, usageError("files put <ファイル>... <Brain のフォルダ> と書きます")
		}
		srcs, dir := a[:len(a)-1], a[len(a)-1]
		for _, s := range srcs {
			if fi, err := os.Stat(s); err != nil || !fi.Mode().IsRegular() {
				return nil, usageError(s + " は読めるファイルではありません")
			}
		}
		return func(c *Client) (string, error) { return putFiles(c, srcs, dir) }, nil
	case "rm", "delete":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("files rm のあとに、消す Brain のファイルを書きます（フォルダは空のときだけ消せます）")
		}
		return func(c *Client) (string, error) { return rmFiles(c, a) }, nil
	}
	return nil, usageError(filesUsage)
}

func fetchFiles(c *Client, dir string, bySize bool) (*fileList, error) {
	if err := need(c, "list_files"); err != nil {
		return nil, err
	}
	// 大きい順はフォルダの中を数えるので、Brain で 8 秒までかかる
	raw, err := c.Call("list_files", map[string]any{"path": dir, "by_size": bySize}, callTimeout()+10*time.Second)
	if err != nil {
		return nil, err
	}
	var l fileList
	err = json.Unmarshal(raw, &l)
	return &l, err
}

func listFiles(c *Client, dir string, bySize, asJSON bool) (string, error) {
	l, err := fetchFiles(c, dir, bySize)
	if err != nil {
		return "", err
	}
	if asJSON {
		b, _ := json.MarshalIndent(l, "", "  ")
		return string(b), nil
	}
	var b strings.Builder
	if l.Path == "" {
		b.WriteString("見られる場所\n")
	} else {
		mode := "読むだけ"
		if l.Delete {
			mode = "足す・消すことができる"
		}
		fmt.Fprintf(&b, "%s（%s。%s）\n", l.Path, l.Root, mode)
	}
	for _, e := range l.Entries {
		name, size := e.Name, kb(e.Size)
		switch {
		case l.Path == "":
			name, size = e.Name+"  "+e.Path, ""
		case e.Dir:
			name += "/"
			if !bySize {
				size = ""
			}
		case e.Link:
			name += " →（リンク。たどらない）"
		}
		when := e.MTime.Local().Format("2006-01-02 15:04")
		if e.MTime.IsZero() {
			when = "（まだない）"
		}
		fmt.Fprintf(&b, "%9s  %s  %s\n", size, when, name)
	}
	if l.Truncated {
		b.WriteString("（多いので、最初の 2000 個だけ）\n")
	}
	if l.Partial {
		b.WriteString("（時間内に数え終わらなかったフォルダは、途中までの合計）\n")
	}
	fmt.Fprintf(&b, "%d 個。SD カードの空き %s", len(l.Entries), kb(l.Free))
	return b.String(), nil
}

// readBrainFile は、get_file で Brain のファイルを読む。
func readBrainFile(c *Client, p string, progress func(done, total int64)) ([]byte, error) {
	if err := need(c, "get_file"); err != nil {
		return nil, err
	}
	var buf []byte
	for {
		raw, err := c.Call("get_file", map[string]any{"path": p, "offset": len(buf)}, callTimeout())
		if err != nil {
			return nil, err
		}
		var r struct {
			Total int64  `json:"total"`
			Data  string `json:"data"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		b, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			return nil, err
		}
		buf = append(buf, b...)
		if progress != nil {
			progress(int64(len(buf)), r.Total)
		}
		if int64(len(buf)) >= r.Total || len(b) == 0 {
			return buf, nil
		}
	}
}

func getFile(c *Client, p, out string, force bool) (string, error) {
	if !force {
		if _, err := os.Stat(out); err == nil {
			return "", usageError(out + " はもうあります（--force で上書き）")
		}
	}
	b, err := readBrainFile(c, p, nil)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s を %s に書きました（%s）", p, out, kb(int64(len(b)))), nil
}

func putFiles(c *Client, srcs []string, dir string) (string, error) {
	if err := need(c, "list_files"); err != nil {
		return "", err
	}
	var lines []string
	for _, s := range srcs {
		b, err := os.ReadFile(s)
		if err != nil {
			return strings.Join(lines, "\n"), err
		}
		raw, err := uploadFileWith(c, "file", filepath.Base(s), b, map[string]any{"path": dir})
		if err != nil {
			return strings.Join(lines, "\n"), err
		}
		var r struct {
			Path string `json:"path"`
		}
		json.Unmarshal(raw, &r)
		lines = append(lines, fmt.Sprintf("%s を %s に足しました（%s）", s, r.Path, kb(int64(len(b)))))
	}
	return strings.Join(lines, "\n"), nil
}

func rmFiles(c *Client, paths []string) (string, error) {
	if err := need(c, "delete_file"); err != nil {
		return "", err
	}
	var lines []string
	for _, p := range paths {
		if _, err := c.Call("delete_file", map[string]any{"path": p}, callTimeout()); err != nil {
			return strings.Join(lines, "\n"), err
		}
		lines = append(lines, p+" を消しました")
	}
	return strings.Join(lines, "\n"), nil
}
