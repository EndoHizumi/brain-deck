package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// ---------- read（読み物） ----------
//
// 青空文庫のテキスト、DRM のない EPUB、Web の記事と RSS を、Brain の読み物のアプリ（widget: reader）に取り込む。
// 重い処理（文字コード、青空文庫の注記、EPUB の HTML、記事の本文の取り出し）は、ここ（PC）で行い、Brain には
// 段落、見出し、ルビ、傍点、縦中横だけの簡単な形（readDoc。gzip した JSON）を送る（upload_* の kind: read）。
// DRM のかかった電子書籍は扱わない（EPUB に暗号化の印があれば断る）。DRM を外す仕組みもない。

const readUsage = `read のあとには、list（省略可）、add <ファイル | URL>...、rm <番号|id>...、
feed add <RSS の URL> [--limit-items N]、feed list、feed rm <番号>、feed sync を書きます`

type readDoc struct {
	Format int      `json:"format"`
	Title  string   `json:"title"`
	Author string   `json:"author,omitempty"`
	Kind   string   `json:"kind"`
	Source string   `json:"source,omitempty"`
	Flow   string   `json:"flow,omitempty"`
	Date   string   `json:"date,omitempty"`
	Blocks []rBlock `json:"blocks"`
}

type rBlock struct {
	T      string `json:"t"`
	L      int    `json:"l,omitempty"`
	Indent int    `json:"i,omitempty"`
	R      []rRun `json:"r,omitempty"`
}

type rRun struct {
	S    string `json:"s"`
	Ruby string `json:"ruby,omitempty"`
	Em   bool   `json:"em,omitempty"`
	TCY  bool   `json:"tcy,omitempty"`
}

func (d *readDoc) chars() int {
	n := 0
	for _, b := range d.Blocks {
		for _, r := range b.R {
			n += utf8.RuneCountInString(r.S)
		}
	}
	return n
}

// encode は、Brain に送る形（gzip した JSON）にする。
func (d *readDoc) encode() ([]byte, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(b)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type readInfo struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Author   string    `json:"author"`
	Kind     string    `json:"kind"`
	Source   string    `json:"source"`
	Date     string    `json:"date"`
	Bytes    int64     `json:"bytes"`
	Chars    int       `json:"chars"`
	Added    time.Time `json:"added"`
	Progress float64   `json:"progress"`
	Pos      *struct{} `json:"pos"`
}

type readList struct {
	Reads   []readInfo `json:"reads"`
	Total   int64      `json:"total_bytes"`
	Limit   int64      `json:"limit_bytes"`
	Free    int64      `json:"free_bytes"`
	Reserve int64      `json:"reserve_bytes"`
}

// readImport は、取り込んで Brain に送る 1 つ。
type readImport struct {
	what string
	doc  readDoc
}

func readCommand(o *options) (func(*Client) (string, error), error) {
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
		return func(c *Client) (string, error) { return listReads(c, o.json) }, nil
	case "add":
		if err := o.checkSub("o"); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("read add のあとに、青空文庫のテキスト（.txt か .zip）、EPUB、Web のページの URL を書きます")
		}
		var docs []readImport
		f := newFetcher(o.fetchTimeout)
		for _, src := range a {
			d, err := importRead(f, src)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", src, err)
			}
			docs = append(docs, readImport{what: src, doc: d})
		}
		if out := o.vals["o"]; out != "" {
			// Brain に送らずに、送る形（gzip した JSON）をファイルに書く（確かめるため）
			if len(docs) != 1 {
				return nil, usageError("-o を書くときは、取り込むものを 1 つだけ書きます")
			}
			data, err := docs[0].doc.encode()
			if err == nil {
				err = os.WriteFile(out, data, 0o644)
			}
			if err != nil {
				return nil, err
			}
			d := docs[0].doc
			return nil, localResult(fmt.Sprintf("「%s」（%s、%d ブロック、%d 文字）を %s に書きました（%s）", d.Title, d.Author, len(d.Blocks), d.chars(), out, kb(int64(len(data)))))
		}
		if o.timeout == defaultTimeout {
			o.timeout = time.Duration(len(docs)) * 2 * time.Minute
		}
		return func(c *Client) (string, error) { return putReads(c, docs) }, nil
	case "rm", "delete":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("read rm のあとに、消す読み物の番号か id（read list）を書きます")
		}
		return func(c *Client) (string, error) { return rmReads(c, a) }, nil
	case "feed":
		return feedCommand(o, a)
	}
	return nil, usageError(readUsage)
}

