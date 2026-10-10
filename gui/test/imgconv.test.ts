// 画像の変換が、brain-deck（Go の internal/imgconv）と画素単位で同じになることを確かめる。
// 元の画像は式で描いた模様（internal/imgconv/imgconv_test.go の Pattern と同じ式）。
// 結果の SHA-256 を、Go のテストが書いた testdata/imgconv.json と比べる。
import { describe, expect, it } from 'vitest'
import { createHash } from 'node:crypto'
import { convert, encodeImageFile, fit565, type ConvertParams, type SourceImage } from '../src/image'
import { repoFile } from './helpers'

function pattern(w: number, h: number, alpha: boolean): SourceImage {
  const data = new Uint8ClampedArray(w * h * 4)
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const o = (y * w + x) * 4
      data[o] = (x * 5 + y * 3) % 256
      data[o + 1] = (x * y) % 256
      data[o + 2] = (x ^ y) & 255
      data[o + 3] = alpha ? (x + y) % 256 : 255
    }
  }
  return { w, h, data }
}

const p = (cx: number, cy: number, zoom: number, brightness: number, fs: boolean): ConvertParams => ({ cx, cy, zoom, brightness, dither: fs ? 'fs' : 'none' })

// Go の Cases と同じ（ZoomMilli は zoom × 1000、Brightness は brightness × 100）
const cases: [string, number, number, boolean, number, number, ConvertParams][] = [
  ['up-fs', 300, 200, false, 800, 480, p(-1, -1, 1, 0, true)],
  ['crop-zoom-dark', 300, 200, false, 120, 200, p(100, 80, 1.5, -0.35, true)],
  ['down-none-bright', 300, 200, false, 64, 40, p(-1, -1, 1, 0.2, false)],
  ['alpha', 90, 70, true, 50, 50, p(-1, -1, 1, 0, true)],
  ['shrink', 2100, 40, false, 400, 20, p(-1, -1, 1, 0, true)],
  ['portrait', 300, 200, false, 480, 800, p(250, -1, 1, 0, true)],
]

const sha = (b: Uint8Array) => createHash('sha256').update(b).digest('hex')

describe('画像の変換（brain-deck と同じ整数の計算）', () => {
  const golden = JSON.parse(new TextDecoder().decode(repoFile('testdata/imgconv.json'))) as Record<string, string>
  for (const [name, sw, sh, alpha, tw, th, params] of cases) {
    it(`${name}：${sw}×${sh} → ${tw}×${th}`, () => {
      expect(sha(convert(pattern(sw, sh, alpha), tw, th, params).file)).toBe(golden[name])
    })
  }
  it('横向きの写真を縦向きの画面に入れる（上下に黒い帯）と、その逆', () => {
    const img = convert(pattern(300, 200, false), 800, 480, cases[0][6]).img
    const port = fit565(img, 480, 800)
    expect(sha(encodeImageFile(port))).toBe(golden['fit-portrait'])
    expect(sha(encodeImageFile(fit565(port, 800, 480)))).toBe(golden['fit-landscape'])
  })
})
