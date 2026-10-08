// Brain の画面のプレビュー。display.go の drawAll と同じ配置・配色・字形で描く。

import { FONT_H, type BitmapFont } from './font'
import { prettyCombo } from './keys'
import { clockDef, clockLines, type ClockDef } from './clock'
import { LAYER_VERB, MOUSE_LABELS, TERM_LABELS, USB_LABELS, PAD_DEFAULTS, actionKind, actionTarget, isLayerAction, layerTitle, resolveGrid, spanOf, type LayerKind } from './model'
import { TEXT_NONE, textExpired, textInk, textLayout } from './textwidget'
import { TODO_EMPTY, TODO_PAD, todoCaption, todoEllipsis, todoGeometry, todoOrder, type TodoGeom } from './todowidget'
import { CAL_BAR_W, CAL_INFO, CAL_NOW, CAL_PAST, CAL_TIME_COL, calLayout, calWidgetOf, type CalRow, type CalWidget } from './calwidget'
import { unpack565, type Image565 } from './image'
import type { ActionSpec, CalendarData, Config, PressStyle, TextEntry, TodoItem, TodoList } from './types'

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
const colHalo: RGB = [0, 0, 0] // 背景画像の上の文字の縁取り
// trackpad.go
const colPadBand: RGB = [0x26, 0x34, 0x48] // スクロールの帯
const colPadRail: RGB = [0x50, 0x60, 0x74] // 帯の中の線
const colUnsynced: RGB = [0xff, 0x80, 0x20] // 時刻を合わせていない時計
// todowidget.go
const colTodoDone: RGB = [0x6a, 0x74, 0x80] // 完了した項目
const colTodoRule: RGB = [0x30, 0x3c, 0x4c] // 行の区切り
const colTodoOff: RGB = [0x40, 0x48, 0x54] // これ以上送れないときの ▲ ▼
// calwidget.go
const colCalNow: RGB = [0x1e, 0x4a, 0x7c] // 今の予定の行
const colCalWarn: RGB = colUnsynced // 最終更新が古い、取得に失敗した、時刻を合わせていない
const modeBorder: Record<Mode, RGB> = { base: colBorder, latched: [0x40, 0xc0, 0x70], temp: [0xff, 0x80, 0x20] }
const modeBadge: Record<Mode, RGB> = { base: [0x3a, 0x48, 0x5c], latched: [0x2e, 0x9e, 0x5b], temp: [0xff, 0x80, 0x20] }
const modeBadgeText: Record<Mode, RGB> = { base: colText, latched: colText, temp: [0, 0, 0] }

const cellGap = 4
export const CELL_GAP = cellGap // セルの箱は、セルの範囲からこれだけ内側（セルの背景画像の大きさ）
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
  widget?: string // ウィジェットの種類
  clock?: ClockDef // 時計のウィジェット
  textId?: string // テキストのウィジェットの id
  todoRows?: number // Todo のウィジェット（0 なら高さで決める）
  cal?: CalWidget // カレンダーのウィジェット
  padScroll?: number // トラックパッドのスクロールの帯の幅（0 なら帯なし）
  background?: string // セルの背景画像の id
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
  const v = cellView1(cfg, a)
  if (a.background) v.background = a.background
  return v
}

