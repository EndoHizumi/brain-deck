package main

import (
	"math"
	"strings"
)

// ---------- SVG のフィルター ----------
//
// 要素を別の層に描いてから、フィルターの手順（fe*）を画素に当て、フィルターの範囲で切って重ねる。
// 範囲は filterUnits（既定 objectBoundingBox）、手順の数（stdDeviation など）は primitiveUnits（既定 userSpaceOnUse）。
// 回転や傾きのある変形では、範囲は画素での外接の長方形、ぼかしの大きさは x と y の拡大率で近づける。
// 手順ごとの範囲（fe* の x、y、width、height）は使わない（フィルター全体の範囲だけ）。

// filterOf は、要素の filter: url(#id) の filter 要素を返す。なければ nil。
func (r *svgRenderer) filterOf(n *svgNode) *svgNode {
	v := strings.TrimSpace(n.attrs["filter"])
	if !strings.HasPrefix(v, "url(") {
		return nil
	}
	end := strings.IndexByte(v, ')')
	if end < 0 {
		return nil
	}
	id := strings.TrimPrefix(strings.Trim(strings.TrimSpace(v[4:end]), `"'`), "#")
	f := r.ids[id]
	if f == nil || f.name != "filter" {
		return nil
	}
	return f
}

// fimg は、フィルターの途中の画像（乗算済みの RGBA、0〜1）。
type fimg = []float32

