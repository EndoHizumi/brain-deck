package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"path/filepath"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

func para(s string) RBlock { return RBlock{T: "p", R: []RRun{{S: s}}} }

func lineText(ln rline) string {
	var b strings.Builder
	for k := ln.c0; k < ln.c1; k++ {
		c := ln.prep.cells[k]
		if c.tcy != "" {
			b.WriteString("[" + c.tcy + "]")
		} else {
			b.WriteRune(c.r)
		}
	}
	return b.String()
}

func TestReadKinsoku(t *testing.T) {
	// 1 行 10 文字（半角で 20）
	p := prepBlock(&RBlock{R: []RRun{{S: "あいうえおかきくけこ。さしすせそたちつてと」なにぬねのはひふへほ「まみむめも"}}}, true)
	var lines []string
	for i := 0; i < len(p.cells); {
		e := p.breakLine(i, 20)
		var s strings.Builder
		for k := i; k < e; k++ {
			s.WriteRune(p.cells[k].r)
		}
		lines = append(lines, s.String())
		i = e
	}
	// 「。」はぶら下げ、「」」は前の文字と一緒に送る、「「」は行の終わりに置かない
	want := "あいうえおかきくけこ。|さしすせそたちつて|と」なにぬねのはひふ|へほ「まみむめも"
	if got := strings.Join(lines, "|"); got != want {
		t.Errorf("lines\n%s\nwant\n%s", got, want)
	}
	// ルビの組は行をまたがない
	r := prepBlock(&RBlock{R: []RRun{{S: "あいうえおかきく"}, {S: "漢字語", Ruby: "かんじご"}, {S: "です"}}}, true)
	if e := r.breakLine(0, 20); e != 8 {
		t.Errorf("ruby group split: line ends at %d", e)
	}
	// 縦中横：前後が全角の 1〜2 桁の数字は 1 つの箱に。3 桁はそのまま
	tc := prepBlock(&RBlock{R: []RRun{{S: "第12回と123と7"}}}, true)
	var got []string
	for _, c := range tc.cells {
		if c.tcy != "" {
			got = append(got, c.tcy)
		}
	}
	if strings.Join(got, ",") != "12,7" {
		t.Errorf("tcy %q", got)
	}
	// 横書きでは縦中横にしない
	if h := prepBlock(&RBlock{R: []RRun{{S: "第12回"}}}, false); len(h.cells) != 4 || h.cells[1].w != 1 {
		t.Errorf("horizontal digits %+v", h.cells)
	}
}

func testDoc(n int) *ReadDoc {
	d := &ReadDoc{Format: 1, Title: "テスト", Kind: "text"}
	d.Blocks = append(d.Blocks, RBlock{T: "h", L: 1, R: []RRun{{S: "一章"}}})
	for i := 0; i < n; i++ {
		d.Blocks = append(d.Blocks, RBlock{T: "p", R: []RRun{{S: "　自分で書いた文章です。"}, {S: "漢字", Ruby: "かんじ", Em: i%3 == 0}, {S: strings.Repeat("あいうえお、かきくけこ。", 1+i%7)}}})
		if i == n/2 {
			d.Blocks = append(d.Blocks, RBlock{T: "pb"}, RBlock{T: "h", L: 2, R: []RRun{{S: "二章"}}})
		}
	}
	return d
}

// ページの始まりから組むと、いつも同じ次のページの始まりになる。すべての文字が、どこかのページに 1 度だけ出る
func TestPaginate(t *testing.T) {
	d := testDoc(60)
	for _, c := range []struct {
		area     image.Rectangle
		size     int
		vertical bool
	}{{image.Rect(0, 0, 780, 440), 24, true}, {image.Rect(0, 0, 460, 760), 24, true}, {image.Rect(0, 0, 460, 760), 32, false}} {
		m := readMetricsFor(c.area, c.size, c.vertical, false)
		starts := paginate(d, m)
		if len(starts) < 3 {
			t.Fatalf("%+v: %d pages", c, len(starts))
		}
		seen := 0
		for i, s := range starts {
			pg := layoutPage(d, m, s, nil)
			if len(pg.lines) > m.lines {
				t.Fatalf("page %d has %d lines (max %d)", i, len(pg.lines), m.lines)
			}
			for _, ln := range pg.lines {
				if !ln.blank {
					for k := ln.c0; k < ln.c1; k++ {
						seen += ln.prep.cells[k].n
					}
				}
			}
			if i+1 < len(starts) && pg.next != starts[i+1] {
				t.Fatalf("page %d: next %v, want %v", i, pg.next, starts[i+1])
			}
			if i+1 == len(starts) && !pg.end {
				t.Fatal("the last page is not the end")
			}
		}
		total, _ := docChars(d, ReadPos{})
		if seen != total {
			t.Errorf("%+v: %d characters on the pages, %d in the doc", c, seen, total)
		}
		// 改ページ：二章は新しいページの頭
		found := false
		for _, s := range starts {
			if d.Blocks[s.B].T == "h" && blockText(&d.Blocks[s.B]) == "二章" && s.C == 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("%+v: 二章 does not start a page", c)
		}
	}
}

