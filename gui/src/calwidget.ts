// カレンダーのウィジェット（widget: calendar）。calwidget.go の calRows、calFooter、calLayout と同じ結果にする。
// 予定は Brain の /var/lib/lefthand/calendar.json にあり、PC の brain-deck calendar sync が送る。
// 時刻と日付は、PC のタイムゾーンで決める（Brain と同じタイムゾーンの PC で使う前提。違えば接続したときに知らせている）。

import { FONT_H } from './font'
import { UNSYNCED_TEXT } from './clock'
import { todoGeometry, type Rect, type TodoGeom } from './todowidget'
import type { CalEvent, Calendar, CalendarData } from './types'

// 行の種類（calwidget.go と同じ番号）
export const CAL_PAST = 0
export const CAL_NOW = 1
export const CAL_FUTURE = 2
export const CAL_ALLDAY = 3
export const CAL_NEXT = 4
export const CAL_INFO = 5

export const CAL_NO_DATA = '予定を受け取っていません'
export const CAL_NONE_TODAY = '今日の予定はありません'
const CAL_NO_TITLE = '（名前なし）'
const CAL_FOOTER_BIG = 240
const CAL_SCALE_WIDTH = 88
export const CAL_BAR_W = 3
export const CAL_TIME_COL = '00:00'
const WIDGET_LINE_GAP = 4
export const DEFAULT_CAL_STALE_MS = 3 * 3600_000
export const DEFAULT_PAGE_RESET = '1m'
const JA_WEEKDAYS = ['日', '月', '火', '水', '木', '金', '土']

export interface CalRow {
  label: string
  title: string
  color: string
  state: number
}

export interface CalLay {
  rows: CalRow[] | null
  message?: string
  auto_page: number
  scale: number
  footer: string
  footer_warn: boolean
  footer_scale: number
  geom: TodoGeom
  footerY: number
}

export interface CalWidget {
  rows: number // 0 なら高さで決める
  calendars?: string[]
  staleMs: number // 0 なら古いと出さない
}

interface Ev {
  ev: CalEvent
  cal: Calendar
  order: number
  s: number // ミリ秒
  e: number
  allDay: boolean
}

const pad2 = (n: number) => String(n).padStart(2, '0')
const hm = (t: number) => {
  const d = new Date(t)
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
}
const dayStart = (t: number) => {
  const d = new Date(t)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}
const addDays = (t: number, n: number) => {
  const d = new Date(t)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n).getTime()
}
const parseDay = (s: string): number => {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s)
  return m ? new Date(+m[1], +m[2] - 1, +m[3]).getTime() : NaN
}

// Go の文字列の比べ方（UTF-8 のバイト順 = コードポイント順）
function cmpStr(a: string, b: string): number {
  const x = [...a]
  const y = [...b]
  for (let i = 0; i < Math.min(x.length, y.length); i++) {
    const d = x[i].codePointAt(0)! - y[i].codePointAt(0)!
    if (d) return d
  }
  return x.length - y.length
}

// parseDuration は、Go の time.ParseDuration の、よく使う形（1h30m、90s、2.5h）。読めなければ null。
export function parseDuration(s: string): number | null {
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h)/gy
  const unit: Record<string, number> = { ms: 1, s: 1000, m: 60_000, h: 3_600_000 }
  if (!s) return null
  let ms = 0
  let m: RegExpExecArray | null
  let at = 0
  while ((m = re.exec(s))) {
    ms += Number(m[1]) * unit[m[2]]
    at = re.lastIndex
  }
  return at === s.length ? ms : null
}

export function calShown(w: CalWidget, d: CalendarData | null | undefined): Calendar[] {
  if (!d) return []
  if (!w.calendars?.length) return d.calendars
  return d.calendars.filter((c) => w.calendars!.includes(c.name))
}

function events(cals: Calendar[]): Ev[] {
  const out: Ev[] = []
  cals.forEach((cal, order) => {
    for (const ev of cal.events ?? []) {
      if (ev.start) {
        out.push({ ev, cal, order, s: Date.parse(ev.start), e: Date.parse(ev.end ?? ev.start), allDay: false })
      } else {
        const s = parseDay(ev.day ?? '')
        out.push({ ev, cal, order, s, e: ev.end_day ? parseDay(ev.end_day) : addDays(s, 1), allDay: true })
      }
    }
  })
  return out
}

