// 天気のウィジェット（widget: weather）。weatherwidget.go の wthLayout と同じ「描くものの並び」を作る。
// 予報は Brain の /var/lib/lefthand/weather.json にあり、PC の brain-deck weather sync が送る（データの元は気象庁）。
// 天気アイコンは Brain にあるもの（get_weather_icon）を使う。ないアイコンは描かない（Brain と同じ）。
// 時刻と日付は、PC のタイムゾーンで決める（calwidget.ts と同じ前提）。

import { FONT_H, type BitmapFont } from './font'
import { UNSYNCED_TEXT } from './clock'
import { calDate, dayKey, parseDuration } from './calwidget'
import { todoEllipsis, type Rect } from './todowidget'
import type { WeatherData, WeatherDay } from './types'

const WTH_COL_MIN = 140
const WTH_COL_GAP = 8
const WTH_LINE_MODE = 48
const WTH_RAIN_ROW = 96
const WTH_RAIN_LABELS = 150
const WTH_REF_W = 48
const WTH_ICON_MIN = 8
const WTH_MAX_SCALE = 3
const CAL_FOOTER_BIG = 240
const WIDGET_LINE_GAP = 4
export const WTH_NO_DATA = '天気を受け取っていません'
export const WTH_FAILED_DATA = '天気を取得できません'
const WTH_NO_DAY = 'データなし'
const WTH_NULL = '--'
const WTH_SOURCE = '気象庁'
const WTH_SOURCE_LONG = '出典:気象庁'
const WTH_RAIN_SLOTS = ['0-6', '6-12', '12-18', '18-24']
export const DEFAULT_WTH_STALE_MS = 3 * 3600_000

// 色（preview.ts と weatherwidget.go と同じ）
const colText = '#ffffff'
const colSub = '#96a4b4'
const colTodoDone = '#6a7480'
const colTodoRule = '#303c4c'
const colWarn = '#ff8020'
const colWthMax = '#ff8a50'
const colWthMin = '#6cb4ff'
const colWthRain1 = '#a8d4ff'
const colWthRain2 = '#4f9dff'

export interface WthItem {
  kind: 'text' | 'icon' | 'rule'
  x: number
  y: number
  s?: number
  w?: number
  h?: number
  text?: string
  color?: string
  icon?: string
}

export interface WeatherWidget {
  staleMs: number // 0 なら古いと出さない
}

export function weatherWidgetOf(a: { stale?: string }): WeatherWidget {
  const st = a.stale ? parseDuration(a.stale) : null
  return { staleMs: st ?? DEFAULT_WTH_STALE_MS }
}

interface Seg {
  text: string
  c: string
}

const div = (a: number, b: number) => Math.trunc(a / b)

const dayStart = (t: number) => {
  const d = new Date(t)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}
const addDays = (t: number, n: number) => {
  const d = new Date(t)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n).getTime()
}

export const iconName = (code: string, w: number, h: number) => `${code}-${w}x${h}`

function failed(d: WeatherData | null): boolean {
  if (!d || !d.error || !d.error_at) return false
  return !d.fetched_at || Date.parse(d.error_at) >= Date.parse(d.fetched_at)
}

function temp(t: number | null | undefined, c: string): Seg {
  return t === null || t === undefined ? { text: WTH_NULL, c: colSub } : { text: `${t}°`, c }
}

function temps(d: WeatherDay): Seg[] {
  return [temp(d.max, colWthMax), { text: '/', c: colSub }, temp(d.min, colWthMin)]
}

function rainColor(p: number | null | undefined): string {
  if (p === null || p === undefined || p < 20) return colSub
  return p < 50 ? colWthRain1 : colWthRain2
}

interface Geom {
  hs: number
  rs: number
  rows: number
  mid: Rect
  textH: number
}

function colGeom(font: BitmapFont, c: Rect): Geom {
  const cw = c.x1 - c.x0
  const ch = c.y1 - c.y0
  const g: Geom = { hs: 1, rs: 1, rows: 0, mid: c, textH: 0 }
  if (cw >= CAL_FOOTER_BIG && ch >= 200) g.hs = 2
  g.rs = g.hs
  while (g.rs > 1 && font.textWidth('18-24') * g.rs > div(cw, 4) - 2) g.rs--
  if (ch >= WTH_RAIN_LABELS) g.rows = 2
  else if (ch >= WTH_RAIN_ROW) g.rows = 1
  const rainH = g.rows > 0 ? g.rows * FONT_H * g.rs + (g.rows - 1) + 2 : 0
  g.mid = { x0: c.x0, y0: c.y0 + FONT_H * g.hs + 2, x1: c.x1, y1: c.y1 - rainH }
  return g
}

