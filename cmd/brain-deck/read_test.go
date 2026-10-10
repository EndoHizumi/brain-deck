//go:build linux

package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func blockText(b rBlock) string {
	var s strings.Builder
	for _, r := range b.R {
		s.WriteString(r.S)
	}
	return s.String()
}

func TestAozoraKumonoIto(t *testing.T) {
	b, err := os.ReadFile("../../testdata/reader/kumono_ito_aozora.zip")
	if err != nil {
		t.Fatal(err)
	}
	text, err := decodeAozora(b)
	if err != nil {
		t.Fatal(err)
	}
	d := parseAozora(text)
	if d.Title != "蜘蛛の糸" || d.Author != "芥川龍之介" || d.Kind != "aozora" {
		t.Fatalf("title %q author %q", d.Title, d.Author)
	}
	var heads, info []string
	ruby := map[string]string{}
	for _, bl := range d.Blocks {
		switch bl.T {
		case "h":
			heads = append(heads, blockText(bl))
		case "info":
			info = append(info, blockText(bl))
		}
		for _, r := range bl.R {
			if r.Ruby != "" {
				ruby[r.S] = r.Ruby
			}
			if strings.ContainsAny(r.S, "《》｜［＃") {
				t.Errorf("an annotation is left in %q", r.S)
			}
		}
	}
	if strings.Join(heads, ",") != "一,二,三" {
		t.Errorf("headings %q", heads)
	}
	// ルビ：｜ のあるものと、ないもの（直前の漢字の続き）
	for base, want := range map[string]string{"御釈迦様": "おしゃかさま", "蓮池": "はすいけ", "地獄": "じごく", "覗": "のぞ", "下": "おろ"} {
		if ruby[base] != want {
			t.Errorf("ruby of %q is %q, want %q", base, ruby[base], want)
		}
	}
	// 底本、入力者などは消さずに残す
	all := strings.Join(info, "\n")
	for _, s := range []string{"底本：「芥川龍之介全集2」ちくま文庫、筑摩書房", "入力：平山誠、野口英司", "校正：もりみつじゅんじ", "青空文庫"} {
		if !strings.Contains(all, s) {
			t.Errorf("the colophon lost %q", s)
		}
	}
	// 外字（JIS X 0213 の面区点番号だけ）は ※ のまま、注記は読み捨て、本文は残す
	body := ""
	for _, bl := range d.Blocks {
		body += blockText(bl)
	}
	if !strings.Contains(body, "※陀多と云う男が一人") || strings.Contains(body, "特のへん") {
		t.Error("gaiji handling")
	}
	// 最後の行（［＃地から１字上げ］（大正七年四月十六日））の注記も読み捨てる
	if !strings.Contains(body, "（大正七年四月十六日）") {
		t.Error("the date line is lost")
	}
}

func TestAozoraNotes(t *testing.T) {
	src := "題\n著者\n\n" +
		"［＃３字下げ］見出し［＃「見出し」は大見出し］\n" +
		"　これは｜自作《じさく》の文章《ぶんしょう》です。［＃「文章」に傍点］\n" +
		"第12回［＃「12」は縦中横］と［＃傍点］強調［＃傍点終わり］。※［＃「木＋世」、U+67BB、1-2］もある。［＃知らない注記］残る\n" +
		"［＃ここから２字下げ］\n下げた段落\n［＃ここで字下げ終わり］\n［＃改ページ］\n次の頁\n"
	d := parseAozora(src)
	if d.Title != "題" || d.Author != "著者" {
		t.Fatal(d.Title)
	}
	if d.Blocks[0].T != "h" || d.Blocks[0].L != 1 || blockText(d.Blocks[0]) != "見出し" {
		t.Errorf("heading %+v", d.Blocks[0])
	}
	p := d.Blocks[1]
	var em, rb []string
	for _, r := range p.R {
		if r.Em {
			em = append(em, r.S)
		}
		if r.Ruby != "" {
			rb = append(rb, r.S+"="+r.Ruby)
		}
	}
	if strings.Join(rb, ",") != "自作=じさく,文章=ぶんしょう" || strings.Join(em, ",") != "文章" {
		t.Errorf("runs %+v", p.R)
	}
	q := d.Blocks[2]
	var tcy, em2 []string
	for _, r := range q.R {
		if r.TCY {
			tcy = append(tcy, r.S)
		}
		if r.Em {
			em2 = append(em2, r.S)
		}
	}
	if strings.Join(tcy, ",") != "12" || strings.Join(em2, ",") != "強調" || !strings.Contains(blockText(q), "枻もある。残る") {
		t.Errorf("tcy %q em %q text %q", tcy, em2, blockText(q))
	}
	var kinds []string
	for _, b := range d.Blocks[3:] {
		kinds = append(kinds, b.T+":"+blockText(b)+":"+string(rune('0'+b.Indent)))
	}
	if strings.Join(kinds, "|") != "p::2|p:下げた段落:2|p::0|pb::0|p:次の頁:0" {
		t.Errorf("blocks %q", kinds)
	}
}

