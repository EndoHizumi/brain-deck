package main

import (
	"bytes"
	"encoding/json"
	"image"
	"strings"
	"sync"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"

	"lefthand/internal/ink"
)

const memoConfig = `
touch: { min_x: 226, max_x: 3938, min_y: 3803, max_y: 384 }
layers:
  - name: base
    touch:
      cols: 1
      rows: 1
      cells:
        "0,0": { widget: memo }
`

// memoRig は、手書きメモのセルを、エンジンと（描画の goroutine を使わない）画面で動かす。
type memoRig struct {
	t     *testing.T
	e     *Engine
	memos *MemoService
	disp  *Display
	tp    *touchProc
	syn   *synth
	mu    sync.Mutex
	inks  int
	notes []string
}

func newMemoRig(t *testing.T, rot int) *memoRig {
	old := imageFreeSpace
	imageFreeSpace = func(string) (int64, int64, error) { return 1 << 30, 3 << 30, nil }
	t.Cleanup(func() { imageFreeSpace = old })
	_, km := compileText(t, memoConfig)
	r := &memoRig{t: t}
	r.memos = OpenMemoService(t.TempDir())
	r.e = NewEngine(km, &State{hid: &hidRec{}, active: map[string]Combo{}})
	r.e.SetRotation(rot, 800, 480, nil)
	rt := NewWidgetRT(nil)
	rt.memos = r.memos
	r.e.SetWidgets(rt)
	r.memos.SetNotify(func(m string) { r.mu.Lock(); r.notes = append(r.notes, m); r.mu.Unlock() })
	pw, ph := 800, 480
	r.disp = &Display{cv: NewCanvas(pw, ph, pw*2, rgb565, rot), env: func() WidgetEnv { return WidgetEnv{Now: time.Now(), Memo: r.memos.Snapshot()} }}
	l := buildLayout(km, r.e.View())
	l.W, l.H = r.disp.cv.W, r.disp.cv.H
	r.disp.layout, r.disp.drawn = l, make([]bool, len(l.Cells))
	r.disp.drawAllLocked(l)
	// 描き足しは、その場で裏画面に描く（実機では描画の goroutine）
	rt.SetInk(func(f func(*Canvas) image.Rectangle) {
		r.mu.Lock()
		r.inks++
		r.mu.Unlock()
		f(r.disp.cv)
	})
	r.tp = &touchProc{e: r.e}
	r.syn = newSynth()
	r.syn.rec.Header.Screen = [2]int{pw, ph}
	return r
}

// play は、作った記録の続き（前に流したものより後）を流す。座標は物理の画面のドット。
func (r *memoRig) play() {
	for _, ev := range r.syn.rec.Events {
		r.tp.event(&evdev.InputEvent{Type: ev.Type, Code: ev.Code, Value: ev.Value}, time.Unix(0, 0).Add(ev.T))
	}
	r.syn.rec.Events = nil
}

// logical は、論理の座標（回した画面）を、物理の画面のドットにする。
func (r *memoRig) phys(p image.Point) fpt {
	x, y := r.disp.cv.phys(p.X, p.Y)
	return fpt{float64(x), float64(y)}
}

func (r *memoRig) geom() memoGeom {
	l := r.disp.layout
	return memoGeometry(l.rect(0, 0).Inset(cellGap + textMargin))
}

func (r *memoRig) tapButton(b int) {
	c := r.geom().btns[b]
	r.syn.tap(r.phys(image.Pt((c.Min.X+c.Max.X)/2, (c.Min.Y+c.Max.Y)/2)), 60*time.Millisecond)
	r.syn.wait(400 * time.Millisecond)
	r.play()
}

func (r *memoRig) draw(from, to image.Point) {
	r.syn.stroke(r.phys(from), r.phys(to), 600*time.Millisecond)
	r.syn.wait(400 * time.Millisecond)
	r.play()
	time.Sleep(defaultPen.Join + 30*time.Millisecond) // 触れ直すのを待ってから、線を終える
}

