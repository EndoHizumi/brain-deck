import { describe, expect, it } from 'vitest'
import { ALL_KEYS, ComboCapture, MODIFIERS, formatCombo, keyFromCode, parseCombo, prettyCombo } from '../src/keys'
import { repoFile } from './helpers'
import { MEDIA_LABELS, MEDIA_OFF_LABEL } from '../src/model'

function capture(events: [string, string][]): string[] {
  const out: string[] = []
  const c = new ComboCapture((s) => out.push(s))
  for (const [t, code] of events) (t === 'down' ? c.keydown({ code }) : c.keyup({ code }))
  return out
}

describe('PC のキーボードからの取り込み', () => {
  it('Ctrl+Shift+Z', () => {
    expect(capture([['down', 'ControlLeft'], ['down', 'ShiftLeft'], ['down', 'KeyZ'], ['up', 'KeyZ'], ['up', 'ShiftLeft'], ['up', 'ControlLeft']]))
      .toEqual(['LCTRL+LSHIFT+Z'])
  })
  it('修飾キーだけ（押して離す）', () => {
    expect(capture([['down', 'ShiftLeft'], ['down', 'ControlRight'], ['up', 'ShiftLeft'], ['up', 'ControlRight']])).toEqual(['LSHIFT+RCTRL'])
  })
  it('配列によらず、押した位置のキーになる', () => {
    expect(keyFromCode('Digit1')).toBe('1')
    expect(keyFromCode('NumpadAdd')).toBe('KPPLUS')
    expect(keyFromCode('Numpad7')).toBe('KP7')
    expect(keyFromCode('BracketLeft')).toBe('LEFTBRACE')
    expect(keyFromCode('F12')).toBe('F12')
    expect(keyFromCode('F13')).toBe('F13')
    expect(keyFromCode('F24')).toBe('F24')
    expect(keyFromCode('F25')).toBeNull()
    expect(keyFromCode('IntlYen')).toBeNull() // 送れないキー
  })
  it('送れないキーは知らせる', () => {
    const bad: string[] = []
    new ComboCapture(() => {}, (c) => bad.push(c)).keydown({ code: 'IntlRo' })
    expect(bad).toEqual(['IntlRo'])
  })
})

describe('ショートカットの組み立て', () => {
  it('分解と組み立て', () => {
    expect(parseCombo('lshift+lctrl+z')).toEqual({ mods: ['LSHIFT', 'LCTRL'], keys: ['Z'] })
    expect(formatCombo({ mods: ['LSHIFT', 'LCTRL'], keys: ['Z'] })).toBe('LCTRL+LSHIFT+Z')
    expect(parseCombo('LCTRL+NOPE')).toBeNull()
  })
  it('画面の表示は main.go の prettyCombo と同じ', () => {
    expect(prettyCombo('LCTRL+KPPLUS')).toBe('Ctrl++')
    expect(prettyCombo('LCTRL+LSHIFT+Z')).toBe('Ctrl+Shift+Z')
  })
})

describe('送れるキーの名前', () => {
  // fixtures/keynames.json は Go の hidUsage と modBits から作る（hid_test.go の TestKeyNamesFixture）
  it('Go と同じ', () => {
    const want = JSON.parse(repoFile('gui/test/fixtures/keynames.json').toString('utf8')) as { keys: string[]; modifiers: string[] }
    expect([...ALL_KEYS].sort()).toEqual(want.keys)
    expect([...MODIFIERS].sort()).toEqual(want.modifiers)
  })
  it('メディアキーの名前と、画面に出す名前も Go と同じ', () => {
    const want = JSON.parse(repoFile('gui/test/fixtures/keynames.json').toString('utf8')) as { media: Record<string, string>; media_order: string[]; media_off: string }
    expect(MEDIA_LABELS).toEqual(want.media)
    expect(Object.keys(MEDIA_LABELS)).toEqual(want.media_order)
    expect(MEDIA_OFF_LABEL).toBe(want.media_off)
  })
})