// calRows は、今日の予定と次の予定の行を作る（calwidget.go の calRows）。
export function calRows(cals: Calendar[], now: number): CalRow[] {
  const t0 = dayStart(now)
  const t1 = addDays(t0, 1)
  const today: Ev[] = []
  const later: Ev[] = []
  for (const it of events(cals)) {
    if (it.s < t1 && (it.e > t0 || (it.e === it.s && it.s >= t0))) today.push(it)
    else if (it.s >= t1) later.push(it)
  }
  const whole = (it: Ev) => it.allDay || (it.s <= t0 && it.e >= t1)
  const cmp = (a: Ev, b: Ev) => {
    const wa = whole(a)
    const wb = whole(b)
    if (wa !== wb) return wa ? -1 : 1
    if (a.s !== b.s) return a.s - b.s
    if (a.e !== b.e) return a.e - b.e
    if (a.order !== b.order) return a.order - b.order
    return cmpStr(a.ev.title, b.ev.title)
  }
  today.sort(cmp)
  const title = (ev: CalEvent) => ev.title || CAL_NO_TITLE
  const rows: CalRow[] = []
  for (const it of today) {
    const r: CalRow = { label: '', title: title(it.ev), color: it.cal.color, state: 0 }
    if (whole(it)) [r.label, r.state] = ['終日', CAL_ALLDAY]
    else if (now >= it.s && now < it.e) {
      r.state = CAL_NOW
      r.label = it.e < t1 ? '〜' + hm(it.e) : '今'
    } else if (it.s < t0) [r.label, r.state] = ['〜' + hm(it.e), CAL_PAST]
    else if (it.e <= now && (it.e > it.s || it.s < now)) [r.label, r.state] = [hm(it.s), CAL_PAST]
    else [r.label, r.state] = [hm(it.s), CAL_FUTURE]
    rows.push(r)
  }
  if (!today.length) rows.push({ label: '', title: CAL_NONE_TODAY, color: '', state: CAL_INFO })
  if (later.length) {
    later.sort((a, b) => (a.s !== b.s ? a.s - b.s : cmp(a, b)))
    const it = later[0]
    const d = new Date(it.s)
    let label = `${d.getMonth() + 1}/${d.getDate()}(${JA_WEEKDAYS[d.getDay()]})`
    if (dayStart(it.s) === t1) label = '明日'
    if (!it.allDay) label += ' ' + hm(it.s)
    rows.push({ label, title: title(it.ev), color: it.cal.color, state: CAL_NEXT })
  }
  return rows
}

// calFooter は、最終更新の行の文と、橙色で出すかどうか（calwidget.go の calFooter）。
export function calFooter(w: CalWidget, cals: Calendar[], now: number, synced: boolean): [string, boolean] {
  if (!synced) return [UNSYNCED_TEXT, true]
  let oldest: number | null = null
  let failed = 0
  for (const c of cals) {
    if (c.error) failed++
    if (c.fetched_at) {
      const t = Date.parse(c.fetched_at)
      if (oldest === null || t < oldest) oldest = t
    }
  }
  let s = '更新 なし'
  let warn = true
  if (oldest !== null) {
    const d = new Date(oldest)
    s = dayStart(oldest) === dayStart(now) ? '更新 ' + hm(oldest) : `更新 ${d.getMonth() + 1}/${d.getDate()} ${hm(oldest)}`
    warn = false
    if (w.staleMs > 0 && now >= oldest + w.staleMs) {
      s += ' 古い'
      warn = true
    }
  }
  if (failed > 0) {
    s += ' 失敗 ' + failed
    warn = true
  }
  return [s, warn]
}

// calLayout は、見出しの下の範囲 area の中の配置を決める（calwidget.go の calLayout）。
export function calLayout(area: Rect, w: CalWidget, d: CalendarData | null | undefined, now: number, synced: boolean): CalLay {
  const cals = calShown(w, d)
  if (!cals.length) {
    return { rows: null, message: CAL_NO_DATA, auto_page: 0, scale: 1, footer: '', footer_warn: false, footer_scale: 0,
      geom: todoGeometry(area, 0, w.rows), footerY: 0 }
  }
  const [footer, warn] = calFooter(w, cals, now, synced)
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  const fs = aw >= CAL_FOOTER_BIG && ah >= CAL_FOOTER_BIG ? 2 : 1
  const body = { ...area, y1: area.y1 - (FONT_H * fs + WIDGET_LINE_GAP) }
  const rows = calRows(cals, now)
  const geom = todoGeometry(body, rows.length, w.rows)
  let scale = geom.scale
  while (scale > 1 && CAL_SCALE_WIDTH * scale > aw) scale--
  let first = 0
  for (let i = 0; i < rows.length; i++) {
    if (rows[i].state !== CAL_PAST && rows[i].state !== CAL_ALLDAY) {
      first = i
      break
    }
  }
  return { rows, auto_page: Math.trunc(first / geom.per), scale, footer, footer_warn: warn, footer_scale: fs, geom,
    footerY: body.y1 + WIDGET_LINE_GAP }
}

// calWidget は、設定のセルから、描くのに要る項目を取り出す。
export function calWidgetOf(a: { rows?: number; calendars?: string[]; stale?: string }): CalWidget {
  const st = a.stale ? parseDuration(a.stale) : null
  return { rows: typeof a.rows === 'number' ? a.rows : 0, calendars: a.calendars, staleMs: st ?? DEFAULT_CAL_STALE_MS }
}
