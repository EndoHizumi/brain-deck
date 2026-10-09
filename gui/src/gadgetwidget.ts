// 電池、ストップウォッチ、ポモドーロ、カウンター、QR コード、在席ランプのウィジェットの中身。
// gadgetwidget.go と gadgets.go の計算と同じ結果にする（プレビューを -render-png と画素単位で比べる）。

import type { ActionSpec, BatteryInfo, PomodoroState, PresenceSpec, StopwatchState, WidgetsState } from './types'

export type RGB = [number, number, number]

export const colInkOK: RGB = [0x50, 0xd8, 0x80]
export const colInkWarn: RGB = [0xff, 0xc0, 0x30]
export const colInkError: RGB = [0xff, 0x58, 0x58]
export const colPomoWork: RGB = [0xff, 0x70, 0x60]
export const colPomoBreak: RGB = colInkOK
export const colUnknown: RGB = [0x40, 0x48, 0x54]
export const colQRDark: RGB = [0, 0, 0]
export const colQRLight: RGB = [0xff, 0xff, 0xff]
export const colQRDarkOld: RGB = [0x30, 0x36, 0x3c]
export const colQRLitOld: RGB = [0x8a, 0x92, 0x9c]

export const POMO_ALERT_MS = 15000
export const POMO_BLINK_MS = 500
export const DEFAULT_WORK_MS = 25 * 60000
export const DEFAULT_BREAK_MS = 5 * 60000
export const GAUGE_H = 8
export const QR_NONE = '未設定'
export const QR_TOO_BIG = 'QR が入りません'
export const STOPWATCH_IDLE = 'タップで開始'
export const QR_MAX_BYTES = 400

export const DEFAULT_PRESENCE: PresenceSpec[] = [
  { name: '在席', color: '#40c070' },
  { name: '集中', color: '#ff5858' },
  { name: '会議中', color: '#ff8020' },
  { name: '休憩', color: '#4f9dff' },
]
export const PRESENCE_PALETTE = ['#40c070', '#ff5858', '#ff8020', '#4f9dff', '#c070ff', '#ffd040', '#20c0c0', '#a0a8b0']

export interface PresenceDef {
  name: string
  color: RGB
}

export function parseColor(s: string): RGB | null {
  const m = /^#([0-9a-fA-F]{6})$/.exec(s)
  if (!m) return null
  const v = parseInt(m[1], 16)
  return [(v >> 16) & 255, (v >> 8) & 255, v & 255]
}

// presenceStates は、セルの states（なければ既定の 4 つ）を、色を決めた形にする。色の誤りは灰色にする（検証は Brain が行う）。
export function presenceStates(specs: PresenceSpec[] | undefined): PresenceDef[] {
  const l = specs && specs.length ? specs : DEFAULT_PRESENCE
  return l.map((s, i) => ({ name: s.name, color: parseColor(s.color || PRESENCE_PALETTE[i % PRESENCE_PALETTE.length]) ?? colUnknown }))
}

// parseDurationMs は Go の time.ParseDuration の一部（h、m、s の組み合わせ）。読めなければ null。
export function parseDurationMs(s: string): number | null {
  const re = /(\d+(?:\.\d+)?)(h|m|s|ms)/y
  let ms = 0
  let i = 0
  if (!s) return null
  while (i < s.length) {
    re.lastIndex = i
    const m = re.exec(s)
    if (!m) return null
    ms += parseFloat(m[1]) * { h: 3600000, m: 60000, s: 1000, ms: 1 }[m[2] as 'h' | 'm' | 's' | 'ms']
    i = re.lastIndex
  }
  return ms
}

// pomoDurations は、ポモドーロのセルの作業と休憩の時間（ミリ秒）。読めなければ既定値。
export function pomoDurations(a: ActionSpec): { work: number; brk: number } {
  return { work: (a.work && parseDurationMs(a.work)) || DEFAULT_WORK_MS, brk: (a.break && parseDurationMs(a.break)) || DEFAULT_BREAK_MS }
}

const ms = (t: string | undefined) => (t ? Date.parse(t) : NaN)

// stopwatchElapsed は、now（ミリ秒）の時点の経過時間（gadgets.go の StopwatchState.Elapsed）。
export function stopwatchElapsed(s: StopwatchState | undefined, now: number): number {
  if (!s) return 0
  let d = s.elapsed_ms ?? 0
  const st = ms(s.started_at)
  if (!isNaN(st) && now > st) d += now - st
  return d
}

