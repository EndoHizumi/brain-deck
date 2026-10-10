import { describe, expect, it } from 'vitest'
import { calcGeom, exprText, formatRat, parseRat } from '../src/calc'

describe('電卓の表示', () => {
  it('数の書き方は Go の formatRat と同じ（calc_test.go の TestFormatRat と同じ表）', () => {
    const cases: [string, number, number, number, string, boolean][] = [
      ['1/3', 10, 8, 20, '0.333333', false], ['2/3', 10, 8, 20, '0.666667', false], ['-2/3', 10, 8, 20, '-0.66667', false],
      ['1/8', 10, 8, 20, '0.125', true], ['1/8', 10, 4, 20, '0.13', false], ['999/1000', 10, 4, 20, '1', false],
      ['-1/1000', 10, 4, 20, '0', false], ['255', 16, 10, 20, 'FF', true], ['-255/16', 16, 10, 20, '-F.F', true],
      ['1/3', 16, 8, 20, '0.555555', false], ['5/2', 2, 10, 20, '10.1', true], ['1/10', 2, 12, 20, '0.000110011', false],
      ['123456789', 10, 4, 20, '123456789', true], ['1/3', 10, 80, 12, '0.333333333333', false],
    ]
    for (const [v, base, n, f, want, exact] of cases) expect(formatRat(parseRat(v)!, base, n, f), `${v} ${base} ${n}`).toEqual([want, exact])
  })

  it('式の表示', () => {
    expect(exprText([{ t: 'op', s: '-' }, { t: 'num', s: '.5' }, { t: 'op', s: '*' }, { t: 'lp' }, { t: 'val', v: '1/3' }, { t: 'op', s: '/' },
      { t: 'num', s: '2' }, { t: 'rp' }], 10)).toBe('−0.5×(0.3333333333…÷2)')
    expect(exprText([{ t: 'val', v: '255/1' }], 16)).toBe('FF')
    expect(parseRat('x')).toBeNull()
  })

  it('ボタンは 6 列 × 5 行、表示は上の残り', () => {
    const g = calcGeom({ x0: 0, y0: 0, x1: 600, y1: 350 })
    expect(g.buttons.length).toBe(30)
    expect(g.disp).toEqual({ x0: 0, y0: 0, x1: 600, y1: 98 })
    expect(g.buttons[29]).toEqual({ x0: 502, y0: 302, x1: 598, y1: 348 })
  })
})
