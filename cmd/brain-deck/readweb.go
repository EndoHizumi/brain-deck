package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// ---------- Web から取ってくる ----------
//
// 同じホストには、前のアクセスから readFetchGap（2 秒）あけてからアクセスする（短い間隔で続けてアクセスしない）。
// User-Agent に brain-deck の名前と版、リポジトリの URL を付ける。1 つの応答は 16 MiB まで。

const (
	readFetchGap  = 2 * time.Second
	readFetchMax  = 16 << 20
	readUserAgent = "brain-deck/%s (+https://github.com/endohizumi/brain-deck; reading for a handheld)"
)

type fetcher struct {
	client *http.Client
	mu     sync.Mutex
	last   map[string]time.Time
	gap    time.Duration
}

func newFetcher(timeout time.Duration) *fetcher {
	return &fetcher{client: &http.Client{Timeout: timeout}, last: map[string]time.Time{}, gap: readFetchGap}
}

// get は、u を取ってくる。本文、Content-Type、最後の URL（リダイレクトのあと）を返す。
func (f *fetcher) get(u string) ([]byte, string, *url.URL, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return nil, "", nil, err
	}
	f.mu.Lock()
	if t, ok := f.last[pu.Host]; ok {
		if w := f.gap - time.Since(t); w > 0 {
			time.Sleep(w)
		}
	}
	f.last[pu.Host] = time.Now()
	f.mu.Unlock()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, "", nil, err
	}
	req.Header.Set("User-Agent", fmt.Sprintf(readUserAgent, versionString()))
	req.Header.Set("Accept-Language", "ja,en;q=0.5")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, readFetchMax+1))
	if err != nil {
		return nil, "", nil, err
	}
	if len(b) > readFetchMax {
		return nil, "", nil, fmt.Errorf("%d MiB を超えるので取り込みません", readFetchMax>>20)
	}
	return b, resp.Header.Get("Content-Type"), resp.Request.URL, nil
}

// ---------- HTML から段落を取り出す（EPUB と Web の記事で共通） ----------

type htmlConv struct {
	blocks []rBlock
	cur    []rRun
	em     int
	inRT   bool
	ruby   *rRun
}

func (c *htmlConv) flush(t string, level int) {
	// 空白をまとめる
	var runs []rRun
	for _, r := range c.cur {
		if r.S == "" {
			continue
		}
		runs = append(runs, r)
	}
	c.cur = nil
	if len(runs) == 0 {
		return
	}
	runs[0].S = strings.TrimLeftFunc(runs[0].S, unicode.IsSpace)
	last := &runs[len(runs)-1]
	last.S = strings.TrimRightFunc(last.S, unicode.IsSpace)
	if len(runs) == 1 && runs[0].S == "" {
		return
	}
	c.blocks = append(c.blocks, rBlock{T: t, L: level, R: runs})
}

func (c *htmlConv) text(s string) {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' }), " ")
	if c.inRT {
		if c.ruby != nil {
			c.ruby.Ruby += strings.TrimSpace(s)
		}
		return
	}
	if c.ruby != nil {
		c.ruby.S += s
		return
	}
	c.cur = append(c.cur, rRun{S: s, Em: c.em > 0})
}

