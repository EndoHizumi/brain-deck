package main

import (
	"image"
	"unicode/utf8"
)

// ---------- 読み物のページの組み方 ----------
//
// ページの分け方は Brain で決める（文字の大きさと、紙の範囲（画面の向き）による）。ページの始まりの位置（ブロックと文字）
// から組むと、いつも同じページになるので、ページの始まりの位置の並び（paginate）だけを覚え、描くときに組み直す。
// しおりは位置で覚えるので、文字の大きさや向きを変えても、その位置を含むページを開く。
//
// - 行の長さは「半角の文字の数」（全角は 2、半角は 1）で数える。行のあいだは、ルビの分（S/2）と S/4 を空ける
// - 禁則：行の頭に来てはいけない文字（、。」』）ー ッ など）は、句読点ならぶら下げ（行の外に 1 文字出す）、
//   ほかは前の文字と一緒に次の行へ送る。行の終わりに来てはいけない文字（「『（ など）は次の行へ送る
// - ルビの付いた文字は、1 行に入りきらない長さでなければ、行をまたがない
// - 縦書き：行は右から左へ、ルビは文字の右。句読点は右上へ、小さいかなは少し右上へ、長音や括弧などは 90 度回す。
//   半角の英数字は 90 度回す。縦中横（tcy）と、前後が全角の 1〜2 桁の数字は、横に並べて 1 文字の箱に入れる
// - 横書き：行は上から下へ、ルビは文字の上
// - 見出しは、前に 1 行空け、2 文字下げて書き、あとに 1 行空ける。改ページ（pb）は次のページから
// - flow が article のときは、段落のあいだを 1 行空ける

// 行の頭に来てはいけない文字
var noLineStart = map[rune]bool{}

// 行の終わりに来てはいけない文字
var noLineEnd = map[rune]bool{}

// ぶら下げる文字（句読点）
var hangable = map[rune]bool{'、': true, '。': true, '，': true, '．': true, ',': true, '.': true}

func init() {
	for _, r := range "、。，．・：；？！」』）〕］｝〉》】〟ゝゞヽヾー…‥々ぁぃぅぇぉっゃゅょゎァィゥェォッャュョヮヵヶ!),.:;?]}" {
		noLineStart[r] = true
	}
	for _, r := range "「『（〔［｛〈《【〝([{" {
		noLineEnd[r] = true
	}
}

// readMetrics は、ページの寸法。
type readMetrics struct {
	S, R     int  // 文字とルビの大きさ
	vertical bool //
	area     image.Rectangle
	lines    int // 1 ページの行の数
	units    int // 1 行の長さ（半角の文字の数）
	pitch    int // 行の送り
	x0, y0   int // 組む範囲の始まり（縦書きでは右上、横書きでは左上）
	flowArt  bool
}

func readMetricsFor(area image.Rectangle, S int, vertical, article bool) readMetrics {
	m := readMetrics{S: S, R: S / 2, vertical: vertical, area: area, pitch: S + S/2 + S/4, flowArt: article}
	margin := S / 2
	if vertical {
		m.units = max((area.Dy()-2*margin)/(S/2), 2)
		m.lines = max((area.Dx()-2*margin)/m.pitch, 1)
		used := m.lines * m.pitch
		m.x0 = area.Max.X - (area.Dx()-used)/2 // 右の端
		m.y0 = area.Min.Y + (area.Dy()-m.units*(S/2))/2
	} else {
		m.units = max((area.Dx()-2*margin)/(S/2), 2)
		m.lines = max((area.Dy()-2*margin)/m.pitch, 1)
		used := m.lines * m.pitch
		m.x0 = area.Min.X + (area.Dx()-m.units*(S/2))/2
		m.y0 = area.Min.Y + (area.Dy()-used)/2
	}
	return m
}

// rcell は、組むときの 1 文字（縦中横は、並べた文字をまとめて 1 つ）。
type rcell struct {
	r    rune
	tcy  string // 縦中横の文字（縦書きのとき）
	w    int    // 半角の文字の数（1 か 2）
	em   bool
	ruby int // ルビの組の番号（-1 はなし）
	c    int // ブロックの中の文字の番号（ReadPos.C）
	n    int // この cell の文字の数（縦中横は 2〜3）
}

type rubyGroup struct {
	start, end int // cell の番号
	text       string
}

