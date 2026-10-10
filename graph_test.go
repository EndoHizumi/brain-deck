package main

import (
	"encoding/json"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fp(v float64) *float64 { return &v }

func graphVals(kv ...any) []GraphValue {
	var out []GraphValue
	for i := 0; i < len(kv); i += 2 {
		out = append(out, GraphValue{Name: kv[i].(string), Value: fp(kv[i+1].(float64))})
	}
	return out
}

func TestGraphAddKeepsNewest(t *testing.T) {
	g := NewGraphService(OpenStore(t.TempDir()))
	now := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)
	for i := 0; i < graphMaxPoints+30; i++ {
		if err := g.Add(graphVals("cpu", float64(i)), now.Add(time.Duration(i)*time.Second), true, "test"); err != nil {
			t.Fatal(err)
		}
	}
	s := g.Snapshot()["cpu"]
	if len(s.Points) != graphMaxPoints || s.Points[0].V != 30 || s.Points[len(s.Points)-1].V != float64(graphMaxPoints+29) {
		t.Fatalf("points %d, first %v, last %v", len(s.Points), s.Points[0].V, s.Points[len(s.Points)-1].V)
	}
	if s.Rev != graphMaxPoints+30 || s.Source != "test" {
		t.Fatalf("rev %d source %q", s.Rev, s.Source)
	}
}

func TestGraphAddRejectsBadValuesAtomically(t *testing.T) {
	g := NewGraphService(OpenStore(t.TempDir()))
	now := time.Now()
	for _, vals := range [][]GraphValue{
		nil,
		{{Name: "cpu"}},
		graphVals("bad id!", 1.0),
		append(graphVals("cpu", 1.0), GraphValue{Name: "mem", Value: fp(math.NaN())}),
		graphVals("cpu", math.Inf(1)),
		graphVals("cpu", 2e12),
	} {
		if err := g.Add(vals, now, true, ""); err == nil {
			t.Errorf("%v: no error", vals)
		}
	}
	if len(g.Snapshot()) != 0 {
		t.Fatalf("something was added: %v", g.Snapshot())
	}
	many := make([]GraphValue, graphMaxBatch+1)
	for i := range many {
		many[i] = GraphValue{Name: "a", Value: fp(1)}
	}
	if err := g.Add(many, now, true, ""); err == nil {
		t.Error("too many values in one call were accepted")
	}
}

func TestGraphEvictsOldestID(t *testing.T) {
	g := NewGraphService(OpenStore(t.TempDir()))
	now := time.Now()
	for i := 0; i <= graphMaxIDs; i++ {
		g.Add(graphVals("id"+string(rune('a'+i%26))+string(rune('a'+i/26)), 1.0), now.Add(time.Duration(i)*time.Second), true, "")
	}
	m := g.Snapshot()
	if len(m) != graphMaxIDs {
		t.Fatalf("%d ids", len(m))
	}
	if _, ok := m["idaa"]; ok {
		t.Fatal("the oldest id was kept")
	}
}

func TestGraphClear(t *testing.T) {
	g := NewGraphService(OpenStore(t.TempDir()))
	g.Add(graphVals("cpu", 1.0), time.Now(), true, "")
	if !g.Clear("cpu") || g.Clear("cpu") {
		t.Fatal("Clear should report whether something was removed")
	}
}

