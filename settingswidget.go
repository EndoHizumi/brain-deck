package main

import (
	"encoding/json"
	"fmt"
	"image"
	"log"
	"strings"
	"sync"
	"time"
)

// ---------- 設定のアプリ（widget: settings） ----------
//
// Brain だけで、よく変える設定を変える画面。上のタブで「設定」「情報」「電源」を切り替える。
//   - 設定：明るさ、暗くなるまでの時間（PC につないでいるとき、外しているとき）、フォトフレーム、画面の向き、
//     読み物の文字の大きさ、ふたを閉じたときの動き、時刻合わせ、上書きを消す。行をタップすると選ぶ画面を出す。
//     変えた値は設定ファイルを書き換えず、上書きの値（overrides.go）に置く。上書きしている行には「上書き」の札を出し、
//     選ぶ画面の「設定ファイルの値に戻す」で上書きを消す。画面の向きと読み物の文字の大きさは、もとから Brain の状態
//     （display.json、reader の state）なので、そちらを変える（上書きではない）。
//   - 情報：デーモンの版、電池、SD カードの空き、PC とつながっているか、最後に同期した時刻、時刻、ふた、上書きの数。
//   - 電源：再起動と電源を切る。2 秒押し続けると行う（短く押しただけでは行わない）。
// 操作はどれも別の goroutine で行う（エンジンのロックを持ったまま呼ばれるので）。

const (
	widgetSettings = "settings"

	setTabH      = 44
	setRowH      = 42
	setNavH      = 40
	setChoiceH   = 44
	setPowerHold = 2 * time.Second
)

const (
	setTabSettings = iota
	setTabInfo
	setTabPower
)

var setTabNames = [...]string{"設定", "情報", "電源"}

var (
	colSetTab     = RGB{0x2a, 0x34, 0x44}
	colSetTabOn   = RGB{0x3c, 0x5a, 0x84}
	colSetOver    = RGB{0xff, 0xa0, 0x30} // 上書きの札
	colSetCurrent = RGB{0xff, 0xd0, 0x40} // 選ぶ画面の、今の値の枠
	colSetDanger  = RGB{0x90, 0x30, 0x30}
	colSetHold    = RGB{0xd0, 0x50, 0x40}
)

// setRowGroups は、設定のタブの行（並ぶ順）と、ページの分け方。1 ページ目はよく変えるもの、2 ページ目はたまに変えるもの。
// 1 つの組が 1 ページに入らないとき（画面が小さいとき）は、その組を続くページに分ける。
var setRowGroups = [][]string{
	{"level", "dim", "dim_offline", "photo", "rotate", "ime"},
	{"reader", "lid", "tablet", "clock", "reset"},
}

// setRowIDs は、設定のタブの行（並ぶ順）。
var setRowIDs = func() []string {
	var ids []string
	for _, g := range setRowGroups {
		ids = append(ids, g...)
	}
	return ids
}()

// settingsPages は、1 ページに per 行のときの、ページごとの行。
func settingsPages(per int) [][]string {
	var pages [][]string
	for _, g := range setRowGroups {
		for len(g) > 0 {
			n := min(per, len(g))
			pages = append(pages, g[:n])
			g = g[n:]
		}
	}
	return pages
}

// setOverrideKey は、行の上書きの名前（上書きでない行は空）。
var setOverrideKey = map[string]string{
	"level": "backlight.level", "dim": "backlight.dim_after", "dim_offline": "backlight.offline.dim_after",
	"photo": "backlight.photo", "lid": "lid.close", "tablet": "lid.tablet_keys", "ime": "ime.method",
}

// setChoice は、選ぶ画面の 1 つ。
type setChoice struct {
	Label string
	Value any // 上書きの値（JSON にする）、画面の向き（int）、読み物の文字（int）
	Reset bool
	Close bool
}

// SettingsDeps は、設定のアプリが使う外のもの。main が作る。nil の項目は使えない（その行は「使えません」）。
type SettingsDeps struct {
	Store         *configStore
	Rotation      func() int
	Rotate        func(int)
	ReaderSize    func() int
	SetReaderSize func(int)
	OpenClock     func()
	Power         func(action string) error
	BrightLevel   func() int // 今のふだんの明るさ（上書きも設定ファイルにもないとき）
	Info          func() []SettingsInfo
	Lid           *Lid
	IME           func() IMEInfo // 日本語入力の状態（nil なら使えない）
}

// SettingsInfo は、情報のタブの 1 行。
type SettingsInfo struct{ Label, Value string }

// SettingsUI は、設定のアプリの状態（1 つのデーモンに 1 つ）。
type SettingsUI struct {
	deps SettingsDeps

	mu       sync.Mutex
	tab      int
	page     int
	choice   string // 選ぶ画面を出している行（空なら出していない）
	confirm  bool   // 「上書きをすべて消す」を聞いている
	held     string // 押している行かボタン
	holdAt   time.Time
	holdT    *time.Timer
	tick     *time.Timer
	powerReq string // 行おうとしている（行った）電源の操作
	note     string
	noteAt   time.Time
	onChange func()
	// 最後に描いたときの写し。タッチの処理（エンジンのロックを持ったまま呼ばれる）は、設定の置き場所を呼ばずにこれを使う
	// （設定の置き場所は、設定を反映するあいだロックを持ったままエンジンを待つので、呼ぶと止まる）
	shownChoice  string
	shownChoices []setChoice
	shownOvers   int
}