const EMPTY: Rect = { x0: 0, y0: 0, x1: 0, y1: 0 }
const isEmpty = (r: Rect) => r.x0 >= r.x1 || r.y0 >= r.y1

// iconBox は weatherwidget.go の wthIconBox と同じ。列の大きさだけで、アイコンの箱を決める。
export function iconBox(font: BitmapFont, c: Rect): { box: Rect; mode: 'line' | 'tall' | 'side'; g: Geom } {
  const cw = c.x1 - c.x0
  const ch = c.y1 - c.y0
  if (ch < WTH_LINE_MODE) {
    let ih = ch
    let iw = div(ih * 3, 2)
    if (iw > div(cw, 3)) {
      iw = div(cw, 3)
      ih = div(iw * 2, 3)
      iw = div(ih * 3, 2)
    }
    const g = { hs: 0, rs: 0, rows: 0, mid: EMPTY, textH: 0 }
    if (ih < WTH_ICON_MIN) return { box: EMPTY, mode: 'line', g }
    const y = c.y0 + div(ch - ih, 2)
    return { box: { x0: c.x0, y0: y, x1: c.x0 + iw, y1: y + ih }, mode: 'line', g }
  }
  const g = colGeom(font, c)
  const m = g.mid
  const mh = m.y1 - m.y0
  if (mh >= div(cw * 3, 4)) {
    let sr = Math.max(Math.min(WTH_MAX_SCALE, div(cw, WTH_REF_W)), 1)
    while (sr > 1 && 2 * FONT_H * sr + 2 > div(mh, 2)) sr--
    const textH = 2 * FONT_H * sr + 2
    const ih = Math.min(mh - textH - 2, div(cw * 2, 3))
    const iw = div(ih * 3, 2)
    g.textH = textH
    if (ih < WTH_ICON_MIN) return { box: EMPTY, mode: 'tall', g }
    const top = m.y0 + div(mh - (ih + 2 + textH), 2)
    const x = c.x0 + div(cw - iw, 2)
    return { box: { x0: x, y0: top, x1: x + iw, y1: top + ih }, mode: 'tall', g }
  }
  let iw = Math.min(div(mh * 3, 2), div(cw, 2))
  const ih = div(iw * 2, 3)
  iw = div(ih * 3, 2)
  if (ih < WTH_ICON_MIN) return { box: EMPTY, mode: 'side', g }
  const y = m.y0 + div(mh - ih, 2)
  return { box: { x0: c.x0, y0: y, x1: c.x0 + iw, y1: y + ih }, mode: 'side', g }
}

// wthLayout は weatherwidget.go の wthLayout と同じ。area は見出しの下の範囲。
export function wthLayout(font: BitmapFont, area: Rect, w: WeatherWidget, d: WeatherData | null, now: number, synced: boolean): WthItem[] {
  const out: WthItem[] = []
  const text = (x: number, y: number, s: number, t: string, c: string) => {
    if (t.trim() !== '') out.push({ kind: 'text', x, y, s, text: t, color: c })
  }
  const centered = (r: Rect, y: number, s: number, t: string, c: string) => {
    const x = r.x0 + div(r.x1 - r.x0 - font.textWidth(t) * s, 2)
    text(Math.max(x, r.x0), y, s, t, c)
  }
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  if (!d || !d.days?.length) {
    const msg = failed(d) ? WTH_FAILED_DATA : WTH_NO_DATA
    const s = Math.max(Math.min(fitScale(font, msg, aw, ah), 2), 1)
    centered(area, area.y0 + div(ah - FONT_H * s, 2), s, todoEllipsis(font, msg, div(aw, s)), colSub)
    return out
  }
  const fs = aw >= CAL_FOOTER_BIG && ah >= CAL_FOOTER_BIG ? 2 : 1
  let body = { ...area, y1: area.y1 - (FONT_H * fs + WIDGET_LINE_GAP) }
  let footer = true
  if (body.y1 - body.y0 < FONT_H) {
    body = { ...area }
    footer = false
  }
  const today = dayStart(now)
  let cols: Rect[] = [body]
  const bw = body.x1 - body.x0
  if (bw >= 2 * WTH_COL_MIN + WTH_COL_GAP) {
    const cw = div(bw - WTH_COL_GAP, 2)
    cols = [
      { x0: body.x0, y0: body.y0, x1: body.x0 + cw, y1: body.y1 },
      { x0: body.x1 - cw, y0: body.y0, x1: body.x1, y1: body.y1 },
    ]
    out.push({ kind: 'rule', x: body.x0 + cw + div(WTH_COL_GAP, 2) - 1, y: body.y0, w: 1, h: body.y1 - body.y0, color: colTodoRule })
  }
  cols.forEach((c, i) => {
    const date = addDays(today, i)
    const day = d.days.find((x) => x.date === dayKey(date)) ?? null
    out.push(...column(font, c, i === 0 ? '今日' : '明日', date, day, now, i === 0))
  })
  if (footer) {
    const { text: f, warn } = wthFooter(font, w, d, now, synced, div(aw, fs))
    text(area.x0, body.y1 + WIDGET_LINE_GAP, fs, f, warn ? colWarn : colSub)
  }
  return out
}

