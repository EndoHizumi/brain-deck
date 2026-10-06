// 時計のウィジェットの書式。widget.go の formatClock と同じ結果にする。
// 書式は Go の time.Format の形（"15:04"、"2006/01/02" など）で、{wday} は日本語の曜日 1 文字。

import { DEFAULT_CLOCK_FORMAT, DEFAULT_DATE_FORMAT } from './model'
import type { ActionSpec } from './types'

export const UNSYNCED_TEXT = '時刻未設定'
const WDAY = '{wday}'
const JA_WEEKDAYS = ['日', '月', '火', '水', '木', '金', '土']
const LONG_MONTHS = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December']
const LONG_DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']

// ClockParts は、あるタイムゾーンでの日時の各部分。
export interface ClockParts {
  year: number
  month: number // 1〜12
  day: number
  hour: number
  minute: number
  second: number
  ms: number
  weekday: number // 0 = 日曜
  yday: number // 1 月 1 日が 1
  offsetSec: number // UTC からのずれ
}

const dtfCache = new Map<string, Intl.DateTimeFormat>()

// clockParts は、date を tz（省略すると PC のタイムゾーン）で分解する。
export function clockParts(date: Date, tz?: string): ClockParts {
  let p: Omit<ClockParts, 'weekday' | 'yday' | 'offsetSec'>
  if (!tz) {
    p = { year: date.getFullYear(), month: date.getMonth() + 1, day: date.getDate(), hour: date.getHours(),
      minute: date.getMinutes(), second: date.getSeconds(), ms: date.getMilliseconds() }
  } else {
    let f = dtfCache.get(tz)
    if (!f) {
      f = new Intl.DateTimeFormat('en-US', { timeZone: tz, hourCycle: 'h23', year: 'numeric', month: 'numeric', day: 'numeric',
        hour: 'numeric', minute: 'numeric', second: 'numeric' })
      dtfCache.set(tz, f)
    }
    const v: Record<string, number> = {}
    for (const x of f.formatToParts(date)) if (x.type !== 'literal') v[x.type] = Number(x.value)
    p = { year: v.year, month: v.month, day: v.day, hour: v.hour % 24, minute: v.minute, second: v.second, ms: date.getUTCMilliseconds() }
  }
  const asUTC = Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second, p.ms)
  const weekday = new Date(Date.UTC(p.year, p.month - 1, p.day)).getUTCDay()
  const yday = Math.round((Date.UTC(p.year, p.month - 1, p.day) - Date.UTC(p.year, 0, 1)) / 86400000) + 1
  return { ...p, weekday, yday, offsetSec: Math.round((asUTC - date.getTime()) / 1000) }
}

const pad = (n: number, w = 2, c = '0') => String(n).padStart(w, c)