func NewSettingsUI(d SettingsDeps) *SettingsUI { return &SettingsUI{deps: d} }

func (u *SettingsUI) SetOnChange(f func()) { u.mu.Lock(); u.onChange = f; u.mu.Unlock() }

// SetDeps は、あとから分かったもの（設定の置き場所、電源）を設定する。
func (u *SettingsUI) SetDeps(f func(*SettingsDeps)) { u.mu.Lock(); f(&u.deps); u.mu.Unlock() }

// Changed は、外で値が変わったとき（上書き、電池、向き）に描き直す。待たずに返る
// （設定の置き場所がロックを持ったまま呼ぶので、ここでロックを待たない）。
func (u *SettingsUI) Changed() {
	if u == nil {
		return
	}
	go func() {
		u.mu.Lock()
		f := u.onChange
		u.mu.Unlock()
		if f != nil {
			f()
		}
	}()
}

func (u *SettingsUI) changedLocked() {
	if f := u.onChange; f != nil {
		go f()
	}
}

func (u *SettingsUI) sayLocked(s string) {
	u.note, u.noteAt = s, time.Now()
	u.changedLocked()
}

// SettingsRow は、設定のタブの 1 行（描くときの写し）。
type SettingsRow struct {
	ID       string
	Label    string
	Value    string
	Override bool   // 上書きしている
	Note     string // 上書きを使っていない理由など
}

// SettingsView は、描くときの写し。
type SettingsView struct {
	Tab      int
	Page     int
	Rows     []SettingsRow
	Info     []SettingsInfo
	Choice   string
	Title    string
	Choices  []setChoice
	Current  int // Choices のうち今の値（-1 なし）
	Confirm  bool
	Held     string
	HoldFrac float64 // 電源のボタンを押している割合（0〜1）
	Power    string
	PowerOK  bool
	Note     string
	Overs    int
}

// Snapshot は、描画の goroutine が使う写し。設定の置き場所などは、u.mu を持たずに読む
// （ロックの順を、設定の置き場所 → 設定のアプリ、の一方だけにする）。
func (u *SettingsUI) Snapshot() *SettingsView {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	v := &SettingsView{Tab: u.tab, Page: u.page, Choice: u.choice, Confirm: u.confirm, Held: u.held, Power: u.powerReq,
		PowerOK: u.deps.Power != nil, Current: -1}
	if u.note != "" && time.Since(u.noteAt) < noticeFor {
		v.Note = u.note
	}
	if u.held == "reboot" || u.held == "poweroff" {
		v.HoldFrac = min(float64(time.Since(u.holdAt))/float64(setPowerHold), 1)
	}
	d := u.deps
	u.mu.Unlock()
	v.Rows = settingsRows(d)
	for _, r := range v.Rows {
		if r.Override {
			v.Overs++
		}
	}
	if v.Tab == setTabInfo && d.Info != nil {
		v.Info = d.Info()
	}
	if v.Choice != "" {
		v.Title, v.Choices, v.Current = settingsChoices(d, v.Choice)
	}
	u.mu.Lock()
	u.shownOvers = v.Overs
	if u.choice == v.Choice {
		u.shownChoice, u.shownChoices = v.Choice, v.Choices
	}
	u.mu.Unlock()
	return v
}

// settingsCfgs は、今の設定（上書きを重ねたもの）と設定ファイルの値と、上書きの様子。
func settingsCfgs(d SettingsDeps) (eff, file *Config, infos map[string]OverrideInfo) {
	infos = map[string]OverrideInfo{}
	if d.Store == nil {
		return &Config{}, &Config{}, infos
	}
	eff, file = d.Store.Current(), d.Store.File()
	for _, in := range d.Store.Overrides() {
		infos[in.Key] = in
	}
	return eff, file, infos
}

