package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
)

// ---------- 青空文庫のテキスト（read add） ----------
//
// 青空文庫の「テキスト版」（Shift_JIS。zip のままでもよい）を、読み物の形（readDoc）にする。
// https://www.aozora.gr.jp/annotation/ の注記のうち、よく使うものを解釈し、解釈できない注記は読み捨てる（本文は残す）。
//   - ルビ《》と、その始まりの ｜。｜ がなければ、直前の同じ種類の文字（漢字、カタカナ、ひらがな、英数字）の続き
//   - ［＃改ページ］［＃改丁］［＃改段］［＃改見開き］：改ページ
//   - ［＃N字下げ］（その行）、［＃ここからN字下げ］〜［＃ここで字下げ終わり］
//   - ［＃「X」は大見出し］（中見出し、小見出し）、［＃大見出し］X［＃大見出し終わり］：見出し
//   - ［＃「X」に傍点］、［＃傍点］〜［＃傍点終わり］（白ゴマ傍点、丸傍点なども傍点にする）
//   - ［＃「X」は縦中横］、［＃縦中横］〜［＃縦中横終わり］
//   - ［＃「X」に「Y」の注記］、［＃「X」の左に「Y」のルビ］：ルビ
//   - 外字 ※［＃「…」、U+XXXX、…］は、その文字にする。JIS X 0213 の面区点番号だけのものは ※ のまま
// 最初の題名と著者、記号の説明（-------- で囲んだところ）は、本文から外す。末尾の「底本：」からあとは、底本や入力者などの
// 情報として消さずに残す（info のブロック）。

type aozoraChar struct {
	r    rune
	ruby int // ルビの組の番号（-1 はなし）
	em   bool
	tcy  bool
}

type aozoraRuby struct {
	start, end int
	text       string
}

// aozoraSpan は、［＃傍点］〜［＃傍点終わり］などの、開いている範囲。
type aozoraSpan struct {
	kind  string
	start int
	level int
}

type aozoraLine struct {
	chars  []aozoraChar
	rubies []aozoraRuby
	head   int // 見出しの大きさ（0 はふつうの段落）
	indent int
	pb     bool
}

var (
	reAozoraNote = regexp.MustCompile(`［＃([^］]*)］`)
	reAozoraU    = regexp.MustCompile(`U\+([0-9A-Fa-f]{4,6})`)
	reQuoted     = regexp.MustCompile(`^「(.+?)」(.*)$`)
)

// decodeAozora は、Shift_JIS（か UTF-8）のテキストか zip を、文字列にする。
func decodeAozora(b []byte) (string, error) {
	if bytes.HasPrefix(b, []byte("PK")) {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return "", err
		}
		for _, f := range zr.File {
			if strings.EqualFold(path.Ext(f.Name), ".txt") {
				rc, err := f.Open()
				if err != nil {
					return "", err
				}
				t, err := io.ReadAll(io.LimitReader(rc, 64<<20))
				rc.Close()
				if err != nil {
					return "", err
				}
				return decodeAozora(t)
			}
		}
		return "", fmt.Errorf("zip の中にテキスト（.txt）がありません")
	}
	if utf8.Valid(b) {
		return strings.TrimPrefix(string(b), "\ufeff"), nil
	}
	t, err := japanese.ShiftJIS.NewDecoder().Bytes(b)
	if err != nil {
		return "", fmt.Errorf("Shift_JIS として読めません: %w", err)
	}
	return string(t), nil
}

