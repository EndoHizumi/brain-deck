// ペンタブレット（pen.go の drawPenTab）と手書きメモ（memo.go の drawMemo）のプレビュー。
// メモの線は Brain のデータなので、プレビューでは空の紙を描く（-render-png で -render-memo を書かないときと同じ）。
import type { BitmapFont } from './font'
import { FONT_H } from './font'
import type { ActionSpec } from './types'

export interface Rect {
  x0: number
  y0: number
  x1: number
  y1: number
}
type RGB = [number, number, number]
interface Px {
  fill(r: Rect, c: RGB): void
  frame(r: Rect, t: number, c: RGB): void
}

export interface PenTabDef {
  map: 'full' | 'aspect' | 'region'
  pressure: boolean
}

export function penTabOf(a: ActionSpec): PenTabDef {
  return { map: a.map === 'aspect' || a.map === 'region' ? a.map : 'full', pressure: a.pressure !== false }
}

const colPenFrame: RGB = [0x5a, 0x6a, 0x80]
const colPenHint: RGB = [0x96, 0xa4, 0xb4]
const colPenOff: RGB = [0xff, 0x80, 0x20]

export function penTabLines(d: PenTabDef, mouseOff: boolean): { lines: string[]; color: RGB } {
  if (mouseOff) return { lines: ['ペンタブレット', 'マウスありの形で使えます', '（usb_mode: mouse）'], color: colPenOff }
  const m = { full: 'PC の画面全体', aspect: 'PC の画面（縦横比を保つ）', region: 'PC の画面の一部' }[d.map]
  return { lines: ['ペンタブレット', m, d.pressure ? '筆圧あり' : '筆圧なし'], color: colPenHint }
}

type Centered = (area: Rect, y: number, s: string, scale: number, c: RGB) => void
type Fit = (lines: string[], w: number, h: number, most: number) => number

export function drawPenTab(px: Px, area: Rect, d: PenTabDef, mouseOff: boolean, fit: Fit, centered: Centered): void {
  px.frame(area, 1, colPenFrame)
  const { lines, color } = penTabLines(d, mouseOff)
  const s = Math.min(fit(lines, area.x1 - area.x0 - 16, Math.trunc((area.y1 - area.y0) / 2), 2), 2)
  let y = area.y0 + Math.trunc((area.y1 - area.y0 - lines.length * FONT_H * s) / 2)
  for (const ln of lines) {
    centered(area, y, ln, s, color)
    y += FONT_H * s
  }
}

// memo.go の値
const MEMO_BAR_H = 44
const MEMO_LABELS = ['細', '中', '太', '消', '戻す', '全消', '＋頁', '新規', '前', '', '次']
const MEMO_NUM = 9
const MEMO_MID = 1
const colMemoPaper: RGB = [0xf4, 0xf1, 0xe6]
const colMemoBar: RGB = [0x24, 0x2c, 0x38]
const colMemoBtn: RGB = [0x34, 0x40, 0x50]
const colMemoOn: RGB = [0x40, 0xa0, 0xe0]
const colMemoEdge: RGB = [0x10, 0x14, 0x18]
const colText: RGB = [0xff, 0xff, 0xff]

function cellSpan(i: number, n: number, size: number): [number, number] {
  return [Math.floor((i * size + n - 1) / n), Math.floor(((i + 1) * size + n - 1) / n)]
}

export function drawMemo(font: BitmapFont, px: Px, area: Rect, fit: Fit, centered: Centered): void {
  const bar: Rect = { x0: area.x0, y0: area.y0, x1: area.x1, y1: area.y0 + Math.min(MEMO_BAR_H, Math.trunc((area.y1 - area.y0) / 3)) }
  const paper: Rect = { x0: area.x0, y0: bar.y1 + 2, x1: area.x1, y1: area.y1 }
  px.fill(bar, colMemoBar)
  const n = MEMO_LABELS.length
  for (let i = 0; i < n; i++) {
    const [x0] = cellSpan(i, n, bar.x1 - bar.x0)
    const [, x1] = cellSpan(i, n, bar.x1 - bar.x0)
    const b: Rect = { x0: bar.x0 + x0 + 2, y0: bar.y0 + 2, x1: bar.x0 + x1 - 2, y1: bar.y1 - 2 }
    let txt = MEMO_LABELS[i]
    let bg = colMemoBtn
    if (i === MEMO_NUM) [txt, bg] = ['-', colMemoBar]
    if (i === MEMO_MID) bg = colMemoOn
    px.fill(b, bg)
    const s = Math.min(fit([txt], b.x1 - b.x0 - 2, b.y1 - b.y0 - 2, 2), 2)
    centered(b, b.y0 + Math.trunc((b.y1 - b.y0 - FONT_H * s) / 2), txt, s, colText)
  }
  void font
  px.fill({ x0: area.x0, y0: bar.y1, x1: area.x1, y1: paper.y0 }, colMemoEdge)
  px.fill(paper, colMemoEdge)
  px.fill(paper, colMemoPaper)
}
