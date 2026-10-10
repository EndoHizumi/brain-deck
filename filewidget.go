package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lefthand/internal/imgconv"
)

// ---------- ファイルエクスプローラ（widget: files） ----------
//
// Brain の画面で、見られる場所（files.go）のフォルダをたどる。名前、大きさ、更新した日時と、SD カードの空きを出す。
// 「大きい順」で、フォルダの中の合計（du）の大きい順に並べる（数えるのは別の goroutine。8 秒まで）。
// ファイルをタップすると中を見る：テキストは読み物のアプリと同じ描き方（横書き 24 ドット）、画像（PNG、JPEG、GIF、
// Brain の .565）は写真と同じ変換（internal/imgconv）で画面に合わせる。大きな画像（400 万画素を超える）は開かない
// （Brain では時間がかかるので、そのことを出す。PC で brain-deck files get）。
// 見ているときに真ん中をタップすると、ほかのアプリで開く：テキストは読み物に、画像は写真か背景画像に足す。
// 行を長押し（0.8 秒）すると、消すかを聞く（消せる場所だけ）。

const (
	widgetFiles = "files"

	filesRowH      = 44
	filesHeadH     = 24
	filesBarH      = 40
	filesHold      = 800 * time.Millisecond
	filesTextMax   = 4 << 20
	filesImageMax  = 4_000_000 // 開く画像の画素の上限（Brain の RAM。展開すると 1 画素 8 バイトほど）
	filesImageSlow = 500_000   // これを超えると「時間がかかります」（実機で 200 万画素に 5 秒、380 万画素に 9 秒）
)

const (
	filesList = iota
	filesText
	filesImage
	filesMenu    // 見ているファイルを、ほかのアプリで開く
	filesConfirm // 消すかを聞く
)

// FilesUI は、ファイルエクスプローラのセルの状態。
type FilesUI struct {
	svc *FilesService

	mu       sync.Mutex
	mode     int
	dir      string // 今のフォルダ（空なら場所の一覧）
	list     FileList
	listErr  string
	bySize   bool
	counting bool
	page     int
	gen      uint64
	sel      *FileEntry // 見ている・消そうとしているファイル
	doc      *ReadDoc   // テキスト
	starts   []ReadPos
	pageText int
	img      *Image // 画像（画面に合わせたもの）
	note     string // 「変換しています…」など
	holdT    *time.Timer
	held     int // 押している行（-1 なし）
	longDone bool
	onChange func()
	notify   func(string)
	photos   *PhotoStore
	images   *ImageStore
	reads    *ReadStore
	added    func()          // 読み物を足したとき（読み物のアプリの一覧を読み直す）
	area     image.Rectangle // 最後に描いた範囲（テキストのページを数える）
}

func NewFilesUI(svc *FilesService) *FilesUI {
	u := &FilesUI{svc: svc, held: -1}
	u.reload()
	return u
}

func (u *FilesUI) SetOnChange(f func())     { u.mu.Lock(); u.onChange = f; u.mu.Unlock() }
func (u *FilesUI) SetNotify(f func(string)) { u.mu.Lock(); u.notify = f; u.mu.Unlock() }

func (u *FilesUI) changedLocked() {
	if f := u.onChange; f != nil {
		go f()
	}
}

func (u *FilesUI) sayLocked(s string) {
	if f := u.notify; f != nil {
		go f(s)
	}
}

// reload は、今のフォルダを読み直す（大きい順なら、別の goroutine で数える）。
func (u *FilesUI) reload() {
	u.mu.Lock()
	dir, bySize := u.dir, u.bySize
	u.gen++
	gen := u.gen
	if bySize && dir != "" {
		u.counting = true
		u.mu.Unlock()
		go func() {
			l, err := u.svc.List(dir, true, true)
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.gen != gen {
				return
			}
			u.counting = false
			u.setListLocked(l, err)
			u.changedLocked()
		}()
		return
	}
	u.mu.Unlock()
	l, err := u.svc.List(dir, false, false)
	u.mu.Lock()
	u.setListLocked(l, err)
	u.changedLocked()
	u.mu.Unlock()
}

func (u *FilesUI) setListLocked(l FileList, err error) {
	u.listErr = ""
	if err != nil {
		u.listErr = err.Error()
		l = FileList{Path: u.dir, Entries: []FileEntry{}}
	}
	u.list = l
	u.page = min(u.page, max(len(l.Entries)-1, 0))
}

