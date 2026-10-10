package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"testing"
)

func render(t *testing.T, body string, w, h int) *image.NRGBA {
	t.Helper()
	src := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="` + strconv.Itoa(w) + `" height="` + strconv.Itoa(h) + `">` + body + `</svg>`
	img, err := RenderSVG([]byte(src), w, h)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func at(img *image.NRGBA, x, y int) color.NRGBA { return img.NRGBAAt(x, y) }

func near(a, b uint8, d int) bool { return int(a)-int(b) <= d && int(b)-int(a) <= d }

func TestSVGNumbers(t *testing.T) {
	got := svgNumbers("1.5.5-2e1,3 -.25e-1 4.")
	want := []float64{1.5, 0.5, -20, 3, -0.025, 4}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Errorf("%d: %v", i, got[i])
		}
	}
	m := parseTransform("translate(10 20) scale(2) rotate(90)")
	if x, y := m.apply(1, 0); math.Abs(x-10) > 1e-9 || math.Abs(y-22) > 1e-9 {
		t.Errorf("transform: %v %v", x, y)
	}
}

func TestSVGFillRules(t *testing.T) {
	// 外側 0〜20、内側 5〜15 の正方形（同じ向き）。非ゼロなら穴なし、偶奇なら穴
	d := `M0 0h20v20H0zM5 5h10v10H5z`
	nz := render(t, `<path d="`+d+`" fill="#ff0000"/>`, 20, 20)
	eo := render(t, `<path d="`+d+`" fill="#ff0000" fill-rule="evenodd"/>`, 20, 20)
	if at(nz, 10, 10).A != 255 || at(eo, 10, 10).A != 0 || at(eo, 2, 2).A != 255 {
		t.Errorf("nonzero %v, evenodd %v %v", at(nz, 10, 10), at(eo, 10, 10), at(eo, 2, 2))
	}
	// 半分だけ覆う画素（x = 0.5 の縦の辺）
	half := render(t, `<rect x="0.5" y="0" width="4" height="4" fill="#00ff00"/>`, 6, 4)
	if a := at(half, 0, 1).A; !near(a, 128, 2) {
		t.Errorf("half covered alpha %d", a)
	}
}

func TestSVGArcsAndShapes(t *testing.T) {
	// 円（直径 16）の面積は、覆う割合の合計とほぼ同じ
	img := render(t, `<circle cx="10" cy="10" r="8" fill="#000"/>`, 20, 20)
	sum := 0.0
	for i := 3; i < len(img.Pix); i += 4 {
		sum += float64(img.Pix[i]) / 255
	}
	if math.Abs(sum-math.Pi*64) > 1.5 {
		t.Errorf("circle area %.2f, want %.2f", sum, math.Pi*64)
	}
	// 円弧のフラグを続けて書く書き方（a8 8 0 1010 0）でも同じ形
	arc := render(t, `<path d="M2 10a8 8 0 1016 0a8 8 0 10-16 0z" fill="#000"/>`, 20, 20)
	diff := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if !near(img.Pix[i], arc.Pix[i], 8) {
			diff++
		}
	}
	if diff > 0 {
		t.Errorf("arc and circle differ at %d pixels", diff)
	}
}

func TestSVGStroke(t *testing.T) {
	// 太さ 4 の横線（y=10）：y=8〜12 を塗る
	img := render(t, `<path d="M2 10H18" stroke="#0000ff" stroke-width="4" fill="none"/>`, 20, 20)
	if at(img, 10, 9).A != 255 || at(img, 10, 11).A != 255 || at(img, 10, 7).A != 0 || at(img, 10, 13).A != 0 || at(img, 1, 10).A != 0 {
		t.Errorf("stroke: %v %v %v %v", at(img, 10, 9), at(img, 10, 7), at(img, 10, 13), at(img, 1, 10))
	}
	// 丸い端は、線の端の外にも出る
	rc := render(t, `<path d="M4 10H16" stroke="#000" stroke-width="4" stroke-linecap="round"/>`, 20, 20)
	if at(rc, 3, 10).A != 255 || at(rc, 2, 10).A < 150 || at(rc, 1, 10).A != 0 {
		t.Errorf("round cap: %v %v %v", at(rc, 3, 10), at(rc, 2, 10), at(rc, 1, 10))
	}
	// 変形の拡大は、線の太さにも当たる
	sc := render(t, `<g transform="scale(2)"><path d="M1 5H9" stroke="#000" stroke-width="2"/></g>`, 20, 20)
	if at(sc, 10, 8).A != 255 || at(sc, 10, 12).A != 0 {
		t.Errorf("scaled stroke: %v %v", at(sc, 10, 8), at(sc, 10, 12))
	}
	// paint-order：stroke を先に描けば、塗りが線の内側の半分を隠す
	po := render(t, `<rect x="5" y="5" width="10" height="10" fill="#ff0000" stroke="#0000ff" stroke-width="4" paint-order="stroke"/>`, 20, 20)
	if c := at(po, 6, 10); c.R != 255 || c.B != 0 {
		t.Errorf("paint-order inner edge %v", c)
	}
}

func TestSVGGradients(t *testing.T) {
	// href で stop を受け継ぐ。objectBoundingBox の既定は左から右
	img := render(t, `<defs><linearGradient id="s"><stop offset="0" stop-color="#000"/><stop offset="1" stop-color="#fff"/></linearGradient>
<linearGradient id="g" xlink:href="#s"/></defs><rect width="100" height="10" fill="url(#g)"/>`, 100, 10)
	l, m, r := at(img, 0, 5), at(img, 50, 5), at(img, 99, 5)
	if l.R > 5 || !near(m.R, 128, 3) || r.R < 250 {
		t.Errorf("gradient %v %v %v", l, m, r)
	}
	// userSpaceOnUse と gradientTransform、stop-opacity
	img = render(t, `<defs><linearGradient id="g" gradientUnits="userSpaceOnUse" x1="0" y1="0" x2="0" y2="10" gradientTransform="scale(1 2)">
<stop offset="0" stop-color="red"/><stop offset="1" stop-color="red" stop-opacity="0"/></linearGradient></defs><rect width="10" height="20" fill="url(#g)"/>`, 10, 20)
	// 画素の中心 y = 10.5 は、グラデーションの座標では 10.5 / 2 = 5.25（0〜10 の 52.5%）
	if a := at(img, 5, 10).A; !near(a, 121, 1) {
		t.Errorf("transformed gradient alpha %d", a)
	}
	if c := at(img, 5, 1); c.R != 255 || c.G != 0 {
		t.Errorf("gradient color %v", c)
	}
}

func TestSVGOpacityAndStyle(t *testing.T) {
	img := render(t, `<g opacity="0.5"><rect width="10" height="10" fill="#fff"/><rect width="10" height="10" fill="#fff"/></g>`, 10, 10)
	if a := at(img, 5, 5).A; !near(a, 128, 1) {
		t.Errorf("group opacity applies once: %d", a)
	}
	img = render(t, `<rect width="10" height="10" style="fill:#00ff00;fill-opacity:.5" fill="#f00"/>`, 10, 10)
	if c := at(img, 5, 5); c.G != 255 || c.R != 0 || !near(c.A, 128, 1) {
		t.Errorf("style wins: %v", c)
	}
	img = render(t, `<g color="#0000ff"><rect width="10" height="10" fill="currentColor"/></g><rect width="5" height="5" display="none"/>`, 10, 10)
	if c := at(img, 2, 2); c.B != 255 || c.R != 0 {
		t.Errorf("currentColor / display none: %v", c)
	}
}

func TestSVGFilters(t *testing.T) {
	// ぼかしは全体の量を保ち、範囲の外へ広がる
	img := render(t, `<defs><filter id="b" x="-1" y="-1" width="3" height="3"><feGaussianBlur stdDeviation="2"/></filter></defs>
<rect x="10" y="10" width="10" height="10" fill="#fff" filter="url(#b)"/>`, 30, 30)
	sum := 0.0
	for i := 3; i < len(img.Pix); i += 4 {
		sum += float64(img.Pix[i]) / 255
	}
	if math.Abs(sum-100) > 2 || at(img, 9, 15).A == 0 || at(img, 15, 15).A == 255 && at(img, 10, 15).A == 255 {
		t.Errorf("blur: sum %.1f, edge %v", sum, at(img, 9, 15))
	}
	// 既定の範囲（-10%〜120%）の外には出ない
	img = render(t, `<defs><filter id="b"><feGaussianBlur stdDeviation="3"/></filter></defs>
<rect x="10" y="10" width="10" height="10" fill="#fff" filter="url(#b)"/>`, 30, 30)
	if at(img, 7, 15).A != 0 || at(img, 9, 15).A == 0 {
		t.Errorf("filter region: %v %v", at(img, 7, 15), at(img, 9, 15))
	}
	// 色の行列（赤と青を入れ替える）
	img = render(t, `<defs><filter id="m" color-interpolation-filters="sRGB"><feColorMatrix values="0 0 1 0 0  0 1 0 0 0  1 0 0 0 0  0 0 0 1 0"/></filter></defs>
<rect width="10" height="10" fill="#ff0000" filter="url(#m)"/>`, 10, 10)
	if c := at(img, 5, 5); c.B != 255 || c.R != 0 {
		t.Errorf("color matrix: %v", c)
	}
	// ガンマ（2·c² − 0.7）と、in で切り抜く合成、arithmetic、screen
	img = render(t, `<defs><filter id="f" color-interpolation-filters="sRGB">
<feComponentTransfer result="g"><feFuncR type="gamma" amplitude="2" exponent="2" offset="-.7"/></feComponentTransfer>
<feComposite in="g" in2="SourceGraphic" operator="in"/></filter></defs><rect width="10" height="10" fill="#cc0000" filter="url(#f)"/>`, 10, 10)
	want := uint8(math.Round((2*0.8*0.8 - 0.7) * 255))
	if c := at(img, 5, 5); !near(c.R, want, 2) || c.A != 255 {
		t.Errorf("gamma: %v, want R %d", c, want)
	}
	img = render(t, `<defs><filter id="f" color-interpolation-filters="sRGB"><feFlood flood-color="#808080" result="a"/>
<feComposite in="a" in2="SourceGraphic" operator="arithmetic" k1=".5" k3=".5"/></filter></defs><rect width="10" height="10" fill="#fff" filter="url(#f)"/>`, 10, 10)
	// 0.5·(0.502·1) + 0.5·1 = 0.751
	if c := at(img, 5, 5); !near(c.R, 191, 2) || !near(c.A, 255, 1) {
		t.Errorf("arithmetic: %v", c)
	}
	// 畳み込み：真ん中だけ 1 の行列は、そのまま
	img = render(t, `<defs><filter id="c" color-interpolation-filters="sRGB"><feConvolveMatrix order="3" kernelMatrix="0 0 0 0 1 0 0 0 0"/></filter></defs>
<rect x="2" y="2" width="6" height="6" fill="#00ff00" filter="url(#c)"/>`, 10, 10)
	if c := at(img, 5, 5); c.G != 255 || c.A != 255 {
		t.Errorf("convolve identity: %v", c)
	}
	img = render(t, `<defs><filter id="s" color-interpolation-filters="sRGB"><feFlood flood-color="#000080" result="a"/>
<feBlend in="SourceGraphic" in2="a" mode="screen"/></filter></defs><rect width="10" height="10" fill="#800000" filter="url(#s)"/>`, 10, 10)
	if c := at(img, 5, 5); !near(c.R, 128, 2) || !near(c.B, 128, 2) {
		t.Errorf("screen: %v", c)
	}
}

func TestSVGImage(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	src.SetNRGBA(1, 0, color.NRGBA{0, 0, 255, 255})
	var b bytes.Buffer
	png.Encode(&b, src)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
	img := render(t, `<image x="0" y="0" width="20" height="10" preserveAspectRatio="none" xlink:href="`+uri+`"/>`, 20, 10)
	if l, r := at(img, 1, 5), at(img, 18, 5); l.R < 250 || r.B < 250 || at(img, 10, 5).A != 255 {
		t.Errorf("image: %v %v", l, r)
	}
	// 読めない画像、知らない要素は無視する
	img = render(t, `<image width="10" height="10" xlink:href="data:image/png;base64,AAAA"/><text>x</text><use xlink:href="#a"/>`, 10, 10)
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0 {
			t.Fatalf("drew something")
		}
	}
}

func TestSVGViewBoxAndErrors(t *testing.T) {
	src := `<svg viewBox="0 0 10 10" xmlns="http://www.w3.org/2000/svg"><rect x="0" y="0" width="5" height="10" fill="#fff"/></svg>`
	img, err := RenderSVG([]byte(src), 40, 20) // xMidYMid meet：20×20 で中央に置く
	if err != nil {
		t.Fatal(err)
	}
	if at(img, 9, 10).A != 0 || at(img, 11, 10).A != 255 || at(img, 21, 10).A != 0 {
		t.Errorf("viewBox: %v %v %v", at(img, 9, 10), at(img, 11, 10), at(img, 21, 10))
	}
	if w, h, err := SVGAspect([]byte(src)); err != nil || w != 10 || h != 10 {
		t.Errorf("aspect %v %v %v", w, h, err)
	}
	for _, bad := range []string{"", "<html/>", "<svg"} {
		if _, err := RenderSVG([]byte(bad), 10, 10); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, err := RenderSVG([]byte(src), 0, 10); err == nil {
		t.Errorf("size 0 was accepted")
	}
}
