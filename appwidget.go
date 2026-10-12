package main

import (
	"fmt"
	"image"
	"sort"
	"strconv"
	"strings"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 自分で足すアプリ（フェーズ K5）：セル（widget: app）と、出力の決まり ----------
//
// セルに `{ widget: app, name: <名前> }` と書くと、/var/lib/lefthand/apps/<名前>/ のスクリプトの出力を描く。
// 出力は 1 行ずつ。@ で始まらない行は、そのまま文字の行（長ければ折り返す）。@ で始まる行は指示：
//
//	@title 見出し            セルの上の見出し（設定の label があれば label）
//	@big 42%                大きな文字の行
//	@color red              これより後の行の色（red orange yellow green blue gray white か #rrggbb。@color だけで元に戻す）
//	@bar 75 [100] [文字]     進み具合の帯（値、最大（既定 100）、帯に重ねる文字）
//	@row 左 | 右             左と右に分けた行（右は右に寄せる）
//	@item <id> 文字          タップできる行。タップすると「item <id>」
//	@sep                    区切りの線
//	@button <id> 文字        下に並ぶボタン（6 つまで）。押すと「button <id>」
//	@input <id> 文字 [| 欄の見出し]   文字を入れるボタン。入れて決定すると「input <id>」（入れた文字は標準入力）
//	@clear                  （stream）ここから新しい画面
//	@@…                     @ で始まる文字の行（@ を 1 つ取る）
//
// スクリプトに渡すもの（interval は引数と標準入力、stream は標準入力の 1 行）：
// 時間で動かすときは何もなし、セルのほかの所をタップ「tap」、ボタン「button <id>」、行「item <id>」、
// 本体キー（app.yaml の keys）「key <名前>」、文字「input <id>」（文字は interval なら標準入力、stream なら同じ行の後ろ）。

const (
	widgetApp = "app"

	appButtonsMax = 6
	appButtonH    = 40
	appItemPad    = 3
	appSepH       = 7
	appLineGap    = 2
	appStatusH    = fontH + 4
)

// appKeyCodes は、app.yaml の keys に書ける名前。
var appKeyCodes = func() map[string]evdev.EvCode {
	m := map[string]evdev.EvCode{
		"left": evdev.KEY_LEFT, "right": evdev.KEY_RIGHT, "up": evdev.KEY_UP, "down": evdev.KEY_DOWN,
		"enter": evdev.KEY_ENTER, "space": evdev.KEY_SPACE, "backspace": evdev.KEY_BACKSPACE, "tab": evdev.KEY_TAB,
		"pageup": evdev.KEY_PAGEUP, "pagedown": evdev.KEY_PAGEDOWN, "home": evdev.KEY_HOME, "end": evdev.KEY_END,
		"delete": evdev.KEY_DELETE, "minus": evdev.KEY_MINUS, "equal": evdev.KEY_EQUAL, "comma": evdev.KEY_COMMA, "dot": evdev.KEY_DOT,
	}
	// 文字と数字のキー（evdev の番号は並んでいないので、名前で引く）
	for name, code := range evdev.KEYFromString {
		if n, ok := strings.CutPrefix(name, "KEY_"); ok && len(n) == 1 {
			if c := n[0]; (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				m[strings.ToLower(n)] = code
			}
		}
	}
	return m
}()

func appKeyNames() []string {
	var out []string
	for k := range appKeyCodes {
		if len(k) > 1 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return append(out, "a〜z", "0〜9")
}

// appKeyName は、キーの名前（keys にないキーなら空）。
func appKeyName(code evdev.EvCode) string {
	for k, c := range appKeyCodes {
		if c == code {
			return k
		}
	}
	return ""
}

// ---------- 出力の決まり ----------

// AppLine は、画面の 1 行。
type AppLine struct {
	Kind     byte // 't' 文字、'b' 大きな文字、'r' 左右、'i' タップできる行、's' 区切り、'p' 進み具合の帯
	Text     string
	Right    string
	ID       string
	Ink      RGB
	HasInk   bool
	Value    float64
	Max      float64
	Unparsed bool
}

// AppButton は、下のボタン。
type AppButton struct {
	ID, Label string
	Input     bool
	Prompt    string
}

// AppScreen は、出力から作った画面。
type AppScreen struct {
	Title   string
	Lines   []AppLine
	Buttons []AppButton
	ink     RGB
	hasInk  bool
}

var appColors = map[string]RGB{"red": colInkError, "orange": colUnsynced, "yellow": colInkWarn, "green": colInkOK,
	"blue": {0x4f, 0x9d, 0xff}, "gray": colSub, "grey": colSub, "white": colText}

// parseAppColor は、色の名前か #rrggbb。
func parseAppColor(s string) (RGB, bool) {
	if c, ok := appColors[strings.ToLower(s)]; ok {
		return c, true
	}
	if len(s) == 7 && s[0] == '#' {
		v, err := strconv.ParseUint(s[1:], 16, 32)
		if err == nil {
			return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}, true
		}
	}
	return RGB{}, false
}

// parseAppOutput は、interval のスクリプトの出力を画面にする。
func parseAppOutput(b []byte) *AppScreen {
	sc := &AppScreen{}
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if len(ln) > appMaxLineBytes {
			ln = ln[:appMaxLineBytes]
		}
		if strings.TrimSpace(ln) == "@clear" {
			*sc = AppScreen{}
			continue
		}
		sc.add(ln)
	}
	return sc
}

