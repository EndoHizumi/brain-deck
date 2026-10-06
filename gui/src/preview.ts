// Brain の画面のプレビュー。display.go の drawAll と同じ配置・配色・字形で描く。

import { FONT_H, type BitmapFont } from './font'
import { prettyCombo } from './keys'
import { clockDef, clockLines, type ClockDef } from './clock'
import { LAYER_VERB, actionKind, actionTarget, isLayerAction, layerTitle, resolveGrid, spanOf, type LayerKind } from './model'
import type { ActionSpec, Config, PressStyle } from './types'

export type Mode = 'base' | 'latched' | 'temp'
type RGB = [number, number, number]

// display.go の色
const colBG: RGB = [0, 0, 0]
const colCell: RGB = [0x1c, 0x28, 0x38]
const colBorder: RGB = [0x8c, 0xa0, 0xbc]
const colText: RGB = [0xff, 0xff, 0xff]
const colSub: RGB = [0x96, 0xa4, 0xb4]
const colPressed: RGB = [0xff, 0xd0, 0x40] // fill の塗り、border の明るい線
const colPressedText: RGB = [0, 0, 0]
const colPressedSub: RGB = [0x50, 0x40, 0x00]
const colPressEdge: RGB = [0, 0, 0] // border の外側の暗い線
const colEmptyBorder: RGB = [0x30, 0x34, 0x3a]
const colLayerCell: RGB = [0x2a, 0x22, 0x3c]
const colUnsynced: RGB = [0xff, 0x80, 0x20] // 時刻を合わせていない時計
const modeBorder: Record<Mode, RGB> = { base: colBorder, latched: [0x40, 0xc0, 0x70], temp: [0xff, 0x80, 0x20] }
const modeBadge: Record<Mode, RGB> = { base: [0x3a, 0x48, 0x5c], latched: [0x2e, 0x9e, 0x5b], temp: [0xff, 0x80, 0x20] }
const modeBadgeText: Record<Mode, RGB> = { base: colText, latched: colText, temp: [0, 0, 0] }

const cellGap = 4
const textMargin = 8
const maxScale = 6
const subScale = 2
const badgeScale = 2
const badgePad = 5
const borderW = 2
// press_style: border の二重の枠（外側が暗い線、内側が明るい線）
const pressEdgeW = 2
const pressGlowW = 5
// widget.go
const clockMaxScale = 10
const dateMaxScale = 3
const captionScale = 2
const widgetLineGap = 4