// FilesView は、描くときの写し。
type FilesView struct {
	mode     int
	list     FileList
	listErr  string
	bySize   bool
	counting bool
	page     int
	held     int
	sel      *FileEntry
	doc      *ReadDoc
	starts   []ReadPos
	pageText int
	img      *Image
	note     string
	ui       *FilesUI
}

func (u *FilesUI) Snapshot() *FilesView {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return &FilesView{mode: u.mode, list: u.list, listErr: u.listErr, bySize: u.bySize, counting: u.counting, page: u.page, held: u.held,
		sel: u.sel, doc: u.doc, starts: u.starts, pageText: u.pageText, img: u.img, note: u.note, ui: u}
}

func filesKey(env WidgetEnv) string {
	v := env.Files
	if v == nil {
		return "files\x00none"
	}
	sel := ""
	if v.sel != nil {
		sel = v.sel.Path
	}
	k := fmt.Sprintf("files\x00%d\x00%s\x00%v\x00%v\x00%d\x00%d\x00%s\x00%d\x00%d\x00%p\x00%s\x00%s\x00%d\x00%d",
		v.mode, v.list.Path, v.bySize, v.counting, v.page, v.held, sel, v.pageText, len(v.starts), v.img, v.note, v.listErr, len(v.list.Entries), v.list.Free>>20)
	for _, e := range v.list.Entries {
		k += "\x00" + e.Name + fmt.Sprint(e.Size, e.MTime.Unix())
	}
	return k
}

// ---------- 配置 ----------

type filesGeom struct {
	head image.Rectangle
	bar  image.Rectangle
	up   image.Rectangle // ↑ 上のフォルダ
	sort image.Rectangle // 名前順・大きい順
	prev image.Rectangle // ▲
	next image.Rectangle // ▼
	rows []image.Rectangle
	per  int
	yes  image.Rectangle // 消す
	no   image.Rectangle // やめる
	menu []image.Rectangle
	body image.Rectangle
}

func filesGeometry(area image.Rectangle) filesGeom {
	g := filesGeom{head: image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Min.Y+filesHeadH)}
	g.bar = image.Rect(area.Min.X, g.head.Max.Y, area.Max.X, g.head.Max.Y+filesBarH)
	w := g.bar.Dx() / 4
	g.up = image.Rect(g.bar.Min.X, g.bar.Min.Y, g.bar.Min.X+w, g.bar.Max.Y)
	g.sort = image.Rect(g.up.Max.X, g.bar.Min.Y, g.up.Max.X+w, g.bar.Max.Y)
	g.prev = image.Rect(g.sort.Max.X, g.bar.Min.Y, g.sort.Max.X+w, g.bar.Max.Y)
	g.next = image.Rect(g.prev.Max.X, g.bar.Min.Y, g.bar.Max.X, g.bar.Max.Y)
	g.body = image.Rect(area.Min.X, g.bar.Max.Y+2, area.Max.X, area.Max.Y)
	g.per = max(g.body.Dy()/filesRowH, 1)
	for i := 0; i < g.per; i++ {
		g.rows = append(g.rows, image.Rect(g.body.Min.X, g.body.Min.Y+i*filesRowH, g.body.Max.X, g.body.Min.Y+(i+1)*filesRowH))
	}
	bw, bh := min(area.Dx()-40, 320), 52
	cx, cy := area.Min.X+area.Dx()/2, area.Min.Y+area.Dy()/2
	g.yes = image.Rect(cx-bw/2, cy+8, cx-4, cy+8+bh)
	g.no = image.Rect(cx+4, cy+8, cx+bw/2, cy+8+bh)
	for i := 0; i < 5; i++ {
		y := area.Min.Y + 40 + i*(bh+8)
		g.menu = append(g.menu, image.Rect(cx-bw/2, y, cx+bw/2, y+bh))
	}
	return g
}

// ---------- 描く ----------

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dK", (n+1023)>>10)
	}
	return fmt.Sprintf("%dB", n)
}

var (
	colFilesRow  = RGB{0x1c, 0x28, 0x38}
	colFilesDir  = RGB{0xff, 0xd0, 0x60}
	colFilesHeld = RGB{0x60, 0x48, 0x10}
	colFilesWarn = RGB{0xff, 0x80, 0x20}
)

