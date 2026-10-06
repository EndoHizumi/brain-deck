import { describe, expect, it } from 'vitest'
import { clockDef, clockLines, clockParts, formatClock, hasSeconds, UNSYNCED_TEXT } from '../src/clock'
import { repoFile } from './helpers'

describe('時計の書式', () => {
  // widget_test.go の goFormatTable が Go で書いた結果（LEFTHAND_UPDATE_GOFORMAT=1 go test -run GoFormatTable）
  const table = JSON.parse(repoFile('gui/test/fixtures/goformat.json').toString('utf8')) as [string, string, string, string][]
  it('Go の time.Format と同じ結果になる', () => {
    const bad = table.filter(([t, tz, layout, want]) => formatClock(clockParts(new Date(t), tz), layout) !== want)
      .map(([t, tz, layout, want]) => `${t} ${tz} ${JSON.stringify(layout)}: ${formatClock(clockParts(new Date(t), tz), layout)} != ${want}`)
    expect(bad).toEqual([])
    expect(table.length).toBeGreaterThan(100)
  })

  it('秒を出す書式が分かる', () => {
    expect(hasSeconds('15:04')).toBe(false)
    expect(hasSeconds('15:04:05')).toBe(true)
    expect(hasSeconds('15:04:05.000')).toBe(true)
    expect(hasSeconds('1月2日({wday})')).toBe(false)
  })

  it('既定の書式と、時刻を合わせていないときの表示', () => {
    const d = clockDef({ widget: 'clock', tz: 'Asia/Tokyo' })
    const now = new Date('2026-10-06T00:05:07Z')
    expect(clockLines(d, now, true)).toEqual({ time: '09:05', date: '10月6日(火)', unsynced: false })
    expect(clockLines(d, now, false)).toEqual({ time: '09:05', date: UNSYNCED_TEXT, unsynced: true })
    expect(clockLines(clockDef({ widget: 'clock', tz: 'UTC', date_format: 'none' }), now, true).date).toBe('')
  })
})
