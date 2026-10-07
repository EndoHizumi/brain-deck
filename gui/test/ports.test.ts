import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from '../src/app'
import { FakeDaemon, FakeTransport } from '../src/demo'
import { PortRoles, looksLikeConsole, looksLikeLefthand, looksLikePrompt } from '../src/ports'
import { ConsoleTransport, sampleConfig, testFont, twoPortSerial } from './helpers'

beforeEach(() => vi.spyOn(console, 'debug').mockImplementation(() => {}))
afterEach(() => vi.restoreAllMocks())

describe('PortRoles', () => {
  const a = {} as SerialPort
  const b = {} as SerialPort

  it('確かめた役割のポートを選ぶ。分からなければ選ばない', () => {
    const r = new PortRoles()
    expect(r.pick('settings', [a, b])).toBeNull()
    expect(r.pick('console', [a, b])).toBeNull()
    r.learn(a, 'settings')
    expect(r.pick('settings', [a, b])).toBe(a)
  })

  it('2 つのうち一方が分かれば、もう一方はその逆', () => {
    const r = new PortRoles()
    r.learn(a, 'settings')
    expect(r.pick('console', [a, b])).toBe(b)
    const r2 = new PortRoles()
    r2.learn(b, 'console')
    expect(r2.pick('settings', [a, b])).toBe(a)
    // 3 つ以上（Brain を 2 台つないだ）なら、推しはからない
    expect(r2.pick('settings', [a, b, {} as SerialPort])).toBeNull()
  })

  it('もう一方のタブで使っているポートは選ばない。抜いたポートは忘れる', () => {
    const r = new PortRoles()
    r.learn(a, 'settings')
    r.use(b, 'settings') // 確かめている途中
    expect(r.pick('console', [a, b])).toBeNull()
    expect(r.conflict(b, 'console')).toContain('設定のタブで使っています')
    expect(r.conflict(a, 'console')).toContain('設定用です')
    expect(r.conflict(b, 'settings')).toBeNull()
    r.forget(a)
    expect(r.pick('settings', [a, b])).toBeNull()
  })

  it('届いた文字が lefthand か、コンソールか', () => {
    expect(looksLikeLefthand('{"id":null,"ok":false,"error":{"code":"parse_error","message":"x"}}\n')).toBe(true)
    expect(looksLikeLefthand('{"event":"input","code":"KEY_Q"}\n')).toBe(true)
    expect(looksLikeLefthand('\r\nBrainux 6.1 (ttyGS0)\r\n\r\nbrain login: ')).toBe(false)
    expect(looksLikeConsole('\r\nbrain login: ')).toBe(true)
    expect(looksLikeConsole('user@brain:~$ ')).toBe(true)
    expect(looksLikeConsole('{"id":3,"ok":tr')).toBe(false) // 途中で切れた JSON
    expect(looksLikeConsole('')).toBe(false)
    // 役割を覚えるのは、プロンプトが見えたときだけ（lefthand の返事の切れ端では覚えない）
    expect(looksLikePrompt('\r\nbrain login: ')).toBe(true)
    expect(looksLikePrompt('Password: ')).toBe(true)
    expect(looksLikePrompt('user@brain:~$ ')).toBe(true)
    expect(looksLikePrompt('ok":true,"result":{"x":1}}\n')).toBe(false)
    expect(looksLikePrompt('Brainux 6.1')).toBe(false)
  })
})

function setup(opts: { consoleGreeting?: string } = {}) {
  const root = document.createElement('div')
  document.body.replaceChildren(root)
  const daemon = new FakeDaemon(sampleConfig())
  const s = twoPortSerial()
  const consoles: ConsoleTransport[] = []
  const settings: FakeTransport[] = []
  const app = new App(root, {
    serial: s.serial,
    openTransport: async (p) => {
      if (p === s.consolePort) {
        const t = new ConsoleTransport(opts.consoleGreeting)
        consoles.push(t)
        return t
      }
      const t = new FakeTransport(daemon)
      settings.push(t)
      return t
    },
    loadFont: async () => testFont(),
    confirm: () => true,
    validateDelayMs: 5,
    helloTimeoutMs: 100,
    keepaliveMs: 1000,
    sniffMs: 20,
  })
  const consoleWrites = () => consoles.reduce((n, t) => n + t.sent.length, 0)
  return { app, root, daemon, s, consoles, settings, consoleWrites }
}