func (v *FilesView) menuItems() []string {
	if v.sel == nil {
		return nil
	}
	if v.doc != nil {
		return []string{"読み物に足す", "一覧に戻る", "やめる"}
	}
	return []string{"写真に足す", "背景画像に足す", "一覧に戻る", "やめる"}
}

func drawFiles(cv *Canvas, area image.Rectangle, env WidgetEnv) {
	v := env.Files
	cv.fill(area, colReadBar)
	if v == nil {
		return
	}
	g := filesGeometry(area)
	switch v.mode {
	case filesText, filesMenu:
		if v.doc != nil {
			drawFilesText(cv, area, v)
		} else if v.img != nil {
			drawFilesImage(cv, area, v)
		}
		if v.mode == filesMenu {
			box := image.Rect(g.menu[0].Min.X-8, g.menu[0].Min.Y-8, g.menu[0].Max.X+8, g.menu[len(v.menuItems())-1].Max.Y+8)
			cv.fill(box, colReadMenu)
			cv.frame(box, 2, colReadFrame)
			for i, l := range v.menuItems() {
				cv.fill(g.menu[i], colFilesRow)
				drawCentered(cv, g.menu[i], g.menu[i].Min.Y+(g.menu[i].Dy()-fontH*2)/2, l, 2, colText)
			}
		}
		return
	case filesImage:
		drawFilesImage(cv, area, v)
		return
	}
	// 一覧
	head := v.list.Root
	if v.list.Path != "" {
		head = v.list.Path
	} else {
		head = "見られる場所"
	}
	free := "空き " + humanSize(v.list.Free)
	fw := font.textWidth(free) * 2
	cv.text(g.head.Max.X-fw-4, g.head.Min.Y+2, free, 2, colReadSub, g.head)
	cv.text(g.head.Min.X+4, g.head.Min.Y+2, clipText(head, g.head.Dx()-fw-16, 2), 2, colText, g.head)
	sortL := "並び：名前"
	if v.bySize {
		sortL = "並び：大きさ"
	}
	if v.counting {
		sortL = "数えています"
	}
	for _, b := range []struct {
		r  image.Rectangle
		s  string
		on bool
	}{{g.up, "↑ 上へ", v.list.Path != ""}, {g.sort, sortL, v.list.Path != ""}, {g.prev, "▲", v.page > 0}, {g.next, "▼", v.page+g.per < len(v.list.Entries)}} {
		c := colText
		if !b.on {
			c = colReadSub
		}
		cv.fill(b.r.Inset(2), colFilesRow)
		s := min(fitScaleMax([]string{b.s}, b.r.Dx()-8, b.r.Dy()-4, 2), 2)
		drawCentered(cv, b.r, b.r.Min.Y+(b.r.Dy()-fontH*s)/2, b.s, s, c)
	}
	if v.listErr != "" {
		drawCentered(cv, g.body, g.body.Min.Y+20, clipText(v.listErr, g.body.Dx()-8, 1), 1, colFilesWarn)
	} else if len(v.list.Entries) == 0 {
		drawCentered(cv, g.body, g.body.Min.Y+20, "空のフォルダ", 2, colReadSub)
	}
	for i, r := range g.rows {
		k := v.page + i
		if k >= len(v.list.Entries) {
			break
		}
		e := v.list.Entries[k]
		b := r.Inset(2)
		bg := colFilesRow
		if v.held == k {
			bg = colFilesHeld
		}
		cv.fill(b, bg)
		name := e.Name
		ink := colText
		if e.Dir {
			name, ink = "■ "+name+"/", colFilesDir
		}
		if e.Link {
			name += " →"
		}
		info := e.MTime.Format("2006-01-02 15:04")
		if e.MTime.IsZero() {
			info = "（まだない）"
		}
		if !e.Dir || e.Size > 0 {
			info = humanSize(e.Size) + "  " + info
		}
		iw := font.textWidth(info)
		cv.text(b.Max.X-iw-6, b.Min.Y+(b.Dy()-fontH)/2, info, 1, colReadSub, b)
		cv.text(b.Min.X+6, b.Min.Y+(b.Dy()-fontH*2)/2, clipText(name, b.Dx()-iw-20, 2), 2, ink, b)
	}
	if v.mode == filesConfirm && v.sel != nil {
		box := image.Rect(g.yes.Min.X-16, g.yes.Min.Y-60, g.no.Max.X+16, g.yes.Max.Y+12)
		cv.fill(box, colReadMenu)
		cv.frame(box, 2, colFilesWarn)
		drawCentered(cv, box, box.Min.Y+8, clipText("「"+v.sel.Name+"」を消しますか？", box.Dx()-8, 2), 2, colText)
		cv.fill(g.yes, RGB{0x90, 0x30, 0x30})
		cv.fill(g.no, colFilesRow)
		drawCentered(cv, g.yes, g.yes.Min.Y+(g.yes.Dy()-fontH*2)/2, "消す", 2, colText)
		drawCentered(cv, g.no, g.no.Min.Y+(g.no.Dy()-fontH*2)/2, "やめる", 2, colText)
	}
	if v.note != "" {
		n := image.Rect(area.Min.X, area.Max.Y-fontH*2-8, area.Max.X, area.Max.Y)
		cv.fill(n, colReadMenu)
		drawCentered(cv, n, n.Min.Y+4, clipText(v.note, n.Dx()-8, 2), 2, colText)
	}
}

