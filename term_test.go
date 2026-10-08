package main

import (
	"strings"
	"testing"
)

func newTestTerm(cols, rows int) (*Term, *strings.Builder) {
	var replies strings.Builder
	t := NewTerm(cols, rows, 100, func(b []byte) { replies.Write(b) })
	return t, &replies
}

func feed(t *Term, s string) { t.Write([]byte(s)) }

func wantLines(tt *testing.T, t *Term, want ...string) {
	tt.Helper()
	got := t.Text()
	for i, w := range want {
		if got[i] != w {
			tt.Errorf("line %d = %q, want %q\nscreen:\n%s", i, got[i], w, strings.Join(got, "\n"))
		}
	}
}

func TestTermPrintWrapScroll(t *testing.T) {
	tm, _ := newTestTerm(10, 3)
	feed(tm, "hello\r\nworld\r\n")
	wantLines(t, tm, "hello", "world", "")
	feed(tm, "0123456789AB")
	wantLines(t, tm, "world", "0123456789", "AB")
	feed(tm, "\r\nx")
	// 10 桁目まで書いたあとは折り返しを待つ。AB は次の行に出て、1 行流れる
	wantLines(t, tm, "0123456789", "AB", "x")
	if _, n := tm.View(); n != 2 {
		t.Errorf("history = %d lines, want 2", n)
	}
}

func TestTermCursorAndErase(t *testing.T) {
	tm, _ := newTestTerm(10, 4)
	feed(tm, "aaaaaaaaaa\r\nbbbbbbbbbb\r\ncccccccccc")
	feed(tm, "\x1b[2;3H\x1b[K")
	wantLines(t, tm, "aaaaaaaaaa", "bb", "cccccccccc")
	feed(tm, "\x1b[1;5H\x1b[1K")
	wantLines(t, tm, "     aaaaa")
	feed(tm, "\x1b[3;4H\x1b[2P")
	wantLines(t, tm, "     aaaaa", "bb", "cccccccc")
	feed(tm, "\x1b[3;1H\x1b[3@")
	wantLines(t, tm, "     aaaaa", "bb", "   ccccccc")
	feed(tm, "\x1b[2J")
	wantLines(t, tm, "", "", "", "")
	feed(tm, "\x1b[4;10Hz\x1b[6n")
	if x, y := tm.Cursor(); x != 9 || y != 3 {
		t.Errorf("cursor = %d,%d", x, y)
	}
}

func TestTermWide(t *testing.T) {
	tm, _ := newTestTerm(6, 2)
	feed(tm, "aあい")
	wantLines(t, tm, "aあい")
	if x, _ := tm.Cursor(); x != 5 {
		t.Errorf("cursor x = %d, want 5", x)
	}
	// 右端に 1 桁しか残っていないので、全角は次の行に出る
	feed(tm, "う")
	wantLines(t, tm, "aあい", "う")
	// 全角の右半分に書くと、左半分も消える
	feed(tm, "\x1b[1;3Hx")
	wantLines(t, tm, "a xい", "う")
	// 結合文字は読み捨てる
	feed(tm, "\x1b[2;3Hé")
	wantLines(t, tm, "a xい", "うe")
}

func TestTermUTF8Split(t *testing.T) {
	tm, _ := newTestTerm(10, 1)
	b := []byte("日本")
	for _, c := range b {
		tm.Write([]byte{c})
	}
	wantLines(t, tm, "日本")
}

func TestTermScrollRegionAndAlt(t *testing.T) {
	tm, _ := newTestTerm(5, 4)
	feed(tm, "1\r\n2\r\n3\r\n4")
	feed(tm, "\x1b[2;3r\x1b[3;1H\n")
	wantLines(t, tm, "1", "3", "", "4")
	feed(tm, "\x1b[2;1H\x1bM")
	wantLines(t, tm, "1", "", "3", "4")
	feed(tm, "\x1b[r\x1b[?1049h\x1b[2JALT")
	wantLines(t, tm, "ALT", "", "", "")
	feed(tm, "\x1b[?1049l")
	wantLines(t, tm, "1", "", "3", "4")
	// alt の画面の出力は履歴に残さない
	if _, n := tm.View(); n != 0 {
		t.Errorf("history = %d", n)
	}
}

