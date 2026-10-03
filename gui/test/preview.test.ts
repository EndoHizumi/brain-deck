import { describe, expect, it } from 'vitest'
import { cellSpan, cellView, renderPreview, type Mode } from '../src/preview'
import { decodePNG, repoFile, sampleConfig, testFont } from './helpers'

// Brain の画面は RGB565。Go の -render-png は、その丸めた色で書き出す（fb.go の pack / unpack）
function to565(px: Uint8ClampedArray): Uint8Array {
  const out = new Uint8Array(px.length)
  const q = (v: number, len: number) => Math.floor(((v >> (8 - len)) * 255) / ((1 << len) - 1))
  for (let i = 0; i < px.length; i += 4) {
    out[i] = q(px[i], 5)
    out[i + 1] = q(px[i + 1], 6)
    out[i + 2] = q(px[i + 2], 5)
    out[i + 3] = 255
  }
  return out
}

describe('Brain の画面のプレビュー', () => {
  const font = testFont()
  const cfg = sampleConfig()

  // fixtures は `go run . -render-png ...` で書き出したもの（README の「開発」）
  const cases: { file: string; stack: number[]; mode: Mode; pressed?: string[] }[] = [
    { file: 'base.png', stack: [0], mode: 'base' },
    { file: 'view.png', stack: [0, 2], mode: 'latched' },
    { file: 'edit-hold.png', stack: [0, 1], mode: 'temp', pressed: ['0,0', '3,2'] },
  ]
  for (const c of cases) {
    it(`lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg, stack: c.stack, mode: c.mode, pressed: new Set(c.pressed) })
      const got = to565(pixels)
      expect(want.w * want.h * 4).toBe(got.length)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  it('セルの境界はタッチの判定と同じ', () => {
    expect(cellSpan(0, 3, 800)).toEqual([0, 267])
    expect(cellSpan(1, 3, 800)).toEqual([267, 534])
    expect(cellSpan(2, 3, 800)).toEqual([534, 800])
  })

  it('セルの内容は main.go の cellView と同じ規則', () => {
    expect(cellView(cfg, { key: 'LCTRL+LSHIFT+Z' })).toMatchObject({ label: 'Ctrl+Shift+Z', sub: '' })
    expect(cellView(cfg, { key: 'B', label: 'ブラシ' })).toMatchObject({ label: 'ブラシ', sub: 'B' })
    expect(cellView(cfg, { layer_toggle: 'view' })).toMatchObject({ layer: true, label: '表示', sub: '切替' })
    expect(cellView(cfg, { layer_hold: 'edit', label: 'E' })).toMatchObject({ label: 'E', sub: '押す間:編集' })
  })

  it('フォントにない文字が分かる', () => {
    expect(font.missing('ブラシ😀')).toEqual(['😀'])
  })
})