function fitScale(font: BitmapFont, s: string, w: number, h: number): number {
  const tw = font.textWidth(s)
  let sc = 6
  while (sc > 1 && (tw * sc > w || FONT_H * sc > h)) sc--
  return sc
}

function segsWidth(font: BitmapFont, segs: Seg[]): number {
  return segs.reduce((n, s) => n + font.textWidth(s.text), 0)
}

function column(font: BitmapFont, c: Rect, label: string, date: number, d: WeatherDay | null, now: number, isToday: boolean): WthItem[] {
  const out: WthItem[] = []
  const text = (x: number, y: number, s: number, t: string, col: string) => {
    if (t.trim() !== '') out.push({ kind: 'text', x, y, s, text: t, color: col })
  }
  const segs = (x: number, y: number, s: number, ss: Seg[]) => {
    for (const g of ss) {
      text(x, y, s, g.text, g.c)
      x += font.textWidth(g.text) * s
    }
  }
  const centered = (r: Rect, y: number, s: number, t: string, col: string) => {
    const x = r.x0 + div(r.x1 - r.x0 - font.textWidth(t) * s, 2)
    text(Math.max(x, r.x0), y, s, t, col)
  }
  const cw = c.x1 - c.x0
  const ch = c.y1 - c.y0
  const { box, mode, g } = iconBox(font, c)
  const icon = () => {
    if (d && d.icon && !isEmpty(box)) {
      const bw = box.x1 - box.x0
      const bh = box.y1 - box.y0
      out.push({ kind: 'icon', x: box.x0, y: box.y0, w: bw, h: bh, icon: iconName(d.icon, bw, bh) })
    }
  }
  let telop = WTH_NO_DAY
  if (d) telop = d.telop || WTH_NULL
  if (mode === 'line') {
    let x = c.x0
    if (d && !isEmpty(box)) {
      icon()
      x = box.x1 + 4
    }
    const room = c.x1 - x
    const ts: Seg[] = [{ text: telop, c: colText }]
    if (!d) ts[0].c = colSub
    else ts.push({ text: ' ', c: colText }, ...temps(d))
    const full: Seg[] = [{ text: label + ' ', c: colSub }, ...ts]
    for (const cand of [full, ts]) {
      let s = Math.min(WTH_MAX_SCALE, Math.max(div(ch, FONT_H), 1))
      while (s > 1 && segsWidth(font, cand) * s > room) s--
      if (segsWidth(font, cand) * s <= room) {
        segs(x, c.y0 + div(ch - FONT_H * s, 2), s, cand)
        return out
      }
    }
    let rest = room
    if (d) rest -= segsWidth(font, ts.slice(1))
    ts[0] = { ...ts[0], text: todoEllipsis(font, telop, Math.max(rest, 0)) }
    segs(x, c.y0 + div(ch - FONT_H, 2), 1, ts)
    return out
  }
  let head = label + ' ' + calDate(date)
  if (font.textWidth(head) * g.hs > cw) head = label
  text(c.x0, c.y0, g.hs, head, colSub)
  if (g.rows > 0 && d) {
    const y = c.y1 - (g.rows * FONT_H * g.rs + (g.rows - 1))
    for (let k = 0; k < 4; k++) {
      const slot: Rect = { x0: c.x0 + div(k * cw, 4), y0: y, x1: c.x0 + div((k + 1) * cw, 4), y1: c.y1 }
      const past = isToday && now >= date + 6 * (k + 1) * 3600_000
      const p = d.rain?.[k] ?? null
      const v = p === null ? WTH_NULL : `${p}%`
      let lc = colSub
      let vc = rainColor(p)
      if (past) lc = vc = colTodoDone
      let vy = y
      if (g.rows === 2) {
        centered(slot, y, g.rs, WTH_RAIN_SLOTS[k], lc)
        vy += FONT_H * g.rs + 1
      }
      centered(slot, vy, g.rs, v, vc)
    }
  }
  const m = g.mid
  const mh = m.y1 - m.y0
  if (!d) {
    const s = Math.max(Math.min(fitScale(font, WTH_NO_DAY, m.x1 - m.x0, mh), 2), 1)
    centered(m, m.y0 + div(mh - FONT_H * s, 2), s, WTH_NO_DAY, colSub)
    return out
  }
  icon()
  const tp = temps(d)
  const tw = font.textWidth(telop)
  const pw = segsWidth(font, tp)
  const widest = Math.max(tw, pw, 1)
  if (mode === 'tall') {
    let s = Math.max(Math.min(WTH_MAX_SCALE, div(cw, widest)), 1)
    while (s > 1 && 2 * FONT_H * s + 2 > g.textH) s--
    const ih = box.y1 - box.y0
    const empty = isEmpty(box)
    const top = empty ? m.y0 + div(mh - g.textH, 2) : m.y0 + div(mh - (ih + 2 + g.textH), 2)
    const ty = empty ? top + div(g.textH - (2 * FONT_H * s + 2), 2) : top + ih + 2 + div(g.textH - (2 * FONT_H * s + 2), 2)
    centered(c, ty, s, todoEllipsis(font, telop, div(cw, s)), colText)
    const px = c.x0 + div(cw - pw * s, 2)
    segs(Math.max(px, c.x0), ty + FONT_H * s + 2, s, tp)
    return out
  }
  const t = { ...m }
  if (!isEmpty(box)) t.x0 = box.x1 + 4
  const tw2 = t.x1 - t.x0
  const s = Math.max(Math.min(WTH_MAX_SCALE, div(tw2, widest), div(mh - 2, 2 * FONT_H)), 1)
  const ty = m.y0 + div(mh - (2 * FONT_H * s + 2), 2)
  centered(t, ty, s, todoEllipsis(font, telop, div(tw2, s)), colText)
  const px = t.x0 + div(tw2 - pw * s, 2)
  segs(Math.max(px, t.x0), ty + FONT_H * s + 2, s, tp)
  return out
}

