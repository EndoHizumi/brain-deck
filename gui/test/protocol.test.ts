import { afterEach, describe, expect, it, vi } from 'vitest'
import { Client, LineFramer, ProtocolError, type Transport } from '../src/protocol'

const enc = new TextEncoder()

describe('LineFramer', () => {
  it('チャンクの境目で分かれた行と、UTF-8 の文字をつなぐ', () => {
    const lines: string[] = []
    const f = new LineFramer((l) => lines.push(l))
    const bytes = enc.encode('{"a":"ブラシ"}\n\n{"b":2}\r\n{"c"')
    for (const b of bytes) f.push(new Uint8Array([b])) // 1 バイトずつ
    f.push(enc.encode(':3}\n'))
    expect(lines).toEqual(['{"a":"ブラシ"}', '{"b":2}', '{"c":3}'])
  })

  it('上限を超えた行は捨てて、次の行から続ける', () => {
    const lines: string[] = []
    let over = 0
    const f = new LineFramer((l) => lines.push(l), () => over++, 8)
    f.push(enc.encode('0123456789'))
    f.push(enc.encode('abc\nok\n'))
    expect(over).toBe(1)
    expect(lines).toEqual(['ok'])
  })
})

// pipe はリクエストを受け取り、reply が返す行をチャンクに分けて返すトランスポート。
function pipe(reply: (req: any) => string | string[] | null, chunk = 5) {
  const sent: string[] = []
  const t: Transport & { push(s: string): void } = {
    onData: () => {},
    onClose: () => {},
    async send(b) {
      for (const line of new TextDecoder().decode(b).split('\n')) {
        if (!line) continue
        sent.push(line)
        const r = reply(JSON.parse(line))
        for (const s of r === null ? [] : Array.isArray(r) ? r : [r]) t.push(s + '\n')
      }
    },
    push(s: string) {
      const b = enc.encode(s)
      for (let i = 0; i < b.length; i += chunk) {
        const part = b.slice(i, i + chunk)
        queueMicrotask(() => t.onData(part))
      }
    },
    async close() {},
  }
  return { t, sent }
}

afterEach(() => vi.useRealTimers())

describe('Client', () => {
  it('id で対応づけ、順番が入れ替わった返事も受け取る', async () => {
    const held: any[] = []
    const { t } = pipe((req) => {
      held.push(req)
      if (held.length < 2) return null
      // 2 つ目を先に返す
      return held.reverse().map((r) => JSON.stringify({ id: r.id, ok: true, result: { cmd: r.cmd } }))
    })
    const c = new Client(t)
    const [a, b] = await Promise.all([c.request('hello'), c.request('get_status')])
    expect(a).toEqual({ cmd: 'hello' })
    expect(b).toEqual({ cmd: 'get_status' })
  })

  it('始めに空行を送り、前の接続の切れ端と混ざらないようにする', async () => {
    const raw: Uint8Array[] = []
    const t: Transport = { onData: () => {}, onClose: () => {}, send: async (b) => void raw.push(b), close: async () => {} }
    await new Client(t).start()
    expect(new TextDecoder().decode(raw[0])).toBe('\n')
  })

  it('通知と、id のない行を分ける', async () => {
    const { t } = pipe((req) => [
      JSON.stringify({ event: 'input', type: 'key', code: 'KEY_Q', layer: 'base' }),
      'login: ', // コンソールのポートだったときなど
      JSON.stringify({ id: null, ok: false, error: { code: 'too_large', message: 'x' } }),
      JSON.stringify({ id: req.id, ok: true, result: 1 }),
    ])
    const c = new Client(t)
    const notes: any[] = []
    const stray: string[] = []
    c.onNotify = (n) => notes.push(n)
    c.onStray = (s) => stray.push(s)
    expect(await c.request('hello')).toBe(1)
    expect(notes).toEqual([{ event: 'input', type: 'key', code: 'KEY_Q', layer: 'base' }])
    expect(stray).toEqual(['login: ', 'too_large: x'])
  })

  it('エラーの返事は、場所付きの ProtocolError になる', async () => {
    const problems = [{ path: '/layers/0/keys/KEY_Q', message: 'bad' }]
    const { t } = pipe((req) => JSON.stringify({ id: req.id, ok: false, error: { code: 'invalid_config', message: 'm', problems } }))
    const err = await new Client(t).request('set_config', { config: {} }).catch((e) => e)
    expect(err).toBeInstanceOf(ProtocolError)
    expect(err.code).toBe('invalid_config')
    expect(err.problems).toEqual(problems)
  })

  it('返事がなければ時間切れになる', async () => {
    vi.useFakeTimers()
    const { t } = pipe(() => null)
    const p = new Client(t, { timeoutMs: 100 }).request('hello').catch((e) => e)
    await vi.advanceTimersByTimeAsync(150)
    expect((await p).code).toBe('timeout')
  })

  it('デーモンの上限を超えるリクエストは送らない', async () => {
    const { t, sent } = pipe(() => null)
    const c = new Client(t, { maxLine: 100 })
    const err = await c.request('validate', { text: 'x'.repeat(200) }).catch((e) => e)
    expect(err.code).toBe('too_large')
    expect(sent).toEqual([])
  })

  it('切れたら、待っているリクエストはすべて失敗する', async () => {
    const { t } = pipe(() => null)
    const c = new Client(t)
    let closed = ''
    c.onClose = (r) => (closed = r)
    const p = c.request('hello').catch((e) => e)
    t.onClose('serial error: The device has been lost.')
    expect((await p).code).toBe('closed')
    expect(closed).toContain('lost')
    expect((await c.request('hello').catch((e) => e)).code).toBe('closed')
  })
})
