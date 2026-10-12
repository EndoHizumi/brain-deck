package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------- app（自分で足すアプリ。フェーズ K5） ----------
//
// フォルダ（app.yaml とスクリプト）を Brain の /var/lib/lefthand/apps/<名前>/ に置く（upload の kind: app）。
// 中身は Brain が確かめる（名前、ふつうのファイルだけ、大きさ、app.yaml）。ここでは送る前に、分かることだけを確かめる。
// 書き方は docs/apps.md。

const appUsage = `app のあとには、list（省略可）、install <フォルダ>...、rm <名前>... を書きます`

var (
	appNamePat = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	appFilePat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

const (
	appMaxFilesPC = 32
	appMaxFilePC  = 256 << 10
	appMaxTotalPC = 1 << 20
)

type appInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Every       string `json:"every,omitempty"`
	Bytes       int64  `json:"bytes"`
	Files       int    `json:"files"`
	Error       string `json:"error,omitempty"`
	Status      string `json:"status,omitempty"`
	LastRun     string `json:"last_run,omitempty"`
}

func appCommand(o *options) (func(*Client) (string, error), error) {
	a := o.args[1:]
	sub := "list"
	if len(a) > 0 {
		sub, a = a[0], a[1:]
	}
	switch sub {
	case "list", "ls":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		return func(c *Client) (string, error) { return listApps(c, o.json) }, nil
	case "install", "add":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("app install のあとに、アプリのフォルダ（app.yaml とスクリプト）を書きます")
		}
		var packs []appPack
		for _, dir := range a {
			p, err := packApp(dir) // 送る前に、すべて確かめる
			if err != nil {
				return nil, usageError(err.Error())
			}
			packs = append(packs, p)
		}
		return func(c *Client) (string, error) { return installApps(c, packs) }, nil
	case "rm", "delete", "remove":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("app rm のあとに、消すアプリの名前（app list）を書きます")
		}
		return func(c *Client) (string, error) { return rmApps(c, a) }, nil
	}
	return nil, usageError(appUsage)
}

func fetchApps(c *Client) ([]appInfo, string, error) {
	if err := need(c, "list_apps"); err != nil {
		return nil, "", err
	}
	raw, err := c.Call("list_apps", nil, callTimeout())
	if err != nil {
		return nil, "", err
	}
	var r struct {
		Apps []appInfo `json:"apps"`
		Dir  string    `json:"dir"`
	}
	err = json.Unmarshal(raw, &r)
	return r.Apps, r.Dir, err
}

func listApps(c *Client, asJSON bool) (string, error) {
	l, dir, err := fetchApps(c)
	if err != nil {
		return "", err
	}
	if asJSON {
		b, _ := json.MarshalIndent(l, "", "  ")
		return string(b), nil
	}
	if len(l) == 0 {
		return "Brain に自分で足したアプリはありません（brain-deck app install <フォルダ>。書き方は docs/apps.md）", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", dir)
	for _, a := range l {
		if a.Error != "" {
			fmt.Fprintf(&b, "  %-16s 使えません：%s\n", a.Name, a.Error)
			continue
		}
		how := "stream（動き続ける）"
		if a.Mode != "stream" {
			how = a.Every + " ごと"
			if a.Every == "off" {
				how = "タップやボタンのときだけ"
			}
		}
		fmt.Fprintf(&b, "  %-16s %s　%s　%d ファイル %s", a.Name, a.Title, how, a.Files, kb(a.Bytes))
		if a.Status != "" {
			fmt.Fprintf(&b, "　%s", a.Status)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// appPack は、送るアプリ（名前と tar.gz）。
type appPack struct {
	name, dir, title string
	files            int
	data             []byte
}

// packApp は、フォルダを確かめて tar.gz にする。名前は app.yaml の name、なければフォルダの名前。
func packApp(dir string) (appPack, error) {
	p := appPack{dir: dir}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return p, err
	}
	var m struct {
		Name  string `yaml:"name"`
		Title string `yaml:"title"`
		Run   string `yaml:"run"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "app.yaml"))
	if err != nil {
		return p, fmt.Errorf("%s に app.yaml がありません（docs/apps.md）", dir)
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return p, fmt.Errorf("%s/app.yaml：%v", dir, err)
	}
	abs, _ := filepath.Abs(dir)
	p.name = m.Name
	if p.name == "" {
		p.name = filepath.Base(abs)
	}
	if !appNamePat.MatchString(p.name) {
		return p, fmt.Errorf("アプリの名前 %q は、英小文字と数字と _ - の 32 字までにします（app.yaml の name か、フォルダの名前）", p.name)
	}
	p.title = m.Title
	if m.Run == "" {
		m.Run = "main.sh"
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	var total int64
	names := []string{}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue // .DS_Store などは送らない
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	hasRun := false
	for _, n := range names {
		fp := filepath.Join(dir, n)
		fi, err := os.Lstat(fp)
		if err != nil {
			return p, err
		}
		if !fi.Mode().IsRegular() || !appFilePat.MatchString(n) {
			return p, fmt.Errorf("%s：アプリのフォルダには、英数字と _ . - の名前のふつうのファイルだけを置きます（フォルダやリンクは置けません）", fp)
		}
		if fi.Size() > appMaxFilePC {
			return p, fmt.Errorf("%s：1 つのファイルは %s までです", fp, kb(appMaxFilePC))
		}
		total += fi.Size()
		data, err := os.ReadFile(fp)
		if err != nil {
			return p, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: fi.ModTime()}); err != nil {
			return p, err
		}
		tw.Write(data)
		p.files++
		if n == m.Run {
			hasRun = true
		}
	}
	if p.files > appMaxFilesPC || total > appMaxTotalPC {
		return p, fmt.Errorf("%s：1 つのアプリは %d ファイル、%s までです", dir, appMaxFilesPC, kb(appMaxTotalPC))
	}
	if !hasRun {
		return p, fmt.Errorf("%s に、動かすスクリプト %s がありません（app.yaml の run）", dir, m.Run)
	}
	tw.Close()
	zw.Close()
	p.data = buf.Bytes()
	return p, nil
}

func installApps(c *Client, packs []appPack) (string, error) {
	if err := need(c, "list_apps"); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range packs {
		raw, err := uploadFile(c, "app", p.name, p.data)
		if err != nil {
			return b.String(), fmt.Errorf("%s：%w", p.dir, err)
		}
		var in appInfo
		json.Unmarshal(raw, &in)
		fmt.Fprintf(&b, "%s を Brain に置きました（%s、%d ファイル）\n", in.Name, in.Title, p.files)
		fmt.Fprintf(&b, "  セルに書く：{widget: app, name: %s}", in.Name)
		if in.Icon != "" {
			fmt.Fprintf(&b, "　ホーム画面：{name: %s, icon: %s, layer: <そのレイヤー>}", in.Title, in.Icon)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func rmApps(c *Client, names []string) (string, error) {
	if err := need(c, "delete_app"); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, n := range names {
		if _, err := c.Call("delete_app", map[string]any{"app": n}, callTimeout()); err != nil {
			return b.String(), fmt.Errorf("%s：%w", n, err)
		}
		fmt.Fprintf(&b, "%s を消しました（アプリが書いたデータも）\n", n)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
