package main

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ---------- SVG を描く（天気のアイコン用） ----------
//
// 気象庁の天気アイコン（SVG）を、Brain に送る大きさの画像にする。PC の brain-deck だけが使い、デーモンには入れない。
// 使える Go のライブラリ（oksvg、tdewolff/canvas など）は、アイコンが使うグラデーションの参照（xlink:href）、
// 偶奇規則、フィルター（ぼかし、色の行列、畳み込み、合成）を描けなかったので、アイコンが使う範囲を標準ライブラリだけで描く。
//
// 描けるもの：
//   - 要素：svg（viewBox、preserveAspectRatio）、g、path、rect、circle、ellipse、line、polyline、polygon、image（data: の PNG）、
//     linearGradient、radialGradient、stop、filter と fe*（feGaussianBlur、feColorMatrix、feComponentTransfer、
//     feComposite、feBlend、feConvolveMatrix、feFlood、feOffset、feMerge）
//   - 属性と style：transform、fill、stroke、stroke-width、stroke-linejoin、stroke-linecap、stroke-miterlimit、
//     stroke-dasharray、stroke-dashoffset、fill-rule、opacity、fill-opacity、stroke-opacity、color（currentColor）、
//     paint-order、display、visibility、filter
//
// 描けないもの（あれば無視する）：text、use、clipPath、mask、pattern、marker、CSS の <style>。
// 塗りは 1 画素を縦に svgSub 本の線で区切り、横は線ごとに正確な長さで覆う割合を求める（アンチエイリアス）。

const svgSub = 16 // 1 画素あたりの、縦の走査線の数

// svgNode は XML の要素 1 つ。
type svgNode struct {
	name     string
	attrs    map[string]string // style の中身も入れる（style が優先）
	children []*svgNode
	data     string // 中の文字（使わない）
}

// parseSVGTree は XML を読む。
func parseSVGTree(b []byte) (*svgNode, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity
	var root *svgNode
	var stack []*svgNode
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("SVG として読めません（%v）", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &svgNode{name: t.Name.Local, attrs: map[string]string{}}
			var style string
			for _, a := range t.Attr {
				switch {
				case a.Name.Local == "style" && a.Name.Space == "":
					style = a.Value
				case a.Name.Local == "href":
					n.attrs["href"] = strings.TrimSpace(a.Value)
				case a.Name.Space == "" || a.Name.Space == "http://www.w3.org/2000/svg":
					n.attrs[a.Name.Local] = strings.TrimSpace(a.Value)
				}
			}
			for _, d := range strings.Split(style, ";") {
				k, v, ok := strings.Cut(d, ":")
				if ok {
					n.attrs[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if root == nil || root.name != "svg" {
		return nil, errors.New("SVG の svg 要素がありません")
	}
	return root, nil
}

// ---------- 数と行列 ----------

// svgMatrix は [a c e; b d f; 0 0 1]。
type svgMatrix struct{ a, b, c, d, e, f float64 }

var svgIdentity = svgMatrix{1, 0, 0, 1, 0, 0}

// mul は m・n（n を先に当てる）。
func (m svgMatrix) mul(n svgMatrix) svgMatrix {
	return svgMatrix{
		m.a*n.a + m.c*n.b, m.b*n.a + m.d*n.b,
		m.a*n.c + m.c*n.d, m.b*n.c + m.d*n.d,
		m.a*n.e + m.c*n.f + m.e, m.b*n.e + m.d*n.f + m.f,
	}
}

func (m svgMatrix) apply(x, y float64) (float64, float64) {
	return m.a*x + m.c*y + m.e, m.b*x + m.d*y + m.f
}

func (m svgMatrix) inverse() (svgMatrix, bool) {
	det := m.a*m.d - m.b*m.c
	if math.Abs(det) < 1e-12 {
		return svgMatrix{}, false
	}
	return svgMatrix{m.d / det, -m.b / det, -m.c / det, m.a / det, (m.c*m.f - m.d*m.e) / det, (m.b*m.e - m.a*m.f) / det}, true
}

// scales は、x と y の向きの拡大率。
func (m svgMatrix) scales() (float64, float64) {
	return math.Hypot(m.a, m.b), math.Hypot(m.c, m.d)
}

// svgNumbers は、数の並び（空白かカンマで区切る。"1.5.5" や "1-2" も読む）を読む。
func svgNumbers(s string) []float64 {
	var out []float64
	p := &svgNumScanner{s: s}
	for {
		v, ok := p.number()
		if !ok {
			return out
		}
		out = append(out, v)
	}
}

type svgNumScanner struct {
	s string
	i int
}

func (p *svgNumScanner) skipSep() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == ',' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

// number は次の数を読む。数がなければ false。
func (p *svgNumScanner) number() (float64, bool) {
	p.skipSep()
	st := p.i
	i := p.i
	if i < len(p.s) && (p.s[i] == '+' || p.s[i] == '-') {
		i++
	}
	digits, dot := 0, false
	for i < len(p.s) {
		c := p.s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		default:
			goto exp
		}
		i++
	}
exp:
	if digits == 0 {
		return 0, false
	}
	if i < len(p.s) && (p.s[i] == 'e' || p.s[i] == 'E') {
		j := i + 1
		if j < len(p.s) && (p.s[j] == '+' || p.s[j] == '-') {
			j++
		}
		if j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
			for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
				j++
			}
			i = j
		}
	}
	v, err := strconv.ParseFloat(p.s[st:i], 64)
	if err != nil {
		return 0, false
	}
	p.i = i
	return v, true
}

// flag は円弧の 0 か 1 を読む（"011" のように続けて書かれることがある）。
func (p *svgNumScanner) flag() (bool, bool) {
	p.skipSep()
	if p.i < len(p.s) && (p.s[p.i] == '0' || p.s[p.i] == '1') {
		v := p.s[p.i] == '1'
		p.i++
		return v, true
	}
	return false, false
}

// parseTransform は transform の属性を読む。
func parseTransform(s string) svgMatrix {
	m := svgIdentity
	for {
		s = strings.TrimLeft(s, " ,\t\n\r")
		open := strings.IndexByte(s, '(')
		end := strings.IndexByte(s, ')')
		if open < 0 || end < open {
			return m
		}
		name := strings.TrimSpace(s[:open])
		v := svgNumbers(s[open+1 : end])
		s = s[end+1:]
		var t svgMatrix
		switch name {
		case "matrix":
			if len(v) != 6 {
				continue
			}
			t = svgMatrix{v[0], v[1], v[2], v[3], v[4], v[5]}
		case "translate":
			if len(v) == 0 {
				continue
			}
			ty := 0.0
			if len(v) > 1 {
				ty = v[1]
			}
			t = svgMatrix{1, 0, 0, 1, v[0], ty}
		case "scale":
			if len(v) == 0 {
				continue
			}
			sy := v[0]
			if len(v) > 1 {
				sy = v[1]
			}
			t = svgMatrix{v[0], 0, 0, sy, 0, 0}
		case "rotate":
			if len(v) == 0 {
				continue
			}
			r := v[0] * math.Pi / 180
			cs, sn := math.Cos(r), math.Sin(r)
			t = svgMatrix{cs, sn, -sn, cs, 0, 0}
			if len(v) == 3 {
				t = svgMatrix{1, 0, 0, 1, v[1], v[2]}.mul(t).mul(svgMatrix{1, 0, 0, 1, -v[1], -v[2]})
			}
		case "skewX":
			if len(v) == 0 {
				continue
			}
			t = svgMatrix{1, 0, math.Tan(v[0] * math.Pi / 180), 1, 0, 0}
		case "skewY":
			if len(v) == 0 {
				continue
			}
			t = svgMatrix{1, math.Tan(v[0] * math.Pi / 180), 0, 1, 0, 0}
		default:
			continue
		}
		m = m.mul(t)
	}
}

// svgLength は長さを読む。% は ref に対する割合。単位（px など）は無視する。
func svgLength(s string, ref, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if v, ok := strings.CutSuffix(s, "%"); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return def
		}
		return f / 100 * ref
	}
	s = strings.TrimRight(s, "abcdefghijklmnopqrstuvwxyz")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}