// settingsRows は、設定のタブの行を作る。
func settingsRows(u SettingsDeps) []SettingsRow {
	eff, _, infos := settingsCfgs(u)
	bl := eff.Backlight
	if bl == nil {
		bl = &BacklightConfig{}
	}
	var rows []SettingsRow
	for _, id := range setRowIDs {
		r := SettingsRow{ID: id}
		switch id {
		case "level":
			r.Label = "明るさ"
			switch {
			case bl.Level != nil:
				r.Value = fmt.Sprintf("%d / 7", *bl.Level)
			case u.BrightLevel != nil:
				r.Value = fmt.Sprintf("%d / 7", u.BrightLevel())
			default:
				r.Value = "起動したとき"
			}
		case "dim":
			r.Label = "暗くなるまで（PC あり）"
			r.Value = dimRowValue(bl.DimAfter, bl.OffAfter)
		case "dim_offline":
			r.Label = "暗くなるまで（PC なし）"
			d, o := bl.DimAfter, bl.OffAfter
			if bl.Offline != nil {
				d, o = or(bl.Offline.DimAfter, d), or(bl.Offline.OffAfter, o)
			}
			r.Value = dimRowValue(d, o)
		case "photo":
			r.Label = "フォトフレーム"
			r.Value = onOff(bl.Photo != nil && *bl.Photo)
		case "rotate":
			r.Label = "画面の向き"
			r.Value = "使えません"
			if u.Rotation != nil {
				r.Value = rotateLabel(u.Rotation())
			}
		case "reader":
			r.Label = "読み物の文字"
			r.Value = "使えません"
			if u.ReaderSize != nil {
				r.Value = readerSizeLabel(u.ReaderSize())
			}
		case "lid":
			r.Label = "ふたを閉じたとき"
			r.Value = lidLabel(eff.Lid.action())
		case "tablet":
			r.Label = "タブレットの形の本体キー"
			r.Value = tabletKeysLabel(eff.Lid.tabletKeys())
		case "ime":
			r.Label = "日本語入力"
			r.Value = imeRowValue(eff.IME.method(), u.IME)
		case "clock":
			r.Label = "時刻合わせ"
			r.Value = "キーボードで入れる"
		case "reset":
			r.Label = "設定ファイルの値に戻す"
			n := len(infos)
			r.Value = fmt.Sprintf("上書き %d 個", n)
			if n == 0 {
				r.Value = "上書きなし"
			}
		}
		if k := setOverrideKey[id]; k != "" {
			if in, ok := infos[k]; ok {
				r.Override = true
				r.Note = in.Note
				if !in.Used {
					r.Note = "使っていない：" + in.Note
				}
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func dimRowValue(dim, off string) string {
	s := durLabel(dim)
	if off != "" && off != durationOff {
		s += "・" + durLabel(off) + "で消す"
	}
	return s
}

func rotateLabel(r int) string {
	switch r {
	case 90:
		return "縦向き"
	case 180:
		return "横向き（逆）"
	case 270:
		return "縦向き（逆）"
	}
	return "横向き"
}

func readerSizeLabel(n int) string {
	if n >= 32 {
		return "大きい（32）"
	}
	return "ふつう（24）"
}

var setDimChoices = []string{"30s", "1m", "3m", "5m", "10m", "30m", durationOff}

// settingsChoices は、行 id の選ぶ画面の、題と選ぶものと、今の値の番号。
func settingsChoices(u SettingsDeps, id string) (string, []setChoice, int) {
	eff, file, infos := settingsCfgs(u)
	var title string
	var cs []setChoice
	cur := -1
	bl := eff.Backlight
	if bl == nil {
		bl = &BacklightConfig{}
	}
	switch id {
	case "level":
		title = "明るさ（上書き）"
		lv := -1
		if bl.Level != nil {
			lv = *bl.Level
		} else if u.BrightLevel != nil {
			lv = u.BrightLevel()
		}
		for i := 1; i <= 7; i++ {
			if i == lv {
				cur = len(cs)
			}
			cs = append(cs, setChoice{Label: fmt.Sprint(i), Value: i})
		}
	case "dim", "dim_offline":
		d := bl.DimAfter
		title = "暗くなるまで（PC につないでいるとき）"
		if id == "dim_offline" {
			title = "暗くなるまで（PC から外しているとき）"
			if bl.Offline != nil {
				d = or(bl.Offline.DimAfter, d)
			}
		}
		for _, c := range setDimChoices {
			if c == or(d, durationOff) || (d != "" && d != durationOff && c != durationOff && sameDur(c, d)) {
				cur = len(cs)
			}
			cs = append(cs, setChoice{Label: durLabel(c), Value: c})
		}
	case "photo":
		title = "フォトフレーム（暗くする代わりに写真）"
		on := bl.Photo != nil && *bl.Photo
		cs = []setChoice{{Label: "使う", Value: true}, {Label: "使わない", Value: false}}
		cur = map[bool]int{true: 0, false: 1}[on]
	case "lid":
		title = "ふたを閉じたとき"
		cs = []setChoice{{Label: "画面を消す", Value: lidOff}, {Label: "何もしない", Value: lidNone}}
		cur = map[bool]int{true: 0, false: 1}[eff.Lid.action() == lidOff]
	case "tablet":
		title = "画面を裏返したとき（タブレットの形）の本体のキー"
		cs = []setChoice{{Label: "使わない", Value: tabletKeysOff}, {Label: "使う", Value: tabletKeysOn}}
		cur = map[bool]int{true: 0, false: 1}[eff.Lid.tabletKeys() == tabletKeysOff]
	case "ime":
		title = "日本語入力（uim-fep の入力方式）"
		var in IMEInfo
		if u.IME != nil {
			in = u.IME()
		}
		cs = []setChoice{{Label: "使わない", Value: imeOff}, {Label: "自動（入っているもの）", Value: imeAuto}}
		for _, m := range in.Installed {
			cs = append(cs, setChoice{Label: imeMethodLabel(m), Value: m})
		}
		for i, c := range cs {
			if c.Value == eff.IME.method() {
				cur = i
			}
		}
		if cur < 0 { // 設定の方式が入っていない
			cs = append(cs, setChoice{Label: imeMethodLabel(eff.IME.method()) + "（入っていない）", Value: eff.IME.method()})
			cur = len(cs) - 1
		}
	case "rotate":
		title = "画面の向き"
		r := 0
		if u.Rotation != nil {
			r = u.Rotation()
		}
		for _, a := range []int{0, 90, 180, 270} {
			if a == r {
				cur = len(cs)
			}
			cs = append(cs, setChoice{Label: rotateLabel(a), Value: a})
		}
	case "reader":
		title = "読み物とヘルプの文字の大きさ"
		n := 24
		if u.ReaderSize != nil {
			n = u.ReaderSize()
		}
		cs = []setChoice{{Label: readerSizeLabel(24), Value: 24}, {Label: readerSizeLabel(32), Value: 32}}
		cur = map[bool]int{true: 1, false: 0}[n >= 32]
	}
	if k := setOverrideKey[id]; k != "" {
		if _, ok := infos[k]; ok {
			cs = append(cs, setChoice{Label: "設定ファイルの値に戻す（" + overrideKeyOf(k).fileValue(file) + "）", Reset: true})
		}
	}
	cs = append(cs, setChoice{Label: "やめる", Close: true})
	return title, cs, cur
}

func sameDur(a, b string) bool {
	x, e1 := time.ParseDuration(a)
	y, e2 := time.ParseDuration(b)
	return e1 == nil && e2 == nil && x == y
}

// ---------- 配置 ----------

type setGeom struct {
	tabs   [3]image.Rectangle
	body   image.Rectangle
	rows   []image.Rectangle
	per    int
	prev   image.Rectangle
	next   image.Rectangle
	power  [2]image.Rectangle // 再起動、電源を切る
	box    image.Rectangle    // 選ぶ画面
	title  image.Rectangle
	choice []image.Rectangle
	yes    image.Rectangle
	no     image.Rectangle
}

func settingsGeometry(area image.Rectangle, nChoices int) setGeom {
	var g setGeom
	w := area.Dx() / 3
	for i := range g.tabs {
		x1 := area.Min.X + (i+1)*w
		if i == 2 {
			x1 = area.Max.X
		}
		g.tabs[i] = image.Rect(area.Min.X+i*w, area.Min.Y, x1, area.Min.Y+setTabH)
	}
	g.body = image.Rect(area.Min.X, area.Min.Y+setTabH+4, area.Max.X, area.Max.Y)
	g.per = max(g.body.Dy()/setRowH, 1)
	list := g.body
	if len(setRowGroups) > 1 || len(setRowIDs) > g.per {
		g.per = max((g.body.Dy()-setNavH)/setRowH, 1)
		nav := image.Rect(g.body.Min.X, g.body.Max.Y-setNavH, g.body.Max.X, g.body.Max.Y)
		g.prev = image.Rect(nav.Min.X, nav.Min.Y, nav.Min.X+nav.Dx()/3, nav.Max.Y)
		g.next = image.Rect(nav.Max.X-nav.Dx()/3, nav.Min.Y, nav.Max.X, nav.Max.Y)
		list.Max.Y = nav.Min.Y
	}
	for i := 0; i < g.per; i++ {
		g.rows = append(g.rows, image.Rect(list.Min.X, list.Min.Y+i*setRowH, list.Max.X, list.Min.Y+(i+1)*setRowH))
	}
	// 電源：2 つの大きなボタン
	bw, bh := min(g.body.Dx()-40, 360), 80
	cx := g.body.Min.X + g.body.Dx()/2
	y0 := g.body.Min.Y + 30
	g.power[0] = image.Rect(cx-bw/2, y0, cx+bw/2, y0+bh)
	g.power[1] = image.Rect(cx-bw/2, y0+bh+24, cx+bw/2, y0+2*bh+24)
	// 選ぶ画面：2 列
	cw := min(area.Dx()-24, 600)
	cols := 2
	nr := (nChoices + cols - 1) / cols
	ch := 40 + nr*(setChoiceH+8) + 8
	bmin := image.Pt(area.Min.X+(area.Dx()-cw)/2, area.Min.Y+max((area.Dy()-ch)/2, 0))
	g.box = image.Rectangle{Min: bmin, Max: bmin.Add(image.Pt(cw, ch))}
	g.title = image.Rect(g.box.Min.X+8, g.box.Min.Y+6, g.box.Max.X-8, g.box.Min.Y+38)
	iw := (cw - 8*3) / cols
	for i := 0; i < nChoices; i++ {
		c, r := i%cols, i/cols
		x := g.box.Min.X + 8 + c*(iw+8)
		y := g.box.Min.Y + 40 + r*(setChoiceH+8)
		g.choice = append(g.choice, image.Rect(x, y, x+iw, y+setChoiceH))
	}
	// 上書きをすべて消すかを聞く
	qw := min(area.Dx()-40, 360)
	qy := area.Min.Y + area.Dy()/2
	g.yes = image.Rect(cx-qw/2, qy+8, cx-4, qy+60)
	g.no = image.Rect(cx+4, qy+8, cx+qw/2, qy+60)
	return g
}

// ---------- 描く ----------

func settingsKey(env WidgetEnv) string {
	v := env.Settings
	if v == nil {
		return "settings\x00none"
	}
	b, _ := json.Marshal(v)
	return "settings\x00" + string(b)
}

// settingsNext は、電源のボタンを押しているあいだ、押した割合を描き直す時刻。
func settingsNext(env WidgetEnv) time.Time {
	if v := env.Settings; v != nil && v.Held != "" && v.HoldFrac < 1 {
		return env.Now.Add(100 * time.Millisecond)
	}
	if v := env.Settings; v != nil && v.Note != "" {
		return env.Now.Add(time.Second)
	}
	return time.Time{}
}

func drawSettings(cv *Canvas, area image.Rectangle, env WidgetEnv) {
	v := env.Settings
	cv.fill(area, colReadBar)
	if v == nil {
		drawAppPlaceholder(cv, area, []string{"設定", "Brain の画面で、明るさや時刻などを変える"})
		return
	}
	g := settingsGeometry(area, len(v.Choices))
	for i, r := range g.tabs {
		bg := colSetTab
		if i == v.Tab {
			bg = colSetTabOn
		}
		cv.fill(r.Inset(2), bg)
		drawCentered(cv, r, r.Min.Y+(r.Dy()-fontH*2)/2, setTabNames[i], 2, colText)
	}
	switch v.Tab {
	case setTabSettings:
		drawSettingsRows(cv, g, v)
	case setTabInfo:
		for i, in := range v.Info {
			y := g.body.Min.Y + 4 + i*36
			if y+fontH*2 > g.body.Max.Y {
				break
			}
			row := image.Rect(g.body.Min.X, y, g.body.Max.X, y+34)
			cv.fill(row, colReadRow)
			lw := min(font.textWidth(in.Label)*2+16, row.Dx()/2)
			cv.text(row.Min.X+8, row.Min.Y+(row.Dy()-fontH*2)/2, clipText(in.Label, lw-8, 2), 2, colReadSub, row)
			s := min(fitScaleMax([]string{in.Value}, row.Dx()-lw-16, row.Dy()-4, 2), 2)
			cv.text(row.Min.X+lw+8, row.Min.Y+(row.Dy()-fontH*s)/2, clipText(in.Value, row.Dx()-lw-16, s), s, colText, row)
		}
	case setTabPower:
		for i, r := range g.power {
			act := [2]string{"reboot", "poweroff"}[i]
			label := [2]string{"再起動", "電源を切る"}[i]
			bg := colFilesRow
			cv.fill(r, bg)
			if v.Held == act {
				fill := r
				fill.Max.X = r.Min.X + int(float64(r.Dx())*v.HoldFrac)
				cv.fill(fill, colSetHold)
			}
			cv.frame(r, 2, colReadFrame)
			c := colText
			if !v.PowerOK {
				c = colReadSub
			}
			drawCentered(cv, r, r.Min.Y+(r.Dy()-fontH*3)/2, label, 3, c)
		}
		hint := "2 秒押し続けると行います（データは保存します）"
		if !v.PowerOK {
			hint = "使えません（systemd のサービスとして動いていません）"
		}
		if v.Power != "" {
			hint = map[string]string{"reboot": "再起動します…", "poweroff": "電源を切ります…"}[v.Power]
		}
		y := g.power[1].Max.Y + 16
		s := min(fitScaleMax([]string{hint}, g.body.Dx()-16, fontH*2, 2), 2)
		drawCentered(cv, g.body, y, hint, s, colReadSub)
		if !strings.HasPrefix(hint, "使えません") {
			sub := "電池は充電されません（充電は純正のファームウェアで）"
			drawCentered(cv, g.body, y+fontH*s+8, clipText(sub, g.body.Dx()-16, 1), 1, colReadSub)
		}
	}
	if v.Choice != "" {
		cv.fill(g.box, colReadMenu)
		cv.frame(g.box, 2, colReadFrame)
		s := min(fitScaleMax([]string{v.Title}, g.title.Dx(), g.title.Dy(), 2), 2)
		drawCentered(cv, g.title, g.title.Min.Y+(g.title.Dy()-fontH*s)/2, v.Title, s, colText)
		for i, c := range v.Choices {
			r := g.choice[i]
			bg := colFilesRow
			if c.Reset {
				bg = colSetTab
			}
			cv.fill(r, bg)
			if i == v.Current {
				cv.frame(r, 3, colSetCurrent)
			}
			s := min(fitScaleMax([]string{c.Label}, r.Dx()-8, r.Dy()-4, 2), 2)
			drawCentered(cv, r, r.Min.Y+(r.Dy()-fontH*s)/2, c.Label, s, colText)
		}
	}
	if v.Confirm {
		box := image.Rect(g.yes.Min.X-16, g.yes.Min.Y-76, g.no.Max.X+16, g.yes.Max.Y+12)
		cv.fill(box, colReadMenu)
		cv.frame(box, 2, colFilesWarn)
		for i, l := range []string{"上書きをすべて消して、", "設定ファイルの値に戻しますか？"} {
			s := min(fitScaleMax([]string{l}, box.Dx()-8, fontH*2, 2), 2)
			drawCentered(cv, box, box.Min.Y+8+i*(fontH*2+4), l, s, colText)
		}
		cv.fill(g.yes, colSetDanger)
		cv.fill(g.no, colFilesRow)
		drawCentered(cv, g.yes, g.yes.Min.Y+(g.yes.Dy()-fontH*2)/2, "戻す", 2, colText)
		drawCentered(cv, g.no, g.no.Min.Y+(g.no.Dy()-fontH*2)/2, "やめる", 2, colText)
	}
	if v.Note != "" {
		n := image.Rect(area.Min.X, area.Max.Y-fontH*2-8, area.Max.X, area.Max.Y)
		cv.fill(n, colReadMenu)
		drawCentered(cv, n, n.Min.Y+4, clipText(v.Note, n.Dx()-8, 2), 2, colText)
	}
}

func drawSettingsRows(cv *Canvas, g setGeom, v *SettingsView) {
	pl := settingsPages(g.per)
	pages := len(pl)
	page := min(max(v.Page, 0), pages-1)
	byID := map[string]SettingsRow{}
	for _, r := range v.Rows {
		byID[r.ID] = r
	}
	for i, r := range g.rows {
		if i >= len(pl[page]) {
			break
		}
		row := byID[pl[page][i]]
		b := r.Inset(2)
		bg := colFilesRow
		if v.Held == row.ID {
			bg = colFilesHeld
		}
		cv.fill(b, bg)
		val := row.Value
		vs := 2
		if font.textWidth(val)*2 > b.Dx()/2 {
			vs = 1
		}
		vw := font.textWidth(val) * vs
		vc := colText
		if row.Override {
			vc = colSetOver
		}
		cv.text(b.Max.X-vw-8, b.Min.Y+(b.Dy()-fontH*vs)/2, val, vs, vc, b)
		x := b.Min.X + 8
		lw := b.Dx() - vw - 24
		if row.Override {
			tag := "上書き"
			tw := font.textWidth(tag) + 8
			t := image.Rect(b.Max.X-vw-16-tw, b.Min.Y+(b.Dy()-fontH-6)/2, b.Max.X-vw-16, b.Min.Y+(b.Dy()+fontH+6)/2)
			cv.fill(t, colSetOver)
			cv.text(t.Min.X+4, t.Min.Y+3, tag, 1, colBG, t)
			lw -= tw + 8
		}
		cv.text(x, b.Min.Y+(b.Dy()-fontH*2)/2, clipText(row.Label, lw, 2), 2, colText, b)
	}
	if !g.prev.Empty() {
		for _, nb := range []struct {
			r  image.Rectangle
			s  string
			on bool
		}{{g.prev, "▲", page > 0}, {g.next, "▼", page+1 < pages}} {
			c := colText
			if !nb.on {
				c = colReadSub
			}
			cv.fill(nb.r.Inset(3), colReadRow)
			drawCentered(cv, nb.r, nb.r.Min.Y+(nb.r.Dy()-fontH*2)/2, nb.s, 2, c)
		}
		nav := image.Rect(g.prev.Max.X, g.prev.Min.Y, g.next.Min.X, g.prev.Max.Y)
		drawCentered(cv, nav, nav.Min.Y+(nav.Dy()-fontH*2)/2, fmt.Sprintf("%d/%d", page+1, pages), 2, colReadSub)
	}
}

// drawAppPlaceholder は、中身が Brain の状態で決まるアプリの、-render-png と GUI のプレビューでの絵（説明だけ）。
// gui/src/readerwidget.ts の drawAppPlaceholder と同じ。
func drawAppPlaceholder(cv *Canvas, area image.Rectangle, lines []string) {
	cv.fill(area, colReadBar)
	y := area.Min.Y + area.Dy()/2 - fontH*2
	for i, l := range lines {
		s := min(fitScaleMax([]string{l}, area.Dx()-8, fontH*2, 2), 2)
		drawCentered(cv, area, y+i*fontH*2+4*i, l, s, colReadSub)
	}
}

// ---------- 操作 ----------

// touchDown は、セル cell の pt に触れたとき。
func (u *SettingsUI) touchDown(cell image.Rectangle, pt image.Point) {
	area := cell.Inset(cellGap + textMargin)
	u.mu.Lock()
	defer u.mu.Unlock()
	defer u.changedLocked()
	u.note = ""
	if u.choice != "" {
		var cs []setChoice
		if u.shownChoice == u.choice {
			cs = u.shownChoices // まだ描いていなければ、選ぶものはない（外のタップとして閉じる）
		}
		g := settingsGeometry(area, len(cs))
		id := u.choice
		for i, c := range cs {
			if pt.In(g.choice[i]) {
				u.choice = ""
				if !c.Close {
					go u.choose(id, c)
				}
				return
			}
		}
		if !pt.In(g.box) {
			u.choice = "" // 外をタップ：閉じる
		}
		return
	}
	g := settingsGeometry(area, 0)
	if u.confirm {
		switch {
		case pt.In(g.yes):
			u.confirm = false
			go u.resetAll()
		case pt.In(g.no):
			u.confirm = false
		}
		return
	}
	for i, r := range g.tabs {
		if pt.In(r) {
			u.tab, u.page = i, 0
			return
		}
	}
	switch u.tab {
	case setTabSettings:
		pl := settingsPages(g.per)
		pages := len(pl)
		switch {
		case pt.In(g.prev):
			u.page = max(u.page-1, 0)
			return
		case pt.In(g.next):
			u.page = min(u.page+1, pages-1)
			return
		}
		page := min(max(u.page, 0), pages-1)
		for i, r := range g.rows {
			if i < len(pl[page]) && pt.In(r) {
				u.held = pl[page][i]
			}
		}
	case setTabPower:
		for i, r := range g.power {
			if !pt.In(r) {
				continue
			}
			if u.deps.Power == nil {
				u.sayLocked("使えません（systemd のサービスとして動いていません）")
				return
			}
			if u.powerReq != "" {
				return
			}
			act := [2]string{"reboot", "poweroff"}[i]
			u.held, u.holdAt = act, time.Now()
			u.holdT = time.AfterFunc(setPowerHold, func() { u.powerFire(act) })
		}
	}
}

// touchUp は、離したとき。設定の行なら、その行の選ぶ画面を出す（時刻合わせは入れる欄を開く）。
func (u *SettingsUI) touchUp(cancel bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	held := u.held
	u.held = ""
	if u.holdT != nil {
		u.holdT.Stop()
		u.holdT = nil
		if !cancel && (held == "reboot" || held == "poweroff") {
			u.sayLocked("2 秒押し続けてください")
			return
		}
	}
	defer u.changedLocked()
	if cancel || held == "" || held == "reboot" || held == "poweroff" {
		return
	}
	switch held {
	case "clock":
		if f := u.deps.OpenClock; f != nil {
			go f()
		}
	case "reset":
		if u.deps.Store == nil || u.shownOvers == 0 {
			u.sayLocked("上書きはありません（設定ファイルの値で動いています）")
			return
		}
		u.confirm = true
	case "rotate":
		if u.deps.Rotate == nil {
			u.sayLocked("使えません")
			return
		}
		u.openChoiceLocked(held)
	case "reader":
		if u.deps.SetReaderSize == nil {
			u.sayLocked("使えません")
			return
		}
		u.openChoiceLocked(held)
	default:
		if u.deps.Store == nil || u.deps.Store.ov == nil {
			u.sayLocked("使えません（上書きの置き場所がありません）")
			return
		}
		u.openChoiceLocked(held)
	}
}

// openChoiceLocked は、行 id の選ぶ画面を出す。選ぶものは、次に描くときに作る（Snapshot）。
func (u *SettingsUI) openChoiceLocked(id string) {
	u.choice = id
	u.shownChoice, u.shownChoices = "", nil
}

// powerFire は、電源のボタンを 2 秒押し続けたとき（タイマーの goroutine）。
func (u *SettingsUI) powerFire(act string) {
	u.mu.Lock()
	if u.held != act || u.powerReq != "" {
		u.mu.Unlock()
		return
	}
	u.holdT = nil
	u.powerReq = act
	f := u.deps.Power
	u.changedLocked()
	u.mu.Unlock()
	log.Printf("settings: %s (held for %v)", act, setPowerHold)
	if err := f(act); err != nil {
		u.mu.Lock()
		u.powerReq = ""
		u.sayLocked("できませんでした：" + err.Error())
		u.mu.Unlock()
	}
}

// choose は、選ぶ画面で選んだとき（別の goroutine）。
func (u *SettingsUI) choose(id string, c setChoice) {
	u.mu.Lock()
	d := u.deps
	u.mu.Unlock()
	var err error
	msg := ""
	switch {
	case id == "rotate":
		d.Rotate(c.Value.(int))
	case id == "reader":
		d.SetReaderSize(c.Value.(int))
	case c.Reset:
		_, err = d.Store.ClearOverrides([]string{setOverrideKey[id]}, sourceBrain)
		msg = "設定ファイルの値に戻しました"
	default:
		key := setOverrideKey[id]
		raw, _ := json.Marshal(c.Value)
		if overrideEqualsFile(key, c.Value, d.Store.File()) {
			_, err = d.Store.ClearOverrides([]string{key}, sourceBrain) // 設定ファイルと同じなら、上書きを置かない
		} else {
			err = d.Store.SetOverride(key, raw, sourceBrain)
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.sayLocked("変えられませんでした：" + err.Error())
		return
	}
	if msg != "" {
		u.sayLocked(msg)
	}
	u.changedLocked()
}

func (u *SettingsUI) resetAll() {
	u.mu.Lock()
	st := u.deps.Store
	u.mu.Unlock()
	done, err := st.ClearOverrides(nil, sourceBrain)
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.sayLocked("消せませんでした：" + err.Error())
		return
	}
	u.sayLocked(fmt.Sprintf("上書きを %d 個消しました（設定ファイルの値に戻しました）", len(done)))
}

// overrideEqualsFile は、上書きの値 v が、設定ファイルの値と同じか（同じなら上書きを置かない）。
func overrideEqualsFile(key string, v any, f *Config) bool {
	if f == nil {
		return false
	}
	b := f.Backlight
	switch key {
	case "backlight.level":
		return b != nil && b.Level != nil && *b.Level == v.(int)
	case "backlight.dim_after":
		return b != nil && dimEq(b.DimAfter, v.(string))
	case "backlight.offline.dim_after":
		if b == nil {
			return false
		}
		d := b.DimAfter
		if b.Offline != nil {
			d = or(b.Offline.DimAfter, d)
		}
		return dimEq(d, v.(string))
	case "backlight.photo":
		return b != nil && (b.Photo != nil && *b.Photo) == v.(bool) && (b.Offline == nil || b.Offline.Photo == nil || *b.Offline.Photo == v.(bool))
	case "lid.close":
		return f.Lid.action() == v.(string)
	case "lid.tablet_keys":
		return f.Lid.tabletKeys() == v.(string)
	}
	return false
}

func dimEq(a, b string) bool {
	if or(a, durationOff) == durationOff || b == durationOff {
		return or(a, durationOff) == b
	}
	return sameDur(a, b)
}

func compileSettings(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: settings handles taps itself; remove key and layer_*")
	}
	return &WidgetDef{Kind: widgetSettings}, nil
}

// settingsInfoIn は、情報のタブに出すもの（main が集める）。
type settingsInfoIn struct {
	Version   string
	Battery   *BatteryInfo
	Free      int64
	Size      int64
	DiskErr   error
	Link      string
	LastSync  *SyncRecord
	LastPCSet *time.Time
	Synced    bool
	Lid       LidInfo
	IME       *IMEInfo
	Overrides []OverrideInfo
}

// settingsInfoRows は、情報のタブの行を作る。
func (in settingsInfoIn) rows() []SettingsInfo {
	var out []SettingsInfo
	add := func(l, v string) { out = append(out, SettingsInfo{l, v}) }
	add("版", in.Version)
	switch b := in.Battery; {
	case b == nil:
		add("電池", "読めません")
	default:
		s := ""
		if b.Percent >= 0 {
			s = fmt.Sprintf("%d%%", b.Percent)
		}
		if b.VoltageMV > 0 {
			s += fmt.Sprintf("（%.2f V）", float64(b.VoltageMV)/1000)
		}
		switch {
		case b.ChargerOff:
			s += " USB 給電、充電していない"
		case b.Charging:
			s += " 充電中"
		case b.Plugged:
			s += " USB 給電"
		}
		add("電池", strings.TrimSpace(s))
	}
	if in.DiskErr != nil {
		add("SD カードの空き", "読めません")
	} else {
		add("SD カードの空き", fmt.Sprintf("%s（全体 %s）", humanSize(in.Free), humanSize(in.Size)))
	}
	switch in.Link {
	case "configured":
		add("PC", "つながっている")
	case "suspended":
		add("PC", "つながっている（PC はスリープ中）")
	case linkPower:
		add("PC", "給電のみ（PC と通信していない）")
	case "":
		add("PC", "分からない")
	default:
		add("PC", "外れている")
	}
	switch {
	case in.LastSync != nil:
		r := "成功"
		if !in.LastSync.OK {
			r = "失敗したものあり"
		}
		add("最後の同期", in.LastSync.At.Local().Format("1月2日 15:04")+"（"+r+"）")
	case in.LastPCSet != nil:
		add("最後の同期", "まだない（時刻だけ "+in.LastPCSet.Local().Format("1月2日 15:04")+" に合わせた）")
	default:
		add("最後の同期", "まだない")
	}
	if in.Synced {
		add("時刻", "合わせてある")
	} else {
		add("時刻", "合わせていない（時刻合わせで入れる）")
	}
	lid := "開いている"
	if in.Lid.Closed {
		lid = "閉じている"
	}
	if in.Lid.Tablet {
		lid += "（タブレットの形）"
		if in.Lid.KeysOff {
			lid += "。本体のキーは使っていない"
		}
	}
	add("ふた", lid+"、閉じたら"+lidLabel(in.Lid.Close))
	if in.IME != nil {
		add("日本語入力", imeInfoLabel(*in.IME))
	}
	if len(in.Overrides) == 0 {
		add("上書き", "なし（設定ファイルの値で動いています）")
	} else {
		var names []string
		for _, o := range in.Overrides {
			names = append(names, o.Label)
		}
		add("上書き", fmt.Sprintf("%d 個：%s", len(in.Overrides), strings.Join(names, "、")))
	}
	return out
}

// renderSettingsView は、-render-settings の画面を作る（設定は cfg、上書きはなし、情報は env から）。
func renderSettingsView(cfg *Config, tab, choice string, rot, readSize int, env WidgetEnv) (*SettingsView, error) {
	t := map[string]int{"settings": setTabSettings, "info": setTabInfo, "power": setTabPower}
	page := 0
	if tab == "settings2" { // 設定のタブの 2 ページ目
		tab, page = "settings", 1
	}
	ti, ok := t[tab]
	if !ok {
		return nil, fmt.Errorf("-render-settings must be settings, settings2, info or power, got %q", tab)
	}
	u := NewSettingsUI(SettingsDeps{
		Store:      &configStore{file: cfg, cfg: cfg},
		Rotation:   func() int { return rot },
		ReaderSize: func() int { return readSize },
		Power:      func(string) error { return nil },
		Info: func() []SettingsInfo {
			in := settingsInfoIn{Version: daemonVersion(), Battery: env.Battery, Free: 11 << 30, Size: 14 << 30, Link: env.Link, Synced: env.TimeSynced,
				LastPCSet: env.LastSync, Lid: LidInfo{Close: cfg.Lid.action()}}
			return in.rows()
		},
	})
	u.tab, u.page = ti, page
	if choice != "" {
		if _, ok := map[string]bool{"level": true, "dim": true, "dim_offline": true, "photo": true, "rotate": true, "reader": true, "lid": true, "tablet": true, "ime": true}[choice]; !ok {
			return nil, fmt.Errorf("-render-settings-choice must be one of level, dim, dim_offline, photo, rotate, reader, lid, tablet, ime, got %q", choice)
		}
		u.choice = choice
	}
	return u.Snapshot(), nil
}