func TestTermSGR(t *testing.T) {
	tm, _ := newTestTerm(10, 1)
	feed(tm, "\x1b[1;31;44mA\x1b[38;5;200;48;2;1;2;3mB\x1b[38:2::9:8:7;4mC\x1b[0mD")
	l := tm.line(0)
	if l[0].fg != tcPalette(1) || l[0].bg != tcPalette(4) || l[0].at&attrBold == 0 {
		t.Errorf("A = %+v", l[0])
	}
	if l[1].fg != tcPalette(200) || l[1].bg != tcTrue(1, 2, 3) {
		t.Errorf("B = %+v", l[1])
	}
	if l[2].fg != tcTrue(9, 8, 7) || l[2].at&attrUnderline == 0 || l[2].bg != tcTrue(1, 2, 3) {
		t.Errorf("C = %+v", l[2])
	}
	if l[3].fg != tcDefault || l[3].bg != tcDefault || l[3].at != 0 {
		t.Errorf("D = %+v", l[3])
	}
}

func TestTermReplies(t *testing.T) {
	tm, rep := newTestTerm(100, 31)
	feed(tm, "\x1b[18t\x1b[5;7H\x1b[6n\x1b[c\x1b[>c\x1b]11;?\x07\x1b[?2004$p\x1b[5n")
	want := "\x1b[8;31;100t\x1b[5;7R\x1b[?1;2c\x1b[>0;10;1c\x1b]11;rgb:0000/0000/0000\a\x1b[?2004;2$y\x1b[0n"
	if rep.String() != want {
		t.Errorf("replies = %q, want %q", rep.String(), want)
	}
}

func TestTermIgnoresUnknown(t *testing.T) {
	tm, _ := newTestTerm(20, 1)
	feed(tm, "\x1b]0;title\x07\x1bP+q544e\x1b\\\x1b[?1000h\x1b[>4;2m\x1b[ q\x1b[22;0;0tok")
	wantLines(t, tm, "ok")
	if tm.Title != "title" {
		t.Errorf("title = %q", tm.Title)
	}
}

func TestTermDECGraphics(t *testing.T) {
	tm, _ := newTestTerm(5, 1)
	feed(tm, "\x1b(0lqk\x1b(Bq")
	wantLines(t, tm, "┌─┐q")
}

func TestTermFrame(t *testing.T) {
	tm, _ := newTestTerm(4, 3)
	var f termFrame
	tm.TakeFrame(&f)
	if !f.All {
		t.Fatal("first frame should be full")
	}
	feed(tm, "a")
	tm.TakeFrame(&f)
	if f.All || !f.Dirty[0] || f.Dirty[1] || f.Dirty[2] {
		t.Errorf("dirty = %v all=%v", f.Dirty, f.All)
	}
	feed(tm, "\r\nb\r\nc\r\nd")
	tm.TakeFrame(&f)
	// 画面全体が 1 行流れた。新しい行と、カーソルのあった行を描き直す
	if f.All || f.Scrolled != 1 {
		t.Errorf("scrolled = %d all=%v", f.Scrolled, f.All)
	}
	if !f.Dirty[2] || string(f.Lines[2][0].r) != "d" {
		t.Errorf("dirty = %v", f.Dirty)
	}
	tm.TakeFrame(&f)
	for y, d := range f.Dirty {
		if d && y != 2 { // カーソルの行だけ
			t.Errorf("row %d dirty without change", y)
		}
	}
	// 履歴を見る
	if !tm.Scroll(1) {
		t.Fatal("cannot scroll back")
	}
	tm.TakeFrame(&f)
	if !f.All || f.CurOn || string(f.Lines[0][0].r) != "a" {
		t.Errorf("history frame: all=%v cur=%v line0=%q", f.All, f.CurOn, string(f.Lines[0][0].r))
	}
}

func TestRuneWidth(t *testing.T) {
	for r, w := range map[rune]int{'a': 1, 'あ': 2, '漢': 2, 'ｱ': 1, '─': 1, '①': 1, '́': 0, '한': 2, '🍣': 2, 'é': 1, '\t': 0} {
		if got := runeWidth(r); got != w {
			t.Errorf("runeWidth(%q) = %d, want %d", r, got, w)
		}
	}
}