// 保存は間引く：前に書いてから every のあいだは書かず、たまった値を 1 回で書く。終わるときは Flush で書く。
func TestGraphSaveIsThinned(t *testing.T) {
	dir := t.TempDir()
	st := OpenStore(dir)
	g := NewGraphService(st)
	g.every, g.delay = 400*time.Millisecond, 50*time.Millisecond
	g.lastSave = time.Now().Add(-time.Hour)
	g.saved = make(chan struct{}, 10)
	path := filepath.Join(dir, graphStoreName+".json")
	t0 := time.Now()
	g.Add(graphVals("cpu", 1.0), time.Now(), true, "")
	g.Add(graphVals("cpu", 2.0), time.Now(), true, "")
	<-g.saved // delay のあと、1 回目
	if d := time.Since(t0); d < 40*time.Millisecond {
		t.Fatalf("saved after %v (want the delay)", d)
	}
	if n := savedPoints(t, path, "cpu"); n != 2 {
		t.Fatalf("first save has %d points, want 2", n)
	}
	t1 := time.Now()
	for i := 0; i < 5; i++ {
		g.Add(graphVals("cpu", float64(i)), time.Now(), true, "")
	}
	select {
	case <-g.saved:
		t.Fatalf("saved again after %v (want every)", time.Since(t1))
	case <-time.After(250 * time.Millisecond):
	}
	<-g.saved
	if d := time.Since(t1); d < 300*time.Millisecond {
		t.Fatalf("second save after %v", d)
	}
	if n := savedPoints(t, path, "cpu"); n != 7 {
		t.Fatalf("second save has %d points, want 7", n)
	}
	select {
	case <-g.saved:
		t.Fatal("an extra save")
	case <-time.After(100 * time.Millisecond):
	}
	// 終わるときは、間引きの途中でもすぐに書く
	g.Add(graphVals("cpu", 9.0), time.Now(), true, "")
	g.Flush()
	<-g.saved
	if n := savedPoints(t, path, "cpu"); n != 8 {
		t.Fatalf("flush has %d points, want 8", n)
	}
	// 読み直す
	g2 := NewGraphService(st)
	if s := g2.Snapshot()["cpu"]; len(s.Points) != 8 || s.Points[7].V != 9 {
		t.Fatalf("reloaded %v", s.Points)
	}
}

func savedPoints(t *testing.T, path, id string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f graphFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return len(f.Series[id].Points)
}

func TestGraphLoadSkipsBadEntries(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "graph.json"), []byte(`{"series":{"cpu":{"points":[{"t":"2026-10-10T00:00:00Z","v":1},{"t":"0001-01-01T00:00:00Z","v":2}]},"bad id":{"points":[]}}}`), 0o644)
	g := NewGraphService(OpenStore(dir))
	m := g.Snapshot()
	if len(m) != 1 || len(m["cpu"].Points) != 1 {
		t.Fatalf("%v", m)
	}
}

func TestGraphClockStepped(t *testing.T) {
	g := NewGraphService(OpenStore(t.TempDir()))
	t0 := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	g.Add(graphVals("cpu", 1.0), t0, false, "")
	g.Add(graphVals("cpu", 2.0), t0.Add(time.Hour), true, "")
	g.ClockStepped(48 * time.Hour)
	pts := g.Snapshot()["cpu"].Points
	if !pts[0].T.Equal(t0.Add(48*time.Hour)) || pts[0].U || !pts[1].T.Equal(t0.Add(time.Hour)) {
		t.Fatalf("%v", pts)
	}
	g.Add(graphVals("cpu", 3.0), t0, false, "")
	g.ClockSynced()
	if p := g.Snapshot()["cpu"].Points[2]; p.U || !p.T.Equal(t0) {
		t.Fatalf("%v", p)
	}
}