describe('設定のタブ：ポートの見分け方', () => {
  it('どちらか分からないポートには書かず、一覧で選んでもらう。分かったあとは聞かない', async () => {
    const t = setup()
    await t.app.connect()
    expect(t.app.connected).toBe(true)
    expect(t.s.state.requests).toBe(1)
    expect(t.consoleWrites()).toBe(0)
    expect(t.app.ports.role(t.s.settingsPort)).toBe('settings')

    await t.app.disconnect()
    await t.app.connect()
    expect(t.app.connected).toBe(true)
    expect(t.s.state.requests).toBe(1) // 聞き直さない
    expect(t.consoles).toHaveLength(0) // コンソール用のポートは開いてもいない
  })

  it('許可済みのポートがあっても、役割が分からなければ自動では hello を送らない（ページを開き直したとき）', async () => {
    const t = setup()
    await t.app.connect() // 許可する
    // ページを開き直したことにする（役割は覚えていない）
    const t2 = { ...t, app: new App(t.root, {
      serial: t.s.serial,
      openTransport: async (p) => (p === t.s.consolePort ? (t.consoles.push(new ConsoleTransport()), t.consoles.at(-1)!) : new FakeTransport(t.daemon)),
      loadFont: async () => testFont(), confirm: () => true, helloTimeoutMs: 100, sniffMs: 20 }) }
    t.s.state.pick = null // 一覧を閉じた
    await t2.app.connect()
    expect(t2.app.connected).toBe(false)
    expect(t.s.state.requests).toBe(2)
    expect(t.consoleWrites()).toBe(0)
    expect(t.root.textContent).toContain('どちらが設定用か見分けられません')
  })

  it('一覧でコンソール用を選び、ログイン画面の文字が届いたら、何も送らずに伝える', async () => {
    const t = setup({ consoleGreeting: '\r\nBrainux (ttyGS0)\r\n\r\nbrain login: ' })
    t.s.state.pick = t.s.consolePort
    await t.app.connect()
    expect(t.app.connected).toBe(false)
    expect(t.consoleWrites()).toBe(0)
    expect(t.root.textContent).toContain('コンソール用のポートかもしれないので、何も送っていません')
    expect(t.app.ports.role(t.s.consolePort)).toBe('console')

    // コンソール用だと分かったので、次は一覧を出さずに、もう一方を使う
    await t.app.connect()
    expect(t.app.connected).toBe(true)
    expect(t.s.state.requests).toBe(1)
    expect(t.consoleWrites()).toBe(0)
  })

  it('lefthand の返事の切れ端が残っていても、コンソール用とは覚えない（送らずに断るだけ）', async () => {
    const t = setup()
    const s2 = t.s
    // 設定用のポートに、前の接続の返事の切れ端が残っている
    const app = new App(t.root, {
      serial: s2.serial,
      openTransport: async () => {
        const c = new ConsoleTransport('ok":true,"result":{}}\n')
        t.consoles.push(c)
        return c
      },
      loadFont: async () => testFont(), confirm: () => true, helloTimeoutMs: 100, sniffMs: 20,
    })
    await app.connect()
    expect(app.connected).toBe(false)
    expect(t.consoleWrites()).toBe(0)
    expect(app.ports.role(s2.settingsPort)).toBeUndefined()
  })

  it('コンソール用だと分かっているポートを一覧で選ぶと、開かずに断る', async () => {
    const t = setup()
    t.app.ports.learn(t.s.consolePort, 'console')
    t.s.state.pick = t.s.consolePort
    // 許可がまだない（getPorts が空）ので、一覧を出す
    await t.app.connect()
    expect(t.consoles).toHaveLength(0)
    expect(t.root.textContent).toContain('そのポートはコンソール用です')
  })

  it('ケーブルを抜くと、覚えた役割を忘れる', async () => {
    const t = setup()
    await t.app.connect()
    t.settings[0].unplug()
    t.s.unplug(t.s.settingsPort)
    expect(t.app.ports.role(t.s.settingsPort)).toBeUndefined()
  })
})
