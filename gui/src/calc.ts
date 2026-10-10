// 電卓（widget: calculator）の表示。calc.go と calcwidget.go と同じ結果にする（プレビューを -render-png と画素単位で比べる）。
// 計算は Brain が行う（有理数）。GUI は Brain の状態（get_widgets の calculator）を描くだけなので、
// 数の書き方（formatRat）と、式の表示と、配置だけを持つ。有理数は "分子/分母" の文字で受け取り、BigInt で扱う。

import { FONT_H, type BitmapFont } from './font'
import type { Rect } from './todowidget'

export const DEFAULT_CALC_ID = 'calculator'
export const CALC_COLS = 6
export const CALC_ROWS = 5
export const CALC_MIN_SPAN = 2
export const CALC_PAD = 6
const DIGITS = '0123456789ABCDEF'

export interface CalcTok {
  t: 'num' | 'op' | 'lp' | 'rp' | 'val'
  s?: string
  v?: string
}

export interface CalcEntry {
  expr: string
  value: string
  base: number
}

export interface CalcState {
  base: number
  tokens: CalcTok[] | null
  result?: string
  done?: string
  msg?: string
  msg_err?: boolean
  history?: CalcEntry[] | null
  rev?: number
}

export const CALC_BUTTONS: string[][] = [
  ['base', 'hist', '(', ')', 'bs', 'ac'],
  ['A', 'B', '7', '8', '9', '/'],
  ['C', 'D', '4', '5', '6', '*'],
  ['E', 'F', '1', '2', '3', '-'],
  ['type', 'neg', '0', '.', '=', '+'],
]
const LABELS: Record<string, string> = { hist: '履歴', bs: 'BS', ac: 'AC', '/': '÷', '*': '×', '-': '−', type: '入力', neg: '±' }
const OP_TEXT: Record<string, string> = { '+': '+', '-': '−', '*': '×', '/': '÷' }

export function baseName(b: number): string {
  return b === 16 ? 'HEX' : b === 2 ? 'BIN' : 'DEC'
}

export function validDigit(d: string, base: number): boolean {
  const i = DIGITS.indexOf(d)
  return d.length === 1 && i >= 0 && i < base
}

export function buttonLabel(b: string, base: number): string {
  return b === 'base' ? baseName(base) : (LABELS[b] ?? b)
}

export type RGB = [number, number, number]
export const colCalcDisp: RGB = [0x0c, 0x10, 0x16]
const colCalcKey: RGB = [0x26, 0x32, 0x44]
const colCalcOp: RGB = [0x22, 0x3c, 0x5e]
const colCalcEq: RGB = [0x2e, 0x9e, 0x5b]
const colCalcType: RGB = [0x7a, 0x56, 0x18]
const colCalcClear: RGB = [0x5e, 0x2a, 0x32]
const colCalcFn: RGB = [0x34, 0x34, 0x48]
export const colCalcBase: RGB = [0x4f, 0x9d, 0xff]

export function buttonColor(b: string): RGB {
  if (['+', '-', '*', '/', '(', ')'].includes(b)) return colCalcOp
  if (b === '=') return colCalcEq
  if (b === 'type') return colCalcType
  if (b === 'bs' || b === 'ac') return colCalcClear
  if (b === 'base' || b === 'hist' || b === 'neg') return colCalcFn
  return colCalcKey
}

// Rat は、約分した有理数（分母は正）。
export interface Rat {
  num: bigint
  den: bigint
}

export function parseRat(s: string | undefined): Rat | null {
  const m = /^(-?\d+)(?:\/(\d+))?$/.exec(s ?? '')
  if (!m) return null
  const den = BigInt(m[2] ?? '1')
  return den === 0n ? null : { num: BigInt(m[1]), den }
}

// formatRat は calc.go の formatRat と同じ。
export function formatRat(r: Rat, base: number, maxChars: number, maxFrac: number): [string, boolean] {
  const neg = r.num < 0n
  const num = neg ? -r.num : r.num
  const den = r.den
  let ip = num / den
  let rem = num % den
  let is = ip.toString(base).toUpperCase()
  const f = Math.min(maxChars - (neg ? 1 : 0) - is.length - 1, maxFrac)
  const b = BigInt(base)
  const digits: string[] = []
  for (let i = 0; i < f && rem !== 0n; i++) {
    rem *= b
    digits.push(DIGITS[Number(rem / den)])
    rem %= den
  }
  const exact = rem === 0n
  if (!exact) {
    if (rem * 2n >= den) {
      let i = digits.length - 1
      for (; i >= 0; i--) {
        const k = DIGITS.indexOf(digits[i]) + 1
        if (k < base) {
          digits[i] = DIGITS[k]
          break
        }
        digits[i] = '0'
      }
      if (i < 0) {
        ip += 1n
        is = ip.toString(base).toUpperCase()
      }
    }
    while (digits.length && digits[digits.length - 1] === '0') digits.pop()
  }
  let s = is
  if (digits.length) s += '.' + digits.join('')
  if (neg && s !== '0') s = '-' + s
  return [s, exact]
}

