package main

import (
	"fmt"
	"image"
	"sort"
	"sync"
	"unicode/utf8"
)

// ---------- ホーム画面（home） ----------
//
// 設定に home を書いたときだけ使う。アプリ（入る先のレイヤー）のアイコンと名前を格子に並べた画面で、
// アイコンをタップするとそのレイヤーに移る（layer_to と同じ）。HOME のソフトキー（home.soft_key）は、
// どのレイヤーでもホーム画面に戻る（レイヤーの soft_keys の割り当てより優先する）。ホーム画面で押すと 1 ページ目に戻る。
//
// ホーム画面は、設定の home から作るレイヤー（名前は home）で、格子は 1×1、そのセルにアプリを並べるウィジェット（widgetHome）を置く。
// アプリが 1 ページに入りきらなければ、下の帯の ◀ ▶（と、画面右の帯の ◀ ▶）でページを送る。
// 1 ページの格子は、横向きでは home.cols×home.rows、縦向きでは home.portrait（省略すると列と行を入れ替えたもの）。
// 書かなければ今までどおり（base から始まり、HOME はレイヤーの soft_keys のとおり）。
// gui/src/home.ts も同じ配置で描く。

const (
	homeLayerName   = "home"
	homeLabel       = "ホーム"
	widgetHome      = "home"
	homeMaxApps     = 64
	homeNameMax     = 16 // アプリの名前の文字数
	homeNavH        = 40 // ページ送りの帯の高さ
	homeIconBase    = 16 // 組み込みのアイコンの大きさ（ドット）
	homeIconScale   = 4  // 組み込みのアイコンの最大の倍率（64×64）
	homeImageIcon   = 64 // 自分の画像のアイコンの大きさ（GUI はこの大きさに変換する）
	homeSlotPad     = 4
	homeNameGap     = 4
	homeDefaultCols = 4
	homeDefaultRows = 3
)

// HomeConfig は設定の home。
type HomeConfig struct {
	Cols     int       `yaml:"cols,omitempty" json:"cols,omitempty"` // 1 ページの列の数（横向き）。既定 4
	Rows     int       `yaml:"rows,omitempty" json:"rows,omitempty"` // 1 ページの行の数（横向き）。既定 3
	Portrait *HomeGrid `yaml:"portrait,omitempty" json:"portrait,omitempty"`
	Label    string    `yaml:"label,omitempty" json:"label,omitempty"`           // 帯と札に出す名前。既定「ホーム」
	Start    string    `yaml:"start,omitempty" json:"start,omitempty"`           // 起動したときの画面（home か base）。既定 home
	SoftKey  string    `yaml:"soft_key,omitempty" json:"soft_key,omitempty"`     // ホームに戻るソフトキーの区画。既定 home
	Bar      *bool     `yaml:"status_bar,omitempty" json:"status_bar,omitempty"` // 状態の帯を出すか。既定は出す
	Wall     string    `yaml:"background,omitempty" json:"background,omitempty"` // 壁紙（背景画像の id）
	Apps     []HomeApp `yaml:"apps" json:"apps"`
}

// HomeGrid は縦向きのときの 1 ページの格子。
type HomeGrid struct {
	Cols int `yaml:"cols" json:"cols"`
	Rows int `yaml:"rows" json:"rows"`
}

// HomeApp はホーム画面のアプリ 1 つ。
type HomeApp struct {
	Name  string `yaml:"name" json:"name"`
	Icon  string `yaml:"icon,omitempty" json:"icon,omitempty"` // 組み込みのアイコンの名前か、背景画像と同じ画像の id。省略すると app
	Layer string `yaml:"layer" json:"layer"`
}

// HomeDef は組み立てたホーム画面。
type HomeDef struct {
	Layer        int // ホーム画面のレイヤーの番号
	Apps         []HomeAppDef
	Cols, Rows   int // 横向きの 1 ページ
	PCols, PRows int // 縦向きの 1 ページ
	Start        bool
	SoftKey      string
	goHome       *Action // HOME のソフトキーの割り当て（layer_to home）

	mu   sync.Mutex
	page int
}