// makeEPUB は、自作の文章の EPUB を作る。encrypt なら暗号化の印を入れる。
func makeEPUB(t *testing.T, encrypt bool) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, _ := zw.Create(name)
		io.WriteString(w, body)
	}
	add("mimetype", "application/epub+zip")
	add("META-INF/container.xml", `<?xml version="1.0"?><container xmlns="urn:oasis:names:tc:opendocument:xmlns:container" version="1.0"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`)
	if encrypt {
		add("META-INF/encryption.xml", `<encryption/>`)
	}
	add("OEBPS/content.opf", `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>テストの本</dc:title><dc:creator>自分</dc:creator></metadata>
<manifest><item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/><item id="c2" href="text/ch%202.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="c1"/><itemref idref="c2"/></spine></package>`)
	add("OEBPS/ch1.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><body><h1>第一章</h1><p>これは<ruby>自作<rt>じさく</rt></ruby>の<em class="sesame">文章</em>です。</p><p>二つ目の<span class="tcy">12</span>段落。<img src="a.png" alt="絵"/></p></body></html>`)
	add("OEBPS/text/ch 2.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><body><h2>第二章</h2><p>終わり。</p></body></html>`)
	zw.Close()
	return buf.Bytes()
}

func TestEPUB(t *testing.T) {
	d, err := parseEPUB(makeEPUB(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "テストの本" || d.Author != "自分" {
		t.Errorf("meta %q %q", d.Title, d.Author)
	}
	var kinds []string
	for _, b := range d.Blocks {
		kinds = append(kinds, b.T+":"+blockText(b))
	}
	if strings.Join(kinds, "|") != "h:第一章|p:これは自作の文章です。|p:二つ目の12段落。|pb:|h:第二章|p:終わり。" {
		t.Errorf("blocks %q", kinds)
	}
	p := d.Blocks[1].R
	if p[1].S != "自作" || p[1].Ruby != "じさく" || !p[3].Em || p[3].S != "文章" {
		t.Errorf("runs %+v", p)
	}
	if !d.Blocks[2].R[1].TCY {
		t.Errorf("tcy %+v", d.Blocks[2].R)
	}
	if _, err := parseEPUB(makeEPUB(t, true)); err != errDRM {
		t.Errorf("an encrypted EPUB: %v", err)
	}
}

const testArticle = `<!doctype html><html><head><meta charset="utf-8"><title>サイト | 記事の題</title><meta property="og:title" content="記事の題"><meta name="author" content="書いた人"><meta property="article:published_time" content="2026-10-11T03:00:00+09:00"></head>
<body><header><nav>メニュー ホーム</nav></header><div class="side">広告</div><article><h1>記事の題</h1><p>これは自作の記事の最初の段落です。テストのために書きました。十分な長さにするため、同じような文を続けます。</p><h2>小見出し</h2><p>二つ目の段落です。<a href="x">リンク</a>も文字として残します。さらに文を足して、本文らしい長さにします。</p><script>var x=1</script><p>三つ目の段落。</p></article><footer>著作権表示</footer></body></html>`

func TestArticle(t *testing.T) {
	d, err := parseArticle([]byte(testArticle), "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "記事の題" || d.Author != "書いた人" || d.Date != "2026-10-11" || d.Flow != "article" {
		t.Errorf("meta %+v", d)
	}
	var kinds []string
	for _, b := range d.Blocks {
		kinds = append(kinds, b.T+":"+blockText(b))
	}
	got := strings.Join(kinds, "|")
	if !strings.HasPrefix(got, "p:これは自作の記事の最初の段落です。") || !strings.Contains(got, "h:小見出し") || !strings.Contains(got, "リンクも文字として残します") ||
		strings.Contains(got, "メニュー") || strings.Contains(got, "広告") || strings.Contains(got, "var x") || strings.Contains(got, "著作権") {
		t.Errorf("blocks %q", got)
	}
}

func TestFeedAndFetcher(t *testing.T) {
	var hits []time.Time
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, time.Now())
		ua = r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/feed":
			w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>テストの RSS</title>
<item><title>新しい記事</title><link>` + "http://" + r.Host + `/a2</link><pubDate>Sun, 11 Oct 2026 03:00:00 +0900</pubDate></item>
<item><title>古い記事</title><link>` + "http://" + r.Host + `/a1</link></item></channel></rss>`))
		case "/atom":
			w.Write([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title><entry><title>A</title><link href="http://x/a"/><updated>2026-10-10T00:00:00Z</updated></entry></feed>`))
		default:
			w.Write([]byte(testArticle))
		}
	}))
	defer srv.Close()
	f := newFetcher(5 * time.Second)
	f.gap = 150 * time.Millisecond
	b, _, _, err := f.get(srv.URL + "/feed")
	if err != nil {
		t.Fatal(err)
	}
	items, title, err := parseFeed(b)
	if err != nil || title != "テストの RSS" || len(items) != 2 || items[0].Title != "新しい記事" || items[0].Date != "2026-10-11" {
		t.Fatalf("feed %q %+v %v", title, items, err)
	}
	if _, _, _, err := f.get(srv.URL + "/a1"); err != nil {
		t.Fatal(err)
	}
	if gap := hits[1].Sub(hits[0]); gap < 140*time.Millisecond {
		t.Errorf("two requests to the same host %v apart", gap)
	}
	if !strings.HasPrefix(ua, "brain-deck/") || !strings.Contains(ua, "github.com/endohizumi/brain-deck") {
		t.Errorf("User-Agent %q", ua)
	}
	b, _, _, _ = f.get(srv.URL + "/atom")
	if items, title, err := parseFeed(b); err != nil || title != "Atom" || items[0].Link != "http://x/a" || items[0].Date != "2026-10-10" {
		t.Errorf("atom %q %+v %v", title, items, err)
	}
}