// rprep は、組むために分けたブロック。
type rprep struct {
	cells  []rcell
	rubies []rubyGroup
	chars  int
}

func isDigitRune(r rune) bool { return r >= '0' && r <= '9' }

// prepBlock は、ブロックを文字の並びにする。vertical なら、縦中横をまとめる。
func prepBlock(b *RBlock, vertical bool) rprep {
	var p rprep
	c := 0
	for _, run := range b.R {
		g := -1
		if run.Ruby != "" {
			g = len(p.rubies)
			p.rubies = append(p.rubies, rubyGroup{start: len(p.cells), text: run.Ruby})
		}
		rs := []rune(run.S)
		if vertical && run.TCY && len(rs) <= 3 && len(rs) > 0 {
			p.cells = append(p.cells, rcell{tcy: run.S, w: 2, em: run.Em, ruby: g, c: c, n: len(rs)})
			c += len(rs)
		} else {
			for i := 0; i < len(rs); i++ {
				r := rs[i]
				// 前後が数字でない 1〜2 桁の数字（縦書き）は、縦中横にする
				if vertical && isDigitRune(r) {
					j := i
					for j < len(rs) && isDigitRune(rs[j]) {
						j++
					}
					prevDigit := i > 0 && halfWidthRune(rs[i-1])
					nextHalf := j < len(rs) && halfWidthRune(rs[j])
					if j-i <= 2 && !prevDigit && !nextHalf {
						p.cells = append(p.cells, rcell{tcy: string(rs[i:j]), w: 2, em: run.Em, ruby: g, c: c, n: j - i})
						c += j - i
						i = j - 1
						continue
					}
				}
				w := 2
				if halfWidthRune(r) {
					w = 1
				}
				p.cells = append(p.cells, rcell{r: r, w: w, em: run.Em, ruby: g, c: c, n: 1})
				c++
			}
		}
		if g >= 0 {
			p.rubies[g].end = len(p.cells)
		}
	}
	p.chars = c
	return p
}

// cellAt は、文字の番号 c を含む cell の番号。
func (p *rprep) cellAt(c int) int {
	for i, x := range p.cells {
		if x.c+x.n > c {
			return i
		}
	}
	return len(p.cells)
}

// breakLine は、cells[from:] から 1 行に入る数を返す（禁則とルビを守る）。avail は半角の文字の数。
func (p *rprep) breakLine(from, avail int) int {
	sum, i := 0, from
	for i < len(p.cells) && sum+p.cells[i].w <= avail {
		sum += p.cells[i].w
		i++
	}
	if i >= len(p.cells) {
		return i
	}
	end := i
	next := p.cells[i]
	switch {
	case hangable[next.r]:
		end = i + 1 // ぶら下げ
	case noLineStart[next.r] && end-from >= 2:
		end-- // 前の文字と一緒に送る
		for end-from >= 2 && noLineStart[p.cells[end].r] {
			end--
		}
	}
	for end-from >= 2 && noLineEnd[p.cells[end-1].r] {
		end--
	}
	// ルビの組を行でまたがない（組が行より長ければ、またいでよい）
	if g := p.cells[end-1].ruby; g >= 0 && end < len(p.cells) && p.cells[end].ruby == g {
		rg := p.rubies[g]
		w := 0
		for k := rg.start; k < rg.end; k++ {
			w += p.cells[k].w
		}
		if rg.start > from && w <= avail {
			end = rg.start
		}
	}
	if end <= from {
		end = from + 1
	}
	return end
}

// rline は、組んだ 1 行。
type rline struct {
	b         int
	c0, c1    int // cell の番号
	indent    int // 字下げ（半角の文字の数）
	blank     bool
	prep      *rprep
	heading   bool
	firstLine bool
}

// readPage は、組んだ 1 ページ。next は次のページの始まり（end なら最後）。
type readPage struct {
	lines []rline
	next  ReadPos
	end   bool
}

