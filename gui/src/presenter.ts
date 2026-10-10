// プレゼン用タイマー（widget: presenter）。presenter.go と同じ計算にする（プレビューを -render-png と画素単位で比べる）。
// 状態は Brain の widgets.json の presenter（ストップウォッチと同じ形）。持ち時間と色はセルの設定。

import { colInkError, parseColor, parseDurationMs, stopwatchElapsed, stopwatchText, type RGB } from './gadgetwidget'
import type { ActionSpec, PresenterColorSpec, StopwatchState } from './types'

export const DEFAULT_PRESENTER_MS = 10 * 60_000
export const DEFAULT_PRESENTER_COLORS: PresenterColorSpec[] = [{ remaining: '5m', color: '#ffc030' }, { remaining: '1m', color: '#ff5858' }]
export const PRESENTER_IDLE = 'タップで開始'
const BLINK_MS = 500

export interface PresenterDef {
  duration: number // ミリ秒
  colors: { remaining: number; color: RGB }[] // 残りの長い順
}

// presenterDef は、セルの duration と colors を読む。読めない値は既定にする（検証は Brain が行う）。
export function presenterDef(a: ActionSpec): PresenterDef {
  const d = a.duration ? parseDurationMs(a.duration) : null
  const colors = (a.colors ?? DEFAULT_PRESENTER_COLORS)
    .map((c) => ({ remaining: parseDurationMs(c.remaining) ?? 0, color: parseColor(c.color) }))
    .filter((c): c is { remaining: number; color: RGB } => c.remaining > 0 && c.color !== null)
    .sort((x, y) => y.remaining - x.remaining)
  return { duration: d && d >= 10_000 ? d : DEFAULT_PRESENTER_MS, colors }
}

export interface PresenterView {
  main: string
  sub: string
  color: RGB | null
  over: boolean
  running: boolean
  blinkOn: boolean
  bar: number // 千分率
  next: number // 次に変わる時刻（動いていなければ 0）
}

// presenterView は presenter.go の presenterView と同じ。
export function presenterView(p: PresenterDef, st: StopwatchState | undefined, now: number): PresenterView {
  const el = stopwatchElapsed(st, now)
  const rem = p.duration - el
  const v: PresenterView = { main: '', sub: '', color: null, over: false, running: !!st?.started_at, blinkOn: false,
    bar: Math.min(Math.trunc((Math.floor(el / 1000) * 1000 * 1000) / p.duration), 1000), next: 0 }
  const elapsed = '経過 ' + stopwatchText(el)
  if (rem > 0) {
    v.main = stopwatchText(Math.ceil(rem / 1000) * 1000)
    for (const c of p.colors) if (rem <= c.remaining) v.color = c.color
  } else {
    v.over = true
    v.main = '+' + stopwatchText(-rem)
    v.color = colInkError
  }
  if (!v.running && el === 0) v.sub = PRESENTER_IDLE
  else if (!v.running) v.sub = '一時停止 ' + elapsed
  else if (v.over) v.sub = '時間切れ ' + elapsed
  else v.sub = elapsed
  if (v.running) {
    v.next = now + 1000 - (el % 1000)
    if (v.over) {
      const over = -rem
      v.blinkOn = Math.floor(over / BLINK_MS) % 2 === 0
      v.next = now + BLINK_MS - (over % BLINK_MS)
    }
  }
  return v
}
