// 月の表示のウィジェット（widget: month）。monthwidget.go の monthLayout と同じ結果にする。
// 日付は PC のタイムゾーンで決める（Brain と同じタイムゾーンの PC で使う前提）。

import { FONT_H, type BitmapFont } from './font'
import { UNSYNCED_TEXT } from './clock'
import { DEFAULT_PAGE_RESET, calDate, calEvents, calShown, dayKey, parseDay, type CalEv } from './calwidget'
import type { Rect } from './todowidget'
import type { Calendar, CalendarData } from './types'

export const DEFAULT_MONTH_ID = 'month'
export const MONTH_MAX_OFFSET = 24
const MONTH_MAX_DOTS = 3
const MONTH_NUM_TEXT = '30'
const MONTH_NO_DATA = '予定未受信'
const MONTH_OUT_RANGE = '範囲外'
const MONTH_PART_RANGE = '一部範囲外'
const MONTH_NO_EVENTS = '予定なし'
const TODO_NAV_H = 44
const WIDGET_LINE_GAP = 4
export const MONTH_MIN_SPAN = 2

export interface MonthWidget {
  id: string
  weekStart: number // 0 が日曜、1 が月曜
  calendars?: string[]
  pageReset: string
}

// 出している月と、選んだ日（-render-month-offset、-render-month-select と同じ）
export interface MonthState {
  offset: number
  sel: string // YYYY-MM-DD。空なら今日
}

export interface MonthDay {
  day: string
  in: boolean
  today?: boolean
  sel?: boolean
  red?: boolean
  sat?: boolean
  holiday?: string
  dots?: string[]
  rect: [number, number, number, number]
}

export interface MonthLay {
  title: string
  title_scale: number
  title_warn: boolean
  status?: string
  status_warn?: boolean
  wday_y: number
  wdays: number[]
  weeks: number
  num_scale: number
  dot: number
  days: MonthDay[]
  info: string
  offset: number
  nav: [number, number, number, number]
  up: [number, number, number, number]
  mid: [number, number, number, number]
  down: [number, number, number, number]
}

