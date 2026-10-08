package main

import (
	"image"
	"image/png"
	"os"
	"testing"
)

// termSample は、プレビューと描画のテストに使う、PC の出力の例。
const termSample = "\r\nUbuntu 22.04 LTS hizumi-pc ttyACM0\r\n\r\nhizumi-pc login: hizumi\r\nPassword: \r\n" +
	"Last login: Thu Oct  8 02:10:11 JST 2026 on ttyACM0\r\n" +
	"\x1b[01;32mhizumi@hizumi-pc\x1b[00m:\x1b[01;34m~\x1b[00m$ ls --color\r\n" +
	"\x1b[0m\x1b[01;34mDesktop\x1b[0m  \x1b[01;34mbrainix-lefthand-device\x1b[0m  \x1b[01;32mrun.sh\x1b[0m  メモ.txt  日本語のファイル名\r\n" +
	"\x1b[01;32mhizumi@hizumi-pc\x1b[00m:\x1b[01;34m~\x1b[00m$ cat /etc/passwd | grep -c bash && echo '{ok}' > /tmp/x; echo $? ~ \\ [a] <b> & ^ * #\r\n" +
	"3\r\n0 ~ \\ [a] <b> & ^ * #\r\n" +
	"┌──────────┬────┐\r\n│ CPU  \x1b[32m||||\x1b[0m  │ 12%│\r\n└──────────┴────┘\r\n" +
	"\x1b[7m 反転 \x1b[0m \x1b[4m下線\x1b[0m \x1b[1m太字\x1b[0m \x1b[38;5;208m256色\x1b[0m \x1b[38;2;80;160;255m24bit\x1b[0m\r\n" +
	"\x1b[01;32mhizumi@hizumi-pc\x1b[00m:\x1b[01;34m~\x1b[00m$ "

func renderTermPNG(t *testing.T, fontName string, st termStatus, out string) *Canvas {
	cv := NewCanvas(800, 480, 1600, rgb565, 0)
	g := termLayout(800, 480, fontName)
	tm := NewTerm(g.Cols, g.Rows, 100, nil)
	tm.Write([]byte(termSample))
	r := &termRenderer{g: g}
	r.render(cv, true, func(f *termFrame) termStatus { tm.TakeFrame(f); return st })
	if out != "" {
		w, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if err := png.Encode(w, cv.Image()); err != nil {
			t.Fatal(err)
		}
	}
	return cv
}

// LEFTHAND_TERM_PNG=dir go test -run TermPreview で、端末モードの画面の例を書き出す
func TestTermPreview(t *testing.T) {
	dir := os.Getenv("LEFTHAND_TERM_PNG")
	st := termStatus{State: "ログイン中", Level: 3, Transport: "シリアル", Pressed: -1, Exitable: true}
	for _, fn := range []string{"wide", "narrow"} {
		out := ""
		if dir != "" {
			out = dir + "/terminal-" + fn + ".png"
		}
		renderTermPNG(t, fn, st, out)
	}
	if dir != "" {
		st2 := st
		st2.Page, st2.Pressed, st2.Ctrl = 1, 3, true
		renderTermPNG(t, "wide", st2, dir+"/terminal-page2.png")
	}
}

func TestTermLayout(t *testing.T) {
	g := termLayout(800, 480, "wide")
	if g.Cols != 100 || g.Rows != 31 {
		t.Errorf("wide: %dx%d, want 100x31", g.Cols, g.Rows)
	}
	if g.Text.Dy() != g.Rows*fontH || g.Text.Max.Y > g.Pad.Min.Y || g.Text.Min.Y < g.Status.Max.Y {
		t.Errorf("text area %v (status %v, pad %v)", g.Text, g.Status, g.Pad)
	}
	if g.Pad.Max.X != 800-termBandSkip {
		t.Errorf("pad %v overlaps the soft-key band", g.Pad)
	}
	if n := termLayout(800, 480, "narrow"); n.Cols != 200 {
		t.Errorf("narrow: %d cols", n.Cols)
	}
	c, r, ok := g.padKeyAt(image.Pt(5, 479))
	if !ok || c != 0 || r != 1 {
		t.Errorf("padKeyAt bottom-left = %d,%d,%v", c, r, ok)
	}
	if _, _, ok := g.padKeyAt(image.Pt(790, 470)); ok {
		t.Error("the band area is a pad key")
	}
}

// 流れたときに点をずらして描いた画面が、全体を描き直した画面と同じになること
func TestTermScrollBlitSame(t *testing.T) {
	g := termLayout(800, 480, "wide")
	tm := NewTerm(g.Cols, g.Rows, 100, nil)
	cv := NewCanvas(800, 480, 1600, rgb565, 0)
	r := &termRenderer{g: g}
	st := termStatus{State: "x", Pressed: -1}
	take := func(f *termFrame) termStatus { tm.TakeFrame(f); return st }
	r.render(cv, true, take)
	for i := 0; i < 50; i++ {
		tm.Write([]byte("line \x1b[31mred\x1b[0m あいう " + itoaB(i) + "\r\n"))
		if i%7 == 0 {
			r.render(cv, false, take)
		}
	}
	tm.Write([]byte("\x1b[5;3Hmid\x1b[K"))
	r.render(cv, false, take)
	if r.frame.Scrolled != 0 {
		t.Log("last frame scrolled")
	}
	cv2 := NewCanvas(800, 480, 1600, rgb565, 0)
	tm.Invalidate()
	(&termRenderer{g: g}).render(cv2, true, take)
	a, b := cv.Image(), cv2.Image()
	for y := g.Text.Min.Y; y < g.Text.Max.Y; y++ {
		for x := 0; x < 800; x++ {
			if a.RGBAAt(x, y) != b.RGBAAt(x, y) {
				t.Fatalf("pixel %d,%d differs after incremental drawing", x, y)
			}
		}
	}
}

// 全体を描き直したときは、画面全体を写す（隙間に前の画面が残らない）
func TestTermFullBlitsWholeScreen(t *testing.T) {
	g := termLayout(800, 480, "wide")
	tm := NewTerm(g.Cols, g.Rows, 10, nil)
	cv := NewCanvas(800, 480, 1600, rgb565, 0)
	rs := (&termRenderer{g: g}).render(cv, true, func(f *termFrame) termStatus { tm.TakeFrame(f); return termStatus{Pressed: -1} })
	if len(rs) != 1 || rs[0] != image.Rect(0, 0, 800, 480) {
		t.Errorf("full render blits %v", rs)
	}
}
