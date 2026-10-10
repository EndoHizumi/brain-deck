import { describe, expect, it } from 'vitest'
// 時計の既定は PC のタイムゾーン。fixtures は TZ=Asia/Tokyo で書き出した
process.env.TZ = 'Asia/Tokyo'
import { cellSpan, cellView, renderPreview, type Mode } from '../src/preview'
import type { PressStyle } from '../src/types'
import { parseConfigText } from '../src/yamlio'
import { PAD_DEFAULTS } from '../src/model'
import { decodePNG, repoFile, repoRoot, sampleConfig, testFont } from './helpers'
import { decodeImageFile, type Image565 } from '../src/image'
import { parseIcon } from '../src/weatherwidget'
import { readdirSync } from 'node:fs'
import { resolve } from 'node:path'

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

  // 月の表示と、follow のカレンダー。config/month-example.yaml の「月」「月(横)」レイヤー、予定と祝日は fixtures/calendar-month.json
  // TZ=Asia/Tokyo go run . -render-png ... -render-layer month -render-calendar gui/test/fixtures/calendar-month.json ...（README の「開発」）
  const monthCfg = parseConfigText(repoFile('config/month-example.yaml').toString('utf8'))
  const monthLayer = (n: string) => monthCfg.layers.findIndex((l) => l.name === n)
  const monthCases: { file: string; layer: string; data: string | null; offset?: number; select?: string; synced?: boolean }[] = [
    { file: 'month.png', layer: 'month', data: 'calendar-month.json' },
    { file: 'month-select.png', layer: 'month', data: 'calendar-month.json', select: '2026-10-17' },
    { file: 'month-prev-holiday.png', layer: 'month', data: 'calendar-month.json', offset: -1, select: '2026-09-22' },
    { file: 'month-wide.png', layer: 'month-wide', data: 'calendar-month.json', select: '2026-10-12' },
    { file: 'month-unsynced.png', layer: 'month', data: null, synced: false },
    { file: 'month-out-of-range.png', layer: 'month', data: 'calendar.json', offset: 1 },
  ]
  for (const c of monthCases) {
    it(`月の表示も lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: monthCfg, stack: [0, monthLayer(c.layer)], mode: 'latched', now, synced: c.synced,
        calendar: c.data ? calJSON(c.data) : null, monthOffset: c.offset, monthSelect: c.select })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  // 背景画像（セルの画像、壁紙、文字の縁取り、押したときの枠）。config/background-example.yaml と config/background-images/
  // TZ=Asia/Tokyo go run . -render-png ... -render-images config/background-images config/background-example.yaml（README の「開発」）
  const bcfg = parseConfigText(repoFile('config/background-example.yaml').toString('utf8'))
  const bimages = new Map<string, Image565>()
  for (const f of readdirSync(resolve(repoRoot, 'config/background-images'))) {
    if (!f.endsWith('.565')) continue
    bimages.set(f.slice(0, -4), decodeImageFile(new Uint8Array(repoFile(`config/background-images/${f}`)))!)
  }
  const blayer = (n: string) => bcfg.layers.findIndex((l) => l.name === n)
  const bcases: { file: string; stack: number[]; mode: Mode; pressed?: string[]; pressStyle?: PressStyle; widgets?: boolean }[] = [
    { file: 'bg-base-pressed.png', stack: [0], mode: 'base', pressed: ['0,0', '3,2'] },
    { file: 'bg-base-pressed-fill.png', stack: [0], mode: 'base', pressed: ['0,0', '1,1'], pressStyle: 'fill' },
    { file: 'bg-menu.png', stack: [0, blayer('menu')], mode: 'latched', pressed: ['1,1'] },
    { file: 'bg-dashboard.png', stack: [0, blayer('dashboard')], mode: 'latched', widgets: true },
  ]
  for (const c of bcases) {
    it(`背景画像も lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: bcfg, stack: c.stack, mode: c.mode, pressed: new Set(c.pressed), pressStyle: c.pressStyle,
        now, texts: c.widgets ? texts : undefined, todo: c.widgets ? todo : undefined, calendar: c.widgets ? calJSON('calendar.json') : null,
        images: (id) => bimages.get(id) })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  // 電池、ストップウォッチ、ポモドーロ、カウンター、QR コード、在席ランプ。config/gadgets-example.yaml の「道具」「共有」レイヤー
  // 状態は fixtures/widgets-state*.json（-render-widgets）、QR コードは fixtures/qr*.json（-render-qr）、電池は -render-battery
  const gcfg = parseConfigText(repoFile('config/gadgets-example.yaml').toString('utf8'))
  const glayer = (n: string) => gcfg.layers.findIndex((l) => l.name === n)
  const fx = (f: string) => JSON.parse(repoFile(`gui/test/fixtures/${f}`).toString('utf8'))
  const gcases: { file: string; layer: string; widgets?: string; qr?: string; battery?: any; pressed?: string[]; pressStyle?: PressStyle }[] = [
    { file: 'gadgets-tools.png', layer: 'tools', widgets: 'widgets-state.json', battery: { percent: 87, plugged: true, charging: true } },
    { file: 'gadgets-tools-paused.png', layer: 'tools', widgets: 'widgets-state-paused.json', battery: { percent: 12, plugged: false, charging: false } },
    { file: 'gadgets-tools-alert.png', layer: 'tools', widgets: 'widgets-state-alert.json', pressed: ['3,0', '3,1'] },
    { file: 'gadgets-tools-alert-fill.png', layer: 'tools', widgets: 'widgets-state-alert.json', battery: { percent: 50, plugged: true, charging: false },
      pressed: ['0,1', '3,1', '2,0'], pressStyle: 'fill' },
    { file: 'gadgets-share.png', layer: 'share', widgets: 'widgets-state.json', qr: 'qr.json' },
    { file: 'gadgets-share-none.png', layer: 'share', pressed: ['2,0'] },
    { file: 'gadgets-share-expired.png', layer: 'share', widgets: 'widgets-state-paused.json', qr: 'qr-expired.json' },
  ]
  for (const c of gcases) {
    it(`電池やストップウォッチなども lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: gcfg, stack: [0, glayer(c.layer)], mode: 'latched', now, pressed: new Set(c.pressed),
        pressStyle: c.pressStyle, widgets: c.widgets ? fx(c.widgets) : undefined, qr: c.qr ? fx(c.qr).texts : undefined, battery: c.battery })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  // 天気のウィジェット。config/weather-example.yaml の「天気」「天気(小)」レイヤー、予報は fixtures/weather*.json（-render-weather）、
  // アイコンは fixtures/weather-icons（-render-weather-icons。テスト用に描いた円と帯で、気象庁のアイコンではない）
  const wcfg2 = parseConfigText(repoFile('config/weather-example.yaml').toString('utf8'))
  const wlayer = (n: string) => wcfg2.layers.findIndex((l) => l.name === n)
  const iconDir = resolve(repoRoot, 'gui/test/fixtures/weather-icons')
  const icons = new Map(readdirSync(iconDir).filter((f) => f.endsWith('.lha'))
    .map((f) => [f.slice(0, -4), parseIcon(new Uint8Array(repoFile(`gui/test/fixtures/weather-icons/${f}`)))!]))
  const thcases: { file: string; layer: string; data?: string; at?: string; synced?: boolean; pressed?: string[]; pressStyle?: PressStyle }[] = [
    { file: 'weather.png', layer: 'weather', data: 'weather.json' },
    { file: 'weather-small.png', layer: 'weather-small', data: 'weather.json' },
    { file: 'weather-night.png', layer: 'weather', data: 'weather-night.json', at: '2026-10-06T03:12:00+09:00' },
    { file: 'weather-stale-fill.png', layer: 'weather', data: 'weather-stale.json', pressed: ['2,0', '2,1'], pressStyle: 'fill' },
    { file: 'weather-none.png', layer: 'weather', synced: false },
    { file: 'weather-failed.png', layer: 'weather-small', data: 'weather-failed.json' },
  ]
  for (const c of thcases) {
    it(`天気も lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: wcfg2, stack: [0, wlayer(c.layer)], mode: 'latched', now: c.at ? new Date(c.at) : now,
        synced: c.synced, pressed: new Set(c.pressed), pressStyle: c.pressStyle, weather: c.data ? fx(c.data) : null, weatherIcons: (n) => icons.get(n) })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }
  it('天気アイコンがなければ、アイコンなしで描く（Brain と同じ）', () => {
    const p = { cfg: wcfg2, stack: [0, wlayer('weather')], mode: 'latched' as Mode, now, weather: fx('weather.json') }
    const none = renderPreview(font, p).pixels
    const some = renderPreview(font, { ...p, weatherIcons: (n: string) => icons.get(n) }).pixels
    expect(Buffer.from(none).equals(Buffer.from(some))).toBe(false)
  })

  it('Brain にない背景画像は、背景なしで描く（Brain と同じ）', () => {
    const none = renderPreview(font, { cfg: bcfg, stack: [0], mode: 'base', images: () => undefined }).pixels
    const plain = parseConfigText(repoFile('config/current.yaml').toString('utf8'))
    expect(Buffer.from(none).equals(Buffer.from(renderPreview(font, { cfg: plain, stack: [0], mode: 'base' }).pixels))).toBe(true)
  })

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

  // トラックパッドとマウスのボタンのセル。config/trackpad-example.yaml の「マウス」レイヤー
  // go run . -render-png gui/test/fixtures/trackpad.png -render-layer mouse config/trackpad-example.yaml
  const pcfg = parseConfigText(repoFile('config/trackpad-example.yaml').toString('utf8'))
  const mouseLayer = pcfg.layers.findIndex((l) => l.name === 'mouse')
  const pcases: { file: string; pressed?: string[]; pressStyle?: PressStyle; mouseOff?: boolean }[] = [
    { file: 'trackpad.png' },
    { file: 'trackpad-off.png', mouseOff: true }, // -render-mouse-off
    { file: 'trackpad-pressed.png', pressed: ['0,0', '3,1'] },
    { file: 'trackpad-pressed-fill.png', pressed: ['0,0', '3,1'], pressStyle: 'fill' },
  ]
  for (const c of pcases) {
    it(`トラックパッドも lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: pcfg, stack: [0, mouseLayer], mode: 'latched', pressed: new Set(c.pressed), pressStyle: c.pressStyle, mouseOff: c.mouseOff })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  // メディアキーと F13〜F24 のセル。config/media-example.yaml の「メディア」と「F13〜F24」のレイヤー
  // go run . -render-png gui/test/fixtures/media.png -render-layer media config/media-example.yaml
  // （media-off は -render-mouse-off、media-off-pressed-fill は -render-mouse-off -render-pressed "1,0 0,1" -render-press-style fill）
  const mcfg = parseConfigText(repoFile('config/media-example.yaml').toString('utf8'))
  const mcases: { file: string; layer: string; pressed?: string[]; pressStyle?: PressStyle; mouseOff?: boolean }[] = [
    { file: 'media.png', layer: 'media' },
    { file: 'media-off.png', layer: 'media', mouseOff: true },
    { file: 'media-off-pressed-fill.png', layer: 'media', mouseOff: true, pressed: ['1,0', '0,1'], pressStyle: 'fill' },
    { file: 'fkeys.png', layer: 'fkeys' },
  ]
  for (const c of mcases) {
    it(`メディアキーと F13〜F24 も lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const layer = mcfg.layers.findIndex((l) => l.name === c.layer)
      const { pixels } = renderPreview(font, { cfg: mcfg, stack: [0, layer], mode: 'latched', pressed: new Set(c.pressed), pressStyle: c.pressStyle, mouseOff: c.mouseOff })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  // コマンドのショートカット（F14〜F18 と Ctrl + F14〜F16 のセル、brain-deck exec の結果のタイル）。config/shortcuts-example.yaml の「コマンド」
  // TZ=Asia/Tokyo go run . -render-png gui/test/fixtures/commands.png -render-layer commands -render-time 2026-10-09T18:21:00+09:00 config/shortcuts-example.yaml
  // （commands-results は -render-texts gui/test/fixtures/texts-exec.json -render-pressed "0,0 1,2"）
  const cmdCfg = parseConfigText(repoFile('config/shortcuts-example.yaml').toString('utf8'))
  const cmdLayer = cmdCfg.layers.findIndex((l) => l.name === 'commands')
  const execTexts = JSON.parse(repoFile('gui/test/fixtures/texts-exec.json').toString('utf8')).texts
  const cmdCases: { file: string; pressed?: string[]; texts?: Record<string, any> }[] = [
    { file: 'commands.png' },
    { file: 'commands-results.png', pressed: ['0,0', '1,2'], texts: execTexts },
  ]
  for (const c of cmdCases) {
    it(`コマンドのショートカットも lefthand -render-png と画素単位で同じ（${c.file}）`, () => {
      const want = decodePNG(repoFile(`gui/test/fixtures/${c.file}`))
      const { pixels } = renderPreview(font, { cfg: cmdCfg, stack: [0, cmdLayer], mode: 'latched', pressed: new Set(c.pressed),
        now: new Date('2026-10-09T18:21:00+09:00'), texts: c.texts })
      const got = to565(pixels)
      let diff = 0
      for (let i = 0; i < got.length; i++) if (got[i] !== want.data[i]) diff++
      expect(diff).toBe(0)
    })
  }

  it('トラックパッドの既定値は Go と同じ（fixtures/pad-defaults.json）', () => {
    expect(JSON.parse(repoFile('gui/test/fixtures/pad-defaults.json').toString('utf8'))).toEqual({ ...PAD_DEFAULTS })
  })

  it('スクロールの帯は scroll_width で変わり、0 なら描かない', () => {
    const at = (pixels: Uint8ClampedArray, x: number, y: number) => Array.from(pixels.slice((y * 800 + x) * 4, (y * 800 + x) * 4 + 3))
    const cfg2 = structuredClone(pcfg)
    const cell = cfg2.layers[mouseLayer].touch!.cells!['0,0']
    cell.scroll_width = 0
    const none = renderPreview(font, { cfg: cfg2, stack: [0, mouseLayer], mode: 'latched' }).pixels
    expect(at(none, 560, 240)).toEqual([0x1c, 0x28, 0x38])
    cell.scroll_width = 120
    const wide = renderPreview(font, { cfg: cfg2, stack: [0, mouseLayer], mode: 'latched' }).pixels
    expect(at(wide, 490, 240)).toEqual([0x26, 0x34, 0x48])
    expect(at(wide, 470, 240)).toEqual([0x1c, 0x28, 0x38])
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
    expect(cellView(cfg, { mouse: 'left' })).toMatchObject({ label: '左クリック', sub: '' })
    expect(cellView(cfg, { mouse: 'scroll_down', label: '下' })).toMatchObject({ label: '下', sub: 'スクロール↓' })
    expect(cellView(cfg, { usb_mode: 'toggle' })).toMatchObject({ layer: false, label: 'マウス切替', sub: '' })
  })

  it('フォントにない文字が分かる', () => {
    expect(font.missing('ブラシ😀')).toEqual(['😀'])
  })
})