// applyFilter は、src（要素だけを描いた層）にフィルターを当てた層を返す。m は要素の座標から画素への行列、bbox は要素の範囲。
func (r *svgRenderer) applyFilter(f *svgNode, src *svgLayer, m svgMatrix, bbox [4]float64) *svgLayer {
	w, h := r.w, r.h
	// フィルターの範囲（要素の座標）
	fx, fy, fw, fh := -0.1, -0.1, 1.2, 1.2
	if f.attrs["filterUnits"] == "userSpaceOnUse" {
		fx = svgLength(f.attrs["x"], float64(w), bbox[0]-0.1*(bbox[2]-bbox[0]))
		fy = svgLength(f.attrs["y"], float64(h), bbox[1]-0.1*(bbox[3]-bbox[1]))
		fw = svgLength(f.attrs["width"], float64(w), 1.2*(bbox[2]-bbox[0]))
		fh = svgLength(f.attrs["height"], float64(h), 1.2*(bbox[3]-bbox[1]))
	} else {
		bw, bh := bbox[2]-bbox[0], bbox[3]-bbox[1]
		fx = bbox[0] + svgLength(f.attrs["x"], 1, fx)*bw
		fy = bbox[1] + svgLength(f.attrs["y"], 1, fy)*bh
		fw = svgLength(f.attrs["width"], 1, fw) * bw
		fh = svgLength(f.attrs["height"], 1, fh) * bh
	}
	if fw <= 0 || fh <= 0 {
		return newSVGLayer(w, h) // 範囲が空なら、何も描かない
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, c := range [][2]float64{{fx, fy}, {fx + fw, fy}, {fx, fy + fh}, {fx + fw, fy + fh}} {
		X, Y := m.apply(c[0], c[1])
		minX, minY, maxX, maxY = math.Min(minX, X), math.Min(minY, Y), math.Max(maxX, X), math.Max(maxY, Y)
	}
	// 範囲の外は透明にする（ぼかしは範囲の外を透明とみなす）
	rx0, ry0 := math.Max(0, minX), math.Max(0, minY)
	rx1, ry1 := math.Min(float64(w), maxX), math.Min(float64(h), maxY)
	if rx1 <= rx0 || ry1 <= ry0 {
		return newSVGLayer(w, h)
	}
	region := func(img fimg) {
		for Y := 0; Y < h; Y++ {
			for X := 0; X < w; X++ {
				// 画素が範囲に入る割合（縁は少しだけ）
				cx := math.Max(0, math.Min(float64(X+1), rx1)-math.Max(float64(X), rx0))
				cy := math.Max(0, math.Min(float64(Y+1), ry1)-math.Max(float64(Y), ry0))
				k := float32(cx * cy)
				if k >= 1 {
					continue
				}
				i := (Y*w + X) * 4
				img[i] *= k
				img[i+1] *= k
				img[i+2] *= k
				img[i+3] *= k
			}
		}
	}
	srcImg := append(fimg(nil), src.p...)
	region(srcImg)
	sx, sy := m.scales()
	bw, bh := bbox[2]-bbox[0], bbox[3]-bbox[1]
	primBBox := f.attrs["primitiveUnits"] == "objectBoundingBox"
	unitX := func(v float64) float64 {
		if primBBox {
			v *= bw
		}
		return v * sx
	}
	unitY := func(v float64) float64 {
		if primBBox {
			v *= bh
		}
		return v * sy
	}
	results := map[string]fimg{}
	var last fimg
	var sourceAlpha fimg
	input := func(name string) fimg {
		switch name {
		case "", "SourceGraphic":
			if name == "" && last != nil {
				return last
			}
			return srcImg
		case "SourceAlpha":
			if sourceAlpha == nil {
				sourceAlpha = make(fimg, len(srcImg))
				for i := 3; i < len(srcImg); i += 4 {
					sourceAlpha[i] = srcImg[i]
				}
			}
			return sourceAlpha
		}
		if v, ok := results[name]; ok {
			return v
		}
		if last != nil {
			return last
		}
		return srcImg
	}
	defLinear := f.attrs["color-interpolation-filters"] != "sRGB"
	for _, p := range f.children {
		if !strings.HasPrefix(p.name, "fe") {
			continue
		}
		linear := defLinear
		if v, ok := p.attrs["color-interpolation-filters"]; ok && v != "auto" && v != "inherit" {
			linear = v == "linearRGB"
		}
		in := input(p.attrs["in"])
		if linear && p.name != "feGaussianBlur" && p.name != "feOffset" && p.name != "feMerge" && p.name != "feFlood" {
			in = toLinear(in)
		}
		var out fimg
		switch p.name {
		case "feGaussianBlur":
			v := svgNumbers(p.attrs["stdDeviation"])
			dx, dy := 0.0, 0.0
			if len(v) > 0 {
				dx, dy = v[0], v[0]
			}
			if len(v) > 1 {
				dy = v[1]
			}
			out = gaussianBlur(in, w, h, unitX(dx), unitY(dy))
		case "feColorMatrix":
			out = colorMatrix(in, p.attrs["type"], svgNumbers(p.attrs["values"]))
		case "feComponentTransfer":
			out = componentTransfer(in, p)
		case "feComposite":
			in2 := input(p.attrs["in2"])
			if linear {
				in2 = toLinear(in2)
			}
			k := [4]float64{svgLength(p.attrs["k1"], 0, 0), svgLength(p.attrs["k2"], 0, 0), svgLength(p.attrs["k3"], 0, 0), svgLength(p.attrs["k4"], 0, 0)}
			out = composite(in, in2, p.attrs["operator"], k)
		case "feBlend":
			in2 := input(p.attrs["in2"])
			if linear {
				in2 = toLinear(in2)
			}
			out = blend(in, in2, p.attrs["mode"])
		case "feConvolveMatrix":
			out = convolve(in, w, h, p)
		case "feOffset":
			out = offset(in, w, h, unitX(svgLength(p.attrs["dx"], 0, 0)), unitY(svgLength(p.attrs["dy"], 0, 0)))
		case "feFlood":
			c, ok := parseSVGColor(func() string {
				if v, ok := p.attrs["flood-color"]; ok {
					return v
				}
				return "black"
			}(), svgColor{})
			if !ok {
				c = svgColor{}
			}
			a := float32(svgFraction(p.attrs["flood-opacity"], 1))
			out = make(fimg, len(srcImg))
			for i := 0; i < len(out); i += 4 {
				out[i], out[i+1], out[i+2], out[i+3] = float32(c.r)*a, float32(c.g)*a, float32(c.b)*a, a
			}
			region(out)
		case "feMerge":
			out = make(fimg, len(srcImg))
			for _, mn := range p.children {
				if mn.name != "feMergeNode" {
					continue
				}
				s := input(mn.attrs["in"])
				for i := 0; i < len(out); i += 4 {
					k := 1 - s[i+3]
					out[i] = s[i] + out[i]*k
					out[i+1] = s[i+1] + out[i+1]*k
					out[i+2] = s[i+2] + out[i+2]*k
					out[i+3] = s[i+3] + out[i+3]*k
				}
			}
		default:
			out = append(fimg(nil), in...) // 知らない手順は、そのまま通す
		}
		if linear && p.name != "feGaussianBlur" && p.name != "feOffset" && p.name != "feMerge" && p.name != "feFlood" {
			out = toSRGB(out)
		}
		region(out)
		last = out
		if res := p.attrs["result"]; res != "" {
			results[res] = out
		}
	}
	if last == nil {
		return newSVGLayer(w, h) // 手順のないフィルターは、何も描かない
	}
	return &svgLayer{w: w, h: h, p: last}
}

// toLinear、toSRGB は、乗算済みの色を、linearRGB と sRGB のあいだで変える。
func toLinear(in fimg) fimg {
	out := make(fimg, len(in))
	for i := 0; i < len(in); i += 4 {
		a := in[i+3]
		out[i+3] = a
		if a <= 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			v := float64(in[i+c] / a)
			if v <= 0.04045 {
				v /= 12.92
			} else {
				v = math.Pow((v+0.055)/1.055, 2.4)
			}
			out[i+c] = float32(v) * a
		}
	}
	return out
}

