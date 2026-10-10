package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"time"
)

// ---------- グラフ（widget: graph）の値 ----------
//
// 値は PC から add_graph で 1 つずつ足す（brain-deck graph <id> <値>）。Brain は id ごとに新しい graphMaxPoints 個を覚え、
// /var/lib/lefthand/graph.json に残す。設定 GUI で設定を保存しても消えない。
//
// 値は数秒〜数十秒ごとに送られることがある。SD カードは書き込みが遅く、書き換えの回数にも限りがあるので、
// 値を受け取るたびには書かない（保存の方針）：
//   - 前に書いてから graphSaveEvery（5 分）たつまでは書かない。そのあいだに来た値は、次の 1 回にまとめて書く。
//   - しばらく値が来ていなかった（前に書いてから 5 分以上たった）ときは、graphSaveDelay（10 秒）待ってから書く
//     （続けて来る値を 1 回にまとめる）。
//   - デーモンが終わるとき（systemctl stop、brain-deck restart、update）は、まだ書いていない値を書いてから終わる。
//   - 消す（clear_graph）ときも同じ間隔で書く。
// そのため、電源が急に切れたときは、最大 5 分ぶんの値がなくなる。返事は保存を待たない。

const (
	graphStoreName  = "graph"
	graphMaxPoints  = 120 // id ごとに覚える値の数
	graphMaxIDs     = 32  // 覚える id の数。超えたら、いちばん前に値を足したものを捨てる
	graphMaxAbs     = 1e12
	graphMaxBatch   = 64 // 1 回の add_graph で足せる値の数
	graphSaveEvery  = 5 * time.Minute
	graphSaveDelay  = 10 * time.Second
	graphUnitRunes  = 8
	graphMaxDecimal = 6
)

// GraphPoint は値 1 つ。
type GraphPoint struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
	// U は、Brain の時刻を合わせる前に受け取ったこと。時刻を合わせたときに、そのずれの分だけ T を動かす
	U bool `json:"u,omitempty"`
}

// GraphSeries は id 1 つの値の並び（古い順）。Points は書き換えない（足すときは新しいスライスを作る）ので、
// 写しはスライスをそのまま渡してよい。
type GraphSeries struct {
	Points []GraphPoint `json:"points"`
	Rev    uint64       `json:"rev"` // 値を足すたびに増える（描き直しの判断）
	Source string       `json:"source,omitempty"`
}

// Last は、いちばん新しい値。なければ false。
func (s GraphSeries) Last() (GraphPoint, bool) {
	if len(s.Points) == 0 {
		return GraphPoint{}, false
	}
	return s.Points[len(s.Points)-1], true
}

// graphFile は graph.json の形。
type graphFile struct {
	Series map[string]GraphSeries `json:"series"`
}

// GraphValue は add_graph の値 1 つ。
type GraphValue struct {
	Name  string   `json:"name"`
	Value *float64 `json:"value"`
}

var errBadGraph = errors.New("bad graph value")

// GraphService はグラフの値を持ち、間引いて保存する。
type GraphService struct {
	mu       sync.Mutex
	m        map[string]GraphSeries
	store    *Store
	onChange func() // 値が変わったとき（画面の描き直し）。待たずに返ること
	timer    *time.Timer
	dirty    bool
	lastSave time.Time
	saveMu   sync.Mutex // 書き込みを重ねない
	every    time.Duration
	delay    time.Duration
	saved    chan struct{} // テスト用：保存を 1 回終えるたびに送る（nil なら送らない）
}

// NewGraphService は graph.json を読む。
func NewGraphService(store *Store) *GraphService {
	g := &GraphService{m: map[string]GraphSeries{}, store: store, every: graphSaveEvery, delay: graphSaveDelay}
	var f graphFile
	if err := store.Load(graphStoreName, &f); err == nil {
		for id, s := range f.Series {
			if !validTextID(id) {
				continue
			}
			pts := make([]GraphPoint, 0, min(len(s.Points), graphMaxPoints))
			for _, p := range s.Points {
				if validGraphValue(p.V) && !p.T.IsZero() {
					pts = append(pts, p)
				}
			}
			if len(pts) > graphMaxPoints {
				pts = pts[len(pts)-graphMaxPoints:]
			}
			s.Points = pts
			g.m[id] = s
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("graph: %v", err)
	}
	// 起動してから最初の値は、10 秒後に書く（5 分は待たない）
	g.lastSave = time.Now().Add(-g.every)
	return g
}

func validGraphValue(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= graphMaxAbs
}

// SetOnChange は、値が変わったときに呼ぶ関数を設定する。
func (g *GraphService) SetOnChange(f func()) {
	g.mu.Lock()
	g.onChange = f
	g.mu.Unlock()
}

// Snapshot は、今の値の写し（描画の goroutine が描き直すたびに使う）。
func (g *GraphService) Snapshot() map[string]GraphSeries {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]GraphSeries, len(g.m))
	for k, v := range g.m {
		out[k] = v
	}
	return out
}

