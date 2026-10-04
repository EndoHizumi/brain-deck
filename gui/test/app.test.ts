import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from '../src/app'
import { FakeDaemon, FakeTransport, fakeSerial } from '../src/demo'
import type { Transport } from '../src/protocol'
import { toYAML } from '../src/yamlio'
import { sampleConfig, testFont } from './helpers'

// Brain の代わりに、FakeDaemon につながったシリアルを使う
function setup(opts: { daemon?: FakeDaemon; transport?: () => Transport; confirm?: boolean } = {}) {
  const root = document.createElement('div')
  document.body.replaceChildren(root)
  const daemon = opts.daemon ?? new FakeDaemon(sampleConfig())
  let transport: FakeTransport | null = null
  const { serial } = fakeSerial()
  const font = testFont()
  const app = new App(root, {
    serial,
    openTransport: async () => opts.transport?.() ?? (transport = new FakeTransport(daemon)),
    loadFont: async () => font,
    confirm: () => opts.confirm ?? true,
    validateDelayMs: 5,
    helloTimeoutMs: 100,
    keepaliveMs: 20,
  })
  const $ = <T extends HTMLElement = HTMLElement>(sel: string) => root.querySelector<T>(sel)!
  const click = (sel: string) => $(sel).click()
  const change = (sel: string, value: string) => {
    const el = $<HTMLInputElement | HTMLSelectElement>(sel)
    el.value = value
    el.dispatchEvent(new Event('change'))
  }
  const cmds = () => daemon.received.map((r) => r.cmd)
  return { app, root, daemon, transport: () => transport!, $, click, change, cmds }
}

async function connected(t = setup()) {
  t.click('#connect')
  await vi.waitFor(() => expect(t.app.connected && t.app.cfg).toBeTruthy())
  await vi.waitFor(() => expect(t.app.validation).toBe('ok'))
  return t
}

beforeEach(() => vi.spyOn(console, 'debug').mockImplementation(() => {}))
afterEach(() => vi.restoreAllMocks())

describe('接続', () => {
  it('hello で確かめ、設定とキー配列を読み、検証する', async () => {
    const t = await connected()
    expect(t.cmds().slice(0, 4)).toEqual(['hello', 'get_keymap', 'get_config', 'get_status'])
    expect(t.cmds()).toContain('validate')
    expect(t.root.textContent).toContain('lefthand demo')
    expect(t.$('[data-code="KEY_Q"]').textContent).toContain('B')
    expect(t.$('[data-code="KEY_A"]').textContent).toContain('Ctrl+Z')
    // 前の接続の切れ端と混ざらないよう、最初に空行を送っている
    expect(t.transport().sent[0]).toContain('"hello"')
  })

  it('答えないポート（コンソール用）を選ぶと、別のポートを選ぶよう伝える', async () => {
    const silent: Transport = { onData: () => {}, onClose: () => {}, send: async () => {}, close: async () => {} }
    const t = setup({ transport: () => silent })
    t.click('#connect')
    await vi.waitFor(() => expect(t.root.textContent).toContain('lefthand が答えません'))
    expect(t.app.connected).toBe(false)
  })

  it('ポートを開けないときは、権限の手順を案内する（「設定用ではない」とは言わない）', async () => {
    const root = document.createElement('div')
    document.body.replaceChildren(root)
    const { serial } = fakeSerial()
    const app = new App(root, {
      serial,
      openTransport: async () => {
        throw new Error('Failed to open serial port.')
      },
      loadFont: async () => testFont(),
      helloTimeoutMs: 100,
    })
    await app.connect()
    expect(root.textContent).toContain('Failed to open serial port.')
    expect(root.textContent).toContain('dialout')
    expect(root.textContent).not.toContain('答えません')
    // 一度許可したポートでも同じ
    await app.connect()
    expect(root.textContent).toContain('dialout')
  })

  it('ケーブルが抜けても、編集中の内容は残る', async () => {
    const t = await connected()
    t.click('[data-code="KEY_Z"]')
    t.change('#kind', 'key')
    t.change('#combo-text', 'X')
    t.transport().unplug()
    expect(t.root.textContent).toContain('接続が切れました')
    expect(t.app.cfg!.layers[0].keys!.KEY_Z).toEqual({ key: 'X' })
    expect(t.$<HTMLButtonElement>('#save').disabled).toBe(true)
  })
})