// svgFraction は、0〜1 の数か % を読む（offset、opacity）。
func svgFraction(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v := svgLength(s, 1, def)
	return math.Max(0, math.Min(1, v))
}

// ---------- 色 ----------

// svgColor は、透明でない色（0〜1）。
type svgColor struct{ r, g, b float64 }

var svgNamedColors = map[string]svgColor{
	"black": {0, 0, 0}, "white": {1, 1, 1}, "red": {1, 0, 0}, "lime": {0, 1, 0}, "green": {0, 128.0 / 255, 0},
	"blue": {0, 0, 1}, "yellow": {1, 1, 0}, "cyan": {0, 1, 1}, "aqua": {0, 1, 1}, "magenta": {1, 0, 1}, "fuchsia": {1, 0, 1},
	"gray": {128.0 / 255, 128.0 / 255, 128.0 / 255}, "grey": {128.0 / 255, 128.0 / 255, 128.0 / 255},
	"silver": {192.0 / 255, 192.0 / 255, 192.0 / 255}, "maroon": {128.0 / 255, 0, 0}, "navy": {0, 0, 128.0 / 255},
	"orange": {1, 165.0 / 255, 0}, "purple": {128.0 / 255, 0, 128.0 / 255}, "teal": {0, 128.0 / 255, 128.0 / 255},
	"olive": {128.0 / 255, 128.0 / 255, 0},
}

// parseSVGColor は色を読む。currentColor は cur。
func parseSVGColor(s string, cur svgColor) (svgColor, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "currentcolor":
		return cur, true
	case strings.HasPrefix(s, "#"):
		h := s[1:]
		if len(h) == 3 || len(h) == 4 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		if len(h) == 8 {
			h = h[:6]
		}
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil || len(h) != 6 {
			return svgColor{}, false
		}
		return svgColor{float64(v>>16&255) / 255, float64(v>>8&255) / 255, float64(v&255) / 255}, true
	case strings.HasPrefix(s, "rgb(") && strings.HasSuffix(s, ")"):
		parts := strings.Split(s[4:len(s)-1], ",")
		if len(parts) != 3 {
			return svgColor{}, false
		}
		var c [3]float64
		for i, p := range parts {
			c[i] = math.Max(0, math.Min(1, svgLength(p, 1, 0)/func() float64 {
				if strings.HasSuffix(strings.TrimSpace(p), "%") {
					return 1
				}
				return 255
			}()))
		}
		return svgColor{c[0], c[1], c[2]}, true
	}
	c, ok := svgNamedColors[s]
	return c, ok
}

// ---------- 道筋 ----------

type svgPt struct{ x, y float64 }

// svgPath は、線分にした道筋。座標は画素（変形を当てたあと）。
type svgPath struct {
	subs   [][]svgPt
	closed []bool
}

// pathBuilder は道筋のコマンドを受けて、変形を当ててから線分にする。
type pathBuilder struct {
	m      svgMatrix
	tol    float64 // 曲線を線分にするときの許容の誤差（画素）
	out    svgPath
	cur    []svgPt
	bbox   [4]float64 // 変形を当てる前の座標の範囲（objectBoundingBox 用）
	hasBox bool
}

func (pb *pathBuilder) track(x, y float64) {
	if !pb.hasBox {
		pb.bbox = [4]float64{x, y, x, y}
		pb.hasBox = true
		return
	}
	pb.bbox[0], pb.bbox[1] = math.Min(pb.bbox[0], x), math.Min(pb.bbox[1], y)
	pb.bbox[2], pb.bbox[3] = math.Max(pb.bbox[2], x), math.Max(pb.bbox[3], y)
}

func (pb *pathBuilder) dev(x, y float64) svgPt {
	X, Y := pb.m.apply(x, y)
	return svgPt{X, Y}
}

func (pb *pathBuilder) moveTo(x, y float64) {
	pb.flush(false)
	pb.track(x, y)
	pb.cur = []svgPt{pb.dev(x, y)}
}

func (pb *pathBuilder) lineTo(x, y float64) {
	if pb.cur == nil {
		pb.moveTo(x, y)
		return
	}
	pb.track(x, y)
	pb.cur = append(pb.cur, pb.dev(x, y))
}

// cubicTo は、3 次のベジェ曲線を、画素での許容の誤差で線分にする。
func (pb *pathBuilder) cubicTo(x0, y0, x1, y1, x2, y2, x3, y3 float64) {
	if pb.cur == nil {
		pb.moveTo(x0, y0)
	}
	// 曲線の範囲は、制御点まで含めれば必ず入る（少し広めの bbox になる。ブラウザは正確な範囲を使う）
	pb.trackCubic(x0, y0, x1, y1, x2, y2, x3, y3)
	p0, p1, p2, p3 := pb.dev(x0, y0), pb.dev(x1, y1), pb.dev(x2, y2), pb.dev(x3, y3)
	d := math.Hypot(p1.x-p0.x, p1.y-p0.y) + math.Hypot(p2.x-p1.x, p2.y-p1.y) + math.Hypot(p3.x-p2.x, p3.y-p2.y)
	n := int(math.Ceil(math.Sqrt(d / pb.tol)))
	n = max(1, min(n, 200))
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		u := 1 - t
		a, b, c, e := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		pb.cur = append(pb.cur, svgPt{a*p0.x + b*p1.x + c*p2.x + e*p3.x, a*p0.y + b*p1.y + c*p2.y + e*p3.y})
	}
}

// trackCubic は、曲線の上の点で範囲を広げる（極値を求める代わりに、細かく点を取る）。
func (pb *pathBuilder) trackCubic(x0, y0, x1, y1, x2, y2, x3, y3 float64) {
	for i := 0; i <= 16; i++ {
		t := float64(i) / 16
		u := 1 - t
		a, b, c, e := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		pb.track(a*x0+b*x1+c*x2+e*x3, a*y0+b*y1+c*y2+e*y3)
	}
}

func (pb *pathBuilder) close() {
	pb.flush(true)
}

func (pb *pathBuilder) flush(closed bool) {
	if len(pb.cur) > 0 {
		pb.out.subs = append(pb.out.subs, pb.cur)
		pb.out.closed = append(pb.out.closed, closed)
	}
	pb.cur = nil
}

