// 状態の帯（statusbar.go と同じ項目、文、色）。描くのは preview.ts の drawBar。

import type { BarItem, BatteryInfo } from './types'

export const BAR_H = 28 // 帯の高さ
export const BAR_SCALE = 2
export const BAR_PAD = 6
export const BAR_GAP = 12
export const BAR_TEXT_Y = 2
export const BAR_BATT_W = 22 // 電池の絵の幅（端子を除く）
export const BAR_BATT_H = 14
export const BAR_BATT_TO = 3 // 電池の絵の右の端子の幅
export const BAR_ITEMS_DEFAULT: BarItem[] = ['sync', 'mouse', 'pc', 'battery', 'time']
export const BAR_ITEM_LABELS: Record<BarItem, string> = { time: '時刻', battery: '電池', pc: 'PC とのつながり', mouse: 'マウスの形', sync: '最後に同期した時刻' }

type RGB = [number, number, number]
export const colBar: RGB = [0x14, 0x1c, 0x28]
export const colBarRule: RGB = [0x30, 0x3c, 0x4c]
const colBarDim: RGB = [0x6a, 0x74, 0x80]
const colText: RGB = [0xff, 0xff, 0xff]
const colUnsynced: RGB = [0xff, 0x80, 0x20]
const colOK: RGB = [0x50, 0xd8, 0x80]
const colWarn: RGB = [0xff, 0xc0, 0x30]
const colError: RGB = [0xff, 0x58, 0x58]

export interface BarEnv {
  now: Date
  synced: boolean
  battery: BatteryInfo | null
  link: string // UDC の state（configured、suspended など）か power（電源だけ。link.go）。空なら出さない
  mouseOff: boolean
  lastSync: Date | null
}

export interface BarItemView {
  kind: BarItem
  text: string
  ink: RGB
  batt?: BatteryInfo
}

const hhmm = (d: Date) => `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`

// linkText は statusbar.go の linkText と同じ。
export function linkText(state: string): [string, RGB] {
  if (state === '') return ['', colText]
  if (state === 'configured') return ['PC', colOK]
  if (state === 'suspended') return ['PC休止', colBarDim]
  if (state === 'power') return ['給電のみ', colUnsynced]
  return ['PCなし', colUnsynced]
}

// barItems は statusbar.go の barItems と同じ。
export function barItems(items: BarItem[], env: BarEnv): BarItemView[] {
  const out: BarItemView[] = []
  for (const k of items) {
    if (k === 'time') out.push({ kind: k, text: hhmm(env.now), ink: env.synced ? colText : colUnsynced })
    else if (k === 'battery') {
      const b = env.battery
      if (!b || b.percent < 0) {
        out.push({ kind: k, text: '電池?', ink: colBarDim })
        continue
      }
      const ink = b.plugged ? colOK : b.percent <= 10 ? colError : b.percent <= 25 ? colWarn : colText
      out.push({ kind: k, text: `${b.percent}%`, ink, batt: b })
    } else if (k === 'pc') {
      const [t, c] = linkText(env.link)
      if (t) out.push({ kind: k, text: t, ink: c })
    } else if (k === 'mouse') {
      if (!env.mouseOff) out.push({ kind: k, text: 'マウス', ink: colText })
    } else if (k === 'sync') {
      const t = env.lastSync
      if (!t) {
        out.push({ kind: k, text: '未同期', ink: colUnsynced })
        continue
      }
      const n = env.now
      const same = t.getFullYear() === n.getFullYear() && t.getMonth() === n.getMonth() && t.getDate() === n.getDate()
      out.push({ kind: k, text: '同期' + (same ? hhmm(t) : `${t.getMonth() + 1}/${t.getDate()}`), ink: colBarDim })
    }
  }
  return out
}
