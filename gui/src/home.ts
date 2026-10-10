// ホーム画面の配置（home.go と同じ）と、組み込みのアイコン（icons.go。表は icons.json）。描くのは preview.ts の drawHome。

import iconsData from './icons.json'
import type { BitmapFont } from './font'
import type { HomeConfig } from './types'

export const HOME_NAV_H = 40
export const HOME_ICON_BASE = 16
export const HOME_ICON_SCALE = 4
export const HOME_IMAGE_ICON = 64 // 自分の画像のアイコンの大きさ
export const HOME_SLOT_PAD = 4
export const HOME_NAME_GAP = 4
export const HOME_ARROW = 20
export const HOME_LABEL = 'ホーム'
export const HOME_LAYER = 'home'

export const ICON_NAMES: string[] = iconsData.names
export const ICON_ROWS: Record<string, string[]> = iconsData.icons
export const ICON_PALETTE: Record<string, [number, number, number]> = iconsData.palette as any

interface Rect {
  x0: number
  y0: number
  x1: number
  y1: number
}

export function cellSpan(i: number, n: number, size: number): [number, number] {
  return [Math.floor((i * size + n - 1) / n), Math.floor(((i + 1) * size + n - 1) / n)]
}

// homeGrid は、向きに合った 1 ページの格子（home.go の compileHome と HomeDef.grid）。
export function homeGrid(h: HomeConfig, portrait: boolean): [number, number] {
  let cols = h.cols ?? 0
  let rows = h.rows ?? 0
  if (!cols && !rows) [cols, rows] = [4, 3]
  if (portrait) return h.portrait ? [h.portrait.cols, h.portrait.rows] : [rows, cols]
  return [cols, rows]
}

export interface HomeGeom {
  slots: Rect[]
  per: number
  pages: number
  nav: Rect | null
  prev: Rect | null
  next: Rect | null
  mid: Rect | null
  page: number
  first: number
  last: number
}

// homeGeometry は home.go の homeGeometry と同じ。
export function homeGeometry(area: Rect, h: HomeConfig, portrait: boolean, page: number): HomeGeom {
  const [cols, rows] = homeGrid(h, portrait)
  const per = Math.max(cols * rows, 1)
  const n = h.apps.length
  const pages = Math.max(Math.ceil(n / per), 1)
  const g: HomeGeom = { slots: [], per, pages, nav: null, prev: null, next: null, mid: null, page: Math.min(Math.max(page, 0), pages - 1), first: 0, last: 0 }
  const icons = { ...area }
  if (pages > 1) {
    icons.y1 -= HOME_NAV_H
    const nav = { x0: area.x0, y0: icons.y1, x1: area.x1, y1: area.y1 }
    const third = Math.floor((nav.x1 - nav.x0) / 3)
    g.nav = nav
    g.prev = { x0: nav.x0, y0: nav.y0, x1: nav.x0 + third, y1: nav.y1 }
    g.next = { x0: nav.x1 - third, y0: nav.y0, x1: nav.x1, y1: nav.y1 }
    g.mid = { x0: g.prev.x1, y0: nav.y0, x1: g.next.x0, y1: nav.y1 }
  }
  for (let r = 0; r < rows; r++)
    for (let c = 0; c < cols; c++) {
      const [x0, x1] = cellSpan(c, cols, icons.x1 - icons.x0)
      const [y0, y1] = cellSpan(r, rows, icons.y1 - icons.y0)
      g.slots.push({ x0: icons.x0 + x0, y0: icons.y0 + y0, x1: icons.x0 + x1, y1: icons.y0 + y1 })
    }
  g.first = g.page * per
  g.last = Math.min(g.first + per, n)
  return g
}

// homeHit は、点がどのアプリか（GUI のプレビューでアプリを選ぶ）。
export function homeHit(g: HomeGeom, x: number, y: number): number {
  const inR = (r: Rect) => x >= r.x0 && x < r.x1 && y >= r.y0 && y < r.y1
  for (let i = 0; i < g.slots.length; i++) if (inR(g.slots[i]) && g.first + i < g.last) return g.first + i
  return -1
}

// homeName は home.go の homeName と同じ。
export function homeName(font: BitmapFont, name: string, w: number): [string, number] {
  if (font.textWidth(name) * 2 <= w) return [name, 2]
  if (font.textWidth(name) <= w) return [name, 1]
  const rs = [...name]
  while (rs.length && font.textWidth(rs.join('') + '…') > w) rs.pop()
  return [rs.join('') + '…', 1]
}

// homeIconFit は、組み込みのアイコンの倍率（home.go の homeIconFit）。
export function homeIconFit(box: Rect): number {
  return Math.max(Math.min(HOME_ICON_SCALE, Math.trunc((box.x1 - box.x0) / HOME_ICON_BASE), Math.trunc((box.y1 - box.y0) / HOME_ICON_BASE)), 1)
}

export const isImageId = (s: string | undefined) => !!s && /^[0-9a-f]{16}$/.test(s)