// settle は、join の待ちが終わるのを待ち、紙を描き直す（実機の Poke と同じ）。
func (r *memoRig) settle() {
	time.Sleep(defaultPen.Join + 30*time.Millisecond)
	r.disp.redrawWidgets()
}

// same は、裏画面が、今の状態で全体を描いたものと同じかを確かめる。
func (r *memoRig) same(what string) {
	r.t.Helper()
	r.settle()
	ref := NewCanvas(r.disp.cv.pw, r.disp.cv.ph, r.disp.cv.stride, rgb565, r.disp.cv.rot)
	rl := *r.disp.layout
	rl.Env = r.disp.env()
	drawAll(ref, &rl, r.disp.drawn)
	if !bytes.Equal(ref.pix, r.disp.cv.pix) {
		n := 0
		for i := range ref.pix {
			if ref.pix[i] != r.disp.cv.pix[i] {
				n++
			}
		}
		r.t.Fatalf("%s: %d bytes differ from a full redraw", what, n)
	}
}

func (r *memoRig) page() ink.Page {
	v := r.memos.Snapshot()
	if v.page == nil {
		return ink.Page{}
	}
	return *v.page
}

func TestMemoWriteUndoPages(t *testing.T) {
	r := newMemoRig(t, 0)
	r.same("empty")
	g := r.geom()
	r.draw(image.Pt(100, 200), image.Pt(400, 260))
	if r.inks < 5 {
		t.Fatalf("drew %d ink ops while writing", r.inks)
	}
	p := r.page()
	if len(p.Strokes) != 1 || p.W != g.paper.Dx() || p.H != g.paper.Dy() || p.Strokes[0].W != memoWidths[1] {
		t.Fatalf("page %dx%d, strokes %+v", p.W, p.H, p.Strokes)
	}
	r.same("after the first stroke (the memo was made)")
	before := r.disp.wkeys[0]
	r.draw(image.Pt(120, 100), image.Pt(160, 140))
	r.same("after a stroke (drawn only by ink)")
	if r.disp.wkeys[0] != before {
		t.Error("the paper was redrawn after a stroke (it should keep the ink)")
	}
	r.tapButton(memoBtnUndo)
	// 太いペンと消しゴム
	r.tapButton(memoBtnThick)
	r.draw(image.Pt(100, 300), image.Pt(500, 300))
	r.tapButton(memoBtnErase)
	r.draw(image.Pt(300, 150), image.Pt(300, 400))
	p = r.page()
	if len(p.Strokes) != 3 || p.Strokes[1].W != 8 || !p.Strokes[2].Erase || p.Strokes[2].W != memoEraseW {
		t.Fatalf("strokes %+v", p.Strokes)
	}
	r.same("thick and eraser")
	// 戻す
	r.tapButton(memoBtnUndo)
	if len(r.page().Strokes) != 2 {
		t.Fatal("undo")
	}
	r.same("after undo")
	// 全消は短く押すと説明だけ。長押し（ここでは直接）で消して、戻すで戻る
	r.tapButton(memoBtnClear)
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	notes := strings.Join(r.notes, "\n")
	r.mu.Unlock()
	if len(r.page().Strokes) != 2 || !strings.Contains(notes, "長押し") {
		t.Fatalf("a short tap on 全消: %d strokes, notes %q", len(r.page().Strokes), notes)
	}
	r.memos.clearPage()
	if len(r.page().Strokes) != 0 {
		t.Fatal("clear")
	}
	r.tapButton(memoBtnUndo)
	if len(r.page().Strokes) != 2 {
		t.Fatal("undo after clear")
	}
	// ページを足して、めくる。新しいメモ
	r.tapButton(memoBtnPage)
	r.draw(image.Pt(200, 200), image.Pt(250, 300))
	r.tapButton(memoBtnNew)
	r.draw(image.Pt(200, 200), image.Pt(260, 200))
	if v := r.memos.Snapshot(); v.label != "2-1" {
		t.Fatalf("label %q", v.label)
	}
	r.tapButton(memoBtnPrev)
	if v := r.memos.Snapshot(); v.label != "1-2" || len(v.page.Strokes) != 1 {
		t.Fatalf("prev: %q", v.label)
	}
	r.same("after turning a page")
	r.tapButton(memoBtnPrev)
	r.tapButton(memoBtnPrev) // 最初より前には行かない
	if v := r.memos.Snapshot(); v.label != "1-1" {
		t.Fatalf("prev twice: %q", v.label)
	}
	// 保存して開き直す
	r.memos.Flush()
	again := OpenMemoService(r.memos.dir)
	l := again.List()
	if len(l) != 2 || l[0].Pages != 2 || l[0].Strokes != 3 || l[1].Strokes != 1 {
		t.Fatalf("reopened %+v", l)
	}
	// get_memo と delete
	b, total, err := again.Read("m1", 0, 0)
	var m Memo
	if err != nil || int64(len(b)) != total || json.Unmarshal(b, &m) != nil || len(m.Pages) != 2 {
		t.Fatalf("read %v", err)
	}
	if err := again.Delete("m1"); err != nil || len(again.List()) != 1 {
		t.Fatalf("delete %v", err)
	}
	if _, _, err := again.Read("m9", 0, 0); err == nil {
		t.Error("read m9")
	}
}