const pad2 = (n: number) => String(n).padStart(2, '0')

// wthFooter は weatherwidget.go の wthFooter と同じ。
export function wthFooter(font: BitmapFont, w: WeatherWidget, d: WeatherData, now: number, synced: boolean, maxW: number): { text: string; warn: boolean } {
  if (!synced) return { text: UNSYNCED_TEXT, warn: true }
  const when = (t: number, hourOnly: boolean) => {
    const x = new Date(t)
    let hm = `${pad2(x.getHours())}:${pad2(x.getMinutes())}`
    if (hourOnly && x.getMinutes() === 0) hm = `${x.getHours()}時`
    else if (x.getHours() < 10) hm = hm.slice(1)
    if (dayStart(t) === dayStart(now)) return hm
    return `${x.getMonth() + 1}/${x.getDate()} ${hm}`
  }
  let upd = '更新 なし'
  let warn = true
  if (d.fetched_at) {
    const f = Date.parse(d.fetched_at)
    upd = '更新 ' + when(f, false)
    warn = false
    if (w.staleMs > 0 && now >= f + w.staleMs) {
      upd += ' 古い'
      warn = true
    }
  }
  if (failed(d)) {
    upd += ' 失敗'
    warn = true
  }
  const pub = d.public_time ? when(Date.parse(d.public_time), true) + '発表' : ''
  for (const parts of [[upd, pub, WTH_SOURCE_LONG], [upd, pub, WTH_SOURCE], [upd, WTH_SOURCE], [upd]]) {
    const s = parts.filter((p) => p !== '').join(' ')
    if (font.textWidth(s) <= maxW) return { text: s, warn }
  }
  return { text: todoEllipsis(font, upd, maxW), warn }
}

// ---------- アイコン（weathericon.go の .lha） ----------

export interface Icon {
  w: number
  h: number
  pix: Uint16Array // RGB565
  a: Uint8Array // 不透明度
}

// parseIcon は Brain のアイコンのファイル（LHA1）を読む。形が違えば null。
export function parseIcon(b: Uint8Array): Icon | null {
  if (b.length < 8 || String.fromCharCode(b[0], b[1], b[2], b[3]) !== 'LHA1') return null
  const w = b[4] | (b[5] << 8)
  const h = b[6] | (b[7] << 8)
  if (w < 1 || h < 1 || b.length !== 8 + w * h * 3) return null
  const pix = new Uint16Array(w * h)
  for (let i = 0; i < w * h; i++) pix[i] = b[8 + i * 2] | (b[9 + i * 2] << 8)
  return { w, h, pix, a: b.slice(8 + w * h * 2) }
}

// blend8 は weathericon.go の blend8 と同じ（不透明度 a で s を d の上に重ね、四捨五入）。
export function blend8(s: number, d: number, a: number): number {
  return Math.floor((s * a + d * (255 - a) + 127) / 255)
}
