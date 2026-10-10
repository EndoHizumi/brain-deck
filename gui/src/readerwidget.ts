// 読み物のアプリ（readerwidget.go）のプレビュー。読み物は Brain のデータなので、プレビューでは一覧（読み物なし）を描く
// （-render-png で -render-read を書かないときと同じ）。
import { FONT_H } from './font'
import type { Rect } from './penwidget'

type RGB = [number, number, number]
interface Px {
  fill(r: Rect, c: RGB): void
}
type Centered = (area: Rect, y: number, s: string, scale: number, c: RGB) => void
type Fit = (lines: string[], w: number, h: number, most: number) => number

const colReadBar: RGB = [0x24, 0x2c, 0x38]
const colReadSub: RGB = [0x96, 0xa4, 0xb4]

export function drawReaderEmpty(px: Px, area: Rect, fit: Fit, centered: Centered): void {
  px.fill(area, colReadBar)
  const lines = ['読み物がありません', 'PC で brain-deck read add <ファイル | URL>']
  const y = area.y0 + Math.trunc((area.y1 - area.y0) / 2) - FONT_H * 2
  lines.forEach((l, i) => {
    const s = Math.min(fit([l], area.x1 - area.x0 - 8, FONT_H * 2, 2), 2)
    centered(area, y + i * FONT_H * 2 + 4 * i, l, s, colReadSub)
  })
}

// drawFilesPlaceholder は、ファイルエクスプローラ（widget: files）のプレビュー。中身は Brain のファイルなので、説明だけ。
export function drawFilesPlaceholder(px: Px, area: Rect, fit: Fit, centered: Centered): void {
  px.fill(area, colReadBar)
  const lines = ['ファイル', 'Brain のファイルを見る（「ファイル」のタブ）']
  const y = area.y0 + Math.trunc((area.y1 - area.y0) / 2) - FONT_H * 2
  lines.forEach((l, i) => {
    const s = Math.min(fit([l], area.x1 - area.x0 - 8, FONT_H * 2, 2), 2)
    centered(area, y + i * FONT_H * 2 + 4 * i, l, s, colReadSub)
  })
}
