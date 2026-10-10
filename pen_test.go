package main

import (
	"encoding/binary"
	"image"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

func TestPenDescriptor(t *testing.T) {
	if l := layoutOf(hidDescCombo, "x"); l.PenID != hidPenID || l.MouseID != hidMouseID || l.MediaID != hidMediaID {
		t.Errorf("combo: %+v", l)
	}
	// 前の版（ペンのない）ディスクリプタのガジェットでも、キーボードとマウスとメディアキーはそのまま
	if l := layoutOf(hidDescComboNoPen, "x"); l.PenID != 0 || l.MouseID != hidMouseID || l.MediaID != hidMediaID || !strings.Contains(l.Source, "pen tablet does not work") {
		t.Errorf("old combo: %+v", l)
	}
	// ペンのレポートは 8 バイト（キーボードの 9 バイト以下。gadget-setup.sh の report_length）
	h := &hidRec{}
	p := NewPen(h, hidPenID)
	p.Report(true, 0x1234, 0x7fff, 512)
	p.Report(false, 0x1234, 0x7fff, 512)
	r := h.take()
	if len(r) != 2 || len(r[0]) != 8 || r[0][0] != hidPenID || r[0][1] != penTip|penInRange ||
		binary.LittleEndian.Uint16(r[0][2:]) != 0x1234 || binary.LittleEndian.Uint16(r[0][4:]) != 0x7fff || binary.LittleEndian.Uint16(r[0][6:]) != 512 {
		t.Fatalf("reports % x", r)
	}
	if r[1][1] != 0 || binary.LittleEndian.Uint16(r[1][6:]) != 0 {
		t.Errorf("lift % x", r[1])
	}
	// 離したのが届かなければ送り直す。動かしているあいだのものは送り直さない
	h.fail = true
	p.Report(true, 1, 1, 1)
	p.retry()
	p.Report(false, 1, 1, 0)
	h.fail = false
	p.retry()
	if r := h.take(); len(r) != 1 || r[0][1] != 0 {
		t.Errorf("retry % x", r)
	}
	// ペンのない形では送らない
	off := NewPen(h, 0)
	off.Report(true, 1, 1, 1)
	if off.Available() || len(h.take()) != 0 {
		t.Error("pen without the descriptor sent a report")
	}
}

func TestPenMapping(t *testing.T) {
	cell := image.Rect(100, 50, 501, 351) // 401×301
	full := &PenTabDef{Map: penMapFull, Pressure: true, Pen: defaultPen}
	if x, y := full.penPoint(cell, fpt{100, 50}); x != 0 || y != 0 {
		t.Errorf("top left %d,%d", x, y)
	}
	if x, y := full.penPoint(cell, fpt{500, 350}); x != penLogicalMax || y != penLogicalMax {
		t.Errorf("bottom right %d,%d", x, y)
	}
	if x, _ := full.penPoint(cell, fpt{-50, 0}); x != 0 {
		t.Error("outside the cell is clamped")
	}
	// 縦横比を保つ：4:3 のセルを 16:9 の画面に当てはめると、左右に余白
	asp := &PenTabDef{Map: penMapAspect, Screen: image.Pt(1920, 1080), Pressure: true, Pen: defaultPen}
	x0, _ := asp.penPoint(cell, fpt{100, 200})
	x1, y1 := asp.penPoint(cell, fpt{500, 350})
	if px := float64(x0) / penLogicalMax * 1920; px < 239 || px > 241 { // (1920 - 1080*4/3) / 2 = 240
		t.Errorf("aspect left %v px", px)
	}
	if px := float64(x1) / penLogicalMax * 1920; px < 1679 || px > 1681 || y1 != penLogicalMax {
		t.Errorf("aspect right %v px, y %d", px, y1)
	}
	reg := &PenTabDef{Map: penMapRegion, Screen: image.Pt(1920, 1080), Region: image.Rect(960, 0, 1920, 540), Pen: defaultPen}
	if x, y := reg.penPoint(cell, fpt{100, 50}); x < penLogicalMax/2 || x > penLogicalMax/2+1 || y != 0 {
		t.Errorf("region %d,%d", x, y)
	}
	// 筆圧
	if v := full.penPressure(400); v != 1 {
		t.Errorf("pressure at low %d", v)
	}
	if v := full.penPressure(1400); v < 510 || v > 514 {
		t.Errorf("pressure in the middle %d", v)
	}
	if v := full.penPressure(4000); v != penPressureMax {
		t.Errorf("pressure at high %d", v)
	}
	if v := reg.penPressure(500); v != penPressureMax {
		t.Errorf("pressure: false sends %d", v)
	}
}

const penConfig = `
touch: { min_x: 226, max_x: 3938, min_y: 3803, max_y: 384 }
layers:
  - name: base
    touch:
      cols: 1
      rows: 1
      cells:
        "0,0": { widget: pentablet }
`

// セルをなぞると、PC にペンのレポートが届く。マウスのない形と、PC から外したときは送らずに知らせる
func TestPenTabletCell(t *testing.T) {
	_, km := compileText(t, penConfig)
	h := &hidRec{}
	s := &State{hid: h, active: map[string]Combo{}, pen: NewPen(h, hidPenID)}
	e := NewEngine(km, s)
	rt := NewWidgetRT(nil)
	rt.pen = s.pen
	e.SetWidgets(rt)
	var notes []string
	e.SetNotify(func(m string) { notes = append(notes, m) })
	syn := newSynth()
	syn.stroke(fpt{200, 100}, fpt{600, 300}, 500*time.Millisecond)
	tp := &touchProc{e: e}
	play := func() {
		for _, ev := range syn.rec.Events {
			tp.event(&evdev.InputEvent{Type: ev.Type, Code: ev.Code, Value: ev.Value}, time.Unix(0, 0).Add(ev.T))
		}
	}
	play()
	r := h.take()
	if len(r) < 10 {
		t.Fatalf("%d reports", len(r))
	}
	last := r[len(r)-1]
	if last[0] != hidPenID || last[1] != 0 {
		t.Errorf("the last report is not a lift: % x", last)
	}
	for _, rep := range r[:len(r)-1] {
		if rep[1] != penTip|penInRange {
			t.Fatalf("report % x is not touching", rep)
		}
		x := binary.LittleEndian.Uint16(rep[2:])
		if f := float64(x) / penLogicalMax; f < 0.2 || f > 0.8 {
			t.Errorf("x %.2f is outside the stroke (a jump was sent)", f)
		}
	}
	// マウスのない形
	s.pen = NewPen(h, 0)
	rt.pen = s.pen
	play()
	if len(h.take()) != 0 || len(notes) == 0 || !strings.Contains(notes[0], "マウスあり") {
		t.Errorf("mouse off: notes %q", notes)
	}
}
