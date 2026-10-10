package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strings"
	"time"

	evdev "github.com/holoplot/go-evdev"

	"lefthand/internal/ink"
)

// ---------- ペンの記録の再生（-replay-pen） ----------
//
// -record-touch で記録したタッチ（.touch）を、線の読み取り（stroke.go）に通し、線の数、点の数、つないだ数、
// 捨てたサンプル、いちばん大きな点の間を書く。-replay-png で、読み取った線を紙に描いた PNG も書く
// （手書きメモと同じ描き方。太さは 3 ドット）。値は -replay-params の JSON で変えられる
// （{"min_pressure":600,"settle_ms":40,"smooth":3,"hold_back":2,"join_ms":100,"join_dist":30}）。

// penReplayParams は、-replay-params で変えられる線の読み取りの項目。
type penReplayParams struct {
	MinPressure *int     `json:"min_pressure"`
	SettleMS    *int     `json:"settle_ms"`
	Smooth      *int     `json:"smooth"`
	HoldBack    *int     `json:"hold_back"`
	JoinMS      *int     `json:"join_ms"`
	JoinDist    *float64 `json:"join_dist"`
}

func (r penReplayParams) apply(p *PenParams) {
	if r.MinPressure != nil {
		p.MinPressure = int32(*r.MinPressure)
	}
	if r.SettleMS != nil {
		p.Settle = time.Duration(*r.SettleMS) * time.Millisecond
	}
	if r.Smooth != nil {
		p.Smooth = max(*r.Smooth, 1)
	}
	if r.HoldBack != nil {
		p.HoldBack = max(*r.HoldBack, 0)
	}
	if r.JoinMS != nil {
		p.Join = time.Duration(*r.JoinMS) * time.Millisecond
	}
	if r.JoinDist != nil {
		p.JoinDist = *r.JoinDist
	}
}

// strokeCollector は、読み取った線を覚える。
type strokeCollector struct {
	strokes []ink.Stroke
	cur     *ink.Stroke
}

func (c *strokeCollector) StrokeDown(pt fpt, _ int32) {
	c.cur = &ink.Stroke{W: 3}
	c.cur.Add(ink.Pt{X: int(pt.X + 0.5), Y: int(pt.Y + 0.5)})
}
func (c *strokeCollector) StrokeMove(pt fpt, _ int32) {
	if c.cur != nil {
		c.cur.Add(ink.Pt{X: int(pt.X + 0.5), Y: int(pt.Y + 0.5)})
	}
}
func (c *strokeCollector) StrokeUp() {
	if c.cur != nil {
		c.strokes = append(c.strokes, *c.cur)
		c.cur = nil
	}
}

// replayPen は、記録を線の読み取りに通す。
func replayPen(rec *TouchRecording, p PenParams) (StrokeStats, []ink.Stroke) {
	h := &rec.Header
	base := time.Unix(1_000_000, 0)
	col := &strokeCollector{}
	f := NewStrokeFilter(p, touchMap{tc: h.touchConfig(), pw: h.Screen[0], ph: h.Screen[1]}, col)
	var x, y, pr int32
	down, pressed := false, false
	for _, ev := range rec.Events {
		t := base.Add(ev.T)
		if d := f.Due(); !d.IsZero() && !t.Before(d) {
			f.Flush()
		}
		switch {
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_X:
			x = ev.Value
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_Y:
			y = ev.Value
		case ev.Type == evdev.EV_ABS && ev.Code == evdev.ABS_PRESSURE:
			pr = ev.Value
		case ev.Type == evdev.EV_KEY && ev.Code == evdev.BTN_TOUCH:
			down = ev.Value == 1
			if !down && pressed {
				f.Up(t)
				pressed = false
			}
		case ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT && down:
			if !pressed {
				pressed = true
				f.Down(t)
			}
			f.Sample(x, y, pr, t)
		}
	}
	f.Flush()
	return f.Stats(), col.strokes
}

// runReplayPen は、-replay-pen の結果を書く。pngOut があれば、最後のファイルの線を描く。
func runReplayPen(w io.Writer, files []string, params, pngOut string) error {
	if len(files) == 0 {
		return fmt.Errorf("-replay-pen: give one or more .touch files")
	}
	p := defaultPen
	if params != "" {
		var r penReplayParams
		dec := json.NewDecoder(strings.NewReader(params))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("-replay-params: %w", err)
		}
		r.apply(&p)
	}
	fmt.Fprintf(w, "params: min_pressure %d, settle %v, smooth %d, hold_back %d, join %v within %.0f dots\n",
		p.MinPressure, p.Settle, p.Smooth, p.HoldBack, p.Join, p.JoinDist)
	var last []ink.Stroke
	var lastRec *TouchRecording
	for _, file := range files {
		rec, err := LoadRecording(file)
		if err != nil {
			return err
		}
		st, strokes := replayPen(rec, p)
		fmt.Fprintf(w, "%s (%s): touches %d, strokes %d (joined %d), points %d, dropped samples %d, ignored touches %d, longest step %.1f dots, length %.0f dots\n",
			file, rec.Header.Gesture, st.Touches, st.Strokes, st.Joined, st.Points, st.Dropped, st.Ignored, st.MaxStep, st.Length)
		last, lastRec = strokes, rec
	}
	if pngOut == "" {
		return nil
	}
	pg := &ink.Page{W: lastRec.Header.Screen[0], H: lastRec.Header.Screen[1], Strokes: last}
	return writeInkPNG(pngOut, pg)
}

// writeInkPNG は、ページを PNG にする。
func writeInkPNG(path string, pg *ink.Page) error {
	rgb := ink.Render(pg)
	img := image.NewRGBA(image.Rect(0, 0, pg.W, pg.H))
	for i := 0; i < pg.W*pg.H; i++ {
		img.Set(i%pg.W, i/pg.W, color.RGBA{rgb[i*3], rgb[i*3+1], rgb[i*3+2], 255})
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
