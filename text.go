package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------- テキストのタイル（widget: text）の中身 ----------
//
// 中身は PC から set_text で書き換える（brain-deck text ...）。設定ファイルには書かず、
// /var/lib/lefthand/text.json に名前（セルの id）ごとに残す。設定 GUI で設定を保存しても消えない。
// SD カードへの書き込みは遅いことがあるので、返事を書き込みの完了まで待たせない（保存は 1 つの goroutine がまとめて行う）。

const (
	textStoreName  = "text"
	textMaxRunes   = 200 // 1 つのテキストの長さの上限（文字数）
	textMaxLines   = 8   // 改行で分けた行数の上限
	textMaxEntries = 64  // 覚えておくテキストの数。超えたら、いちばん古いものを捨てる
	textMaxTTL     = 30 * 24 * time.Hour
)

// テキストの色の種類
const (
	textNormal = "normal"
	textOK     = "ok"
	textError  = "error"
	textWarn   = "warn"
)

var textStyles = []string{textNormal, textOK, textError, textWarn}

var textIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// validTextID は、テキストの id（セルの id と、set_text の name）が使える形かを調べる。
func validTextID(id string) bool { return textIDPattern.MatchString(id) }

func validTextStyle(s string) bool {
	for _, x := range textStyles {
		if s == x {
			return true
		}
	}
	return false
}