// arcTo は、SVG の円弧（端点の書き方）を 3 次のベジェ曲線に分けて足す。
func (pb *pathBuilder) arcTo(x1, y1, rx, ry, phiDeg float64, large, sweep bool, x2, y2 float64) {
	if x1 == x2 && y1 == y2 {
		return
	}
	rx, ry = math.Abs(rx), math.Abs(ry)
	if rx == 0 || ry == 0 {
		pb.lineTo(x2, y2)
		return
	}
	phi := phiDeg * math.Pi / 180
	cs, sn := math.Cos(phi), math.Sin(phi)
	dx, dy := (x1-x2)/2, (y1-y2)/2
	x1p, y1p := cs*dx+sn*dy, -sn*dx+cs*dy
	lam := x1p*x1p/(rx*rx) + y1p*y1p/(ry*ry)
	if lam > 1 {
		s := math.Sqrt(lam)
		rx, ry = rx*s, ry*s
	}
	num := rx*rx*ry*ry - rx*rx*y1p*y1p - ry*ry*x1p*x1p
	den := rx*rx*y1p*y1p + ry*ry*x1p*x1p
	co := 0.0
	if den > 0 && num > 0 {
		co = math.Sqrt(num / den)
	}
	if large == sweep {
		co = -co
	}
	cxp, cyp := co*rx*y1p/ry, -co*ry*x1p/rx
	cx, cy := cs*cxp-sn*cyp+(x1+x2)/2, sn*cxp+cs*cyp+(y1+y2)/2
	ang := func(ux, uy, vx, vy float64) float64 {
		a := math.Atan2(ux*vy-uy*vx, ux*vx+uy*vy)
		return a
	}
	t1 := ang(1, 0, (x1p-cxp)/rx, (y1p-cyp)/ry)
	dt := ang((x1p-cxp)/rx, (y1p-cyp)/ry, (-x1p-cxp)/rx, (-y1p-cyp)/ry)
	if !sweep && dt > 0 {
		dt -= 2 * math.Pi
	} else if sweep && dt < 0 {
		dt += 2 * math.Pi
	}
	n := int(math.Ceil(math.Abs(dt) / (math.Pi / 2)))
	n = max(n, 1)
	step := dt / float64(n)
	k := 4.0 / 3 * math.Tan(step/4)
	pt := func(t float64) (float64, float64) {
		ex, ey := rx*math.Cos(t), ry*math.Sin(t)
		return cs*ex - sn*ey + cx, sn*ex + cs*ey + cy
	}
	der := func(t float64) (float64, float64) {
		ex, ey := -rx*math.Sin(t), ry*math.Cos(t)
		return cs*ex - sn*ey, sn*ex + cs*ey
	}
	t := t1
	px, py := x1, y1
	for i := 0; i < n; i++ {
		t2 := t + step
		qx, qy := pt(t2)
		if i == n-1 {
			qx, qy = x2, y2
		}
		d1x, d1y := der(t)
		d2x, d2y := der(t2)
		pb.cubicTo(px, py, px+k*d1x, py+k*d1y, qx-k*d2x, qy-k*d2y, qx, qy)
		px, py, t = qx, qy, t2
	}
}

// buildPathData は、path の d を読んで pb に足す。
func buildPathData(pb *pathBuilder, d string) {
	p := &svgNumScanner{s: d}
	var cx, cy, sx, sy float64 // 今の点と、部分の始まり
	var lcx, lcy float64       // 前の曲線の 2 つ目の制御点（S、T 用）
	var prev byte              // 前のコマンド
	cmd := byte(0)
	num := func() (float64, bool) { return p.number() }
	for {
		p.skipSep()
		if p.i >= len(p.s) {
			break
		}
		c := p.s[p.i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			cmd = c
			p.i++
			if cmd == 'Z' || cmd == 'z' {
				pb.close()
				cx, cy = sx, sy
				prev = cmd
				continue
			}
		} else if cmd == 0 {
			return
		}
		rel := cmd >= 'a'
		ox, oy := 0.0, 0.0
		if rel {
			ox, oy = cx, cy
		}
		switch cmd | 0x20 {
		case 'm':
			x, ok1 := num()
			y, ok2 := num()
			if !ok1 || !ok2 {
				return
			}
			cx, cy = ox+x, oy+y
			sx, sy = cx, cy
			pb.moveTo(cx, cy)
			// m のあとに続く数は l とみなす
			if rel {
				cmd = 'l'
			} else {
				cmd = 'L'
			}
			prev = 'm'
			continue
		case 'l':
			x, ok1 := num()
			y, ok2 := num()
			if !ok1 || !ok2 {
				return
			}
			cx, cy = ox+x, oy+y
			pb.lineTo(cx, cy)
		case 'h':
			x, ok := num()
			if !ok {
				return
			}
			cx = ox + x
			pb.lineTo(cx, cy)
		case 'v':
			y, ok := num()
			if !ok {
				return
			}
			if rel {
				cy += y
			} else {
				cy = y
			}
			pb.lineTo(cx, cy)
		case 'c':
			var v [6]float64
			for i := range v {
				f, ok := num()
				if !ok {
					return
				}
				v[i] = f
			}
			x1, y1, x2, y2, x, y := ox+v[0], oy+v[1], ox+v[2], oy+v[3], ox+v[4], oy+v[5]
			pb.cubicTo(cx, cy, x1, y1, x2, y2, x, y)
			lcx, lcy, cx, cy = x2, y2, x, y
		case 's':
			var v [4]float64
			for i := range v {
				f, ok := num()
				if !ok {
					return
				}
				v[i] = f
			}
			x1, y1 := cx, cy
			if pc := prev | 0x20; pc == 'c' || pc == 's' {
				x1, y1 = 2*cx-lcx, 2*cy-lcy
			}
			x2, y2, x, y := ox+v[0], oy+v[1], ox+v[2], oy+v[3]
			pb.cubicTo(cx, cy, x1, y1, x2, y2, x, y)
			lcx, lcy, cx, cy = x2, y2, x, y
		case 'q':
			var v [4]float64
			for i := range v {
				f, ok := num()
				if !ok {
					return
				}
				v[i] = f
			}
			qx, qy, x, y := ox+v[0], oy+v[1], ox+v[2], oy+v[3]
			pb.cubicTo(cx, cy, cx+2.0/3*(qx-cx), cy+2.0/3*(qy-cy), x+2.0/3*(qx-x), y+2.0/3*(qy-y), x, y)
			lcx, lcy, cx, cy = qx, qy, x, y
		case 't':
			var v [2]float64
			for i := range v {
				f, ok := num()
				if !ok {
					return
				}
				v[i] = f
			}
			qx, qy := cx, cy
			if pc := prev | 0x20; pc == 'q' || pc == 't' {
				qx, qy = 2*cx-lcx, 2*cy-lcy
			}
			x, y := ox+v[0], oy+v[1]
			pb.cubicTo(cx, cy, cx+2.0/3*(qx-cx), cy+2.0/3*(qy-cy), x+2.0/3*(qx-x), y+2.0/3*(qy-y), x, y)
			lcx, lcy, cx, cy = qx, qy, x, y
		case 'a':
			rx, ok1 := num()
			ry, ok2 := num()
			rot, ok3 := num()
			large, ok4 := p.flag()
			sweep, ok5 := p.flag()
			x, ok6 := num()
			y, ok7 := num()
			if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) {
				return
			}
			x, y = ox+x, oy+y
			if pb.cur == nil {
				pb.moveTo(cx, cy)
			}
			pb.arcTo(cx, cy, rx, ry, rot, large, sweep, x, y)
			cx, cy = x, y
		default:
			return
		}
		prev = cmd
	}
	pb.flush(false)
}

// ---------- 塗りつぶし（覆う割合） ----------

// coverage は、1 画素ごとの覆う割合（0〜1）。範囲は [x0, x0+w) × [y0, y0+h)。
type coverage struct {
	x0, y0, w, h int
	a            []float32
}

type svgEdge struct {
	x0, y0, x1, y1 float64
	dir            int
}

