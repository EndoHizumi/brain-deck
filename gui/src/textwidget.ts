// テキストのタイル（widget: text）。widget.go の textLayout、wrapText、textColors と同じ結果にする。
// 中身は Brain の /var/lib/lefthand/text.json にあり、PC から set_text（brain-deck text ...）で書き換える。

import { FONT_H, type BitmapFont } from './font'
import type { TextEntry, TextStyle } from './types'

type RGB = [number, number, number]

export const TEXT_MAX_SCALE = 6
export const TEXT_NONE = '未設定' // まだ一度も set_text していない
const ELLIPSIS = '…'

export const TEXT_STYLES: Record<TextStyle, string> = { normal: '通常', ok: '成功', error: '失敗', warn: '警告' }

// 種類ごとに、ふつうのときと、有効期限が切れたとき（薄く）の色
const TEXT_COLORS: Record<TextStyle, [RGB, RGB]> = {
  normal: [[0xff, 0xff, 0xff], [0x6a, 0x74, 0x80]],
  ok: [[0x50, 0xd8, 0x80], [0x2e, 0x5a, 0x44]],
  error: [[0xff, 0x58, 0x58], [0x6a, 0x34, 0x3a]],
  warn: [[0xff, 0xc0, 0x30], [0x6a, 0x58, 0x2c]],
}

// textId は widget.go の textIDPattern と同じ。
export const TEXT_ID_PATTERN = /^[A-Za-z0-9_.-]{1,32}$/

export function textInk(style: string, expired: boolean): RGB {
  const c = TEXT_COLORS[style as TextStyle] ?? TEXT_COLORS.normal
  return expired ? c[1] : c[0]
}

export function textExpired(e: TextEntry, now: Date): boolean {
  return !!e.expires_at && now.getTime() >= Date.parse(e.expires_at)
}

// textLayout は、テキストを w×h に収まるように折り返し、行と倍率を返す。
export function textLayout(font: BitmapFont, s: string, w: number, h: number): { lines: string[]; scale: number } {
  const paras = s.split('\n')
  for (let sc = TEXT_MAX_SCALE; sc >= 1; sc--) {
    const lines = wrapText(font, paras, Math.trunc(w / sc))
    if (lines.length * FONT_H * sc <= h) return { lines, scale: sc }
  }
  const lines = wrapText(font, paras, w)
  const n = Math.max(Math.trunc(h / FONT_H), 1)
  if (lines.length <= n) return { lines, scale: 1 }
  const out = lines.slice(0, n)
  let last = [...out[n - 1]]
  while (last.length > 0 && font.textWidth(last.join('') + ELLIPSIS) > w) last = last.slice(0, -1)
  out[n - 1] = last.join('') + ELLIPSIS
  return { lines: out, scale: 1 }
}

// wrapText は、段落を幅 w（等倍のドット）で折り返す。空白があれば最後の空白で、なければ文字の境目で折り返す。
export function wrapText(font: BitmapFont, paras: string[], w: number): string[] {
  const out: string[] = []
  for (const p of paras) {
    let line: string[] = []
    let lw = 0
    for (const ch of p) {
      const gw = font.glyph(ch.codePointAt(0)!).width
      if (lw + gw > w && line.length > 0) {
        const sp = lastSpace(line)
        if (sp > 0) {
          out.push(line.slice(0, sp).join(''))
          line = line.slice(sp + 1)
        } else {
          out.push(line.join(''))
          line = []
        }
        lw = font.textWidth(line.join(''))
        if (ch === ' ' && line.length === 0) continue // 行の頭の空白は描かない
      }
      line.push(ch)
      lw += gw
    }
    out.push(line.join(''))
  }
  return out
}

function lastSpace(line: string[]): number {
  for (let i = line.length - 1; i > 0; i--) if (line[i] === ' ') return i
  return -1
}