// readFake は、Brain の読み物の置き場所の代わり。
type readFake struct {
	docs []map[string]any
	cur  []byte
}

func (r *readFake) handle(req map[string]any) (any, bool) {
	switch req["cmd"] {
	case "hello":
		return map[string]any{"protocol": 1, "daemon": "lefthand", "version": "test", "upload_kinds": []string{"read"},
			"commands": []string{"hello", "list_reads", "delete_read", "upload_begin"}}, true
	case "upload_begin":
		r.cur = nil
		return map[string]any{"upload": "u1", "chunk_bytes": 4000}, true
	case "upload_chunk":
		b, _ := base64.StdEncoding.DecodeString(req["data"].(string))
		r.cur = append(r.cur, b...)
		return map[string]any{"received": len(r.cur)}, true
	case "upload_end":
		zr, _ := gzip.NewReader(bytes.NewReader(r.cur))
		var d readDoc
		json.NewDecoder(zr).Decode(&d)
		id := "r00000000000000" + string(rune('0'+len(r.docs)))
		r.docs = append(r.docs, map[string]any{"id": id, "title": d.Title, "kind": d.Kind, "source": d.Source, "chars": d.chars(), "bytes": len(r.cur)})
		return map[string]any{"id": id, "chars": d.chars()}, true
	case "list_reads":
		return map[string]any{"reads": r.docs, "total_bytes": 1, "limit_bytes": 32 << 20, "free_bytes": 100 << 20, "reserve_bytes": 64 << 20}, true
	case "delete_read":
		for i, d := range r.docs {
			if d["id"] == req["read"] {
				r.docs = append(r.docs[:i], r.docs[i+1:]...)
			}
		}
		return map[string]any{"deleted": req["read"]}, true
	}
	return nil, false
}

