package main

import (
	"fmt"
	"image"
	"strings"
	"time"
)

// ---------- 状態の帯（status_bar） ----------
//
// 画面の上に、今のレイヤーの名前と、時刻、電池、PC とのつながり、マウスの形、最後に同期した時刻を出す細い帯。
// 帯を出したレイヤーでは、格子は帯の下の範囲に並ぶ（セルがその分だけ小さくなる）。
// 出すかどうかはレイヤーごとの status_bar で決め、書いていないレイヤーは下のレイヤー、なければ status_bar.show。
// ホーム画面は home.status_bar（既定は出す）。帯を出すあいだは、右上のレイヤーの札の代わりに、帯の左に名前を出す。
// gui/src/statusbar.ts も同じ配置で描く。

const (
	barH      = 28 // 帯の高さ（倍率 2 の文字 + 上下 2 ドット）
	barScale  = 2
	barPad    = 6  // 左右の端と、名前の札の内側
	barGap    = 12 // 項目のあいだ
	barTextY  = 2
	barBattW  = 22 // 電池の絵の幅（端子を除く）
	barBattH  = 14
	barBattTo = 3 // 電池の絵の右の端子の幅

	barTime    = "time"
	barBattery = "battery"
	barPC      = "pc"
	barMouse   = "mouse"
	barSync    = "sync"
)

// barItemsDefault は、items を省略したときの項目（左から）。
var barItemsDefault = []string{barSync, barMouse, barPC, barBattery, barTime}

var barItemNames = map[string]bool{barTime: true, barBattery: true, barPC: true, barMouse: true, barSync: true}

var (
	colBar     = RGB{0x14, 0x1c, 0x28}
	colBarRule = RGB{0x30, 0x3c, 0x4c}
	colBarDim  = RGB{0x6a, 0x74, 0x80}
)

// StatusBarConfig は設定の status_bar。
type StatusBarConfig struct {
	// Show は、status_bar を書いていないレイヤーで帯を出すか（既定は出さない）
	Show *bool `yaml:"show,omitempty" json:"show,omitempty"`
	// Items は帯の右に並べる項目（左から）。省略すると sync、mouse、pc、battery、time
	Items []string `yaml:"items,omitempty" json:"items,omitempty"`
}

// BarDef は組み立てた status_bar。
type BarDef struct {
	Show  bool
	Items []string
}

func (c *StatusBarConfig) validate(fail func(path, format string, args ...any)) {
	if c == nil {
		return
	}
	seen := map[string]bool{}
	for i, it := range c.Items {
		if !barItemNames[it] || seen[it] {
			fail(fmt.Sprintf("/status_bar/items/%d", i), "status_bar.items must be distinct names from time, battery, pc, mouse and sync, got %q", it)
		}
		seen[it] = true
	}
}

func (c *StatusBarConfig) compile() BarDef {
	d := BarDef{Items: barItemsDefault}
	if c == nil {
		return d
	}
	if c.Show != nil {
		d.Show = *c.Show
	}
	if c.Items != nil {
		d.Items = c.Items
	}
	return d
}

// barItem は、帯に出す項目 1 つ。
type barItem struct {
	kind string
	text string
	ink  RGB
	batt *BatteryInfo // battery：絵を描く
}

// linkText は、PC とのつながり（UDC の state）を帯に出す文と色にする。分からなければ空。
func linkText(state string) (string, RGB) {
	switch state {
	case "":
		return "", colText
	case "configured":
		return "PC", colInkOK
	case "suspended":
		return "PC休止", colBarDim
	}
	return "PCなし", colUnsynced
}

