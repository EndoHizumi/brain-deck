// Brain の画面のプレビュー。display.go の drawAll と同じ配置・配色・字形で描く。

import { drawMemo, drawPenTab, penTabOf, type PenTabDef } from './penwidget'
import { FONT_H, type BitmapFont } from './font'
import { prettyCombo } from './keys'
import { clockDef, clockLines, type ClockDef } from './clock'
import { BAR_BATT_H, BAR_BATT_TO, BAR_BATT_W, BAR_GAP, BAR_H, BAR_ITEMS_DEFAULT, BAR_PAD, BAR_SCALE, BAR_TEXT_Y, barItems, colBar, colBarRule, type BarEnv, type BarItemView } from './statusbar'
import { HOME_ARROW, HOME_ICON_BASE, HOME_IMAGE_ICON, HOME_LABEL, HOME_NAME_GAP, HOME_SLOT_PAD, ICON_PALETTE, ICON_ROWS, homeGeometry, homeIconFit, homeName } from './home'
import { LAYER_VERB, ROTATE_LABELS, INPUT_LABELS, OFFLINE_LABEL, linkOnline, needsPC, barShown, MEDIA_LABELS, MEDIA_OFF_LABEL, MOUSE_LABELS, TERM_LABELS, USB_LABELS, PAD_DEFAULTS, actionKind, actionTarget, isLayerAction, layerTitle, resolveGrid, spanOf, type LayerKind } from './model'
import { TEXT_NONE, textExpired, textInk, textLayout } from './textwidget'
import { TODO_EMPTY, TODO_PAD, todoCaption, todoEllipsis, todoGeometry, todoOrder, type TodoGeom } from './todowidget'
import { CAL_BAR_W, CAL_INFO, CAL_NOW, CAL_PAST, CAL_TIME_COL, calCaption, calLayout, calWidgetOf, type CalRow, type CalWidget } from './calwidget'
import { MONTH_MAX_OFFSET, monthFollowDay, monthLayout, monthWidgetOf, type MonthDay, type MonthState, type MonthWidget } from './monthwidget'
import { unpack565, type Image565 } from './image'
import { blend8, weatherWidgetOf, wthLayout, type Icon, type WeatherWidget } from './weatherwidget'
import { encodeQR } from './qr'
import { presenterDef, presenterView, type PresenterDef } from './presenter'
import { CALC_BUTTONS, CALC_COLS, CALC_PAD, DEFAULT_CALC_ID, baseName, buttonColor, buttonLabel, calcBigText, calcDisplay, calcGeom, calcTail,
  colCalcBase, colCalcDisp, validDigit, type CalcState } from './calc'
import { GRAPH_LINE_W, GRAPH_NONE, graphLayout, graphSegment, graphWidgetOf, type GraphWidget } from './graphwidget'
import {
  GAUGE_H, QR_NONE, QR_TOO_BIG, STOPWATCH_IDLE, batteryLines, colInkError, colInkOK, colInkWarn, colQRDark, colQRDarkOld, colQRLight, colQRLitOld,
  contrastInk, pomoDurations, pomoInk, pomoText, pomodoroView, presenceStates, presenceView, qrFit, stateId, stopwatchElapsed,
  stopwatchText, type PresenceDef,
} from './gadgetwidget'
import type { ActionSpec, BarItem, BatteryInfo, HomeConfig, CalendarData, Config, GraphSeries, PressStyle, TextEntry, TodoItem, TodoList, WeatherData, WidgetsState } from './types'

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
// monthwidget.go
const colMonthSat: RGB = [0x60, 0xa8, 0xff] // 土曜
const colMonthSun: RGB = [0xff, 0x60, 0x60] // 日曜と祝日
const colMonthDim: RGB = [0x50, 0x5a, 0x66] // 前後の月の日付
const colMonthDimSat: RGB = [0x30, 0x50, 0x78]
const colMonthDimSun: RGB = [0x78, 0x38, 0x3c]
const colMonthToday: RGB = colText // 今日の塗り
const colMonthTodayIn: RGB = [0x10, 0x14, 0x1a] // 今日の数字
const colMonthTodaySa: RGB = [0x10, 0x50, 0xc0]
const colMonthTodaySu: RGB = [0xd0, 0x20, 0x20]
const colMonthMore: RGB = colSub // 点に入りきらないカレンダーがある
// graphwidget.go
const colGraphLine: RGB = [0x4f, 0x9d, 0xff]
const colGraphOld: RGB = [0x34, 0x4c, 0x68] // 古いときの線
const colGraphGrid: RGB = colTodoRule
const JA_WEEKDAYS = ['日', '月', '火', '水', '木', '金', '土']
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
  month?: MonthWidget // 月の表示のウィジェット
  weather?: WeatherWidget // 天気のウィジェット
  graph?: GraphWidget // グラフのウィジェット
  calcId?: string // 電卓のウィジェットの id
  penTab?: PenTabDef // ペンタブレット
  memo?: boolean // 手書きメモ
  padScroll?: number // トラックパッドのスクロールの帯の幅（0 なら帯なし）
  background?: string // セルの背景画像の id
  gadget?: Gadget // 電池、ストップウォッチ、ポモドーロ、カウンター、QR コード、在席ランプ
  media?: boolean // メディアキーのセル。USB にマウスがない（ブートキーボードの形）ときは、無効と描く
  needsPC?: boolean // PC に送るセルか、PC から受け取るタイル。PC から外れているあいだは、休みと描く（offline.go）
}