// importRead は、ファイルか URL を読み物にする。
func importRead(f *fetcher, src string) (readDoc, error) {
	if u, err := url.Parse(src); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		body, ctype, final, err := f.get(src)
		if err != nil {
			return readDoc{}, err
		}
		switch {
		case strings.HasSuffix(strings.ToLower(u.Path), ".zip") || strings.HasSuffix(strings.ToLower(u.Path), ".txt") && strings.Contains(u.Host, "aozora"):
			t, err := decodeAozora(body)
			if err != nil {
				return readDoc{}, err
			}
			d := parseAozora(t)
			d.Source = src
			return d, nil
		case strings.HasSuffix(strings.ToLower(u.Path), ".epub"):
			d, err := parseEPUB(body)
			d.Source = src
			return d, err
		}
		d, err := parseArticle(body, ctype, final)
		d.Source = src
		return d, err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return readDoc{}, err
	}
	name := filepath.Base(src)
	switch strings.ToLower(filepath.Ext(src)) {
	case ".epub":
		d, err := parseEPUB(b)
		d.Source = name
		return d, err
	case ".html", ".htm", ".xhtml":
		d, err := parseArticle(b, "text/html", nil)
		d.Source = name
		return d, err
	}
	t, err := decodeAozora(b)
	if err != nil {
		return readDoc{}, err
	}
	d := parseAozora(t)
	d.Source = name
	if !strings.Contains(t, "《") && !strings.Contains(t, "［＃") && !strings.Contains(t, "底本") {
		d.Kind = "text" // 注記のない、ただのテキスト
	}
	return d, nil
}

func fetchReads(c *Client) (readList, error) {
	var l readList
	if err := need(c, "list_reads"); err != nil {
		return l, err
	}
	raw, err := c.Call("list_reads", nil, callTimeout())
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(raw, &l)
	return l, err
}

func listReads(c *Client, asJSON bool) (string, error) {
	l, err := fetchReads(c)
	if err != nil {
		return "", err
	}
	if asJSON {
		b, _ := json.MarshalIndent(l, "", "  ")
		return string(b), nil
	}
	var b strings.Builder
	for i, r := range l.Reads {
		prog := ""
		if r.Pos != nil {
			prog = fmt.Sprintf("  しおり %d%%", int(r.Progress*100+0.5))
		}
		fmt.Fprintf(&b, "%2d  %s  %-6s %s%s  %s、%d 文字%s\n", i+1, r.ID, r.Kind, r.Title, map[bool]string{true: "（" + r.Author + "）", false: ""}[r.Author != ""],
			kb(r.Bytes), r.Chars, prog)
	}
	fmt.Fprintf(&b, "%d 件、%s（上限 %s）。SD カードの空き %s（%s は残す）", len(l.Reads), kb(l.Total), kb(l.Limit), kb(l.Free), kb(l.Reserve))
	return b.String(), nil
}

