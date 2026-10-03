// Brain の画面のプレビュー。display.go の drawAll と同じ配置・配色・字形で描く。

import { FONT_H, type BitmapFont } from './font'
import { prettyCombo } from './keys'
import { LAYER_VERB, actionKind, actionTarget, layerTitle, resolveGrid, type LayerKind } from './model'
import type { ActionSpec, Config } from './types'

export type Mode = 'base' | 'latched' | 'temp'
type RGB = [number, number, number]

// display.go の色
const colBG: RGB = [0, 0, 0]
const colCell: RGB = [0x1c, 0x28, 0x38]
const colBorder: RGB = [0x8c, 0xa0, 0xbc]
const colText: RGB = [0xff, 0xff, 0xff]
const colSub: RGB = [0x96, 0xa4, 0xb4]
const colPressed: RGB = [0xff, 0xd0, 0x40]
const colPressedText: RGB = [0, 0, 0]
const colPressedSub: RGB = [0x50, 0x40, 0x00]
const colEmptyBorder: RGB = [0x30, 0x34, 0x3a]
const colLayerCell: RGB = [0x2a, 0x22, 0x3c]
const modeBorder: Record<Mode, RGB> = { base: colBorder, latched: [0x40, 0xc0, 0x70], temp: [0xff, 0x80, 0x20] }
const modeBadge: Record<Mode, RGB> = { base: [0x3a, 0x48, 0x5c], latched: [0x2e, 0x9e, 0x5b], temp: [0xff, 0x80, 0x20] }
const modeBadgeText: Record<Mode, RGB> = { base: colText, latched: colText, temp: [0, 0, 0] }

const cellGap = 4
const textMargin = 8
const maxScale = 6
const subScale = 2
const badgeScale = 2
const badgePad = 5

export interface CellView {
  mapped: boolean
  layer: boolean
  label: string
  sub: string
}

interface Rect {
  x0: number
  y0: number
  x1: number
  y1: number
}

// cellSpan は display.go と同じ。タッチの判定（cellOf）と同じ境界になる。
export function cellSpan(i: number, n: number, size: number): [number, number] {
  return [Math.floor((i * size + n - 1) / n), Math.floor(((i + 1) * size + n - 1) / n)]
}

// cellView は main.go の cellView と同じ。
export function cellView(cfg: Config, a: ActionSpec | null): CellView {
  if (!a) return { mapped: false, layer: false, label: '', sub: '' }
  const k = actionKind(a)
  let label = a.label ?? ''
  if (k !== 'key' && k !== 'none') {
    const t = actionTarget(a)
    const dest = cfg.layers.find((l) => l.name === t)
    const destTitle = dest ? layerTitle(dest) : (t ?? '')
    let sub = LAYER_VERB[k as LayerKind]
    if (label === '') label = destTitle
    else sub += ':' + destTitle
    return { mapped: true, layer: true, label, sub }
  }
  const keys = prettyCombo(a.key ?? '')
  if (label === '' || label === keys) return { mapped: true, layer: false, label: keys, sub: '' }
  return { mapped: true, layer: false, label, sub: keys }
}

class Pixels {
  data: Uint8ClampedArray
  constructor(
    public w: number,
    public h: number,
  ) {
    this.data = new Uint8ClampedArray(w * h * 4)
  }
  fill(r: Rect, c: RGB): void {
    const x0 = Math.max(0, r.x0)
    const y0 = Math.max(0, r.y0)
    const x1 = Math.min(this.w, r.x1)
    const y1 = Math.min(this.h, r.y1)
    for (let y = y0; y < y1; y++) {
      let o = (y * this.w + x0) * 4
      for (let x = x0; x < x1; x++, o += 4) {
        this.data[o] = c[0]
        this.data[o + 1] = c[1]
        this.data[o + 2] = c[2]
        this.data[o + 3] = 255
      }
    }
  }
  frame(r: Rect, t: number, c: RGB): void {
    this.fill({ x0: r.x0, y0: r.y0, x1: r.x1, y1: r.y0 + t }, c)
    this.fill({ x0: r.x0, y0: r.y1 - t, x1: r.x1, y1: r.y1 }, c)
    this.fill({ x0: r.x0, y0: r.y0, x1: r.x0 + t, y1: r.y1 }, c)
    this.fill({ x0: r.x1 - t, y0: r.y0, x1: r.x1, y1: r.y1 }, c)
  }
  text(font: BitmapFont, x: number, y: number, s: string, scale: number, c: RGB, clip: Rect): void {
    for (const ch of s) {
      const g = font.glyph(ch.codePointAt(0)!)
      for (let gy = 0; gy < g.rows.length; gy++) {
        const bits = g.rows[gy]
        if (!bits) continue
        for (let gx = 0; gx < 8; gx++) {
          if (!(bits & (0x80 >> gx))) continue
          const px = { x0: x + gx * scale, y0: y + gy * scale, x1: x + (gx + 1) * scale, y1: y + (gy + 1) * scale }
          this.fill(intersect(px, clip), c)
        }
      }
      x += g.width * scale
    }
  }
}