// fillCoverage は、道筋を塗る割合を求める。evenOdd なら偶奇規則、そうでなければ非ゼロ規則。clip は描く範囲（画素）。
func fillCoverage(p svgPath, evenOdd bool, clip image.Rectangle) *coverage {
	var edges []svgEdge
	minY, maxY := math.Inf(1), math.Inf(-1)
	minX, maxX := math.Inf(1), math.Inf(-1)
	for _, sub := range p.subs {
		n := len(sub)
		if n < 2 {
			continue
		}
		for i := 0; i < n; i++ {
			a, b := sub[i], sub[(i+1)%n] // 塗りでは、閉じていない部分も閉じる
			minX, maxX = math.Min(minX, a.x), math.Max(maxX, a.x)
			minY, maxY = math.Min(minY, a.y), math.Max(maxY, a.y)
			if a.y == b.y {
				continue
			}
			e := svgEdge{a.x, a.y, b.x, b.y, 1}
			if a.y > b.y {
				e = svgEdge{b.x, b.y, a.x, a.y, -1}
			}
			edges = append(edges, e)
		}
	}
	if len(edges) == 0 {
		return nil
	}
	r := image.Rect(int(math.Floor(minX)), int(math.Floor(minY)), int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1).Intersect(clip)
	if r.Empty() {
		return nil
	}
	cv := &coverage{x0: r.Min.X, y0: r.Min.Y, w: r.Dx(), h: r.Dy(), a: make([]float32, r.Dx()*r.Dy())}
	sort.Slice(edges, func(i, j int) bool { return edges[i].y0 < edges[j].y0 })
	type cross struct {
		x   float64
		dir int
	}
	var active []int
	next := 0
	xs := make([]cross, 0, 64)
	row := make([]float64, cv.w+2)
	for py := r.Min.Y; py < r.Max.Y; py++ {
		for i := range row {
			row[i] = 0
		}
		for s := 0; s < svgSub; s++ {
			y := float64(py) + (float64(s)+0.5)/svgSub
			for next < len(edges) && edges[next].y0 <= y {
				active = append(active, next)
				next++
			}
			xs = xs[:0]
			k := 0
			for _, ei := range active {
				e := &edges[ei]
				if e.y1 <= y {
					continue // 終わった辺
				}
				active[k] = ei
				k++
				if e.y0 > y {
					continue
				}
				t := (y - e.y0) / (e.y1 - e.y0)
				xs = append(xs, cross{e.x0 + t*(e.x1-e.x0), e.dir})
			}
			active = active[:k]
			if len(xs) < 2 {
				continue
			}
			sort.Slice(xs, func(i, j int) bool { return xs[i].x < xs[j].x })
			wind := 0
			for i := 0; i < len(xs)-1; i++ {
				wind += xs[i].dir
				in := wind != 0
				if evenOdd {
					in = wind%2 != 0
				}
				if !in {
					continue
				}
				addSpan(row, xs[i].x-float64(r.Min.X), xs[i+1].x-float64(r.Min.X), cv.w)
			}
		}
		o := (py - r.Min.Y) * cv.w
		for x := 0; x < cv.w; x++ {
			cv.a[o+x] = float32(math.Min(1, row[x]/svgSub))
		}
	}
	return cv
}

// addSpan は、[a, b) を覆う長さを、画素ごとに row に足す。
func addSpan(row []float64, a, b float64, w int) {
	a, b = math.Max(a, 0), math.Min(b, float64(w))
	if b <= a {
		return
	}
	ia, ib := int(a), int(b)
	if ia == ib {
		row[ia] += b - a
		return
	}
	row[ia] += float64(ia+1) - a
	for i := ia + 1; i < ib && i < w; i++ {
		row[i]++
	}
	if ib < w {
		row[ib] += b - float64(ib)
	}
}

// ---------- 線（太さのある線を、塗る形にする） ----------

type strokeStyle struct {
	width      float64 // 画素
	join       string  // miter、round、bevel
	capStyle   string  // butt、round、square
	miterLimit float64
	dash       []float64 // 画素
	dashOff    float64
}

// strokeToPolys は、線を塗りの形（非ゼロ規則で重ねる多角形の集まり）にする。
// 線分ごとの長方形と、つなぎ目（丸、とがり、面取り）、端（丸、四角）を、すべて同じ向きの多角形で足す。
func strokeToPolys(p svgPath, st strokeStyle) svgPath {
	var out svgPath
	hw := st.width / 2
	if hw <= 0 {
		return out
	}
	add := func(poly []svgPt) {
		if len(poly) < 3 {
			return
		}
		// 向きを、面積が正（時計回り：画面の y は下向き）にそろえる
		area := 0.0
		for i := range poly {
			a, b := poly[i], poly[(i+1)%len(poly)]
			area += a.x*b.y - b.x*a.y
		}
		if area < 0 {
			for i, j := 0, len(poly)-1; i < j; i, j = i+1, j-1 {
				poly[i], poly[j] = poly[j], poly[i]
			}
		}
		out.subs = append(out.subs, poly)
		out.closed = append(out.closed, true)
	}
	circle := func(c svgPt) {
		n := max(8, min(64, int(math.Ceil(hw*2*math.Pi/1.5))))
		poly := make([]svgPt, n)
		for i := range poly {
			t := 2 * math.Pi * float64(i) / float64(n)
			poly[i] = svgPt{c.x + hw*math.Cos(t), c.y + hw*math.Sin(t)}
		}
		add(poly)
	}
	subs, closed := p.subs, p.closed
	if len(st.dash) > 0 {
		subs, closed = dashPath(p, st.dash, st.dashOff)
	}
	for si, sub := range subs {
		pts := dedupPts(sub)
		isClosed := closed[si]
		if isClosed && len(pts) > 1 && pts[0] == pts[len(pts)-1] {
			pts = pts[:len(pts)-1]
		}
		if len(pts) == 1 {
			// 長さ 0 の線は、丸か四角の端だけを描く
			switch st.capStyle {
			case "round":
				circle(pts[0])
			case "square":
				c := pts[0]
				add([]svgPt{{c.x - hw, c.y - hw}, {c.x + hw, c.y - hw}, {c.x + hw, c.y + hw}, {c.x - hw, c.y + hw}})
			}
			continue
		}
		n := len(pts)
		segs := n - 1
		if isClosed {
			segs = n
		}
		for i := 0; i < segs; i++ {
			a, b := pts[i], pts[(i+1)%n]
			dx, dy := b.x-a.x, b.y-a.y
			l := math.Hypot(dx, dy)
			nx, ny := -dy/l*hw, dx/l*hw
			a2, b2 := a, b
			if !isClosed && st.capStyle == "square" {
				ex, ey := dx/l*hw, dy/l*hw
				if i == 0 {
					a2 = svgPt{a.x - ex, a.y - ey}
				}
				if i == segs-1 {
					b2 = svgPt{b.x + ex, b.y + ey}
				}
			}
			add([]svgPt{{a2.x + nx, a2.y + ny}, {b2.x + nx, b2.y + ny}, {b2.x - nx, b2.y - ny}, {a2.x - nx, a2.y - ny}})
		}
		// つなぎ目
		for i := 0; i < n; i++ {
			if !isClosed && (i == 0 || i == n-1) {
				continue
			}
			prev, cur, nxt := pts[(i-1+n)%n], pts[i], pts[(i+1)%n]
			joinPoly(add, circle, prev, cur, nxt, hw, st)
		}
		if !isClosed && st.capStyle == "round" {
			circle(pts[0])
			circle(pts[n-1])
		}
	}
	return out
}

// joinPoly は、prev→cur→next のつなぎ目の外側を埋める形を足す。
func joinPoly(add func([]svgPt), circle func(svgPt), prev, cur, next svgPt, hw float64, st strokeStyle) {
	d1x, d1y := cur.x-prev.x, cur.y-prev.y
	d2x, d2y := next.x-cur.x, next.y-cur.y
	l1, l2 := math.Hypot(d1x, d1y), math.Hypot(d2x, d2y)
	if l1 == 0 || l2 == 0 {
		return
	}
	d1x, d1y, d2x, d2y = d1x/l1, d1y/l1, d2x/l2, d2y/l2
	crs := d1x*d2y - d1y*d2x
	if math.Abs(crs) < 1e-9 && d1x*d2x+d1y*d2y > 0 {
		return // まっすぐ
	}
	if st.join == "round" {
		circle(cur)
		return
	}
	// 外側の向き（曲がる向きの反対）
	sgn := 1.0
	if crs > 0 {
		sgn = -1
	}
	n1 := svgPt{-d1y * hw * sgn, d1x * hw * sgn}
	n2 := svgPt{-d2y * hw * sgn, d2x * hw * sgn}
	p1 := svgPt{cur.x + n1.x, cur.y + n1.y}
	p2 := svgPt{cur.x + n2.x, cur.y + n2.y}
	if st.join != "bevel" {
		cosT := -(d1x*d2x + d1y*d2y) // 2 本の線のなす角 θ の cos
		sinHalf := math.Sqrt(math.Max(0, (1-cosT)/2))
		if sinHalf > 1e-9 && 1/sinHalf <= st.miterLimit {
			// とがった先：2 本の外側の線の交わる点
			bx, by := n1.x+n2.x, n1.y+n2.y
			bl := math.Hypot(bx, by)
			if bl > 1e-9 {
				ml := hw / sinHalf
				tip := svgPt{cur.x + bx/bl*ml, cur.y + by/bl*ml}
				add([]svgPt{cur, p1, tip, p2})
				return
			}
		}
	}
	add([]svgPt{cur, p1, p2})
}

