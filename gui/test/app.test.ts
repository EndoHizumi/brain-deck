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
    expect(t.cmds().slice(0, 5)).toEqual(['hello', 'set_time', 'get_keymap', 'get_config', 'get_status'])
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

  it('押したときの見せ方を選んで保存でき、プレビューのセルを押さえると押した見た目になる', async () => {
    const t = await connected()
    expect(t.$<HTMLSelectElement>('#press-style').value).toBe('border') // 書いていなければ border
    t.change('#press-style', 'fill')
    expect(t.app.cfg!.display!.press_style).toBe('fill')
    await vi.waitFor(() => expect(t.app.validation).toBe('ok'))
    t.click('#save')
    expect(t.$('.modal').textContent).toContain('press_style')
    t.click('#confirm-save')
    await vi.waitFor(() => expect(t.app.dirty).toBe(false))
    expect(t.daemon.config.display!.press_style).toBe('fill')

    // マウスで押さえているあいだだけ、そのセルを押したものとして描く
    const cell = t.$('[data-cell="2,1"]')
    cell.dispatchEvent(new Event('pointerdown'))
    expect(t.app.previewPress).toBe('2,1')
    cell.dispatchEvent(new Event('pointerup'))
    expect(t.app.previewPress).toBeNull()
    cell.dispatchEvent(new Event('pointerdown'))
    cell.dispatchEvent(new Event('pointerleave'))
    expect(t.app.previewPress).toBeNull()
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

describe('時刻とウィジェット', () => {
  it('接続すると Brain の時刻を PC に合わせ、ずれていたことを伝える', async () => {
    const daemon = new FakeDaemon(sampleConfig())
    daemon.clockOffsetMs = (36 * 3600 + 5) * 1000
    const before = Date.now()
    const t = await connected(setup({ daemon }))
    const req = t.daemon.received.find((r) => r.cmd === 'set_time')
    expect(req.source).toBe('gui')
    expect(req.unix_ms).toBeGreaterThanOrEqual(before)
    expect(req.unix_ms).toBeLessThanOrEqual(Date.now())
    expect(t.root.textContent).toContain('Brain の時刻を PC に合わせました（1 日 12 時間ずれていました）')
    expect(t.daemon.timeSynced).toBe(true)
  })

  it('セルを時計のウィジェットにし、書式、タップしたとき、大きさを編集して保存できる', async () => {
    const confirms: string[] = []
    const t = await connected()
    t.app['deps'].confirm = (m: string) => (confirms.push(m), true)
    t.click('[data-cell="1,0"]')
    t.change('#kind', 'widget')
    expect(t.app.cfg!.layers[0].touch!.cells!['1,0']).toMatchObject({ widget: 'clock' })
    expect(t.$('.inspector').textContent).toContain('タップしたとき')
    t.change('#format', '15:04:05')
    t.change('#date_format', 'none')
    t.change('#tz', 'UTC')
    t.change('#tap', 'key')
    t.change('#combo-text', 'F5')
    t.change('#span-w', '2') // 右のセル 2,0（取り消し）を覆う。消してよいか聞く（confirm は true）
    const cells = t.app.cfg!.layers[0].touch!.cells!
    expect(cells['1,0']).toEqual({ widget: 'clock', label: '消しゴム', format: '15:04:05', date_format: 'none', tz: 'UTC', key: 'F5', span: [2, 1] })
    expect(cells['2,0']).toBeUndefined()
    expect(confirms.join()).toContain('2,0')
    // 覆われたセルのボタンはなく、時計のボタンは 2 列ぶんの幅
    expect(t.root.querySelector('[data-cell="2,0"]')).toBeNull()
    expect(t.$('[data-cell="1,0"]').style.width).toBe('50%')
    t.change('#tap', 'widget') // タップしても何もしない
    expect(t.app.cfg!.layers[0].touch!.cells!['1,0'].key).toBeUndefined()
    await vi.waitFor(() => expect(t.app.validation).toBe('ok'))

    t.click('#save')
    expect(t.$('.modal').textContent).toContain('widget: clock')
    t.click('#confirm-save')
    await vi.waitFor(() => expect(t.app.dirty).toBe(false))
    expect(t.daemon.config.layers[0].touch!.cells!['1,0']).toMatchObject({ widget: 'clock', span: [2, 1] })

    // 学習モードで span のセルの右側を押しても、左上のセルを選ぶ
    t.click('#learn')
    await vi.waitFor(() => expect(t.app.learning).toBe(true))
    t.app.sel = null
    t.daemon.touch(2546, 3233) // セル 2,0 の中央
    await vi.waitFor(() => expect(t.app.sel).toEqual({ kind: 'cell', col: 1, row: 0 }))
    t.click('#learn')
  })

  it('知らないタイムゾーンは誤りになる。キーやソフトキーには選べない', async () => {
    const t = await connected()
    t.click('[data-cell="0,0"]')
    t.change('#kind', 'widget')
    t.change('#tz', 'Mars/Base')
    await vi.waitFor(() => expect(t.app.validation).toBe('invalid'))
    expect(t.$('.inspector').textContent).toContain('unknown tz')
    t.click('[data-code="KEY_Z"]')
    expect([...t.root.querySelectorAll('#kind option')].map((o) => (o as HTMLOptionElement).value)).not.toContain('widget')
  })
})

describe('テキストのタイル', () => {
  it('セルをテキストにし、id を選ぶと、Brain の中身をプレビューと欄に出す', async () => {
    const daemon = new FakeDaemon(sampleConfig())
    daemon.texts = { build: { text: 'ビルド成功', style: 'ok', set_at: '2026-10-06T00:00:00Z' } }
    const t = await connected(setup({ daemon }))
    expect(t.cmds()).toContain('get_text')
    expect(t.app.texts.build.text).toBe('ビルド成功')
    t.click('[data-cell="1,0"]')
    t.change('#kind', 'widget')
    t.change('#widget', 'text')
    // 種類を変えると、時計の項目は消え、まだ使っていない id（Brain にあるもの）を選ぶ
    expect(t.app.cfg!.layers[0].touch!.cells!['1,0']).toEqual({ widget: 'text', id: 'build', label: '消しゴム' })
    expect(t.$('.inspector').textContent).toContain('今の中身：「ビルド成功」（成功）')
    expect(t.$('.inspector').textContent).toContain('brain-deck text build')
    await vi.waitFor(() => expect(t.app.validation).toBe('ok'))
    t.change('#text-id', 'bad id')
    await vi.waitFor(() => expect(t.app.validation).toBe('invalid'))
    expect(t.$('.inspector').textContent).toContain('英数字と _ . -')
    t.change('#text-id', 'deploy')
    await vi.waitFor(() => expect(t.app.validation).toBe('ok'))
    expect(t.$('.inspector').textContent).toContain('まだありません')
    t.change('#widget', 'clock')
    expect(t.app.cfg!.layers[0].touch!.cells!['1,0']).toEqual({ widget: 'clock', label: '消しゴム' })
  })
})

describe('Todo', () => {
  const texts = (t: ReturnType<typeof setup>) =>
    [...t.root.querySelectorAll<HTMLInputElement>('.todo-item .todo-text')].map((i) => (i.closest('.todo-item')!.classList.contains('done') ? '✓' : '') + i.value)
  const item = (t: ReturnType<typeof setup>, text: string) =>
    [...t.root.querySelectorAll<HTMLElement>('.todo-item')].find((li) => li.querySelector<HTMLInputElement>('.todo-text')!.value === text)!

  it('接続すると一覧を読み、Brain での変更の通知を受ける。タブで足し、完了にし、並べ替え、消す', async () => {
    const t = await connected()
    expect(t.cmds()).toContain('get_todo')
    expect(t.cmds()).toContain('subscribe_data')
    t.click('#section-todo')
    expect(t.root.textContent).toContain('まだ項目がありません')
    expect(t.root.textContent).toContain('Todo のセルがない')
    // 足す（Enter）。先頭にも足せる
    const add = async (text: string, top = false) => {
      t.$<HTMLInputElement>('#todo-new').value = text
      t.$('#todo-new').dispatchEvent(new Event('input'))
      t.click(top ? '#todo-add-top' : '#todo-add')
      await vi.waitFor(() => expect(texts(t)).toContain(text))
    }
    await add('牛乳を買う')
    await add('歯医者の予約')
    await add('急ぎ', true)
    expect(texts(t)).toEqual(['急ぎ', '牛乳を買う', '歯医者の予約'])
    expect(t.$<HTMLInputElement>('#todo-new').value).toBe('')
    expect(t.daemon.todo.items.map((i) => i.source)).toEqual(['gui', 'gui', 'gui'])
    // 完了にすると下に並ぶ
    item(t, '急ぎ').querySelector<HTMLInputElement>('input[type=checkbox]')!.click()
    await vi.waitFor(() => expect(texts(t)).toEqual(['牛乳を買う', '歯医者の予約', '✓急ぎ']))
    // 並べ替え（完了との境はまたがない）
    expect(item(t, '歯医者の予約').querySelector<HTMLButtonElement>('.down')!.disabled).toBe(true)
    item(t, '歯医者の予約').querySelector<HTMLButtonElement>('.up')!.click()
    await vi.waitFor(() => expect(texts(t)).toEqual(['歯医者の予約', '牛乳を買う', '✓急ぎ']))
    // 文を書き換える
    const input = item(t, '牛乳を買う').querySelector<HTMLInputElement>('.todo-text')!
    input.value = '牛乳と卵'
    input.dispatchEvent(new Event('input'))
    input.dispatchEvent(new Event('change'))
    await vi.waitFor(() => expect(texts(t)).toEqual(['歯医者の予約', '牛乳と卵', '✓急ぎ']))
    // Brain で長押しして切り替えると、通知で反映する
    t.daemon.toggleTodo(t.daemon.todo.items.find((i) => i.text === '歯医者の予約')!.id)
    await vi.waitFor(() => expect(texts(t)).toEqual(['牛乳と卵', '✓急ぎ', '✓歯医者の予約']))
    expect(item(t, '歯医者の予約').textContent).toContain('Brain')
    expect(t.$('#section-todo').textContent).toBe('Todo1')
    // 完了した項目をまとめて消す
    t.click('#todo-clear-done')
    await vi.waitFor(() => expect(texts(t)).toEqual(['牛乳と卵']))
    item(t, '牛乳と卵').querySelector<HTMLButtonElement>('.del')!.click()
    await vi.waitFor(() => expect(texts(t)).toEqual([]))
    // Todo は設定とは別なので、設定は変わっていない
    expect(t.app.dirty).toBe(false)
  })

  it('Brain で変わっていた項目は書き換えず、読み直す', async () => {
    const daemon = new FakeDaemon(sampleConfig())
    daemon.todoCmd({ cmd: 'todo_add', text: 'a' })
    const t = await connected(setup({ daemon }))
    t.click('#section-todo')
    // GUI が知らないうちに Brain で変わった（通知が届かなかった）
    daemon.dataSubscribed = false
    daemon.toggleTodo('t1')
    daemon.dataSubscribed = true
    expect(texts(t)).toEqual(['a'])
    item(t, 'a').querySelector<HTMLInputElement>('input[type=checkbox]')!.click()
    await vi.waitFor(() => expect(t.root.textContent).toContain('Brain で項目が変わっていた'))
    await vi.waitFor(() => expect(texts(t)).toEqual(['✓a']))
    expect(daemon.todo.items[0].done).toBe(true)
  })

  it('古い rev の通知や返事は使わない', async () => {
    const t = await connected()
    t.app.onNotify({ event: 'todo', rev: 5, items: [{ id: 't1', text: 'new', done: false, rev: 5, created_at: '', updated_at: '' }] })
    t.app.onNotify({ event: 'todo', rev: 4, items: [] })
    expect(t.app.todo!.items.map((i) => i.text)).toEqual(['new'])
  })

  it('セルを Todo にすると、タップの動きは選べず、行数を決められる。プレビューに出す', async () => {
    const daemon = new FakeDaemon(sampleConfig())
    daemon.todoCmd({ cmd: 'todo_add', text: '牛乳を買う' })
    const t = await connected(setup({ daemon }))
    t.click('[data-cell="1,0"]')
    t.change('#kind', 'widget')
    t.change('#tap', 'key')
    t.change('#combo-text', 'F5')
    t.change('#widget', 'todo')
    expect(t.app.cfg!.layers[0].touch!.cells!['1,0']).toEqual({ widget: 'todo', label: '消しゴム' })
    expect(t.root.querySelector('#tap')).toBeNull()
    expect(t.$('.inspector').textContent).toContain('今は 1 件が未完了')
    t.change('#todo-rows', '3')
    expect(t.app.cfg!.layers[0].touch!.cells!['1,0']).toEqual({ widget: 'todo', label: '消しゴム', rows: 3 })
    await vi.waitFor(() => expect(t.app.validation).toBe('ok'))
    t.change('#todo-rows', '')
    expect(t.app.cfg!.layers[0].touch!.cells!['1,0'].rows).toBeUndefined()
    // YAML に書き出しても rows は数のまま
    t.change('#todo-rows', '2')
    expect(toYAML(t.app.cfg!)).toContain('rows: 2')
    t.click('#section-todo')
    expect(t.root.textContent).not.toContain('Todo のセルがない')
  })
})
