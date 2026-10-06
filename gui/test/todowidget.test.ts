import { describe, expect, it } from 'vitest'
import { todoEllipsis, todoGeometry, todoOrder, type Rect } from '../src/todowidget'
import type { TodoItem } from '../src/types'
import { repoFile, testFont } from './helpers'

const r4 = (r: Rect | null) => (r ? [r.x0, r.y0, r.x1, r.y1] : null)

describe('Todo のウィジェット', () => {
  const font = testFont()
  // todo_test.go の todoLayoutTable が Go で書いた結果（LEFTHAND_UPDATE_TODOLAYOUT=1 go test -run TodoLayoutTable）
  const table = JSON.parse(repoFile('gui/test/fixtures/todolayout.json').toString('utf8'))

  it('Go の todoGeometry と同じ配置にする', () => {
    const bad = (table.layout as any[]).filter((c) => {
      const [x0, y0, x1, y1] = c.area
      const g = todoGeometry({ x0, y0, x1, y1 }, c.n, c.rows)
      const got = { per: g.per, pages: g.pages, scale: g.scale, first: r4(g.rows[0]), last: r4(g.rows[g.rows.length - 1]),
        nav: g.nav ? [r4(g.up), r4(g.mid), r4(g.down)] : null }
      return JSON.stringify(got) !== JSON.stringify({ per: c.per, pages: c.pages, scale: c.scale, first: c.first, last: c.last, nav: c.nav })
    })
    expect(bad).toEqual([])
    expect(table.layout.length).toBeGreaterThan(100)
  })

  it('Go の todoEllipsis と同じように省略する', () => {
    const bad = (table.ellipsis as any[]).filter((c) => todoEllipsis(font, c.text, c.w) !== c.out)
    expect(bad).toEqual([])
  })

  it('画面の順は、未完了のあとに完了', () => {
    const it = (id: string, done: boolean) => ({ id, text: id, done, rev: 1, created_at: '', updated_at: '' }) as TodoItem
    expect(todoOrder([it('a', true), it('b', false), it('c', true), it('d', false)]).map((x) => x.id)).toEqual(['b', 'd', 'a', 'c'])
  })
})