func TestReadAgainstFakeBrain(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newFakeBrain(t)
	rf := &readFake{}
	f.handler = rf.handle
	go f.serve(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed" {
			w.Write([]byte(`<rss><channel><title>T</title><item><title>記事1</title><link>http://` + r.Host + `/p1</link></item><item><title>記事2</title><link>http://` + r.Host + `/p2</link></item></channel></rss>`))
			return
		}
		w.Write([]byte(testArticle))
	}))
	defer srv.Close()
	code, out, errs := runCLI(t, "--port", f.slave, "read", "add", "../../testdata/reader/kumono_ito_aozora.zip")
	if code != exitOK || !strings.Contains(out, "「蜘蛛の糸」") {
		t.Fatalf("add: %d %s %s", code, out, errs)
	}
	p := filepath.Join(t.TempDir(), "book.epub")
	os.WriteFile(p, makeEPUB(t, false), 0o644)
	if code, out, errs = runCLI(t, "--port", f.slave, "read", "add", p); code != exitOK || !strings.Contains(out, "テストの本") {
		t.Fatalf("add epub: %d %s %s", code, out, errs)
	}
	os.WriteFile(p, makeEPUB(t, true), 0o644)
	if code, _, errs = runCLI(t, "--port", f.slave, "read", "add", p); code == exitOK || !strings.Contains(errs, "DRM") {
		t.Fatalf("add drm: %d %s", code, errs)
	}
	if code, out, _ = runCLI(t, "--port", f.slave, "read"); code != exitOK || !strings.Contains(out, "蜘蛛の糸") || !strings.Contains(out, "2 件") {
		t.Fatalf("list: %d %s", code, out)
	}
	// RSS：足して、新しい記事を取り込む。2 回目は足さない
	if code, out, _ = runCLI(t, "read", "feed", "add", srv.URL+"/feed", "--limit-items", "5"); code != exitOK || !strings.Contains(out, "feeds.yaml") {
		t.Fatalf("feed add: %d %s", code, out)
	}
	if code, out, errs = runCLI(t, "--port", f.slave, "--fetch-timeout", "5s", "read", "feed", "sync"); code != exitOK || !strings.Contains(out, "T：2 件を足しました") {
		t.Fatalf("feed sync: %d %s %s", code, out, errs)
	}
	if code, out, _ = runCLI(t, "--port", f.slave, "read", "feed", "sync"); code != exitOK || !strings.Contains(out, "0 件") {
		t.Fatalf("feed sync again: %d %s", code, out)
	}
	if len(rf.docs) != 4 || rf.docs[2]["kind"] != "rss" || rf.docs[2]["title"] != "記事1" {
		t.Fatalf("docs %+v", rf.docs)
	}
	if code, out, _ = runCLI(t, "read", "feed", "list"); code != exitOK || !strings.Contains(out, "5 件まで") {
		t.Fatalf("feed list: %d %s", code, out)
	}
	if code, _, _ = runCLI(t, "--port", f.slave, "read", "rm", "1"); code != exitOK || len(rf.docs) != 3 {
		t.Fatalf("rm: %d", code)
	}
	// -o：送らずにファイルに書く
	o := filepath.Join(t.TempDir(), "k.json.gz")
	if code, out, _ = runCLI(t, "read", "add", "../../testdata/reader/kumono_ito_aozora.zip", "-o", o); code != exitOK || !strings.Contains(out, "3127 文字") {
		t.Fatalf("-o: %d %s", code, out)
	}
}