const addDays = (t: number, n: number) => {
  const d = new Date(t)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n).getTime()
}
const dayStart = (t: number) => {
  const d = new Date(t)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

export function monthWidgetOf(a: { id?: string; week_start?: string; calendars?: string[]; page_reset?: string }): MonthWidget {
  return { id: a.id || DEFAULT_MONTH_ID, weekStart: a.week_start === 'monday' ? 1 : 0, calendars: a.calendars, pageReset: a.page_reset ?? DEFAULT_PAGE_RESET }
}

// covers は、送られてきた範囲 [from, from+days] に day が入るか（calendar.go の covers）。
export function covers(d: CalendarData | null | undefined, day: number): boolean {
  if (!d?.from) return true
  const a = parseDay(d.from)
  if (Number.isNaN(a)) return true
  return day >= a && day < addDays(a, (d.days ?? 0) + 1)
}

// eachDay は、予定が重なる日（0 時）のうち [from, to) に入るものについて f を呼ぶ（monthwidget.go の eachDay）。
export function eachDay(ev: { s: number; e: number }, from: number, to: number, f: (day: number) => void): void {
  const s0 = dayStart(ev.s)
  for (let d = Math.max(s0, from); d < to; d = addDays(d, 1)) {
    if (!(ev.e > d || (ev.e === ev.s && d === s0))) break
    f(d)
  }
}

function monthCals(w: MonthWidget, d: CalendarData | null | undefined): { marks: Calendar[]; hols: Calendar[] } {
  const marks = calShown({ rows: 0, staleMs: 0, calendars: w.calendars }, d).filter((c) => !c.holiday)
  const hols = d ? d.calendars.filter((c) => c.holiday) : []
  return { marks, hols }
}

// monthLayout は、見出しの下の範囲 area の中の配置と、日付ごとの色と印を決める（monthwidget.go の monthLayout）。
export function monthLayout(font: BitmapFont, area: Rect, w: MonthWidget, d: CalendarData | null | undefined, now: number, synced: boolean,
  st: MonthState = { offset: 0, sel: '' }): MonthLay {
  const today = dayStart(now)
  const td = new Date(today)
  const first = new Date(td.getFullYear(), td.getMonth() + st.offset, 1).getTime()
  const fd = new Date(first)
  const next = new Date(fd.getFullYear(), fd.getMonth() + 1, 1).getTime()
  const lead = (fd.getDay() - w.weekStart + 7) % 7
  const start = addDays(first, -lead)
  const dim = new Date(addDays(next, -1)).getDate()
  const weeks = Math.trunc((lead + dim + 6) / 7)
  const end = addDays(start, weeks * 7)

  const title = `${fd.getFullYear()}年${fd.getMonth() + 1}月`
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  const titleS = aw >= 300 && ah >= 220 && font.textWidth(title) * 2 <= Math.trunc(aw / 2) ? 2 : 1
  const navH = Math.min(Math.max(Math.trunc(ah / 7), FONT_H + 8), TODO_NAV_H)
  const wdayY = area.y0 + FONT_H * titleS + WIDGET_LINE_GAP
  const gridTop = wdayY + FONT_H + 2
  const gridBot = area.y1 - navH
  const cellH = Math.max(Math.trunc((gridBot - gridTop) / weeks), 1)
  const wdays = [0, 1, 2, 3, 4, 5, 6].map((i) => (w.weekStart + i) % 7)
  const cellW = Math.trunc(aw / 7)
  let numS = 1
  for (let s = 3; s > 1; s--) {
    if (FONT_H * s + 1 + 2 * s + 2 <= cellH && font.textWidth(MONTH_NUM_TEXT) * s + 4 <= cellW) {
      numS = s
      break
    }
  }
  const l: MonthLay = { title, title_scale: titleS, title_warn: !synced, wday_y: wdayY, wdays, weeks, num_scale: numS, dot: 2 * numS,
    days: [], info: '', offset: st.offset, nav: [0, 0, 0, 0], up: [0, 0, 0, 0], mid: [0, 0, 0, 0], down: [0, 0, 0, 0] }

  const hasData = !!d && d.calendars.length > 0
  if (!synced) {
    l.status = UNSYNCED_TEXT
    l.status_warn = true
  } else if (hasData) {
    let n = 0
    for (let t = first; t < next; t = addDays(t, 1)) if (covers(d, t)) n++
    if (n === 0) l.status = MONTH_OUT_RANGE
    else if (n < dim) l.status = MONTH_PART_RANGE
  }

  const { marks, hols } = monthCals(w, d)
  const holEvs = calEvents(hols)
  const markEvs = calEvents(marks)
  const holiday = new Map<number, string>()
  for (const it of holEvs) {
    eachDay(it, start, end, (day) => {
      if (!holiday.has(day)) holiday.set(day, it.ev.title || '祝日')
    })
  }
  const marked = new Map<number, number[]>()
  for (const it of markEvs) {
    eachDay(it, start, end, (day) => {
      const m = marked.get(day) ?? []
      if (!m.length || m[m.length - 1] !== it.order) m.push(it.order)
      marked.set(day, m)
    })
  }

  let selDay = today
  if (st.sel) {
    const t = parseDay(st.sel)
    if (!Number.isNaN(t)) selDay = t
  }
  for (let i = 0; i < weeks * 7; i++) {
    const day = addDays(start, i)
    const dd = new Date(day)
    const col = i % 7
    const row = Math.trunc(i / 7)
    const x0 = area.x0 + Math.trunc((col * aw) / 7)
    const x1 = area.x0 + Math.trunc(((col + 1) * aw) / 7)
    const y0 = gridTop + row * cellH
    const md: MonthDay = { day: dayKey(day), in: day >= first && day < next, rect: [x0, y0, x1, y0 + cellH] }
    if (day === today) md.today = true
    if (day === selDay && day !== today) md.sel = true
    if (dd.getDay() === 0) md.red = true
    if (dd.getDay() === 6) md.sat = true
    const h = holiday.get(day)
    if (h !== undefined) {
      md.holiday = h
      md.red = true
    }
    const m = marked.get(day)
    if (m) {
      md.dots = []
      for (let k = 0; k < m.length; k++) {
        if (m.length > MONTH_MAX_DOTS && k === MONTH_MAX_DOTS - 1) {
          md.dots.push('more')
          break
        }
        md.dots.push(marks[m[k]].color)
      }
    }
    l.days.push(md)
  }

  l.info = calDate(selDay)
  if (!hasData) l.info += ' ' + MONTH_NO_DATA
  else if (!covers(d, selDay)) l.info += ' ' + MONTH_OUT_RANGE
  else {
    let n = 0
    let h = ''
    const to = addDays(selDay, 1)
    for (const it of markEvs) eachDay(it, selDay, to, () => n++)
    for (const it of holEvs) eachDay(it, selDay, to, () => {
      if (!h) h = it.ev.title
    })
    if (h) l.info += ' ' + h
    if (n > 0) l.info += ' 予定' + n
    else if (!h) l.info += ' ' + MONTH_NO_EVENTS
  }

  const a = area.x0 + Math.trunc(aw / 5)
  const b = area.x1 - Math.trunc(aw / 5)
  const ny = area.y1 - navH
  l.nav = [area.x0, ny, area.x1, area.y1]
  l.up = [area.x0, ny, a, area.y1]
  l.mid = [a, ny, b, area.y1]
  l.down = [b, ny, area.x1, area.y1]
  return l
}

// monthFollowDay は、follow のカレンダーに出す日（選んだ日が今日でなければ、その日の 0 時）。
export function monthFollowDay(st: MonthState | undefined, now: number): number | null {
  if (!st?.sel) return null
  const t = parseDay(st.sel)
  if (Number.isNaN(t) || t === dayStart(now)) return null
  return t
}

export type { CalEv }