// 横向きで書いたページを縦向きで見る（縮めて置く）。縦向きで書き足しても、描き足しと全体の描き直しが同じ
func TestMemoRotated(t *testing.T) {
	r := newMemoRig(t, 0)
	r.draw(image.Pt(100, 200), image.Pt(500, 260))
	r.memos.Flush()
	p := newMemoRig(t, 90)
	p.memos = OpenMemoService(r.memos.dir)
	rt := NewWidgetRT(nil)
	rt.memos = p.memos
	rt.SetInk(func(f func(*Canvas) image.Rectangle) { f(p.disp.cv) })
	p.e.SetWidgets(rt)
	p.disp.env = func() WidgetEnv { return WidgetEnv{Now: time.Now(), Memo: p.memos.Snapshot()} }
	p.disp.drawAllLocked(p.disp.layout)
	pg := p.page()
	v := viewOf(p.geom().paper, &pg)
	if v.num == v.den || v.area.Dx() > p.geom().paper.Dx() {
		t.Fatalf("view %+v", v)
	}
	p.draw(image.Pt(100, 300), image.Pt(300, 400))
	if got := p.page(); len(got.Strokes) != 2 || got.W != pg.W {
		t.Fatalf("strokes %d, page width %d", len(got.Strokes), got.W)
	}
	p.same("portrait, scaled page")
}

// Brain の紙と、brain-deck memo get の PNG（ink.Render）は、同じ画素にインクがのる（色は RGB565 に丸める違いだけ）
func TestMemoScreenMatchesPNG(t *testing.T) {
	r := newMemoRig(t, 0)
	r.draw(image.Pt(100, 200), image.Pt(400, 260))
	r.tapButton(memoBtnThick)
	r.draw(image.Pt(150, 100), image.Pt(160, 400))
	r.settle()
	pg := r.page()
	rgb := ink.Render(&pg)
	paper := r.geom().paper
	diff := 0
	for y := 0; y < pg.H; y++ {
		for x := 0; x < pg.W; x++ {
			inkPNG := rgb[(y*pg.W+x)*3] == ink.Ink[0]
			c := r.disp.cv.at(paper.Min.X+x, paper.Min.Y+y)
			inkScreen := c.R < 0x80
			if inkPNG != inkScreen {
				diff++
			}
		}
	}
	if diff != 0 {
		t.Errorf("%d pixels differ between the screen and the PNG", diff)
	}
}