// layoutPage は、位置 pos から 1 ページを組む。preps は、組んだブロックを覚えておく先（nil でもよい）。
func layoutPage(d *ReadDoc, m readMetrics, pos ReadPos, preps map[int]*rprep) readPage {
	var pg readPage
	prep := func(b int) *rprep {
		if p, ok := preps[b]; ok {
			return p
		}
		p := prepBlock(&d.Blocks[b], m.vertical)
		if preps != nil {
			preps[b] = &p
		}
		return &p
	}
	line := 0
	for line < m.lines {
		if pos.B >= len(d.Blocks) {
			pg.end = true
			break
		}
		bl := &d.Blocks[pos.B]
		if bl.T == "pb" {
			pos = ReadPos{pos.B + 1, 0}
			if line > 0 {
				break // 次のページから
			}
			continue
		}
		p := prep(pos.B)
		heading := bl.T == "h"
		indent := bl.Indent * 2
		if heading {
			indent += 4
		}
		ci := p.cellAt(pos.C)
		if ci == 0 && heading && line > 0 {
			pg.lines = append(pg.lines, rline{blank: true})
			line++
			if line >= m.lines {
				break
			}
		}
		if len(p.cells) == 0 {
			pg.lines = append(pg.lines, rline{b: pos.B, blank: true})
			line++
			pos = ReadPos{pos.B + 1, 0}
			continue
		}
		avail := max(m.units-indent, 2)
		end := p.breakLine(ci, avail)
		pg.lines = append(pg.lines, rline{b: pos.B, c0: ci, c1: end, indent: indent, prep: p, heading: heading, firstLine: ci == 0})
		line++
		if end >= len(p.cells) {
			pos = ReadPos{pos.B + 1, 0}
			nextEmpty := pos.B < len(d.Blocks) && d.Blocks[pos.B].T == "p" && len(d.Blocks[pos.B].R) == 0
			if (heading || m.flowArt && bl.T != "info") && !nextEmpty && line < m.lines {
				pg.lines = append(pg.lines, rline{blank: true})
				line++
			}
		} else {
			pos = ReadPos{pos.B, p.cells[end].c}
		}
	}
	if pos.B >= len(d.Blocks) {
		pg.end = true
	}
	pg.next = pos
	return pg
}

// paginate は、すべてのページの始まりの位置を返す。
func paginate(d *ReadDoc, m readMetrics) []ReadPos {
	starts := []ReadPos{{}}
	pos := ReadPos{}
	preps := map[int]*rprep{} // ページをまたぐ長い段落を、ページごとに分け直さない
	for {
		for b := range preps {
			if b < pos.B {
				delete(preps, b)
			}
		}
		pg := layoutPage(d, m, pos, preps)
		if pg.end || !pos.less(pg.next) {
			return starts
		}
		pos = pg.next
		starts = append(starts, pos)
	}
}

// pageOf は、位置 p を含むページの番号。
func pageOf(starts []ReadPos, p ReadPos) int {
	i := 0
	for k, s := range starts {
		if !p.less(s) {
			i = k
		}
	}
	return i
}

// ---------- 描く ----------

var (
	colReadPaper = RGB{0xf4, 0xf1, 0xe6}
	colReadInk   = RGB{0x18, 0x18, 0x20}
	colReadHead  = RGB{0x20, 0x30, 0x60}
	colReadInfo  = RGB{0x50, 0x50, 0x58}
)

// drawReadPage は、ページを描く（紙は呼ぶ側が塗る）。
func drawReadPage(cv *Canvas, d *ReadDoc, m readMetrics, pg readPage) {
	f := readFontOf(m.S)
	rf := readFontOf(m.R)
	lvInk := makeInkLevels(colReadPaper, colReadInk)
	lvHead := makeInkLevels(colReadPaper, colReadHead)
	lvInfo := makeInkLevels(colReadPaper, colReadInfo)
	clip := m.area
	half := m.S / 2
	for li, ln := range pg.lines {
		if ln.blank || ln.prep == nil {
			continue
		}
		lv := &lvInk
		switch d.Blocks[ln.b].T {
		case "h":
			lv = &lvHead
		case "info":
			lv = &lvInfo
		}
		// 行の始まりの、文字の箱の左上と、文字の進む向き
		var bx, by int
		if m.vertical {
			bx = m.x0 - li*m.pitch - m.R - m.S
			by = m.y0 + ln.indent*half
		} else {
			bx = m.x0 + ln.indent*half
			by = m.y0 + li*m.pitch + m.R
		}
		off := 0 // 半角の文字の数
		start := make([]int, ln.c1-ln.c0+1)
		for k := ln.c0; k < ln.c1; k++ {
			c := ln.prep.cells[k]
			start[k-ln.c0] = off
			x, y := bx, by
			if m.vertical {
				y += off * half
			} else {
				x += off * half
			}
			switch {
			case c.tcy != "" && m.vertical:
				drawTCY(cv, f, c.tcy, x, y, m.S, lv, clip)
			default:
				g := glyphFor(f, c.r, m.vertical)
				drawGlyph(cv, g, x, y, lv, clip)
			}
			if c.em {
				drawEmDot(cv, m, x, y, c.w*half, lv, clip)
			}
			off += c.w
		}
		start[ln.c1-ln.c0] = off
		// ルビ
		for gi, rg := range ln.prep.rubies {
			a, b := max(rg.start, ln.c0), min(rg.end, ln.c1)
			if a >= b {
				continue
			}
			rs := []rune(rg.text)
			total := rg.end - rg.start
			k0 := len(rs) * (a - rg.start) / total
			k1 := len(rs) * (b - rg.start) / total
			if b == rg.end {
				k1 = len(rs)
			}
			drawRuby(cv, rf, m, rs[k0:k1], bx, by, start[a-ln.c0]*half, (start[b-ln.c0]-start[a-ln.c0])*half, &lvInk, clip)
			_ = gi
		}
	}
}