func toSRGB(in fimg) fimg {
	out := make(fimg, len(in))
	for i := 0; i < len(in); i += 4 {
		a := in[i+3]
		out[i+3] = a
		if a <= 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			v := float64(in[i+c] / a)
			if v <= 0.0031308 {
				v *= 12.92
			} else {
				v = 1.055*math.Pow(v, 1/2.4) - 0.055
			}
			out[i+c] = float32(v) * a
		}
	}
	return out
}

// gaussianBlur は、x、y の向きに標準偏差 dx、dy（画素）でぼかす。乗算済みのまま混ぜる。
func gaussianBlur(in fimg, w, h int, dx, dy float64) fimg {
	if dx < 0 || dy < 0 {
		return make(fimg, len(in))
	}
	out := append(fimg(nil), in...)
	if dx > 0 {
		out = blur1(out, w, h, dx, true)
	}
	if dy > 0 {
		out = blur1(out, w, h, dy, false)
	}
	return out
}

func gaussKernel(sd float64) []float32 {
	r := int(math.Ceil(sd * 3))
	k := make([]float32, 2*r+1)
	sum := 0.0
	for i := -r; i <= r; i++ {
		v := math.Exp(-float64(i*i) / (2 * sd * sd))
		k[i+r] = float32(v)
		sum += v
	}
	for i := range k {
		k[i] /= float32(sum)
	}
	return k
}

func blur1(in fimg, w, h int, sd float64, horiz bool) fimg {
	k := gaussKernel(sd)
	r := len(k) / 2
	out := make(fimg, len(in))
	n, lines := w, h
	if !horiz {
		n, lines = h, w
	}
	idx := func(line, i int) int {
		if horiz {
			return (line*w + i) * 4
		}
		return (i*w + line) * 4
	}
	for l := 0; l < lines; l++ {
		for i := 0; i < n; i++ {
			var acc [4]float32
			for j := -r; j <= r; j++ {
				t := i + j
				if t < 0 || t >= n {
					continue // 外は透明
				}
				q := idx(l, t)
				kv := k[j+r]
				acc[0] += in[q] * kv
				acc[1] += in[q+1] * kv
				acc[2] += in[q+2] * kv
				acc[3] += in[q+3] * kv
			}
			o := idx(l, i)
			out[o], out[o+1], out[o+2], out[o+3] = acc[0], acc[1], acc[2], acc[3]
		}
	}
	return out
}

// colorMatrix は feColorMatrix（matrix、saturate、hueRotate、luminanceToAlpha）。乗算していない色で計算する。
func colorMatrix(in fimg, typ string, v []float64) fimg {
	var mx [20]float64
	switch typ {
	case "saturate":
		s := 1.0
		if len(v) > 0 {
			s = v[0]
		}
		mx = [20]float64{0.213 + 0.787*s, 0.715 - 0.715*s, 0.072 - 0.072*s, 0, 0,
			0.213 - 0.213*s, 0.715 + 0.285*s, 0.072 - 0.072*s, 0, 0,
			0.213 - 0.213*s, 0.715 - 0.715*s, 0.072 + 0.928*s, 0, 0,
			0, 0, 0, 1, 0}
	case "hueRotate":
		a := 0.0
		if len(v) > 0 {
			a = v[0] * math.Pi / 180
		}
		c, s := math.Cos(a), math.Sin(a)
		mx = [20]float64{0.213 + c*0.787 - s*0.213, 0.715 - c*0.715 - s*0.715, 0.072 - c*0.072 + s*0.928, 0, 0,
			0.213 - c*0.213 + s*0.143, 0.715 + c*0.285 + s*0.140, 0.072 - c*0.072 - s*0.283, 0, 0,
			0.213 - c*0.213 - s*0.787, 0.715 - c*0.715 + s*0.715, 0.072 + c*0.928 + s*0.072, 0, 0,
			0, 0, 0, 1, 0}
	case "luminanceToAlpha":
		mx = [20]float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0.2125, 0.7154, 0.0721, 0, 0}
	default:
		if len(v) != 20 {
			return append(fimg(nil), in...) // 数が違えば、何もしない（単位行列）
		}
		copy(mx[:], v)
	}
	out := make(fimg, len(in))
	for i := 0; i < len(in); i += 4 {
		a := float64(in[i+3])
		var c [4]float64
		if a > 0 {
			c = [4]float64{float64(in[i]) / a, float64(in[i+1]) / a, float64(in[i+2]) / a, a}
		}
		var o [4]float64
		for row := 0; row < 4; row++ {
			o[row] = mx[row*5]*c[0] + mx[row*5+1]*c[1] + mx[row*5+2]*c[2] + mx[row*5+3]*c[3] + mx[row*5+4]
			o[row] = math.Max(0, math.Min(1, o[row]))
		}
		out[i], out[i+1], out[i+2], out[i+3] = float32(o[0]*o[3]), float32(o[1]*o[3]), float32(o[2]*o[3]), float32(o[3])
	}
	return out
}

