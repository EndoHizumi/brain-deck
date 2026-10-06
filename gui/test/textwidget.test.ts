import { describe, expect, it } from 'vitest'
import { TEXT_ID_PATTERN, textExpired, textInk, textLayout } from '../src/textwidget'
import { repoFile, testFont } from './helpers'

describe('テキストのタイル', () => {
  const font = testFont()

  // text_test.go の textLayoutTable が Go で書いた結果（LEFTHAND_UPDATE_TEXTLAYOUT=1 go test -run TextLayoutTable）
  const table = JSON.parse(repoFile('gui/test/fixtures/textlayout.json').toString('utf8')) as
    { text: string; w: number; h: number; lines: string[]; scale: number }[]
  it('Go の textLayout と同じように折り返す', () => {
    const bad = table.filter((c) => {
      const got = textLayout(font, c.text, c.w, c.h)
      return got.scale !== c.scale || JSON.stringify(got.lines) !== JSON.stringify(c.lines)
    }).map((c) => `${JSON.stringify(c.text)} ${c.w}x${c.h}: ${JSON.stringify(textLayout(font, c.text, c.w, c.h))}`)
    expect(bad).toEqual([])
    expect(table.length).toBeGreaterThan(50)
  })

  it('期限が切れると薄い色になる', () => {
    const e = { text: 'x', style: 'ok' as const, set_at: '2026-10-06T00:00:00Z', expires_at: '2026-10-06T00:10:00Z' }
    expect(textExpired(e, new Date('2026-10-06T00:09:59Z'))).toBe(false)
    expect(textExpired(e, new Date('2026-10-06T00:10:00Z'))).toBe(true)
    expect(textInk('ok', false)).toEqual([0x50, 0xd8, 0x80])
    expect(textInk('ok', true)).toEqual([0x2e, 0x5a, 0x44])
    expect(textInk('unknown', false)).toEqual([0xff, 0xff, 0xff])
  })

  it('id の形は text.go と同じ', () => {
    expect(TEXT_ID_PATTERN.test('build.main-1_x')).toBe(true)
    expect(TEXT_ID_PATTERN.test('a b')).toBe(false)
    expect(TEXT_ID_PATTERN.test('x'.repeat(33))).toBe(false)
  })
})