func drawFilesText(cv *Canvas, area image.Rectangle, v *FilesView) {
	cv.fill(area, colReadPaper)
	body := image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Max.Y-readFooterH)
	m := readMetricsFor(body, 24, false, false)
	pos := ReadPos{}
	if v.pageText < len(v.starts) {
		pos = v.starts[v.pageText]
	}
	drawReadPage(cv, v.doc, m, layoutPage(v.doc, m, pos, nil))
	foot := image.Rect(area.Min.X, area.Max.Y-readFooterH, area.Max.X, area.Max.Y)
	cv.fill(foot, colReadBar)
	num := fmt.Sprintf("%d / %d", v.pageText+1, max(len(v.starts), 1))
	nw := font.textWidth(num)
	cv.text(foot.Max.X-nw-6, foot.Min.Y+3, num, 1, colReadSub, foot)
	cv.text(foot.Min.X+6, foot.Min.Y+3, clipText(v.sel.Name+"（真ん中をタップで道具）", foot.Dx()-nw-24, 1), 1, colText, foot)
}

func drawFilesImage(cv *Canvas, area image.Rectangle, v *FilesView) {
	cv.fill(area, colBG)
	if v.img != nil {
		cv.blitImage(v.img, centered(v.img, area).dst, area)
	}
	if v.note != "" || v.img == nil {
		n := image.Rect(area.Min.X, area.Max.Y-fontH*2-8, area.Max.X, area.Max.Y)
		cv.fill(n, colReadMenu)
		drawCentered(cv, n, n.Min.Y+4, clipText(v.note, n.Dx()-8, 2), 2, colText)
	}
}

// ---------- 操作 ----------

func isTextName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".log", ".csv", ".json", ".yaml", ".yml", ".conf", ".ini", ".sh", ".go", ".py", ".c", ".h", ".js", ".ts", ".html", ".xml", ".toml", ".rc", "":
		return true
	}
	return false
}

func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".565":
		return true
	}
	return false
}

// touchDown は、セル cell の pt に触れたとき。長押しは filesHold で数える。
func (u *FilesUI) touchDown(cell image.Rectangle, pt image.Point) {
	area := cell.Inset(cellGap + textMargin)
	g := filesGeometry(area)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.area = area
	u.longDone = false
	switch u.mode {
	case filesConfirm:
		switch {
		case pt.In(g.yes):
			sel := u.sel
			u.mode, u.sel = filesList, nil
			go func() {
				if err := u.svc.Delete(sel.Path); err != nil {
					u.mu.Lock()
					u.sayLocked("消せませんでした：" + err.Error())
					u.mu.Unlock()
				}
				u.reload()
			}()
		case pt.In(g.no):
			u.mode, u.sel = filesList, nil
		}
		u.changedLocked()
		return
	case filesMenu:
		items := (&FilesView{sel: u.sel, doc: u.doc, mode: u.mode}).menuItems()
		for i, l := range items {
			if !pt.In(g.menu[i]) {
				continue
			}
			u.menuLocked(l)
			u.changedLocked()
			return
		}
		u.mode = map[bool]int{true: filesText, false: filesImage}[u.doc != nil]
		u.changedLocked()
		return
	case filesText:
		third := area.Dx() / 3
		switch {
		case pt.X < area.Min.X+third:
			u.pageText = max(u.pageText-1, 0)
		case pt.X >= area.Max.X-third:
			u.pageText = min(u.pageText+1, max(len(u.starts)-1, 0))
		default:
			u.mode = filesMenu
		}
		u.changedLocked()
		return
	case filesImage:
		if u.img != nil {
			u.mode = filesMenu
		} else {
			u.backLocked()
		}
		u.changedLocked()
		return
	}
	// 一覧
	switch {
	case pt.In(g.up):
		if u.list.Path != "" {
			u.dir = u.list.Parent
			u.page = 0
			go u.reload()
		}
	case pt.In(g.sort):
		if u.list.Path != "" {
			u.bySize = !u.bySize
			go u.reload()
		}
	case pt.In(g.prev):
		u.page = max(u.page-g.per, 0)
	case pt.In(g.next):
		if u.page+g.per < len(u.list.Entries) {
			u.page += g.per
		}
	default:
		for i, r := range g.rows {
			k := u.page + i
			if pt.In(r) && k < len(u.list.Entries) {
				u.held = k
				e := u.list.Entries[k]
				u.holdT = time.AfterFunc(filesHold, func() { u.longPress(k, e) })
			}
		}
	}
	u.changedLocked()
}

