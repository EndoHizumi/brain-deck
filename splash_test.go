package main

import (
	"strings"
	"testing"
	"time"
)

func TestDrawSplash(t *testing.T) {
	for _, rot := range []int{0, 90, 180, 270} {
		cv := NewCanvas(800, 480, 800*rgb565.Bpp, rgb565, rot)
		drawSplash(cv, "test")
		lit := 0
		for y := 0; y < cv.H; y++ {
			for x := 0; x < cv.W; x++ {
				if cv.at(x, y) == colText {
					lit++
				}
			}
		}
		if lit < 200 {
			t.Errorf("rotate %d: only %d text pixels", rot, lit)
		}
		// 文字は画面の中に収まる
		if w := font.textWidth(splashText) * 4; w > cv.W {
			t.Errorf("rotate %d: text %d px is wider than %d", rot, w, cv.W)
		}
	}
}

func TestStartupTrace(t *testing.T) {
	tr := newStartupTrace()
	tr.mark("a")
	time.Sleep(2 * time.Millisecond)
	tr.mark("b")
	tr.finish()
	tr.mark("c") // 終えたあとは覚えない
	if len(tr.marks) != 2 || !strings.HasPrefix(tr.marks[0], "a ") || !strings.HasPrefix(tr.marks[1], "b ") {
		t.Fatalf("marks %q", tr.marks)
	}
	if d, ok := sinceProcStart(); !ok || d <= 0 || d > time.Hour {
		t.Errorf("sinceProcStart = %v, %v", d, ok)
	}
}
