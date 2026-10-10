package main

import (
	"fmt"
	"image"
	"strconv"
	"strings"
	"time"
)

// ---------- 天気のウィジェット（widget: weather） ----------
//
// 今日（と、幅があれば明日）の天気を、列に分けて出す。列には、見出し（今日 10/10(土)）、天気のアイコン、天気の文、
// 最高と最低の気温、6 時間ごとの降水確率を、列の大きさに応じて出す。いちばん下に、最終更新の時刻、発表の時刻、出典を小さく出す
// （古くなった、取得に失敗した、Brain の時刻を合わせていないときは橙色）。
//
//   - 列が低い（wthLineMode より低い）：1 行に、アイコン、見出し、天気、気温
//   - 列が縦長：アイコンの下に、天気と気温
//   - それ以外：アイコンの右に、天気と気温
//   - 列の高さが wthRainRow 以上なら下に降水確率、wthRainLabels 以上なら時間帯の見出しも
//
// 気温と降水確率の null（API で取れなかったもの。今日の最低気温、17 時の発表のあとの今日の最高気温、過ぎた時間帯）は「--」。
// 今日の、過ぎた時間帯の降水確率は薄く描く。アイコンの箱の大きさは列の大きさだけで決める（PC はこの大きさでアイコンを作る）。
// 配置は「描くものの並び」（wthItem）で作り、描くのはその並びを描くだけにする。gui/src/weatherwidget.ts と同じ結果にする。

const (
	wthColMin     = 140 // 列の幅がこれ以上とれれば、明日の列も出す
	wthColGap     = 8
	wthLineMode   = 48 // 列の高さがこれより低ければ 1 行
	wthRainRow    = 96
	wthRainLabels = 150
	wthRefW       = 48 // 縦長の列で、文字の倍率を決めるときの文の幅（全角 6 文字）。アイコンの箱を、文の長さで変えない
	wthIconMin    = 8  // アイコンの箱の高さがこれより小さければ、アイコンを出さない
	wthMaxScale   = 3
	wthNoData     = "天気を受け取っていません"
	wthFailedData = "天気を取得できません"
	wthNoDay      = "データなし"
	wthNull       = "--"
	wthSource     = "気象庁"
	wthSourceLong = "出典:気象庁"
)

var wthRainSlots = [4]string{"0-6", "6-12", "12-18", "18-24"}

var (
	colWthMax   = RGB{0xff, 0x8a, 0x50}
	colWthMin   = RGB{0x6c, 0xb4, 0xff}
	colWthRain1 = RGB{0xa8, 0xd4, 0xff} // 20〜49%
	colWthRain2 = RGB{0x4f, 0x9d, 0xff} // 50% 以上
)