// copy は、描くための写し（stream は作りながら描くので）。
func (sc *AppScreen) copy() *AppScreen {
	c := *sc
	c.Lines = append([]AppLine(nil), sc.Lines...)
	c.Buttons = append([]AppButton(nil), sc.Buttons...)
	return &c
}

// add は、出力の 1 行を足す。
func (sc *AppScreen) add(ln string) {
	ln = strings.TrimRight(ln, "\r")
	ln = strings.Map(func(r rune) rune {
		if r < ' ' && r != '\t' {
			return -1
		}
		if r == '\t' {
			return ' '
		}
		return r
	}, ln)
	text := func(kind byte, t string) {
		if len(sc.Lines) >= appMaxScreen {
			copy(sc.Lines, sc.Lines[1:]) // stream は最後の行を残す
			sc.Lines = sc.Lines[:appMaxScreen-1]
		}
		sc.Lines = append(sc.Lines, AppLine{Kind: kind, Text: t, Ink: sc.ink, HasInk: sc.hasInk})
	}
	if !strings.HasPrefix(ln, "@") {
		text('t', ln)
		return
	}
	if strings.HasPrefix(ln, "@@") {
		text('t', ln[1:])
		return
	}
	cmd, rest, _ := strings.Cut(ln[1:], " ")
	rest = strings.TrimSpace(rest)
	switch cmd {
	case "title":
		sc.Title = rest
	case "big":
		text('b', rest)
	case "color":
		if rest == "" {
			sc.hasInk = false
		} else if c, ok := parseAppColor(rest); ok {
			sc.ink, sc.hasInk = c, true
		}
	case "row":
		l, r, _ := strings.Cut(rest, "|")
		text('r', strings.TrimSpace(l))
		sc.Lines[len(sc.Lines)-1].Right = strings.TrimSpace(r)
	case "item":
		id, t, _ := strings.Cut(rest, " ")
		text('i', strings.TrimSpace(t))
		sc.Lines[len(sc.Lines)-1].ID = id
	case "sep":
		text('s', "")
	case "bar":
		fs := strings.Fields(rest)
		v, max := 0.0, 100.0
		label := ""
		if len(fs) > 0 {
			v, _ = strconv.ParseFloat(fs[0], 64)
			fs = fs[1:]
		}
		if len(fs) > 0 {
			if m, err := strconv.ParseFloat(fs[0], 64); err == nil {
				max = m
				fs = fs[1:]
			}
		}
		label = strings.Join(fs, " ")
		text('p', label)
		l := &sc.Lines[len(sc.Lines)-1]
		l.Value, l.Max = v, max
	case "button", "input":
		if len(sc.Buttons) >= appButtonsMax {
			return
		}
		id, t, _ := strings.Cut(rest, " ")
		t, prompt, _ := strings.Cut(t, "|")
		b := AppButton{ID: id, Label: strings.TrimSpace(t), Input: cmd == "input", Prompt: strings.TrimSpace(prompt)}
		if b.Label == "" {
			b.Label = id
		}
		if b.Prompt == "" {
			b.Prompt = b.Label
		}
		if id != "" {
			sc.Buttons = append(sc.Buttons, b)
		}
	default:
		text('t', ln) // 知らない指示は、そのまま文字で出す（書き間違いに気づけるように）
		sc.Lines[len(sc.Lines)-1].Unparsed = true
	}
}