describe('編集と保存', () => {
  it('キーに割り当て、差分を見てから保存すると、Brain に送られる', async () => {
    const t = await connected()
    t.click('[data-code="KEY_Z"]')
    t.change('#kind', 'key')
    t.change('#main-key', 'X')
    const ctrl = [...t.root.querySelectorAll<HTMLInputElement>('.mods input')][0]
    ctrl.checked = true
    ctrl.dispatchEvent(new Event('change'))
    expect(t.app.cfg!.layers[0].keys!.KEY_Z).toEqual({ key: 'LCTRL+X' })
    await vi.waitFor(() => expect(t.app.validation).toBe('ok'))
    const last = t.daemon.received.filter((r) => r.cmd === 'validate').at(-1)
    expect(last.config.layers[0].keys.KEY_Z).toEqual({ key: 'LCTRL+X' })

    t.click('#save')
    expect(t.$('.modal').textContent).toContain('キー Z（KEY_Z）')
    expect(t.$('.modal').textContent).toContain('LCTRL+X')
    t.click('#confirm-save')
    await vi.waitFor(() => expect(t.app.dirty).toBe(false))
    expect(t.daemon.config.layers[0].keys!.KEY_Z).toEqual({ key: 'LCTRL+X' })
    expect(t.root.textContent).toContain('保存して反映しました')
    expect(t.root.querySelector('.modal')).toBeNull()
  })

  it('誤りは、その場所と一覧に出る。直すまで保存できない', async () => {
    const t = await connected()
    t.click('[data-layer="1"]')
    t.click('[data-code="KEY_Q"]')
    t.change('#combo-text', 'LCTRL+NOPE')
    await vi.waitFor(() => expect(t.app.validation).toBe('invalid'))
    expect(t.$('[data-code="KEY_Q"]').classList.contains('error')).toBe(true)
    expect(t.$('.inspector .err').textContent).toContain('unknown key "NOPE"')
    expect(t.$('[data-layer="1"] .badge').textContent).toBe('1')
    // 一覧から誤りの場所へ移れる
    t.click('[data-layer="0"]')
    t.$<HTMLAnchorElement>('.problems a').click()
    expect(t.app.layer).toBe(1)
    expect(t.app.sel).toEqual({ kind: 'key', code: 'KEY_Q' })
    t.click('#save')
    expect(t.$<HTMLButtonElement>('#confirm-save').disabled).toBe(true)
  })

  it('選びかけの割り当ては、GUI の中で誤りにする', async () => {
    const t = await connected()
    t.click('[data-cell="1,1"]')
    t.change('#kind', 'layer_hold')
    t.change('#target', '')
    await vi.waitFor(() => expect(t.app.validation).toBe('invalid'))
    expect(t.app.problems[0]).toMatchObject({ path: '/layers/0/touch/cells/1,1' })
    // デーモンには選びかけのものを送らない
    const last = t.daemon.received.filter((r) => r.cmd === 'validate').at(-1)
    expect(last.config.layers[0].touch.cells['1,1']).toBeUndefined()
  })

  it('反映に失敗したら、そのことを伝える', async () => {
    const t = await connected()
    t.daemon.failApply = true
    t.click('[data-code="KEY_Z"]')
    t.change('#kind', 'none')
    await vi.waitFor(() => expect(t.app.validation).toBe('ok'))
    t.click('#save')
    t.click('#confirm-save')
    await vi.waitFor(() => expect(t.root.textContent).toContain('前の設定に戻しました'))
    expect(t.app.dirty).toBe(true)
  })

  it('レイヤーの名前を変えると、参照も変わる。追加と削除もできる', async () => {
    const t = await connected()
    t.click('[data-layer="1"]')
    t.change('[data-focus="layer-name"]', 'editing')
    expect(t.app.cfg!.layers[0].keys!.KEY_LEFTALT).toEqual({ layer_hold: 'editing' })
    t.change('[data-focus="layer-name"]', 'view') // 重複は断る
    expect(t.app.cfg!.layers[1].name).toBe('editing')
    expect(t.root.textContent).toContain('すでにあります')
    t.click('.tab.add')
    expect(t.app.cfg!.layers).toHaveLength(4)
    expect(t.app.layer).toBe(3)
    t.click('.layer-props .danger')
    expect(t.app.cfg!.layers).toHaveLength(3)
  })

  it('格子を小さくするとき、はみ出すセルを消してよいか聞く', async () => {
    const t = await connected(setup({ confirm: false }))
    t.change('[data-focus="cols-0"]', '2')
    expect(t.app.cfg!.layers[0].touch!.cols).toBe(4) // 断ったので変わらない
    t.app['deps'].confirm = () => true
    t.change('[data-focus="cols-0"]', '2')
    expect(t.app.cfg!.layers[0].touch!.cols).toBe(2)
    expect(Object.keys(t.app.cfg!.layers[0].touch!.cells!).sort()).toEqual(['0,0', '0,1', '0,2', '1,0', '1,1'])
  })
})

