import { describe, expect, it } from 'vitest'
import { formatGraph, graphDecimals, graphRange, graphSegment, graphStaleText, graphWidgetOf } from '../src/graphwidget'

describe('グラフのウィジェット', () => {
  it('値の書き方は Go の formatGraph と同じ（toFixed とは、ちょうど半分の丸め方が違う）', () => {
    const cases: [number, number, string][] = [
      [27.6, -1, '27.6'], [27.0, -1, '27'], [1.125, -1, '1.13'], [0.125, -1, '0.13'], [-0.004, -1, '0'],
      [-12.34, -1, '-12.3'], [123.5, -1, '124'], [99.96, -1, '100'], [1e12, -1, '1000000000000'],
      [3, 2, '3.00'], [2.5, 0, '3'], [-2.5, 0, '-3'], [0.000001, 6, '0.000001'],
    ]
    for (const [v, fix, want] of cases) {
      const [d, trim] = graphDecimals(v, fix)
      expect(formatGraph(v, d, trim), `${v} ${fix}`).toBe(want)
    }
  })

  it('縦の範囲は Go の graphRange と同じ', () => {
    const pts = (...vs: number[]) => vs.map((v) => ({ t: '', v }))
    const g = (a: object) => graphWidgetOf({ widget: 'graph', id: 'x', ...a })
    expect(graphRange(g({}), pts(3, 7, 5))).toEqual([3, 7])
    expect(graphRange(g({ chart: 'bar' }), pts(3, 7, 5))).toEqual([0, 7])
    expect(graphRange(g({ chart: 'bar' }), pts(-3, -7))).toEqual([-7, 0])
    expect(graphRange(g({}), pts(5, 5))).toEqual([4, 6])
    expect(graphRange(g({ min: 0 }), pts(0, 0))).toEqual([0, 1])
    expect(graphRange(g({ max: 10 }), pts(20))).toEqual([9, 10])
    expect(graphRange(g({ min: 0, max: 100 }), pts(200))).toEqual([0, 100])
  })

  it('既定値と、古いときの時刻', () => {
    expect(graphWidgetOf({ widget: 'graph', id: 'cpu' })).toEqual({ id: 'cpu', unit: '', min: undefined, max: undefined, chart: 'line', points: 60, decimals: -1, stale: 300000 })
    const now = new Date(2026, 9, 10, 12, 0)
    expect(graphStaleText(new Date(2026, 9, 10, 11, 59), now)).toBe('古い 11:59')
    expect(graphStaleText(new Date(2026, 9, 9, 8, 5), now)).toBe('古い 10/9 08:05')
  })

  it('線の点はブレゼンハム（始点を除く）', () => {
    const got: [number, number][] = []
    graphSegment(0, 0, 3, 1, (x, y) => got.push([x, y]))
    expect(got.length).toBe(3)
    expect(got[2]).toEqual([3, 1])
  })
})