// HomeAppDef は組み立てたアプリ。
type HomeAppDef struct {
	Name  string
	Icon  string // 組み込みのアイコンの名前（Image がなければ）
	Image string // 画像の id
	Layer int
}

// grid は、向きに合った 1 ページの格子。
func (h *HomeDef) grid(portrait bool) (int, int) {
	if portrait {
		return h.PCols, h.PRows
	}
	return h.Cols, h.Rows
}

// Page は今のページ（0 から）。
func (h *HomeDef) Page() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.page
}

// SetPage はページを決める。
func (h *HomeDef) SetPage(p int) {
	h.mu.Lock()
	h.page = max(p, 0)
	h.mu.Unlock()
}

// compileHome は設定の home を検証し、ホーム画面のレイヤーを km.Layers のいちばん後ろに足す。
func compileHome(cfg *Config, km *Keymap, index map[string]int, fail func(path, format string, args ...any)) []string {
	hc := cfg.Home
	if hc == nil {
		return nil
	}
	if cfg.Touch == nil {
		fail("/home", "home needs the touch section (the home screen is drawn on the touch panel)")
		return nil
	}
	if _, dup := index[homeLayerName]; dup {
		fail("/home", "home: a layer named %q is reserved for the home screen; rename that layer", homeLayerName)
		return nil
	}
	h := &HomeDef{Layer: len(km.Layers), Cols: hc.Cols, Rows: hc.Rows, Start: true, SoftKey: hc.SoftKey}
	if h.Cols == 0 && h.Rows == 0 {
		h.Cols, h.Rows = homeDefaultCols, homeDefaultRows
	}
	if h.Cols < 1 || h.Rows < 1 || h.Cols > 8 || h.Rows > 8 {
		fail("/home", "home: cols and rows must be 1..8 (write both, or neither for 4x3)")
		h.Cols, h.Rows = homeDefaultCols, homeDefaultRows
	}
	h.PCols, h.PRows = h.Rows, h.Cols
	if p := hc.Portrait; p != nil {
		if p.Cols < 1 || p.Rows < 1 || p.Cols > 8 || p.Rows > 8 {
			fail("/home/portrait", "home.portrait: cols and rows must be 1..8")
		} else {
			h.PCols, h.PRows = p.Cols, p.Rows
		}
	}
	switch hc.Start {
	case "", "home":
	case "base":
		h.Start = false
	default:
		fail("/home/start", "home.start must be home or base")
	}
	if h.SoftKey == "" {
		h.SoftKey = "home"
	}
	if _, ok := km.area(h.SoftKey); !ok {
		fail("/home/soft_key", "home: soft key %q (the way back to the home screen) is not defined in touch.soft_areas", h.SoftKey)
	}
	if hc.Wall != "" && !validImageID(hc.Wall) {
		fail("/home/background", "home.background must be the 16-digit lowercase hex id of an image (as the settings GUI writes it), got %q", hc.Wall)
	}
	if len(hc.Apps) == 0 || len(hc.Apps) > homeMaxApps {
		fail("/home/apps", "home.apps must have 1..%d apps", homeMaxApps)
	}
	for i, a := range hc.Apps {
		path := fmt.Sprintf("/home/apps/%d", i)
		d := HomeAppDef{Name: a.Name, Icon: "app"}
		if a.Name == "" || utf8.RuneCountInString(a.Name) > homeNameMax {
			fail(path+"/name", "home.apps[%d]: name must be 1..%d characters", i, homeNameMax)
		}
		switch {
		case a.Icon == "":
		case validImageID(a.Icon):
			d.Image = a.Icon
		case builtinIcons[a.Icon] != nil:
			d.Icon = a.Icon
		default:
			fail(path+"/icon", "home.apps[%d]: icon must be a built-in icon (%s) or the id of an image, got %q", i, iconNames(), a.Icon)
		}
		li, ok := index[a.Layer]
		if !ok {
			fail(path+"/layer", "home.apps[%d] %q: unknown layer %q", i, a.Name, a.Layer)
		}
		d.Layer = li
		h.Apps = append(h.Apps, d)
	}
	h.goHome = &Action{Kind: actTo, Layer: h.Layer, SpanW: 1, SpanH: 1, Spec: ActionSpec{LayerTo: homeLayerName}}

	bar := true
	if hc.Bar != nil {
		bar = *hc.Bar
	}
	label := hc.Label
	if label == "" {
		label = homeLabel
	}
	w := &WidgetDef{Kind: widgetHome, home: h}
	cell := &Action{Kind: actWidget, Widget: w, SpanW: 1, SpanH: 1, Spec: ActionSpec{Widget: widgetHome}}
	g := &Grid{Cols: 1, Rows: 1, Cells: map[cellPos]*Action{{0, 0}: cell}, Background: hc.Wall}
	l := &Layer{Name: homeLayerName, Label: label, Keys: nil, Grid: g, PGrid: g, Soft: map[string]*Action{}, StatusBar: &bar, home: true}
	for name, delta := range map[string]int{"left": -1, "right": +1} {
		if _, ok := km.area(name); ok && name != h.SoftKey {
			l.Soft[name] = &Action{Kind: actHomePage, Rotate: delta, SpanW: 1, SpanH: 1}
		}
	}
	km.Layers = append(km.Layers, l)
	km.Home = h

	var warns []string
	for _, lc := range cfg.Layers {
		if _, ok := lc.SoftKeys[h.SoftKey]; ok {
			warns = append(warns, fmt.Sprintf("layer %q soft key %q: with home, this soft key always goes to the home screen; the assignment is not used", lc.Name, h.SoftKey))
		}
	}
	sort.Strings(warns)
	return warns
}