export function stopwatchText(d: number): string {
  const s = Math.floor(d / 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  if (s >= 3600) return `${Math.floor(s / 3600)}:${p(Math.floor(s / 60) % 60)}:${p(s % 60)}`
  return `${Math.floor(s / 60)}:${p(s % 60)}`
}

export function pomoText(d: number): string {
  const s = Math.ceil(d / 1000)
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

export interface PomoView {
  started: boolean
  running: boolean
  phase: 'work' | 'break'
  remaining: number
  count: number
  alert: boolean
  blinkOn: boolean
  next: number // 次に表示が変わる時刻（動いていなければ 0）
}

// pomodoroView は gadgets.go の PomodoroState.view と同じ。work は、まだ始めていないときに出す作業の時間（セルの設定）。
export function pomodoroView(p: PomodoroState | undefined, now: number, work: number): PomoView {
  const anchor = ms(p?.anchor)
  const w = p?.work_ms ?? 0
  const b = p?.break_ms ?? 0
  if (!p || !p.running || isNaN(anchor) || w <= 0 || b <= 0) {
    const started = !!p?.phase
    return { started, running: false, phase: started ? p!.phase! : 'work', remaining: started ? (p!.remaining_ms ?? 0) : work,
      count: p?.done ?? 0, alert: false, blinkOn: false, next: 0 }
  }
  const el = Math.max(now - anchor, 0)
  const k = Math.floor(el / (w + b))
  const inC = el % (w + b)
  const start = anchor + k * (w + b)
  const v: PomoView = { started: true, running: true, phase: 'work', remaining: 0, count: (p.done ?? 0) + k, alert: false, blinkOn: false, next: 0 }
  let switched = 0
  if (inC < w) {
    v.remaining = w - inC
    if (k > 0) switched = start
  } else {
    v.phase = 'break'
    v.remaining = w + b - inC
    v.count++
    switched = start + w
  }
  let frac = v.remaining % 1000
  if (frac === 0) frac = 1000
  v.next = now + frac
  const ack = ms(p.ack_at)
  if (switched && now - switched < POMO_ALERT_MS && (isNaN(ack) || ack < switched)) {
    v.alert = true
    const since = now - switched
    v.blinkOn = Math.floor(since / POMO_BLINK_MS) % 2 === 0
    const t = switched + (Math.floor(since / POMO_BLINK_MS) + 1) * POMO_BLINK_MS
    if (t < v.next) v.next = t
  }
  return v
}

// presenceView は、在席ランプに出す名前と色（gadgetwidget.go の presenceView）。
export function presenceView(states: PresenceDef[], w: WidgetsState, id: string): { name: string; color: RGB } {
  const st = w.presence?.[id]
  if (!st || !st.state) return { name: states[0].name, color: states[0].color }
  const i = states.findIndex((s) => s.name === st.state)
  return { name: st.state, color: i >= 0 ? states[i].color : colUnknown }
}

export function contrastInk(c: RGB): RGB {
  return 299 * c[0] + 587 * c[1] + 114 * c[2] >= 140 * 1000 ? [0, 0, 0] : [0xff, 0xff, 0xff]
}

export function pomoInk(phase: string): RGB {
  return phase === 'break' ? colPomoBreak : colPomoWork
}

// batteryLines は gadgetwidget.go の batteryLines と同じ。pct が -1 なら帯を出さない。
export function batteryLines(b: BatteryInfo | null | undefined, ink: RGB, colSub: RGB): { main: string; sub: string; c: RGB; pct: number } {
  if (!b || b.percent < 0) return { main: '--%', sub: '電池を読めません', c: colSub, pct: -1 }
  let c = ink
  let sub = '電池'
  if (b.charging) [sub, c] = ['充電中', colInkOK]
  else if (b.plugged) sub = 'USB 給電'
  if (!b.charging) {
    if (b.percent <= 15) c = colInkError
    else if (b.percent <= 30) c = colInkWarn
  }
  return { main: `${b.percent}%`, sub, c, pct: b.percent }
}

// qrFit は gadgetwidget.go の qrFit と同じ。入らなければ px が 0。
export function qrFit(size: number, aw: number, ah: number): { px: number; quiet: number } {
  for (const q of [4, 2]) {
    const p = Math.floor(Math.min(aw, ah) / (size + 2 * q))
    if (p >= 2) return { px: p, quiet: q }
  }
  return { px: 0, quiet: 0 }
}

// stateId は、ウィジェットの状態の id（gadgets.go の widgetStateID）。
export function stateId(a: ActionSpec): string {
  return a.id || a.widget || ''
}