func uploadRead(t *testing.T, u *Uploads, d *ReadDoc) (string, error) {
	t.Helper()
	b, _ := json.Marshal(d)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	data := buf.Bytes()
	s := sha256.Sum256(data)
	st, err := u.Begin(UploadBegin{Kind: "read", SHA256: hex.EncodeToString(s[:]), Bytes: int64(len(data)), Name: d.Title, Source: "test"})
	if err != nil {
		return "", err
	}
	for off := 0; off < len(data); off += st.ChunkBytes {
		if _, err := u.Chunk(st.Upload, int64(off), data[off:min(off+st.ChunkBytes, len(data))]); err != nil {
			return "", err
		}
	}
	res, err := u.End(st.Upload)
	if err != nil {
		return "", err
	}
	return res.(map[string]any)["id"].(string), nil
}

const readerConfig = `
touch: { min_x: 226, max_x: 3938, min_y: 3803, max_y: 384 }
layers:
  - name: base
    touch:
      cols: 1
      rows: 1
      cells:
        "0,0": { widget: reader }
`

func TestReaderWidget(t *testing.T) {
	old := imageFreeSpace
	imageFreeSpace = func(string) (int64, int64, error) { return 1 << 30, 3 << 30, nil }
	defer func() { imageFreeSpace = old }()
	dir := filepath.Join(t.TempDir(), "reader")
	st := OpenReadStore(dir)
	u := NewUploads()
	u.Register("read", st.UploadKind())
	id, err := uploadRead(t, u, testDoc(60))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uploadRead(t, u, &ReadDoc{Format: 1, Title: "", Blocks: []RBlock{para("x")}}); err == nil {
		t.Error("a reading without a title was accepted")
	}
	old2 := readQuota
	readQuota = 100
	if _, err := uploadRead(t, u, testDoc(5)); err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Errorf("quota: %v", err)
	}
	readQuota = old2
	rs := NewReaderService(st)
	_, km := compileText(t, readerConfig)
	e := NewEngine(km, &State{hid: &hidRec{}, active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	rt.reader = rs
	e.SetWidgets(rt)
	d := &Display{cv: NewCanvas(800, 480, 1600, rgb565, 0), env: func() WidgetEnv { return WidgetEnv{Now: time.Now(), Reader: rs.Snapshot()} }}
	l := buildLayout(km, e.View())
	l.W, l.H = 800, 480
	d.layout, d.drawn = l, make([]bool, len(l.Cells))
	d.drawAllLocked(l)
	area := l.rect(0, 0).Inset(cellGap + textMargin)
	tap := func(x, y int) {
		rx, ry := hwRaw(km.Touch, 0, 800, 480, x, y)
		e.PressTouch(rx, ry)
		e.Release("t")
	}
	wait := func(what string, ok func(v *ReaderView) bool) *ReaderView {
		t.Helper()
		for i := 0; i < 300; i++ {
			d.redrawWidgets() // 描くときに寸法を決めて、ページを数え始める
			if v := rs.Snapshot(); ok(v) {
				return v
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out: %s", what)
		return nil
	}
	// 一覧の最初の行をタップして開く
	g := libGeometry(area, 1)
	tap((g.rows[0].Min.X+g.rows[0].Max.X)/2, (g.rows[0].Min.Y+g.rows[0].Max.Y)/2)
	v := wait("open and count", func(v *ReaderView) bool { return v.doc != nil && v.pages > 0 })
	if v.mode != readRead || v.page != 1 {
		t.Fatalf("mode %d page %d", v.mode, v.page)
	}
	pages := v.pages
	// 縦書き：左をタップすると次、右で前。キーの ← とスペースでも次
	tap(area.Min.X+20, area.Min.Y+100)
	if v := rs.Snapshot(); v.page != 2 {
		t.Fatalf("left tap: page %d", v.page)
	}
	e.PressKey(evdev.KEY_LEFT)
	e.ReleaseKey(evdev.KEY_LEFT)
	e.PressKey(evdev.KEY_SPACE)
	e.ReleaseKey(evdev.KEY_SPACE)
	tap(area.Max.X-20, area.Min.Y+100)
	if v := rs.Snapshot(); v.page != 3 {
		t.Fatalf("keys and right tap: page %d", v.page)
	}
	if h := e.out.hid.(*hidRec).take(); len(h) != 0 {
		t.Errorf("the reader keys went to the PC: % x", h)
	}
	bm := st.Bookmark(id)
	// 真ん中で道具。文字を大きくしても、読んでいた位置を含むページを開く
	tap(area.Min.X+area.Dx()/2, area.Min.Y+area.Dy()/2)
	if v := rs.Snapshot(); v.mode != readMenu {
		t.Fatalf("menu: mode %d", v.mode)
	}
	bs := menuGeometry(area)
	tap((bs[2].Min.X+bs[2].Max.X)/2, (bs[2].Min.Y+bs[2].Max.Y)/2) // 文字を大きく
	v = wait("recount for 32", func(v *ReaderView) bool { return v.size == 32 && v.pages > 0 })
	if v.pages <= pages {
		t.Errorf("32 dots: %d pages, 24 dots: %d", v.pages, pages)
	}
	p := rs.Snapshot().pos
	if bm.less(p) {
		t.Errorf("the bookmark %v is before the page %v", bm, p)
	}
	pg := layoutPage(rs.doc, rs.m, p, nil)
	if !bm.less(pg.next) {
		t.Errorf("the bookmark %v is not on the page %v..%v", bm, p, pg.next)
	}
	// 画面が全体を描いたものと同じ
	d.redrawWidgets()
	ref := NewCanvas(800, 480, 1600, rgb565, 0)
	rl := *d.layout
	rl.Env = d.env()
	drawAll(ref, &rl, d.drawn)
	if !bytes.Equal(ref.pix, d.cv.pix) {
		t.Error("the screen differs from a full redraw")
	}
	// 目次から二章へ
	tap(area.Min.X+area.Dx()/2, area.Min.Y+area.Dy()/2)
	tap((bs[1].Min.X+bs[1].Max.X)/2, (bs[1].Min.Y+bs[1].Max.Y)/2)
	if v := rs.Snapshot(); v.mode != readTOC {
		t.Fatalf("toc: mode %d", v.mode)
	}
	body := image.Rect(area.Min.X, area.Min.Y+fontH*2+8, area.Max.X, area.Max.Y)
	tg := libGeometry(body, 2)
	tap((tg.rows[1].Min.X+tg.rows[1].Max.X)/2, (tg.rows[1].Min.Y+tg.rows[1].Max.Y)/2)
	v = rs.Snapshot()
	first := layoutPage(v.doc, rs.m, v.pos, nil).lines[0]
	if v.mode != readRead || blockText(&v.doc.Blocks[first.b]) != "二章" {
		t.Fatalf("jump: mode %d at %v (the page starts with block %d)", v.mode, v.pos, first.b)
	}
	// しおりと設定を書いて、開き直すと、同じ読み物の同じ位置を開く
	st.FlushState()
	st2 := OpenReadStore(dir)
	rs2 := NewReaderService(st2)
	if v := rs2.Snapshot(); v.mode != readRead || v.size != 32 || v.pos != rs.Snapshot().pos {
		t.Errorf("reopen: mode %d size %d pos %v", v.mode, v.size, v.pos)
	}
	if l := st2.List(); len(l) != 1 || l[0].Pos == nil || l[0].Progress <= 0 || l[0].Progress >= 1 {
		t.Errorf("list %+v", l)
	}
	// 消すと一覧に戻る
	if err := st.Delete(id); err != nil {
		t.Fatal(err)
	}
	rs.Closed(id)
	if v := rs.Snapshot(); v.mode != readLib || len(v.list) != 0 {
		t.Errorf("after delete: mode %d", v.mode)
	}
}
