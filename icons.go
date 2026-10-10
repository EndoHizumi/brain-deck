package main

import (
	"image"
	"sort"
	"strings"
)

// ---------- 組み込みのアイコン（ホーム画面） ----------
//
// このリポジトリのために描いた 16×16 のドット絵。角を丸めた色の板（tile）の上に、12×12 の絵（glyph）を重ねる。
// 絵の '.' は板の色、ほかの文字は iconPalette の色。板の外（角）は透明。
// 黒い背景でも壁紙の上でも見えるよう、どのアイコンも色の板を持つ。
// GUI（gui/src/icons.json）にも同じ表を置く。変えたら `LEFTHAND_UPDATE_ICONS=1 go test -run IconsJSON` で書き直す。

var iconPalette = map[byte]RGB{
	'w': {0xff, 0xff, 0xff}, // 白
	'k': {0x10, 0x14, 0x1a}, // 黒
	'g': {0xb0, 0xb8, 0xc4}, // 明るい灰色
	'd': {0x60, 0x6a, 0x78}, // 灰色
	'r': {0xff, 0x58, 0x58}, // 赤
	'o': {0xff, 0x90, 0x30}, // 橙
	'y': {0xff, 0xd0, 0x40}, // 黄
	'G': {0x3c, 0xb8, 0x6a}, // 緑
	'b': {0x3c, 0x88, 0xe8}, // 青
	'B': {0x1e, 0x4a, 0x7c}, // 紺
	'c': {0x30, 0xb0, 0xc0}, // 水色
	'p': {0x9a, 0x6a, 0xe8}, // 紫
	'n': {0xa0, 0x70, 0x40}, // 茶
	'P': {0xe8, 0x70, 0xa8}, // 桃
}

// iconArt は、アイコン 1 つ（板の色と、12×12 の絵）。
type iconArt struct {
	tile  byte
	glyph [12]string
}