// touchUp は、離したとき。長押しでなければ、押していた行を開く。
func (u *FilesUI) touchUp(cancel bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	k := u.held
	u.held = -1
	if u.holdT != nil {
		u.holdT.Stop()
		u.holdT = nil
	}
	if cancel || k < 0 || u.longDone || k >= len(u.list.Entries) {
		u.changedLocked()
		return
	}
	e := u.list.Entries[k]
	switch {
	case e.Dir && !e.Link:
		u.dir, u.page = e.Path, 0
		go u.reload()
	case e.Link:
		u.sayLocked("シンボリックリンクはたどりません")
	default:
		u.openLocked(e)
	}
	u.changedLocked()
}

func (u *FilesUI) longPress(k int, e FileEntry) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.held != k {
		return
	}
	u.longDone = true
	u.held = -1
	if !u.list.Delete || u.list.Path == "" {
		u.sayLocked("この場所では消せません")
	} else {
		ee := e
		u.sel, u.mode = &ee, filesConfirm
	}
	u.changedLocked()
}

func (u *FilesUI) backLocked() {
	u.mode, u.sel, u.doc, u.img, u.starts, u.note = filesList, nil, nil, nil, nil, ""
}

// openLocked は、ファイルの中を見る（別の goroutine で読む）。
func (u *FilesUI) openLocked(e FileEntry) {
	ee := e
	u.sel = &ee
	u.doc, u.img, u.starts, u.pageText = nil, nil, nil, 0
	area := u.area
	switch {
	case isImageName(e.Name):
		u.mode, u.note = filesImage, "開いています…"
		go u.loadImage(ee, area)
	case isTextName(e.Name):
		if e.Size > filesTextMax {
			u.sayLocked(fmt.Sprintf("%s は大きいので Brain では開きません（PC で brain-deck files get）", humanSize(e.Size)))
			u.sel = nil
			return
		}
		u.mode, u.note = filesText, ""
		go u.loadText(ee, area)
	default:
		u.sayLocked("この種類のファイルは Brain では開けません（PC で brain-deck files get）")
		u.sel = nil
	}
}

// textDoc は、テキストを、行ごとの段落の読み物にする。
func textDoc(name string, b []byte) *ReadDoc {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "〓")
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\t", "    ")
	d := &ReadDoc{Format: 1, Title: name, Kind: "text", Flow: "book"}
	for _, ln := range strings.Split(s, "\n") {
		d.Blocks = append(d.Blocks, RBlock{T: "p", R: []RRun{{S: ln}}})
	}
	return d
}

func (u *FilesUI) loadText(e FileEntry, area image.Rectangle) {
	b, _, err := u.svc.Read(e.Path, 0, filesChunkMax)
	if err == nil && e.Size > int64(len(b)) {
		b, err = os.ReadFile(e.Path) // 大きさは filesTextMax までと確かめてある
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.sel == nil || u.sel.Path != e.Path {
		return
	}
	if err == nil && bytes.IndexByte(b[:min(len(b), 4096)], 0) >= 0 {
		err = fmt.Errorf("テキストのファイルではありません")
	}
	if err != nil {
		u.sayLocked("開けません：" + err.Error())
		u.backLocked()
		u.changedLocked()
		return
	}
	d := textDoc(e.Name, b)
	body := image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Max.Y-readFooterH)
	u.doc, u.starts = d, paginate(d, readMetricsFor(body, 24, false, false))
	u.changedLocked()
}