// drawTCY は、縦中横の文字を、S×S の箱に横に並べて描く。
func drawTCY(cv *Canvas, f *ReadFont, s string, x, y, S int, lv *inkLevels, clip image.Rectangle) {
	rs := []rune(s)
	if len(rs) == 1 {
		g := glyphFor(f, rs[0], false)
		drawGlyph(cv, g, x+(S-g.w)/2, y, lv, clip)
		return
	}
	// 2〜3 文字：半角の箱（S/2）を並べる。3 文字なら少し詰める
	w := S / 2
	if len(rs) == 3 {
		w = S / 3
	}
	ox := x + (S-w*len(rs))/2
	for i, r := range rs {
		g := glyphFor(f, r, false)
		drawGlyph(cv, g, ox+i*w-(g.w-w)/2, y, lv, image.Rect(ox+i*w, y, ox+(i+1)*w, y+S).Intersect(clip))
	}
}

// drawEmDot は、傍点（ゴマの代わりに小さな丸）を描く。縦書きは文字の右、横書きは上。
func drawEmDot(cv *Canvas, m readMetrics, x, y, len int, lv *inkLevels, clip image.Rectangle) {
	r := max(m.S/12, 1)
	var cx, cy int
	if m.vertical {
		cx, cy = x+m.S+m.R/2, y+len/2
	} else {
		cx, cy = x+len/2, y-m.R/2
	}
	c := cv.unpackRGB(lv[15])
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				p := image.Pt(cx+dx, cy+dy)
				if p.In(clip) {
					cv.set(p.X, p.Y, c)
				}
			}
		}
	}
}

// drawRuby は、ルビ rs を、行の off から span（ドット）の文字に付ける。
func drawRuby(cv *Canvas, rf *ReadFont, m readMetrics, rs []rune, bx, by, off, span int, lv *inkLevels, clip image.Rectangle) {
	if len(rs) == 0 {
		return
	}
	R := m.R
	n := 0
	for _, r := range rs {
		if halfWidthRune(r) {
			n += R / 2
		} else {
			n += R
		}
	}
	p := off + (span-n)/2 // 文字の範囲の中央に置く（長ければはみ出す）
	for _, r := range rs {
		g := glyphFor(rf, r, m.vertical)
		if g.pix == nil && !halfWidthRune(r) {
			g.w = R
		}
		adv := R
		if halfWidthRune(r) {
			adv = R / 2
		}
		if m.vertical {
			drawGlyph(cv, g, bx+m.S, by+p, lv, clip)
		} else {
			drawGlyph(cv, g, bx+p, by-R, lv, clip)
		}
		p += adv
	}
}

// docChars は、読み物の文字の数と、位置 p までの文字の数（進み具合）。
func docChars(d *ReadDoc, p ReadPos) (total, upto int) {
	for i, b := range d.Blocks {
		n := 0
		for _, r := range b.R {
			n += utf8.RuneCountInString(r.S)
		}
		total += n
		switch {
		case i < p.B:
			upto += n
		case i == p.B:
			upto += min(p.C, n)
		}
	}
	return total, upto
}