// componentTransfer は feComponentTransfer（feFuncR、G、B、A の identity、table、discrete、linear、gamma）。
func componentTransfer(in fimg, p *svgNode) fimg {
	var funcs [4]func(float64) float64
	for _, c := range p.children {
		ch := map[string]int{"feFuncR": 0, "feFuncG": 1, "feFuncB": 2, "feFuncA": 3}
		k, ok := ch[c.name]
		if !ok {
			continue
		}
		at := func(n string, def float64) float64 { return svgLength(c.attrs[n], 0, def) }
		switch c.attrs["type"] {
		case "gamma":
			amp, ex, off := at("amplitude", 1), at("exponent", 1), at("offset", 0)
			funcs[k] = func(v float64) float64 { return amp*math.Pow(v, ex) + off }
		case "linear":
			sl, ic := at("slope", 1), at("intercept", 0)
			funcs[k] = func(v float64) float64 { return sl*v + ic }
		case "table", "discrete":
			tv := svgNumbers(c.attrs["tableValues"])
			if len(tv) == 0 {
				continue
			}
			disc := c.attrs["type"] == "discrete"
			funcs[k] = func(v float64) float64 {
				n := len(tv)
				if disc {
					i := int(math.Floor(v * float64(n)))
					return tv[max(0, min(n-1, i))]
				}
				if n == 1 {
					return tv[0]
				}
				pos := v * float64(n-1)
				i := int(math.Floor(pos))
				if i >= n-1 {
					return tv[n-1]
				}
				return tv[i] + (pos-float64(i))*(tv[i+1]-tv[i])
			}
		}
	}
	out := make(fimg, len(in))
	for i := 0; i < len(in); i += 4 {
		a := float64(in[i+3])
		var c [4]float64
		if a > 0 {
			c = [4]float64{float64(in[i]) / a, float64(in[i+1]) / a, float64(in[i+2]) / a, a}
		}
		for k := 0; k < 4; k++ {
			if funcs[k] != nil {
				c[k] = math.Max(0, math.Min(1, funcs[k](c[k])))
			}
		}
		out[i], out[i+1], out[i+2], out[i+3] = float32(c[0]*c[3]), float32(c[1]*c[3]), float32(c[2]*c[3]), float32(c[3])
	}
	return out
}

// composite は feComposite（over、in、out、atop、xor、arithmetic）。a が in、b が in2。乗算済み。
func composite(a, b fimg, op string, k [4]float64) fimg {
	out := make(fimg, len(a))
	for i := 0; i < len(a); i += 4 {
		aa, ba := a[i+3], b[i+3]
		for c := 0; c < 4; c++ {
			av, bv := a[i+c], b[i+c]
			var v float32
			switch op {
			case "in":
				v = av * ba
			case "out":
				v = av * (1 - ba)
			case "atop":
				v = av*ba + bv*(1-aa)
			case "xor":
				v = av*(1-ba) + bv*(1-aa)
			case "arithmetic":
				v = float32(k[0]*float64(av)*float64(bv) + k[1]*float64(av) + k[2]*float64(bv) + k[3])
			default: // over
				v = av + bv*(1-aa)
			}
			out[i+c] = max(0, min(1, v))
		}
		if op == "arithmetic" { // 色は透明度を超えない
			for c := 0; c < 3; c++ {
				out[i+c] = min(out[i+c], out[i+3])
			}
		}
	}
	return out
}