// fitScreen は、画像を area に入る、縦横比を保った大きさにする。
func fitScreen(w, h int, area image.Rectangle) (int, int) {
	aw, ah := area.Dx(), area.Dy()
	if w*ah > h*aw {
		return aw, max(h*aw/w, 1)
	}
	return max(w*ah/h, 1), ah
}

func decodeRGBA(b []byte) (imgconv.RGBA, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return imgconv.RGBA{}, err
	}
	r := img.Bounds()
	if r.Dx()*r.Dy() > filesImageMax {
		return imgconv.RGBA{}, fmt.Errorf("%d×%d は大きいので、Brain では開きません（%d 万画素まで。PC で brain-deck files get）", r.Dx(), r.Dy(), filesImageMax/10000)
	}
	n := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(n, n.Bounds(), img, r.Min, draw.Src)
	return imgconv.RGBA{W: r.Dx(), H: r.Dy(), Pix: n.Pix}, nil
}

func (u *FilesUI) loadImage(e FileEntry, area image.Rectangle) {
	t0 := time.Now()
	var out *Image
	var err error
	if e.Size > 64<<20 {
		err = fmt.Errorf("%s は大きいので、Brain では開きません", humanSize(e.Size))
	} else if b, rerr := os.ReadFile(e.Path); rerr != nil {
		err = rerr
	} else if strings.HasSuffix(strings.ToLower(e.Name), ".565") {
		var im *Image
		if im, err = parseImage(b); err == nil {
			out = im
			if im.W > area.Dx() || im.H > area.Dy() {
				w, h := fitScreen(im.W, im.H, area)
				out = photoForScreen(im, w, h)
			}
		}
	} else {
		// 画素の数を先に見て、重いものはそう出す
		if cfg, _, cerr := image.DecodeConfig(bytes.NewReader(b)); cerr == nil {
			if cfg.Width*cfg.Height > filesImageMax {
				err = fmt.Errorf("%d×%d は大きいので、Brain では開きません（%d 万画素まで。PC で brain-deck files get）", cfg.Width, cfg.Height, filesImageMax/10000)
			} else if cfg.Width*cfg.Height > filesImageSlow {
				u.mu.Lock()
				u.note = fmt.Sprintf("%d×%d の画像を変換しています（Brain では時間がかかります）…", cfg.Width, cfg.Height)
				u.changedLocked()
				u.mu.Unlock()
			}
		}
		if err == nil {
			var src imgconv.RGBA
			if src, err = decodeRGBA(b); err == nil {
				w, h := fitScreen(src.W, src.H, area)
				pix, _ := imgconv.Convert(src, w, h, imgconv.Params{CX: -1, CY: -1, Dither: true})
				out, _ = parseImage(imgconv.Encode(w, h, pix))
			}
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.sel == nil || u.sel.Path != e.Path {
		return
	}
	if err != nil {
		u.note = err.Error()
		u.changedLocked()
		return
	}
	u.img, u.note = out, ""
	vlogf("files: opened %s in %v", e.Path, time.Since(t0))
	u.changedLocked()
}

// menuLocked は、ほかのアプリで開く（足す）。
func (u *FilesUI) menuLocked(item string) {
	sel := u.sel
	switch item {
	case "一覧に戻る":
		u.backLocked()
		return
	case "やめる":
		u.mode = map[bool]int{true: filesText, false: filesImage}[u.doc != nil]
		return
	}
	u.mode = map[bool]int{true: filesText, false: filesImage}[u.doc != nil]
	u.note = "足しています…"
	doc := u.doc
	go func() {
		msg, err := u.addTo(item, sel, doc)
		u.mu.Lock()
		defer u.mu.Unlock()
		u.note = ""
		if err != nil {
			u.sayLocked("足せませんでした：" + err.Error())
		} else {
			u.sayLocked(msg)
		}
		u.changedLocked()
	}()
}

// addTo は、ファイルを読み物、写真、背景画像に足す。
func (u *FilesUI) addTo(item string, e *FileEntry, doc *ReadDoc) (string, error) {
	put := func(k UploadKind, data []byte, name string) (any, error) {
		if k.Check != nil {
			if err := k.Check(UploadBegin{Bytes: int64(len(data)), Name: name}); err != nil {
				return nil, err
			}
		}
		if err := os.MkdirAll(k.Dir, 0o755); err != nil {
			return nil, err
		}
		part := filepath.Join(k.Dir, ".upload-files"+newToken()+".part")
		if err := os.WriteFile(part, data, 0o644); err != nil {
			return nil, err
		}
		f, err := os.Open(part)
		if err != nil {
			os.Remove(part)
			return nil, err
		}
		s := sha256.Sum256(data)
		return k.Done(UploadBegin{Bytes: int64(len(data)), Name: name, SHA256: hex.EncodeToString(s[:]), Source: "files"}, f, part)
	}
	switch item {
	case "読み物に足す":
		if u.reads == nil || doc == nil {
			return "", fmt.Errorf("読み物がありません")
		}
		b, _ := json.Marshal(doc)
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(b)
		zw.Close()
		if _, err := put(u.reads.UploadKind(), buf.Bytes(), doc.Title); err != nil {
			return "", err
		}
		if u.added != nil {
			u.added()
		}
		return "読み物に足しました：" + doc.Title, nil
	case "写真に足す", "背景画像に足す":
		b, err := os.ReadFile(e.Path)
		if err != nil {
			return "", err
		}
		var src imgconv.RGBA
		if strings.HasSuffix(strings.ToLower(e.Name), ".565") {
			w, h, pix, err := imgconv.Decode(b)
			if err != nil {
				return "", err
			}
			src = imgconv.RGBA{W: w, H: h, Pix: make([]byte, w*h*4)}
			for i, v := range pix {
				r, g, bl := imgconv.Unpack(v)
				src.Pix[i*4], src.Pix[i*4+1], src.Pix[i*4+2], src.Pix[i*4+3] = r, g, bl, 255
			}
		} else if src, err = decodeRGBA(b); err != nil {
			return "", err
		}
		tw, th := 800, 480
		if src.H > src.W {
			tw, th = 480, 800
		}
		if item == "背景画像に足す" {
			if u.images == nil {
				return "", fmt.Errorf("背景画像の置き場所がありません")
			}
			pix, _ := imgconv.Convert(src, tw, th, imgconv.Params{CX: -1, CY: -1, Brightness: -35, Dither: true})
			res, err := put(u.images.UploadKind(), imgconv.Encode(tw, th, pix), e.Name)
			if err != nil {
				return "", err
			}
			id := ""
			if m, ok := res.(map[string]any); ok {
				id, _ = m["id"].(string)
			}
			return fmt.Sprintf("背景画像に足しました（%s。設定 GUI で場所を選ぶ）", id), nil
		}
		if u.photos == nil {
			return "", fmt.Errorf("写真の置き場所がありません")
		}
		pix, _ := imgconv.Convert(src, tw, th, imgconv.Params{CX: -1, CY: -1, Dither: true})
		if _, err := put(u.photos.UploadKind(), imgconv.Encode(tw, th, pix), strings.TrimSuffix(e.Name, filepath.Ext(e.Name))); err != nil {
			return "", err
		}
		return "写真に足しました", nil
	}
	return "", fmt.Errorf("知らない項目 %q", item)
}

func compileFiles(s ActionSpec) (*WidgetDef, error) {
	if s.count() > 0 {
		return nil, fmt.Errorf("widget: files handles taps itself; remove key and layer_*")
	}
	return &WidgetDef{Kind: widgetFiles}, nil
}

// renderFilesView は、-render-files の画面（path のフォルダの一覧か、ファイルの中）を、待って作る。
func renderFilesView(svc *FilesService, path string, bySize bool, area image.Rectangle) (*FilesView, error) {
	u := &FilesUI{svc: svc, held: -1, area: area, bySize: bySize}
	if path == "/" {
		path = ""
	}
	fi, err := os.Stat(path)
	if path != "" && err == nil && !fi.IsDir() {
		e := FileEntry{Name: filepath.Base(path), Path: path, Size: fi.Size(), MTime: fi.ModTime()}
		u.sel = &e
		if isImageName(e.Name) {
			u.mode = filesImage
			u.loadImage(e, area)
		} else {
			u.mode = filesText
			u.loadText(e, area)
		}
		return u.Snapshot(), nil
	}
	u.dir = path
	l, err := svc.List(path, bySize, bySize)
	if err != nil {
		return nil, err
	}
	u.setListLocked(l, nil)
	return u.Snapshot(), nil
}