// wthItem は、描くもの 1 つ。kind が text なら文字（X、Y は左上、S は倍率）、icon ならアイコンの箱（W×H）、rule なら線（W×H の塗り）。
type wthItem struct {
	Kind  string `json:"kind"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	S     int    `json:"s,omitempty"`
	W     int    `json:"w,omitempty"`
	H     int    `json:"h,omitempty"`
	Text  string `json:"text,omitempty"`
	Color string `json:"color,omitempty"` // #rrggbb
	Icon  string `json:"icon,omitempty"`  // アイコンの名前（<番号>-<幅>x<高さ>）
}

func (c RGB) hexStr() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// wthSeg は、色の違う文字のひとつながり。
type wthSeg struct {
	text string
	c    RGB
}

func segsWidth(segs []wthSeg) int {
	w := 0
	for _, s := range segs {
		w += font.textWidth(s.text)
	}
	return w
}

// wthTemp は気温の文（26° か --）と色。
func wthTemp(t *int, c RGB) wthSeg {
	if t == nil {
		return wthSeg{wthNull, colSub}
	}
	return wthSeg{strconv.Itoa(*t) + "°", c}
}

func wthTemps(d *WeatherDay) []wthSeg {
	return []wthSeg{wthTemp(d.Max, colWthMax), {"/", colSub}, wthTemp(d.Min, colWthMin)}
}

func wthRainColor(p *int) RGB {
	switch {
	case p == nil || *p < 20:
		return colSub
	case *p < 50:
		return colWthRain1
	}
	return colWthRain2
}

// wthLayout は、天気のセルの、見出しの下の範囲 area に描くものの並び。
func wthLayout(area image.Rectangle, w *WidgetDef, env WidgetEnv) []wthItem {
	d := env.Weather
	var out []wthItem
	text := func(x, y, s int, t string, c RGB) {
		if strings.TrimSpace(t) != "" {
			out = append(out, wthItem{Kind: "text", X: x, Y: y, S: s, Text: t, Color: c.hexStr()})
		}
	}
	centered := func(r image.Rectangle, y, s int, t string, c RGB) {
		x := r.Min.X + (r.Dx()-font.textWidth(t)*s)/2
		text(max(x, r.Min.X), y, s, t, c)
	}
	if d == nil || len(d.Days) == 0 {
		msg := wthNoData
		if d.failed() {
			msg = wthFailedData
		}
		s := max(min(fitScale([]string{msg}, area.Dx(), area.Dy()), 2), 1)
		centered(area, area.Min.Y+(area.Dy()-fontH*s)/2, s, todoEllipsis(msg, area.Dx()/s), colSub)
		return out
	}
	fs := 1
	if area.Dx() >= calFooterBig && area.Dy() >= calFooterBig {
		fs = 2
	}
	body := area
	body.Max.Y -= fontH*fs + widgetLineGap
	footer := true
	if body.Dy() < fontH { // 最終更新の行を置くと、予報が 1 行も入らない：最終更新の行を省く
		body, footer = area, false
	}
	now := env.Now.In(time.Local)
	today := dayStart(now)
	cols := []image.Rectangle{body}
	if body.Dx() >= 2*wthColMin+wthColGap {
		cw := (body.Dx() - wthColGap) / 2
		cols = []image.Rectangle{
			image.Rect(body.Min.X, body.Min.Y, body.Min.X+cw, body.Max.Y),
			image.Rect(body.Max.X-cw, body.Min.Y, body.Max.X, body.Max.Y),
		}
		out = append(out, wthItem{Kind: "rule", X: body.Min.X + cw + wthColGap/2 - 1, Y: body.Min.Y, W: 1, H: body.Dy(), Color: colTodoRule.hexStr()})
	}
	for i, c := range cols {
		date := today.AddDate(0, 0, i)
		label := "今日"
		if i == 1 {
			label = "明日"
		}
		out = append(out, wthColumn(c, label, date, d.day(date.Format(calDayLayout)), now, i == 0)...)
	}
	if footer {
		f, warn := wthFooter(w, d, env, area.Dx()/fs)
		fc := colSub
		if warn {
			fc = colCalWarn
		}
		text(area.Min.X, body.Max.Y+widgetLineGap, fs, f, fc)
	}
	return out
}

// wthIconBox は、列 c（行のときは 1 行の範囲）のアイコンの箱。列の大きさだけで決める。高さが wthIconMin より小さければ空。
// 返す mode は、line（1 行）、tall（縦長）、side（横に並べる）。
func wthIconBox(c image.Rectangle) (image.Rectangle, string, wthGeom) {
	cw, ch := c.Dx(), c.Dy()
	if ch < wthLineMode {
		ih := ch
		iw := ih * 3 / 2
		if iw > cw/3 {
			iw = cw / 3
			ih = iw * 2 / 3
			iw = ih * 3 / 2
		}
		if ih < wthIconMin {
			return image.Rectangle{}, "line", wthGeom{}
		}
		y := c.Min.Y + (ch-ih)/2
		return image.Rect(c.Min.X, y, c.Min.X+iw, y+ih), "line", wthGeom{}
	}
	g := wthColGeom(c)
	m := g.mid
	if m.Dy() >= cw*3/4 {
		sr := max(min(wthMaxScale, cw/wthRefW), 1)
		for sr > 1 && 2*fontH*sr+2 > m.Dy()/2 {
			sr--
		}
		textH := 2*fontH*sr + 2
		ih := min(m.Dy()-textH-2, cw*2/3)
		iw := ih * 3 / 2
		g.textH = textH
		if ih < wthIconMin {
			return image.Rectangle{}, "tall", g
		}
		top := m.Min.Y + (m.Dy()-(ih+2+textH))/2
		x := c.Min.X + (cw-iw)/2
		return image.Rect(x, top, x+iw, top+ih), "tall", g
	}
	iw := min(m.Dy()*3/2, cw/2)
	ih := iw * 2 / 3
	iw = ih * 3 / 2
	if ih < wthIconMin {
		return image.Rectangle{}, "side", g
	}
	y := m.Min.Y + (m.Dy()-ih)/2
	return image.Rect(c.Min.X, y, c.Min.X+iw, y+ih), "side", g
}

// wthGeom は、列の中の分け方。
type wthGeom struct {
	hs, rs, rows int             // 見出しの倍率、降水確率の倍率、降水確率の行数（0〜2）
	mid          image.Rectangle // 見出しと降水確率のあいだ
	textH        int             // 縦長の列で、文字に残す高さ
}

func wthColGeom(c image.Rectangle) wthGeom {
	cw, ch := c.Dx(), c.Dy()
	g := wthGeom{hs: 1}
	if cw >= calFooterBig && ch >= 200 {
		g.hs = 2
	}
	g.rs = g.hs
	for g.rs > 1 && font.textWidth("18-24")*g.rs > cw/4-2 {
		g.rs--
	}
	switch {
	case ch >= wthRainLabels:
		g.rows = 2
	case ch >= wthRainRow:
		g.rows = 1
	}
	rainH := 0
	if g.rows > 0 {
		rainH = g.rows*fontH*g.rs + (g.rows - 1) + 2
	}
	g.mid = image.Rect(c.Min.X, c.Min.Y+fontH*g.hs+2, c.Max.X, c.Max.Y-rainH)
	return g
}

// wthColumn は、1 日の列に描くもの。
func wthColumn(c image.Rectangle, label string, date time.Time, d *WeatherDay, now time.Time, isToday bool) []wthItem {
	var out []wthItem
	text := func(x, y, s int, t string, col RGB) {
		if strings.TrimSpace(t) != "" {
			out = append(out, wthItem{Kind: "text", X: x, Y: y, S: s, Text: t, Color: col.hexStr()})
		}
	}
	segs := func(x, y, s int, ss []wthSeg) {
		for _, g := range ss {
			text(x, y, s, g.text, g.c)
			x += font.textWidth(g.text) * s
		}
	}
	centered := func(r image.Rectangle, y, s int, t string, col RGB) {
		x := r.Min.X + (r.Dx()-font.textWidth(t)*s)/2
		text(max(x, r.Min.X), y, s, t, col)
	}
	cw, ch := c.Dx(), c.Dy()
	box, mode, g := wthIconBox(c)
	icon := func() {
		if d != nil && d.Icon != "" && !box.Empty() {
			out = append(out, wthItem{Kind: "icon", X: box.Min.X, Y: box.Min.Y, W: box.Dx(), H: box.Dy(), Icon: iconName(d.Icon, box.Size())})
		}
	}
	telop := wthNoDay
	if d != nil {
		telop = d.Telop
		if telop == "" {
			telop = wthNull
		}
	}
	if mode == "line" {
		x := c.Min.X
		if d != nil && !box.Empty() {
			icon()
			x = box.Max.X + 4
		}
		room := c.Max.X - x
		ts := []wthSeg{{telop, colText}}
		if d == nil {
			ts[0].c = colSub
		} else {
			ts = append(ts, wthSeg{" ", colText})
			ts = append(ts, wthTemps(d)...)
		}
		full := append([]wthSeg{{label + " ", colSub}}, ts...)
		for _, cand := range [][]wthSeg{full, ts} {
			s := min(wthMaxScale, max(ch/fontH, 1))
			for s > 1 && segsWidth(cand)*s > room {
				s--
			}
			if segsWidth(cand)*s <= room {
				segs(x, c.Min.Y+(ch-fontH*s)/2, s, cand)
				return out
			}
		}
		// 等倍でも入らなければ、天気の文を切る
		rest := room
		if d != nil {
			rest -= segsWidth(ts[1:])
		}
		ts[0].text = todoEllipsis(telop, max(rest, 0))
		segs(x, c.Min.Y+(ch-fontH)/2, 1, ts)
		return out
	}
	// 見出し
	head := label + " " + calDate(date)
	if font.textWidth(head)*g.hs > cw {
		head = label
	}
	text(c.Min.X, c.Min.Y, g.hs, head, colSub)
	// 降水確率（予報のない日は出さない）
	if g.rows > 0 && d != nil {
		y := c.Max.Y - (g.rows*fontH*g.rs + (g.rows - 1))
		for k := 0; k < 4; k++ {
			slot := image.Rect(c.Min.X+k*cw/4, y, c.Min.X+(k+1)*cw/4, c.Max.Y)
			past := isToday && !now.Before(date.Add(time.Duration(6*(k+1))*time.Hour))
			v := wthNull
			var p *int
			if d != nil {
				p = d.Rain[k]
			}
			if p != nil {
				v = strconv.Itoa(*p) + "%"
			}
			lc, vc := colSub, wthRainColor(p)
			if past {
				lc, vc = colTodoDone, colTodoDone
			}
			vy := y
			if g.rows == 2 {
				centered(slot, y, g.rs, wthRainSlots[k], lc)
				vy += fontH*g.rs + 1
			}
			centered(slot, vy, g.rs, v, vc)
		}
	}
	m := g.mid
	if d == nil {
		s := max(min(fitScale([]string{wthNoDay}, m.Dx(), m.Dy()), 2), 1)
		centered(m, m.Min.Y+(m.Dy()-fontH*s)/2, s, wthNoDay, colSub)
		return out
	}
	icon()
	temps := wthTemps(d)
	tw, pw := font.textWidth(telop), segsWidth(temps)
	widest := max(tw, pw, 1)
	if mode == "tall" {
		s := max(min(wthMaxScale, cw/widest), 1)
		for s > 1 && 2*fontH*s+2 > g.textH {
			s--
		}
		ih := box.Dy()
		top := m.Min.Y + (m.Dy()-(ih+2+g.textH))/2
		if box.Empty() {
			top = m.Min.Y + (m.Dy()-g.textH)/2
		}
		ty := top + ih + 2 + (g.textH-(2*fontH*s+2))/2
		if box.Empty() {
			ty = top + (g.textH-(2*fontH*s+2))/2
		}
		centered(c, ty, s, todoEllipsis(telop, cw/s), colText)
		px := c.Min.X + (cw-pw*s)/2
		segs(max(px, c.Min.X), ty+fontH*s+2, s, temps)
		return out
	}
	t := m
	if !box.Empty() {
		t.Min.X = box.Max.X + 4
	}
	s := max(min(wthMaxScale, t.Dx()/widest, (m.Dy()-2)/(2*fontH)), 1)
	ty := m.Min.Y + (m.Dy()-(2*fontH*s+2))/2
	centered(t, ty, s, todoEllipsis(telop, t.Dx()/s), colText)
	px := t.Min.X + (t.Dx()-pw*s)/2
	segs(max(px, t.Min.X), ty+fontH*s+2, s, temps)
	return out
}

// wthFooter は、最終更新の行の文と、橙色で出すかどうか。幅 maxW（等倍のドット）に入るように、発表の時刻、出典の順に省く。
func wthFooter(w *WidgetDef, d *WeatherData, env WidgetEnv, maxW int) (string, bool) {
	if !env.TimeSynced {
		return unsyncedText, true
	}
	now := env.Now.In(time.Local)
	when := func(t time.Time, hourOnly bool) string {
		t = t.In(time.Local)
		hm := calHM(t)
		if hourOnly && t.Minute() == 0 {
			hm = strconv.Itoa(t.Hour()) + "時"
		} else if t.Hour() < 10 {
			hm = hm[1:]
		}
		if dayStart(t).Equal(dayStart(now)) {
			return hm
		}
		return fmt.Sprintf("%d/%d %s", int(t.Month()), t.Day(), hm)
	}
	upd, warn := "更新 なし", true
	if d.FetchedAt != nil {
		upd, warn = "更新 "+when(*d.FetchedAt, false), false
		if w.Stale > 0 && !env.Now.Before(d.FetchedAt.Add(w.Stale)) {
			upd += " 古い"
			warn = true
		}
	}
	if d.failed() {
		upd += " 失敗"
		warn = true
	}
	pub := ""
	if d.PublicTime != nil {
		pub = when(*d.PublicTime, true) + "発表"
	}
	for _, parts := range [][]string{{upd, pub, wthSourceLong}, {upd, pub, wthSource}, {upd, wthSource}, {upd}} {
		var ps []string
		for _, p := range parts {
			if p != "" {
				ps = append(ps, p)
			}
		}
		s := strings.Join(ps, " ")
		if font.textWidth(s) <= maxW {
			return s, warn
		}
	}
	return todoEllipsis(upd, maxW), warn
}

// wthKey は、描き直すかどうかを決める中身。予報の版、今日の日付と時間帯、最終更新の行、アイコンの増減。
// 配置はセルの大きさと、これらで決まる。
func wthKey(w *WidgetDef, env WidgetEnv) string {
	d := env.Weather
	if d == nil {
		return "none"
	}
	now := env.Now.In(time.Local)
	f, warn := wthFooter(w, d, env, 1<<20)
	return fmt.Sprintf("%d\x00%s\x00%d\x00%s\x00%v\x00%d\x00%v", d.Rev, now.Format(calDayLayout), now.Hour()/6, f, warn,
		env.WeatherIcons.Gen(), d.failed())
}

// wthNext は、天気のセルの中身が次に変わる時刻（日付の変わり目、6 時間ごとの区切り、古くなる時刻）。
func wthNext(w *WidgetDef, env WidgetEnv) time.Time {
	now := env.Now.In(time.Local)
	t0 := dayStart(now)
	next := t0.AddDate(0, 0, 1)
	for k := 1; k < 4; k++ {
		if t := t0.Add(time.Duration(6*k) * time.Hour); t.After(now) && t.Before(next) {
			next = t
			break
		}
	}
	if d := env.Weather; d != nil && d.FetchedAt != nil && w.Stale > 0 {
		if t := d.FetchedAt.Add(w.Stale); t.After(env.Now) && t.Before(next) {
			next = t
		}
	}
	return next
}

// drawWeather は、天気のセルの中身を描く。area は見出しの下の範囲。
func drawWeather(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv) {
	for _, it := range wthLayout(area, w, env) {
		switch it.Kind {
		case "text":
			cv.text(it.X, it.Y, it.Text, it.S, parseHexColor(it.Color), area)
		case "rule":
			cv.fill(image.Rect(it.X, it.Y, it.X+it.W, it.Y+it.H), parseHexColor(it.Color))
		case "icon":
			if ic := env.WeatherIcons.Icon(it.Icon); ic != nil {
				cv.blendIcon(ic, image.Rect(it.X, it.Y, it.X+it.W, it.Y+it.H), area)
			}
		}
	}
}

// weatherCellArea は、セル（左上 col、row）の、見出しの下の範囲。
func weatherCellArea(l *Grid, p cellPos, a *Action, W, H int) image.Rectangle {
	box := cellRect(p.Col, p.Row, a.SpanW, a.SpanH, l.Cols, l.Rows, W, H).Inset(cellGap)
	_, _, area := widgetCaption(box.Inset(textMargin), a.Spec.Label)
	return area
}

// weatherIconBoxes は、今の設定の天気のセルで使うアイコンの箱の大きさ。
func weatherIconBoxes(km *Keymap, W, H int) map[image.Point]bool {
	out := map[image.Point]bool{}
	if km == nil {
		return out
	}
	// 配置は予報の中身によらないので、2 日分の仮の予報で並べる
	now := time.Now()
	t0 := dayStart(now.In(time.Local))
	fake := &WeatherData{Days: []WeatherDay{{Date: t0.Format(calDayLayout), Icon: "100"}, {Date: t0.AddDate(0, 0, 1).Format(calDayLayout), Icon: "100"}}}
	for _, l := range km.Layers {
		if l.Grid == nil {
			continue
		}
		for p, a := range l.Grid.Cells {
			if a.Widget == nil || a.Widget.Kind != widgetWeather {
				continue
			}
			area := weatherCellArea(l.Grid, p, a, W, H)
			for _, it := range wthLayout(area, a.Widget, WidgetEnv{Now: now, TimeSynced: true, Weather: fake}) {
				if it.Kind == "icon" {
					out[image.Pt(it.W, it.H)] = true
				}
			}
		}
	}
	return out
}

// weatherMissing は、予報の天気の番号と、今の設定の箱の大きさの組で、まだ Brain にないアイコン。
func weatherMissing(d *WeatherData, boxes map[image.Point]bool, icons *IconStore) []map[string]any {
	out := []map[string]any{}
	if d == nil {
		return out
	}
	seen := map[string]bool{}
	var bs []image.Point
	for b := range boxes {
		bs = append(bs, b)
	}
	// 大きい箱から（同じ順にそろえる）
	sortPoints(bs)
	for _, day := range d.Days {
		if day.Icon == "" {
			continue
		}
		for _, b := range bs {
			n := iconName(day.Icon, b)
			if seen[n] || icons.Has(n) {
				continue
			}
			seen[n] = true
			out = append(out, map[string]any{"name": n, "icon": day.Icon, "w": b.X, "h": b.Y})
		}
	}
	return out
}

func sortPoints(ps []image.Point) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && (ps[j].X > ps[j-1].X || (ps[j].X == ps[j-1].X && ps[j].Y > ps[j-1].Y)); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}