function intersect(a: Rect, b: Rect): Rect {
  return { x0: Math.max(a.x0, b.x0), y0: Math.max(a.y0, b.y0), x1: Math.min(a.x1, b.x1), y1: Math.min(a.y1, b.y1) }
}

function inset(r: Rect, n: number): Rect {
  return { x0: r.x0 + n, y0: r.y0 + n, x1: r.x1 - n, y1: r.y1 - n }
}

function fitScale(font: BitmapFont, lines: string[], w: number, h: number): number {
  let tw = 0
  for (const s of lines) tw = Math.max(tw, font.textWidth(s))
  const th = lines.length * FONT_H
  let s = maxScale
  while (s > 1 && (tw * s > w || th * s > h)) s--
  return s
}

export interface PreviewParams {
  cfg: Config
  stack: number[] // 下から重ねたレイヤーの番号
  mode: Mode
  pressed?: Set<string> // 押下中として描くセル "列,行"
  w?: number
  h?: number
}

export interface PreviewLayout {
  cols: number
  rows: number
  w: number
  h: number
  rect(col: number, row: number): Rect
}

// renderPreview は画面を RGBA のピクセルにする。
export function renderPreview(font: BitmapFont, p: PreviewParams): { pixels: Uint8ClampedArray; layout: PreviewLayout } {
  const W = p.w ?? 800
  const H = p.h ?? 480
  const g = resolveGrid(p.cfg, p.stack)
  const top = p.cfg.layers[p.stack[p.stack.length - 1]]
  const title = top ? layerTitle(top) : ''
  const px = new Pixels(W, H)
  const rect = (c: number, r: number): Rect => {
    const [x0, x1] = cellSpan(c, g.cols, W)
    const [y0, y1] = cellSpan(r, g.rows, H)
    return { x0, y0, x1, y1 }
  }
  px.fill({ x0: 0, y0: 0, x1: W, y1: H }, colBG)
  for (let r = 0; r < g.rows; r++) {
    for (let c = 0; c < g.cols; c++) {
      const v = cellView(p.cfg, g.cells[r * g.cols + c]?.action ?? null)
      drawCell(font, px, rect(c, r), v, p.mode, p.pressed?.has(`${c},${r}`) ?? false)
    }
  }
  if (title) {
    const bw = font.textWidth(title) * badgeScale + 2 * badgePad
    const bh = FONT_H * badgeScale + 2 * badgePad
    const b = { x0: W - bw, y0: 0, x1: W, y1: bh }
    px.fill(b, modeBadge[p.mode])
    px.text(font, b.x0 + badgePad, b.y0 + badgePad, title, badgeScale, modeBadgeText[p.mode], b)
  }
  return { pixels: px.data, layout: { cols: g.cols, rows: g.rows, w: W, h: H, rect } }
}

function drawCell(font: BitmapFont, px: Pixels, cell: Rect, v: CellView, mode: Mode, pressed: boolean): void {
  px.fill(cell, colBG)
  const box = inset(cell, cellGap)
  if (!v.mapped) {
    px.frame(box, 1, colEmptyBorder)
    return
  }
  let fillC = v.layer ? colLayerCell : colCell
  let textC = colText
  let subC = colSub
  if (pressed) [fillC, textC, subC] = [colPressed, colPressedText, colPressedSub]
  px.fill(box, fillC)
  px.frame(box, 2, modeBorder[mode])
  const inner = inset(box, textMargin)
  const iw = inner.x1 - inner.x0
  const ih = inner.y1 - inner.y0
  const subH = v.sub ? FONT_H * subScale + 4 : 0
  const lines = v.label.split('\n')
  const s = fitScale(font, lines, iw, ih - subH)
  const blockH = lines.length * FONT_H * s
  let y = inner.y0 + Math.trunc((ih - subH - blockH) / 2)
  for (const ln of lines) {
    const x = inner.x0 + Math.trunc((iw - font.textWidth(ln) * s) / 2)
    px.text(font, Math.max(x, inner.x0), y, ln, s, textC, inner)
    y += FONT_H * s
  }
  if (v.sub) {
    const ss = Math.min(fitScale(font, [v.sub], iw, FONT_H * subScale), subScale)
    const x = inner.x0 + Math.trunc((iw - font.textWidth(v.sub) * ss) / 2)
    px.text(font, Math.max(x, inner.x0), inner.y1 - FONT_H * ss, v.sub, ss, subC, inner)
  }
}