// ---------- 配置 ----------

// homeGeom は、ホーム画面の中の配置。
type homeGeom struct {
	slots       []image.Rectangle // 1 ページのアプリの場所（per 個）
	per, pages  int
	nav         image.Rectangle // ページ送りの帯。1 ページに入れば空
	prev, next  image.Rectangle
	mid         image.Rectangle
	page        int // 出すページ（範囲に収めたもの）
	first, last int // 出すアプリの番号の範囲 [first, last)
}

// homeGeometry は、area にアプリを並べる配置を決める。
func homeGeometry(area image.Rectangle, h *HomeDef, portrait bool, page int) homeGeom {
	cols, rows := h.grid(portrait)
	g := homeGeom{per: cols * rows}
	n := len(h.Apps)
	g.pages = max((n+g.per-1)/g.per, 1)
	g.page = min(max(page, 0), g.pages-1)
	icons := area
	if g.pages > 1 {
		icons.Max.Y -= homeNavH
		g.nav = image.Rect(area.Min.X, icons.Max.Y, area.Max.X, area.Max.Y)
		third := g.nav.Dx() / 3
		g.prev = image.Rect(g.nav.Min.X, g.nav.Min.Y, g.nav.Min.X+third, g.nav.Max.Y)
		g.next = image.Rect(g.nav.Max.X-third, g.nav.Min.Y, g.nav.Max.X, g.nav.Max.Y)
		g.mid = image.Rect(g.prev.Max.X, g.nav.Min.Y, g.next.Min.X, g.nav.Max.Y)
	}
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			x0, x1 := cellSpan(c, cols, icons.Dx())
			y0, y1 := cellSpan(r, rows, icons.Dy())
			g.slots = append(g.slots, image.Rect(icons.Min.X+x0, icons.Min.Y+y0, icons.Min.X+x1, icons.Min.Y+y1))
		}
	}
	g.first = g.page * g.per
	g.last = min(g.first+g.per, n)
	return g
}

// homeHit は、ホーム画面の点 pt がどのアプリか（app）、ページ送りか（delta）を返す。
func homeHit(g homeGeom, pt image.Point) (app, delta int) {
	switch {
	case pt.In(g.prev):
		return -1, -1
	case pt.In(g.next):
		return -1, +1
	case pt.In(g.nav):
		return -1, 0
	}
	for i, s := range g.slots {
		if pt.In(s) && g.first+i < g.last {
			return g.first + i, 0
		}
	}
	return -1, 0
}