// blend は feBlend（normal、multiply、screen、darken、lighten）。a が in（上）、b が in2（下）。乗算済み。
func blend(a, b fimg, mode string) fimg {
	out := make(fimg, len(a))
	for i := 0; i < len(a); i += 4 {
		qa, qb := a[i+3], b[i+3]
		for c := 0; c < 3; c++ {
			ca, cb := a[i+c], b[i+c]
			var v float32
			switch mode {
			case "multiply":
				v = (1-qa)*cb + (1-qb)*ca + ca*cb
			case "screen":
				v = cb + ca - ca*cb
			case "darken":
				v = min((1-qa)*cb+ca, (1-qb)*ca+cb)
			case "lighten":
				v = max((1-qa)*cb+ca, (1-qb)*ca+cb)
			default:
				v = (1-qa)*cb + ca
			}
			out[i+c] = max(0, min(1, v))
		}
		out[i+3] = 1 - (1-qa)*(1-qb)
	}
	return out
}

// convolve は feConvolveMatrix。端は edgeMode（既定 duplicate）。preserveAlpha が true なら透明度は変えない。
func convolve(in fimg, w, h int, p *svgNode) fimg {
	order := svgNumbers(p.attrs["order"])
	ox, oy := 3, 3
	if len(order) > 0 {
		ox, oy = int(order[0]), int(order[0])
	}
	if len(order) > 1 {
		oy = int(order[1])
	}
	km := svgNumbers(p.attrs["kernelMatrix"])
	if ox < 1 || oy < 1 || len(km) != ox*oy {
		return make(fimg, len(in)) // 誤った行列は、透明な黒（仕様のとおり）
	}
	div := 0.0
	for _, v := range km {
		div += v
	}
	if d, ok := p.attrs["divisor"]; ok {
		if v := svgLength(d, 0, 0); v != 0 {
			div = v
		}
	}
	if div == 0 {
		div = 1
	}
	bias := svgLength(p.attrs["bias"], 0, 0)
	tx, ty := ox/2, oy/2
	if v, ok := p.attrs["targetX"]; ok {
		tx = int(svgLength(v, 0, float64(tx)))
	}
	if v, ok := p.attrs["targetY"]; ok {
		ty = int(svgLength(v, 0, float64(ty)))
	}
	edge := p.attrs["edgeMode"]
	if edge == "" {
		edge = "duplicate"
	}
	keepA := p.attrs["preserveAlpha"] == "true"
	get := func(x, y int) (int, bool) {
		switch edge {
		case "wrap":
			x, y = (x%w+w)%w, (y%h+h)%h
		case "none":
			if x < 0 || y < 0 || x >= w || y >= h {
				return 0, false
			}
		default:
			x, y = max(0, min(w-1, x)), max(0, min(h-1, y))
		}
		return (y*w + x) * 4, true
	}
	out := make(fimg, len(in))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var acc [4]float64
			for j := 0; j < oy; j++ {
				for i := 0; i < ox; i++ {
					q, ok := get(x-tx+i, y-ty+j)
					if !ok {
						continue
					}
					kv := km[(oy-1-j)*ox+(ox-1-i)] // 行列は 180 度回して当てる
					if keepA {
						a := float64(in[q+3])
						if a > 0 {
							acc[0] += float64(in[q]) / a * kv
							acc[1] += float64(in[q+1]) / a * kv
							acc[2] += float64(in[q+2]) / a * kv
						}
						continue
					}
					acc[0] += float64(in[q]) * kv
					acc[1] += float64(in[q+1]) * kv
					acc[2] += float64(in[q+2]) * kv
					acc[3] += float64(in[q+3]) * kv
				}
			}
			o := (y*w + x) * 4
			if keepA {
				a := in[o+3]
				for c := 0; c < 3; c++ {
					out[o+c] = float32(math.Max(0, math.Min(1, acc[c]/div+bias))) * a
				}
				out[o+3] = a
				continue
			}
			na := math.Max(0, math.Min(1, acc[3]/div+bias))
			out[o+3] = float32(na)
			for c := 0; c < 3; c++ {
				out[o+c] = float32(math.Max(0, math.Min(na, acc[c]/div+bias*na)))
			}
		}
	}
	return out
}

// offset は feOffset（画素で dx、dy だけ動かす。整数に丸める）。
func offset(in fimg, w, h int, dx, dy float64) fimg {
	out := make(fimg, len(in))
	ix, iy := int(math.Round(dx)), int(math.Round(dy))
	for y := 0; y < h; y++ {
		sy := y - iy
		if sy < 0 || sy >= h {
			continue
		}
		for x := 0; x < w; x++ {
			sx := x - ix
			if sx < 0 || sx >= w {
				continue
			}
			copy(out[(y*w+x)*4:(y*w+x)*4+4], in[(sy*w+sx)*4:(sy*w+sx)*4+4])
		}
	}
	return out
}