func TestCompileGraph(t *testing.T) {
	ok := []ActionSpec{
		{Widget: widgetGraph, ID: "cpu"},
		{Widget: widgetGraph, ID: "cpu", Unit: "%", Min: fp(0), Max: fp(100), Chart: "bar", Points: ip(120), Decimals: ip(0), Stale: "2m"},
		{Widget: widgetGraph, ID: "t", Unit: "℃", Key: "F5"},
	}
	for _, s := range ok {
		if _, err := compileWidget(s); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := map[string]ActionSpec{
		"needs id":           {Widget: widgetGraph},
		"chart must be":      {Widget: widgetGraph, ID: "a", Chart: "pie"},
		"min (5) must be":    {Widget: widgetGraph, ID: "a", Min: fp(5), Max: fp(5)},
		"points must be":     {Widget: widgetGraph, ID: "a", Points: ip(1)},
		"decimals must be":   {Widget: widgetGraph, ID: "a", Decimals: ip(7)},
		"unit must be":       {Widget: widgetGraph, ID: "a", Unit: "123456789"},
		"stale must be":      {Widget: widgetGraph, ID: "a", Stale: "1s"},
		"are for widget: gr": {Widget: widgetClock, Unit: "%"},
		"max must be":        {Widget: widgetGraph, ID: "a", Max: fp(math.Inf(1))},
	}
	for want, s := range bad {
		if _, err := compileWidget(s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v (want %q)", s, err, want)
		}
	}
	w, _ := compileWidget(ActionSpec{Widget: widgetGraph, ID: "cpu"})
	if w.Graph.Points != defaultGraphPoints || w.Graph.Chart != graphChartLine || w.Graph.Decimals != -1 || w.Stale != defaultGraphStale {
		t.Fatalf("defaults %+v %v", w.Graph, w.Stale)
	}
}

func TestFormatGraph(t *testing.T) {
	for _, c := range []struct {
		v    float64
		fix  int
		want string
	}{
		{27.6, -1, "27.6"}, {27.0, -1, "27"}, {1.125, -1, "1.13"}, {0.125, -1, "0.13"}, {-0.004, -1, "0"},
		{-12.34, -1, "-12.3"}, {123.5, -1, "124"}, {99.96, -1, "100"}, {1e12, -1, "1000000000000"},
		{3, 2, "3.00"}, {2.5, 0, "3"}, {-2.5, 0, "-3"}, {0.000001, 6, "0.000001"},
	} {
		d, trim := graphDecimals(c.v, c.fix)
		if got := formatGraph(c.v, d, trim); got != c.want {
			t.Errorf("formatGraph(%v, %d) = %q, want %q", c.v, c.fix, got, c.want)
		}
	}
}

func TestGraphRange(t *testing.T) {
	pts := func(vs ...float64) []GraphPoint {
		var out []GraphPoint
		for _, v := range vs {
			out = append(out, GraphPoint{V: v})
		}
		return out
	}
	for _, c := range []struct {
		g      GraphDef
		vs     []float64
		lo, hi float64
	}{
		{GraphDef{Chart: "line"}, []float64{3, 7, 5}, 3, 7},
		{GraphDef{Chart: "bar"}, []float64{3, 7, 5}, 0, 7},
		{GraphDef{Chart: "bar"}, []float64{-3, -7}, -7, 0},
		{GraphDef{Chart: "line"}, []float64{5, 5}, 4, 6},
		{GraphDef{Chart: "line", Min: fp(0)}, []float64{0, 0}, 0, 1},
		{GraphDef{Chart: "line", Max: fp(10)}, []float64{20}, 9, 10},
		{GraphDef{Chart: "line", Min: fp(0), Max: fp(100)}, []float64{200}, 0, 100},
	} {
		lo, hi := graphRange(&c.g, pts(c.vs...))
		if lo != c.lo || hi != c.hi {
			t.Errorf("%+v %v: %v..%v, want %v..%v", c.g, c.vs, lo, hi, c.lo, c.hi)
		}
	}
}

func TestGraphLayout(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	w, _ := compileWidget(ActionSpec{Widget: widgetGraph, ID: "cpu", Min: fp(0), Max: fp(100), Points: ip(5)})
	env := WidgetEnv{Now: now, Graph: map[string]GraphSeries{}}
	if l := graphLayout(image.Rect(0, 0, 180, 130), w, env); !l.None {
		t.Fatal("no values should be None")
	}
	for i := 0; i < 7; i++ { // points: 5 なので、新しい 5 個（0、25、50、75、100）を描く
		s := env.Graph["cpu"]
		s.Points = append(s.Points, GraphPoint{T: now.Add(time.Duration(i-7) * time.Second), V: float64(i-2) * 25})
		s.Rev++
		env.Graph["cpu"] = s
	}
	area := image.Rect(10, 20, 190, 150)
	l := graphLayout(area, w, env)
	if l.Value != "100" || l.Stale || l.Plot.Empty() {
		t.Fatalf("%+v", l)
	}
	if len(l.XS) != 5 || l.XS[0] != l.Plot.Min.X || l.XS[4] != l.Plot.Max.X-graphLineW {
		t.Fatalf("xs %v plot %v", l.XS, l.Plot)
	}
	if l.YS[0] != l.Plot.Max.Y-graphLineW || l.YS[4] != l.Plot.Min.Y {
		t.Fatalf("ys %v plot %v", l.YS, l.Plot)
	}
	if l.HiLabel != "100" || l.LoLabel != "0" {
		t.Fatalf("labels %q %q", l.HiLabel, l.LoLabel)
	}
	// 横長のセル：値は左、図は右
	wl := graphLayout(image.Rect(0, 0, 380, 100), w, env)
	if wl.ValueBox.Max.X > wl.Plot.Min.X || wl.Plot.Min.Y != 0 {
		t.Fatalf("wide %+v", wl)
	}
	// 低すぎるセル：値だけ
	if s := graphLayout(image.Rect(0, 0, 50, 30), w, env); !s.Plot.Empty() || s.ValueBox != image.Rect(0, 0, 50, 30) {
		t.Fatalf("short %+v", s)
	}
	// 古い値：薄く、「古い」と時刻
	env.Now = now.Add(10 * time.Minute)
	if o := graphLayout(area, w, env); !o.Stale || o.Sub != "古い 11:59" {
		t.Fatalf("stale %+v", o)
	}
	env.Now = now.Add(30 * time.Hour)
	if o := graphLayout(area, w, env); o.Sub != "古い 10/10 11:59" {
		t.Fatalf("stale another day %q", o.Sub)
	}
}

func TestGraphKeyAndNext(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	w, _ := compileWidget(ActionSpec{Widget: widgetGraph, ID: "cpu", Stale: "2m"})
	env := WidgetEnv{Now: now, Graph: map[string]GraphSeries{"cpu": {Points: []GraphPoint{{T: now, V: 1}}, Rev: 1}}}
	v := &CellView{Widget: w}
	k := widgetKey(v, env)
	if n := widgetNext(v, env); !n.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("next %v", n)
	}
	env.Now = now.Add(2 * time.Minute)
	if widgetKey(v, env) == k {
		t.Fatal("the key does not change when the value gets old")
	}
	if !widgetNext(v, env).IsZero() {
		t.Fatal("no more changes after stale")
	}
}