export interface CellView {
  mapped: boolean
  layer: boolean
  label: string // ウィジェットでは上に小さく出す見出し
  sub: string
  clock?: ClockDef // 時計のウィジェット
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
  if (a.widget) return { mapped: true, layer: isLayerAction(a), label, sub: '', clock: clockDef(a) }
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

function fitScale(font: BitmapFont, lines: string[], w: number, h: number, most = maxScale): number {
  let tw = 0
  for (const s of lines) tw = Math.max(tw, font.textWidth(s))
  const th = lines.length * FONT_H
  let s = most
  while (s > 1 && (tw * s > w || th * s > h)) s--
  return s
}

export interface PreviewParams {
  cfg: Config
  stack: number[] // 下から重ねたレイヤーの番号
  mode: Mode
  pressed?: Set<string> // 押下中として描くセル "列,行"
  pressStyle?: PressStyle // 省略すると cfg.display.press_style、それもなければ border
  w?: number
  h?: number
  now?: Date // 時計に出す時刻。省略すると今
  synced?: boolean // false なら、時刻を合わせていないときの時計を描く
}

export interface WidgetEnv {
  now: Date
  synced: boolean
}

export interface PreviewLayout {
  cols: number
  rows: number
  w: number
  h: number
  rect(col: number, row: number): Rect // span のセルは覆う範囲全体
  anchor: number[] // resolveGrid の anchor
}

// renderPreview は画面を RGBA のピクセルにする。
export function renderPreview(font: BitmapFont, p: PreviewParams): { pixels: Uint8ClampedArray; layout: PreviewLayout } {
  const W = p.w ?? 800
  const H = p.h ?? 480
  const g = resolveGrid(p.cfg, p.stack)
  const top = p.cfg.layers[p.stack[p.stack.length - 1]]
  const title = top ? layerTitle(top) : ''
  const fill = (p.pressStyle ?? p.cfg.display?.press_style) === 'fill'
  const px = new Pixels(W, H)
  const env: WidgetEnv = { now: p.now ?? new Date(), synced: p.synced ?? true }
  // display.go の Layout.rect と同じ。span のセルは覆う範囲全体
  const rect = (c: number, r: number): Rect => {
    const own = g.anchor[r * g.cols + c] === r * g.cols + c ? g.cells[r * g.cols + c] : null
    const [w, h] = own ? spanOf(own.action) : [1, 1]
    const [x0] = cellSpan(c, g.cols, W)
    const [, x1] = cellSpan(Math.min(c + w, g.cols) - 1, g.cols, W)
    const [y0] = cellSpan(r, g.rows, H)
    const [, y1] = cellSpan(Math.min(r + h, g.rows) - 1, g.rows, H)
    return { x0, y0, x1, y1 }
  }
  px.fill({ x0: 0, y0: 0, x1: W, y1: H }, colBG)
  for (let r = 0; r < g.rows; r++) {
    for (let c = 0; c < g.cols; c++) {
      const i = r * g.cols + c
      if (g.anchor[i] >= 0 && g.anchor[i] !== i) continue // span のセルに覆われている
      const v = cellView(p.cfg, g.cells[i]?.action ?? null)
      drawCell(font, px, rect(c, r), v, p.mode, p.pressed?.has(`${c},${r}`) ?? false, fill, env)
    }
  }
  if (title) {
    const bw = font.textWidth(title) * badgeScale + 2 * badgePad
    const bh = FONT_H * badgeScale + 2 * badgePad
    const b = { x0: W - bw, y0: 0, x1: W, y1: bh }
    px.fill(b, modeBadge[p.mode])
    px.text(font, b.x0 + badgePad, b.y0 + badgePad, title, badgeScale, modeBadgeText[p.mode], b)
  }
  return { pixels: px.data, layout: { cols: g.cols, rows: g.rows, w: W, h: H, rect, anchor: g.anchor } }
}

function drawCell(font: BitmapFont, px: Pixels, cell: Rect, v: CellView, mode: Mode, pressed: boolean, pressFill: boolean, env: WidgetEnv): void {
  px.fill(cell, colBG)
  const box = inset(cell, cellGap)
  if (!v.mapped) {
    px.frame(box, 1, colEmptyBorder)
    return
  }
  let fillC = v.layer ? colLayerCell : colCell
  let textC = colText
  let subC = colSub
  if (pressed && pressFill) [fillC, textC, subC] = [colPressed, colPressedText, colPressedSub]
  px.fill(box, fillC)
  px.frame(box, borderW, modeBorder[mode])
  if (pressed && !pressFill) {
    // display.go の drawRing と同じ。帯はラベルの余白（textMargin）より細い
    px.frame(box, pressEdgeW, colPressEdge)
    px.frame(inset(box, pressEdgeW), pressGlowW, colPressed)
  }
  const inner = inset(box, textMargin)
  if (v.clock) {
    drawWidget(font, px, inner, v, env, textC, subC)
    return
  }
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

// drawWidget は widget.go の drawWidget と同じ。
function drawWidget(font: BitmapFont, px: Pixels, inner: Rect, v: CellView, env: WidgetEnv, ink: RGB, subInk: RGB): void {
  const area = { ...inner }
  const aw = area.x1 - area.x0
  if (v.label) {
    const cap = v.label.replace(/\n/g, ' ')
    const s = Math.min(fitScale(font, [cap], aw, FONT_H * captionScale), captionScale)
    drawCentered(font, px, area, area.y0, cap, s, subInk)
    area.y0 += FONT_H * s + widgetLineGap
  }
  if (!v.clock) return
  const { time, date, unsynced } = clockLines(v.clock, env.now, env.synced)
  if (unsynced && ink === colText) {
    ink = colUnsynced
    subInk = colUnsynced
  }
  let bh = 0
  let ds = 0
  if (date) {
    ds = Math.min(fitScale(font, [date], aw, FONT_H * dateMaxScale, dateMaxScale), dateMaxScale)
    bh = FONT_H * ds + widgetLineGap
  }
  const ah = area.y1 - area.y0
  const ts = fitScale(font, [time], aw, ah - bh, clockMaxScale)
  const y = area.y0 + Math.trunc((ah - FONT_H * ts - bh) / 2)
  drawCentered(font, px, area, y, time, ts, ink)
  if (date) drawCentered(font, px, area, y + FONT_H * ts + widgetLineGap, date, ds, subInk)
}

function drawCentered(font: BitmapFont, px: Pixels, area: Rect, y: number, s: string, scale: number, c: RGB): void {
  const x = area.x0 + Math.trunc((area.x1 - area.x0 - font.textWidth(s) * scale) / 2)
  px.text(font, Math.max(x, area.x0), y, s, scale, c, area)
}
