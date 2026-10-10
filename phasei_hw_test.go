package main

import (
	"image"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// フェーズ I の実機のテスト（読み物：開く時間、ページを数える時間、ページめくりの描き直しの時間）。本番のサービスを止めてから実行する。
// テストの実行ファイルと同じディレクトリに reader-example.yaml（config/reader-example.yaml の写し）と、
// read-doc.json.gz（brain-deck read add <青空文庫の作品> -o read-doc.json.gz で作ったもの）を置く。
// 読み物はテストの一時ディレクトリに置く（本番のデータには触れない）。
//
//	sudo LEFTHAND_HW_TEST=1 timeout 300 ./lefthand.test -test.run HWReader -test.v
func TestHWReader(t *testing.T) {
	if os.Getenv("LEFTHAND_HW_TEST") == "" {
		t.Skip("set LEFTHAND_HW_TEST=1 on the device")
	}
	hwProcs(t)
	raw, err := os.ReadFile("reader-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, km := compileText(t, string(raw))
	doc, err := os.ReadFile("read-doc.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "reader")
	os.MkdirAll(dir, 0o755)
	const id = "00000000000000aa"
	os.WriteFile(filepath.Join(dir, id+readExt), doc, 0o644)
	st := OpenReadStore(dir)
	t0 := time.Now()
	d, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	total, _ := docChars(d, ReadPos{})
	t.Logf("open %q (%d KB gzip, %d blocks, %d chars): %v", d.Title, len(doc)>>10, len(d.Blocks), total, time.Since(t0))
	st.idx[id] = readMeta{Title: d.Title, Author: d.Author, Chars: total}
	for _, s := range []int{24, 32} {
		t0 = time.Now()
		readFontOf(s)
		t.Logf("font %d: unpacked in %v", s, time.Since(t0))
	}
	for _, c := range []struct {
		area     string
		r        image_R
		size     int
		vertical bool
	}{{"portrait", image_R{0, 0, 456, 750}, 24, true}, {"portrait", image_R{0, 0, 456, 750}, 32, true}, {"landscape", image_R{0, 0, 776, 430}, 24, true}, {"landscape", image_R{0, 0, 776, 430}, 24, false}} {
		m := readMetricsFor(c.r.rect(), c.size, c.vertical, false)
		t0 = time.Now()
		starts := paginate(d, m)
		t.Logf("paginate %s %d vertical=%v: %d pages in %v", c.area, c.size, c.vertical, len(starts), time.Since(t0))
	}

	env := func() WidgetEnv { return WidgetEnv{Now: time.Now(), TimeSynced: true, Battery: &BatteryInfo{Percent: 80, Source: "test"}, Link: "configured"} }
	rs := NewReaderService(st)
	e := NewEngine(km, &State{hid: NewHIDWriter(os.DevNull), active: map[string]Combo{}})
	rt := NewWidgetRT(nil)
	rt.reader = rs
	e.SetWidgets(rt)
	full := func() WidgetEnv { v := env(); v.Reader = rs.Snapshot(); return v }
	rt.SetEnv(full)
	for _, rot := range []int{90, 0} {
		e.SetRotation(rot, 800, 480, nil)
		e.mu.Lock()
		e.stack = nil
		for i, ly := range km.Layers {
			if ly.Name == "read" {
				e.stack = []stackEntry{{layer: i, kind: actTo}}
			}
		}
		e.refresh()
		e.mu.Unlock()
		l := buildLayout(km, e.View())
		dd, err := StartDisplay(cfg.Display, l, full, nil)
		if err != nil {
			t.Fatal(err)
		}
		rt.SetScreen(l.W, l.H, dd.Poke)
		rs.SetOnChange(dd.Poke)
		rs.mu.Lock()
		rs.openLocked(id)
		rs.mu.Unlock()
		for i := 0; i < 2000 && (rs.Snapshot().pages == 0); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		v := rs.Snapshot()
		t.Logf("rotate %d: %d pages", rot, v.pages)
		// ページめくり：キーを押してから、描き直したセルがフレームバッファに写るまで
		var turns []time.Duration
		for i := 0; i < 12; i++ {
			k0 := widgetKeyOf(dd)
			t0 := time.Now()
			rs.mu.Lock()
			rs.turnLocked(true)
			rs.changedLocked()
			rs.mu.Unlock()
			for widgetKeyOf(dd) == k0 && time.Since(t0) < 5*time.Second {
				time.Sleep(time.Millisecond)
			}
			turns = append(turns, time.Since(t0))
		}
		sort.Slice(turns, func(i, j int) bool { return turns[i] < turns[j] })
		t.Logf("rotate %d: page turn %v (median) %v (min) %v (max)", rot, turns[len(turns)/2], turns[0], turns[len(turns)-1])
		hwSameAsFull(t, dd, full, "reader-"+map[int]string{0: "landscape", 90: "portrait"}[rot])
		dd.Close()
	}
}

type image_R struct{ x0, y0, x1, y1 int }

func (r image_R) rect() image.Rectangle { return image.Rect(r.x0, r.y0, r.x1, r.y1) }

// widgetKeyOf は、描いた最初のウィジェットのセルの中身（描き直したかを見る）。
func widgetKeyOf(d *Display) string {
	d.drawMu.Lock()
	defer d.drawMu.Unlock()
	for _, k := range d.wkeys {
		if k != "" {
			return k
		}
	}
	return ""
}
