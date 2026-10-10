import { describe, expect, it } from 'vitest'
process.env.TZ = 'Asia/Tokyo'
import { renderPreview, type Mode } from '../src/preview'
import { parseConfigText } from '../src/yamlio'
import { decodePNG, repoFile, testFont } from './helpers'

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

// フェーズ H：ペンタブレットと手書きメモ（config/pen-example.yaml）
// TZ=Asia/Tokyo go run . -render-png gui/test/fixtures/pen-memo-portrait.png -render-layer memo -render-rotate 90 -render-time 2026-10-06T09:41:27+09:00 config/pen-example.yaml
// （pen-tablet.png は -render-layer pen、pen-tablet-mouseoff.png はさらに -render-mouse-off）
describe('ペンタブレットと手書きメモのプレビュー', () => {
  const font = testFont()
  const cfg = parseConfigText(repoFile('config/pen-example.yaml').toString('utf8'))
  const now = new Date('2026-10-06T09:41:27+09:00')
  const cases = [
    { file: 'pen-memo-portrait.png', layer: 'memo', portrait: true },
    { file: 'pen-tablet.png', layer: 'pen' },
    { file: 'pen-tablet-mouseoff.png', layer: 'pen', mouseOff: true },
  ]
  for (const t of cases) {
    it(`lefthand -render-png と画素単位で同じ（${t.file}）`, () => {
      const l = cfg.layers.findIndex((x) => x.name === t.layer)
      const [w, h] = t.portrait ? [480, 800] : [800, 480]
      const mode: Mode = 'latched'
      const { pixels } = renderPreview(font, { cfg, stack: [0, l], mode, w, h, now, battery: null, mouseOff: t.mouseOff })
      const want = decodePNG(repoFile(`gui/test/fixtures/${t.file}`))
      expect(want.w).toBe(w)
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }
})