// parseAozora は、青空文庫のテキストを読み物にする。
func parseAozora(text string) readDoc {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	doc := readDoc{Format: 1, Kind: "aozora", Flow: "book"}
	i := 0
	// 題名、副題、著者（最初の空行まで）
	var head []string
	for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
		head = append(head, strings.TrimSpace(lines[i]))
		i++
	}
	if len(head) > 0 {
		doc.Title = head[0]
	}
	if len(head) > 1 {
		doc.Author = head[len(head)-1]
		if len(head) > 2 {
			doc.Title += " " + strings.Join(head[1:len(head)-1], " ")
		}
	}
	// 記号の説明（--- で囲んだところ）を飛ばす
	for k := i; k < len(lines) && k < i+5; k++ {
		if strings.HasPrefix(lines[k], "-----") {
			for k++; k < len(lines) && !strings.HasPrefix(lines[k], "-----"); k++ {
			}
			i = k + 1
			break
		}
	}
	indent := 0
	info := false
	for ; i < len(lines); i++ {
		raw := lines[i]
		if strings.HasPrefix(raw, "底本：") || strings.HasPrefix(raw, "底本:") {
			info = true
		}
		if info {
			t := strings.TrimSpace(raw)
			if t == "" {
				continue
			}
			doc.Blocks = append(doc.Blocks, rBlock{T: "info", R: []rRun{{S: t}}})
			continue
		}
		ln, nextIndent := parseAozoraLine(raw, indent)
		indent = nextIndent
		if ln.pb {
			doc.Blocks = append(doc.Blocks, rBlock{T: "pb"})
			if len(ln.chars) == 0 {
				continue
			}
		}
		b := rBlock{T: "p", Indent: min(ln.indent, 20), R: aozoraRuns(ln)}
		if ln.head > 0 {
			b.T, b.L, b.Indent = "h", ln.head, 0
		}
		doc.Blocks = append(doc.Blocks, b)
	}
	// 最初と最後の空の段落は外す
	for len(doc.Blocks) > 0 && doc.Blocks[0].T == "p" && len(doc.Blocks[0].R) == 0 {
		doc.Blocks = doc.Blocks[1:]
	}
	for len(doc.Blocks) > 0 && doc.Blocks[len(doc.Blocks)-1].T == "p" && len(doc.Blocks[len(doc.Blocks)-1].R) == 0 {
		doc.Blocks = doc.Blocks[:len(doc.Blocks)-1]
	}
	if doc.Title == "" {
		doc.Title = "（題名なし）"
	}
	return doc
}

// kindOf は、ルビの親文字を探すときの文字の種類。
func kindOf(r rune) int {
	switch {
	case unicode.Is(unicode.Han, r) || r == '々' || r == '〆' || r == 'ヶ' || r == '〇' || r == '※':
		return 1
	case unicode.Is(unicode.Katakana, r) || r == 'ー':
		return 2
	case unicode.Is(unicode.Hiragana, r):
		return 3
	case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r >= 0xff10 && r <= 0xff5a:
		return 4
	}
	return 0
}

// parseAozoraLine は、1 行を読む。indent は「ここから字下げ」の続き。
func parseAozoraLine(raw string, indent int) (aozoraLine, int) {
	ln := aozoraLine{indent: indent}
	rubyStart := -1 // ｜ の位置
	var open []aozoraSpan
	rs := []rune(raw)
	for k := 0; k < len(rs); k++ {
		r := rs[k]
		switch {
		case r == '｜':
			rubyStart = len(ln.chars)
			continue
		case r == '《':
			end := k + 1
			for end < len(rs) && rs[end] != '》' {
				end++
			}
			ruby := string(rs[k+1 : min(end, len(rs))])
			start := rubyStart
			if start < 0 {
				// 直前の同じ種類の文字の続き
				start = len(ln.chars)
				kd := 0
				if start > 0 {
					kd = kindOf(ln.chars[start-1].r)
				}
				for start > 0 && kd != 0 && kindOf(ln.chars[start-1].r) == kd && ln.chars[start-1].ruby < 0 {
					start--
				}
			}
			if start < len(ln.chars) && ruby != "" {
				g := len(ln.rubies)
				ln.rubies = append(ln.rubies, aozoraRuby{start: start, end: len(ln.chars), text: ruby})
				for x := start; x < len(ln.chars); x++ {
					ln.chars[x].ruby = g
				}
			}
			rubyStart = -1
			k = end
			continue
		case r == '［' && k+1 < len(rs) && rs[k+1] == '＃':
			end := k + 2
			for end < len(rs) && rs[end] != '］' {
				end++
			}
			note := string(rs[k+2 : min(end, len(rs))])
			k = end
			// 外字：※ の直後の注記
			if len(ln.chars) > 0 && ln.chars[len(ln.chars)-1].r == '※' {
				if m := reAozoraU.FindStringSubmatch(note); m != nil {
					if v, err := strconv.ParseUint(m[1], 16, 32); err == nil {
						ln.chars[len(ln.chars)-1].r = rune(v)
					}
				}
				if strings.Contains(note, "水準") || strings.Contains(note, "U+") || strings.Contains(note, "、") {
					continue
				}
			}
			indent = ln.applyNote(note, indent, &open)
			continue
		}
		ln.chars = append(ln.chars, aozoraChar{r: r, ruby: -1})
		for _, sp := range open {
			switch sp.kind {
			case "em":
				ln.chars[len(ln.chars)-1].em = true
			case "tcy":
				ln.chars[len(ln.chars)-1].tcy = true
			}
		}
	}
	return ln, indent
}

var aozoraHeads = map[string]int{"大見出し": 1, "中見出し": 2, "小見出し": 3, "窓大見出し": 1, "窓中見出し": 2, "窓小見出し": 3, "同行大見出し": 1, "同行中見出し": 2, "同行小見出し": 3}

func toHalfDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return r - '０' + '0'
		}
		return r
	}, s)
}

