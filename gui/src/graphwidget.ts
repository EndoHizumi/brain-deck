// グラフのウィジェット（widget: graph）。graphwidget.go の graphLayout と同じ配置を作る（プレビューを -render-png と画素単位で比べる）。
// 値は Brain の /var/lib/lefthand/graph.json にあり、PC の brain-deck graph <id> <値> が足す（get_graph で読む）。
// 時刻は PC のタイムゾーンで書く（calwidget.ts と同じ前提）。

import { FONT_H, type BitmapFont } from './font'
import { parseDuration } from './calwidget'
import type { Rect } from './todowidget'
import type { ActionSpec, GraphPoint, GraphSeries } from './types'

export const GRAPH_MAX_POINTS = 120
export const DEFAULT_GRAPH_POINTS = 60
export const DEFAULT_GRAPH_STALE_MS = 5 * 60_000
export const GRAPH_LINE_W = 2
const GRAPH_VALUE_MAX = 6
const GRAPH_GAP = 6
const GRAPH_MIN_PLOT_H = 16
const WIDGET_LINE_GAP = 4
export const GRAPH_NONE = '未受信'

// GraphWidget は、グラフのセルの設定（graphwidget.go の GraphDef と WidgetDef.Stale）。
export interface GraphWidget {
  id: string
  unit: string
  min?: number
  max?: number
  chart: 'line' | 'bar'
  points: number
  decimals: number // -1 なら値の大きさで決める
  stale: number // ミリ秒
}

export function graphWidgetOf(a: ActionSpec): GraphWidget {
  const st = a.stale ? parseDuration(a.stale) : null
  return {
    id: a.id ?? '',
    unit: a.unit ?? '',
    min: typeof a.min === 'number' ? a.min : undefined,
    max: typeof a.max === 'number' ? a.max : undefined,
    chart: a.chart === 'bar' ? 'bar' : 'line',
    points: Number.isInteger(a.points) && a.points! >= 2 && a.points! <= GRAPH_MAX_POINTS ? a.points! : DEFAULT_GRAPH_POINTS,
    decimals: Number.isInteger(a.decimals) && a.decimals! >= 0 && a.decimals! <= 6 ? a.decimals! : -1,
    stale: st ?? DEFAULT_GRAPH_STALE_MS,
  }
}

// graphDecimals は graphwidget.go と同じ。
export function graphDecimals(v: number, fixed: number): [number, boolean] {
  if (fixed >= 0) return [fixed, false]
  const a = Math.abs(v)
  if (a >= 100) return [0, true]
  if (a >= 10) return [1, true]
  return [2, true]
}

// formatGraph は graphwidget.go と同じ。10^d 倍して 0.5 を足し、切り捨てた整数から書く（toFixed は丸め方が Go と違う）。
export function formatGraph(v: number, d: number, trim: boolean): string {
  const p = 10 ** d
  const n = BigInt(Math.floor(Math.abs(v) * p + 0.5))
  const pi = BigInt(p)
  let s = (n / pi).toString()
  if (d > 0) {
    s += '.' + (n % pi).toString().padStart(d, '0')
    if (trim) s = s.replace(/0+$/, '').replace(/\.$/, '')
  }
  if (v < 0 && n !== 0n) s = '-' + s
  return s
}

// graphRange は graphwidget.go と同じ。
export function graphRange(g: GraphWidget, pts: GraphPoint[]): [number, number] {
  let dmin = pts[0].v
  let dmax = pts[0].v
  for (const p of pts.slice(1)) {
    dmin = Math.min(dmin, p.v)
    dmax = Math.max(dmax, p.v)
  }
  const autoLo = g.min === undefined
  const autoHi = g.max === undefined
  let lo = dmin
  let hi = dmax
  if (g.chart === 'bar' && autoLo) lo = Math.min(lo, 0)
  if (g.chart === 'bar' && autoHi) hi = Math.max(hi, 0)
  if (!autoLo) lo = g.min!
  if (!autoHi) hi = g.max!
  if (hi <= lo) {
    if (autoLo && autoHi) [lo, hi] = [lo - 1, hi + 1]
    else if (autoHi) hi = lo + 1
    else lo = hi - 1
  }
  return [lo, hi]
}

export interface GraphLay {
  none: boolean
  value: string
  valueBox: Rect
  valueS: number
  sub: string
  subS: number
  stale: boolean
  plot: Rect | null
  hiLabel: string
  loLabel: string
  labelX: number
  xs: number[]
  ys: number[]
  barR: number[]
  barBase: number
}

const pad2 = (n: number) => String(n).padStart(2, '0')

// graphStaleText は graphwidget.go と同じ（「古い 15:04」、日付が違えば「古い 10/9 15:04」）。
export function graphStaleText(t: Date, now: Date): string {
  const hm = `${pad2(t.getHours())}:${pad2(t.getMinutes())}`
  if (t.getFullYear() === now.getFullYear() && t.getMonth() === now.getMonth() && t.getDate() === now.getDate()) return '古い ' + hm
  return `古い ${t.getMonth() + 1}/${t.getDate()} ${hm}`
}

function fitScale(font: BitmapFont, s: string, w: number, h: number, most: number): number {
  const tw = font.textWidth(s)
  let sc = most
  while (sc > 1 && (tw * sc > w || FONT_H * sc > h)) sc--
  return sc
}