// ---------- 設定 ----------

func compileApp(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: app handles taps itself (they go to the script); remove key and layer_*")
	}
	if !appNameRE.MatchString(s.Name) {
		return nil, fmt.Errorf("widget: app needs name (the folder in /var/lib/lefthand/apps: 1-32 characters of a-z 0-9 _ -), got %q", s.Name)
	}
	return &WidgetDef{Kind: widgetApp, ID: s.Name}, nil
}

// appViewOf は、描くときのアプリの写し。
func appViewOf(w *WidgetDef, env WidgetEnv) *AppView {
	if env.AppViews != nil {
		return env.AppViews[w.ID]
	}
	if env.Apps != nil {
		return env.Apps.View(w.ID)
	}
	return nil
}

func appKey(w *WidgetDef, env WidgetEnv) string {
	if env.Apps != nil {
		env.Apps.Seen(w.ID) // 画面にあるあいだだけ動かす
	}
	v := appViewOf(w, env)
	if v == nil {
		return "app\x00none"
	}
	return fmt.Sprintf("app\x00%s\x00%d\x00%v\x00%s\x00%p", v.Bad, v.Gen, v.Running, v.Status, v.Def)
}

// appNext は、画面にあることを 2 秒ごとに知らせる（描画の goroutine が起きて、appKey を呼ぶ）。
func appNext(env WidgetEnv) time.Time {
	if env.Apps == nil {
		return time.Time{}
	}
	return env.Now.Add(2 * time.Second)
}

// appCaption は、セルの上の見出し。設定の label、出力の @title、app.yaml の title の順。
func appCaption(w *WidgetDef, label string, env WidgetEnv) string {
	if label != "" {
		return label
	}
	if v := appViewOf(w, env); v != nil {
		if v.Screen != nil && v.Screen.Title != "" {
			return v.Screen.Title
		}
		if v.Def != nil {
			return v.Def.Title
		}
	}
	return ""
}

// appGeom は、セルの中の配置。
type appGeom struct {
	scale   int
	body    image.Rectangle
	status  image.Rectangle
	buttons []image.Rectangle
	lines   []appPlaced // 描く行（はみ出すものは入れない）
	more    bool        // 入りきらない行がある
}

type appPlaced struct {
	line  AppLine
	text  string // 折り返したあとの 1 行
	r     image.Rectangle
	scale int
}