// Gadget は、新しいウィジェットの描き方に要る設定（gadgetwidget.go）。
export interface Gadget {
  kind: 'battery' | 'stopwatch' | 'pomodoro' | 'counter' | 'qr' | 'presence' | 'presenter'
  id: string
  work: number // pomodoro：作業の時間（ミリ秒）
  states: PresenceDef[] // presence
  presenter?: PresenterDef // presenter
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
    const v: CellView = { mapped: true, layer: isLayerAction(a), label, sub: '', widget: a.widget, needsPC: needsPC(a) }
    if (a.widget === 'clock') v.clock = clockDef(a)
    if (a.widget === 'text') v.textId = a.id ?? ''
    if (a.widget === 'todo') v.todoRows = typeof a.rows === 'number' ? a.rows : 0
    if (a.widget === 'calendar') v.cal = calWidgetOf(a)
    if (a.widget === 'month') v.month = monthWidgetOf(a)
    if (a.widget === 'weather') v.weather = weatherWidgetOf(a)
    if (a.widget === 'graph') v.graph = graphWidgetOf(a)
    if (a.widget === 'calculator') v.calcId = a.id || DEFAULT_CALC_ID
    if (a.widget === 'pentablet') v.penTab = penTabOf(a)
    if (a.widget === 'memo') v.memo = true
    if (a.widget === 'trackpad') v.padScroll = typeof a.scroll_width === 'number' ? a.scroll_width : PAD_DEFAULTS.scroll_width
    if (['battery', 'stopwatch', 'pomodoro', 'counter', 'qr', 'presence', 'presenter'].includes(a.widget)) {
      v.gadget = { kind: a.widget as Gadget['kind'], id: stateId(a), work: pomoDurations(a).work, states: presenceStates(a.states) }
      if (a.widget === 'presenter') v.gadget.presenter = presenterDef(a)
    }
    return v
  }
  if (k !== 'key' && k !== 'none' && k !== 'mouse' && k !== 'media' && k !== 'usb_mode' && k !== 'terminal' && k !== 'rotate' && k !== 'input') {
    const t = actionTarget(a)
    const dest = cfg.layers.find((l) => l.name === t)
    const destTitle = dest ? layerTitle(dest) : (t ?? '')
    let sub = LAYER_VERB[k as LayerKind]
    if (label === '') label = destTitle
    else sub += ':' + destTitle
    return { mapped: true, layer: true, label, sub }
  }
  const keys = k === 'mouse' ? (MOUSE_LABELS[a.mouse!] ?? '') : k === 'media' ? (MEDIA_LABELS[a.media!] ?? '')
    : k === 'usb_mode' ? (USB_LABELS[a.usb_mode!] ?? '') : k === 'terminal' ? (TERM_LABELS[a.terminal!] ?? '')
    : k === 'rotate' ? (ROTATE_LABELS[a.rotate!] ?? '') : k === 'input' ? (INPUT_LABELS[a.input!] ?? '') : prettyCombo(a.key ?? '')
  const v: CellView = label === '' || label === keys ? { mapped: true, layer: false, label: keys, sub: '' } : { mapped: true, layer: false, label, sub: keys }
  if (k === 'media') v.media = true
  if (k !== 'none' && needsPC(a)) v.needsPC = true
  return v
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
  // blendIcon は、天気アイコンを箱 box の中央に、不透明度で重ねる（weathericon.go の blendIcon）。
  // 下の色は Brain の画面と同じく RGB565 に丸めてから混ぜる。
  blendIcon(ic: Icon, box: Rect, clip: Rect): void {
    const x0 = box.x0 + Math.trunc((box.x1 - box.x0 - ic.w) / 2)
    const y0 = box.y0 + Math.trunc((box.y1 - box.y0 - ic.h) / 2)
    const r = intersect(intersect({ x0, y0, x1: x0 + ic.w, y1: y0 + ic.h }, clip), { x0: 0, y0: 0, x1: this.w, y1: this.h })
    for (let y = r.y0; y < r.y1; y++) {
      for (let x = r.x0; x < r.x1; x++) {
        const i = (y - y0) * ic.w + (x - x0)
        const a = ic.a[i]
        if (a === 0) continue
        let [R, G, B] = unpack565(ic.pix[i])
        const o = (y * this.w + x) * 4
        if (a < 255) {
          const d = this.data
          const [dr, dg, db] = unpack565(((d[o] >> 3) << 11) | ((d[o + 1] >> 2) << 5) | (d[o + 2] >> 3))
          ;[R, G, B] = [blend8(R, dr, a), blend8(G, dg, a), blend8(B, db, a)]
        }
        this.data[o] = R
        this.data[o + 1] = G
        this.data[o + 2] = B
        this.data[o + 3] = 255
      }
    }
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
  monthOffset?: number // 月の表示に出す月（今月からのずれ。-render-month-offset）
  monthSelect?: string // 月の表示で選んだ日（YYYY-MM-DD。-render-month-select）。follow のカレンダーにもその日の予定が出る
  images?: (id: string) => Image565 | undefined // 背景画像。ない id は背景なしで描く（Brain と同じ）
  mouseOff?: boolean // USB にマウスがない（キーボードだけの形）。トラックパッドに「マウスはオフ」と出す
  widgets?: WidgetsState // ストップウォッチ、ポモドーロ、カウンター、在席ランプの状態（Brain の get_widgets）
  qr?: Record<string, TextEntry> // QR コードのウィジェットの中身（Brain の get_qr）
  battery?: BatteryInfo | null // 電池（Brain の get_status）。省略すると読めないとき
  weather?: WeatherData | null // 天気予報（Brain の get_weather）
  weatherIcons?: (name: string) => Icon | undefined // 天気アイコン（Brain の get_weather_icon）。ないアイコンは描かない
  graph?: Record<string, GraphSeries> // グラフの値（Brain の get_graph）
  calc?: Record<string, CalcState> // 電卓の状態（Brain の get_widgets の calculator）
  calcPressed?: number // 電卓で押しているボタン（行 × 6 + 列。-render-calc-pressed）
  home?: number // ホーム画面を、このページ（0 から）で描く（stack は使わない）
  link?: string // PC とのつながり（UDC の state か power。-render-link）。省略すると configured。configured と suspended のほかでは、PC に送るセルを休みと描く
  lastSync?: Date | null // 状態の帯の、最後に同期した時刻（-render-last-sync）
}