// graphPoints は、描く値（新しい points 個）。
export function graphPoints(g: GraphWidget, s: GraphSeries | undefined): GraphPoint[] {
  const pts = s?.points ?? []
  return pts.length > g.points ? pts.slice(pts.length - g.points) : pts
}

// graphLayout は graphwidget.go の graphLayout と同じ。
export function graphLayout(font: BitmapFont, area: Rect, g: GraphWidget, s: GraphSeries | undefined, now: Date): GraphLay {
  const l: GraphLay = { none: false, value: '', valueBox: area, valueS: 1, sub: '', subS: 0, stale: false, plot: null, hiLabel: '', loLabel: '',
    labelX: 0, xs: [], ys: [], barR: [], barBase: 0 }
  const pts = graphPoints(g, s)
  if (!pts.length) return { ...l, none: true }
  const last = pts[pts.length - 1]
  const [d, trim] = graphDecimals(last.v, g.decimals)
  const lastT = new Date(last.t)
  l.value = formatGraph(last.v, d, trim) + g.unit
  l.stale = g.stale > 0 && now.getTime() - lastT.getTime() >= g.stale
  if (l.stale) l.sub = graphStaleText(lastT, now)
  const aw = area.x1 - area.x0
  const ah = area.y1 - area.y0
  const wide = aw >= ah * 2
  let plot: Rect
  if (wide) {
    l.valueBox = { ...area, x1: area.x0 + Math.trunc((aw * 2) / 5) }
    plot = { ...area, x0: l.valueBox.x1 + GRAPH_GAP }
  } else {
    l.valueBox = { ...area, y1: area.y0 + Math.trunc(ah / 2) }
    plot = { ...area }
  }
  let subH = 0
  if (l.sub) {
    l.subS = Math.min(fitScale(font, l.sub, l.valueBox.x1 - l.valueBox.x0, FONT_H * 2, 2), 2)
    subH = FONT_H * l.subS + WIDGET_LINE_GAP
  }
  l.valueS = fitScale(font, l.value, l.valueBox.x1 - l.valueBox.x0, l.valueBox.y1 - l.valueBox.y0 - subH, GRAPH_VALUE_MAX)
  if (!wide) {
    l.valueBox = { ...l.valueBox, y1: l.valueBox.y0 + FONT_H * l.valueS + subH }
    plot = { ...plot, y0: l.valueBox.y1 + GRAPH_GAP }
  }
  if (plot.y1 - plot.y0 < GRAPH_MIN_PLOT_H || plot.x1 - plot.x0 < GRAPH_MIN_PLOT_H) {
    l.valueBox = area
    l.valueS = fitScale(font, l.value, aw, ah - subH, GRAPH_VALUE_MAX)
    return l
  }
  const [lo, hi] = graphRange(g, pts)
  if (plot.y1 - plot.y0 >= FONT_H * 3) {
    const [hd, ht] = graphDecimals(Math.max(Math.abs(hi), Math.abs(lo)), g.decimals)
    l.hiLabel = formatGraph(hi, hd, ht)
    l.loLabel = formatGraph(lo, hd, ht)
    const gw = Math.max(font.textWidth(l.hiLabel), font.textWidth(l.loLabel)) + 4
    if (plot.x1 - plot.x0 - gw >= GRAPH_MIN_PLOT_H * 2) {
      l.labelX = plot.x0
      plot = { ...plot, x0: plot.x0 + gw }
    } else {
      l.hiLabel = ''
      l.loLabel = ''
    }
  }
  l.plot = plot
  const P = g.points
  const n = pts.length
  const pw = plot.x1 - plot.x0
  const ph = plot.y1 - plot.y0
  const clamp = (v: number) => Math.min(Math.max(v, lo), hi)
  if (g.chart === 'bar') {
    const yb = (v: number) => plot.y0 + Math.floor(((hi - clamp(v)) / (hi - lo)) * ph + 0.5)
    l.barBase = yb(0)
    pts.forEach((p, i) => {
      const k = P - n + i
      const x0 = plot.x0 + Math.trunc((pw * k) / P)
      let x1 = plot.x0 + Math.trunc((pw * (k + 1)) / P)
      if (x1 - x0 >= 3) x1--
      l.xs.push(x0)
      l.barR.push(x1)
      l.ys.push(yb(p.v))
    })
    return l
  }
  const yl = (v: number) => plot.y0 + Math.floor(((hi - clamp(v)) / (hi - lo)) * (ph - GRAPH_LINE_W) + 0.5)
  pts.forEach((p, i) => {
    const k = P - n + i
    l.xs.push(plot.x0 + Math.trunc(((pw - GRAPH_LINE_W) * k) / (P - 1)))
    l.ys.push(yl(p.v))
  })
  return l
}

// graphSegment は graphwidget.go と同じ（ブレゼンハム。始点は渡さない）。
export function graphSegment(x0: number, y0: number, x1: number, y1: number, f: (x: number, y: number) => void): void {
  let dx = x1 - x0
  let dy = y1 - y0
  let sx = 1
  let sy = 1
  if (dx < 0) [dx, sx] = [-dx, -1]
  if (dy < 0) [dy, sy] = [-dy, -1]
  let err = dx - dy
  while (x0 !== x1 || y0 !== y1) {
    const e2 = 2 * err
    if (e2 > -dy) {
      err -= dy
      x0 += sx
    }
    if (e2 < dx) {
      err += dx
      y0 += sy
    }
    f(x0, y0)
  }
}
