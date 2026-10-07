import { describe, expect, it } from 'vitest'
// 時計の既定は PC のタイムゾーン。fixtures は TZ=Asia/Tokyo で書き出した
process.env.TZ = 'Asia/Tokyo'
import { cellSpan, cellView, renderPreview, type Mode } from '../src/preview'
import type { PressStyle } from '../src/types'
import { parseConfigText } from '../src/yamlio'
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

  // ウィジェット（時計）と span のセル。config/widgets-example.yaml の「情報」レイヤー
  // TZ=Asia/Tokyo go run . -render-png ... -render-layer info -render-time 2026-10-06T09:41:27+09:00 config/widgets-example.yaml
  const wcfg = parseConfigText(repoFile('config/widgets-example.yaml').toString('utf8'))
  const info = wcfg.layers.findIndex((l) => l.name === 'info')
  const now = new Date('2026-10-06T09:41:27+09:00')
  // テキストのタイルの中身は fixtures/texts.json（-render-texts）。build は表示中、deploy は期限切れ
  const texts = JSON.parse(repoFile('gui/test/fixtures/texts.json').toString('utf8')).texts
  const wcases: { file: string; pressed?: string[]; pressStyle?: PressStyle; synced?: boolean; texts?: Record<string, any> }[] = [
    { file: 'widgets.png' },
    { file: 'widgets-unsynced-pressed.png', pressed: ['3,0', '0,2'], synced: false },
    { file: 'widgets-pressed-fill.png', pressed: ['3,0', '0,2'], pressStyle: 'fill' },
    { file: 'widgets-texts.png', texts },
    { file: 'widgets-texts-pressed.png', pressed: ['2,1', '3,1'], texts },
    { file: 'widgets-texts-pressed-fill.png', pressed: ['2,1', '3,1'], pressStyle: 'fill', texts },
  ]
  for (const c of wcases) {
    it(`ウィジェットも lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: wcfg, stack: [0, info], mode: 'latched', pressed: new Set(c.pressed),
        pressStyle: c.pressStyle, now, synced: c.synced, texts: c.texts })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  // Todo のウィジェット。config/widgets-example.yaml の「Todo」レイヤー、項目は fixtures/todo.json（-render-todo）
  // TZ=Asia/Tokyo go run . -render-png ... -render-layer todo -render-todo-page 2 -render-todo gui/test/fixtures/todo.json ...
  const todoLayer = wcfg.layers.findIndex((l) => l.name === 'todo')
  const todo = JSON.parse(repoFile('gui/test/fixtures/todo.json').toString('utf8'))
  const tcases: { file: string; page?: number; empty?: boolean }[] = [
    { file: 'todo.png' },
    { file: 'todo-page2.png', page: 1 },
    { file: 'todo-empty.png', empty: true },
  ]
  for (const c of tcases) {
    it(`Todo も lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: wcfg, stack: [0, todoLayer], mode: 'latched', now, texts,
        todo: c.empty ? { rev: 0, items: [] } : todo, todoPage: c.page })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  // カレンダーのウィジェット。config/widgets-example.yaml の「予定」レイヤー、予定は fixtures/calendar.json（-render-calendar）
  // TZ=Asia/Tokyo go run . -render-png ... -render-layer calendar -render-calendar gui/test/fixtures/calendar.json ...
  const calLayer = wcfg.layers.findIndex((l) => l.name === 'calendar')
  const calJSON = (f: string) => JSON.parse(repoFile(`gui/test/fixtures/${f}`).toString('utf8'))
  const ccases: { file: string; data: string | null; page?: number; synced?: boolean }[] = [
    { file: 'calendar.png', data: 'calendar.json' },
    { file: 'calendar-page2.png', data: 'calendar.json', page: 1 },
    { file: 'calendar-stale.png', data: 'calendar-stale.json' },
    { file: 'calendar-none.png', data: null, synced: false },
  ]
  for (const c of ccases) {
    it(`カレンダーも lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: wcfg, stack: [0, calLayer], mode: 'latched', now, synced: c.synced,
        calendar: c.data ? calJSON(c.data) : null, calendarPage: c.page })
      const got = to565(pixels)
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