function cellView1(cfg: Config, a: ActionSpec): CellView {
  const k = actionKind(a)
  let label = a.label ?? ''
  if (a.widget) {
    const v: CellView = { mapped: true, layer: isLayerAction(a), label, sub: '', widget: a.widget }
    if (a.widget === 'clock') v.clock = clockDef(a)
    if (a.widget === 'text') v.textId = a.id ?? ''
    if (a.widget === 'todo') v.todoRows = typeof a.rows === 'number' ? a.rows : 0
    if (a.widget === 'calendar') v.cal = calWidgetOf(a)
    if (a.widget === 'trackpad') v.padScroll = typeof a.scroll_width === 'number' ? a.scroll_width : PAD_DEFAULTS.scroll_width
    return v
  }
  if (k !== 'key' && k !== 'none' && k !== 'mouse' && k !== 'usb_mode' && k !== 'terminal') {
    const t = actionTarget(a)
    const dest = cfg.layers.find((l) => l.name === t)
    const destTitle = dest ? layerTitle(dest) : (t ?? '')
    let sub = LAYER_VERB[k as LayerKind]
    if (label === '') label = destTitle
    else sub += ':' + destTitle
    return { mapped: true, layer: true, label, sub }
  }
  const keys = k === 'mouse' ? (MOUSE_LABELS[a.mouse!] ?? '') : k === 'usb_mode' ? (USB_LABELS[a.usb_mode!] ?? '')
    : k === 'terminal' ? (TERM_LABELS[a.terminal!] ?? '') : prettyCombo(a.key ?? '')
  if (label === '' || label === keys) return { mapped: true, layer: false, label: keys, sub: '' }
  return { mapped: true, layer: false, label, sub: keys }
}

// Placed は、画面に置いた画像（display.go の placed）。
interface Placed {
  img: Image565
  dst: Rect
}

// centered は、画像を area の中央に置く（display.go の centered。負の数の割り算は 0 に向けて切り捨てる）。
export function centered(img: Image565 | undefined, area: Rect): Placed | null {
  if (!img) return null
  const x = area.x0 + Math.trunc((area.x1 - area.x0 - img.w) / 2)
  const y = area.y0 + Math.trunc((area.y1 - area.y0 - img.h) / 2)
  return { img, dst: { x0: x, y0: y, x1: x + img.w, y1: y + img.h } }
}

function inside(r: Rect, d: Rect): boolean {
  if (r.x0 >= r.x1 || r.y0 >= r.y1) return true
  return r.x0 >= d.x0 && r.y0 >= d.y0 && r.x1 <= d.x1 && r.y1 <= d.y1
}

