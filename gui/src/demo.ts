// デモ用の簡易デーモンと、WebSerial のモック。
// Brain がなくても GUI を試せるようにする（?demo を付けて開く）。テストでも使う。
// 検証は lefthand の一部だけをまねたもので、本物の検証は Brain に接続して行う。

import { ALL_KEYS, MODIFIERS } from './keys'
import keymapJSON from './keymap-pwsh2.json'
import { LAYER_KINDS, actionKind, actionTarget, parseCellKey } from './model'
import type { Transport } from './protocol'
import type { ActionSpec, Config, KeymapInfo, Problem } from './types'

const keymap = keymapJSON as KeymapInfo
const SOURCE_KEYS = new Set(keymap.keys.flatMap((k) => [k.code, k.symbol]).filter(Boolean) as string[])

export class FakeDaemon {
  config: Config
  path = '/etc/lefthand/config.yaml'
  stack: { layer: string; kind: string }[] = []
  subscribed = false
  suppress = false
  received: any[] = [] // 受け取ったリクエスト（テスト用）
  failApply = false // true なら set_config の反映に失敗したことにする（テスト用）
  // 通知を送る先（FakeTransport が設定する）
  emit: (line: string) => void = () => {}

  constructor(config: Config) {
    this.config = structuredClone(config)
  }

  status() {
    const top = this.stack.length ? this.stack[this.stack.length - 1].layer : this.config.layers[0].name
    const l = this.config.layers.find((x) => x.name === top) ?? this.config.layers[0]
    const g = l.touch?.cols ? l.touch : this.config.layers[0].touch
    const mode = !this.stack.length ? 'base' : ['layer_hold', 'layer_oneshot'].includes(this.stack[this.stack.length - 1].kind) ? 'temp' : 'latched'
    return { layer: l.name, label: l.label || l.name, mode, stack: this.stack, cols: g?.cols ?? 0, rows: g?.rows ?? 0 }
  }

  // handle は 1 行を処理し、返す行を返す。
  handle(line: string): string {
    let req: any
    try {
      req = JSON.parse(line)
    } catch {
      return JSON.stringify({ id: null, ok: false, error: { code: 'parse_error', message: 'invalid JSON' } })
    }
    this.received.push(req)
    const id = req.id
    const ok = (result: unknown) => JSON.stringify({ id, ok: true, result })
    const err = (code: string, message: string, problems?: Problem[]) =>
      JSON.stringify({ id, ok: false, error: { code, message, problems } })
    switch (req.cmd) {
      case 'hello':
        return ok({ protocol: 1, daemon: 'lefthand', version: 'demo', max_line: 262144, config_path: this.path, commands: [] })
      case 'get_config':
        return ok({ config: this.config, path: this.path })
      case 'get_keymap':
        return ok(keymap)
      case 'get_status':
        return ok({ status: this.status(), uptime_sec: 1, subscribed: this.subscribed, suppressing: this.suppress })
      case 'validate': {
        const p = validate(req.config)
        return ok({ valid: p.length === 0, errors: p, warnings: [], config: p.length ? undefined : req.config })
      }
      case 'set_config': {
        const p = validate(req.config)
        if (p.length) return err('invalid_config', 'the config is not valid; nothing was changed', p)
        if (this.failApply) return err('apply_failed', 'apply failed: simulated: rolled back to the previous config')
        this.config = structuredClone(req.config)
        this.stack = []
        return ok({ saved: this.path, previous: this.path + '.prev', warnings: [], status: this.status() })
      }
      case 'subscribe_input':
        this.subscribed = req.enable !== false
        this.suppress = this.subscribed && !!req.suppress
        return ok({ subscribed: this.subscribed, suppress: this.suppress, lease_sec: 30 })
      default:
        return err('unknown_command', `unknown command ${JSON.stringify(req.cmd)}`)
    }
  }

  // pressKey / touch は、Brain で押したことにする（学習モードの通知）。
  pressKey(code: string): void {
    if (this.subscribed) this.emit(JSON.stringify({ event: 'input', type: 'key', code, layer: this.status().layer, suppressed: this.suppress }))
  }

  touch(x: number, y: number, soft?: string): void {
    if (this.subscribed) this.emit(JSON.stringify({ event: 'input', type: 'touch', x, y, soft, layer: this.status().layer, suppressed: this.suppress }))
  }

  setLayer(name: string | null, kind = 'layer_toggle'): void {
    this.stack = name ? [{ layer: name, kind }] : []
    this.emit(JSON.stringify({ event: 'layer', ...this.status() }))
  }
}

