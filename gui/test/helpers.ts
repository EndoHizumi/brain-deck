import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { inflateSync } from 'node:zlib'
import { BitmapFont } from '../src/font'
import { parseConfigText } from '../src/yamlio'

// リポジトリのいちばん上（gui/ の親）
const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..')

export function repoFile(path: string): Buffer {
  return readFileSync(resolve(root, path))
}

export function testFont(): BitmapFont {
  const b = repoFile('font/k8x12.bin')
  return new BitmapFont(new Uint8Array(b).buffer)
}

export function sampleConfig() {
  return parseConfigText(repoFile('config.yaml').toString('utf8'))
}

// decodePNG は 8 ビットの RGB / RGBA の PNG（Go の image/png が書くもの）を RGBA にする。
export function decodePNG(buf: Buffer): { w: number; h: number; data: Uint8Array } {
  let o = 8
  let w = 0
  let h = 0
  let type = 0
  const idat: Buffer[] = []
  while (o < buf.length) {
    const len = buf.readUInt32BE(o)
    const t = buf.toString('ascii', o + 4, o + 8)
    const d = buf.subarray(o + 8, o + 8 + len)
    if (t === 'IHDR') {
      w = d.readUInt32BE(0)
      h = d.readUInt32BE(4)
      if (d[8] !== 8 || d[12] !== 0) throw new Error('unsupported PNG')
      type = d[9]
    } else if (t === 'IDAT') idat.push(d)
    o += 12 + len
  }
  const bpp = type === 6 ? 4 : type === 2 ? 3 : 0
  if (!bpp) throw new Error(`unsupported PNG color type ${type}`)
  const raw = inflateSync(Buffer.concat(idat))
  const stride = w * bpp
  const cur = new Uint8Array(stride)
  let prev = new Uint8Array(stride)
  const out = new Uint8Array(w * h * 4)
  for (let y = 0; y < h; y++) {
    const f = raw[y * (stride + 1)]
    const line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1))
    for (let i = 0; i < stride; i++) {
      const a = i >= bpp ? cur[i - bpp] : 0
      const b = prev[i]
      const c = i >= bpp ? prev[i - bpp] : 0
      let v = line[i]
      if (f === 1) v += a
      else if (f === 2) v += b
      else if (f === 3) v += (a + b) >> 1
      else if (f === 4) {
        const p = a + b - c
        const pa = Math.abs(p - a)
        const pb = Math.abs(p - b)
        const pc = Math.abs(p - c)
        v += pa <= pb && pa <= pc ? a : pb <= pc ? b : c
      }
      cur[i] = v & 0xff
    }
    for (let x = 0; x < w; x++) {
      out.set([cur[x * bpp], cur[x * bpp + 1], cur[x * bpp + 2], 255], (y * w + x) * 4)
    }
    prev = cur.slice()
  }
  return { w, h, data: out }
}
