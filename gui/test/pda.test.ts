import { describe, expect, it } from 'vitest'
// 時計の既定は PC のタイムゾーン。fixtures は TZ=Asia/Tokyo で書き出した
process.env.TZ = 'Asia/Tokyo'
import { readdirSync } from 'node:fs'
import { resolve } from 'node:path'
import { renderPreview, type Mode } from '../src/preview'
import { parseConfigText } from '../src/yamlio'
import { barShown, layerGrid, resolveGrid, transposeCells } from '../src/model'
import { homeGeometry, homeGrid, homeHit, ICON_NAMES } from '../src/home'
import { barItems } from '../src/statusbar'
import { decodeImageFile, type Image565 } from '../src/image'
import { decodePNG, repoFile, repoRoot, testFont } from './helpers'
import type { PressStyle } from '../src/types'

// Brain の画面は RGB565。Go の -render-png は、その丸めた色で書き出す
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

describe('ホーム画面、状態の帯、縦向きの格子のプレビュー', () => {
  const font = testFont()
  const cfg = parseConfigText(repoFile('config/pda-example.yaml').toString('utf8'))
  const hcfg = parseConfigText(repoFile('gui/test/fixtures/home-images.yaml').toString('utf8'))
  const images = new Map<string, Image565>()
  for (const f of readdirSync(resolve(repoRoot, 'config/background-images'))) {
    if (f.endsWith('.565')) images.set(f.slice(0, -4), decodeImageFile(new Uint8Array(repoFile(`config/background-images/${f}`)))!)
  }
  const now = new Date('2026-10-06T09:41:27+09:00')
  const fx = (f: string) => JSON.parse(repoFile(`gui/test/fixtures/${f}`).toString('utf8'))
  const li = (c: typeof cfg, n: string) => c.layers.findIndex((l) => l.name === n)

  // fixtures は README の「開発」の -render-png（-render-rotate、-render-home-page、-render-link、-render-last-sync）で書き出したもの
  const cases: { file: string; c?: typeof cfg; layer?: string; home?: number; portrait?: boolean; battery?: any; link?: string; lastSync?: string;
    mouseOff?: boolean; widgets?: string; calendar?: string; pressed?: string[]; pressStyle?: PressStyle; images?: boolean }[] = [
    { file: 'pda-home.png', home: 0, battery: { percent: 87, plugged: false, charging: false }, lastSync: '2026-10-06T09:30:00+09:00' },
    { file: 'pda-home-p2-portrait.png', home: 1, portrait: true, battery: { percent: 12, plugged: false, charging: false }, link: 'suspended', mouseOff: true },
    { file: 'pda-tools.png', layer: 'tools', battery: { percent: 50, plugged: true, charging: true }, link: 'not attached', widgets: 'widgets-state.json' },
    { file: 'pda-calendar-portrait.png', layer: 'calendar', portrait: true, calendar: 'calendar.json', lastSync: '2026-10-05T18:00:00+09:00' },
    { file: 'pda-info-portrait-pressed.png', layer: 'info', portrait: true, pressed: ['0,0', '1,3'] },
    { file: 'pda-system-portrait-fill.png', layer: 'system', portrait: true, pressed: ['0,2'], pressStyle: 'fill', battery: { percent: 50, plugged: true, charging: false } },
    { file: 'pda-base-180.png', layer: 'base' },
    { file: 'home-images.png', c: hcfg, home: 0, images: true },
    { file: 'home-images-portrait.png', c: hcfg, home: 0, portrait: true, images: true },
    { file: 'home-images-stretch.png', c: hcfg, layer: 'info', portrait: true, images: true },
  ]
  for (const t of cases) {
    it(`lefthand -render-png と画素単位で同じ（${t.file}）`, () => {
      const c = t.c ?? cfg
      const want = decodePNG(repoFile(`gui/test/fixtures/${t.file}`))
      const l = t.layer ? li(c, t.layer) : -1
      const stack = l > 0 ? [0, l] : [0]
      const mode: Mode = l > 0 ? 'latched' : 'base'
      const [w, h] = t.portrait ? [480, 800] : [800, 480]
      const { pixels } = renderPreview(font, { cfg: c, stack, mode, w, h, now, home: t.home, battery: t.battery ?? null, link: t.link,
        lastSync: t.lastSync ? new Date(t.lastSync) : null, mouseOff: t.mouseOff, widgets: t.widgets ? fx(t.widgets) : undefined,
        calendar: t.calendar ? fx(t.calendar) : null, pressed: new Set(t.pressed), pressStyle: t.pressStyle,
        images: t.images ? (id) => images.get(id) : undefined })
      expect(want.w).toBe(w)
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  it('縦向きの格子は rotate.go と同じ規則で作る', () => {
    const tools = li(cfg, 'tools')
    expect(layerGrid(cfg, tools, 'portrait')).toMatchObject({ cols: 2, rows: 4, own: true })
    const info = li(cfg, 'info')
    const g = layerGrid(cfg, info, 'portrait')!
    expect(g).toMatchObject({ cols: 3, rows: 4, own: false })
    expect(g.cells['0,0'].span).toEqual([2, 2])
    expect(g.cells['2,0'].span).toEqual([1, 3]) // 横向きの "0,2" の [3, 1]
    expect(transposeCells({ '3,1': { key: 'A' } })).toEqual({ '1,3': { key: 'A' } })
    expect(resolveGrid(cfg, [0, info], 'portrait').cols).toBe(3)
    expect(layerGrid(hcfg, info, 'portrait')).toMatchObject({ cols: 4, rows: 3, own: false }) // stretch
  })

  it('状態の帯を出すかは layer.go の view と同じ規則', () => {
    expect(barShown(cfg, [0])).toBe(false) // base は status_bar: false
    expect(barShown(cfg, [0, li(cfg, 'tools')])).toBe(true) // status_bar.show
    const c = structuredClone(cfg)
    c.layers.push({ name: 'hold', keys: { KEY_A: { key: 'B' } } })
    expect(barShown(c, [0, c.layers.length - 1])).toBe(false) // 格子のないレイヤーは下のとおり
    expect(barItems(['pc', 'mouse', 'time'], { now, synced: false, battery: null, link: 'suspended', mouseOff: true, lastSync: null }).map((i) => i.text)).toEqual(['PC休止', '09:41'])
  })

  it('ホーム画面の配置とページ（home.go と同じ）', () => {
    const h = cfg.home!
    expect(homeGrid(h, false)).toEqual([4, 3])
    expect(homeGrid(h, true)).toEqual([3, 4])
    expect(homeGrid({ apps: [] }, true)).toEqual([3, 4])
    const g = homeGeometry({ x0: 0, y0: 28, x1: 800, y1: 480 }, h, false, 5)
    expect(g.pages).toBe(2)
    expect(g.page).toBe(1)
    expect(g.first).toBe(12)
    expect(homeHit(g, 100, 100)).toBe(12)
    expect(homeHit(g, 700, 300)).toBe(-1) // 空き
    expect(ICON_NAMES).toContain('calendar')
  })
})