// applyNote は、注記を 1 つ解釈する。解釈できなければ何もしない（読み捨てる）。
func (ln *aozoraLine) applyNote(note string, indent int, open *[]aozoraSpan) int {
	n := toHalfDigits(note)
	switch {
	case n == "改ページ" || n == "改丁" || n == "改段" || n == "改見開き":
		ln.pb = true
		return indent
	case strings.HasPrefix(n, "ここから") && strings.HasSuffix(n, "字下げ"):
		if v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(n, "ここから"), "字下げ")); err == nil {
			ln.indent = v
			return v
		}
		return indent
	case strings.HasPrefix(n, "ここで字下げ終わり") || n == "ここで字下げ終り":
		ln.indent = 0
		return 0
	case strings.HasSuffix(n, "字下げ") && len(ln.chars) == 0:
		if v, err := strconv.Atoi(strings.TrimSuffix(n, "字下げ")); err == nil {
			ln.indent = v
		}
		return indent
	}
	for name, lv := range aozoraHeads {
		if n == name {
			*open = append(*open, aozoraSpan{"head", len(ln.chars), lv})
			ln.head = lv
			return indent
		}
		if n == name+"終わり" {
			return indent
		}
	}
	if strings.Contains(n, "傍点") && !strings.Contains(n, "「") {
		if strings.HasSuffix(n, "終わり") {
			ln.closeSpan(open, "em")
		} else {
			*open = append(*open, aozoraSpan{"em", len(ln.chars), 0})
		}
		return indent
	}
	if n == "縦中横" || n == "縦中横終わり" {
		if n == "縦中横" {
			*open = append(*open, aozoraSpan{"tcy", len(ln.chars), 0})
		} else {
			ln.closeSpan(open, "tcy")
		}
		return indent
	}
	// 「X」…：直前の X に付ける
	if m := reQuoted.FindStringSubmatch(note); m != nil {
		target, rest := []rune(m[1]), m[2]
		start := ln.findBack(target)
		if start < 0 {
			return indent
		}
		end := start + len(target)
		switch {
		case strings.HasPrefix(rest, "は") && aozoraHeads[strings.TrimPrefix(rest, "は")] > 0:
			ln.head = aozoraHeads[strings.TrimPrefix(rest, "は")]
		case strings.Contains(rest, "傍点"):
			for x := start; x < end; x++ {
				ln.chars[x].em = true
			}
		case rest == "は縦中横":
			for x := start; x < end; x++ {
				ln.chars[x].tcy = true
			}
		default:
			if mm := reQuoted.FindStringSubmatch(strings.TrimPrefix(strings.TrimPrefix(rest, "に"), "の左に")); mm != nil &&
				(strings.HasSuffix(mm[2], "の注記") || strings.HasSuffix(mm[2], "のルビ")) {
				g := len(ln.rubies)
				ln.rubies = append(ln.rubies, aozoraRuby{start: start, end: end, text: mm[1]})
				for x := start; x < end; x++ {
					ln.chars[x].ruby = g
				}
			}
		}
	}
	return indent
}

func (ln *aozoraLine) closeSpan(open *[]aozoraSpan, kind string) {
	for i := len(*open) - 1; i >= 0; i-- {
		if (*open)[i].kind == kind {
			*open = append((*open)[:i], (*open)[i+1:]...)
			return
		}
	}
}

// findBack は、行の終わりから target を探し、始まりの位置を返す（なければ -1）。
func (ln *aozoraLine) findBack(target []rune) int {
	for s := len(ln.chars) - len(target); s >= 0; s-- {
		ok := true
		for k, r := range target {
			if ln.chars[s+k].r != r {
				ok = false
				break
			}
		}
		if ok {
			return s
		}
	}
	return -1
}

// aozoraRuns は、文字の並びを、同じ飾りの続き（run）にまとめる。
func aozoraRuns(ln aozoraLine) []rRun {
	var out []rRun
	for i := 0; i < len(ln.chars); {
		c := ln.chars[i]
		j := i + 1
		if c.ruby >= 0 {
			g := ln.rubies[c.ruby]
			j = g.end
		} else {
			for j < len(ln.chars) && ln.chars[j].ruby < 0 && ln.chars[j].em == c.em && ln.chars[j].tcy == c.tcy {
				j++
			}
		}
		var s strings.Builder
		for k := i; k < j; k++ {
			s.WriteRune(ln.chars[k].r)
		}
		run := rRun{S: s.String(), Em: c.em, TCY: c.tcy}
		if c.ruby >= 0 {
			run.Ruby = ln.rubies[c.ruby].text
		}
		out = append(out, run)
		i = j
	}
	return out
}