// appLayout は、範囲 area に、画面 sc と知らせ status を並べる。tail なら最後の行を残す（stream）。
func appLayout(area image.Rectangle, sc *AppScreen, status string, tail bool) appGeom {
	g := appGeom{scale: 1, body: area}
	if area.Dx() >= 300 && area.Dy() >= 120 {
		g.scale = 2
	}
	if status != "" {
		g.status = image.Rect(area.Min.X, area.Max.Y-appStatusH, area.Max.X, area.Max.Y)
		g.body.Max.Y = g.status.Min.Y
	}
	if sc != nil && len(sc.Buttons) > 0 {
		bh := min(appButtonH, g.body.Dy()/3)
		row := image.Rect(g.body.Min.X, g.body.Max.Y-bh, g.body.Max.X, g.body.Max.Y)
		g.body.Max.Y = row.Min.Y - appLineGap
		n := len(sc.Buttons)
		for i := range sc.Buttons {
			x0 := row.Min.X + row.Dx()*i/n
			x1 := row.Min.X + row.Dx()*(i+1)/n
			g.buttons = append(g.buttons, image.Rect(x0+2, row.Min.Y, x1-2, row.Max.Y))
		}
	}
	if sc == nil {
		return g
	}
	// 行を並べる（折り返し、行ごとの高さ）
	var placed []appPlaced
	for _, l := range sc.Lines {
		s := g.scale
		switch l.Kind {
		case 'b':
			s = max(fitScaleMax([]string{l.Text}, g.body.Dx(), fontH*g.scale*3, g.scale*3), 1) // 幅に入る大きさ（ふつうの 3 倍まで）
			placed = append(placed, appPlaced{line: l, text: l.Text, scale: s, r: image.Rect(0, 0, g.body.Dx(), fontH*s)})
		case 's':
			placed = append(placed, appPlaced{line: l, scale: s, r: image.Rect(0, 0, g.body.Dx(), appSepH)})
		case 'i':
			placed = append(placed, appPlaced{line: l, text: l.Text, scale: s, r: image.Rect(0, 0, g.body.Dx(), fontH*s+2*appItemPad)})
		case 'p':
			placed = append(placed, appPlaced{line: l, text: l.Text, scale: s, r: image.Rect(0, 0, g.body.Dx(), fontH*s+4)})
		case 'r':
			placed = append(placed, appPlaced{line: l, text: l.Text, scale: s, r: image.Rect(0, 0, g.body.Dx(), fontH*s)})
		default:
			for _, t := range wrapText([]string{l.Text}, g.body.Dx()/s) {
				placed = append(placed, appPlaced{line: l, text: t, scale: s, r: image.Rect(0, 0, g.body.Dx(), fontH*s)})
			}
		}
	}
	total := 0
	for _, p := range placed {
		total += p.r.Dy() + appLineGap
	}
	from := 0
	if tail {
		for h := total; h > g.body.Dy() && from < len(placed); from++ {
			h -= placed[from].r.Dy() + appLineGap
		}
	}
	y := g.body.Min.Y
	for i := from; i < len(placed); i++ {
		p := placed[i]
		if y+p.r.Dy() > g.body.Max.Y {
			g.more = true
			break
		}
		p.r = p.r.Add(image.Pt(g.body.Min.X, y))
		g.lines = append(g.lines, p)
		y += p.r.Dy() + appLineGap
	}
	if from > 0 {
		g.more = true
	}
	return g
}

func drawApp(cv *Canvas, area image.Rectangle, w *WidgetDef, env WidgetEnv) {
	v := appViewOf(w, env)
	switch {
	case v == nil:
		drawAppPlaceholder(cv, area, []string{"アプリ：" + w.ID, "スクリプトの出力をここに出す"})
		return
	case v.Def == nil && v.Bad != "":
		drawAppPlaceholder(cv, area, []string{"アプリ：" + w.ID, "使えません"})
		drawAppStatus(cv, image.Rect(area.Min.X, area.Max.Y-appStatusH, area.Max.X, area.Max.Y), v.Bad, noticeError)
		return
	case v.Def == nil:
		drawAppPlaceholder(cv, area, []string{"アプリ：" + w.ID, "置かれていません（brain-deck app install）"})
		return
	case v.Screen == nil:
		msg := "まだ動かしていません"
		if v.Running {
			msg = "動かしています…"
		}
		drawAppPlaceholder(cv, area, []string{v.Def.Title, msg})
		if v.Status != "" {
			drawAppStatus(cv, image.Rect(area.Min.X, area.Max.Y-appStatusH, area.Max.X, area.Max.Y), v.Status, v.Level)
		}
		return
	}
	g := appLayout(area, v.Screen, v.Status, v.Def.Stream)
	for _, p := range g.lines {
		ink := colText
		if p.line.HasInk {
			ink = p.line.Ink
		}
		if p.line.Unparsed {
			ink = colInkWarn
		}
		r := p.r
		switch p.line.Kind {
		case 's':
			cv.fill(image.Rect(r.Min.X, r.Min.Y+appSepH/2-1, r.Max.X, r.Min.Y+appSepH/2+1), colBarRule)
		case 'i':
			cv.fill(r, colReadRow)
			cv.text(r.Min.X+4, r.Min.Y+appItemPad, clipText(p.text, r.Dx()-8, p.scale), p.scale, ink, r)
		case 'r':
			rw := font.textWidth(p.line.Right) * p.scale
			cv.text(r.Max.X-rw, r.Min.Y, p.line.Right, p.scale, ink, r)
			cv.text(r.Min.X, r.Min.Y, clipText(p.text, max(r.Dx()-rw-8, 0), p.scale), p.scale, ink, r)
		case 'p':
			cv.frame(r, 1, colSub)
			in := r.Inset(2)
			frac := 0.0
			if p.line.Max > 0 {
				frac = min(max(p.line.Value/p.line.Max, 0), 1)
			}
			c := colInkOK
			if p.line.HasInk {
				c = p.line.Ink
			}
			cv.fill(image.Rect(in.Min.X, in.Min.Y, in.Min.X+int(float64(in.Dx())*frac+0.5), in.Max.Y), c)
			if p.text != "" {
				s := p.scale
				t := clipText(p.text, r.Dx()-8, s)
				cv.text(r.Min.X+(r.Dx()-font.textWidth(t)*s)/2, r.Min.Y+(r.Dy()-fontH*s)/2, t, s, colText, r)
			}
		case 'b':
			drawCentered(cv, r, r.Min.Y, p.text, p.scale, ink)
		default:
			cv.text(r.Min.X, r.Min.Y, p.text, p.scale, ink, r)
		}
	}
	if g.more {
		cv.text(g.body.Max.X-font.textWidth("…")*g.scale, g.body.Max.Y-fontH*g.scale, "…", g.scale, colSub, g.body)
	}
	for i, b := range v.Screen.Buttons {
		r := g.buttons[i]
		cv.fill(r, colReadRow)
		cv.frame(r, 1, colBarRule)
		s := max(min(fitScaleMax([]string{b.Label}, r.Dx()-6, r.Dy()-4, 2), 2), 1)
		drawCentered(cv, r, r.Min.Y+(r.Dy()-fontH*s)/2, clipText(b.Label, r.Dx()-6, s), s, colText)
	}
	if v.Status != "" {
		drawAppStatus(cv, g.status, v.Status, v.Level)
	}
}

