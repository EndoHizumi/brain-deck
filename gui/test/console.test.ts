import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from '../src/app'
import { isTerminalReport, type TermLike } from '../src/console'
import { FakeDaemon, FakeTransport } from '../src/demo'
import { ConsoleTransport, sampleConfig, testFont, twoPortSerial } from './helpers'

// FakeTerm は xterm.js の代わり。書かれた文字を覚え、type で打ったことにする。
class FakeTerm implements TermLike {
  out = ''
  rows = 24
  cols = 80
  private data: (s: string) => void = () => {}
  opened = false
  open(): void {
    this.opened = true
  }
  write(s: string): void {
    this.out += s
  }
  onData(f: (s: string) => void): void {
    this.data = f
  }
  onBinary(): void {}
  fit() {
    return { rows: this.rows, cols: this.cols }
  }
  focus(): void {}
  dispose(): void {}
  type(s: string): void {
    this.data(s)
  }
}

beforeEach(() => vi.spyOn(console, 'debug').mockImplementation(() => {}))
afterEach(() => vi.restoreAllMocks())

const LOGIN = '\r\nDebian GNU/Linux 13 brain ttyGS0\r\n\r\nbrain login: '

function setup(opts: { greeting?: string } = {}) {
  const root = document.createElement('div')
  document.body.replaceChildren(root)
  const daemon = new FakeDaemon(sampleConfig())
  const s = twoPortSerial()
  const term = new FakeTerm()
  const consoles: ConsoleTransport[] = []
  const settings: FakeTransport[] = []
  const app = new App(root, {
    serial: s.serial,
    openTransport: async (p) => {
      if (p === s.consolePort) {
        const t = new ConsoleTransport(opts.greeting ?? LOGIN)
        consoles.push(t)
        return t
      }
      const t = new FakeTransport(daemon)
      settings.push(t)
      return t
    },
    createTerminal: async () => term,
    loadFont: async () => testFont(),
    confirm: () => true,
    validateDelayMs: 5,
    helloTimeoutMs: 100,
    keepaliveMs: 1000,
    sniffMs: 20,
  })
  const $ = <T extends HTMLElement = HTMLElement>(sel: string) => root.querySelector<T>(sel)!
  const click = (sel: string) => $(sel).click()
  const sent = () => consoles.at(-1)?.sentText ?? ''
  return { app, root, daemon, s, term, consoles, settings, $, click, sent }
}

async function openConsole(t = setup()) {
  t.s.state.pick = t.s.consolePort
  t.click('#section-console')
  await vi.waitFor(() => expect(t.term.opened).toBe(true))
  t.click('#console-connect')
  await vi.waitFor(() => expect(t.app.console.open).toBe(true))
  return t
}