class Pixels {
  data: Uint8ClampedArray
  halo = false // true なら、text は文字の周り 1 ドットを colHalo で縁取る（fb.go の Canvas.halo）
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
  // blit は、dst に置いた画像のうち clip に入る部分を写す（fb.go の blitImage）。
  blit(img: Image565, dst: Rect, clip: Rect): void {
    const r = intersect(intersect(dst, clip), { x0: 0, y0: 0, x1: this.w, y1: this.h })
    for (let y = r.y0; y < r.y1; y++) {
      let o = (y * this.w + r.x0) * 4
      let i = (y - dst.y0) * img.w + (r.x0 - dst.x0)
      for (let x = r.x0; x < r.x1; x++, o += 4, i++) {
        const [R, G, B] = unpack565(img.pix[i])
        this.data[o] = R
        this.data[o + 1] = G
        this.data[o + 2] = B
        this.data[o + 3] = 255
      }
    }
  }
  // paintBack は display.go の paintBack と同じ。base の色の上に imgs を順に重ねる。
  paintBack(r: Rect, base: RGB, ...imgs: (Placed | null)[]): void {
    let first = 0
    imgs.forEach((p, i) => {
      if (p && inside(r, p.dst)) first = i
    })
    if (first === 0 && !(imgs[0] && inside(r, imgs[0].dst))) this.fill(r, base)
    for (const p of imgs.slice(first)) if (p) this.blit(p.img, p.dst, r)
  }
  // text は fb.go の text と同じ。halo なら、先に文字列全体の縁取りを描いてから文字を描く。
  text(font: BitmapFont, x: number, y: number, s: string, scale: number, c: RGB, clip: Rect): void {
    if (this.halo) this.glyphs(font, x, y, s, scale, colHalo, clip, 1)
    this.glyphs(font, x, y, s, scale, c, clip, 0)
  }
  private glyphs(font: BitmapFont, x: number, y: number, s: string, scale: number, c: RGB, clip: Rect, grow: number): void {
    for (const ch of s) {
      const g = font.glyph(ch.codePointAt(0)!)
      for (let gy = 0; gy < g.rows.length; gy++) {
        const bits = g.rows[gy]
        if (!bits) continue
        for (let gx = 0; gx < 8; gx++) {
          if (!(bits & (0x80 >> gx))) continue
          const px = { x0: x + gx * scale - grow, y0: y + gy * scale - grow, x1: x + (gx + 1) * scale + grow, y1: y + (gy + 1) * scale + grow }
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
  texts?: Record<string, TextEntry> // テキストのタイルの中身（Brain の get_text）
  todo?: TodoList // Todo の一覧（Brain の get_todo）
  todoPage?: number // Todo のセルに出すページ（0 から）
  calendar?: CalendarData | null // カレンダーの予定（Brain の get_calendar）
  calendarPage?: number // カレンダーのセルに出すページ（0 から）。省略すると、触っていないときのページ
  images?: (id: string) => Image565 | undefined // 背景画像。ない id は背景なしで描く（Brain と同じ）
  mouseOff?: boolean // USB にマウスがない（キーボードだけの形）。トラックパッドに「マウスはオフ」と出す
}

export interface WidgetEnv {
  now: Date
  synced: boolean
  texts: Record<string, TextEntry>
  todo: TodoItem[]
  todoPage: number
  calendar: CalendarData | null
  calendarPage?: number
  mouseOff: boolean
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
  const env: WidgetEnv = { now: p.now ?? new Date(), synced: p.synced ?? true, texts: p.texts ?? {}, todo: p.todo?.items ?? [],
    todoPage: p.todoPage ?? 0, calendar: p.calendar ?? null, calendarPage: p.calendarPage, mouseOff: p.mouseOff ?? false }
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
  const image = (id?: string) => (id ? p.images?.(id) : undefined)
  const wall = centered(image(g.wallpaper), { x0: 0, y0: 0, x1: W, y1: H })
  for (let r = 0; r < g.rows; r++) {
    for (let c = 0; c < g.cols; c++) {
      const i = r * g.cols + c
      if (g.anchor[i] >= 0 && g.anchor[i] !== i) continue // span のセルに覆われている
      const v = cellView(p.cfg, g.cells[i]?.action ?? null)
      drawCell(font, px, rect(c, r), v, p.mode, p.pressed?.has(`${c},${r}`) ?? false, fill, env, wall, image(v.background))
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

// drawCell は display.go の drawCell と同じ。背景は 黒（または壁紙）→ セルの塗り（壁紙があれば壁紙）→ セルの画像。
// 背景画像があるセルでは、文字を縁取る。fill で押しているあいだは黄色で塗りつぶす。
function drawCell(font: BitmapFont, px: Pixels, cell: Rect, v: CellView, mode: Mode, pressed: boolean, pressFill: boolean, env: WidgetEnv,
  wall: Placed | null, ownImg: Image565 | undefined): void {
  const box = inset(cell, cellGap)
  const own = centered(ownImg, box)
  let hasImg = !!wall || !!own
  px.paintBack(cell, colBG, wall)
  if (!v.mapped) {
    px.frame(box, 1, colEmptyBorder)
    return
  }
  let fillC = v.layer ? colLayerCell : colCell
  let textC = colText
  let subC = colSub
  if (pressed && pressFill) {
    ;[fillC, textC, subC] = [colPressed, colPressedText, colPressedSub]
    px.fill(box, fillC)
    hasImg = false
  } else px.paintBack(box, fillC, wall, own)
  px.frame(box, borderW, modeBorder[mode])
  if (pressed && !pressFill) {
    // display.go の drawRing と同じ。帯はラベルの余白（textMargin）より細い
    px.frame(box, pressEdgeW, colPressEdge)
    px.frame(inset(box, pressEdgeW), pressGlowW, colPressed)
  }
  const inner = inset(box, textMargin)
  px.halo = hasImg
  try {
    if (v.widget === 'trackpad') drawPad(font, px, cell, box, inner, v, env, subC)
    else drawCellContent(font, px, inner, v, env, textC, subC)
  } finally {
    px.halo = false
  }
}

// drawPad は trackpad.go の drawPad と同じ。見出しと、右端のスクロールの帯（タッチの判定と同じく、セルの右端から scroll_width ドット）。
function drawPad(font: BitmapFont, px: Pixels, cell: Rect, box: Rect, inner: Rect, v: CellView, env: WidgetEnv, subInk: RGB): void {
  const sw = v.padScroll ?? 0
  const area = { ...inner }
  if (sw > 0) {
    const inb = inset(box, borderW)
    const band = { x0: Math.max(cell.x1 - sw, inb.x0), y0: inb.y0, x1: inb.x1, y1: inb.y1 }
    if (band.x0 < band.x1 && band.y0 < band.y1) {
      area.x1 = Math.min(area.x1, band.x0 - textMargin)
      const halo = px.halo
      px.halo = false
      px.fill(band, colPadBand)
      const bw = band.x1 - band.x0
      const bh = band.y1 - band.y0
      const cx = band.x0 + Math.trunc(bw / 2)
      const s = Math.min(fitScale(font, ['▲'], bw - 4, Math.trunc(bh / 4), 2), 2)
      const top = band.y0 + textMargin
      const bottom = band.y1 - textMargin - FONT_H * s
      const railTop = top + FONT_H * s + 4
      const railBottom = bottom - 4
      if (railBottom > railTop) px.fill({ x0: cx - 1, y0: railTop, x1: cx + 1, y1: railBottom }, colPadRail)
      drawCentered(font, px, band, top, '▲', s, colSub)
      drawCentered(font, px, band, bottom, '▼', s, colSub)
      px.halo = halo
    }
  }
  const aw = area.x1 - area.x0
  if (aw <= 0) return
  if (v.label) {
    const cap = v.label.replace(/\n/g, ' ')
    const s = Math.min(fitScale(font, [cap], aw, FONT_H * captionScale), captionScale)
    drawCentered(font, px, area, area.y0, cap, s, subInk)
    area.y0 += FONT_H * s + widgetLineGap
  }
  if (env.mouseOff) {
    const ah = area.y1 - area.y0
    const s1 = Math.min(fitScale(font, [PAD_OFF_TEXT], aw, Math.trunc(ah / 2), 3), 3)
    const s2 = Math.min(fitScale(font, [PAD_OFF_HINT], aw, Math.trunc(ah / 4), 2), 2)
    const hh = FONT_H * s1 + widgetLineGap + FONT_H * s2
    const y = area.y0 + Math.trunc((ah - hh) / 2)
    drawCentered(font, px, area, y, PAD_OFF_TEXT, s1, colPadOff)
    drawCentered(font, px, area, y + FONT_H * s1 + widgetLineGap, PAD_OFF_HINT, s2, subInk)
  }
}

const PAD_OFF_TEXT = 'マウスはオフ'
const PAD_OFF_HINT = 'USB はキーボードだけの形です'
const colPadOff: RGB = [0xff, 0x80, 0x20]

function drawCellContent(font: BitmapFont, px: Pixels, inner: Rect, v: CellView, env: WidgetEnv, textC: RGB, subC: RGB): void {
  if (v.widget) {
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
  const label = v.todoRows !== undefined ? todoCaption(v.label, env.todo) : v.label
  if (label) {
    const cap = label.replace(/\n/g, ' ')
    const s = Math.min(fitScale(font, [cap], aw, FONT_H * captionScale), captionScale)
    drawCentered(font, px, area, area.y0, cap, s, subInk)
    area.y0 += FONT_H * s + widgetLineGap
  }
  if (v.todoRows !== undefined) {
    drawTodo(font, px, area, v.todoRows, env, subInk)
    return
  }
  if (v.cal) {
    drawCalendar(font, px, area, v.cal, env, subInk)
    return
  }
  if (v.textId !== undefined) {
    const e = Object.hasOwn(env.texts, v.textId) ? env.texts[v.textId] : undefined
    let body = TEXT_NONE
    let c = subInk
    if (e) {
      body = e.text
      c = ink === colText ? textInk(e.style, textExpired(e, env.now)) : ink // 押したとき（fill）は、押したときの色
    }
    const { lines, scale } = textLayout(font, body, aw, area.y1 - area.y0)
    let y = area.y0 + Math.trunc((area.y1 - area.y0 - lines.length * FONT_H * scale) / 2)
    for (const ln of lines) {
      drawCentered(font, px, area, y, ln, scale, c)
      y += FONT_H * scale
    }
    return
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

// drawTodo は todowidget.go の drawTodo と同じ。area は見出しの下の範囲。
function drawTodo(font: BitmapFont, px: Pixels, area: Rect, rows: number, env: WidgetEnv, subInk: RGB): void {
  const items = todoOrder(env.todo)
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  if (!items.length) {
    const s = Math.min(fitScale(font, [TODO_EMPTY], aw, ah), 2)
    drawCentered(font, px, area, area.y0 + Math.trunc((ah - FONT_H * s) / 2), TODO_EMPTY, s, subInk)
    return
  }
  const g = todoGeometry(area, items.length, rows)
  const page = Math.max(Math.min(env.todoPage, g.pages - 1), 0)
  g.rows.forEach((r, i) => {
    const k = page * g.per + i
    if (k >= items.length) return
    if (i > 0) px.fill({ ...r, y1: r.y0 + 1 }, colTodoRule)
    drawTodoRow(font, px, r, items[k], g.scale)
  })
  if (g.nav) drawPageNav(font, px, g, page)
}

// drawPageNav は、ページ送りの帯（▲、ページ番号、▼）を描く（todowidget.go の drawPageNav）。
function drawPageNav(font: BitmapFont, px: Pixels, g: TodoGeom, page: number): void {
  px.fill({ ...g.nav!, y1: g.nav!.y0 + 1 }, colTodoRule)
  const arrow = (zone: Rect, s: string, ok: boolean) => {
    const sc = Math.min(fitScale(font, [s], zone.x1 - zone.x0, zone.y1 - zone.y0 - 4), 3)
    drawCentered(font, px, zone, zone.y0 + Math.trunc((zone.y1 - zone.y0 - FONT_H * sc) / 2), s, sc, ok ? colText : colTodoOff)
  }
  arrow(g.up!, '▲', page > 0)
  arrow(g.down!, '▼', page < g.pages - 1)
  const p = `${page + 1}/${g.pages}`
  const mid = g.mid!
  const s = Math.min(fitScale(font, [p], mid.x1 - mid.x0, FONT_H * 2), 2)
  drawCentered(font, px, mid, mid.y0 + Math.trunc((mid.y1 - mid.y0 - FONT_H * s) / 2), p, s, colSub)
}

// drawCalendar は calwidget.go の drawCalendar と同じ。area は見出しの下の範囲。
function drawCalendar(font: BitmapFont, px: Pixels, area: Rect, w: CalWidget, env: WidgetEnv, subInk: RGB): void {
  const l = calLayout(area, w, env.calendar, env.now.getTime(), env.synced)
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  if (l.message) {
    const s = Math.min(fitScale(font, [l.message], aw, ah), 2)
    drawCentered(font, px, area, area.y0 + Math.trunc((ah - FONT_H * s) / 2), l.message, s, subInk)
    return
  }
  const g = l.geom
  const rows = l.rows ?? []
  const page = Math.max(Math.min(env.calendarPage ?? l.auto_page, g.pages - 1), 0)
  g.rows.forEach((r, i) => {
    const k = page * g.per + i
    if (k >= rows.length) return
    if (i > 0) px.fill({ ...r, y1: r.y0 + 1 }, colTodoRule)
    drawCalRow(font, px, r, rows[k], l.scale)
  })
  if (g.nav) drawPageNav(font, px, g, page)
  const f = todoEllipsis(font, l.footer, Math.trunc(aw / l.footer_scale))
  px.text(font, area.x0, l.footerY, f, l.footer_scale, l.footer_warn ? colCalWarn : colSub, area)
}

function hexColor(s: string): RGB {
  const m = /^#([0-9a-fA-F]{6})$/.exec(s)
  if (!m) return colSub
  const v = parseInt(m[1], 16)
  return [(v >> 16) & 255, (v >> 8) & 255, v & 255]
}

// drawCalRow は 1 行を描く。左にカレンダーの色の帯、時刻、予定の名前（calwidget.go の drawCalRow）。
function drawCalRow(font: BitmapFont, px: Pixels, r: Rect, row: CalRow, s: number): void {
  let labelC = colSub
  let titleC = colText
  if (row.state === CAL_NOW) {
    px.fill(inset(r, 1), colCalNow)
    labelC = colText
  } else if (row.state === CAL_PAST) [labelC, titleC] = [colTodoDone, colTodoDone]
  else if (row.state === CAL_INFO) titleC = colSub
  let x = r.x0 + TODO_PAD
  if (row.color) {
    const c = row.state === CAL_PAST ? colTodoDone : hexColor(row.color)
    const bh = FONT_H * s
    const y = r.y0 + Math.trunc((r.y1 - r.y0 - bh) / 2)
    px.fill({ x0: x, y0: y, x1: x + CAL_BAR_W * s, y1: y + bh }, c)
    x += CAL_BAR_W * s + TODO_PAD
  }
  const ty = r.y0 + Math.trunc((r.y1 - r.y0 - FONT_H * s) / 2)
  if (row.label) {
    px.text(font, x, ty, row.label, s, labelC, r)
    x += Math.max(font.textWidth(row.label), font.textWidth(CAL_TIME_COL)) * s + 4 * s
  }
  const w = Math.trunc((r.x1 - TODO_PAD - x) / s)
  if (w > 0) px.text(font, x, ty, todoEllipsis(font, row.title, w), s, titleC, r)
}

// drawTodoRow は 1 行を描く。左にチェックの箱、右に項目の文（完了なら薄く、取り消し線）。
function drawTodoRow(font: BitmapFont, px: Pixels, r: Rect, it: TodoItem, scale: number): void {
  const [textC, boxC] = it.done ? [colTodoDone, colTodoDone] : [colText, colSub]
  const bs = FONT_H * scale - 2
  const by = r.y0 + Math.trunc((r.y1 - r.y0 - bs) / 2)
  const box = { x0: r.x0 + TODO_PAD, y0: by, x1: r.x0 + TODO_PAD + bs, y1: by + bs }
  px.frame(box, 2, boxC)
  if (it.done) px.fill(inset(box, 4), boxC)
  const tx = box.x1 + TODO_PAD
  const s = todoEllipsis(font, it.text, Math.trunc((r.x1 - TODO_PAD - tx) / scale))
  const ty = r.y0 + Math.trunc((r.y1 - r.y0 - FONT_H * scale) / 2)
  px.text(font, tx, ty, s, scale, textC, r)
  if (it.done) {
    const y = ty + Math.trunc((FONT_H * scale) / 2)
    px.fill({ x0: tx, y0: y, x1: Math.min(tx + font.textWidth(s) * scale, r.x1 - TODO_PAD), y1: y + Math.max(scale - 1, 1) }, textC)
  }
}