// barItems は、今の状態から帯の項目を作る。
func barItems(items []string, env WidgetEnv) []barItem {
	var out []barItem
	for _, k := range items {
		switch k {
		case barTime:
			ink := colText
			if !env.TimeSynced {
				ink = colUnsynced
			}
			out = append(out, barItem{kind: k, text: env.Now.Format("15:04"), ink: ink})
		case barBattery:
			b := env.Battery
			if b == nil || b.Percent < 0 {
				out = append(out, barItem{kind: k, text: "電池?", ink: colBarDim})
				continue
			}
			ink := colText
			switch {
			case b.Plugged:
				ink = colInkOK
			case b.Percent <= 10:
				ink = colInkError
			case b.Percent <= 25:
				ink = colInkWarn
			}
			out = append(out, barItem{kind: k, text: fmt.Sprintf("%d%%", b.Percent), ink: ink, batt: b})
		case barPC:
			if s, c := linkText(env.Link); s != "" {
				out = append(out, barItem{kind: k, text: s, ink: c})
			}
		case barMouse:
			if !env.MouseOff {
				out = append(out, barItem{kind: k, text: "マウス", ink: colText})
			}
		case barSync:
			if env.LastSync == nil {
				out = append(out, barItem{kind: k, text: "未同期", ink: colUnsynced})
				continue
			}
			t := env.LastSync.In(env.Now.Location())
			s := t.Format("15:04")
			if y1, m1, d1 := t.Date(); y1 != env.Now.Year() || m1 != env.Now.Month() || d1 != env.Now.Day() {
				s = t.Format("1/2")
			}
			out = append(out, barItem{kind: k, text: "同期" + s, ink: colBarDim})
		}
	}
	return out
}

// width は、項目の幅（ドット）。
func (it barItem) width() int {
	w := font.textWidth(it.text) * barScale
	if it.batt != nil {
		w += barBattW + barBattTo + 4
	}
	return w
}

// barKey は、帯に描く中身を表す文字列。前に描いたものと違えば描き直す。
func barKey(l *Layout, env WidgetEnv) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%d", l.Title, l.Mode)
	for _, it := range barItems(l.BarItems, env) {
		fmt.Fprintf(&b, "\x00%s:%v", it.text, it.ink)
		if it.batt != nil {
			fmt.Fprintf(&b, ":%d", it.batt.Percent)
		}
	}
	return b.String()
}

// barNext は、帯の中身が次に変わる時刻（時刻か同期の項目があれば、次の分）。
func barNext(l *Layout, env WidgetEnv) time.Time {
	for _, k := range l.BarItems {
		if k == barTime || k == barSync {
			return env.Now.Truncate(time.Minute).Add(time.Minute)
		}
	}
	return time.Time{}
}

// barRect は帯の範囲。
func (l *Layout) barRect() image.Rectangle { return image.Rect(0, 0, l.W, barH) }

// drawBar は、状態の帯を描き、その範囲を返す。
func drawBar(cv *Canvas, l *Layout, env WidgetEnv) image.Rectangle {
	r := l.barRect()
	cv.fill(r, colBar)
	cv.fill(image.Rect(0, r.Max.Y-1, r.Max.X, r.Max.Y), colBarRule)
	items := barItems(l.BarItems, env)
	x := l.W - barPad
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		x -= it.width()
		drawBarItem(cv, x, it)
		x -= barGap
	}
	// 左に今のレイヤーの名前。入りきらなければ後ろを切る
	title := l.Title
	room := x - 2*barPad
	for title != "" && font.textWidth(title)*barScale > room {
		rs := []rune(title)
		title = string(rs[:len(rs)-1])
	}
	if title != "" {
		tw := font.textWidth(title)*barScale + 2*barPad
		b := image.Rect(0, 0, tw, barH-1)
		cv.fill(b, modeBadge[l.Mode])
		cv.text(barPad, barTextY, title, barScale, modeBadgeText[l.Mode], b)
	}
	return r
}

// drawBarItem は、項目 1 つを x から描く。
func drawBarItem(cv *Canvas, x int, it barItem) {
	if b := it.batt; b != nil {
		y := (barH - 1 - barBattH) / 2
		box := image.Rect(x, y, x+barBattW, y+barBattH)
		cv.frame(box, 2, it.ink)
		cv.fill(image.Rect(box.Max.X, y+4, box.Max.X+barBattTo, box.Max.Y-4), it.ink)
		in := box.Inset(3)
		w := in.Dx() * min(max(b.Percent, 0), 100) / 100
		if b.Percent > 0 {
			w = max(w, 1)
		}
		cv.fill(image.Rect(in.Min.X, in.Min.Y, in.Min.X+w, in.Max.Y), it.ink)
		x += barBattW + barBattTo + 4
	}
	cv.text(x, barTextY, it.text, barScale, it.ink, image.Rect(x, 0, x+font.textWidth(it.text)*barScale, barH))
}