// hasClass は、要素の class に、いずれかの言葉を含むか。
func hasClass(n *html.Node, words ...string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, w := range words {
				if strings.Contains(a.Val, w) {
					return true
				}
			}
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

var blockTags = map[string]bool{"p": true, "div": true, "li": true, "blockquote": true, "pre": true, "dd": true, "dt": true, "tr": true, "section": true, "article": true, "figcaption": true, "td": true}
var skipTags = map[string]bool{"script": true, "style": true, "noscript": true, "nav": true, "header": true, "footer": true, "aside": true, "form": true, "button": true, "svg": true, "iframe": true, "rp": true, "template": true}

func (c *htmlConv) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		c.text(n.Data)
		return
	case html.ElementNode:
	default:
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch)
		}
		return
	}
	tag := n.Data
	if skipTags[tag] {
		return
	}
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.flush("p", 0)
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch)
		}
		lv := min(int(tag[1]-'0'), 3)
		c.flush("h", lv)
		return
	case "br":
		c.flush("p", 0)
		return
	case "img":
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" && false {
			c.text(alt)
		}
		return
	case "ruby":
		// <ruby>漢字<rt>かんじ</rt></ruby>（<rb> があってもよい）
		r := rRun{Em: c.em > 0}
		c.ruby = &r
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch)
		}
		c.ruby = nil
		if r.S != "" {
			c.cur = append(c.cur, r)
		}
		return
	case "rt":
		c.inRT = true
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch)
		}
		c.inRT = false
		return
	case "em", "strong", "b":
		if tag == "em" || hasClass(n, "sesame", "bouten", "em-") {
			c.em++
			defer func() { c.em-- }()
		}
	case "span":
		if hasClass(n, "sesame", "bouten", "em-sesame", "em-dot") {
			c.em++
			defer func() { c.em-- }()
		}
		if hasClass(n, "tcy", "tate-chu-yoko") {
			var buf htmlConv
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				buf.walk(ch)
			}
			var s strings.Builder
			for _, r := range buf.cur {
				s.WriteString(r.S)
			}
			c.cur = append(c.cur, rRun{S: s.String(), TCY: true, Em: c.em > 0})
			return
		}
	}
	block := blockTags[tag]
	if block {
		c.flush("p", 0)
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.walk(ch)
	}
	if block {
		c.flush("p", 0)
	}
}

// ---------- Web の記事 ----------
//
// 本文の取り出しは、ライブラリを使わない自作の簡単な方法（Readability の考え方の一部）：
// <article> か <main> があれば、その中（いくつもあれば文字の多いもの）。なければ、<p> の文字の合計がいちばん多い要素。
// nav、header、footer、aside、script、style、form は読まない。画像は扱わない（alt も入れない）。
// 題名は og:title、<h1>、<title> の順。著者は meta の author、日付は article:published_time か <time>。

func textLen(n *html.Node) int {
	if n.Type == html.TextNode {
		return len([]rune(strings.TrimSpace(n.Data)))
	}
	if n.Type == html.ElementNode && skipTags[n.Data] {
		return 0
	}
	t := 0
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		t += textLen(ch)
	}
	return t
}

func findAll(n *html.Node, pred func(*html.Node) bool, out *[]*html.Node) {
	if n.Type == html.ElementNode && pred(n) {
		*out = append(*out, n)
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		findAll(ch, pred, out)
	}
}