// Add は値を足す。どれか 1 つでも誤りがあれば、何も足さない。clockSet は、Brain の時刻を合わせてあるか。
func (g *GraphService) Add(vals []GraphValue, now time.Time, clockSet bool, source string) error {
	if len(vals) == 0 || len(vals) > graphMaxBatch {
		return fmt.Errorf("%w: give 1..%d values", errBadGraph, graphMaxBatch)
	}
	for i, v := range vals {
		if !validTextID(v.Name) {
			return fmt.Errorf("%w: points[%d]: name %q must be 1-32 characters of A-Z a-z 0-9 _ . -", errBadGraph, i, v.Name)
		}
		if v.Value == nil {
			return fmt.Errorf("%w: points[%d]: value (a number) is required", errBadGraph, i)
		}
		if !validGraphValue(*v.Value) {
			return fmt.Errorf("%w: points[%d]: value must be a finite number from -1e12 to 1e12", errBadGraph, i)
		}
	}
	t := now.UTC()
	g.mu.Lock()
	for _, v := range vals {
		s := g.m[v.Name]
		n := len(s.Points) + 1
		pts := make([]GraphPoint, 0, min(n, graphMaxPoints))
		if n > graphMaxPoints {
			pts = append(pts, s.Points[n-graphMaxPoints:]...)
		} else {
			pts = append(pts, s.Points...)
		}
		pts = append(pts, GraphPoint{T: t, V: *v.Value, U: !clockSet})
		s.Points = pts
		s.Rev++
		s.Source = source
		g.m[v.Name] = s
	}
	g.evictLocked()
	g.mu.Unlock()
	g.changed()
	return nil
}

// Clear は id の値をすべて消す。消すものがあったかを返す。
func (g *GraphService) Clear(name string) bool {
	g.mu.Lock()
	_, had := g.m[name]
	delete(g.m, name)
	g.mu.Unlock()
	if had {
		g.changed()
	}
	return had
}

func (g *GraphService) evictLocked() {
	for len(g.m) > graphMaxIDs {
		oldest := ""
		var at time.Time
		for id, s := range g.m {
			p, _ := s.Last()
			if oldest == "" || p.T.Before(at) {
				oldest, at = id, p.T
			}
		}
		log.Printf("graph: more than %d ids, dropping %q", graphMaxIDs, oldest)
		delete(g.m, oldest)
	}
}

// ClockStepped は、時刻合わせでシステムの時刻が off だけ動いたときに呼ぶ。合わせる前に受け取った値の時刻を動かす。
func (g *GraphService) ClockStepped(off time.Duration) { g.fixClock(off) }

// ClockSynced は、時刻を合わせた（ずれが小さく、動かさなかった）ときに呼ぶ。
func (g *GraphService) ClockSynced() { g.fixClock(0) }

func (g *GraphService) fixClock(off time.Duration) {
	if g == nil {
		return
	}
	n := 0
	g.mu.Lock()
	for id, s := range g.m {
		var pts []GraphPoint
		for i, p := range s.Points {
			if !p.U {
				continue
			}
			if pts == nil {
				pts = append([]GraphPoint(nil), s.Points...)
			}
			pts[i].T = p.T.Add(off)
			pts[i].U = false
			n++
		}
		if pts != nil {
			s.Points = pts
			s.Rev++
			g.m[id] = s
		}
	}
	g.mu.Unlock()
	if n > 0 {
		if off != 0 {
			log.Printf("graph: shifted %d values received before the clock was set by %v", n, off.Round(time.Millisecond))
		}
		g.changed()
	}
}

// changed は描き直させ、保存を頼む（間引く）。
func (g *GraphService) changed() {
	g.mu.Lock()
	g.dirty = true
	if g.timer == nil {
		wait := max(g.delay, time.Until(g.lastSave.Add(g.every)))
		g.timer = time.AfterFunc(wait, g.save)
	}
	f := g.onChange
	g.mu.Unlock()
	if f != nil {
		f()
	}
}

// save は、まだ書いていない値を書く。
func (g *GraphService) save() {
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	g.mu.Lock()
	g.timer = nil
	if !g.dirty {
		g.mu.Unlock()
		return
	}
	g.dirty = false
	g.lastSave = time.Now()
	f := graphFile{Series: make(map[string]GraphSeries, len(g.m))}
	for k, v := range g.m {
		f.Series[k] = v
	}
	g.mu.Unlock()
	t := time.Now()
	if err := g.store.Save(graphStoreName, f); err != nil {
		log.Printf("graph: save: %v", err)
	} else if d := time.Since(t); d > time.Second {
		log.Printf("graph: saving %s took %v", graphStoreName, d.Round(time.Millisecond))
	}
	if g.saved != nil {
		g.saved <- struct{}{}
	}
}

// Flush は、まだ書いていない値をすぐに書く（デーモンが終わるとき）。
func (g *GraphService) Flush() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	g.mu.Unlock()
	g.save()
}

// GraphInfo は get_graph で返す、id 1 つの様子。
type GraphInfo struct {
	Points []GraphPoint `json:"points"`
	Count  int          `json:"count"`
	Last   *GraphPoint  `json:"last,omitempty"`
	Source string       `json:"source,omitempty"`
}

// List は、すべての id の値を返す。
func (g *GraphService) List() map[string]GraphInfo {
	out := map[string]GraphInfo{}
	for id, s := range g.Snapshot() {
		in := GraphInfo{Points: s.Points, Count: len(s.Points), Source: s.Source}
		if p, ok := s.Last(); ok {
			in.Last = &p
		}
		if in.Points == nil {
			in.Points = []GraphPoint{}
		}
		out[id] = in
	}
	return out
}

// graphIDs は、設定のどこかでグラフのセルに使われている id を、名前の順に返す。
func graphIDs(cfg *Config) []string {
	ids := gadgetIDs(cfg, widgetGraph)
	sort.Strings(ids)
	return ids
}