function valText(v: string | undefined, base: number): string {
  const r = parseRat(v)
  if (!r) return '?'
  const [s, exact] = formatRat(r, base, 24, 10)
  return exact ? s : s + '…'
}

// exprText は calc.go の calcExprText と同じ。
export function exprText(toks: CalcTok[] | null | undefined, base: number): string {
  let out = ''
  for (const t of toks ?? []) {
    if (t.t === 'num') out += (t.s?.startsWith('.') ? '0' : '') + (t.s ?? '')
    else if (t.t === 'op') out += OP_TEXT[t.s ?? ''] ?? ''
    else if (t.t === 'lp') out += '('
    else if (t.t === 'rp') out += ')'
    else out += valText(t.v, base)
  }
  return out
}

export interface CalcGeom {
  disp: Rect
  buttons: Rect[]
}

// calcGeom は calcwidget.go の newCalcGeom と同じ。
export function calcGeom(area: Rect): CalcGeom {
  const bh = Math.trunc((area.y1 - area.y0) / 7)
  const top = area.y1 - bh * CALC_ROWS
  const w = area.x1 - area.x0
  const buttons: Rect[] = []
  for (let r = 0; r < CALC_ROWS; r++) {
    for (let c = 0; c < CALC_COLS; c++) {
      const x0 = area.x0 + Math.trunc((w * c) / CALC_COLS)
      const x1 = area.x0 + Math.trunc((w * (c + 1)) / CALC_COLS)
      buttons.push({ x0: x0 + 2, y0: top + bh * r + 2, x1: x1 - 2, y1: top + bh * (r + 1) - 2 })
    }
  }
  return { disp: { x0: area.x0, y0: area.y0, x1: area.x1, y1: top - 2 }, buttons }
}

export interface CalcDisp {
  small: string
  smallErr: boolean | null // null なら知らせではない（灰色）
  big: string
  result: Rat | null
  dim: boolean
}

// calcDisplay は calcwidget.go の calcDisplay と同じ。
export function calcDisplay(s: CalcState): CalcDisp {
  const d: CalcDisp = { small: '', smallErr: null, big: '', result: null, dim: false }
  if (s.msg) {
    d.small = s.msg
    d.smallErr = !!s.msg_err
  } else if (s.done) d.small = s.done
  else if (s.history?.length) {
    const h = s.history[0]
    const r = parseRat(h.value)
    if (r) {
      const [v, exact] = formatRat(r, s.base, 24, 10)
      d.small = `${h.expr} = ${v}${exact ? '' : '…'}`
    }
  }
  if (s.result) d.result = parseRat(s.result)
  else {
    d.big = exprText(s.tokens, s.base)
    if (!d.big) {
      d.big = '0'
      d.dim = true
    }
  }
  return d
}

// calcTail は calcwidget.go と同じ。入らなければ、先頭を … にして後ろを残す。
export function calcTail(font: BitmapFont, s: string, w: number): string {
  if (font.textWidth(s) <= w) return s
  let r = [...s]
  while (r.length && font.textWidth('…' + r.join('')) > w) r = r.slice(1)
  return '…' + r.join('')
}

// calcBigText は calcwidget.go と同じ。
export function calcBigText(font: BitmapFont, d: CalcDisp, base: number, w: number, h: number): [string, number] {
  const zero = font.textWidth('0')
  for (let sc = 4; sc >= 1; sc--) {
    if (FONT_H * sc > h && sc > 1) continue
    if (!d.result) {
      if (font.textWidth(d.big) * sc <= w) return [d.big, sc]
      continue
    }
    const n = Math.trunc(w / (zero * sc))
    let [t, exact] = formatRat(d.result, base, n, 20)
    if (!exact) t = '約' + formatRat(d.result, base, n - 2, 20)[0]
    if (font.textWidth(t) * sc <= w) return [t, sc]
  }
  if (!d.result) return [calcTail(font, d.big, w), 1]
  let [t, exact] = formatRat(d.result, base, Math.trunc(w / zero), 20)
  if (!exact) t = '約' + t
  return [calcTail(font, t, w), 1]
}
