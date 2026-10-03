// lefthand のシリアルプロトコル（docs/protocol.md）。
// 1 行に 1 つの JSON。リクエストには id を付け、レスポンスは同じ id で返る。
// id のない行（event）は通知。トランスポート（WebSerial、テストのモック）とは切り離してある。

import type { Notification } from './types'

export const PROTOCOL_VERSION = 1
// 受け取る 1 行の上限。デーモンの応答は大きな設定でも数百 KB に収まる
const MAX_RECV_LINE = 4 << 20

// LineFramer はバイト列を行に分ける。UTF-8 の文字がチャンクの境目で分かれても壊れない。
export class LineFramer {
  private parts: Uint8Array[] = []
  private size = 0
  private discarding = false
  private dec = new TextDecoder('utf-8', { fatal: false })

  constructor(
    private onLine: (line: string) => void,
    private onOverflow: () => void = () => {},
    private max = MAX_RECV_LINE,
  ) {}

  push(chunk: Uint8Array): void {
    let start = 0
    for (;;) {
      const nl = chunk.indexOf(0x0a, start)
      const end = nl < 0 ? chunk.length : nl
      if (!this.discarding && end > start) {
        if (this.size + (end - start) > this.max) {
          this.discarding = true
          this.parts = []
          this.size = 0
          this.onOverflow()
        } else {
          this.parts.push(chunk.subarray(start, end))
          this.size += end - start
        }
      }
      if (nl < 0) return
      if (!this.discarding) {
        const buf = new Uint8Array(this.size)
        let o = 0
        for (const p of this.parts) {
          buf.set(p, o)
          o += p.length
        }
        const line = this.dec.decode(buf).replace(/\r$/, '')
        if (line.trim() !== '') this.onLine(line)
      }
      this.parts = []
      this.size = 0
      this.discarding = false
      start = nl + 1
    }
  }

  reset(): void {
    this.parts = []
    this.size = 0
    this.discarding = false
  }
}

// Transport は 1 行（改行なし）を送り、届いたバイト列を onData に渡す。
export interface Transport {
  send(bytes: Uint8Array): Promise<void>
  onData: (chunk: Uint8Array) => void
  onClose: (reason: string) => void
  close(): Promise<void>
}

export class ProtocolError extends Error {
  constructor(
    public code: string,
    message: string,
    public problems: { path: string; message: string }[] = [],
  ) {
    super(message)
    this.name = 'ProtocolError'
  }
}

interface Pending {
  resolve: (v: any) => void
  reject: (e: Error) => void
  timer: ReturnType<typeof setTimeout>
}

export interface ClientOptions {
  timeoutMs?: number
  maxLine?: number // hello で分かったデーモン側の上限
}

// Client はリクエストを送り、同じ id のレスポンスを待つ。
export class Client {
  private nextId = 1
  private pending = new Map<number, Pending>()
  private framer: LineFramer
  private enc = new TextEncoder()
  private writing: Promise<void> = Promise.resolve()
  closed = false
  timeoutMs: number
  maxLine: number
  onNotify: (n: Notification) => void = () => {}
  // id のないエラー（壊れた行、大きすぎる行など）や、読めない行
  onStray: (msg: string) => void = () => {}
  onClose: (reason: string) => void = () => {}

  constructor(
    private t: Transport,
    opts: ClientOptions = {},
  ) {
    this.timeoutMs = opts.timeoutMs ?? 5000
    this.maxLine = opts.maxLine ?? 256 * 1024
    this.framer = new LineFramer(
      (l) => this.onLine(l),
      () => this.onStray('received a line that is too long; discarded'),
    )
    t.onData = (c) => this.framer.push(c)
    t.onClose = (reason) => this.shutdown(reason)
  }

  // start は、前の接続の切れ端が残っていても混ざらないよう、空行を送って区切る。
  async start(): Promise<void> {
    await this.write(this.enc.encode('\n'))
  }

  request<T = any>(cmd: string, params: Record<string, unknown> = {}, timeoutMs = this.timeoutMs): Promise<T> {
    if (this.closed) return Promise.reject(new ProtocolError('closed', 'not connected'))
    const id = this.nextId++
    const line = JSON.stringify({ ...params, id, cmd })
    const bytes = this.enc.encode(line + '\n')
    if (bytes.length - 1 > this.maxLine) {
      return Promise.reject(
        new ProtocolError('too_large', `request is ${bytes.length} bytes; the limit is ${this.maxLine}`),
      )
    }
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new ProtocolError('timeout', `no reply to ${cmd} within ${timeoutMs} ms`))
      }, timeoutMs)
      this.pending.set(id, { resolve, reject, timer })
      this.write(bytes).catch((e) => {
        clearTimeout(timer)
        this.pending.delete(id)
        reject(new ProtocolError('write_failed', String(e?.message ?? e)))
      })
    })
  }

  // 書き込みは 1 つずつ順に行い、行が混ざらないようにする。
  private write(bytes: Uint8Array): Promise<void> {
    const p = this.writing.then(() => this.t.send(bytes))
    this.writing = p.catch(() => {})
    return p
  }

  private onLine(line: string): void {
    let m: any
    try {
      m = JSON.parse(line)
    } catch {
      // コンソール用のポート（getty）につないだときなど
      this.onStray(line)
      return
    }
    if (m === null || typeof m !== 'object') {
      this.onStray(line)
      return
    }
    if (typeof m.event === 'string') {
      this.onNotify(m as Notification)
      return
    }
    const p = typeof m.id === 'number' ? this.pending.get(m.id) : undefined
    if (!p) {
      this.onStray(m.error ? `${m.error.code}: ${m.error.message}` : line)
      return
    }
    this.pending.delete(m.id)
    clearTimeout(p.timer)
    if (m.ok) p.resolve(m.result)
    else p.reject(new ProtocolError(m.error?.code ?? 'error', m.error?.message ?? 'error', m.error?.problems ?? []))
  }

  private shutdown(reason: string): void {
    if (this.closed) return
    this.closed = true
    for (const [, p] of this.pending) {
      clearTimeout(p.timer)
      p.reject(new ProtocolError('closed', reason))
    }
    this.pending.clear()
    this.onClose(reason)
  }

  async close(): Promise<void> {
    this.shutdown('closed by user')
    await this.t.close()
  }
}