func dedupPts(pts []svgPt) []svgPt {
	out := make([]svgPt, 0, len(pts))
	for _, p := range pts {
		if len(out) > 0 {
			q := out[len(out)-1]
			if math.Abs(q.x-p.x) < 1e-9 && math.Abs(q.y-p.y) < 1e-9 {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// dashPath は、道筋を破線の部分に分ける。
func dashPath(p svgPath, dash []float64, off float64) ([][]svgPt, []bool) {
	total := 0.0
	for _, d := range dash {
		total += d
	}
	if total <= 0 {
		return p.subs, p.closed
	}
	if len(dash)%2 == 1 {
		dash = append(dash, dash...)
	}
	var subs [][]svgPt
	for si, sub := range p.subs {
		pts := sub
		if p.closed[si] && len(sub) > 1 {
			pts = append(append([]svgPt{}, sub...), sub[0])
		}
		// 破線の位置
		pos := math.Mod(off, total)
		if pos < 0 {
			pos += total
		}
		di := 0
		for pos >= dash[di] {
			pos -= dash[di]
			di = (di + 1) % len(dash)
		}
		left := dash[di] - pos
		on := di%2 == 0
		var cur []svgPt
		if on && len(pts) > 0 {
			cur = []svgPt{pts[0]}
		}
		for i := 0; i+1 < len(pts); i++ {
			a, b := pts[i], pts[i+1]
			l := math.Hypot(b.x-a.x, b.y-a.y)
			t := 0.0
			for l-t > left {
				t += left
				q := svgPt{a.x + (b.x-a.x)*t/l, a.y + (b.y-a.y)*t/l}
				if on {
					cur = append(cur, q)
					subs = append(subs, cur)
					cur = nil
				} else {
					cur = []svgPt{q}
				}
				on = !on
				di = (di + 1) % len(dash)
				left = dash[di]
			}
			left -= l - t
			if on {
				cur = append(cur, b)
			}
		}
		if on && len(cur) > 1 {
			subs = append(subs, cur)
		}
	}
	closed := make([]bool, len(subs))
	return subs, closed
}

// ---------- 画素（乗算済みの RGBA、0〜1） ----------

type svgLayer struct {
	w, h int
	p    []float32 // r, g, b, a（乗算済み）
}

func newSVGLayer(w, h int) *svgLayer { return &svgLayer{w: w, h: h, p: make([]float32, w*h*4)} }

// over は、src を dst の上に重ねる（乗算済み）。opacity を掛ける。
func (dst *svgLayer) over(src *svgLayer, opacity float64) {
	o := float32(opacity)
	for i := 0; i < len(dst.p); i += 4 {
		sa := src.p[i+3] * o
		if sa == 0 {
			continue
		}
		k := 1 - sa
		dst.p[i] = src.p[i]*o + dst.p[i]*k
		dst.p[i+1] = src.p[i+1]*o + dst.p[i+1]*k
		dst.p[i+2] = src.p[i+2]*o + dst.p[i+2]*k
		dst.p[i+3] = sa + dst.p[i+3]*k
	}
}

// paint は、覆う割合 cv の画素に、色 col(x, y)（乗算していない r, g, b, a）を重ねる。
func (dst *svgLayer) paint(cv *coverage, col func(x, y int) (float64, float64, float64, float64)) {
	if cv == nil {
		return
	}
	for y := 0; y < cv.h; y++ {
		Y := cv.y0 + y
		if Y < 0 || Y >= dst.h {
			continue
		}
		for x := 0; x < cv.w; x++ {
			c := cv.a[y*cv.w+x]
			X := cv.x0 + x
			if c <= 0 || X < 0 || X >= dst.w {
				continue
			}
			r, g, b, a := col(X, Y)
			sa := float32(a) * c
			if sa <= 0 {
				continue
			}
			i := (Y*dst.w + X) * 4
			k := 1 - sa
			dst.p[i] = float32(r)*sa + dst.p[i]*k
			dst.p[i+1] = float32(g)*sa + dst.p[i+1]*k
			dst.p[i+2] = float32(b)*sa + dst.p[i+2]*k
			dst.p[i+3] = sa + dst.p[i+3]*k
		}
	}
}

// ---------- 描く ----------

// svgStyle は、受け継ぐ描き方。
type svgStyle struct {
	fill, stroke     string
	fillOp, strokeOp float64
	fillRule         string
	strokeW          float64
	join, capS       string
	miter            float64
	dash             string
	dashOff          float64
	color            svgColor
	paintOrder       string
	visible          bool
}

var defaultSVGStyle = svgStyle{fill: "black", stroke: "none", fillOp: 1, strokeOp: 1, fillRule: "nonzero", strokeW: 1,
	join: "miter", capS: "butt", miter: 4, visible: true}

type svgRenderer struct {
	ids  map[string]*svgNode
	w, h int
	tol  float64
}

// RenderSVG は SVG を w×h の画素に描き、乗算していない RGBA（image.NRGBA）を返す。
// viewBox は preserveAspectRatio にしたがって w×h に当てる。
func RenderSVG(b []byte, w, h int) (*image.NRGBA, error) {
	root, err := parseSVGTree(b)
	if err != nil {
		return nil, err
	}
	if w <= 0 || h <= 0 || w > 2000 || h > 2000 {
		return nil, fmt.Errorf("描く大きさ %dx%d が範囲の外です", w, h)
	}
	r := &svgRenderer{ids: map[string]*svgNode{}, w: w, h: h, tol: 0.05}
	var collect func(n *svgNode)
	collect = func(n *svgNode) {
		if id := n.attrs["id"]; id != "" {
			if _, dup := r.ids[id]; !dup {
				r.ids[id] = n
			}
		}
		for _, c := range n.children {
			collect(c)
		}
	}
	collect(root)
	vw, vh := SVGSize(root)
	m := svgIdentity
	if vb := svgNumbers(root.attrs["viewBox"]); len(vb) == 4 && vb[2] > 0 && vb[3] > 0 {
		m = viewBoxMatrix(vb, float64(w), float64(h), root.attrs["preserveAspectRatio"])
	} else if vw > 0 && vh > 0 {
		m = viewBoxMatrix([]float64{0, 0, vw, vh}, float64(w), float64(h), root.attrs["preserveAspectRatio"])
	}
	layer := newSVGLayer(w, h)
	st := defaultSVGStyle
	st = r.style(root, st)
	for _, c := range root.children {
		r.node(layer, c, m, st)
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		a := layer.p[i*4+3]
		if a <= 0 {
			continue
		}
		a = min(a, 1)
		for c := 0; c < 3; c++ {
			v := layer.p[i*4+c] / a
			out.Pix[i*4+c] = uint8(math.Round(float64(max(0, min(1, v))) * 255))
		}
		out.Pix[i*4+3] = uint8(math.Round(float64(a) * 255))
	}
	return out, nil
}

// SVGSize は、svg の幅と高さ（width、height、なければ viewBox）。
func SVGSize(root *svgNode) (float64, float64) {
	vb := svgNumbers(root.attrs["viewBox"])
	w := svgLength(root.attrs["width"], 0, 0)
	h := svgLength(root.attrs["height"], 0, 0)
	if strings.HasSuffix(root.attrs["width"], "%") || w <= 0 {
		w = 0
	}
	if strings.HasSuffix(root.attrs["height"], "%") || h <= 0 {
		h = 0
	}
	if len(vb) == 4 {
		if w == 0 {
			w = vb[2]
		}
		if h == 0 {
			h = vb[3]
		}
	}
	return w, h
}

// SVGAspect は、SVG の幅と高さを読む（描く大きさを決めるため）。
func SVGAspect(b []byte) (float64, float64, error) {
	root, err := parseSVGTree(b)
	if err != nil {
		return 0, 0, err
	}
	w, h := SVGSize(root)
	if w <= 0 || h <= 0 {
		return 0, 0, errors.New("SVG に大きさ（width、height、viewBox）がありません")
	}
	return w, h, nil
}

// viewBoxMatrix は、viewBox を w×h に当てる行列。
func viewBoxMatrix(vb []float64, w, h float64, par string) svgMatrix {
	sx, sy := w/vb[2], h/vb[3]
	f := strings.Fields(par)
	align, slice := "xMidYMid", false
	if len(f) > 0 {
		align = f[0]
	}
	if len(f) > 1 && f[1] == "slice" {
		slice = true
	}
	if align == "none" {
		return svgMatrix{sx, 0, 0, sy, -vb[0] * sx, -vb[1] * sy}
	}
	s := math.Min(sx, sy)
	if slice {
		s = math.Max(sx, sy)
	}
	tx, ty := -vb[0]*s, -vb[1]*s
	ex, ey := w-vb[2]*s, h-vb[3]*s
	switch {
	case strings.Contains(align, "xMid"):
		tx += ex / 2
	case strings.Contains(align, "xMax"):
		tx += ex
	}
	switch {
	case strings.Contains(align, "YMid"):
		ty += ey / 2
	case strings.Contains(align, "YMax"):
		ty += ey
	}
	return svgMatrix{s, 0, 0, s, tx, ty}
}

// style は、要素の描き方を親から受け継いで、要素の属性で上書きする。
func (r *svgRenderer) style(n *svgNode, st svgStyle) svgStyle {
	at := func(k string) (string, bool) {
		v, ok := n.attrs[k]
		if !ok || v == "inherit" {
			return "", false
		}
		return v, true
	}
	if v, ok := at("color"); ok {
		if c, ok := parseSVGColor(v, st.color); ok {
			st.color = c
		}
	}
	if v, ok := at("fill"); ok {
		st.fill = v
	}
	if v, ok := at("stroke"); ok {
		st.stroke = v
	}
	if v, ok := at("fill-opacity"); ok {
		st.fillOp = svgFraction(v, 1)
	}
	if v, ok := at("stroke-opacity"); ok {
		st.strokeOp = svgFraction(v, 1)
	}
	if v, ok := at("fill-rule"); ok {
		st.fillRule = v
	}
	if v, ok := at("stroke-width"); ok {
		st.strokeW = svgLength(v, 1, 1)
	}
	if v, ok := at("stroke-linejoin"); ok {
		st.join = v
	}
	if v, ok := at("stroke-linecap"); ok {
		st.capS = v
	}
	if v, ok := at("stroke-miterlimit"); ok {
		st.miter = svgLength(v, 1, 4)
	}
	if v, ok := at("stroke-dasharray"); ok {
		st.dash = v
	}
	if v, ok := at("stroke-dashoffset"); ok {
		st.dashOff = svgLength(v, 1, 0)
	}
	if v, ok := at("paint-order"); ok {
		st.paintOrder = v
	}
	if v, ok := at("visibility"); ok {
		st.visible = v != "hidden" && v != "collapse"
	}
	return st
}

// node は要素 1 つを dst に描く。m は、要素の座標から画素への行列（要素の transform を当てる前）。
func (r *svgRenderer) node(dst *svgLayer, n *svgNode, m svgMatrix, st svgStyle) {
	switch n.name {
	case "defs", "linearGradient", "radialGradient", "filter", "title", "desc", "metadata", "style", "clipPath", "mask",
		"pattern", "marker", "symbol", "script", "text":
		return
	}
	if n.attrs["display"] == "none" {
		return
	}
	if t, ok := n.attrs["transform"]; ok {
		m = m.mul(parseTransform(t))
	}
	st = r.style(n, st)
	opacity := svgFraction(n.attrs["opacity"], 1)
	if opacity <= 0 {
		return
	}
	filt := r.filterOf(n)
	// 不透明度かフィルターがあれば、別の層に描いてから重ねる
	target := dst
	if opacity < 1 || filt != nil {
		target = newSVGLayer(r.w, r.h)
	}
	var bbox [4]float64
	hasBox := false
	switch n.name {
	case "svg", "g", "a", "switch":
		for _, c := range n.children {
			r.node(target, c, m, st)
		}
		if filt != nil {
			bbox, hasBox = r.groupBBox(n, svgIdentity)
		}
	case "image":
		bbox, hasBox = r.image(target, n, m)
	default:
		pb := r.shape(n, m)
		if pb == nil {
			return
		}
		bbox, hasBox = pb.bbox, pb.hasBox
		if st.visible {
			r.drawShape(target, pb, m, st)
		}
	}
	if target == dst {
		return
	}
	if filt != nil && hasBox {
		target = r.applyFilter(filt, target, m, bbox)
	}
	dst.over(target, opacity)
}

// groupBBox は、g の中の形の範囲（g の座標）。フィルターの範囲に使う。
func (r *svgRenderer) groupBBox(n *svgNode, m svgMatrix) ([4]float64, bool) {
	var box [4]float64
	has := false
	for _, c := range n.children {
		cm := m
		if t, ok := c.attrs["transform"]; ok {
			cm = m.mul(parseTransform(t))
		}
		var b [4]float64
		ok := false
		switch c.name {
		case "g", "a":
			b, ok = r.groupBBox(c, cm)
		default:
			if pb := r.shape(c, cm); pb != nil && pb.hasBox {
				// shape の bbox は c の座標なので、cm で g の座標にする
				x0, y0 := cm.apply(pb.bbox[0], pb.bbox[1])
				x1, y1 := cm.apply(pb.bbox[2], pb.bbox[1])
				x2, y2 := cm.apply(pb.bbox[0], pb.bbox[3])
				x3, y3 := cm.apply(pb.bbox[2], pb.bbox[3])
				b = [4]float64{math.Min(math.Min(x0, x1), math.Min(x2, x3)), math.Min(math.Min(y0, y1), math.Min(y2, y3)),
					math.Max(math.Max(x0, x1), math.Max(x2, x3)), math.Max(math.Max(y0, y1), math.Max(y2, y3))}
				ok = true
			}
		}
		if !ok {
			continue
		}
		if !has {
			box, has = b, true
			continue
		}
		box = [4]float64{math.Min(box[0], b[0]), math.Min(box[1], b[1]), math.Max(box[2], b[2]), math.Max(box[3], b[3])}
	}
	return box, has
}

// shape は、形の要素を道筋にする。形でなければ nil。
func (r *svgRenderer) shape(n *svgNode, m svgMatrix) *pathBuilder {
	pb := &pathBuilder{m: m, tol: r.tol}
	num := func(k string) float64 { return svgLength(n.attrs[k], 0, 0) }
	switch n.name {
	case "path":
		buildPathData(pb, n.attrs["d"])
	case "rect":
		x, y, w, h := num("x"), num("y"), num("width"), num("height")
		if w <= 0 || h <= 0 {
			return nil
		}
		rx, ry := num("rx"), num("ry")
		if _, ok := n.attrs["ry"]; !ok {
			ry = rx
		}
		if _, ok := n.attrs["rx"]; !ok {
			rx = ry
		}
		rx, ry = math.Min(rx, w/2), math.Min(ry, h/2)
		if rx <= 0 || ry <= 0 {
			pb.moveTo(x, y)
			pb.lineTo(x+w, y)
			pb.lineTo(x+w, y+h)
			pb.lineTo(x, y+h)
			pb.close()
			break
		}
		pb.moveTo(x+rx, y)
		pb.lineTo(x+w-rx, y)
		pb.arcTo(x+w-rx, y, rx, ry, 0, false, true, x+w, y+ry)
		pb.lineTo(x+w, y+h-ry)
		pb.arcTo(x+w, y+h-ry, rx, ry, 0, false, true, x+w-rx, y+h)
		pb.lineTo(x+rx, y+h)
		pb.arcTo(x+rx, y+h, rx, ry, 0, false, true, x, y+h-ry)
		pb.lineTo(x, y+ry)
		pb.arcTo(x, y+ry, rx, ry, 0, false, true, x+rx, y)
		pb.close()
	case "circle", "ellipse":
		cx, cy := num("cx"), num("cy")
		rx, ry := num("r"), num("r")
		if n.name == "ellipse" {
			rx, ry = num("rx"), num("ry")
		}
		if rx <= 0 || ry <= 0 {
			return nil
		}
		pb.moveTo(cx+rx, cy)
		pb.arcTo(cx+rx, cy, rx, ry, 0, false, true, cx-rx, cy)
		pb.arcTo(cx-rx, cy, rx, ry, 0, false, true, cx+rx, cy)
		pb.close()
	case "line":
		pb.moveTo(num("x1"), num("y1"))
		pb.lineTo(num("x2"), num("y2"))
		pb.flush(false)
	case "polyline", "polygon":
		v := svgNumbers(n.attrs["points"])
		for i := 0; i+1 < len(v); i += 2 {
			if i == 0 {
				pb.moveTo(v[0], v[1])
			} else {
				pb.lineTo(v[i], v[i+1])
			}
		}
		if n.name == "polygon" {
			pb.close()
		} else {
			pb.flush(false)
		}
	default:
		return nil
	}
	if len(pb.out.subs) == 0 {
		return nil
	}
	return pb
}

// drawShape は、形を塗って線を引く（paint-order の順）。
func (r *svgRenderer) drawShape(dst *svgLayer, pb *pathBuilder, m svgMatrix, st svgStyle) {
	clip := image.Rect(0, 0, r.w, r.h)
	fillFirst := true
	if po := strings.Fields(st.paintOrder); len(po) > 0 {
		fi, si := 99, 99
		for i, k := range po {
			switch k {
			case "fill":
				fi = i
			case "stroke":
				si = i
			}
		}
		// 書いていないものは、書いたもののあとに既定の順（fill、stroke）で続く
		if fi == 99 && si != 99 {
			fi = 100
		}
		fillFirst = fi < si
	}
	doFill := func() {
		if st.fill == "none" || st.fillOp <= 0 {
			return
		}
		col := r.paintFunc(st.fill, st.fillOp, st.color, m, pb.bbox)
		if col == nil {
			return
		}
		dst.paint(fillCoverage(pb.out, st.fillRule == "evenodd", clip), col)
	}
	doStroke := func() {
		if st.stroke == "none" || st.strokeOp <= 0 || st.strokeW <= 0 {
			return
		}
		col := r.paintFunc(st.stroke, st.strokeOp, st.color, m, pb.bbox)
		if col == nil {
			return
		}
		sx, sy := m.scales()
		scale := math.Sqrt(sx * sy)
		ss := strokeStyle{width: st.strokeW * scale, join: st.join, capStyle: st.capS, miterLimit: st.miter, dashOff: st.dashOff * scale}
		for _, d := range svgNumbers(strings.ReplaceAll(st.dash, "none", "")) {
			ss.dash = append(ss.dash, d*scale)
		}
		dst.paint(fillCoverage(strokeToPolys(pb.out, ss), false, clip), col)
	}
	if fillFirst {
		doFill()
		doStroke()
	} else {
		doStroke()
		doFill()
	}
}

// paintFunc は、塗りの書き方（色か url(#id)）から、画素ごとの色を返す関数を作る。描かないなら nil。
func (r *svgRenderer) paintFunc(spec string, opacity float64, cur svgColor, m svgMatrix, bbox [4]float64) func(x, y int) (float64, float64, float64, float64) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "url(") {
		end := strings.IndexByte(spec, ')')
		if end < 0 {
			return nil
		}
		id := strings.Trim(strings.TrimSpace(spec[4:end]), `"'`)
		id = strings.TrimPrefix(id, "#")
		fallback := strings.TrimSpace(spec[end+1:])
		if g := r.ids[id]; g != nil && (g.name == "linearGradient" || g.name == "radialGradient") {
			if f := r.gradient(g, opacity, m, bbox); f != nil {
				return f
			}
			return nil
		}
		if fallback == "" || fallback == "none" {
			return nil
		}
		spec = fallback
	}
	c, ok := parseSVGColor(spec, cur)
	if !ok {
		return nil
	}
	return func(int, int) (float64, float64, float64, float64) { return c.r, c.g, c.b, opacity }
}

type gradStop struct {
	off        float64
	r, g, b, a float64
}

// gradAttr は、グラデーションの属性を、href をたどって探す。
func (r *svgRenderer) gradAttr(g *svgNode, k string) (string, bool) {
	for i := 0; g != nil && i < 16; i++ {
		if v, ok := g.attrs[k]; ok {
			return v, true
		}
		g = r.ids[strings.TrimPrefix(g.attrs["href"], "#")]
	}
	return "", false
}

// gradStops は、stop を持つグラデーションまで href をたどって stop を読む。
func (r *svgRenderer) gradStops(g *svgNode) []gradStop {
	for i := 0; g != nil && i < 16; i++ {
		var out []gradStop
		last := 0.0
		for _, s := range g.children {
			if s.name != "stop" {
				continue
			}
			off := math.Max(last, svgFraction(s.attrs["offset"], 0))
			last = off
			c, ok := parseSVGColor(func() string {
				if v, ok := s.attrs["stop-color"]; ok {
					return v
				}
				return "black"
			}(), svgColor{})
			if !ok {
				c = svgColor{}
			}
			out = append(out, gradStop{off, c.r, c.g, c.b, svgFraction(s.attrs["stop-opacity"], 1)})
		}
		if len(out) > 0 {
			return out
		}
		g = r.ids[strings.TrimPrefix(g.attrs["href"], "#")]
	}
	return nil
}

// gradient は、グラデーションの色を返す関数を作る。
func (r *svgRenderer) gradient(g *svgNode, opacity float64, m svgMatrix, bbox [4]float64) func(x, y int) (float64, float64, float64, float64) {
	stops := r.gradStops(g)
	if len(stops) == 0 {
		return nil
	}
	if len(stops) == 1 {
		s := stops[0]
		return func(int, int) (float64, float64, float64, float64) { return s.r, s.g, s.b, s.a * opacity }
	}
	units, _ := r.gradAttr(g, "gradientUnits")
	gt := svgIdentity
	if t, ok := r.gradAttr(g, "gradientTransform"); ok {
		gt = parseTransform(t)
	}
	spread, _ := r.gradAttr(g, "spreadMethod")
	user := units == "userSpaceOnUse"
	// グラデーションの座標から画素への行列
	gm := m
	if !user {
		bw, bh := bbox[2]-bbox[0], bbox[3]-bbox[1]
		if bw <= 0 || bh <= 0 {
			return nil // 幅か高さが 0 の形には、objectBoundingBox のグラデーションを使わない
		}
		gm = gm.mul(svgMatrix{bw, 0, 0, bh, bbox[0], bbox[1]})
	}
	gm = gm.mul(gt)
	inv, ok := gm.inverse()
	if !ok {
		return nil
	}
	length := func(k string, def string, ref float64) float64 {
		v, ok := r.gradAttr(g, k)
		if !ok {
			v = def
		}
		if user {
			return svgLength(v, ref, 0)
		}
		return svgLength(v, 1, 0) // objectBoundingBox では、% も 0〜1 の割合
	}
	sw, sh := float64(r.w), float64(r.h) // userSpaceOnUse の % は、本来は viewport に対する。アイコンでは使わない
	var tOf func(x, y float64) float64
	if g.name == "linearGradient" {
		x1, y1 := length("x1", "0%", sw), length("y1", "0%", sh)
		x2, y2 := length("x2", "100%", sw), length("y2", "0%", sh)
		dx, dy := x2-x1, y2-y1
		dd := dx*dx + dy*dy
		if dd == 0 {
			s := stops[len(stops)-1]
			return func(int, int) (float64, float64, float64, float64) { return s.r, s.g, s.b, s.a * opacity }
		}
		tOf = func(x, y float64) float64 { return ((x-x1)*dx + (y-y1)*dy) / dd }
	} else {
		cx, cy := length("cx", "50%", sw), length("cy", "50%", sh)
		rr := length("r", "50%", math.Hypot(sw, sh)/math.Sqrt2)
		fx, fy := cx, cy
		if _, ok := r.gradAttr(g, "fx"); ok {
			fx = length("fx", "50%", sw)
		}
		if _, ok := r.gradAttr(g, "fy"); ok {
			fy = length("fy", "50%", sh)
		}
		if rr <= 0 {
			s := stops[len(stops)-1]
			return func(int, int) (float64, float64, float64, float64) { return s.r, s.g, s.b, s.a * opacity }
		}
		tOf = func(x, y float64) float64 {
			// 焦点 f から点 p への半直線が円と交わるところまでの割合
			px, py := x-fx, y-fy
			ox, oy := fx-cx, fy-cy
			a := px*px + py*py
			if a == 0 {
				return 0
			}
			b := 2 * (px*ox + py*oy)
			c := ox*ox + oy*oy - rr*rr
			disc := b*b - 4*a*c
			if disc < 0 {
				return 1
			}
			t := (-b + math.Sqrt(disc)) / (2 * a)
			if t <= 0 {
				return 1
			}
			return 1 / t
		}
	}
	return func(X, Y int) (float64, float64, float64, float64) {
		x, y := inv.apply(float64(X)+0.5, float64(Y)+0.5)
		t := tOf(x, y)
		switch spread {
		case "repeat":
			t -= math.Floor(t)
		case "reflect":
			t = math.Mod(math.Abs(t), 2)
			if t > 1 {
				t = 2 - t
			}
		default:
			t = math.Max(0, math.Min(1, t))
		}
		s0 := stops[0]
		if t <= s0.off {
			return s0.r, s0.g, s0.b, s0.a * opacity
		}
		for i := 1; i < len(stops); i++ {
			s1 := stops[i]
			if t <= s1.off {
				f := 0.0
				if s1.off > s0.off {
					f = (t - s0.off) / (s1.off - s0.off)
				}
				// 色は乗算済みで混ぜる（ブラウザと同じ）
				a := s0.a + (s1.a-s0.a)*f
				if a <= 0 {
					return 0, 0, 0, 0
				}
				mix := func(c0, c1 float64) float64 { return (c0*s0.a + (c1*s1.a-c0*s0.a)*f) / a }
				return mix(s0.r, s1.r), mix(s0.g, s1.g), mix(s0.b, s1.b), a * opacity
			}
			s0 = s1
		}
		return s0.r, s0.g, s0.b, s0.a * opacity
	}
}

// image は、data: の PNG（か JPEG 以外の、Go が読める画像）を描く。範囲（要素の座標）を返す。
func (r *svgRenderer) image(dst *svgLayer, n *svgNode, m svgMatrix) ([4]float64, bool) {
	x, y := svgLength(n.attrs["x"], 0, 0), svgLength(n.attrs["y"], 0, 0)
	w, h := svgLength(n.attrs["width"], 0, 0), svgLength(n.attrs["height"], 0, 0)
	href := n.attrs["href"]
	if w <= 0 || h <= 0 || !strings.HasPrefix(href, "data:") {
		return [4]float64{}, false
	}
	comma := strings.IndexByte(href, ',')
	if comma < 0 || !strings.Contains(href[:comma], ";base64") {
		return [4]float64{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, href[comma+1:]))
	if err != nil {
		return [4]float64{}, false
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return [4]float64{}, false
	}
	b := img.Bounds()
	iw, ih := float64(b.Dx()), float64(b.Dy())
	if iw <= 0 || ih <= 0 {
		return [4]float64{}, false
	}
	box := [4]float64{x, y, x + w, y + h}
	// 画像の画素 → 要素の座標
	im := m.mul(viewBoxMatrix([]float64{0, 0, iw, ih}, w, h, n.attrs["preserveAspectRatio"]).translateBy(x, y))
	inv, ok := im.inverse()
	if !ok {
		return box, true
	}
	// 乗算済みの画素を先に作る
	pre := make([]float64, b.Dx()*b.Dy()*4)
	for yy := 0; yy < b.Dy(); yy++ {
		for xx := 0; xx < b.Dx(); xx++ {
			cr, cg, cb, ca := img.At(b.Min.X+xx, b.Min.Y+yy).RGBA() // 乗算済み、0〜65535
			i := (yy*b.Dx() + xx) * 4
			pre[i], pre[i+1], pre[i+2], pre[i+3] = float64(cr)/65535, float64(cg)/65535, float64(cb)/65535, float64(ca)/65535
		}
	}
	sample := func(u, v float64) (float64, float64, float64, float64) {
		// バイリニア（画素の中心どうしを結ぶ）。外は透明
		u -= 0.5
		v -= 0.5
		x0, y0 := int(math.Floor(u)), int(math.Floor(v))
		fu, fv := u-float64(x0), v-float64(y0)
		var acc [4]float64
		for j := 0; j < 2; j++ {
			for i := 0; i < 2; i++ {
				xx, yy := x0+i, y0+j
				wt := (1 - math.Abs(float64(i)-fu)) * (1 - math.Abs(float64(j)-fv))
				xx = max(0, min(b.Dx()-1, xx))
				yy = max(0, min(b.Dy()-1, yy))
				k := (yy*b.Dx() + xx) * 4
				for c := 0; c < 4; c++ {
					acc[c] += pre[k+c] * wt
				}
			}
		}
		return acc[0], acc[1], acc[2], acc[3]
	}
	// 描く範囲（画素）
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, c := range [][2]float64{{0, 0}, {iw, 0}, {0, ih}, {iw, ih}} {
		X, Y := im.apply(c[0], c[1])
		minX, minY, maxX, maxY = math.Min(minX, X), math.Min(minY, Y), math.Max(maxX, X), math.Max(maxY, Y)
	}
	x0, y0 := max(0, int(math.Floor(minX))), max(0, int(math.Floor(minY)))
	x1, y1 := min(r.w, int(math.Ceil(maxX))), min(r.h, int(math.Ceil(maxY)))
	// 縁を滑らかにするため、1 画素を 4×4 で見る
	for Y := y0; Y < y1; Y++ {
		for X := x0; X < x1; X++ {
			var acc [4]float64
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					u, v := inv.apply(float64(X)+(float64(sx)+0.5)/4, float64(Y)+(float64(sy)+0.5)/4)
					if u < 0 || v < 0 || u >= iw || v >= ih {
						continue
					}
					cr, cg, cb, ca := sample(u, v)
					acc[0] += cr
					acc[1] += cg
					acc[2] += cb
					acc[3] += ca
				}
			}
			sa := acc[3] / 16
			if sa <= 0 {
				continue
			}
			i := (Y*r.w + X) * 4
			k := float32(1 - sa)
			dst.p[i] = float32(acc[0]/16) + dst.p[i]*k
			dst.p[i+1] = float32(acc[1]/16) + dst.p[i+1]*k
			dst.p[i+2] = float32(acc[2]/16) + dst.p[i+2]*k
			dst.p[i+3] = float32(sa) + dst.p[i+3]*k
		}
	}
	return box, true
}

// translateBy は、先に (x, y) だけ動かしてから m を当てる行列（m のあとに平行移動）。
func (m svgMatrix) translateBy(x, y float64) svgMatrix {
	return svgMatrix{1, 0, 0, 1, x, y}.mul(m)
}