export interface WidgetEnv {
  now: Date
  synced: boolean
  texts: Record<string, TextEntry>
  todo: TodoItem[]
  todoPage: number
  calendar: CalendarData | null
  calendarPage?: number
  month: MonthState
  mouseOff: boolean
  widgets: WidgetsState
  qr: Record<string, TextEntry>
  battery: BatteryInfo | null
  weather: WeatherData | null
  weatherIcons: (name: string) => Icon | undefined
  graph: Record<string, GraphSeries>
  calc: Record<string, CalcState>
  calcPressed: number
  offline: boolean // PC から外れている（needsPC のセルを休みと描く）
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
  const portrait = W < H
  const home = p.home !== undefined && p.cfg.home ? p.cfg.home : null
  // ホーム画面は 1×1 の格子に、アプリを並べるセルを 1 つ置いたもの（home.go）
  const g = home
    ? { cols: 1, rows: 1, cells: [{ action: {} as ActionSpec, from: -1 }], anchor: [0], owner: -1, wallpaper: home.background }
    : resolveGrid(p.cfg, p.stack, portrait ? 'portrait' : 'landscape')
  const topL = p.cfg.layers[p.stack[p.stack.length - 1]]
  const title = home ? home.label || HOME_LABEL : topL ? layerTitle(topL) : ''
  const bar = home ? (home.status_bar ?? true) : barShown(p.cfg, p.stack)
  const gtop = bar ? BAR_H : 0
  const mode: Mode = home ? 'base' : p.mode
  const fill = (p.pressStyle ?? p.cfg.display?.press_style) === 'fill'
  const px = new Pixels(W, H)
  const env: WidgetEnv = { now: p.now ?? new Date(), synced: p.synced ?? true, texts: p.texts ?? {}, todo: p.todo?.items ?? [],
    todoPage: p.todoPage ?? 0, calendar: p.calendar ?? null, calendarPage: p.calendarPage,
    month: { offset: Math.max(Math.min(p.monthOffset ?? 0, MONTH_MAX_OFFSET), -MONTH_MAX_OFFSET), sel: p.monthSelect ?? '' }, mouseOff: p.mouseOff ?? false,
    widgets: p.widgets ?? {}, qr: p.qr ?? {}, battery: p.battery ?? null, weather: p.weather ?? null,
    weatherIcons: p.weatherIcons ?? (() => undefined), graph: p.graph ?? {}, calc: p.calc ?? {}, calcPressed: p.calcPressed ?? -1,
    offline: !linkOnline(p.link ?? 'configured') }
  // display.go の Layout.rect と同じ。span のセルは覆う範囲全体
  const rect = (c: number, r: number): Rect => {
    const own = g.anchor[r * g.cols + c] === r * g.cols + c ? g.cells[r * g.cols + c] : null
    const [w, h] = own ? spanOf(own.action) : [1, 1]
    const [x0] = cellSpan(c, g.cols, W)
    const [, x1] = cellSpan(Math.min(c + w, g.cols) - 1, g.cols, W)
    const [y0] = cellSpan(r, g.rows, H - gtop)
    const [, y1] = cellSpan(Math.min(r + h, g.rows) - 1, g.rows, H - gtop)
    return { x0, y0: y0 + gtop, x1, y1: y1 + gtop }
  }
  px.fill({ x0: 0, y0: 0, x1: W, y1: H }, colBG)
  const image = (id?: string) => (id ? p.images?.(id) : undefined)
  const wall = centered(image(g.wallpaper), { x0: 0, y0: 0, x1: W, y1: H })
  for (let r = 0; r < g.rows; r++) {
    for (let c = 0; c < g.cols; c++) {
      const i = r * g.cols + c
      if (g.anchor[i] >= 0 && g.anchor[i] !== i) continue // span のセルに覆われている
      if (home) {
        // ホーム画面：枠も塗りも描かず、壁紙（なければ黒）の上にアプリを並べる
        px.paintBack(rect(0, 0), colBG, wall)
        drawHome(font, px, rect(0, 0), home, portrait, p.home!, image, !!wall)
        continue
      }
      const v = cellView(p.cfg, g.cells[i]?.action ?? null)
      drawCell(font, px, rect(c, r), v, mode, p.pressed?.has(`${c},${r}`) ?? false, fill, env, wall, image(v.background))
    }
  }
  if (bar) {
    drawBar(font, px, W, title, mode, p.cfg.status_bar?.items ?? BAR_ITEMS_DEFAULT,
      { now: env.now, synced: env.synced, battery: env.battery, link: p.link ?? 'configured', mouseOff: env.mouseOff, lastSync: p.lastSync ?? null })
  } else if (title) {
    const bw = font.textWidth(title) * badgeScale + 2 * badgePad
    const bh = FONT_H * badgeScale + 2 * badgePad
    const b = { x0: W - bw, y0: 0, x1: W, y1: bh }
    px.fill(b, modeBadge[mode])
    px.text(font, b.x0 + badgePad, b.y0 + badgePad, title, badgeScale, modeBadgeText[mode], b)
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
  const wf = widgetFill(v, env)
  if (pressed && pressFill) {
    ;[fillC, textC, subC] = [colPressed, colPressedText, colPressedSub]
    px.fill(box, fillC)
    hasImg = false
  } else if (wf) {
    // 在席ランプ、点滅しているポモドーロ：ウィジェットが決めた色で塗る（display.go の drawCell）
    fillC = wf
    textC = contrastInk(wf)
    subC = textC
    px.fill(box, fillC)
    hasImg = false
  } else px.paintBack(box, fillC, wall, own)
  px.frame(box, borderW, modeBorder[mode])
  if (pressed && !pressFill) {
    // display.go の drawRing と同じ。帯はラベルの余白（textMargin）より細い
    px.frame(box, pressEdgeW, colPressEdge)
    px.frame(inset(box, pressEdgeW), pressGlowW, colPressed)
  }
  if (!v.widget && v.needsPC && env.offline) {
    // PC から外れている：押しても送らないことを、下の小さな文字と薄い文字で知らせる（display.go の drawCell と同じ）
    v = { ...v, sub: OFFLINE_LABEL }
    if (!(pressed && pressFill)) textC = subC
  } else if (v.media && env.mouseOff) {
    // メディアキーを送れない形（ブートキーボード）。display.go の drawCell と同じ
    v = { ...v, sub: MEDIA_OFF_LABEL }
    if (!(pressed && pressFill)) textC = subC
  }
  const inner = inset(box, textMargin)
  px.halo = hasImg
  try {
    if (v.widget === 'trackpad') drawPad(font, px, cell, box, inner, v, env, subC)
    else drawCellContent(font, px, inner, v, env, textC, subC)
    if (v.widget && v.needsPC && env.offline) drawOfflineTag(font, px, inner)
  } finally {
    px.halo = false
  }
}

// drawOfflineTag は、タイルの右下の隅に「PCなし」の札を描く（offline.go の drawOfflineTag と同じ）。
function drawOfflineTag(font: BitmapFont, px: Pixels, inner: Rect): void {
  const w = font.textWidth(OFFLINE_LABEL) + 4
  const h = FONT_H + 2
  const r = { x0: Math.max(inner.x1 - w, inner.x0), y0: Math.max(inner.y1 - h, inner.y0), x1: inner.x1, y1: inner.y1 }
  px.fill(r, colBG)
  px.text(font, r.x0 + 2, r.y0 + 1, OFFLINE_LABEL, 1, colUnsynced, r)
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
  const follow = v.cal?.follow ? monthFollowDay(env.month, env.now.getTime()) : null
  const label = v.todoRows !== undefined ? todoCaption(v.label, env.todo) : v.cal ? calCaption(v.label, follow) : v.label
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
    drawCalendar(font, px, area, v.cal, env, subInk, follow)
    return
  }
  if (v.month) {
    drawMonth(font, px, area, v.month, env)
    return
  }
  if (v.weather) {
    drawWeather(font, px, area, v.weather, env)
    return
  }
  if (v.graph) {
    drawGraph(font, px, area, v.graph, env, ink, subInk)
    return
  }
  if (v.calcId !== undefined) {
    drawCalc(font, px, area, v.calcId, env)
    return
  }
  const fitF = (l: string[], w: number, hh: number, most: number) => fitScale(font, l, w, hh, most)
  const centeredF = (r: Rect, y: number, s: string, scale: number, c: RGB) => drawCentered(font, px, r, y, s, scale, c)
  if (v.penTab) {
    drawPenTab(px, area, v.penTab, env.mouseOff, fitF, centeredF)
    return
  }
  if (v.memo) {
    drawMemo(font, px, area, fitF, centeredF)
    return
  }
  if (v.gadget) {
    drawGadget(font, px, area, v.gadget, env, ink, subInk)
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

// drawCalc は calcwidget.go の drawCalc と同じ。
function drawCalc(font: BitmapFont, px: Pixels, area: Rect, id: string, env: WidgetEnv): void {
  const s: CalcState = Object.hasOwn(env.calc, id) ? env.calc[id] : { base: 10, tokens: [] }
  const g = calcGeom(area)
  px.fill(g.disp, colCalcDisp)
  const inn = inset(g.disp, CALC_PAD)
  const d = calcDisplay(s)
  const ss = inn.y1 - inn.y0 >= FONT_H * 6 ? 2 : 1
  const bn = baseName(s.base)
  px.text(font, inn.x0, inn.y0, bn, ss, colCalcBase, inn)
  if (d.small) {
    const x0 = inn.x0 + (font.textWidth(bn) + 4) * ss
    const t = calcTail(font, d.small, Math.trunc((inn.x1 - x0) / ss))
    const c = d.smallErr === null ? colSub : d.smallErr ? colUnsynced : colInkOK
    px.text(font, inn.x1 - font.textWidth(t) * ss, inn.y0, t, ss, c, inn)
  }
  const big = { x0: inn.x0, y0: inn.y0 + FONT_H * ss + 2, x1: inn.x1, y1: inn.y1 }
  const [t, sc] = calcBigText(font, d, s.base, big.x1 - big.x0, big.y1 - big.y0)
  px.text(font, big.x1 - font.textWidth(t) * sc, big.y1 - FONT_H * sc, t, sc, d.dim ? colTodoOff : colText, big)
  g.buttons.forEach((r, i) => {
    const b = CALC_BUTTONS[Math.trunc(i / CALC_COLS)][i % CALC_COLS]
    const label = buttonLabel(b, s.base)
    let fill = buttonColor(b)
    let ink = colText
    if (validDigit(b, 16) && !validDigit(b, s.base)) ink = colTodoOff
    if (i === env.calcPressed) [fill, ink] = [colPressed, colPressedText]
    px.fill(r, fill)
    const bs = Math.min(fitScale(font, [label], r.x1 - r.x0 - 4, r.y1 - r.y0 - 4, 3), 3)
    drawCentered(font, px, r, r.y0 + Math.trunc((r.y1 - r.y0 - FONT_H * bs) / 2), label, bs, ink)
  })
}

// drawGraph は graphwidget.go の drawGraph と同じ。area は見出しの下の範囲。
function drawGraph(font: BitmapFont, px: Pixels, area: Rect, g: GraphWidget, env: WidgetEnv, ink: RGB, subInk: RGB): void {
  const l = graphLayout(font, area, g, Object.hasOwn(env.graph, g.id) ? env.graph[g.id] : undefined, env.now)
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  if (l.none) {
    const s = Math.min(fitScale(font, [GRAPH_NONE], aw, ah), 2)
    drawCentered(font, px, area, area.y0 + Math.trunc((ah - FONT_H * s) / 2), GRAPH_NONE, s, subInk)
    return
  }
  let valC = ink
  let lineC = colGraphLine
  if (l.stale) {
    valC = sameRGB(ink, colText) ? colTodoDone : ink // 押したとき（fill）は、押したときの色
    lineC = colGraphOld
  }
  const vb = l.valueBox
  const subH = l.sub ? FONT_H * l.subS + widgetLineGap : 0
  const y = vb.y0 + Math.trunc((vb.y1 - vb.y0 - FONT_H * l.valueS - subH) / 2)
  drawCentered(font, px, vb, y, l.value, l.valueS, valC)
  if (l.sub) drawCentered(font, px, vb, y + FONT_H * l.valueS + widgetLineGap, l.sub, l.subS, colUnsynced)
  const p = l.plot
  if (!p) return
  px.fill({ x0: p.x0, y0: p.y0, x1: p.x1, y1: p.y0 + 1 }, colGraphGrid)
  px.fill({ x0: p.x0, y0: p.y1 - 1, x1: p.x1, y1: p.y1 }, colGraphGrid)
  if (l.hiLabel) {
    px.text(font, l.labelX, p.y0, l.hiLabel, 1, subInk, area)
    px.text(font, l.labelX, p.y1 - FONT_H, l.loLabel, 1, subInk, area)
  }
  if (g.chart === 'bar') {
    l.xs.forEach((x, i) => {
      const y0 = Math.min(l.ys[i], l.barBase)
      let y1 = Math.max(l.ys[i], l.barBase)
      if (y1 === y0) y1 = y0 + 1
      px.fill(intersect({ x0: x, y0, x1: l.barR[i], y1 }, p), lineC)
    })
    return
  }
  const dot = (x: number, y: number) => px.fill({ x0: x, y0: y, x1: x + GRAPH_LINE_W, y1: y + GRAPH_LINE_W }, lineC)
  dot(l.xs[0], l.ys[0])
  for (let i = 1; i < l.xs.length; i++) graphSegment(l.xs[i - 1], l.ys[i - 1], l.xs[i], l.ys[i], dot)
}

// widgetFill は gadgetwidget.go の widgetFill と同じ。セルの塗りをウィジェットが決めるとき、その色。
function widgetFill(v: CellView, env: WidgetEnv): RGB | null {
  const g = v.gadget
  if (!g) return null
  if (g.kind === 'presence') return presenceView(g.states, env.widgets, g.id).color
  if (g.kind === 'pomodoro') {
    const pv = pomodoroView(env.widgets.pomodoro?.[g.id], env.now.getTime(), g.work)
    if (pv.alert && pv.blinkOn) return pomoInk(pv.phase)
  }
  if (g.kind === 'presenter' && presenterView(g.presenter!, env.widgets.presenter?.[g.id], env.now.getTime()).blinkOn) return colInkError
  return null
}

// drawTwoLines は gadgetwidget.go の drawTwoLines と同じ（時計と同じ配置の 2 行）。
function drawTwoLines(font: BitmapFont, px: Pixels, area: Rect, a: string, b: string, inkA: RGB, inkB: RGB): void {
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  let bh = 0
  let ds = 0
  if (b) {
    ds = Math.min(fitScale(font, [b], aw, FONT_H * dateMaxScale, dateMaxScale), dateMaxScale)
    bh = FONT_H * ds + widgetLineGap
  }
  const ts = fitScale(font, [a], aw, ah - bh, clockMaxScale)
  const y = area.y0 + Math.trunc((ah - FONT_H * ts - bh) / 2)
  drawCentered(font, px, area, y, a, ts, inkA)
  if (b) drawCentered(font, px, area, y + FONT_H * ts + widgetLineGap, b, ds, inkB)
}

// drawGadget は gadgetwidget.go の drawGadget と同じ。
function drawGadget(font: BitmapFont, px: Pixels, area: Rect, g: Gadget, env: WidgetEnv, ink: RGB, subInk: RGB): void {
  const now = env.now.getTime()
  const pressed = !sameRGB(ink, colText)
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  const message = (s: string, c: RGB) => {
    const sc = Math.min(fitScale(font, [s], aw, ah), 2)
    drawCentered(font, px, area, area.y0 + Math.trunc((ah - FONT_H * sc) / 2), s, sc, c)
  }
  switch (g.kind) {
    case 'presenter': {
      // presenter.go の drawPresenter と同じ
      const pv = presenterView(g.presenter!, env.widgets.presenter?.[g.id], now)
      let c = ink
      if (pv.color && !pressed) c = pv.color
      if (!pv.running && !pv.over && !pressed) c = subInk
      if (pv.blinkOn && !pressed) {
        c = contrastInk(colInkError)
        subInk = c
      }
      if (ah > GAUGE_H * 3) {
        const gr = { x0: area.x0, y0: area.y1 - GAUGE_H, x1: area.x1, y1: area.y1 }
        px.frame(gr, 1, subInk)
        const inn = inset(gr, 2)
        const fw = Math.trunc(((inn.x1 - inn.x0) * pv.bar) / 1000)
        let gc: RGB = pv.color ?? colSub // gadgetwidget.go の colGauge（colSub と同じ色）
        if (pressed || pv.blinkOn) gc = subInk
        if (fw > 0) px.fill({ ...inn, x1: inn.x0 + fw }, gc)
        area = { ...area, y1: area.y1 - (GAUGE_H + widgetLineGap) }
      }
      drawTwoLines(font, px, area, pv.main, pv.sub, c, subInk)
      return
    }
    case 'battery': {
      const b = batteryLines(env.battery, ink, colSub)
      let c = pressed ? ink : b.c
      if (b.pct >= 0 && ah > GAUGE_H * 3) {
        const gr = { x0: area.x0, y0: area.y1 - GAUGE_H, x1: area.x1, y1: area.y1 }
        px.frame(gr, 1, subInk)
        const inn = inset(gr, 2)
        const fw = Math.trunc(((inn.x1 - inn.x0) * b.pct) / 100)
        if (fw > 0) px.fill({ ...inn, x1: inn.x0 + fw }, pressed ? subInk : c)
        area = { ...area, y1: area.y1 - (GAUGE_H + widgetLineGap) }
      }
      drawTwoLines(font, px, area, b.main, b.sub, c, subInk)
      return
    }
    case 'stopwatch': {
      const st = env.widgets.stopwatch?.[g.id]
      const el = stopwatchElapsed(st, now)
      const running = !!st?.started_at
      let c = ink
      let sub = '計測中'
      if (!running && el < 1000) [c, sub] = [subInk, STOPWATCH_IDLE]
      else if (!running) [c, sub] = [colInkWarn, '停止中']
      if (pressed) c = ink
      drawTwoLines(font, px, area, stopwatchText(el), sub, c, subInk)
      return
    }
    case 'pomodoro': {
      const pv = pomodoroView(env.widgets.pomodoro?.[g.id], now, g.work)
      const phase = pv.phase === 'break' ? '休憩' : '作業'
      let c: RGB
      let sub: string
      if (!pv.started) [c, sub] = [ink, STOPWATCH_IDLE]
      else if (pv.running) [c, sub] = [pomoInk(pv.phase), phase + '中']
      else [c, sub] = [subInk, '一時停止 ' + phase]
      if (pv.count > 0) sub += ` ${pv.count}回`
      if (pressed) c = ink
      if (pv.alert && pv.blinkOn && !pressed) {
        c = contrastInk(pomoInk(pv.phase))
        subInk = c
      }
      drawTwoLines(font, px, area, pomoText(pv.remaining), sub, c, subInk)
      return
    }
    case 'counter':
      drawTwoLines(font, px, area, String(env.widgets.counter?.[g.id]?.value ?? 0), '', ink, subInk)
      return
    case 'presence': {
      const { name } = presenceView(g.states, env.widgets, g.id)
      const s = Math.min(fitScale(font, [name], aw, ah, TEXT_MAX_SCALE), TEXT_MAX_SCALE)
      drawCentered(font, px, area, area.y0 + Math.trunc((ah - FONT_H * s) / 2), name, s, ink)
      return
    }
    case 'qr': {
      const e = Object.hasOwn(env.qr, g.id) ? env.qr[g.id] : undefined
      if (!e) {
        message(QR_NONE, subInk)
        return
      }
      const old = textExpired(e, env.now)
      if (!drawQR(px, area, e.text, old ? colQRDarkOld : colQRDark, old ? colQRLitOld : colQRLight)) message(QR_TOO_BIG, colInkWarn)
    }
  }
}

// drawQR は gadgetwidget.go の drawQR と同じ。入らなければ false。
function drawQR(px: Pixels, area: Rect, text: string, dark: RGB, light: RGB): boolean {
  const q = encodeQR(text)
  if (!q) return false
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  const { px: m, quiet } = qrFit(q.size, aw, ah)
  if (!m) return false
  const total = (q.size + 2 * quiet) * m
  const x0 = area.x0 + Math.trunc((aw - total) / 2)
  const y0 = area.y0 + Math.trunc((ah - total) / 2)
  px.fill({ x0, y0, x1: x0 + total, y1: y0 + total }, light)
  const ox = x0 + quiet * m
  const oy = y0 + quiet * m
  for (let y = 0; y < q.size; y++) {
    for (let x = 0; x < q.size; x++) if (q.dark(x, y)) px.fill({ x0: ox + x * m, y0: oy + y * m, x1: ox + (x + 1) * m, y1: oy + (y + 1) * m }, dark)
  }
  return true
}

function sameRGB(a: RGB, b: RGB): boolean {
  return a[0] === b[0] && a[1] === b[1] && a[2] === b[2]
}

const TEXT_MAX_SCALE = 6 // widget.go の textMaxScale

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
function drawCalendar(font: BitmapFont, px: Pixels, area: Rect, w: CalWidget, env: WidgetEnv, subInk: RGB, follow: number | null): void {
  const l = calLayout(area, w, env.calendar, env.now.getTime(), env.synced, follow)
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

// drawMonth は monthwidget.go の drawMonth と同じ。area は見出しの下の範囲。
function drawMonth(font: BitmapFont, px: Pixels, area: Rect, w: MonthWidget, env: WidgetEnv): void {
  const l = monthLayout(font, area, w, env.calendar, env.now.getTime(), env.synced, env.month)
  const r4 = (r: [number, number, number, number]): Rect => ({ x0: r[0], y0: r[1], x1: r[2], y1: r[3] })
  const titleH = FONT_H * l.title_scale
  px.text(font, area.x0 + 2, area.y0, l.title, l.title_scale, l.title_warn ? colCalWarn : colText, area)
  if (l.status) {
    const sx = area.x1 - font.textWidth(l.status) - 2
    px.text(font, Math.max(sx, area.x0), area.y0 + Math.trunc((titleH - FONT_H) / 2), l.status, 1, l.status_warn ? colCalWarn : colSub, area)
  }
  const aw = area.x1 - area.x0
  l.wdays.forEach((wd, i) => {
    const x0 = area.x0 + Math.trunc((i * aw) / 7)
    const x1 = area.x0 + Math.trunc(((i + 1) * aw) / 7)
    const c = wd === 0 ? colMonthSun : wd === 6 ? colMonthSat : colSub
    drawCentered(font, px, { x0, y0: area.y0, x1, y1: area.y1 }, l.wday_y, JA_WEEKDAYS[wd], 1, c)
  })
  for (const d of l.days) drawMonthDay(font, px, d, l.num_scale, l.dot)
  const nav = r4(l.nav)
  px.fill({ ...nav, y1: nav.y0 + 1 }, colTodoRule)
  const arrow = (zone: Rect, s: string, ok: boolean) => {
    const sc = Math.min(fitScale(font, [s], zone.x1 - zone.x0, zone.y1 - zone.y0 - 4), 3)
    drawCentered(font, px, zone, zone.y0 + Math.trunc((zone.y1 - zone.y0 - FONT_H * sc) / 2), s, sc, ok ? colText : colTodoOff)
  }
  arrow(r4(l.up), '▲', l.offset > -MONTH_MAX_OFFSET)
  arrow(r4(l.down), '▼', l.offset < MONTH_MAX_OFFSET)
  const mid = r4(l.mid)
  const mw = mid.x1 - mid.x0
  const is = font.textWidth(l.info) * 2 <= mw && FONT_H * 2 <= mid.y1 - mid.y0 - 4 ? 2 : 1
  const info = todoEllipsis(font, l.info, Math.trunc(mw / is))
  drawCentered(font, px, mid, mid.y0 + Math.trunc((mid.y1 - mid.y0 - FONT_H * is) / 2), info, is, colSub)
}

function monthDayInk(d: MonthDay): RGB {
  if (d.today) return d.red ? colMonthTodaySu : d.sat ? colMonthTodaySa : colMonthTodayIn
  if (!d.in) return d.red ? colMonthDimSun : d.sat ? colMonthDimSat : colMonthDim
  return d.red ? colMonthSun : d.sat ? colMonthSat : colText
}

// drawMonthDay は、日付 1 つを描く（monthwidget.go の drawMonthDay）。
function drawMonthDay(font: BitmapFont, px: Pixels, d: MonthDay, s: number, dot: number): void {
  const r: Rect = { x0: d.rect[0], y0: d.rect[1], x1: d.rect[2], y1: d.rect[3] }
  if (d.today) px.fill(inset(r, 1), colMonthToday)
  if (d.sel) px.frame(inset(r, 1), 2, colPressed)
  const num = String(Number(d.day.slice(8)))
  const y = r.y0 + Math.trunc((r.y1 - r.y0 - (FONT_H * s + 1 + dot)) / 2)
  drawCentered(font, px, r, y, num, s, monthDayInk(d))
  if (!d.dots?.length) return
  const gap = Math.max(Math.trunc(dot / 2), 1)
  const n = d.dots.length
  let x = r.x0 + Math.trunc((r.x1 - r.x0 - (n * dot + (n - 1) * gap)) / 2)
  const dy = y + FONT_H * s + 1
  for (const c of d.dots) {
    px.fill(intersect({ x0: x, y0: dy, x1: x + dot, y1: dy + dot }, r), c === 'more' ? colMonthMore : hexColor(c))
    x += dot + gap
  }
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

// drawWeather は weatherwidget.go の drawWeather と同じ。描くものの並びを順に描く。
function drawWeather(font: BitmapFont, px: Pixels, area: Rect, w: WeatherWidget, env: WidgetEnv): void {
  for (const it of wthLayout(font, area, w, env.weather, env.now.getTime(), env.synced)) {
    if (it.kind === 'text') px.text(font, it.x, it.y, it.text!, it.s!, hexRGB(it.color!), area)
    else if (it.kind === 'rule') px.fill({ x0: it.x, y0: it.y, x1: it.x + it.w!, y1: it.y + it.h! }, hexRGB(it.color!))
    else {
      const ic = env.weatherIcons(it.icon!)
      if (ic) px.blendIcon(ic, { x0: it.x, y0: it.y, x1: it.x + it.w!, y1: it.y + it.h! }, area)
    }
  }
}

function hexRGB(s: string): RGB {
  const v = parseInt(s.slice(1), 16)
  return [(v >> 16) & 255, (v >> 8) & 255, v & 255]
}

// ---------- 状態の帯（statusbar.go の drawBar） ----------

function barItemWidth(font: BitmapFont, it: BarItemView): number {
  return font.textWidth(it.text) * BAR_SCALE + (it.batt ? BAR_BATT_W + BAR_BATT_TO + 4 : 0)
}

function drawBar(font: BitmapFont, px: Pixels, W: number, title: string, mode: Mode, items: BarItem[], env: BarEnv): void {
  px.fill({ x0: 0, y0: 0, x1: W, y1: BAR_H }, colBar)
  px.fill({ x0: 0, y0: BAR_H - 1, x1: W, y1: BAR_H }, colBarRule)
  const its = barItems(items, env)
  let x = W - BAR_PAD
  for (let i = its.length - 1; i >= 0; i--) {
    const it = its[i]
    x -= barItemWidth(font, it)
    drawBarItem(font, px, x, it)
    x -= BAR_GAP
  }
  const room = x - 2 * BAR_PAD
  let t = title
  while (t !== '' && font.textWidth(t) * BAR_SCALE > room) t = [...t].slice(0, -1).join('')
  if (t !== '') {
    const b = { x0: 0, y0: 0, x1: font.textWidth(t) * BAR_SCALE + 2 * BAR_PAD, y1: BAR_H - 1 }
    px.fill(b, modeBadge[mode])
    px.text(font, BAR_PAD, BAR_TEXT_Y, t, BAR_SCALE, modeBadgeText[mode], b)
  }
}

function drawBarItem(font: BitmapFont, px: Pixels, x: number, it: BarItemView): void {
  const b = it.batt
  if (b) {
    const y = Math.trunc((BAR_H - 1 - BAR_BATT_H) / 2)
    const box = { x0: x, y0: y, x1: x + BAR_BATT_W, y1: y + BAR_BATT_H }
    px.frame(box, 2, it.ink)
    px.fill({ x0: box.x1, y0: y + 4, x1: box.x1 + BAR_BATT_TO, y1: box.y1 - 4 }, it.ink)
    const inn = inset(box, 3)
    let w = Math.trunc(((inn.x1 - inn.x0) * Math.min(Math.max(b.percent, 0), 100)) / 100)
    if (b.percent > 0) w = Math.max(w, 1)
    px.fill({ x0: inn.x0, y0: inn.y0, x1: inn.x0 + w, y1: inn.y1 }, it.ink)
    x += BAR_BATT_W + BAR_BATT_TO + 4
  }
  px.text(font, x, BAR_TEXT_Y, it.text, BAR_SCALE, it.ink, { x0: x, y0: 0, x1: x + font.textWidth(it.text) * BAR_SCALE, y1: BAR_H })
}

// ---------- ホーム画面（home.go の drawHome） ----------

const colHomeNav: RGB = [0x14, 0x1c, 0x28]

function drawIcon(px: Pixels, x: number, y: number, s: number, name: string, clip: Rect): void {
  const rows = ICON_ROWS[name] ?? ICON_ROWS.app
  rows.forEach((row, gy) => {
    for (let gx = 0; gx < row.length; gx++) {
      const c = ICON_PALETTE[row[gx]]
      if (c) px.fill(intersect({ x0: x + gx * s, y0: y + gy * s, x1: x + (gx + 1) * s, y1: y + (gy + 1) * s }, clip), c)
    }
  })
}

function drawTriangle(px: Pixels, r: Rect, dir: number, c: RGB): void {
  const half = HOME_ARROW / 2
  const cx = r.x0 + Math.trunc((r.x1 - r.x0) / 2) - Math.trunc(half / 2)
  const cy = r.y0 + Math.trunc((r.y1 - r.y0) / 2)
  for (let i = 0; i < half; i++) {
    const x = dir > 0 ? cx + half - 1 - i : cx + i
    px.fill({ x0: x, y0: cy - i - 1, x1: x + 1, y1: cy + i + 1 }, c)
  }
}

function drawHome(font: BitmapFont, px: Pixels, area: Rect, h: HomeConfig, portrait: boolean, page: number,
  image: (id?: string) => Image565 | undefined, hasWall: boolean): void {
  const g = homeGeometry(area, h, portrait, page)
  for (let i = g.first; i < g.last; i++) {
    const slot = inset(g.slots[i - g.first], HOME_SLOT_PAD)
    const a = h.apps[i]
    const [name, ns] = homeName(font, a.name, slot.x1 - slot.x0)
    const nameH = FONT_H * ns
    const isImg = !!a.icon && /^[0-9a-f]{16}$/.test(a.icon)
    const img = isImg ? image(a.icon) : undefined
    const box = { x0: slot.x0, y0: slot.y0, x1: slot.x1, y1: slot.y1 - nameH - HOME_NAME_GAP }
    let iw = HOME_IMAGE_ICON
    let ih = HOME_IMAGE_ICON
    let s = 0
    if (img) [iw, ih] = [img.w, img.h]
    else {
      s = homeIconFit(box)
      iw = ih = HOME_ICON_BASE * s
    }
    const total = ih + HOME_NAME_GAP + nameH
    const y = slot.y0 + Math.trunc((slot.y1 - slot.y0 - total) / 2)
    const x = slot.x0 + Math.trunc((slot.x1 - slot.x0 - iw) / 2)
    if (img) px.blit(img, { x0: x, y0: y, x1: x + iw, y1: y + ih }, slot)
    else drawIcon(px, x, y, s, isImg || !a.icon ? 'app' : a.icon, slot)
    px.halo = hasWall
    drawCentered(font, px, slot, y + ih + HOME_NAME_GAP, name, ns, colText)
    px.halo = false
  }
  if (!g.nav) return
  px.fill(g.nav, colHomeNav)
  px.fill({ x0: g.nav.x0, y0: g.nav.y0, x1: g.nav.x1, y1: g.nav.y0 + 1 }, colTodoRule)
  const ty = g.nav.y0 + Math.trunc((g.nav.y1 - g.nav.y0 - FONT_H * 2) / 2)
  drawTriangle(px, g.prev!, -1, g.page > 0 ? colText : colTodoOff)
  drawTriangle(px, g.next!, +1, g.page < g.pages - 1 ? colText : colTodoOff)
  drawCentered(font, px, g.mid!, ty, `${g.page + 1}/${g.pages}`, 2, colSub)
}
