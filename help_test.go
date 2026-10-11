package main

import (
	"image"
	"strings"
	"testing"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- ヘルプのアプリ（フェーズ K1） ----------

// ヘルプ：章は新しいページから、目次は章と節、{version} を版に置き換える。めくる。
func TestHelpDoc(t *testing.T) {
	d := helpDoc("# A\n\n本文{version}です。\n続き。\n\n## B\n\n・一\n・二\n\n終わり。\n\n# C\n\nx\n", "v1")
	var kinds []string
	for _, b := range d.Blocks {
		kinds = append(kinds, b.T+":"+blockText(&b))
	}
	got := strings.Join(kinds, "|")
	want := "h:■ A|p:本文v1です。続き。|h:B|p:・一|p:・二|p:|p:終わり。|pb:|h:■ C|p:x"
	if got != want {
		t.Errorf("blocks\n got %s\nwant %s", got, want)
	}
	if err := d.validate(); err != nil {
		t.Error(err)
	}
	full := helpDoc(helpText, "test")
	if err := full.validate(); err != nil {
		t.Fatal(err)
	}
	toc := tocEntries(full)
	if len(toc) < 20 {
		t.Errorf("toc has %d entries", len(toc))
	}
	for _, need := range []string{"基本の操作", "アプリの使い方", "PC から外したとき", "困ったとき", "設定", "ふた"} {
		found := false
		for _, i := range toc {
			if strings.Contains(blockText(&full.Blocks[i]), need) {
				found = true
			}
		}
		if !found {
			t.Errorf("no heading %q", need)
		}
	}
	if strings.Contains(helpText, "◀") || strings.Contains(helpText, "▶") {
		t.Error("◀ ▶ are not in the reader font")
	}
	// めくる、目次、キー
	h := NewHelpUI(func() int { return 24 }, nil)
	area := image.Rect(0, 0, 800, 480).Inset(cellGap + textMargin)
	v := renderHelpView(area, 1, 24)
	if v.pages < 10 {
		t.Fatalf("pages = %d", v.pages)
	}
	h.mu.Lock()
	h.starts, h.mkey, h.m = v.ui.starts, v.ui.mkey, v.ui.m
	h.mu.Unlock()
	cell := image.Rect(0, 0, 800, 480)
	h.touch(cell, image.Pt(790, 200)) // 右：次
	if h.Snapshot().page != 2 {
		t.Errorf("page = %d", h.Snapshot().page)
	}
	h.key(evdev.KEY_LEFT, false)
	if h.Snapshot().page != 1 {
		t.Errorf("after ← page = %d", h.Snapshot().page)
	}
	h.touch(cell, image.Pt(400, 200)) // 真ん中：道具
	if h.Snapshot().mode != readMenu {
		t.Fatal("menu")
	}
	bs := menuGeometry(area)
	h.touch(cell, bs[2].Min.Add(image.Pt(5, 5))) // 困ったとき
	if s := helpSection(full, h.Snapshot().pos); !strings.Contains(s, "困ったとき") && !strings.Contains(s, "電池") {
		t.Errorf("trouble section: %q", s)
	}
	// 道具の箱は範囲の真ん中に置く（前は左上に寄っていた）
	box := bs[len(bs)-1]
	if box.Min.X <= area.Min.X || box.Min.Y <= area.Min.Y || box.Max.X >= area.Max.X {
		t.Errorf("menu box = %v in %v", box, area)
	}
}

func TestHelpWidget(t *testing.T) {
	if _, err := compileWidget(mustSpec(t, `{widget: help}`)); err != nil {
		t.Error(err)
	}
	if _, err := compileWidget(mustSpec(t, `{widget: help, key: A}`)); err == nil {
		t.Error("help with key accepted")
	}
}
