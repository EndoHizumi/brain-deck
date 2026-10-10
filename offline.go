package main

import (
	"errors"
	"image"
	"log"
	"time"
)

// ---------- PC から外したとき（offline） ----------
//
// PC とつながっていないあいだ（link.go の linkOnline が false）は、PC に送るもの（キー、マウス、メディアキー、
// トラックパッド、端末モード）を休みにする。押しても何も送らず、画面の下に「PC につながっていません」と出す。
// そのセルは薄く描き、下に「PCなし」と出す（PC から受け取るテキストとグラフのタイルにも、隅に「PCなし」）。
// 外れたときに押していたキーとマウスのボタンは離す。
//
// 設定の offline.home を書けば、外れたときにホーム画面に切り替え、つなぎ直したら外れる前のレイヤーに戻す
// （ホーム画面のまま触っていなければ。ほかのアプリに入っていたら、そのまま）。

// OfflineConfig は設定の offline。
type OfflineConfig struct {
	// Home は、PC から外れたらホーム画面に切り替えるか（home を書いたときだけ）。既定 false
	Home bool `yaml:"home,omitempty" json:"home,omitempty"`
}

const (
	offlineLabel      = "PCなし"                  // 休みにしたセルの下と、タイルの隅に出す文字
	offlineNoticeText = "PC につながっていません"         // 休みのものを押したときに、画面の下に出す
	offlineNoticeGap  = 1500 * time.Millisecond // 続けて押したときに、出し直すまで
)

var errOffline = errors.New("PC につながっていません")

// needsPC は、PC とつながっていないと働かない割り当てか。
func (a *Action) needsPC() bool {
	if a == nil {
		return false
	}
	switch a.Kind {
	case actKey, actMouse, actMedia, actTerminal:
		return true
	}
	return a.Widget != nil && a.Widget.Kind == widgetPad
}

// widgetFromPC は、中身を PC から受け取って出すタイル（外しているあいだは更新されない）か。
func (w *WidgetDef) fromPC() bool {
	return w != nil && (w.Kind == widgetText || w.Kind == widgetGraph)
}

// SetOffline は、PC から外れた（true）・つながった（false）ときに呼ぶ。
func (e *Engine) SetOffline(off bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.offline == off {
		return
	}
	e.offline = off
	h := e.km.Home
	if off {
		e.out.releaseAll()
		e.pad.Cancel()
		if h != nil && e.km.OfflineHome && !e.term.Active() && (e.view == nil || e.view.Top != h.Layer) {
			e.autoHome = append([]stackEntry{}, e.stack...)
			e.stack = []stackEntry{{layer: h.Layer, kind: actTo}}
			h.SetPage(0)
			log.Printf("offline: switched to the home screen")
		}
	} else if e.autoHome != nil {
		if h != nil && len(e.stack) == 1 && e.stack[0].layer == h.Layer {
			e.stack = e.autoHome
			log.Printf("offline: back to %s", e.describeStack(e.stack))
		}
		e.autoHome = nil
	}
	e.refresh()
}

// Offline は、PC から外れているか。
func (e *Engine) Offline() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.offline
}

// SetNotify は、画面の下に知らせを出す関数を設定する。待たずに返ること。
func (e *Engine) SetNotify(f func(msg string)) {
	e.mu.Lock()
	e.notify = f
	e.mu.Unlock()
}

// offlineNoticeLocked は、休みのものを押したことを知らせる（続けて押したときは出し直さない）。
func (e *Engine) offlineNoticeLocked() {
	if e.notify == nil || time.Since(e.offNoticeAt) < offlineNoticeGap {
		return
	}
	e.offNoticeAt = time.Now()
	e.notify(offlineNoticeText)
}

func (e *Engine) describeStack(st []stackEntry) string {
	if len(st) == 0 {
		return e.km.Layers[0].Name
	}
	return e.km.Layers[st[len(st)-1].layer].Name
}

// drawOfflineTag は、タイルの右下の隅に「PCなし」の札を描く（PC から外れているあいだ）。
func drawOfflineTag(cv *Canvas, inner image.Rectangle) {
	w := font.textWidth(offlineLabel) + 4
	h := fontH + 2
	r := image.Rect(inner.Max.X-w, inner.Max.Y-h, inner.Max.X, inner.Max.Y).Intersect(inner)
	cv.fill(r, colBG)
	cv.text(r.Min.X+2, r.Min.Y+1, offlineLabel, 1, colUnsynced, r)
}
