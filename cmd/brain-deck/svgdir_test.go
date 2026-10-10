package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderSVGDir は、BRAIN_DECK_SVG_DIR の *.svg を描いて、同じ場所に <名前>.svg.go.png を書く。
// ブラウザで描いたものと手で見比べるための道具で、ふつうは飛ばす（気象庁のアイコンはリポジトリに入れない）。
// 大きさは BRAIN_DECK_SVG_SIZE（既定 240x180）。
func TestRenderSVGDir(t *testing.T) {
	dir := os.Getenv("BRAIN_DECK_SVG_DIR")
	if dir == "" {
		t.Skip("BRAIN_DECK_SVG_DIR が空")
	}
	w, h := 240, 180
	if s := os.Getenv("BRAIN_DECK_SVG_SIZE"); s != "" {
		if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil {
			t.Fatalf("BRAIN_DECK_SVG_SIZE %q: %v", s, err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.svg"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		img, err := RenderSVG(b, w, h)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		o, err := os.Create(f + ".go.png")
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(o, img)
		o.Close()
	}
}

// TestMakeIconsDir は、BRAIN_DECK_SVG_DIR の <番号>.svg から、BRAIN_DECK_ICON_BOXES（"87x58,183x122" など）の箱の
// アイコン（<番号>-<幅>x<高さ>.lha）を BRAIN_DECK_ICON_OUT に書く（lefthand -render-png -render-weather-icons で見るため。ふつうは飛ばす）。
func TestMakeIconsDir(t *testing.T) {
	dir, out, boxes := os.Getenv("BRAIN_DECK_SVG_DIR"), os.Getenv("BRAIN_DECK_ICON_OUT"), os.Getenv("BRAIN_DECK_ICON_BOXES")
	if dir == "" || out == "" || boxes == "" {
		t.Skip("BRAIN_DECK_SVG_DIR、BRAIN_DECK_ICON_OUT、BRAIN_DECK_ICON_BOXES が空")
	}
	os.MkdirAll(out, 0o755)
	files, _ := filepath.Glob(filepath.Join(dir, "[0-9][0-9][0-9].svg"))
	for _, f := range files {
		svg, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		code := filepath.Base(f)[:3]
		for _, bs := range strings.Split(boxes, ",") {
			var w, h int
			if _, err := fmt.Sscanf(bs, "%dx%d", &w, &h); err != nil {
				t.Fatalf("box %q: %v", bs, err)
			}
			data, _, err := makeIcon(svg, image.Pt(w, h))
			if err != nil {
				t.Fatalf("%s %s: %v", f, bs, err)
			}
			if err := os.WriteFile(filepath.Join(out, fmt.Sprintf("%s-%dx%d.lha", code, w, h)), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