describe('学習モード', () => {
  it('Brain で押したキーやセルを選ぶ。PC には送らない設定で購読する', async () => {
    const t = await connected()
    t.click('#learn')
    await vi.waitFor(() => expect(t.app.learning).toBe(true))
    expect(t.daemon.received.find((r) => r.cmd === 'subscribe_input')).toMatchObject({ enable: true, suppress: true })

    t.daemon.pressKey('KEY_W')
    await vi.waitFor(() => expect(t.app.sel).toEqual({ kind: 'key', code: 'KEY_W' }))
    expect(t.$('.inspector').textContent).toContain('キー W（KEY_W）')
    t.daemon.pressKey('KEY_GRAVE') // 「記号」+ D
    await vi.waitFor(() => expect(t.app.symbolMode).toBe(true))
    t.daemon.touch(229, 3834) // 左上
    await vi.waitFor(() => expect(t.app.sel).toEqual({ kind: 'cell', col: 0, row: 0 }))
    t.daemon.touch(3800, 3700, 'home') // HOME は割り当てがあるのでソフトキー
    await vi.waitFor(() => expect(t.app.sel).toEqual({ kind: 'soft', name: 'home' }))
    t.daemon.touch(3800, 3000, 'up') // ▲ は割り当てがないので、右の列のセル
    await vi.waitFor(() => expect(t.app.sel).toEqual({ kind: 'cell', col: 3, row: 0 }))

    // デーモンの学習モードが切れないよう、定期的にリクエストを送る
    const n = t.cmds().filter((c) => c === 'get_status').length
    await vi.waitFor(() => expect(t.cmds().filter((c) => c === 'get_status').length).toBeGreaterThan(n + 1))

    t.click('#learn')
    await vi.waitFor(() =>
      expect(t.daemon.received.filter((r) => r.cmd === 'subscribe_input').at(-1)).toMatchObject({ enable: false }))
    expect(t.app.learning).toBe(false)
  })

  it('Brain のレイヤーの変化を表示する', async () => {
    const t = await connected()
    t.daemon.setLayer('edit', 'layer_hold')
    await vi.waitFor(() => expect(t.$('.brain-layer').textContent).toContain('編集'))
    expect(t.$('.brain-layer').classList.contains('mode-temp')).toBe(true)
  })
})

describe('ファイル', () => {
  it('YAML を開いて編集できる（接続していなくても）', async () => {
    const t = setup()
    const cfg = sampleConfig()
    cfg.layers[0].label = 'ファイルの基本'
    await t.app.openFile(new File([toYAML(cfg)], 'backup.yaml'))
    expect(t.root.textContent).toContain('ファイル backup.yaml')
    expect(t.$('[data-layer="0"]').textContent).toContain('ファイルの基本')
    expect(t.app.validation).toBe('offline')
    t.app.openFile(new File(['layers: [ {name'], 'broken.yaml'))
    await vi.waitFor(() => expect(t.root.textContent).toContain('読み込めません'))
  })
})