// homeName は、アプリの名前を幅 w に入る倍率で返す。倍率 1 でも入らなければ、後ろを … で切る。
func homeName(name string, w int) (string, int) {
	if font.textWidth(name)*2 <= w {
		return name, 2
	}
	if font.textWidth(name) <= w {
		return name, 1
	}
	rs := []rune(name)
	for len(rs) > 0 && font.textWidth(string(rs)+textEllipsis) > w {
		rs = rs[:len(rs)-1]
	}
	return string(rs) + textEllipsis, 1
}

// homeIconScale は、組み込みのアイコンを box に入れる倍率（1〜homeIconScale）。
func homeIconFit(box image.Rectangle) int {
	return max(min(homeIconScale, box.Dx()/homeIconBase, box.Dy()/homeIconBase), 1)
}

var (
	colHomeNav    = RGB{0x14, 0x1c, 0x28}
	colHomeNavOff = colTodoOff
)

// drawHome は、ホーム画面を area に描く。背景（壁紙）は描いてあること。
func drawHome(cv *Canvas, l *Layout, area image.Rectangle, h *HomeDef) {
	g := homeGeometry(area, h, l.W < l.H, h.Page())
	for i := g.first; i < g.last; i++ {
		slot := g.slots[i-g.first].Inset(homeSlotPad)
		a := h.Apps[i]
		name, ns := homeName(a.Name, slot.Dx())
		nameH := fontH * ns
		img := l.image(a.Image)
		box := image.Rect(slot.Min.X, slot.Min.Y, slot.Max.X, slot.Max.Y-nameH-homeNameGap)
		iw, ih := homeImageIcon, homeImageIcon
		s := 0
		if img != nil {
			iw, ih = img.W, img.H
		} else {
			s = homeIconFit(box)
			iw, ih = homeIconBase*s, homeIconBase*s
		}
		total := ih + homeNameGap + nameH
		y := slot.Min.Y + (slot.Dy()-total)/2
		x := slot.Min.X + (slot.Dx()-iw)/2
		if img != nil {
			cv.blitImage(img, image.Rect(x, y, x+iw, y+ih), slot)
		} else {
			icon := a.Icon
			if a.Image != "" {
				icon = "app" // 画像が Brain にない
			}
			drawIcon(cv, x, y, s, icon, slot)
		}
		if l.hasWall() {
			cv.halo = &colHalo
		}
		drawCentered(cv, slot, y+ih+homeNameGap, name, ns, colText)
		cv.halo = nil
	}
	if g.nav.Empty() {
		return
	}
	cv.fill(g.nav, colHomeNav)
	cv.fill(image.Rect(g.nav.Min.X, g.nav.Min.Y, g.nav.Max.X, g.nav.Min.Y+1), colTodoRule)
	ty := g.nav.Min.Y + (g.nav.Dy()-fontH*2)/2 // ページ番号
	ink := func(ok bool) RGB {
		if ok {
			return colText
		}
		return colHomeNavOff
	}
	drawTriangle(cv, g.prev, -1, ink(g.page > 0))
	drawTriangle(cv, g.next, +1, ink(g.page < g.pages-1))
	drawCentered(cv, g.mid, ty, fmt.Sprintf("%d/%d", g.page+1, g.pages), 2, colSub)
}

// homeArrow は、ページ送りの三角の高さ（幅はその半分）。
const homeArrow = 20

// drawTriangle は、r の中央に、dir（-1 で左、+1 で右）を向いた三角を描く（フォントに ◀ ▶ がないため）。
func drawTriangle(cv *Canvas, r image.Rectangle, dir int, c RGB) {
	half := homeArrow / 2
	cx := r.Min.X + r.Dx()/2 - half/2
	cy := r.Min.Y + r.Dy()/2
	for i := 0; i < half; i++ {
		x := cx + i // 左向きなら、左端がとがる
		if dir > 0 {
			x = cx + half - 1 - i
		}
		cv.fill(image.Rect(x, cy-i-1, x+1, cy+i+1), c)
	}
}