// TextEntry は 1 つのテキストの中身（text.json の 1 項目）。
type TextEntry struct {
	Text      string     `json:"text"`
	Style     string     `json:"style"`
	SetAt     time.Time  `json:"set_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // これを過ぎたら薄く表示する（消さない）
	Source    string     `json:"source,omitempty"`
	// ClockUnset は、Brain の時刻を合わせる前に書いたこと。set_at と expires_at は合っていない時計で決めたので、
	// あとで時刻を合わせたときに、そのずれの分だけ動かす（有効期限を、書いてからの経過時間として保つ）
	ClockUnset bool `json:"clock_unset,omitempty"`
}

// Expired は、t の時点で有効期限が過ぎているか。
func (e TextEntry) Expired(t time.Time) bool { return e.ExpiresAt != nil && !t.Before(*e.ExpiresAt) }

// TextService はテキストの中身を持ち、保存する。
type TextService struct {
	mu       sync.Mutex
	m        map[string]TextEntry
	store    *Store
	onChange func() // 中身が変わったとき（画面の描き直し）。待たずに返ること
	dirty    chan struct{}
	saved    chan struct{} // テスト用：保存を 1 回終えるたびに送る（nil なら送らない）
}

// textFile は text.json の形。
type textFile struct {
	Texts map[string]TextEntry `json:"texts"`
}

func NewTextService(store *Store) *TextService {
	ts := &TextService{m: map[string]TextEntry{}, store: store, dirty: make(chan struct{}, 1)}
	var f textFile
	if err := store.Load(textStoreName, &f); err == nil {
		for id, e := range f.Texts {
			if validTextID(id) {
				if !validTextStyle(e.Style) {
					e.Style = textNormal
				}
				ts.m[id] = e
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("text: %v", err)
	}
	go ts.saver()
	return ts
}

// SetOnChange は、中身が変わったときに呼ぶ関数を設定する。
func (ts *TextService) SetOnChange(f func()) {
	ts.mu.Lock()
	ts.onChange = f
	ts.mu.Unlock()
}

// Snapshot は、今の中身の写しを返す（描画の goroutine が描き直すたびに使う）。
func (ts *TextService) Snapshot() map[string]TextEntry {
	if ts == nil {
		return nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make(map[string]TextEntry, len(ts.m))
	for k, v := range ts.m {
		out[k] = v
	}
	return out
}

// TextRequest は set_text の引数。
type TextRequest struct {
	Name   string
	Text   string
	Style  string
	TTL    time.Duration // 0 なら期限なし
	Clear  bool
	Source string
}

var errBadText = errors.New("bad text")

// normalizeText は、表示できる形にそろえる。\r を除き、タブを空白にする。ほかの制御文字は誤り。
func normalizeText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: text is not valid UTF-8", errBadText)
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", " ")
	for _, r := range s {
		if r != '\n' && unicode.IsControl(r) {
			return "", fmt.Errorf("%w: text contains a control character %U", errBadText, r)
		}
	}
	if n := utf8.RuneCountInString(s); n > textMaxRunes {
		return "", fmt.Errorf("%w: text is %d characters (at most %d)", errBadText, n, textMaxRunes)
	}
	if n := strings.Count(s, "\n") + 1; n > textMaxLines {
		return "", fmt.Errorf("%w: text has %d lines (at most %d)", errBadText, n, textMaxLines)
	}
	return s, nil
}

// Set はテキストを書き換える（Clear なら消す）。clockSet は、Brain の時刻を合わせてあるか。
// 消したときは、ok=false の TextEntry を返す。
func (ts *TextService) Set(req TextRequest, now time.Time, clockSet bool) (TextEntry, bool, error) {
	if !validTextID(req.Name) {
		return TextEntry{}, false, fmt.Errorf("%w: name %q must be 1-32 characters of A-Z a-z 0-9 _ . -", errBadText, req.Name)
	}
	if req.Clear {
		ts.mu.Lock()
		_, had := ts.m[req.Name]
		delete(ts.m, req.Name)
		ts.mu.Unlock()
		if had {
			ts.changed()
		}
		return TextEntry{}, false, nil
	}
	text, err := normalizeText(req.Text)
	if err != nil {
		return TextEntry{}, false, err
	}
	style := req.Style
	if style == "" {
		style = textNormal
	}
	if !validTextStyle(style) {
		return TextEntry{}, false, fmt.Errorf("%w: unknown style %q (%s)", errBadText, style, strings.Join(textStyles, ", "))
	}
	if req.TTL < 0 || req.TTL > textMaxTTL {
		return TextEntry{}, false, fmt.Errorf("%w: ttl must be between 1 second and %v", errBadText, textMaxTTL)
	}
	e := TextEntry{Text: text, Style: style, SetAt: now.UTC(), Source: req.Source, ClockUnset: !clockSet}
	if req.TTL > 0 {
		exp := now.Add(req.TTL).UTC()
		e.ExpiresAt = &exp
	}
	ts.mu.Lock()
	ts.m[req.Name] = e
	ts.evictLocked()
	ts.mu.Unlock()
	ts.changed()
	return e, true, nil
}

// evictLocked は、数が上限を超えたら、いちばん前に書いたものから捨てる。
func (ts *TextService) evictLocked() {
	for len(ts.m) > textMaxEntries {
		oldest := ""
		for id, e := range ts.m {
			if oldest == "" || e.SetAt.Before(ts.m[oldest].SetAt) {
				oldest = id
			}
		}
		log.Printf("text: more than %d texts, dropping %q", textMaxEntries, oldest)
		delete(ts.m, oldest)
	}
}

// ClockStepped は、時刻合わせでシステムの時刻が off だけ動いたときに呼ぶ。
// 時刻を合わせる前に書いたテキストの時刻を、同じだけ動かす。
func (ts *TextService) ClockStepped(off time.Duration) {
	if ts == nil {
		return
	}
	n := 0
	ts.mu.Lock()
	for id, e := range ts.m {
		if !e.ClockUnset {
			continue
		}
		e.SetAt = e.SetAt.Add(off)
		if e.ExpiresAt != nil {
			t := e.ExpiresAt.Add(off)
			e.ExpiresAt = &t
		}
		e.ClockUnset = false
		ts.m[id] = e
		n++
	}
	ts.mu.Unlock()
	if n > 0 {
		log.Printf("text: shifted %d texts written before the clock was set by %v", n, off.Round(time.Millisecond))
		ts.changed()
	}
}

// ClockSynced は、時刻を合わせた（ずれが小さく、動かさなかった）ときに呼ぶ。
// 合わせる前に書いたテキストの時刻は、そのままで正しかったことになる。
func (ts *TextService) ClockSynced() {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	n := 0
	for id, e := range ts.m {
		if e.ClockUnset {
			e.ClockUnset = false
			ts.m[id] = e
			n++
		}
	}
	ts.mu.Unlock()
	if n > 0 {
		ts.changed()
	}
}

// changed は保存を頼み、画面を描き直させる。
func (ts *TextService) changed() {
	select {
	case ts.dirty <- struct{}{}:
	default: // すでに頼んである。保存する側は、そのときの最新を書く
	}
	ts.mu.Lock()
	f := ts.onChange
	ts.mu.Unlock()
	if f != nil {
		f()
	}
}

// saver は、変わるたびに text.json を書く。書いているあいだの変更は、次の 1 回にまとめる。
func (ts *TextService) saver() {
	for range ts.dirty {
		f := textFile{Texts: ts.Snapshot()}
		if err := ts.store.Save(textStoreName, f); err != nil {
			log.Printf("text: save %s: %v", textStoreName, err)
		}
		if ts.saved != nil {
			ts.saved <- struct{}{}
		}
	}
}

// TextInfo は get_text と set_text で返す、1 つのテキストの状態。
type TextInfo struct {
	TextEntry
	Expired bool `json:"expired"`
}

// List は、名前の順にすべてのテキストを返す。
func (ts *TextService) List(now time.Time) map[string]TextInfo {
	out := map[string]TextInfo{}
	for id, e := range ts.Snapshot() {
		out[id] = TextInfo{TextEntry: e, Expired: e.Expired(now)}
	}
	return out
}

// textIDs は、設定のどこかでテキストのセルに使われている id を、名前の順に返す。
func textIDs(cfg *Config) []string {
	seen := map[string]bool{}
	for _, l := range cfg.Layers {
		if l.Touch == nil {
			continue
		}
		for _, a := range l.Touch.Cells {
			if a.Widget == widgetText && a.ID != "" {
				seen[a.ID] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
