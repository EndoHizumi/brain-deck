import { describe, expect, it } from 'vitest'
import { diffConfigs } from '../src/diff'
import keymap from '../src/keymap-pwsh2.json'
import type { KeymapInfo } from '../src/types'
import { FileError, parseConfigText, sameConfig, toJSON, toYAML } from '../src/yamlio'
import { repoFile, sampleConfig } from './helpers'

describe('ファイルの読み書き', () => {
  it('YAML と JSON で書き出し、読み直すと同じになる', () => {
    const cfg = sampleConfig()
    cfg.layers[0].touch!.cells!['1,2'] = { key: '1', label: '二行\nの: #ラベル' }
    for (const text of [toYAML(cfg), toJSON(cfg)]) expect(sameConfig(parseConfigText(text), cfg)).toBe(true)
  })

  it('YAML はデーモンが保存するのと同じ見た目', () => {
    const y = toYAML(sampleConfig())
    expect(y).toContain('KEY_A: LCTRL+Z')
    expect(y).toContain('KEY_LEFTALT: { layer_hold: edit }')
    expect(y).toContain('"0,0": { key: B, label: ブラシ }')
    expect(y).toContain('home: { x: [ 3740, 4095 ], y: [ 3377, 4095 ] }')
  })

  it('JSON の例（docs/config.md）も読める', () => {
    const md = repoFile('docs/config.md').toString('utf8')
    const json = md.match(/## JSON の例\s+```json\n([\s\S]*?)```/)![1]
    const cfg = parseConfigText(json)
    expect(cfg.layers[0].keys!.KEY_Q).toEqual({ key: 'B' })
  })

  it('読めないファイルは理由を出す', () => {
    expect(() => parseConfigText('layers: [ {name: base')).toThrow(FileError)
    expect(() => parseConfigText('')).toThrow(/空/)
    expect(() => parseConfigText('- a\n- b')).toThrow(FileError)
  })
})

describe('保存の前の差分', () => {
  it('変えた割り当てだけを、人が読める場所の名前で出す', () => {
    const a = sampleConfig()
    const b = sampleConfig()
    b.layers[0].keys!.KEY_A = { key: 'LCTRL+Y' }
    delete b.layers[1].keys!.KEY_Q
    b.layers[2].touch!.cells!['0,0'].label = 'ズーム'
    b.layers[0].keys!.KEY_1 = { key: 'F1' }
    const d = diffConfigs(a, b, keymap as KeymapInfo)
    expect(d).toContainEqual({ where: 'レイヤー「基本」 / キー A（KEY_A）', before: 'LCTRL+Z', after: 'LCTRL+Y' })
    expect(d).toContainEqual({ where: 'レイヤー「編集」 / キー Q（KEY_Q）', before: 'LCTRL+C', after: null })
    expect(d).toContainEqual({ where: 'レイヤー「表示」 / セル 0,0', before: 'LCTRL+KPPLUS「拡大」', after: 'LCTRL+KPPLUS「ズーム」' })
    expect(d).toContainEqual({ where: 'レイヤー「基本」 / キー 記号+Q（KEY_1）', before: null, after: 'F1' })
    expect(d).toHaveLength(4)
    expect(diffConfigs(a, sampleConfig())).toEqual([])
  })
})
