// Todo のウィジェット（widget: todo）。todowidget.go の todoGeometry、todoOrder、todoEllipsis と同じ結果にする。
// 項目は Brain の /var/lib/lefthand/todo.json にあり、設定 GUI の「Todo」タブか brain-deck todo で書き換える。

import { FONT_H, type BitmapFont } from './font'
import type { TodoItem } from './types'

export const TODO_ROW_H = 52 // rows を書かないときの、1 行の高さ
export const TODO_NAV_H = 44 // ページ送りの帯の高さ
export const TODO_PAD = 6
export const TODO_MAX_TEXT_SCALE = 3
export const TODO_MAX_ROWS = 20
export const TODO_EMPTY = 'Todo はありません'
export const TODO_MAX_RUNES = 200
const ELLIPSIS = '…'

export interface Rect {
  x0: number
  y0: number
  x1: number
  y1: number
}

export interface TodoGeom {
  rows: Rect[] // 1 ページの行（per 個）
  per: number
  pages: number
  scale: number // 項目の文字の倍率
  nav: Rect | null // ページ送りの帯。1 ページに収まれば null
  up: Rect | null
  mid: Rect | null
  down: Rect | null
}

// todoOrder は、画面に出す順（未完了を並べた順に、そのあとに完了したもの）。
export function todoOrder(items: TodoItem[]): TodoItem[] {
  return [...items.filter((i) => !i.done), ...items.filter((i) => i.done)]
}

// todoGeometry は、見出しの下の範囲 area に n 個の項目を並べる配置を決める。
export function todoGeometry(area: Rect, n: number, rows = 0): TodoGeom {
  const ah = area.y1 - area.y0
  const aw = area.x1 - area.x0
  const fit = (hh: number) => (rows > 0 ? rows : Math.max(Math.trunc(hh / TODO_ROW_H), 1))
  let per = fit(ah)
  let navH = 0
  if (n > per) {
    navH = Math.min(TODO_NAV_H, Math.trunc(ah / 2))
    per = fit(ah - navH)
  }
  const rowH = Math.max(Math.trunc((ah - navH) / per), 1)
  const g: TodoGeom = { rows: [], per, pages: Math.max(Math.trunc((n + per - 1) / per), 1), scale: 1, nav: null, up: null, mid: null, down: null }
  for (let i = 0; i < per; i++) g.rows.push({ x0: area.x0, y0: area.y0 + i * rowH, x1: area.x1, y1: area.y0 + (i + 1) * rowH })
  for (let s = TODO_MAX_TEXT_SCALE; s > 1; s--) {
    if (FONT_H * s <= rowH - 8) {
      g.scale = s
      break
    }
  }
  if (navH > 0) {
    const nav = { x0: area.x0, y0: area.y1 - navH, x1: area.x1, y1: area.y1 }
    const a = area.x0 + Math.trunc((aw * 3) / 8)
    const b = area.x1 - Math.trunc((aw * 3) / 8)
    g.nav = nav
    g.up = { ...nav, x1: a }
    g.mid = { ...nav, x0: a, x1: b }
    g.down = { ...nav, x0: b }
  }
  return g
}

// todoEllipsis は、s が等倍で幅 w に入らなければ、入るところまでで切って … を付ける。
export function todoEllipsis(font: BitmapFont, s: string, w: number): string {
  if (font.textWidth(s) <= w) return s
  let r = [...s]
  while (r.length > 0 && font.textWidth(r.join('') + ELLIPSIS) > w) r = r.slice(0, -1)
  return r.join('') + ELLIPSIS
}