var iconArts = map[string]iconArt{
	"clock": {'B', [12]string{
		"...wwwwww...",
		"..w......w..",
		".w...w....w.",
		"w....w.....w",
		"w....w.....w",
		"w....wwww..w",
		"w..........w",
		"w..........w",
		".w........w.",
		"..w......w..",
		"...wwwwww...",
		"............"}},
	"text": {'d', [12]string{
		"............",
		".wwwwwwwwww.",
		"............",
		".wwwwwwww...",
		"............",
		".wwwwwwwwww.",
		"............",
		".wwwwww.....",
		"............",
		".wwwwwwwww..",
		"............",
		"............"}},
	"todo": {'G', [12]string{
		"............",
		".www........",
		".wBw.wwwwww.",
		".www........",
		"............",
		".www........",
		".wBw.wwwwww.",
		".www........",
		"............",
		".www........",
		".w.w.wwwww..",
		".www........"}},
	"calendar": {'o', [12]string{
		"..w......w..",
		".wwwwwwwwww.",
		".wwwwwwwwww.",
		".w........w.",
		".w.ww.....w.",
		".w........w.",
		".w.wwwww..w.",
		".w........w.",
		".w.www....w.",
		".w........w.",
		".wwwwwwwwww.",
		"............"}},
	"month": {'r', [12]string{
		"..w......w..",
		".wwwwwwwwww.",
		".wwwwwwwwww.",
		"............",
		".w.w.w.w.w..",
		"............",
		".w.w.w.w.w..",
		"............",
		".w.w.w.y.w..",
		"............",
		".w.w.w......",
		"............"}},
	"weather": {'b', [12]string{
		".......y....",
		"...y..yyy...",
		"....yyyyy.y.",
		"...yyyyyyy..",
		".y..yyyyy...",
		"....wwyy....",
		"..wwwwww....",
		".wwwwwwwww..",
		"wwwwwwwwwwww",
		"wwwwwwwwwwww",
		".wwwwwwwwww.",
		"............"}},
	"graph": {'B', [12]string{
		"w...........",
		"w.........y.",
		"w........y..",
		"w..y....y...",
		"w.y.y..y....",
		"wy...yy.....",
		"w...........",
		"w..w..w..w..",
		"w..w..w..w..",
		"w..w..w..w..",
		"wwwwwwwwwwww",
		"............"}},
	"calculator": {'o', [12]string{
		".wwwwwwwwww.",
		".wkkkkkkkkw.",
		".wkkkkkkkkw.",
		".wwwwwwwwww.",
		"............",
		".ww.ww.ww...",
		".ww.ww.ww...",
		"............",
		".ww.ww.ww...",
		".ww.ww.ww...",
		"............",
		"............"}},
	"presenter": {'p', [12]string{
		"wwwwwwwwwwww",
		"w..........w",
		"w....w.....w",
		"w....ww....w",
		"w....www...w",
		"w....ww....w",
		"w....w.....w",
		"w..........w",
		"wwwwwwwwwwww",
		".....ww.....",
		"....wwww....",
		"............"}},
	"trackpad": {'d', [12]string{
		"............",
		".wwwwwwwwww.",
		".w........w.",
		".w........w.",
		".w....y...w.",
		".w........w.",
		".w........w.",
		".wwwwwwwwww.",
		".w....w...w.",
		".wwwwwwwwww.",
		"............",
		"............"}},
	"battery": {'G', [12]string{
		"............",
		"............",
		".wwwwwwwwww.",
		".w........ww",
		".w.yyyyy..ww",
		".w.yyyyy..ww",
		".w.yyyyy..ww",
		".w........ww",
		".wwwwwwwwww.",
		"............",
		"............",
		"............"}},
	"stopwatch": {'r', [12]string{
		"....wwww....",
		".....ww.....",
		"...wwwwww.w.",
		"..w......w..",
		".w...w....w.",
		".w...w....w.",
		".w...ww...w.",
		".w........w.",
		".w........w.",
		"..w......w..",
		"...wwwwww...",
		"............"}},
	"pomodoro": {'G', [12]string{
		"....wwww....",
		".....ww.....",
		"..rrrrrrrr..",
		".rrrrrrrrrr.",
		"rrrwrrrrrrrr",
		"rrwrrrrrrrrr",
		"rrrrrrrrrrrr",
		"rrrrrrrrrrrr",
		".rrrrrrrrrr.",
		"..rrrrrrrr..",
		"............",
		"............"}},
	"counter": {'b', [12]string{
		"............",
		"............",
		"..w......w..",
		"..w.....ww..",
		"wwwww....w..",
		"..w......w..",
		"..w......w..",
		".........w..",
		"........www.",
		"............",
		"............",
		"............"}},
	"qr": {'w', [12]string{
		"kkk.k...kkk.",
		"k.k..k..k.k.",
		"kkk.k.k.kkk.",
		"....kk......",
		"k.kk..k.k.k.",
		".k..kk.k..k.",
		"kk.k...kk...",
		"............",
		"kkk..k.k.k..",
		"k.k.k..kk...",
		"kkk..kk..k..",
		"............"}},
	"presence": {'G', [12]string{
		"....wwww....",
		"...wwwwww...",
		"...wwwwww...",
		"...wwwwww...",
		"....wwww....",
		"............",
		"..wwwwwwww..",
		".wwwwwwwwww.",
		".wwwwwwwwww.",
		".wwwwwwwwww.",
		"............",
		"............"}},
	"keyboard": {'B', [12]string{
		"............",
		"............",
		"wwwwwwwwwwww",
		"w..........w",
		"w.w.w.w.w..w",
		"w..........w",
		"w.w.w.w.w..w",
		"w..........w",
		"w..wwwwww..w",
		"w..........w",
		"wwwwwwwwwwww",
		"............"}},
	"terminal": {'d', [12]string{
		"kkkkkkkkkkkk",
		"kGkkkkkkkkkk",
		"kkGkkkkkkkkk",
		"kkkGkkkkkkkk",
		"kkGkkkkkkkkk",
		"kGkkkGGGGkkk",
		"kkkkkkkkkkkk",
		"kkkkkkkkkkkk",
		"kkkkkkkkkkkk",
		"kkkkkkkkkkkk",
		"kkkkkkkkkkkk",
		"kkkkkkkkkkkk"}},
	"settings": {'d', [12]string{
		".....ww.....",
		"..w..ww..w..",
		"...wwwwww...",
		"...ww..ww...",
		"wwww....wwww",
		"wwww....wwww",
		"...ww..ww...",
		"...wwwwww...",
		"..w..ww..w..",
		".....ww.....",
		"............",
		"............"}},
	"info": {'b', [12]string{
		".....ww.....",
		".....ww.....",
		"............",
		"....www.....",
		".....ww.....",
		".....ww.....",
		".....ww.....",
		".....ww.....",
		".....ww.....",
		"....wwww....",
		"............",
		"............"}},
	"memo": {'y', [12]string{
		"............",
		".dddddddddd.",
		"............",
		".dddddddd...",
		"............",
		".dddddddddd.",
		"............",
		".dddddd.....",
		"............",
		"..........r.",
		".........r..",
		"........r..."}},
	"home": {'o', [12]string{
		".....ww.....",
		"....wwww....",
		"...wwwwww...",
		"..wwwwwwww..",
		".wwwwwwwwww.",
		"wwwwwwwwwwww",
		".ww......ww.",
		".ww..ww..ww.",
		".ww..ww..ww.",
		".ww..ww..ww.",
		".wwwwwwwwww.",
		"............"}},
	"media": {'P', [12]string{
		".....wwwwwww",
		".....wwwwwww",
		".....w.....w",
		".....w.....w",
		".....w.....w",
		".....w.....w",
		"...www...www",
		"..wwww..wwww",
		"..wwww..wwww",
		"...ww....ww.",
		"............",
		"............"}},
	"mouse": {'c', [12]string{
		"....wwww....",
		"...w..w.w...",
		"..w...w..w..",
		"..w...w..w..",
		"..wwwwwwww..",
		"..w......w..",
		"..w......w..",
		"..w......w..",
		"..w......w..",
		"...w....w...",
		"....wwww....",
		"............"}},
	"code": {'p', [12]string{
		"............",
		"...w....w...",
		"..w......w..",
		"..w......w..",
		"..w......w..",
		".w........w.",
		"..w......w..",
		"..w......w..",
		"..w......w..",
		"...w....w...",
		"............",
		"............"}},
	"browser": {'b', [12]string{
		"...wwwwww...",
		"..w..w...w..",
		".w..w.....w.",
		"wwwwwwwwwwww",
		"w...w......w",
		"w...w......w",
		"wwwwwwwwwwww",
		".w..w.....w.",
		"..w..w...w..",
		"...wwwwww...",
		"............",
		"............"}},
	"folder": {'y', [12]string{
		"............",
		".nnnn.......",
		".nnnnnnnnnn.",
		".n........n.",
		".n........n.",
		".n........n.",
		".n........n.",
		".n........n.",
		".nnnnnnnnnn.",
		"............",
		"............",
		"............"}},
	"sync": {'G', [12]string{
		"...wwwwww...",
		"..w......w..",
		".w.......ww.",
		"w........www",
		"w...........",
		"w...........",
		"...........w",
		"...........w",
		"www........w",
		".ww.......w.",
		"..w......w..",
		"...wwwwww..."}},
	"app": {'d', [12]string{
		"............",
		".wwww..wwww.",
		".wwww..wwww.",
		".wwww..wwww.",
		".wwww..wwww.",
		"............",
		"............",
		".wwww..wwww.",
		".wwww..wwww.",
		".wwww..wwww.",
		".wwww..wwww.",
		"............"}},
	"window": {'B', [12]string{
		"wwwwwwwwwwww",
		"wwwwwwwwwwww",
		"w..........w",
		"w..........w",
		"w..........w",
		"w..........w",
		"w..........w",
		"w..........w",
		"w..........w",
		"wwwwwwwwwwww",
		"............",
		"............"}},
	"symbol": {'o', [12]string{
		"............",
		"...w...w....",
		"...w...w....",
		".wwwwwwwww..",
		"...w...w....",
		"...w...w....",
		".wwwwwwwww..",
		"...w...w....",
		"...w...w....",
		"............",
		"............",
		"............"}},
	"command": {'p', [12]string{
		".......ww...",
		"......ww....",
		".....ww.....",
		"....ww......",
		"...wwwwww...",
		"......ww....",
		".....ww.....",
		"....ww......",
		"...ww.......",
		"..w.........",
		"............",
		"............"}},
	"share": {'c', [12]string{
		".........ww.",
		"........wwww",
		".........ww.",
		".......w....",
		".ww...w.....",
		"wwww.w......",
		".ww...w.....",
		".......w....",
		".........ww.",
		"........wwww",
		".........ww.",
		"............"}},
	"rotate": {'b', [12]string{
		"....www.....",
		"...www......",
		"..wwwwwwww..",
		"...www...w..",
		"....www..w..",
		".........w..",
		"..w......w..",
		"..w..www....",
		"..w...www...",
		"..wwwwwwww..",
		"......www...",
		".....www...."}},
}