func putReads(c *Client, docs []readImport) (string, error) {
	if err := need(c, "list_reads"); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, d := range docs {
		data, err := d.doc.encode()
		if err != nil {
			return b.String(), err
		}
		raw, err := uploadFile(c, "read", d.doc.Title, data)
		if err != nil {
			return b.String(), fmt.Errorf("%s: %w", d.what, err)
		}
		var r struct {
			ID    string `json:"id"`
			Chars int    `json:"chars"`
		}
		json.Unmarshal(raw, &r)
		fmt.Fprintf(&b, "%s → %s「%s」（%d 文字、%s を送った）\n", d.what, r.ID, d.doc.Title, r.Chars, kb(int64(len(data))))
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func readRef(l readList, ref string) (string, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		if n < 1 || n > len(l.Reads) {
			return "", usageError(fmt.Sprintf("読み物の番号 %d はありません（1〜%d。brain-deck read list）", n, len(l.Reads)))
		}
		return l.Reads[n-1].ID, nil
	}
	for _, r := range l.Reads {
		if r.ID == ref {
			return r.ID, nil
		}
	}
	return "", usageError("読み物 " + ref + " はありません（brain-deck read list）")
}

func rmReads(c *Client, refs []string) (string, error) {
	l, err := fetchReads(c)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, r := range refs {
		id, err := readRef(l, r)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if _, err := c.Call("delete_read", map[string]any{"read": id}, callTimeout()); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%d 件を消しました", len(ids)), nil
}

// ---------- RSS（read feed） ----------
//
// 購読する RSS は ~/.config/brain-deck/feeds.yaml（read feed add が書く）。read feed sync で、それぞれの新しい記事
// （Brain にまだない URL）を、新しいものから取り込む。1 つの RSS で Brain に置く記事は max_items（既定 10）まで
// （超えるときは足さずに知らせる。古い記事は自分で read rm で消す）。

type feedsFile struct {
	Feeds []feedEntry `yaml:"feeds"`
}

type feedEntry struct {
	URL      string `yaml:"url"`
	Title    string `yaml:"title,omitempty"`
	MaxItems int    `yaml:"max_items,omitempty"`
}

const defaultFeedItems = 10

func feedsPath() (string, error) {
	return filepath.Join(filepath.Dir(calendarsPath()), "feeds.yaml"), nil
}

func loadFeeds() (feedsFile, string, error) {
	var f feedsFile
	p, err := feedsPath()
	if err != nil {
		return f, "", err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return f, p, nil
	}
	if err != nil {
		return f, p, err
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return f, p, fmt.Errorf("%s: %w", p, err)
	}
	return f, p, nil
}

func saveFeeds(f feedsFile, p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := yaml.Marshal(f)
	return os.WriteFile(p, append([]byte("# brain-deck read feed add が書く。読み物に取り込む RSS（brain-deck read feed sync）\n"), b...), 0o644)
}

func feedCommand(o *options, a []string) (func(*Client) (string, error), error) {
	if len(a) == 0 {
		return nil, usageError(readUsage)
	}
	sub, a := a[0], a[1:]
	switch sub {
	case "add":
		if err := o.checkSub("limit-items"); err != nil {
			return nil, err
		}
		if len(a) != 1 {
			return nil, usageError("read feed add <RSS の URL> と書きます")
		}
		u, err := url.Parse(a[0])
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, usageError("RSS の URL（http か https）を書きます")
		}
		n := 0
		if v := o.vals["limit-items"]; v != "" {
			if n, err = strconv.Atoi(v); err != nil || n < 1 || n > 100 {
				return nil, usageError("--limit-items は 1〜100 です")
			}
		}
		f, p, err := loadFeeds()
		if err != nil {
			return nil, err
		}
		for _, e := range f.Feeds {
			if e.URL == a[0] {
				return nil, usageError("その RSS はもう足してあります")
			}
		}
		f.Feeds = append(f.Feeds, feedEntry{URL: a[0], MaxItems: n})
		if err := saveFeeds(f, p); err != nil {
			return nil, err
		}
		return nil, localResult(fmt.Sprintf("%s を %s に足しました。brain-deck read feed sync で取り込みます", a[0], p))
	case "list", "ls":
		f, p, err := loadFeeds()
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		for i, e := range f.Feeds {
			fmt.Fprintf(&b, "%2d  %s（%d 件まで）\n", i+1, e.URL, e.itemsMax())
		}
		fmt.Fprintf(&b, "%d 件（%s）", len(f.Feeds), p)
		return nil, localResult(b.String())
	case "rm", "delete":
		f, p, err := loadFeeds()
		if err != nil {
			return nil, err
		}
		if len(a) != 1 {
			return nil, usageError("read feed rm <番号>（read feed list）と書きます")
		}
		n, err := strconv.Atoi(a[0])
		if err != nil || n < 1 || n > len(f.Feeds) {
			return nil, usageError(fmt.Sprintf("番号は 1〜%d です", len(f.Feeds)))
		}
		u := f.Feeds[n-1].URL
		f.Feeds = append(f.Feeds[:n-1], f.Feeds[n:]...)
		if err := saveFeeds(f, p); err != nil {
			return nil, err
		}
		return nil, localResult(u + " を外しました（Brain の記事は残しています）")
	case "sync":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if o.timeout == defaultTimeout {
			o.timeout = 15 * time.Minute
		}
		return func(c *Client) (string, error) {
			out, _, err := syncFeeds(c, newFetcher(o.fetchTimeout))
			return out, err
		}, nil
	}
	return nil, usageError(readUsage)
}

func (e feedEntry) itemsMax() int {
	if e.MaxItems > 0 {
		return e.MaxItems
	}
	return defaultFeedItems
}

// localOutput は、Brain につながずに終わるコマンドの結果（run が書いて終わる）。
type localOutput string

func (l localOutput) Error() string { return string(l) }

func localResult(s string) error { return localOutput(s) }

// syncFeeds は、RSS の新しい記事を取り込む。足した数を返す。
func syncFeeds(c *Client, f *fetcher) (string, int, error) {
	ff, _, err := loadFeeds()
	if err != nil {
		return "", 0, err
	}
	if len(ff.Feeds) == 0 {
		return "RSS がありません（brain-deck read feed add <URL> で足す）", 0, nil
	}
	l, err := fetchReads(c)
	if err != nil {
		return "", 0, err
	}
	have := map[string]bool{}
	for _, r := range l.Reads {
		have[r.Source] = true
	}
	var b strings.Builder
	added := 0
	var errs []string
	for _, fe := range ff.Feeds {
		body, _, _, err := f.get(fe.URL)
		if err != nil {
			errs = append(errs, fe.URL+": "+err.Error())
			continue
		}
		items, title, err := parseFeed(body)
		if err != nil {
			errs = append(errs, fe.URL+": "+err.Error())
			continue
		}
		onBrain := 0
		for _, it := range items {
			if have[it.Link] {
				onBrain++
			}
		}
		n := 0
		for _, it := range items {
			if have[it.Link] || it.Link == "" {
				continue
			}
			if onBrain >= fe.itemsMax() {
				fmt.Fprintf(&b, "%s：Brain にもう %d 件あるので、新しい記事は足していません（read rm で消してください）\n", title, onBrain)
				break
			}
			body, ctype, final, err := f.get(it.Link)
			if err != nil {
				errs = append(errs, it.Link+": "+err.Error())
				continue
			}
			d, err := parseArticle(body, ctype, final)
			if err != nil {
				errs = append(errs, it.Link+": "+err.Error())
				continue
			}
			d.Kind, d.Source = "rss", it.Link
			if it.Title != "" {
				d.Title = it.Title
			}
			if d.Author == "" {
				d.Author = title
			}
			d.Date = it.Date
			data, err := d.encode()
			if err != nil {
				return b.String(), added, err
			}
			if _, err := uploadFile(c, "read", d.Title, data); err != nil {
				errs = append(errs, it.Link+": "+err.Error())
				continue
			}
			have[it.Link] = true
			onBrain++
			n++
			added++
		}
		fmt.Fprintf(&b, "%s：%d 件を足しました\n", title, n)
	}
	sort.Strings(errs)
	out := strings.TrimSuffix(b.String(), "\n")
	if len(errs) > 0 {
		return out, added, fmt.Errorf("取り込めなかったもの：\n%s", strings.Join(errs, "\n"))
	}
	return out, added, nil
}
