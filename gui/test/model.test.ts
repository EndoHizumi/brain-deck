import { describe, expect, it } from 'vitest'
import {
  actionKind, cellOf, clean, deleteLayer, isIncomplete, normalizeConfig, parsePath, references, renameLayer,
  resolveCell, resolveGrid, resolveKey, resolveSoft, touchCell,
} from '../src/model'
import { sampleConfig } from './helpers'

describe('設定の読み込み', () => {
  it('旧形式を base レイヤーにそろえ、割り当てをオブジェクトの形にする', () => {
    const c = normalizeConfig({
      keys: { KEY_Q: 'B', KEY_W: 1 },
      touch: { min_x: 0, max_x: 1, min_y: 0, max_y: 1, cols: 2, rows: 1, cells: { '0,0': 'A' } },
    })
    expect(c.layers).toEqual([
      { name: 'base', keys: { KEY_Q: { key: 'B' }, KEY_W: { key: '1' } }, touch: { cols: 2, rows: 1, cells: { '0,0': { key: 'A' } } } },
    ])
    expect(c.touch).toEqual({ min_x: 0, max_x: 1, min_y: 0, max_y: 1 })
    expect((c as any).keys).toBeUndefined()
  })

  it('空の項目を除いて保存する', () => {
    const c = clean({ layers: [{ name: 'base', label: '', keys: {}, touch: { cols: 1, rows: 1, cells: {} } }] })
    expect(c).toEqual({ layers: [{ name: 'base', touch: { cols: 1, rows: 1 } }] })
  })
})

describe('透過の解決（layer.go と同じ規則）', () => {
  const cfg = sampleConfig()
  it('書いていないキーは下のレイヤーを使う', () => {
    expect(resolveKey(cfg, [0, 1], 'KEY_Q')).toMatchObject({ action: { key: 'LCTRL+C' }, from: 1 })
    expect(resolveKey(cfg, [0, 1], 'KEY_S')).toMatchObject({ action: { key: 'LCTRL+LSHIFT+Z' }, from: 0 })
    expect(resolveKey(cfg, [0, 1], 'KEY_Z')).toBeNull()
  })
  it('格子の大きさが同じあいだだけ、セルが透過する', () => {
    const edit = resolveGrid(cfg, [0, 1])
    expect([edit.cols, edit.rows]).toEqual([4, 3])
    expect(edit.cells[2 * 4 + 3]?.action).toEqual({ key: 'SPACE', label: '手のひら' }) // base から
    const view = resolveGrid(cfg, [0, 2])
    expect([view.cols, view.rows]).toEqual([3, 2])
    expect(view.cells.filter(Boolean)).toHaveLength(6)
    expect(resolveCell(cfg, [0, 2], 2, 1)?.from).toBe(2)
  })
  it('none は下のレイヤーも使わない', () => {
    const c = normalizeConfig({
      layers: [{ name: 'base', touch: { cols: 1, rows: 1, cells: { '0,0': 'A' } } }, { name: 'x', touch: { cells: { '0,0': 'none' } } }],
    })
    expect(resolveGrid(c, [0, 1]).cells[0]).toBeNull()
    expect(actionKind({ key: 'none' })).toBe('none')
  })
  it('ソフトキー', () => {
    expect(resolveSoft(cfg, [0, 2], 'home')).toMatchObject({ action: { layer_to: 'base' }, from: 0 })
  })
})

describe('レイヤーの操作', () => {
  it('名前を変えると、参照も書き換える', () => {
    const c = sampleConfig()
    renameLayer(c, 1, 'editing')
    expect(c.layers[0].keys!.KEY_LEFTALT).toEqual({ layer_hold: 'editing' })
    expect(c.layers[0].keys!.KEY_PAGEDOWN).toEqual({ layer_oneshot: 'editing' })
    expect(references(c, 'edit')).toEqual([])
  })
  it('消すと、そこへ切り替える割り当ても消える', () => {
    const c = sampleConfig()
    const refs = deleteLayer(c, 2)
    expect(refs.map((r) => `${r.where}:${r.id}`).sort()).toEqual(['cells:0,2', 'keys:KEY_TAB'])
    expect(c.layers.map((l) => l.name)).toEqual(['base', 'edit'])
    expect(c.layers[0].keys!.KEY_TAB).toBeUndefined()
    expect(() => deleteLayer(c, 0)).toThrow()
  })
})

describe('そのほか', () => {
  it('タッチの座標は main.go の cellOf と同じ', () => {
    for (const [v, min, max, n, want] of [
      [0, 0, 4095, 4, 0], [4095, 0, 4095, 4, 3], [2048, 0, 4095, 4, 2], [-50, 0, 4095, 4, 0],
      [5000, 0, 4095, 4, 3], [4000, 4095, 0, 4, 0], [100, 4095, 0, 4, 3],
    ]) expect(cellOf(v, min, max, n)).toBe(want)
    const t = sampleConfig().touch!
    expect(touchCell(t, 4, 3, 2191, 2222)).toEqual([2, 1]) // REPORT.md の実測（中央）
    expect(touchCell(t, 4, 3, 229, 3834)).toEqual([0, 0])
    expect(touchCell(t, 4, 3, 3886, 352)).toEqual([3, 2])
  })
  it('誤りの場所を読む', () => {
    expect(parsePath('/layers/1/keys/KEY_Q')).toEqual({ kind: 'key', layer: 1, id: 'KEY_Q' })
    expect(parsePath('/layers/0/touch/cells/2,1')).toEqual({ kind: 'cell', layer: 0, id: '2,1' })
    expect(parsePath('/layers/0/soft_keys/a~1b')).toEqual({ kind: 'soft', layer: 0, id: 'a/b' })
    expect(parsePath('/layers/2/touch')).toEqual({ kind: 'grid', layer: 2 })
    expect(parsePath('/layers/2')).toEqual({ kind: 'layer', layer: 2 })
    expect(parsePath('/hid_device')).toEqual({ kind: 'other', path: '/hid_device' })
    expect(parsePath('')).toEqual({ kind: 'other', path: '' })
  })
  it('選びかけの割り当て', () => {
    expect(isIncomplete({ key: '' })).toBe(true)
    expect(isIncomplete({ layer_hold: '', label: 'x' })).toBe(true)
    expect(isIncomplete({ label: 'x' })).toBe(true)
    expect(isIncomplete({ key: 'none' })).toBe(false)
    expect(actionKind({ layer_to: '' })).toBe('layer_to')
  })
})