func metaContent(doc *html.Node, keys ...string) string {
	var metas []*html.Node
	findAll(doc, func(n *html.Node) bool { return n.Data == "meta" }, &metas)
	for _, k := range keys {
		for _, m := range metas {
			if attr(m, "property") == k || attr(m, "name") == k {
				if v := strings.TrimSpace(attr(m, "content")); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// mainContent は、記事の本文の要素を選ぶ。
func mainContent(doc *html.Node) *html.Node {
	var cands []*html.Node
	findAll(doc, func(n *html.Node) bool { return n.Data == "article" || n.Data == "main" || attr(n, "role") == "main" }, &cands)
	best, bestLen := (*html.Node)(nil), 0
	for _, c := range cands {
		if l := textLen(c); l > bestLen {
			best, bestLen = c, l
		}
	}
	if best != nil && bestLen > 200 {
		return best
	}
	// <p> の文字の合計がいちばん多い親
	score := map[*html.Node]int{}
	var ps []*html.Node
	findAll(doc, func(n *html.Node) bool { return n.Data == "p" }, &ps)
	for _, p := range ps {
		if p.Parent != nil {
			score[p.Parent] += textLen(p)
		}
	}
	for n, s := range score {
		if s > bestLen {
			best, bestLen = n, s
		}
	}
	if best == nil {
		var bodies []*html.Node
		findAll(doc, func(n *html.Node) bool { return n.Data == "body" }, &bodies)
		if len(bodies) > 0 {
			return bodies[0]
		}
		return doc
	}
	return best
}

// parseArticle は、Web のページを記事の読み物にする。
func parseArticle(body []byte, ctype string, final *url.URL) (readDoc, error) {
	r, err := charset.NewReader(bytes.NewReader(body), ctype)
	if err != nil {
		return readDoc{}, err
	}
	doc, err := html.Parse(r)
	if err != nil {
		return readDoc{}, err
	}
	d := readDoc{Format: 1, Kind: "web", Flow: "article"}
	d.Title = metaContent(doc, "og:title", "twitter:title")
	if d.Title == "" {
		var hs []*html.Node
		findAll(doc, func(n *html.Node) bool { return n.Data == "h1" || n.Data == "title" }, &hs)
		for _, h := range hs {
			var c htmlConv
			c.walk(h)
			if len(c.cur) > 0 {
				t := ""
				for _, r := range c.cur {
					t += r.S
				}
				if t = strings.TrimSpace(t); t != "" {
					d.Title = t
					break
				}
			}
		}
	}
	d.Author = metaContent(doc, "author", "article:author")
	d.Date = metaContent(doc, "article:published_time", "date")
	if len(d.Date) > 10 {
		d.Date = d.Date[:10]
	}
	var c htmlConv
	c.walk(mainContent(doc))
	c.flush("p", 0)
	d.Blocks = c.blocks
	if final != nil {
		d.Source = final.String()
	}
	if len(d.Blocks) == 0 {
		return d, errors.New("本文が見つかりません")
	}
	if d.Title == "" {
		d.Title = "（題名なし）"
	}
	// 本文の最初の見出しが題名と同じなら、外す
	if b := d.Blocks[0]; b.T == "h" && len(b.R) > 0 && strings.TrimSpace(b.R[0].S) == d.Title {
		d.Blocks = d.Blocks[1:]
	}
	if len(d.Blocks) == 0 {
		return d, errors.New("本文が見つかりません")
	}
	return d, nil
}

// ---------- EPUB ----------
//
// DRM のない EPUB（EPUB 2 と 3）。META-INF/container.xml から OPF を読み、spine の順に XHTML を段落にする。
// 暗号化の印（META-INF/encryption.xml、rights.xml）があれば、DRM のかかった本とみなして断る
// （フォントの難読化だけの encryption.xml も断る。読み物には要らない）。章（spine の 1 つ）の始まりは改ページ。

var errDRM = errors.New("DRM のかかった（暗号化された）EPUB は扱いません")

func parseEPUB(b []byte) (readDoc, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return readDoc{}, fmt.Errorf("EPUB（zip）として読めません: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	if files["META-INF/encryption.xml"] != nil || files["META-INF/rights.xml"] != nil {
		return readDoc{}, errDRM
	}
	read := func(name string) ([]byte, error) {
		f := files[name]
		if f == nil {
			return nil, fmt.Errorf("%s がありません", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 32<<20))
	}
	cb, err := read("META-INF/container.xml")
	if err != nil {
		return readDoc{}, err
	}
	var cont struct {
		Rootfiles []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xml.Unmarshal(cb, &cont); err != nil || len(cont.Rootfiles) == 0 {
		return readDoc{}, errors.New("container.xml に OPF がありません")
	}
	opfPath := cont.Rootfiles[0].Path
	ob, err := read(opfPath)
	if err != nil {
		return readDoc{}, err
	}
	var opf struct {
		Title    []string `xml:"metadata>title"`
		Creator  []string `xml:"metadata>creator"`
		Date     []string `xml:"metadata>date"`
		Manifest []struct {
			ID   string `xml:"id,attr"`
			Href string `xml:"href,attr"`
			Type string `xml:"media-type,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"spine>itemref"`
	}
	if err := xml.Unmarshal(ob, &opf); err != nil {
		return readDoc{}, fmt.Errorf("OPF: %w", err)
	}
	d := readDoc{Format: 1, Kind: "epub", Flow: "book"}
	if len(opf.Title) > 0 {
		d.Title = strings.TrimSpace(opf.Title[0])
	}
	if len(opf.Creator) > 0 {
		d.Author = strings.TrimSpace(strings.Join(opf.Creator, "、"))
	}
	if len(opf.Date) > 0 {
		d.Date = strings.TrimSpace(opf.Date[0])
	}
	hrefs := map[string]string{}
	for _, it := range opf.Manifest {
		hrefs[it.ID] = it.Href
	}
	dir := path.Dir(opfPath)
	for i, s := range opf.Spine {
		h, ok := hrefs[s.IDRef]
		if !ok {
			continue
		}
		name := path.Clean(path.Join(dir, strings.SplitN(h, "#", 2)[0]))
		if un, err := url.PathUnescape(name); err == nil {
			name = un
		}
		xb, err := read(name)
		if err != nil {
			continue
		}
		hd, err := html.Parse(bytes.NewReader(xb))
		if err != nil {
			continue
		}
		var c htmlConv
		var bodies []*html.Node
		findAll(hd, func(n *html.Node) bool { return n.Data == "body" }, &bodies)
		if len(bodies) == 0 {
			continue
		}
		c.walk(bodies[0])
		c.flush("p", 0)
		if len(c.blocks) == 0 {
			continue
		}
		if i > 0 && len(d.Blocks) > 0 {
			d.Blocks = append(d.Blocks, rBlock{T: "pb"})
		}
		d.Blocks = append(d.Blocks, c.blocks...)
	}
	if len(d.Blocks) == 0 {
		return d, errors.New("本文がありません")
	}
	if d.Title == "" {
		d.Title = "（題名なし）"
	}
	return d, nil
}

// ---------- RSS と Atom ----------

type feedItem struct {
	Title, Link, Date string
}

// parseFeed は、RSS 2.0、RSS 1.0（RDF）、Atom の記事を、新しいものから返す。
func parseFeed(b []byte) ([]feedItem, string, error) {
	var x struct {
		XMLName xml.Name
		Title   string `xml:"title"`
		Channel struct {
			Title string `xml:"title"`
			Items []struct {
				Title   string `xml:"title"`
				Link    string `xml:"link"`
				PubDate string `xml:"pubDate"`
				Date    string `xml:"date"`
			} `xml:"item"`
		} `xml:"channel"`
		Items []struct { // RSS 1.0
			Title string `xml:"title"`
			Link  string `xml:"link"`
			Date  string `xml:"date"`
		} `xml:"item"`
		Entries []struct { // Atom
			Title string `xml:"title"`
			Links []struct {
				Href string `xml:"href,attr"`
				Rel  string `xml:"rel,attr"`
			} `xml:"link"`
			Updated   string `xml:"updated"`
			Published string `xml:"published"`
		} `xml:"entry"`
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.CharsetReader = charset.NewReaderLabel
	dec.Strict = false
	if err := dec.Decode(&x); err != nil {
		return nil, "", fmt.Errorf("RSS として読めません: %w", err)
	}
	var out []feedItem
	title := strings.TrimSpace(x.Channel.Title)
	for _, it := range x.Channel.Items {
		date := it.PubDate
		if t, err := time.Parse(time.RFC1123Z, strings.TrimSpace(it.PubDate)); err == nil {
			date = t.Format("2006-01-02")
		} else if t, err := time.Parse(time.RFC1123, strings.TrimSpace(it.PubDate)); err == nil {
			date = t.Format("2006-01-02")
		} else if it.Date != "" && len(it.Date) >= 10 {
			date = it.Date[:10]
		}
		out = append(out, feedItem{strings.TrimSpace(it.Title), strings.TrimSpace(it.Link), date})
	}
	for _, it := range x.Items {
		out = append(out, feedItem{strings.TrimSpace(it.Title), strings.TrimSpace(it.Link), firstN(it.Date, 10)})
	}
	if x.XMLName.Local == "feed" {
		title = strings.TrimSpace(x.Title)
		for _, e := range x.Entries {
			link := ""
			for _, l := range e.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = l.Href
					break
				}
			}
			date := e.Published
			if date == "" {
				date = e.Updated
			}
			out = append(out, feedItem{strings.TrimSpace(e.Title), strings.TrimSpace(link), firstN(date, 10)})
		}
	}
	if title == "" {
		title = "RSS"
	}
	return out, title, nil
}

func firstN(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
