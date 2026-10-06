import { describe, expect, it } from 'vitest'
import { cellSpan, cellView, renderPreview, type Mode } from '../src/preview'
import type { PressStyle } from '../src/types'
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
  // 押したときは、右上の札に重なるセル（3,0）とレイヤーのセル（0,2）も含める
  const cases: { file: string; stack: number[]; mode: Mode; pressed?: string[]; pressStyle?: PressStyle }[] = [
    { file: 'base.png', stack: [0], mode: 'base' },
    { file: 'view.png', stack: [0, 2], mode: 'latched' },
    { file: 'edit-hold.png', stack: [0, 1], mode: 'temp', pressed: ['0,0', '3,2'] },
    { file: 'base-pressed.png', stack: [0], mode: 'base', pressed: ['0,0', '3,0', '0,2'] },
    { file: 'base-pressed-fill.png', stack: [0], mode: 'base', pressed: ['0,0', '3,0', '0,2'], pressStyle: 'fill' },
  ]
  for (const c of cases) {
    it(`lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg, stack: c.stack, mode: c.mode, pressed: new Set(c.pressed), pressStyle: c.pressStyle })
      const got = to565(pixels)
      expect(want.w * want.h * 4).toBe(got.length)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  it('press_style を省略すると枠を光らせ、設定の press_style に従う', () => {
    const at = (pixels: Uint8ClampedArray, x: number, y: number) => Array.from(pixels.slice((y * 800 + x) * 4, (y * 800 + x) * 4 + 3))
    const pressed = new Set(['1,1'])
    // セル 1,1 の枠の左端（cellSpan 200..400、160..320 に cellGap 4）から 3 ドット内側と、中央付近
    const border = renderPreview(font, { cfg, stack: [0], mode: 'base', pressed }).pixels
    expect(at(border, 204 + 3, 240)).toEqual([0xff, 0xd0, 0x40])
    expect(at(border, 204 + 10, 240)).toEqual([0x1c, 0x28, 0x38])
    const fillCfg = { ...cfg, display: { ...cfg.display, press_style: 'fill' as const } }
    const fill = renderPreview(font, { cfg: fillCfg, stack: [0], mode: 'base', pressed }).pixels
    expect(at(fill, 204 + 10, 240)).toEqual([0xff, 0xd0, 0x40])
  })

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