// goFormat は Go の time.Format（の、よく使う部分）。Go の nextStdChunk と同じ順で書式を読む。
// 時間帯の略称（MST）は、Go と違って "+0900" の形の代わりに "GMT+9" などになることがある。
export function goFormat(t: ClockParts, layout: string): string {
  let out = ''
  let i = 0
  const zone = (sep: boolean, secs: boolean, z: boolean, short = false) => {
    if (z && t.offsetSec === 0) return 'Z'
    const sign = t.offsetSec < 0 ? '-' : '+'
    const a = Math.abs(t.offsetSec)
    let s = sign + pad(Math.floor(a / 3600))
    if (short) return s
    s += (sep ? ':' : '') + pad(Math.floor(a / 60) % 60)
    if (secs) s += (sep ? ':' : '') + pad(a % 60)
    return s
  }
  const hour12 = () => (t.hour % 12 === 0 ? 12 : t.hour % 12)
  const rules: [string, () => string][] = [
    ['January', () => LONG_MONTHS[t.month - 1]],
    ['Jan', () => LONG_MONTHS[t.month - 1].slice(0, 3)],
    ['Monday', () => LONG_DAYS[t.weekday]],
    ['Mon', () => LONG_DAYS[t.weekday].slice(0, 3)],
    ['MST', () => zone(false, false, false)],
    ['01', () => pad(t.month)],
    ['02', () => pad(t.day)],
    ['03', () => pad(hour12())],
    ['04', () => pad(t.minute)],
    ['05', () => pad(t.second)],
    ['06', () => pad(t.year % 100)],
    ['002', () => pad(t.yday, 3)],
    ['15', () => pad(t.hour)],
    ['1', () => String(t.month)],
    ['2006', () => pad(t.year, 4)],
    ['2', () => String(t.day)],
    ['__2', () => pad(t.yday, 3, ' ')],
    ['_2', () => pad(t.day, 2, ' ')],
    ['3', () => String(hour12())],
    ['4', () => String(t.minute)],
    ['5', () => String(t.second)],
    ['PM', () => (t.hour >= 12 ? 'PM' : 'AM')],
    ['pm', () => (t.hour >= 12 ? 'pm' : 'am')],
    ['-07:00:00', () => zone(true, true, false)],
    ['-0700', () => zone(false, false, false)],
    ['-07:00', () => zone(true, false, false)],
    ['-07', () => zone(false, false, false, true)],
    ['Z07:00:00', () => zone(true, true, true)],
    ['Z0700', () => zone(false, false, true)],
    ['Z07:00', () => zone(true, false, true)],
    ['Z07', () => zone(false, false, true, true)],
  ]
  while (i < layout.length) {
    const rest = layout.slice(i)
    // 小数の秒（.000、.999、,000）。数字でない文字が続くときだけ
    const frac = rest.match(/^[.,]([09])\1*/)
    if (frac && !/^\d/.test(rest.slice(frac[0].length))) {
      const n = frac[0].length - 1
      let digits = pad(t.ms, 3).padEnd(9, '0').slice(0, n)
      if (frac[1] === '9') digits = digits.replace(/0+$/, '')
      out += digits ? frac[0][0] + digits : ''
      i += frac[0].length
      continue
    }
    let hit = false
    for (const [tok, f] of rules) {
      if (!rest.startsWith(tok)) continue
      // Go の規則：Jan、Mon などは小文字が続くと別の語、2006 などは数字が続くと別の数
      if ((tok === 'Jan' || tok === 'Mon') && /^[a-z]/.test(rest.slice(tok.length))) continue
      out += f()
      i += tok.length
      hit = true
      break
    }
    if (!hit) {
      const ch = String.fromCodePoint(layout.codePointAt(i)!)
      out += ch
      i += ch.length
    }
  }
  return out
}

// formatClock は Go の書式で書き、{wday} を日本語の曜日にする。
export function formatClock(t: ClockParts, layout: string): string {
  return layout.split(WDAY).map((p) => goFormat(t, p)).join(JA_WEEKDAYS[t.weekday])
}

// ClockDef は、時計のセルの書式を既定値で埋めたもの（widget.go の compileWidget）。
export interface ClockDef {
  format: string
  dateFormat: string // 空なら日付の行を出さない
  tz?: string
}

export function clockDef(a: ActionSpec): ClockDef {
  const dateFormat = a.date_format === 'none' ? '' : a.date_format || DEFAULT_DATE_FORMAT
  return { format: a.format || DEFAULT_CLOCK_FORMAT, dateFormat, tz: a.tz || undefined }
}

// clockLines は、時計に出す 2 行と、時刻を合わせていないかどうか（widget.go の clockLines）。
export function clockLines(d: ClockDef, now: Date, synced: boolean): { time: string; date: string; unsynced: boolean } {
  let p: ClockParts
  try {
    p = clockParts(now, d.tz)
  } catch {
    p = clockParts(now) // 知らないタイムゾーン（Brain の検証で誤りになる）
  }
  const time = formatClock(p, d.format)
  const date = !synced ? UNSYNCED_TEXT : d.dateFormat ? formatClock(p, d.dateFormat) : ''
  return { time, date, unsynced: !synced }
}

// hasSeconds は、書式の結果が 1 分の中で変わるか（秒を出すか）。widget.go の changesWithin と同じ。
export function hasSeconds(layout: string): boolean {
  if (!layout) return false
  const base = Date.UTC(2026, 0, 5, 10, 20, 0)
  const a = formatClock(clockParts(new Date(base), 'UTC'), layout)
  return [1000, 30000, 500].some((d) => formatClock(clockParts(new Date(base + d), 'UTC'), layout) !== a)
}