func drawAppStatus(cv *Canvas, r image.Rectangle, msg string, level int) {
	cv.fill(r, colReadBar)
	cv.text(r.Min.X+4, r.Min.Y+2, clipText(msg, r.Dx()-8, 1), 1, noticeInk(level), r)
}

// ---------- タップとキー ----------

// appTouch は、アプリのセルの pt をタップしたとき（入力の goroutine。待たずに返る）。
func (rt *WidgetRT) appTouch(w *WidgetDef, t widgetTouch, label string) {
	if rt == nil || rt.apps == nil {
		return
	}
	env := rt.touchEnv()
	env.Apps = rt.apps
	v := rt.apps.View(w.ID)
	if v == nil || v.Def == nil {
		return
	}
	area := widgetArea(t.cell, appCaption(w, label, env))
	if v.Screen != nil {
		g := appLayout(area, v.Screen, v.Status, v.Def.Stream)
		for i, r := range g.buttons {
			if t.pt.In(r) {
				b := v.Screen.Buttons[i]
				if b.Input {
					go rt.apps.openInput(w.ID, b.ID, b.Prompt)
				} else {
					rt.apps.Event(w.ID, appEvent{args: []string{"button", b.ID}})
				}
				return
			}
		}
		for _, p := range g.lines {
			if p.line.Kind == 'i' && p.line.ID != "" && t.pt.In(p.r) {
				rt.apps.Event(w.ID, appEvent{args: []string{"item", p.line.ID}})
				return
			}
		}
	}
	rt.apps.Event(w.ID, appEvent{args: []string{"tap"}})
}

// appKeyLocked は、今の画面にアプリのセルがあり、そのアプリが受け取るキー（app.yaml の keys）なら、スクリプトに渡す。
func (e *Engine) appKeyLocked(code evdev.EvCode) bool {
	if e.widgets == nil || e.widgets.apps == nil || e.view == nil {
		return false
	}
	name := appKeyName(code)
	if name == "" {
		return false
	}
	for _, c := range []evdev.EvCode{evdev.KEY_LEFTCTRL, evdev.KEY_RIGHTCTRL, evdev.KEY_LEFTALT, evdev.KEY_RIGHTALT} {
		if _, ok := e.down[keyID(c)]; ok {
			return false
		}
	}
	for _, a := range e.view.Cells {
		if a == nil || a.Widget == nil || a.Widget.Kind != widgetApp {
			continue
		}
		if d, _ := e.widgets.apps.store.Get(a.Widget.ID); d != nil && d.Keys[name] {
			e.widgets.apps.Event(a.Widget.ID, appEvent{args: []string{"key", name}})
			e.down[keyID(code)] = nil
			return true
		}
	}
	return false
}
