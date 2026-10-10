import { describe, expect, it } from 'vitest'
// fixtures は Asia/Tokyo で作った。今日と日付は PC のタイムゾーンで決める
process.env.TZ = 'Asia/Tokyo'
import { blend8, parseIcon, weatherWidgetOf, wthFooter, wthLayout } from '../src/weatherwidget'
import { repoFile, testFont } from './helpers'
import { readdirSync } from 'node:fs'
import { resolve } from 'node:path'

describe('天気のウィジェット', () => {
  // weather_test.go の TestWeatherFixtures が Go で書いた結果（LEFTHAND_UPDATE_WEATHER=1 go test -run TestWeatherFixtures）
  const table = JSON.parse(repoFile('gui/test/fixtures/weatherlayout.json').toString('utf8')) as any[]
  const fx = (f: string) => JSON.parse(repoFile(`gui/test/fixtures/${f}`).toString('utf8'))
  const font = testFont()
  const canon = (o: unknown): string => JSON.stringify(o, (_k, v) => (v && typeof v === 'object' && !Array.isArray(v)
    ? Object.fromEntries(Object.keys(v).sort().filter((k) => v[k] !== undefined).map((k) => [k, v[k]])) : v))
  const layout = (c: any, stale?: number) => {
    const [x0, y0, x1, y1] = c.area
    return wthLayout(font, { x0, y0, x1, y1 }, { staleMs: stale ?? c.stale_sec * 1000 }, c.data ? fx(c.data) : null, Date.parse(c.now), c.synced)
  }

  it('Go の wthLayout と同じ文字、色、アイコンの箱、配置にする', () => {
    const bad = table.filter((c) => canon(layout(c)) !== canon(c.items))
    expect(bad.slice(0, 2).map((c) => [c.name, c.area, canon(layout(c)), canon(c.items)])).toEqual([])
    expect(table.length).toBeGreaterThan(30)
    expect(new Set(table.map((c) => c.name)).size).toBe(7)
    // 比べ方が違いを見つけること：古いとみなすまでを変えると一致しない
    const c = table.find((x) => x.name === 'day')
    expect(canon(layout(c, 60_000))).not.toBe(canon(c.items))
  })

  it('stale を読む（既定 3 時間）', () => {
    expect(weatherWidgetOf({}).staleMs).toBe(3 * 3600_000)
    expect(weatherWidgetOf({ stale: '90m' }).staleMs).toBe(90 * 60_000)
  })

  it('最終更新の行は、幅が足りなければ発表、出典の順に省く', () => {
    const d = fx('weather-stale.json')
    const now = Date.parse('2026-10-06T09:41:27+09:00')
    const w = { staleMs: 3 * 3600_000 }
    expect(wthFooter(font, w, d, now, true, 1000)).toEqual({ text: '更新 3:00 古い 失敗 10/5 23時発表 出典:気象庁', warn: true })
    expect(wthFooter(font, w, d, now, true, font.textWidth('更新 3:00 古い 失敗 気象庁')).text).toBe('更新 3:00 古い 失敗 気象庁')
    expect(wthFooter(font, w, d, now, false, 1000)).toEqual({ text: '時刻未設定', warn: true })
  })

  it('アイコンのファイル（LHA1）を読み、Go と同じく混ぜる', () => {
    const dir = resolve(__dirname, 'fixtures/weather-icons')
    const files = readdirSync(dir).filter((f) => f.endsWith('.lha'))
    expect(files.length).toBeGreaterThan(5)
    for (const f of files) {
      const ic = parseIcon(new Uint8Array(repoFile(`gui/test/fixtures/weather-icons/${f}`)))
      const [, w, h] = /^\d{3}-(\d+)x(\d+)\.lha$/.exec(f)!
      expect(ic && ic.w <= +w && ic.h <= +h).toBe(true)
    }
    expect(parseIcon(new Uint8Array([0x4c, 0x48, 0x49, 0x31, 1, 0, 1, 0, 0, 0, 0]))).toBeNull()
    expect([blend8(255, 0, 255), blend8(255, 0, 0), blend8(255, 0, 128), blend8(0, 255, 64)]).toEqual([255, 0, 128, 191])
  })
})
