// k8x12 フォント（font/k8x12.bin、font.go と同じ形式）。
// Brain の画面と同じ字形で描くので、フォントにない文字は実機と同じく □ になる。
//
// 形式："K812" + グリフ数（u16 LE）+ グリフ表（rune 3 バイト LE、送り幅 1 バイト、12 行）

export const FONT_H = 12
const STRIDE = 16

// 字形のない文字の代わりに描く枠（font.go の missingGlyph）
const MISSING = new Uint8Array([0, 0xfe, 0x82, 0x82, 0x82, 0x82, 0x82, 0x82, 0x82, 0xfe, 0, 0])

export class BitmapFont {
  private n: number
  private data: Uint8Array

  constructor(buf: ArrayBuffer) {
    const b = new Uint8Array(buf)
    if (b.length < 6 || String.fromCharCode(b[0], b[1], b[2], b[3]) !== 'K812') throw new Error('font is broken')
    this.n = b[4] | (b[5] << 8)
    this.data = b.subarray(6, 6 + this.n * STRIDE)
  }

  private runeAt(i: number): number {
    const o = i * STRIDE
    return this.data[o] | (this.data[o + 1] << 8) | (this.data[o + 2] << 16)
  }

  has(cp: number): boolean {
    return this.find(cp) >= 0
  }

  private find(cp: number): number {
    let lo = 0
    let hi = this.n
    while (lo < hi) {
      const m = (lo + hi) >> 1
      if (this.runeAt(m) < cp) lo = m + 1
      else hi = m
    }
    return lo < this.n && this.runeAt(lo) === cp ? lo : -1
  }

  glyph(cp: number): { width: number; rows: Uint8Array } {
    const i = this.find(cp)
    if (i >= 0) {
      const o = i * STRIDE
      return { width: this.data[o + 3], rows: this.data.subarray(o + 4, o + 4 + FONT_H) }
    }
    return { width: cp < 0x80 ? 4 : 8, rows: MISSING }
  }

  textWidth(s: string): number {
    let w = 0
    for (const ch of s) w += this.glyph(ch.codePointAt(0)!).width
    return w
  }

  // missing は、フォントにない文字を返す（ラベルの入力欄で知らせる）。
  missing(s: string): string[] {
    const out = new Set<string>()
    for (const ch of s) if (ch !== '\n' && !this.has(ch.codePointAt(0)!)) out.add(ch)
    return [...out]
  }
}

let loading: Promise<BitmapFont> | null = null

export function loadFont(): Promise<BitmapFont> {
  loading ??= fetch(new URL('../../font/k8x12.bin', import.meta.url))
    .then((r) => {
      if (!r.ok) throw new Error(`font: HTTP ${r.status}`)
      return r.arrayBuffer()
    })
    .then((b) => new BitmapFont(b))
  return loading
}