// builtinIcons は、組み立てた 16×16 のアイコン（行ごとの文字。' ' は透明）。
var builtinIcons = buildIcons()

func buildIcons() map[string][]string {
	out := map[string][]string{}
	for name, a := range iconArts {
		rows := make([]string, homeIconBase)
		for y := range homeIconBase {
			var b strings.Builder
			for x := range homeIconBase {
				// 角を丸める：1 行目と最後の行は両端 2 ドット、2 行目と最後から 2 行目は両端 1 ドットを透明に
				cut := 0
				switch y {
				case 0, homeIconBase - 1:
					cut = 2
				case 1, homeIconBase - 2:
					cut = 1
				}
				c := byte(' ')
				if x >= cut && x < homeIconBase-cut {
					c = a.tile
					if gx, gy := x-2, y-2; gx >= 0 && gx < 12 && gy >= 0 && gy < 12 && gx < len(a.glyph[gy]) && a.glyph[gy][gx] != '.' {
						c = a.glyph[gy][gx]
					}
				}
				b.WriteByte(c)
			}
			rows[y] = b.String()
		}
		out[name] = rows
	}
	return out
}

// iconNames は、組み込みのアイコンの名前（エラーの文に使う）。
func iconNames() string {
	names := make([]string, 0, len(builtinIcons))
	for n := range builtinIcons {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// drawIcon は、組み込みのアイコン name を (x, y) から s 倍で描く。clip の外には描かない。
func drawIcon(cv *Canvas, x, y, s int, name string, clip image.Rectangle) {
	rows := builtinIcons[name]
	if rows == nil {
		rows = builtinIcons["app"]
	}
	for gy, row := range rows {
		for gx := 0; gx < len(row); gx++ {
			c, ok := iconPalette[row[gx]]
			if !ok {
				continue
			}
			cv.fill(image.Rect(x+gx*s, y+gy*s, x+(gx+1)*s, y+(gy+1)*s).Intersect(clip), c)
		}
	}
}
