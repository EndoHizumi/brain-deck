package main

import (
	"image"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lefthand/internal/ink"
)

// 作った記録：跳ねを捨て、線はなぞった所にのり、短く離れても同じ線になる
func TestStrokeFilterSynth(t *testing.T) {
	s := newSynth()
	s.stroke(fpt{100, 200}, fpt{300, 200}, 800*time.Millisecond)
	s.wait(60 * time.Millisecond) // 一瞬離れる
	s.stroke(fpt{300, 200}, fpt{400, 260}, 400*time.Millisecond)
	s.wait(time.Second)
	s.tap(fpt{500, 100}, 30*time.Millisecond) // 点
	st, strokes := replayPen(&s.rec, defaultPen)
	if st.Strokes != 2 || st.Joined != 1 || len(strokes) != 2 {
		t.Fatalf("stats %+v, %d strokes", st, len(strokes))
	}
	for _, p := range strokes[0].Points() {
		if p.Y < 190 || p.Y > 270 || p.X < 90 || p.X > 410 {
			t.Errorf("point %v is off the line (a jump was used)", p)
		}
	}
	if st.MaxStep > defaultPen.JoinDist {
		t.Errorf("longest step %.1f", st.MaxStep)
	}
	if pts := strokes[1].Points(); len(pts) != 1 || math.Hypot(float64(pts[0].X-500), float64(pts[0].Y-100)) > 6 {
		t.Errorf("the tap is %v, want a dot at 500,100", pts)
	}
	// join を 0 にすると、離れたところで線が分かれる
	p := defaultPen
	p.Join = 0
	if st, _ := replayPen(&s.rec, p); st.Strokes != 3 {
		t.Errorf("without join: %d strokes", st.Strokes)
	}
	// 弱い押し方は捨てる
	p = defaultPen
	p.MinPressure = 700
	if st, _ := replayPen(&s.rec, p); st.Strokes != 0 || st.Ignored == 0 {
		t.Errorf("min_pressure 700: %+v", st)
	}
}

// 実機（指）の記録：タップは点になり、なぞった線は 1 本ずつ。値を変えたら結果を見直す（README の「ペンの記録を取る」）
func TestStrokeFilterRecordings(t *testing.T) {
	for _, c := range []struct {
		file             string
		strokes, maxStep float64
	}{
		{"tap.touch", 8, 5},
		{"doubletap.touch", 11, 1},
		{"slow.touch", 3, 8},
		{"fast.touch", 10, 40},
	} {
		rec, err := LoadRecording(filepath.Join("testdata", "touch", c.file))
		if err != nil {
			t.Fatal(err)
		}
		st, _ := replayPen(rec, defaultPen)
		if float64(st.Strokes) != c.strokes || st.MaxStep > c.maxStep {
			t.Errorf("%s: %d strokes, longest step %.1f (want %v and at most %v)", c.file, st.Strokes, st.MaxStep, c.strokes, c.maxStep)
		}
	}
}

func TestPenParamsConfig(t *testing.T) {
	_, km := compileText(t, `
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 2
      rows: 1
      cells:
        "0,0": { widget: pentablet, map: region, pc_screen: 1920x1080, region: [0, 0, 960, 1080], pressure_range: [300, 2000], min_pressure: 350, smooth: 2 }
        "1,0": { widget: memo, settle_ms: 40 }
`)
	pt := km.Layers[0].Grid.Cells[cellPos{0, 0}].Widget.PenTab
	if pt.Map != penMapRegion || pt.Pen.PressureLo != 300 || pt.Pen.MinPressure != 350 || pt.Pen.Smooth != 2 {
		t.Errorf("pentablet %+v", pt)
	}
	if m := km.Layers[0].Grid.Cells[cellPos{1, 0}].Widget.Memo; m.Pen.Settle != 40*time.Millisecond {
		t.Errorf("memo %+v", m)
	}
	for src, want := range map[string]string{
		`{ widget: pentablet, map: aspect }`:                                               "needs pc_screen",
		`{ widget: pentablet, map: region, pc_screen: 100x100 }`:                           "needs region",
		`{ widget: pentablet, map: region, pc_screen: 100x100, region: [50, 50, 80, 80] }`: "inside pc_screen",
		`{ widget: pentablet, speed: 2 }`:                                                  "for widget: trackpad",
		`{ widget: pentablet, key: A }`:                                                    "handles the stylus",
		`{ widget: memo, map: full }`:                                                      "memo takes",
		`{ widget: clock, pressure: true }`:                                                "for widget: pentablet",
		`{ widget: pentablet, pressure_range: [900, 100] }`:                                "pressure_range",
	} {
		cfg, err := parseConfig([]byte(`
touch: { min_x: 0, max_x: 800, min_y: 0, max_y: 480 }
layers:
  - name: base
    touch:
      cols: 1
      rows: 1
      cells:
        "0,0": ` + src + "\n"))
		if err == nil {
			_, _, err = compileKeymap(cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", src, err, want)
		}
	}
}

func TestInkStrokeBoundsHelper(t *testing.T) {
	s := ink.Stroke{W: 3}
	s.Add(ink.Pt{X: 1, Y: 1})
	if s.Bounds() != image.Rect(0, 0, 3, 3) {
		t.Errorf("bounds %v", s.Bounds())
	}
}
