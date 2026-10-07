import { describe, expect, it } from 'vitest'
// fixtures は Asia/Tokyo で作った。今日の範囲と時刻は PC のタイムゾーンで決める
process.env.TZ = 'Asia/Tokyo'
import { calLayout, parseDuration } from '../src/calwidget'
import { todoCaption } from '../src/todowidget'
import type { TodoItem } from '../src/types'
import { repoFile } from './helpers'

describe('カレンダーのウィジェット', () => {
  // calendar_test.go の TestCalLayoutTable が Go で書いた結果（LEFTHAND_UPDATE_CALLAYOUT=1 go test -run CalLayoutTable）
  const table = JSON.parse(repoFile('gui/test/fixtures/callayout.json').toString('utf8')) as any[]
  const data: Record<string, any> = {
    'calendar.json': JSON.parse(repoFile('gui/test/fixtures/calendar.json').toString('utf8')),
    'calendar-stale.json': JSON.parse(repoFile('gui/test/fixtures/calendar-stale.json').toString('utf8')),
    none: null,
  }

  it('Go の calLayout と同じ行、ページ、最終更新にする', () => {
    const bad = table.filter((c) => {
      const [x0, y0, x1, y1] = c.area
      const stale = c.stale ? parseDuration(c.stale)! : 3 * 3600_000
      const l = calLayout({ x0, y0, x1, y1 }, { rows: c.rows, calendars: c.calendars, staleMs: stale }, data[c.fixture], Date.parse(c.now), c.synced)
      const got: Record<string, unknown> = { rows: l.rows, auto_page: l.auto_page, scale: l.scale, footer: l.footer, footer_warn: l.footer_warn,
        footer_scale: l.footer_scale, per: l.geom.per, pages: l.geom.pages, footer_y: l.footerY }
      if (l.message) got.message = l.message
      const want = { ...c.layout, per: c.per, pages: c.pages, footer_y: c.footer_y }
      const canon = (o: Record<string, unknown>) => JSON.stringify(Object.keys(o).sort().map((k) => [k, o[k]]))
      return canon(got) !== canon(want)
    })
    expect(bad.slice(0, 3)).toEqual([])
    expect(table.length).toBeGreaterThan(300)
  })

  it('時間の書き方を読む', () => {
    expect(parseDuration('3h')).toBe(3 * 3600_000)
    expect(parseDuration('1h30m')).toBe(90 * 60_000)
    expect(parseDuration('90s')).toBe(90_000)
    expect(parseDuration('soon')).toBeNull()
    expect(parseDuration('')).toBeNull()
  })

  it('Todo の見出しに残りの件数を出す（todowidget.go の todoCaption）', () => {
    const it = (done: boolean) => ({ id: 'x', text: 'x', done, rev: 1, created_at: '', updated_at: '' }) as TodoItem
    expect(todoCaption('Todo', [it(true), it(false), it(false)])).toBe('Todo 残り 2')
    expect(todoCaption('', [it(false)])).toBe('残り 1')
    expect(todoCaption('Todo', [])).toBe('Todo')
    expect(todoCaption('Todo', [it(true)])).toBe('Todo すべて完了')
  })
})