describe('コンソールのタブ', () => {
  it('設定に接続していなくても開ける。一覧で選んだポートにつなぎ、自分からは何も送らない', async () => {
    const t = await openConsole()
    expect(t.s.state.requests).toBe(1)
    await vi.waitFor(() => expect(t.term.out).toContain('brain login: '))
    expect(t.sent()).toBe('')
    expect(t.$('#console-state').textContent).toContain('接続中')
    expect(t.$('#section-console').textContent).toContain('接続中')
    // ログイン画面が見えたので、コンソール用だと分かる
    expect(t.app.ports.role(t.s.consolePort)).toBe('console')
  })

  it('打った文字をそのまま送る', async () => {
    const t = await openConsole()
    t.term.type('user\r')
    t.term.type('\x03') // Ctrl+C
    await vi.waitFor(() => expect(t.sent()).toBe('user\r\x03'))
    t.term.type('echo 日本語\r')
    await vi.waitFor(() => expect(t.sent()).toContain('echo 日本語\r'))
  })

  it('たまっていた問い合わせへの端末の自動の報告は、ユーザーが打つまで送らない', async () => {
    const t = await openConsole()
    // getty の ESC[6n に、xterm.js がカーソル位置を返したことにする
    t.term.type('\x1b[4;1R')
    t.term.type('\x1b[30;136R\x1b[30;136R')
    t.term.type('\x1b[?1;2c')
    await new Promise((r) => setTimeout(r, 10))
    expect(t.sent()).toBe('')
    // 打ったあとの問い合わせ（getty の起動し直し、resize など）には答える
    t.term.type('u')
    t.term.type('\x1b[30;136R')
    await vi.waitFor(() => expect(t.sent()).toBe('u\x1b[30;136R'))
  })

  it('端末の自動の報告を見分ける', () => {
    for (const r of ['\x1b[4;1R', '\x1b[30;136R\x1b[30;136R', '\x1b[?1;2c', '\x1b[>0;276;0c', '\x1b[0n', '\x1b[8;24;80t', '\x1b]11;rgb:1010/1616/1f1f\x1b\\'])
      expect(isTerminalReport(r), JSON.stringify(r)).toBe(true)
    for (const k of ['a', '\r', '\x1b[A', '\x1b[1;5C', '\x1bOP', '\x1b', '\x03', 'ls\r', '\x1b[200~paste\x1b[201~'])
      expect(isTerminalReport(k), JSON.stringify(k)).toBe(false)
  })

  it('UTF-8 の文字が途中で切れて届いても、化けない', async () => {
    const t = await openConsole()
    const c = t.consoles[0]
    const bytes = new TextEncoder().encode('ファイル一覧：日本語.txt\r\n')
    // 1 バイトずつ届ける（3 バイトの文字の途中で切れる）
    for (const b of bytes) c.emit(new Uint8Array([b]))
    expect(t.term.out).toContain('ファイル一覧：日本語.txt\r\n')
    expect(t.term.out).not.toContain('�')
  })

  it('大きさは、ボタンを押したときだけ stty で送る', async () => {
    const t = await openConsole()
    t.term.rows = 30
    t.term.cols = 100
    t.app.console.fit()
    expect(t.$('#console-size').textContent).toContain('stty rows 30 cols 100')
    expect(t.$('#console-size-state').textContent).toContain('まだ伝えていません')
    expect(t.sent()).toBe('')
    t.click('#console-size')
    await vi.waitFor(() => expect(t.sent()).toBe('stty rows 30 cols 100\r'))
    await vi.waitFor(() => expect(t.$('#console-size-state').textContent).toContain('伝えてあります'))
    // 大きさが変わると、また「まだ」になる。自動では送らない
    t.term.cols = 90
    t.app.console.fit()
    expect(t.$('#console-size-state').textContent).toContain('まだ伝えていません')
    expect(t.sent()).toBe('stty rows 30 cols 100\r')
  })

  it('設定のタブと同時に開ける。コンソール用が分かれば、設定のタブは一覧を出さずにもう一方を使う', async () => {
    const t = await openConsole()
    await vi.waitFor(() => expect(t.app.ports.role(t.s.consolePort)).toBe('console'))
    await t.app.connect()
    expect(t.app.connected).toBe(true)
    expect(t.app.console.open).toBe(true)
    expect(t.s.state.requests).toBe(1)
    expect(t.sent()).toBe('') // コンソール用には hello を書いていない
    expect(t.settings).toHaveLength(1)
  })

  it('設定のタブが使っているポートは、コンソールのタブでは開かない', async () => {
    const t = setup()
    await t.app.connect() // 一覧で設定用を選ぶ
    t.s.state.pick = t.s.settingsPort // 間違えて設定用を選ぶ
    t.click('#section-console')
    await vi.waitFor(() => expect(t.term.opened).toBe(true))
    // 設定用の逆なので、一覧を出さずにコンソール用を使う
    t.click('#console-connect')
    await vi.waitFor(() => expect(t.app.console.open).toBe(true))
    expect(t.consoles).toHaveLength(1)
    expect(t.s.state.requests).toBe(1)
  })

  it('一覧で設定用を選び、lefthand の返事が届いたら、そう伝える', async () => {
    const t = setup({ greeting: '' })
    const t2 = { ...t }
    // コンソールのタブで、設定用のポートを選んだことにする（返事は lefthand の parse_error）
    const app = new App(t.root, {
      serial: t.s.serial,
      openTransport: async () => {
        const c = new ConsoleTransport()
        t.consoles.push(c)
        setTimeout(() => c.emit('{"id":null,"ok":false,"error":{"code":"parse_error","message":"invalid character"}}\n'), 5)
        return c
      },
      createTerminal: async () => t.term,
      loadFont: async () => testFont(),
    })
    t2.s.state.pick = t.s.settingsPort
    await app.console.connect()
    await vi.waitFor(() => expect(app.console.status?.text).toContain('設定用のポートです'))
    expect(app.ports.role(t.s.settingsPort)).toBe('settings')
    expect(t.term.out).toContain('これは設定用のポートです')
  })

  it('切断と、ケーブルを抜いたときの表示。前に選んだポートは、選び直さない', async () => {
    const t = await openConsole()
    t.click('#console-disconnect')
    await vi.waitFor(() => expect(t.app.console.open).toBe(false))
    expect(t.term.out).toContain('[切断しました]')
    expect(t.$('#console-status').textContent).toContain('切断しました')
    expect(t.term.out.match(/切断しました/g)).toHaveLength(1)

    t.click('#console-connect')
    await vi.waitFor(() => expect(t.app.console.open).toBe(true))
    expect(t.s.state.requests).toBe(1) // 選び直さない

    t.consoles.at(-1)!.lose()
    t.s.unplug(t.s.consolePort)
    expect(t.app.console.open).toBe(false)
    expect(t.$('#console-status').textContent).toContain('ケーブルが抜けたか')
    expect(t.term.out).toContain('ケーブルが抜けたか')
    expect(t.app.ports.role(t.s.consolePort)).toBeUndefined()
    // 抜いたあとは、もう一度一覧で選ぶ
    t.s.state.pick = null
    t.click('#console-connect')
    await vi.waitFor(() => expect(t.s.state.requests).toBe(2))
  })

  it('別のタブに切り替えても、つないだまま。端末は作り直さない', async () => {
    const t = await openConsole()
    const host = t.app.console.host
    t.click('#section-config')
    expect(host.hidden).toBe(true)
    expect(t.app.console.open).toBe(true)
    t.consoles[0].emit('バックグラウンドの出力\r\n')
    t.click('#section-console')
    expect(host.hidden).toBe(false)
    expect(t.app.console.host).toBe(host)
    expect(t.term.out).toContain('バックグラウンドの出力')
  })
})
