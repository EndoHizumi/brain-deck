import { describe, expect, it } from 'vitest'
// fixtures は Asia/Tokyo で作った。今日と日付は PC のタイムゾーンで決める
process.env.TZ = 'Asia/Tokyo'
import { calCaption, calLayout } from '../src/calwidget'
import { covers, eachDay, monthFollowDay, monthLayout, monthWidgetOf } from '../src/monthwidget'
import { repoFile, testFont } from './helpers'

describe('月の表示のウィジェット', () => {
  // monthwidget_test.go の TestMonthLayoutTable が Go で書いた結果（LEFTHAND_UPDATE_MONTHLAYOUT=1 go test -run MonthLayoutTable）
  const table = JSON.parse(repoFile('gui/test/fixtures/monthlayout.json').toString('utf8')) as any[]
  const fx = (f: string) => JSON.parse(repoFile(`gui/test/fixtures/${f}`).toString('utf8'))
  const data: Record<string, any> = { 'calendar-month.json': fx('calendar-month.json'), 'calendar.json': fx('calendar.json'), none: null }
  const font = testFont()
  const canon = (o: unknown): string => JSON.stringify(o, (_k, v) => (v && typeof v === 'object' && !Array.isArray(v)
    ? Object.fromEntries(Object.keys(v).sort().filter((k) => v[k] !== undefined).map((k) => [k, v[k]])) : v))

  it('Go の monthLayout と同じ日付、色、印、配置にする', () => {
    const bad = table.filter((c) => {
      const [x0, y0, x1, y1] = c.area
      const w = monthWidgetOf({ week_start: c.week_start, calendars: c.calendars })
      const got = monthLayout(font, { x0, y0, x1, y1 }, w, data[c.fixture], Date.parse(c.now), c.synced, { offset: c.offset, sel: c.select ?? '' })
      return canon(got) !== canon(c.layout)
    })
    expect(bad.slice(0, 2).map((c) => [c.now, c.area, c.offset, c.select])).toEqual([])
    expect(table.length).toBeGreaterThan(100)
    // 比べ方が違いを見つけること：週の始まりを変えると一致しない
    const c = table[0]
    const [x0, y0, x1, y1] = c.area
    const other = monthLayout(font, { x0, y0, x1, y1 }, monthWidgetOf({ week_start: 'monday' }), data[c.fixture], Date.parse(c.now), c.synced)
    expect(c.week_start).toBeUndefined()
    expect(canon(other)).not.toBe(canon(c.layout))
  })

  it('選んだ日の予定を、follow のカレンダーに Go と同じく出す', () => {
    const cases = table.filter((c) => c.follow)
    expect(cases.length).toBeGreaterThan(5)
    const bad = cases.filter((c) => {
      const now = Date.parse(c.now)
      const day = monthFollowDay({ offset: c.offset, sel: c.select }, now)
      const l = calLayout({ x0: 412, y0: 40, x1: 788, y1: 308 }, { rows: 0, staleMs: 3 * 3600_000 }, data[c.fixture], now, c.synced, day)
      const got: Record<string, unknown> = { rows: l.rows, auto_page: l.auto_page, scale: l.scale, footer: l.footer, footer_warn: l.footer_warn,
        footer_scale: l.footer_scale }
      if (l.message) got.message = l.message
      return canon(got) !== canon(c.follow) || calCaption('予定', day) !== c.caption
    })
    expect(bad.slice(0, 2).map((c) => [c.now, c.select])).toEqual([])
  })

  it('予定の印を付ける日（0 時ちょうどに終わる予定、長さ 0、範囲の前後）', () => {
    const t = (s: string) => Date.parse(s + '+09:00')
    const days = (s: string, e: string, from = '2026-10-01T00:00:00', to = '2026-10-11T00:00:00') => {
      const out: number[] = []
      eachDay({ s: t(s), e: t(e) }, t(from), t(to), (d) => out.push(new Date(d).getDate()))
      return out
    }
    expect(days('2026-10-05T22:00:00', '2026-10-06T00:00:00')).toEqual([5])
    expect(days('2026-10-05T22:00:00', '2026-10-06T00:00:01')).toEqual([5, 6])
    expect(days('2026-10-05T00:00:00', '2026-10-05T00:00:00')).toEqual([5])
    expect(days('2026-09-29T10:00:00', '2026-10-02T10:00:00')).toEqual([1, 2])
    expect(days('2026-10-09T10:00:00', '2026-10-20T10:00:00')).toEqual([9, 10])
    expect(covers({ rev: 1, calendars: [], from: '2026-10-06', days: 7 }, t('2026-10-13T00:00:00'))).toBe(true)
    expect(covers({ rev: 1, calendars: [], from: '2026-10-06', days: 7 }, t('2026-10-14T00:00:00'))).toBe(false)
    expect(covers({ rev: 1, calendars: [] }, t('2000-01-01T00:00:00'))).toBe(true)
  })
})