func TestGraphSegment(t *testing.T) {
	var got []image.Point
	graphSegment(0, 0, 3, 1, func(x, y int) { got = append(got, image.Pt(x, y)) })
	if len(got) != 3 || got[2] != image.Pt(3, 1) {
		t.Fatalf("%v", got)
	}
}

func TestControlGraph(t *testing.T) {
	d := newTestDaemon(t, `
touch: { min_x: 0, max_x: 4095, min_y: 0, max_y: 4095 }
layers:
  - name: base
    touch:
      cols: 2
      rows: 1
      cells:
        "0,0": { widget: graph, id: cpu, unit: "%" }
        "1,0": { widget: graph, id: temp }
`)
	hello := result(t, d.call("hello", nil))
	for _, c := range []string{"add_graph", "clear_graph", "get_graph"} {
		if !strings.Contains(fmt.Sprint(hello["commands"]), c) {
			t.Errorf("hello does not list %s", c)
		}
	}
	r := result(t, d.call("add_graph", map[string]any{"points": []map[string]any{{"name": "cpu", "value": 12.5}, {"name": "gpu", "value": 3}}, "source": "test"}))
	if r["added"] != 2.0 || fmt.Sprint(r["unknown"]) != "[gpu]" {
		t.Fatalf("add_graph = %v", r)
	}
	g := result(t, d.call("get_graph", nil))
	cpu := g["series"].(map[string]any)["cpu"].(map[string]any)
	if fmt.Sprint(g["ids"]) != "[cpu temp]" || cpu["count"] != 1.0 || cpu["last"].(map[string]any)["v"] != 12.5 || cpu["source"] != "test" {
		t.Fatalf("get_graph = %v", g)
	}
	for _, p := range []map[string]any{{}, {"points": []map[string]any{{"name": "cpu"}}},
		{"points": []map[string]any{{"name": "", "value": 1}}}} {
		if m := d.call("add_graph", p); errCode(m) != errBadRequest {
			t.Errorf("add_graph %v = %v", p, m)
		}
	}
	if r := result(t, d.call("clear_graph", map[string]any{"name": "cpu"})); r["cleared"] != true {
		t.Fatalf("clear_graph = %v", r)
	}
	if m := d.call("clear_graph", map[string]any{}); errCode(m) != errBadRequest {
		t.Fatalf("clear_graph without name = %v", m)
	}
}