// validate は lefthand の検証のうち、よく起きるものだけをまねる。
export function validate(cfg: Config): Problem[] {
  const out: Problem[] = []
  if (!cfg?.layers?.length) return [{ path: '/layers', message: 'no layers' }]
  const names = new Set<string>()
  cfg.layers.forEach((l, i) => {
    if (!l.name) out.push({ path: `/layers/${i}/name`, message: `layers[${i}]: name is required` })
    else if (names.has(l.name)) out.push({ path: `/layers/${i}/name`, message: `layer "${l.name}" is defined twice` })
    names.add(l.name)
  })
  const action = (path: string, where: string, a: ActionSpec) => {
    const n = (['key', ...LAYER_KINDS] as const).filter((k) => a[k]).length
    if (n !== 1) return out.push({ path, message: `${where}: write exactly one of key, layer_hold, layer_toggle, layer_oneshot, layer_to` })
    const k = actionKind(a)
    if (k === 'key') {
      for (const p of a.key!.split('+')) {
        const u = p.trim().toUpperCase()
        if (!ALL_KEYS.has(u) && !(MODIFIERS as readonly string[]).includes(u))
          out.push({ path, message: `${where}: unknown key "${u}" in "${a.key}"` })
      }
    } else if (k !== 'none') {
      const t = actionTarget(a)!
      if (!names.has(t)) out.push({ path, message: `${where}: ${k} refers to unknown layer "${t}"` })
      else if (k === 'layer_toggle' && t === cfg.layers[0].name)
        out.push({ path, message: `${where}: layer_toggle cannot target the base layer "${t}" (use layer_to)` })
    }
  }
  const base = cfg.layers[0].touch
  cfg.layers.forEach((l, i) => {
    for (const [code, a] of Object.entries(l.keys ?? {})) {
      const where = `layer "${l.name}" key ${code}`
      if (!SOURCE_KEYS.has(code) && !/^KEY_[A-Z0-9_]+$/.test(code)) out.push({ path: `/layers/${i}/keys/${code}`, message: `${where}: unknown source key` })
      else action(`/layers/${i}/keys/${code}`, where, a)
    }
    if (l.touch) {
      const cols = l.touch.cols ?? base?.cols ?? 0
      const rows = l.touch.rows ?? base?.rows ?? 0
      if (cols < 1 || rows < 1 || cols > 16 || rows > 16)
        out.push({ path: `/layers/${i}/touch`, message: `layer "${l.name}" touch: cols and rows must be 1..16` })
      for (const [k, a] of Object.entries(l.touch.cells ?? {})) {
        const p = parseCellKey(k)
        const where = `layer "${l.name}" touch cell "${k}"`
        if (!p || p[0] >= cols || p[1] >= rows) out.push({ path: `/layers/${i}/touch/cells/${k}`, message: `${where}: out of the ${cols}x${rows} grid` })
        else action(`/layers/${i}/touch/cells/${k}`, where, a)
      }
    }
    for (const [n, a] of Object.entries(l.soft_keys ?? {})) {
      const where = `layer "${l.name}" soft key "${n}"`
      if (!cfg.touch?.soft_areas?.[n]) out.push({ path: `/layers/${i}/soft_keys/${n}`, message: `${where}: not defined in touch.soft_areas` })
      else action(`/layers/${i}/soft_keys/${n}`, where, a)
    }
  })
  return out
}

// FakeTransport は FakeDaemon とつなぐトランスポート。返事は小さなチャンクに分けて、非同期に届ける
// （本物のシリアルと同じく、行の途中で切れて届くことがある）。
export class FakeTransport implements Transport {
  onData: (chunk: Uint8Array) => void = () => {}
  onClose: (reason: string) => void = () => {}
  sent: string[] = []
  private enc = new TextEncoder()
  private dec = new TextDecoder()
  private buf = ''
  closed = false

  constructor(
    public daemon: FakeDaemon,
    private chunkSize = 7,
    private delayMs = 1,
  ) {
    daemon.emit = (line) => this.deliver(line)
  }

  async send(bytes: Uint8Array): Promise<void> {
    if (this.closed) throw new Error('closed')
    this.buf += this.dec.decode(bytes, { stream: true })
    let i: number
    while ((i = this.buf.indexOf('\n')) >= 0) {
      const line = this.buf.slice(0, i)
      this.buf = this.buf.slice(i + 1)
      if (!line.trim()) continue
      this.sent.push(line)
      this.deliver(this.daemon.handle(line))
    }
  }

  private deliver(line: string): void {
    const b = this.enc.encode(line + '\n')
    for (let o = 0; o < b.length; o += this.chunkSize) {
      const part = b.slice(o, o + this.chunkSize)
      setTimeout(() => !this.closed && this.onData(part), this.delayMs)
    }
  }

  // unplug はケーブルを抜いたことにする。
  unplug(): void {
    this.closed = true
    this.onClose('serial error: The device has been lost.')
  }

  async close(): Promise<void> {
    this.closed = true
  }
}

// fakeSerial は navigator.serial の代わり。ポートを 1 つだけ持つ。
export function fakeSerial(): { serial: Serial; port: SerialPort } {
  const port = {
    getInfo: () => ({ usbVendorId: 0x1d6b, usbProductId: 0x0104 }),
  } as unknown as SerialPort
  let granted = false
  const serial = {
    getPorts: async () => (granted ? [port] : []),
    requestPort: async () => {
      granted = true
      return port
    },
    addEventListener: () => {},
    removeEventListener: () => {},
  } as unknown as Serial
  return { serial, port }
}
