import { describe, expect, it } from 'vitest'
import { readdirSync } from 'node:fs'
import { resolve } from 'node:path'
import {
  adjust, checkTarget, convert, decodeImageFile, encodeImageFile, fitCrop, fromBase64, resample, sha256Hex, to565, toBase64, unpack565,
  type SourceImage,
} from '../src/image'
import { repoFile, repoRoot } from './helpers'

function solid(w: number, h: number, c: [number, number, number, number]): SourceImage {
  const data = new Uint8ClampedArray(w * h * 4)
  for (let i = 0; i < w * h; i++) data.set(c, i * 4)
  return { w, h, data }
}

describe('背景画像の変換', () => {
  it('切り抜きは縦横比を保ち、元の画像からはみ出さない', () => {
    // 横長の元の画像から、縦長のセルの範囲を取る
    const c = fitCrop(1000, 500, 192, 232, 1, 500, 250)
    expect(c.h).toBeCloseTo(500)
    expect(c.w / c.h).toBeCloseTo(192 / 232)
    expect(c.x).toBeCloseTo(500 - c.w / 2)
    // 拡大すると狭くなり、端に寄せても中に収まる
    const z = fitCrop(1000, 500, 192, 232, 4, 0, 0)
    expect(z.w).toBeCloseTo(c.w / 4)
    expect([z.x, z.y]).toEqual([0, 0])
    const e = fitCrop(1000, 500, 192, 232, 2, 5000, 5000)
    expect(e.x + e.w).toBeCloseTo(1000)
    expect(e.y + e.h).toBeCloseTo(500)
    // 拡大率は 1〜8 に収める
    expect(fitCrop(100, 100, 10, 10, 0.1, 50, 50).w).toBeCloseTo(100)
    expect(fitCrop(100, 100, 10, 10, 99, 50, 50).w).toBeCloseTo(100 / 8)
  })

  it('縮小は面積の平均、拡大はバイリニア。透明な部分は黒の上に重ねる', () => {
    // 左半分が白、右半分が黒の 4×1 を 2×1 と 1×1 に
    const src: SourceImage = { w: 4, h: 1, data: new Uint8ClampedArray([255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 255, 0, 0, 0, 255]) }
    expect(Array.from(resample(src, { x: 0, y: 0, w: 4, h: 1 }, 2, 1))).toEqual([255, 255, 255, 0, 0, 0])
    expect(Array.from(resample(src, { x: 0, y: 0, w: 4, h: 1 }, 1, 1))).toEqual([127.5, 127.5, 127.5])
    // 半端な位置（1.5 画素ぶん）も、覆う割合で平均する
    const r = resample(src, { x: 1, y: 0, w: 2, h: 1 }, 1, 1)
    expect(r[0]).toBeCloseTo(127.5)
    // 2×1 を 4×1 に拡大すると、間をつなぐ
    const up = resample({ w: 2, h: 1, data: new Uint8ClampedArray([0, 0, 0, 255, 200, 200, 200, 255]) }, { x: 0, y: 0, w: 2, h: 1 }, 4, 1)
    expect(Array.from(up.filter((_, i) => i % 3 === 0))).toEqual([0, 50, 150, 200])
    // 透明は黒になる。半透明は半分の明るさ
    expect(resample(solid(2, 2, [200, 100, 50, 0]), { x: 0, y: 0, w: 2, h: 2 }, 1, 1)[0]).toBe(0)
    expect(resample(solid(2, 2, [200, 100, 50, 51]), { x: 0, y: 0, w: 2, h: 2 }, 1, 1)[0]).toBeCloseTo(40)
  })

  it('明るさは、負なら黒に、正なら白に近づける', () => {
    const v = new Float32Array([0, 100, 255])
    expect(Array.from(adjust(v, -0.5))).toEqual([0, 50, 127.5])
    expect(Array.from(adjust(v, 0.5))).toEqual([127.5, 177.5, 255])
    expect(Array.from(adjust(v, 0))).toEqual([0, 100, 255])
  })

  it('RGB565 への変換：なしは丸めるだけ、Floyd–Steinberg は平均の色を保つ', () => {
    expect(to565(new Float32Array([255, 255, 255, 0, 0, 0]), 2, 1, 'none')).toEqual(new Uint16Array([0xffff, 0]))
    expect(unpack565(0xffff)).toEqual([255, 255, 255])
    expect(unpack565(0b00001_000001_00001)).toEqual([8, 4, 8])
    // 5 ビットの段の間の明るさ（8.2 と 16.4 の間）の灰色を、64×64 に敷き詰める
    const w = 64
    const h = 64
    const rgb = new Float32Array(w * h * 3).fill(12)
    const mean = (pix: Uint16Array) => pix.reduce((a, v) => a + unpack565(v)[0], 0) / pix.length
    const none = to565(rgb, w, h, 'none')
    const fs = to565(rgb, w, h, 'fs')
    expect(new Set(none).size).toBe(1) // どの画素も同じ色（段がつく）
    expect(new Set(fs).size).toBeGreaterThan(1) // 2 つの色を混ぜる
    expect(Math.abs(mean(fs) - 12)).toBeLessThan(0.3)
    expect(Math.abs(mean(none) - 12)).toBeGreaterThan(3)
  })

  it('ファイルの形と id は Brain（image.go、tools/mkbg）と同じ', async () => {
    const dir = resolve(repoRoot, 'config/background-images')
    const files = readdirSync(dir).filter((f) => f.endsWith('.565'))
    expect(files.length).toBeGreaterThan(0)
    for (const f of files) {
      const b = new Uint8Array(repoFile(`config/background-images/${f}`))
      const img = decodeImageFile(b)!
      expect(img).not.toBeNull()
      const again = encodeImageFile(img)
      expect(Buffer.from(again).equals(Buffer.from(b))).toBe(true)
      expect((await sha256Hex(again)).slice(0, 16)).toBe(f.slice(0, -4))
    }
    expect(decodeImageFile(new Uint8Array([1, 2, 3]))).toBeNull()
    expect(decodeImageFile(encodeImageFile({ w: 2, h: 2, pix: new Uint16Array(4) }).slice(0, 10))).toBeNull()
  })

  it('convert は、切り抜き、縮小、明るさ、ディザリングをして Brain のファイルにする', () => {
    const src = solid(400, 300, [255, 255, 255, 255])
    const r = convert(src, 192, 152, { cx: 200, cy: 150, zoom: 1, brightness: -0.5, dither: 'none' })
    expect([r.img.w, r.img.h, r.file.length]).toEqual([192, 152, 8 + 192 * 152 * 2])
    expect(unpack565(r.img.pix[0])).toEqual([131, 129, 131]) // 127.5 を 5、6 ビットに丸めた色
    expect(r.crop.w / r.crop.h).toBeCloseTo(192 / 152)
  })

  it('大きさの上限と base64', () => {
    expect(checkTarget(800, 480)).toBeNull()
    expect(checkTarget(480, 800)).toBeNull()
    expect(checkTarget(801, 10)).toContain('置けません')
    expect(checkTarget(800, 481)).toContain('置けません')
    const b = new Uint8Array(100000).map((_, i) => i * 7)
    expect(fromBase64(toBase64(b))).toEqual(b)
  })
})
