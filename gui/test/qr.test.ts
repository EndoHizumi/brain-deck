import { describe, expect, it } from 'vitest'
import { encodeQR } from '../src/qr'
import { repoFile } from './helpers'

// qr.go と同じ模様になること。表は `LEFTHAND_UPDATE_QRCODES=1 go test -run QRCodesTable` で Go が書いたもの
// （Project Nayuki の Python 版でも同じ模様になることを確かめてある）
describe('QR コード', () => {
  const table = JSON.parse(repoFile('gui/test/fixtures/qrcodes.json').toString('utf8')) as
    { text: string; version: number; ecl: number; mask: number; rows: string[] }[]
  for (const c of table) {
    it(`Go と同じ模様（${JSON.stringify(c.text.slice(0, 24))}）`, () => {
      const q = encodeQR(c.text)!
      expect([q.version, q.ecl, q.mask]).toEqual([c.version, c.ecl, c.mask])
      const rows: string[] = []
      for (let y = 0; y < q.size; y++) {
        let r = ''
        for (let x = 0; x < q.size; x++) r += q.dark(x, y) ? '1' : '0'
        rows.push(r)
      }
      expect(rows).toEqual(c.rows)
    })
  }
  it('長すぎる文字列は null', () => {
    expect(encodeQR('a'.repeat(3000))).toBeNull()
  })
})
