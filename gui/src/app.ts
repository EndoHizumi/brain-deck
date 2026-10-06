// 設定 GUI の画面。状態は App が持ち、変わるたびに描き直す。

import { diffConfigs, type Change } from './diff'
import { h, replaceKeepingFocus } from './dom'
import { loadFont, type BitmapFont } from './font'
import {
  ComboCapture, KEY_GROUPS, MODIFIERS, PRETTY, formatCombo, keyTitle, parseCombo, prettyCombo, type Modifier,
} from './keys'
import defaultKeymap from './keymap-pwsh2.json'
import {
  DEFAULT_CLOCK_FORMAT, DEFAULT_DATE_FORMAT, KIND_LABELS, LAYER_KINDS, LAYER_VERB, WIDGET_FIELDS, WIDGET_LABELS,
  actionKind, actionTarget, addLayer, anchorOf, cellKey, cellsOutside, clean, deleteLayer, describeAction, editStack,
  isIncomplete, gridSize, layerTitle, normalizeConfig, parsePath, references, renameLayer, resolveCell, resolveGrid,
  resolveKey, resolveSoft, setCellAction, setKeyAction, setSoftAction, spanOf, touchCell, type ActionKind, type LayerKind,
  type Location, type ResolvedAction,
} from './model'
import { hasSeconds } from './clock'
import { cellSpan, renderPreview, type Mode } from './preview'
import { Client, PROTOCOL_VERSION, ProtocolError, type Transport } from './protocol'
import { BRAIN_FILTER, WebSerialTransport, serialSupported } from './serial'
import type {
  ActionSpec, Config, EngineStatus, HelloResult, InputEvent, KeymapInfo, LayerConfig, Notification, PhysKey,
  PressStyle, Problem, SetTimeResult, ValidateResult, WidgetKind,
} from './types'
import { FileError, parseConfigText, sameConfig, toJSON, toYAML } from './yamlio'

export type Selection =
  | { kind: 'key'; code: string }
  | { kind: 'cell'; col: number; row: number }
  | { kind: 'soft'; name: string }

interface Message {
  id: number
  text: string
  level: 'info' | 'ok' | 'error'
}

export interface AppDeps {
  serial?: Serial | null // navigator.serial。テストではモック
  openTransport?: (port: SerialPort) => Promise<Transport>
  loadFont?: () => Promise<BitmapFont>
  confirm?: (msg: string) => boolean
  validateDelayMs?: number
  helloTimeoutMs?: number
  keepaliveMs?: number
}

// ソフトキーの名前と、帯に印刷された文字
const SOFT_TITLES: Record<string, string> = {
  home: 'HOME', up: '▲', down: '▼', right: '▶', left: '◀', enter: '決定', back: '戻る', menu: '操作機能',
}

type TryResult = { result: 'ok' } | { result: 'no_answer' } | { result: 'open_failed'; error: string }

// openFailedMessage は、ポートを開けなかったときの案内。Linux では権限がないことが多い。
function openFailedMessage(err: string): string {
  return (
    `シリアルポートを開けません（${err}）。` +
    'Linux では、/dev/ttyACM* を開く権限が要ります。' +
    '「sudo usermod -aG dialout $USER」のあとログインし直すか、今だけなら「sudo setfacl -m u:$USER:rw /dev/ttyACM1」を実行してください' +
    '（setfacl はケーブルを抜き差しすると消えます）。ほかのタブやアプリがポートを使っているときも開けません'
  )
}

export class App {
  // 接続
  client: Client | null = null
  hello: HelloResult | null = null
  connecting = false
  // 設定
  keymap: KeymapInfo = defaultKeymap as KeymapInfo
  saved: Config | null = null // Brain に保存されている設定（差分の基準）
  cfg: Config | null = null // 編集中の設定
  source = '' // 編集中の設定の出どころ
  layer = 0
  sel: Selection | null = null
  symbolMode = false
  // 検証
  problems: Problem[] = []
  warnings: string[] = []
  validation: 'idle' | 'pending' | 'ok' | 'invalid' | 'offline' | 'error' = 'idle'
  private validateSeq = 0
  private validateTimer: ReturnType<typeof setTimeout> | null = null
  // 学習モード・Brain の状態
  learning = false
  brainStatus: EngineStatus | null = null
  flash: Selection | null = null
  // プレビューで、マウスで押さえているセル（"列,行"）。押したときの見た目で描く
  previewPress: string | null = null
  private keepalive: ReturnType<typeof setInterval> | null = null
  // 時計のプレビューを、時刻が変わるたびに描き直すためのタイマー
  private previewTimer: ReturnType<typeof setTimeout> | null = null
  // そのほか
  capturing = false
  private capture: ComboCapture | null = null
  diff: Change[] | null = null
  saving = false
  messages: Message[] = []
  private msgId = 0
  font: BitmapFont | null = null
  private deps: Required<AppDeps>

  constructor(
    private root: HTMLElement,
    deps: AppDeps = {},
  ) {
    this.deps = {
      serial: deps.serial === undefined ? (serialSupported() ? navigator.serial : null) : deps.serial,
      openTransport: deps.openTransport ?? ((p) => WebSerialTransport.open(p)),
      loadFont: deps.loadFont ?? loadFont,
      confirm: deps.confirm ?? ((m) => window.confirm(m)),
      validateDelayMs: deps.validateDelayMs ?? 250,
      helloTimeoutMs: deps.helloTimeoutMs ?? 1500,
      keepaliveMs: deps.keepaliveMs ?? 10000,
    }
    this.deps
      .loadFont()
      .then((f) => {
        this.font = f
        this.render()
      })
      .catch((e) => this.say(`フォントを読み込めません（プレビューは表示されません）：${e?.message ?? e}`, 'error'))
    window.addEventListener('keydown', (e) => this.onKey(e, true), true)
    window.addEventListener('keyup', (e) => this.onKey(e, false), true)
    window.addEventListener('beforeunload', (e) => {
      if (this.dirty) e.preventDefault()
    })
    this.deps.serial?.addEventListener?.('disconnect', () => {
      /* 読み込みループの終わりで onClose が呼ばれる */
    })
    this.render()
  }

  get connected(): boolean {
    return !!this.client && !this.client.closed && !!this.hello
  }

  get dirty(): boolean {
    return !!this.cfg && (!this.saved || !sameConfig(this.cfg, this.saved))
  }

  // ---------- メッセージ ----------

  say(text: string, level: Message['level'] = 'info'): void {
    const m = { id: ++this.msgId, text, level }
    this.messages = [...this.messages.slice(-3), m]
    setTimeout(() => {
      this.messages = this.messages.filter((x) => x !== m)
      this.render()
    }, level === 'error' ? 12000 : 5000)
    this.render()
  }

  // ---------- 接続 ----------

  async connect(): Promise<void> {
    const serial = this.deps.serial
    if (!serial) {
      this.say('このブラウザは WebSerial に対応していません。Chrome か Edge で開いてください', 'error')
      return
    }
    this.connecting = true
    this.render()
    try {
      // 以前に許可したポートを先に試す（ACM が 2 つあるので、応答したほうが設定用）
      const known = (await serial.getPorts()).filter((p) => {
        const i = p.getInfo()
        return i.usbVendorId === BRAIN_FILTER.usbVendorId && i.usbProductId === BRAIN_FILTER.usbProductId
      })
      let openError = ''
      for (const p of known) {
        const r = await this.tryPort(p)
        if (r.result === 'ok') return
        if (r.result === 'open_failed') openError = r.error
      }
      let port: SerialPort
      try {
        port = await serial.requestPort({ filters: [BRAIN_FILTER] })
      } catch {
        this.say(openError ? openFailedMessage(openError) : 'ポートが選ばれませんでした', openError ? 'error' : 'info')
        return
      }
      const r = await this.tryPort(port)
      if (r.result === 'open_failed') this.say(openFailedMessage(r.error), 'error')
      else if (r.result === 'no_answer')
        this.say(
          'このポートは開けましたが、lefthand が答えません。Brain のシリアルは 2 つあり、もう一方（コンソール用）を選んだかもしれません。' +
            'もう一度「接続」を押して、別のポートを選んでください',
          'error',
        )
    } finally {
      this.connecting = false
      this.render()
    }
  }

  // tryPort はポートを開いて hello を送る。lefthand が答えたら、そのまま使う。
  private async tryPort(port: SerialPort): Promise<TryResult> {
    let t: Transport
    try {
      t = await this.deps.openTransport(port)
    } catch (e: any) {
      return { result: 'open_failed', error: String(e?.message ?? e) }
    }
    const c = new Client(t)
    try {
      await c.start()
      const hello = await c.request<HelloResult>('hello', {}, this.deps.helloTimeoutMs)
      if (hello.daemon !== 'lefthand') throw new Error('not lefthand')
      if (hello.protocol !== PROTOCOL_VERSION) {
        this.say(`プロトコルの版が違います（Brain: ${hello.protocol}、GUI: ${PROTOCOL_VERSION}）。どちらかを更新してください`, 'error')
        await c.close()
        return { result: 'ok' } // 設定用のポートではあった
      }
      c.maxLine = hello.max_line
      this.attach(c, hello)
      await this.syncTime()
      await this.loadFromBrain()
      return { result: 'ok' }
    } catch {
      await c.close().catch(() => {})
      return { result: 'no_answer' }
    }
  }

  private attach(c: Client, hello: HelloResult): void {
    this.client = c
    this.hello = hello
    c.onNotify = (n) => this.onNotify(n)
    c.onStray = (s) => console.debug('lefthand: stray line', s)
    c.onClose = (reason) => {
      if (this.client !== c) return
      this.client = null
      this.hello = null
      this.stopLearning(false)
      this.brainStatus = null
      if (reason !== 'closed by user') this.say(`Brain との接続が切れました（${reason}）。編集中の内容は残っています`, 'error')
      this.validation = 'offline'
      this.render()
    }
    this.say(`接続しました：lefthand ${hello.version}`, 'ok')
  }

  // syncTime は、Brain の時刻を PC の時刻に合わせる（Brain には RTC がないので、電源を切ると遅れる）。
  // 失敗しても、設定の編集はそのまま続けられる。
  async syncTime(): Promise<void> {
    if (!this.hello?.commands?.includes('set_time')) return
    try {
      const r = await this.client!.request<SetTimeResult>('set_time', { unix_ms: Date.now(), source: 'gui' })
      if (r.stepped) this.say(`Brain の時刻を PC に合わせました（${formatOffset(r.offset_ms)}ずれていました）`, 'ok')
      const pc = -new Date().getTimezoneOffset() * 60
      if (r.utc_offset_sec !== pc)
        this.say(
          `Brain のタイムゾーン（${r.timezone || '不明'}、${utcOffset(r.utc_offset_sec)}）が PC（${utcOffset(pc)}）と違います。` +
            '時計のウィジェットは、tz を書かなければ Brain のタイムゾーンで表示します',
          'error',
        )
    } catch (e: any) {
      this.say(`Brain の時刻を合わせられませんでした：${e?.message ?? e}`, 'error')
    }
  }

  async disconnect(): Promise<void> {
    this.stopLearning(true)
    const c = this.client
    this.client = null
    this.hello = null
    this.brainStatus = null
    await c?.close()
    this.validation = 'offline'
    this.render()
  }

  // loadFromBrain は Brain の設定とキー配列を読む。編集中の変更があれば残すかどうか聞く。
  async loadFromBrain(): Promise<void> {
    const c = this.client!
    this.keymap = await c.request<KeymapInfo>('get_keymap')
    const r = await c.request<{ config: Config; path: string }>('get_config')
    const brain = normalizeConfig(r.config)
    const keep =
      this.cfg && this.dirty && this.deps.confirm('編集中の設定があります。残しますか？\n（キャンセルすると、Brain の設定を読み込みます）')
    this.saved = brain
    if (!keep) {
      this.cfg = structuredClone(brain)
      this.source = `Brain（${r.path}）`
      this.layer = 0
      this.sel = null
    }
    this.brainStatus = (await c.request<{ status: EngineStatus }>('get_status')).status
    this.render()
    this.scheduleValidate(0)
  }

  // ---------- 編集 ----------

  get layerCfg(): LayerConfig | null {
    return this.cfg?.layers[this.layer] ?? null
  }

  // changed は編集のたびに呼ぶ。描き直して、少し待ってから検証する。
  changed(): void {
    this.render()
    this.scheduleValidate()
  }

  private scheduleValidate(delay = this.deps.validateDelayMs): void {
    if (this.validateTimer) clearTimeout(this.validateTimer)
    this.validation = this.connected ? 'pending' : 'offline'
    this.validateTimer = setTimeout(() => void this.validate(), delay)
  }

  // localProblems は、デーモンに送る前に GUI で分かる誤り（まだ選んでいないキーなど）。
  localProblems(): Problem[] {
    const out: Problem[] = []
    this.cfg?.layers.forEach((l, li) => {
      const scan = (base: string, m?: Record<string, ActionSpec>) => {
        for (const [id, a] of Object.entries(m ?? {})) {
          if (isIncomplete(a))
            out.push({ path: `${base}/${id.replace(/~/g, '~0').replace(/\//g, '~1')}`, message: '割り当てを最後まで選んでください' })
        }
      }
      scan(`/layers/${li}/keys`, l.keys)
      scan(`/layers/${li}/touch/cells`, l.touch?.cells)
      scan(`/layers/${li}/soft_keys`, l.soft_keys)
    })
    return out
  }

  // forBrain は、デーモンに送る形（空の項目や、選びかけの割り当てを除いたもの）にする。
  forBrain(): Config {
    const c = clean(this.cfg!)
    const strip = (m?: Record<string, ActionSpec>) => {
      for (const [id, a] of Object.entries(m ?? {})) {
        for (const k of Object.keys(a) as (keyof ActionSpec)[]) if (a[k] === '' && k !== 'label') delete a[k]
        if (a.label === '') delete a.label
        if (Object.keys(a).filter((k) => k !== 'label').length === 0) delete m![id]
      }
    }
    for (const l of c.layers) {
      strip(l.keys)
      strip(l.touch?.cells)
      strip(l.soft_keys)
    }
    return clean(c)
  }

  async validate(): Promise<void> {
    this.validateTimer = null
    if (!this.cfg) return
    const local = this.localProblems()
    if (!this.connected) {
      this.problems = local
      this.warnings = []
      this.validation = 'offline'
      this.render()
      return
    }
    const seq = ++this.validateSeq
    try {
      const r = await this.client!.request<ValidateResult>('validate', { config: this.forBrain() })
      if (seq !== this.validateSeq) return // 新しい編集の検証が先にある
      this.problems = [...local, ...r.errors]
      this.warnings = r.warnings
      this.validation = this.problems.length ? 'invalid' : 'ok'
    } catch (e: any) {
      if (seq !== this.validateSeq) return
      this.problems = local
      this.validation = 'error'
      if (!(e instanceof ProtocolError && e.code === 'closed')) this.say(`検証できません：${e?.message ?? e}`, 'error')
    }
    this.render()
  }

  selectLayer(i: number): void {
    this.layer = i
    this.changed()
  }

  addLayer(): void {
    if (!this.cfg) return
    this.layer = addLayer(this.cfg)
    this.sel = null
    this.changed()
  }

  deleteLayer(): void {
    if (!this.cfg || this.layer === 0) return
    const l = this.cfg.layers[this.layer]
    const refs = references(this.cfg, l.name).filter((r) => r.layer !== this.layer)
    const msg =
      `レイヤー「${layerTitle(l)}」を消しますか？` +
      (refs.length ? `\nこのレイヤーに切り替える割り当て ${refs.length} 個も消えます（透過に戻ります）。` : '')
    if (!this.deps.confirm(msg)) return
    deleteLayer(this.cfg, this.layer)
    this.layer = Math.max(0, this.layer - 1)
    this.sel = null
    this.changed()
  }

  renameLayer(name: string): void {
    if (!this.cfg) return
    name = name.trim()
    const l = this.cfg.layers[this.layer]
    if (name === l.name) return
    if (!name) {
      this.say('レイヤーの名前は空にできません', 'error')
    } else if (this.cfg.layers.some((x) => x.name === name)) {
      this.say(`「${name}」という名前のレイヤーは、すでにあります`, 'error')
    } else {
      renameLayer(this.cfg, this.layer, name) // 参照している割り当ても書き換える
    }
    this.changed()
  }

  setLabel(label: string): void {
    const l = this.layerCfg
    if (!l) return
    l.label = label
    this.changed()
  }

  // ---------- 選んだものの割り当て ----------

  ownAction(sel: Selection | null = this.sel, li = this.layer): ActionSpec | null {
    const l = this.cfg?.layers[li]
    if (!l || !sel) return null
    if (sel.kind === 'key') return l.keys?.[sel.code] ?? null
    if (sel.kind === 'soft') return l.soft_keys?.[sel.name] ?? null
    return l.touch?.cells?.[cellKey(sel.col, sel.row)] ?? null
  }

  resolved(sel: Selection | null = this.sel): ResolvedAction | null {
    if (!this.cfg || !sel) return null
    const st = editStack(this.layer)
    if (sel.kind === 'key') return resolveKey(this.cfg, st, sel.code)
    if (sel.kind === 'soft') return resolveSoft(this.cfg, st, sel.name)
    return resolveCell(this.cfg, st, sel.col, sel.row)
  }

  setAction(a: ActionSpec | null): void {
    const l = this.layerCfg
    const s = this.sel
    if (!l || !s) return
    if (s.kind === 'key') setKeyAction(l, s.code, a)
    else if (s.kind === 'soft') setSoftAction(l, s.name, a)
    else setCellAction(l, s.col, s.row, a)
    this.changed()
  }

  setKind(kind: ActionKind | 'inherit'): void {
    const cur = this.ownAction()
    const label = cur?.label
    let a: ActionSpec | null
    switch (kind) {
      case 'inherit':
        a = null
        break
      case 'none':
        a = { key: 'none' }
        break
      case 'widget':
        a = { widget: 'clock' }
        break
      case 'key':
        a = { key: cur && actionKind(cur) === 'key' ? cur.key : '' }
        break
      default: {
        const others = this.cfg!.layers.filter((_, i) => i !== this.layer && !(kind === 'layer_toggle' && i === 0))
        const t = (cur && actionTarget(cur)) || others[0]?.name || ''
        a = { [kind]: t } as ActionSpec
      }
    }
    if (a && label) a.label = label
    if (a && cur?.span && kind !== 'none') a.span = cur.span
    this.setAction(a)
  }

  // setTap は、ウィジェットのセルをタップしたときの動きを変える（ウィジェットの項目と大きさは残す）。
  setTap(kind: ActionKind): void {
    const cur = this.ownAction()
    if (!cur?.widget) return
    const a: ActionSpec = {}
    for (const f of [...WIDGET_FIELDS, 'label', 'span'] as const) if (cur[f] !== undefined) (a as any)[f] = cur[f]
    if (kind === 'key') a.key = cur.key && cur.key.toLowerCase() !== 'none' ? cur.key : ''
    else if (LAYER_KINDS.includes(kind as LayerKind)) {
      const others = this.cfg!.layers.filter((_, i) => i !== this.layer && !(kind === 'layer_toggle' && i === 0))
      a[kind as LayerKind] = actionTarget(cur) || others[0]?.name || ''
    }
    this.setAction(a)
  }

  // setSpan はセルの大きさを変える。[1, 1] なら span を書かない。
  setSpan(w: number, h: number): void {
    const cur = this.ownAction()
    const s = this.sel
    if (!cur || s?.kind !== 'cell') return
    const g = resolveGrid(this.cfg!, editStack(this.layer))
    w = Math.max(1, Math.min(Math.trunc(w) || 1, g.cols - s.col))
    h = Math.max(1, Math.min(Math.trunc(h) || 1, g.rows - s.row))
    // 広げた範囲にある、このレイヤーのセルは覆われて使えなくなる（デーモンの検証で誤りになる）ので消す
    const l = this.layerCfg!
    const covered = Object.keys(l.touch?.cells ?? {}).filter((k) => {
      const m = k.match(/^(\d+),(\d+)$/)
      if (!m || (+m[1] === s.col && +m[2] === s.row)) return false
      return +m[1] >= s.col && +m[1] < s.col + w && +m[2] >= s.row && +m[2] < s.row + h
    })
    if (covered.length && !this.deps.confirm(`広げた範囲にあるセル ${covered.length} 個（${covered.join('、')}）の割り当てを消します。よいですか？`)) {
      this.render()
      return
    }
    for (const k of covered) delete l.touch!.cells![k]
    const a = { ...cur }
    if (w === 1 && h === 1) delete a.span
    else a.span = [w, h]
    this.setAction(a)
  }

  patchAction(p: Partial<ActionSpec>): void {
    const cur = this.ownAction()
    if (!cur) return
    const a = { ...cur, ...p }
    if (a.label === '') delete a.label
    for (const f of ['format', 'date_format', 'tz'] as const) if (a[f] === '') delete a[f]
    this.setAction(a)
  }

  // ---------- 学習モード ----------

  async toggleLearning(): Promise<void> {
    if (this.learning) {
      this.stopLearning(true)
      this.render()
      return
    }
    if (!this.connected) return
    try {
      await this.client!.request('subscribe_input', { enable: true, suppress: true })
    } catch (e: any) {
      this.say(`学習モードにできません：${e?.message ?? e}`, 'error')
      return
    }
    this.learning = true
    // デーモンは 30 秒リクエストがないと、学習モードを自動で解く。そうならないよう定期的に知らせる
    this.keepalive = setInterval(() => {
      this.client?.request('get_status').catch(() => {})
    }, this.deps.keepaliveMs)
    this.say('学習モード：Brain のキーを押すかタッチすると、そのキーやセルを選びます。PC には送りません')
    this.render()
  }

  stopLearning(tell: boolean): void {
    if (this.keepalive) clearInterval(this.keepalive)
    this.keepalive = null
    if (this.learning && tell) this.client?.request('subscribe_input', { enable: false }).catch(() => {})
    this.learning = false
  }

  onNotify(n: Notification): void {
    if (n.event === 'layer') {
      const { event: _, ...st } = n
      this.brainStatus = st
      this.render()
      return
    }
    if (n.event === 'input' && this.learning) this.learn(n)
  }

  // learn は、Brain で押されたキーやセルを選ぶ。
  learn(ev: InputEvent): void {
    if (!this.cfg) return
    if (ev.type === 'key' && ev.code) {
      const normal = this.keymap.keys.some((k) => k.code === ev.code)
      const sym = this.keymap.keys.some((k) => k.symbol === ev.code)
      if (!normal && sym) this.symbolMode = true
      else if (normal && !this.keymap.keys.some((k) => k.symbol === ev.code && k.code !== ev.code)) this.symbolMode = false
      this.sel = { kind: 'key', code: ev.code }
    } else if (ev.type === 'touch' && this.cfg.touch && ev.x !== undefined && ev.y !== undefined) {
      const st = editStack(this.layer)
      // ソフトキーの範囲は画面の右端と重なる。デーモンと同じく、割り当てがあるときだけソフトキーとみなす
      if (ev.soft && resolveSoft(this.cfg, st, ev.soft)) {
        this.sel = { kind: 'soft', name: ev.soft }
      } else {
        const g = resolveGrid(this.cfg, st)
        if (g.cols === 0) return
        const [c, r] = touchCell(this.cfg.touch, g.cols, g.rows, ev.x, ev.y)
        const [col, row] = anchorOf(g, c, r) // span のセルは左上のセルとして選ぶ
        this.sel = { kind: 'cell', col, row }
      }
    } else {
      return
    }
    this.flash = this.sel
    setTimeout(() => {
      if (this.flash === this.sel) this.flash = null
      this.render()
    }, 600)
    this.render()
  }

  // ---------- PC のキーの取り込み ----------

  startCapture(): void {
    this.capturing = true
    this.capture = new ComboCapture(
      (combo) => {
        this.capturing = false
        this.capture = null
        const label = this.ownAction()?.label
        this.setAction(label ? { key: combo, label } : { key: combo })
      },
      (code) => this.say(`${code} は送れるキーにありません。一覧から選んでください`, 'error'),
    )
    this.render()
  }

  cancelCapture(): void {
    this.capturing = false
    this.capture = null
    this.render()
  }

  private onKey(e: KeyboardEvent, down: boolean): void {
    if (this.capture) {
      e.preventDefault()
      e.stopPropagation()
      if (down) this.capture.keydown(e)
      else this.capture.keyup(e)
      return
    }
    if (down && e.key === 'Escape' && this.diff) {
      this.diff = null
      this.render()
    }
  }

  // ---------- ファイル ----------

  async openFile(file: File): Promise<void> {
    let cfg: Config
    try {
      cfg = parseConfigText(await file.text())
    } catch (e: any) {
      this.say(e instanceof FileError ? e.message : `読み込めません：${e?.message ?? e}`, 'error')
      return
    }
    if (this.dirty && !this.deps.confirm('編集中の変更を捨てて、ファイルを読み込みますか？')) return
    this.cfg = cfg
    this.source = `ファイル ${file.name}`
    this.layer = 0
    this.sel = null
    this.say(`${file.name} を読み込みました。Brain にはまだ保存していません`, 'ok')
    this.changed()
  }

  download(kind: 'yaml' | 'json'): void {
    if (!this.cfg) return
    const text = kind === 'yaml' ? toYAML(this.forBrain()) : toJSON(this.forBrain())
    const d = new Date()
    const p = (n: number) => String(n).padStart(2, '0')
    const name = `lefthand-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}.${kind}`
    const url = URL.createObjectURL(new Blob([text], { type: kind === 'yaml' ? 'text/yaml' : 'application/json' }))
    const a = h('a', { href: url, download: name })
    document.body.appendChild(a)
    a.click()
    a.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }

  revert(): void {
    if (!this.saved || !this.deps.confirm('編集中の変更をすべて捨てて、Brain の設定に戻しますか？')) return
    this.cfg = structuredClone(this.saved)
    this.layer = 0
    this.sel = null
    this.changed()
  }

  // ---------- 保存 ----------

  reviewSave(): void {
    if (!this.cfg || !this.saved) return
    const changes = diffConfigs(clean(this.saved), this.forBrain(), this.keymap)
    if (changes.length === 0) {
      this.say('Brain の設定と同じです。保存するものはありません')
      return
    }
    this.diff = changes
    this.render()
  }

  async save(): Promise<void> {
    if (!this.connected || !this.cfg) return
    this.saving = true
    this.render()
    try {
      const r = await this.client!.request<{ saved: string; previous: string; warnings: string[] }>(
        'set_config',
        { config: this.forBrain() },
        15000,
      )
      this.diff = null
      this.say(`保存して反映しました（${r.saved}。前の版は ${r.previous}）`, 'ok')
      const g = await this.client!.request<{ config: Config }>('get_config')
      this.saved = normalizeConfig(g.config)
      this.cfg = structuredClone(this.saved)
      if (this.layer >= this.cfg.layers.length) this.layer = 0
      this.warnings = r.warnings
      this.scheduleValidate(0)
    } catch (e: any) {
      this.diff = null
      if (e instanceof ProtocolError && e.code === 'invalid_config') {
        this.problems = e.problems
        this.validation = 'invalid'
        this.say('設定に誤りがあるため、保存しませんでした。赤い印の場所を直してください', 'error')
      } else if (e instanceof ProtocolError && e.code === 'apply_failed') {
        this.say(`反映に失敗したため、前の設定に戻しました：${e.message}`, 'error')
      } else {
        this.say(`保存できませんでした：${e?.message ?? e}`, 'error')
      }
    } finally {
      this.saving = false
      this.render()
    }
  }

  // ---------- 誤りの場所 ----------

  problemsAt(match: (loc: Location) => boolean): Problem[] {
    return this.problems.filter((p) => match(parsePath(p.path)))
  }

  private selMatches(loc: Location, sel: Selection, li: number): boolean {
    if (!('layer' in loc) || loc.layer !== li) return false
    if (sel.kind === 'key') return loc.kind === 'key' && loc.id === sel.code
    if (sel.kind === 'soft') return loc.kind === 'soft' && loc.id === sel.name
    return loc.kind === 'cell' && loc.id === cellKey(sel.col, sel.row)
  }

  goTo(p: Problem): void {
    const loc = parsePath(p.path)
    if ('layer' in loc && this.cfg && loc.layer < this.cfg.layers.length) this.layer = loc.layer
    if (loc.kind === 'key') this.sel = { kind: 'key', code: loc.id }
    else if (loc.kind === 'soft') this.sel = { kind: 'soft', name: loc.id }
    else if (loc.kind === 'cell') {
      const m = loc.id.match(/^(\d+),(\d+)$/)
      this.sel = m ? { kind: 'cell', col: +m[1], row: +m[2] } : null
    }
    this.render()
  }

  // ---------- 描画 ----------

  render(): void {
    replaceKeepingFocus(this.root, this.view())
    this.drawPreview()
  }

  private view(): HTMLElement {
    return h(
      'div',
      { class: 'app' },
      this.viewHeader(),
      this.viewMessages(),
      this.cfg ? this.viewEditor() : this.viewStart(),
      this.diff ? this.viewDiff() : null,
    )
  }

  private viewHeader(): HTMLElement {
    const fileInput = h('input', {
      type: 'file',
      accept: '.yaml,.yml,.json,application/json,text/yaml',
      class: 'hidden',
      onchange: (e: Event) => {
        const f = (e.target as HTMLInputElement).files?.[0]
        if (f) void this.openFile(f)
      },
    })
    const conn = this.connected
      ? [
          h('span', { class: 'conn ok', title: this.hello!.config_path }, `● 接続中　lefthand ${this.hello!.version}`),
          this.brainStatus ? h('span', { class: `brain-layer mode-${this.brainStatus.mode}` }, `Brain：${this.brainStatus.label}`) : null,
          h('button', { onclick: () => void this.disconnect() }, '切断'),
        ]
      : [
          h('span', { class: 'conn off' }, this.deps.serial ? '○ 未接続' : '○ WebSerial 非対応のブラウザ'),
          h('button', { class: 'primary', disabled: this.connecting || !this.deps.serial, onclick: () => void this.connect(), id: 'connect' },
            this.connecting ? '接続中…' : 'Brain に接続'),
        ]
    const canSave = this.connected && !!this.cfg && this.dirty && !this.saving
    return h(
      'header',
      null,
      h('h1', null, 'lefthand 設定'),
      h('div', { class: 'conn-area' }, conn),
      h('div', { class: 'spacer' }),
      this.connected && this.cfg
        ? h('button', { class: this.learning ? 'learning on' : 'learning', onclick: () => void this.toggleLearning(), id: 'learn',
            title: 'Brain のキーを押すかタッチすると、そのキーやセルを選びます' }, this.learning ? '学習モード：ON' : '学習モード')
        : null,
      fileInput,
      h('button', { onclick: () => fileInput.click() }, 'ファイルを開く'),
      h('button', { disabled: !this.cfg, onclick: () => this.download('yaml') }, 'YAML で書き出す'),
      h('button', { disabled: !this.cfg, onclick: () => this.download('json') }, 'JSON で書き出す'),
      this.saved ? h('button', { disabled: !this.dirty, onclick: () => this.revert() }, '変更を捨てる') : null,
      h('button', { class: 'primary', id: 'save', disabled: !canSave, onclick: () => this.reviewSave(),
        title: this.connected ? '' : 'Brain に接続すると保存できます' }, this.saving ? '保存中…' : 'Brain に保存…'),
    )
  }

  private viewMessages(): HTMLElement {
    return h('div', { class: 'messages' }, this.messages.map((m) => h('div', { class: `msg ${m.level}` }, m.text)))
  }

  private viewStart(): HTMLElement {
    return h(
      'main',
      { class: 'start' },
      h('h2', null, 'はじめに'),
      h('ol', null,
        h('li', null, 'Brain と PC を USB ケーブルでつなぎ、「Brain に接続」を押します。'),
        h('li', null, 'ポートを選ぶ画面で、Brain（USB 1d6b:0104）のポートを選びます。Brain には 2 つのシリアルがあり、設定用は 2 つ目です（Linux では /dev/ttyACM1）。違うほうを選んだときは、そう表示されます。'),
        h('li', null, 'つながらなくても、「ファイルを開く」で設定ファイル（YAML / JSON）を編集できます。'),
      ),
      this.deps.serial
        ? null
        : h('p', { class: 'warn' }, 'このブラウザは WebSerial に対応していません。Chrome か Edge で、https か localhost のページとして開いてください。'),
    )
  }

  private viewEditor(): HTMLElement {
    return h(
      'main',
      null,
      h('div', { class: 'source' }, `編集中：${this.source}`, this.dirty ? h('span', { class: 'dirty' }, '（未保存の変更あり）') : null),
      this.viewTabs(),
      this.viewLayerProps(),
      h('div', { class: 'columns' },
        h('div', { class: 'left' }, this.viewKeyboard(), this.viewTouch()),
        h('div', { class: 'right' }, this.viewInspector(), this.viewProblems(), this.viewDeviceInfo()),
      ),
    )
  }

  private viewTabs(): HTMLElement {
    const cfg = this.cfg!
    return h(
      'nav',
      { class: 'tabs', role: 'tablist' },
      cfg.layers.map((l, i) => {
        const n = this.problemsAt((loc) => 'layer' in loc && loc.layer === i).length
        return h('button', { role: 'tab', class: ['tab', i === this.layer && 'active'], 'aria-selected': i === this.layer ? 'true' : 'false',
          onclick: () => this.selectLayer(i), dataset: { layer: String(i) } },
          layerTitle(l), i === 0 ? h('small', null, ' base') : null, n ? h('span', { class: 'badge' }, String(n)) : null)
      }),
      h('button', { class: 'tab add', onclick: () => this.addLayer(), title: 'レイヤーを追加' }, '＋ レイヤー'),
    )
  }

  private viewLayerProps(): HTMLElement {
    const l = this.layerCfg!
    const errs = this.problemsAt((loc) => loc.kind === 'layer' && loc.layer === this.layer)
    const refs = references(this.cfg!, l.name).filter((r) => r.layer !== this.layer || this.layer !== 0)
    const how = refs.length
      ? [...new Set(refs.map((r) => LAYER_VERB[r.kind]))].join('・')
      : this.layer === 0 ? '' : 'どこからも切り替えられません'
    return h(
      'div',
      { class: 'layer-props' },
      h('label', null, '名前 ', h('input', { value: l.name, 'data-focus': 'layer-name', size: 10,
        onchange: (e: Event) => this.renameLayer((e.target as HTMLInputElement).value),
        title: 'ほかのレイヤーから参照する名前。変えると、参照している割り当ても書き換えます' })),
      h('label', null, '表示名 ', h('input', { value: l.label ?? '', 'data-focus': 'layer-label', size: 12, placeholder: l.name,
        oninput: (e: Event) => this.setLabel((e.target as HTMLInputElement).value), title: 'Brain の画面の右上に出る名前' })),
      this.layer === 0
        ? h('span', { class: 'hint' }, 'base：いつも一番下にあるレイヤー。消せません')
        : [
            h('span', { class: 'hint' }, how ? `入り方：${how}` : ''),
            h('button', { class: 'danger', onclick: () => this.deleteLayer() }, 'このレイヤーを消す'),
          ],
      errs.map((p) => h('div', { class: 'err' }, p.message)),
    )
  }

  // ---------- キーボード ----------

  private viewKeyboard(): HTMLElement {
    const cfg = this.cfg!
    const unit = 54
    const keys = this.keymap.keys
    const minX = Math.min(...keys.map((k) => k.x))
    const maxX = Math.max(...keys.map((k) => k.x + k.w))
    const rows = Math.max(...keys.map((k) => k.row)) + 1
    const st = editStack(this.layer)
    return h(
      'section',
      { class: 'panel keyboard-panel' },
      h('div', { class: 'panel-head' },
        h('h2', null, 'キーボード'),
        h('label', { class: 'toggle', title: '「記号」を押しているあいだに届くコード（KEY_1 など）を編集します' },
          h('input', { type: 'checkbox', checked: this.symbolMode, onchange: (e: Event) => {
            this.symbolMode = (e.target as HTMLInputElement).checked
            this.render()
          } }), ' 「記号」を押しながら'),
      ),
      h('div', { class: 'keyboard', style: { width: `${(maxX - minX) * unit}px`, height: `${rows * unit}px` } },
        keys.map((k) => this.viewKey(cfg, k, st, unit, minX))),
      h('p', { class: 'legend' },
        h('span', { class: 'sw own' }), 'このレイヤーで割り当て　',
        h('span', { class: 'sw inherited' }), '下のレイヤーから透過　',
        h('span', { class: 'sw none' }), 'none　',
        h('span', { class: 'sw off' }), '使えないキー'),
    )
  }

  private viewKey(cfg: Config, k: PhysKey, st: number[], unit: number, minX: number): HTMLElement {
    const code = this.symbolMode ? k.symbol : k.code
    const style = { left: `${(k.x - minX) * unit}px`, top: `${k.row * unit}px`, width: `${k.w * unit - 4}px`, height: `${unit - 4}px` }
    if (!code) {
      const why = this.symbolMode && k.code ? '「記号」を押しているあいだは届かない' : (k.note ?? '割り当てられない')
      return h('button', { class: 'key off', style, disabled: true, title: `${k.label}：${why}` }, h('span', { class: 'cap' }, k.label))
    }
    const r = resolveKey(cfg, st, code)
    const own = r && r.from === this.layer
    const kind = r ? actionKind(r.action) : null
    const sel: Selection = { kind: 'key', code }
    const selected = this.sel?.kind === 'key' && this.sel.code === code
    const flash = this.flash?.kind === 'key' && this.flash.code === code
    const err = this.problemsAt((loc) => this.selMatches(loc, sel, this.layer)).length > 0
    return h(
      'button',
      {
        class: ['key', own ? 'own' : r ? 'inherited' : 'empty', kind === 'none' && 'none', r && actionKind(r.action).startsWith('layer') && 'layerkey',
          selected && 'selected', flash && 'flash', err && 'error'],
        style,
        title: `${k.label}（${code}）${k.note ? '\n' + k.note : ''}`,
        dataset: { code, id: k.id },
        onclick: () => {
          this.sel = sel
          this.render()
        },
      },
      h('span', { class: 'cap' }, this.symbolMode && k.symbol !== k.code ? `${k.label}→${symbolName(code)}` : k.label),
      h('span', { class: 'act' }, r ? shortAction(cfg, r.action) : ''),
    )
  }

  // ---------- タッチ ----------

  private viewTouch(): HTMLElement | null {
    const cfg = this.cfg!
    if (!cfg.touch) return h('section', { class: 'panel' }, h('h2', null, 'タッチ'), h('p', { class: 'hint' }, 'この設定にはタッチパネルの設定（touch）がありません'))
    const st = editStack(this.layer)
    const g = resolveGrid(cfg, st)
    const W = this.keymap.screen.w
    const H = this.keymap.screen.h
    const cells: HTMLElement[] = []
    for (let r = 0; r < g.rows; r++) {
      for (let c = 0; c < g.cols; c++) {
        const i = r * g.cols + c
        if (g.anchor[i] >= 0 && g.anchor[i] !== i) continue // span のセルに覆われている
        const [w, hh] = g.anchor[i] === i ? spanOf(g.cells[i]?.action) : [1, 1]
        const [x0] = cellSpan(c, g.cols, W)
        const [, x1] = cellSpan(Math.min(c + w, g.cols) - 1, g.cols, W)
        const [y0] = cellSpan(r, g.rows, H)
        const [, y1] = cellSpan(Math.min(r + hh, g.rows) - 1, g.rows, H)
        const sel: Selection = { kind: 'cell', col: c, row: r }
        const selected = this.sel?.kind === 'cell' && this.sel.col === c && this.sel.row === r
        const flash = this.flash?.kind === 'cell' && this.flash.col === c && this.flash.row === r
        const err = this.problemsAt((loc) => this.selMatches(loc, sel, this.layer)).length > 0
        const own = !!this.ownAction(sel)
        cells.push(h('button', {
          class: ['cell', selected && 'selected', flash && 'flash', err && 'error', own && 'own'],
          style: { left: `${(x0 / W) * 100}%`, top: `${(y0 / H) * 100}%`, width: `${((x1 - x0) / W) * 100}%`, height: `${((y1 - y0) / H) * 100}%` },
          title: `セル ${c},${r}（押さえているあいだ、押したときの見た目になります）`, dataset: { cell: cellKey(c, r) },
          onclick: () => {
            this.sel = sel
            this.render()
          },
          onpointerdown: () => this.setPreviewPress(cellKey(c, r)),
          onpointerup: () => this.setPreviewPress(null),
          onpointerleave: () => this.setPreviewPress(null),
          onpointercancel: () => this.setPreviewPress(null),
        }, this.font ? '' : h('span', { class: 'cell-text' }, shortAction(cfg, g.cells[r * g.cols + c]?.action ?? null))))
      }
    }
    return h(
      'section',
      { class: 'panel touch-panel' },
      h('div', { class: 'panel-head' }, h('h2', null, 'タッチ'), this.viewGridControls(), this.viewPressStyle()),
      h('div', { class: 'screen-row' },
        h('div', { class: 'screen', style: { aspectRatio: `${W} / ${H}` } }, h('canvas', { width: W, height: H, class: 'preview' }), cells),
        this.viewSoftStrip(),
      ),
      this.viewTouchSettings(),
    )
  }

  // viewPressStyle は、押しているセルの見せ方（display.press_style）を選ぶ欄。
  // display のほかの項目と違い、保存すれば再起動なしで Brain に反映する。
  private viewPressStyle(): HTMLElement {
    const cfg = this.cfg!
    const cur: PressStyle = cfg.display?.press_style ?? 'border'
    return h('label', { class: 'press-style', title: 'Brain でセルを押しているあいだの見せ方。プレビューのセルを押さえると確かめられます' },
      '押したとき ',
      h('select', { id: 'press-style', 'data-focus': 'press-style',
        onchange: (e: Event) => {
          const v = (e.target as HTMLSelectElement).value as PressStyle
          if (v === cur) return
          ;(cfg.display ??= {}).press_style = v
          this.changed()
        } },
        h('option', { value: 'border', selected: cur === 'border' }, '枠を光らせる（border）'),
        h('option', { value: 'fill', selected: cur === 'fill' }, '塗りつぶす（fill）')))
  }

  private setPreviewPress(cell: string | null): void {
    if (this.previewPress === cell) return
    this.previewPress = cell
    this.drawPreview()
  }

  private viewGridControls(): HTMLElement {
    const cfg = this.cfg!
    const l = this.layerCfg!
    const li = this.layer
    const size = gridSize(cfg, li)
    const errs = this.problemsAt((loc) => loc.kind === 'grid' && loc.layer === li)
    const num = (v: number | undefined, f: (n: number) => void, id: string) =>
      h('input', { type: 'number', min: 1, max: 16, value: v ?? '', 'data-focus': id, class: 'num',
        onchange: (e: Event) => f(Number((e.target as HTMLInputElement).value)) })
    const setSize = (cols: number | undefined, rows: number | undefined) => {
      const out = cellsOutside(l.touch, cols ?? size?.cols ?? 0, rows ?? size?.rows ?? 0)
      if (out.length && !this.deps.confirm(`格子からはみ出すセル ${out.length} 個（${out.join('、')}）の割り当てを消します。よいですか？`)) {
        this.render()
        return
      }
      l.touch ??= {}
      for (const k of out) delete l.touch.cells?.[k]
      if (cols !== undefined) l.touch.cols = cols
      if (rows !== undefined) l.touch.rows = rows
      this.changed()
    }
    let mode: 'inherit' | 'base' | 'own' = !l.touch ? 'inherit' : l.touch.cols || l.touch.rows ? 'own' : 'base'
    const parts: (HTMLElement | string)[] = []
    if (li > 0) {
      parts.push(h('select', {
        'data-focus': 'grid-mode',
        onchange: (e: Event) => {
          const v = (e.target as HTMLSelectElement).value as typeof mode
          if (v === 'inherit') {
            const n = Object.keys(l.touch?.cells ?? {}).length
            if (n && !this.deps.confirm(`このレイヤーのセルの割り当て ${n} 個を消します。よいですか？`)) return this.render()
            delete l.touch
          } else if (v === 'base') {
            const b = gridSize(cfg, 0)
            const out = b ? cellsOutside(l.touch, b.cols, b.rows) : []
            if (out.length && !this.deps.confirm(`格子からはみ出すセル ${out.length} 個の割り当てを消します。よいですか？`)) return this.render()
            l.touch = { cells: l.touch?.cells }
            for (const k of out) delete l.touch.cells?.[k]
          } else {
            const s = gridSize(cfg, li) ?? gridSize(cfg, 0) ?? { cols: 4, rows: 3 }
            l.touch = { cols: s.cols, rows: s.rows, cells: l.touch?.cells }
          }
          this.changed()
        },
      },
      h('option', { value: 'inherit', selected: mode === 'inherit' }, '下のレイヤーのまま（touch なし）'),
      h('option', { value: 'base', selected: mode === 'base' }, 'base と同じ大きさで上書き'),
      h('option', { value: 'own', selected: mode === 'own' }, '独自の大きさ')))
    }
    if (li === 0 || mode === 'own') {
      parts.push(h('label', null, ' 列 ', num(l.touch?.cols, (n) => setSize(n, undefined), `cols-${li}`)))
      parts.push(h('label', null, ' 行 ', num(l.touch?.rows, (n) => setSize(undefined, n), `rows-${li}`)))
    } else if (size) {
      parts.push(h('span', { class: 'hint' }, ` ${size.cols}×${size.rows}`))
    }
    if (li > 0 && mode !== 'inherit' && size && gridSize(cfg, 0) && (size.cols !== gridSize(cfg, 0)!.cols || size.rows !== gridSize(cfg, 0)!.rows))
      parts.push(h('span', { class: 'hint' }, '（base と大きさが違うので、書いていないセルは空になります）'))
    return h('div', { class: 'grid-controls' }, parts, errs.map((p) => h('div', { class: 'err' }, p.message)))
  }

  private softNames(): string[] {
    const t = this.cfg!.touch!
    const areas = Object.entries(t.soft_areas ?? {})
    // 画面の上から順に並べる（Y 軸の向きは min_y と max_y で決まる）
    const pos = ([, a]: [string, { y: [number, number] }]) => ((a.y[0] + a.y[1]) / 2 - t.min_y) / (t.max_y - t.min_y || 1)
    return areas.sort((a, b) => pos(a) - pos(b)).map(([n]) => n)
  }

  private viewSoftStrip(): HTMLElement | null {
    const cfg = this.cfg!
    const names = this.softNames()
    if (!names.length) return null
    const st = editStack(this.layer)
    return h(
      'div',
      { class: 'soft-strip', title: '画面右に印刷された帯。割り当てた区画だけがセルより優先されます' },
      names.map((n) => {
        const r = resolveSoft(cfg, st, n)
        const sel: Selection = { kind: 'soft', name: n }
        const selected = this.sel?.kind === 'soft' && this.sel.name === n
        const flash = this.flash?.kind === 'soft' && this.flash.name === n
        const err = this.problemsAt((loc) => this.selMatches(loc, sel, this.layer)).length > 0
        return h('button', {
          class: ['soft', r ? (r.from === this.layer ? 'own' : 'inherited') : 'empty', selected && 'selected', flash && 'flash', err && 'error'],
          dataset: { soft: n },
          onclick: () => {
            this.sel = sel
            this.render()
          },
        }, h('span', { class: 'cap' }, SOFT_TITLES[n] ?? n), h('span', { class: 'act' }, r ? shortAction(cfg, r.action) : ''))
      }),
    )
  }

  private viewTouchSettings(): HTMLElement {
    const t = this.cfg!.touch!
    const errs = this.problemsAt((loc) => loc.kind === 'touch')
    const numIn = (v: number, f: (n: number) => void, id: string) =>
      h('input', { type: 'number', value: v, class: 'num wide', 'data-focus': id,
        onchange: (e: Event) => { f(Number((e.target as HTMLInputElement).value)); this.changed() } })
    const areas = Object.entries(t.soft_areas ?? {})
    return h(
      'details',
      { class: 'touch-settings', open: errs.length > 0 || undefined },
      h('summary', null, 'タッチパネルの調整（キャリブレーションとソフトキーの区画）'),
      h('p', { class: 'hint' }, '値はパネルの生の座標。min_x などは lefthand -calibrate で測ります（README の「タッチのキャリブレーション」）。'),
      h('div', { class: 'calib' },
        (['min_x', 'max_x', 'min_y', 'max_y'] as const).map((f) => h('label', null, `${f} `, numIn(t[f], (n) => (t[f] = n), f))),
        h('label', null, h('input', { type: 'checkbox', checked: !!t.swap_xy, onchange: (e: Event) => {
          if ((e.target as HTMLInputElement).checked) t.swap_xy = true
          else delete t.swap_xy
          this.changed()
        } }), ' swap_xy')),
      h('table', { class: 'areas' },
        h('thead', null, h('tr', null, ['名前', 'x から', 'x まで', 'y から', 'y まで', ''].map((s) => h('th', null, s)))),
        h('tbody', null, areas.map(([n, a]) => h('tr', null,
          h('td', null, `${n}${SOFT_TITLES[n] ? `（${SOFT_TITLES[n]}）` : ''}`),
          h('td', null, numIn(a.x[0], (v) => (a.x[0] = v), `${n}-x0`)),
          h('td', null, numIn(a.x[1], (v) => (a.x[1] = v), `${n}-x1`)),
          h('td', null, numIn(a.y[0], (v) => (a.y[0] = v), `${n}-y0`)),
          h('td', null, numIn(a.y[1], (v) => (a.y[1] = v), `${n}-y1`)),
          h('td', null, h('button', { class: 'small danger', onclick: () => {
            const used = this.cfg!.layers.filter((l) => l.soft_keys?.[n]).length
            if (!this.deps.confirm(`区画 ${n} を消しますか？${used ? `\n${used} 個のレイヤーの割り当ても消えます。` : ''}`)) return
            delete t.soft_areas![n]
            for (const l of this.cfg!.layers) delete l.soft_keys?.[n]
            this.changed()
          } }, '消す')),
        )))),
      h('button', { class: 'small', onclick: () => {
        const name = prompt('区画の名前（英数字）')?.trim()
        if (!name) return
        if (t.soft_areas?.[name]) return this.say(`区画 ${name} はすでにあります`, 'error')
        ;(t.soft_areas ??= {})[name] = { x: [3740, 4095], y: [0, 0] }
        this.changed()
      } }, '区画を追加'),
      errs.map((p) => h('div', { class: 'err' }, p.message)),
    )
  }

  private drawPreview(): void {
    const canvas = this.root.querySelector<HTMLCanvasElement>('canvas.preview')
    if (!canvas || !this.font || !this.cfg) return
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    const li = this.layer
    const refs = li === 0 ? [] : references(this.cfg, this.cfg.layers[li].name)
    // 入り方で枠の色を変える（切り替えたままなら緑、一時的なら橙）
    const mode: Mode = li === 0 ? 'base' : refs.some((r) => r.kind === 'layer_toggle' || r.kind === 'layer_to') || !refs.length ? 'latched' : 'temp'
    const pressed = new Set(this.previewPress ? [this.previewPress] : [])
    const now = new Date()
    const { pixels } = renderPreview(this.font, { cfg: this.cfg, stack: editStack(li), mode, pressed, w: canvas.width, h: canvas.height, now })
    ctx.putImageData(new ImageData(pixels as any, canvas.width, canvas.height), 0, 0)
    this.schedulePreviewTick(now)
  }

  // schedulePreviewTick は、時計が出ていれば、表示が変わる時刻（次の秒か分）にプレビューを描き直す。
  private schedulePreviewTick(now: Date): void {
    if (this.previewTimer) clearTimeout(this.previewTimer)
    this.previewTimer = null
    const g = resolveGrid(this.cfg!, editStack(this.layer))
    const clocks = g.cells.filter((c) => c?.action.widget === 'clock').map((c) => c!.action)
    if (!clocks.length) return
    const sec = clocks.some((a) => hasSeconds(a.format || DEFAULT_CLOCK_FORMAT) || hasSeconds(a.date_format && a.date_format !== 'none' ? a.date_format : ''))
    const unit = sec ? 1000 : 60000
    this.previewTimer = setTimeout(() => this.drawPreview(), unit - (now.getTime() % unit) + 5)
  }

  // ---------- 選んだものの編集 ----------

  private selTitle(sel: Selection): string {
    if (sel.kind === 'key') {
      const names = this.keymap.keys.filter((k) => k.code === sel.code).map((k) => k.label)
      const sym = this.keymap.keys.filter((k) => k.symbol === sel.code && k.code !== sel.code).map((k) => `記号+${k.label}`)
      return `キー ${[...new Set([...names, ...sym])].join('・') || sel.code}（${sel.code}）`
    }
    if (sel.kind === 'soft') return `ソフトキー ${SOFT_TITLES[sel.name] ?? sel.name}（${sel.name}）`
    return `セル ${sel.col},${sel.row}`
  }

  private viewInspector(): HTMLElement {
    const cfg = this.cfg!
    const sel = this.sel
    const l = this.layerCfg!
    if (!sel) {
      return h('section', { class: 'panel inspector' }, h('h2', null, '割り当て'),
        h('p', { class: 'hint' }, 'キーボードのキー、タッチのセル、右の帯のソフトキーをクリックして選びます。学習モードなら、Brain で押して選べます。'))
    }
    const own = this.ownAction()
    const r = this.resolved()
    const errs = this.problemsAt((loc) => this.selMatches(loc, sel, this.layer))
    const kind: ActionKind | 'inherit' = own ? (own.widget !== undefined ? 'widget' : actionKind(own)) : 'inherit'
    // ウィジェットのセルで、タップしたときに働くもの
    const tap: ActionKind | null = own?.widget !== undefined ? actionKind(own) : null
    const inheritLabel = this.layer === 0 ? '割り当てなし' : '透過（下のレイヤーのものを使う）'
    const notes = sel.kind === 'key' ? this.keymap.keys.filter((k) => k.code === sel.code || k.symbol === sel.code).map((k) => k.note).filter(Boolean) : []

    const body: (HTMLElement | null)[] = []
    if (!own && r && r.from !== this.layer)
      body.push(h('p', { class: 'inherit-note' }, `今は「${layerTitle(cfg.layers[r.from])}」の ${describeAction(r.action)} を使っています`))
    body.push(h('label', { class: 'row' }, '種類 ', h('select', { 'data-focus': 'kind', id: 'kind',
      onchange: (e: Event) => this.setKind((e.target as HTMLSelectElement).value as ActionKind | 'inherit') },
      h('option', { value: 'inherit', selected: kind === 'inherit' }, inheritLabel),
      (Object.keys(KIND_LABELS) as ActionKind[]).filter((k) => k !== 'widget' || sel.kind === 'cell')
        .map((k) => h('option', { value: k, selected: kind === k }, KIND_LABELS[k])))))

    if (own && kind === 'widget') body.push(this.viewWidgetEditor(own))
    if (tap) {
      body.push(h('label', { class: 'row' }, 'タップしたとき ', h('select', { 'data-focus': 'tap', id: 'tap',
        onchange: (e: Event) => this.setTap((e.target as HTMLSelectElement).value as ActionKind) },
        h('option', { value: 'widget', selected: tap === 'widget' }, '何もしない'),
        (['key', ...LAYER_KINDS] as ActionKind[]).map((k) => h('option', { value: k, selected: tap === k }, KIND_LABELS[k])))))
    }
    const act = tap ?? kind
    if (own && act === 'key') body.push(this.viewComboEditor(own))
    if (own && LAYER_KINDS.includes(act as LayerKind)) {
      const lk = act as LayerKind
      body.push(h('label', { class: 'row' }, '行き先 ', h('select', { 'data-focus': 'target', id: 'target',
        onchange: (e: Event) => this.patchAction({ [lk]: (e.target as HTMLSelectElement).value }) },
        h('option', { value: '', selected: !own[lk] }, '（選んでください）'),
        cfg.layers.map((x, i) => h('option', { value: x.name, selected: own[lk] === x.name, disabled: lk === 'layer_toggle' && i === 0 },
          `${layerTitle(x)}（${x.name}）${i === 0 ? ' base' : ''}`)))))
      if (lk === 'layer_toggle') body.push(h('p', { class: 'hint' }, 'base は外せないので、base に戻るには「そのレイヤーへ移る」で base を選びます。'))
    }
    if (own && kind !== 'inherit') {
      const missing = own.label && this.font ? this.font.missing(own.label) : []
      body.push(h('label', { class: 'row' }, sel.kind === 'key' ? '表示名（画面には出ません） ' : kind === 'widget' ? '見出し ' : '表示名 ',
        h('textarea', { rows: 2, 'data-focus': 'label', id: 'label',
          placeholder: kind === 'widget' ? '省略可。セルの上に小さく出します' : '省略するとキーの名前を出します',
          value: own.label ?? '', oninput: (e: Event) => this.patchAction({ label: (e.target as HTMLTextAreaElement).value }) })))
      if (missing.length) body.push(h('div', { class: 'warn' }, `Brain のフォントにない文字があります（□ になります）：${missing.join(' ')}`))
    }
    if (own && kind !== 'inherit' && kind !== 'none' && sel.kind === 'cell') body.push(this.viewSpanEditor(own))
    for (const n of notes) body.push(h('p', { class: 'hint' }, n!))
    for (const p of errs) body.push(h('div', { class: 'err' }, p.message))

    return h('section', { class: 'panel inspector' },
      h('h2', null, '割り当て'),
      h('div', { class: 'sel-title' }, `レイヤー「${layerTitle(l)}」/ ${this.selTitle(sel)}`),
      body)
  }

  // viewWidgetEditor は、ウィジェットの種類と書式の欄。書式は Go の time.Format の形。
  private viewWidgetEditor(own: ActionSpec): HTMLElement {
    const text = (f: 'format' | 'date_format' | 'tz', label: string, placeholder: string, title: string) =>
      h('label', { class: 'row', title }, label, h('input', { value: own[f] ?? '', 'data-focus': f, id: f, placeholder,
        onchange: (e: Event) => this.patchAction({ [f]: (e.target as HTMLInputElement).value.trim() }) }))
    const missing = this.font ? this.font.missing([own.format, own.date_format].filter(Boolean).join('')) : []
    return h('div', { class: 'widget-editor' },
      h('label', { class: 'row' }, 'ウィジェット ', h('select', { 'data-focus': 'widget', id: 'widget',
        onchange: (e: Event) => this.patchAction({ widget: (e.target as HTMLSelectElement).value as WidgetKind }) },
        (Object.keys(WIDGET_LABELS) as WidgetKind[]).map((w) => h('option', { value: w, selected: own.widget === w }, WIDGET_LABELS[w])))),
      text('format', '時刻の書式 ', DEFAULT_CLOCK_FORMAT, '例：15:04（24 時間）、15:04:05（秒も出す。1 秒ごとに描き直す）、3:04 PM'),
      text('date_format', '日付の書式 ', DEFAULT_DATE_FORMAT, '例：1月2日({wday})、2006/01/02 Mon。none で日付を出さない。{wday} は日本語の曜日'),
      text('tz', 'タイムゾーン ', 'Brain のタイムゾーン', '例：Asia/Tokyo、America/Los_Angeles、UTC。省略すると Brain のタイムゾーン'),
      h('p', { class: 'hint' }, '書式は Go の書き方です（2006=年、01 か 1=月、02 か 2=日、15=時、04=分、05=秒、Mon=曜日、{wday}=日本語の曜日）。時刻を一度も合わせていないあいだは、Brain では「時刻未設定」と橙色で出ます。'),
      missing.length ? h('div', { class: 'warn' }, `Brain のフォントにない文字があります（□ になります）：${missing.join(' ')}`) : null,
    )
  }

  // viewSpanEditor は、セルの大きさ（span）の欄。右と下のセルを覆う。
  private viewSpanEditor(own: ActionSpec): HTMLElement {
    const [w, hh] = spanOf(own)
    const num = (v: number, f: (n: number) => void, id: string) =>
      h('input', { type: 'number', min: 1, max: 16, value: v, 'data-focus': id, id, class: 'num',
        onchange: (e: Event) => f(Number((e.target as HTMLInputElement).value)) })
    return h('div', { class: 'row span-editor', title: 'セルを右と下に広げます。覆ったセルの割り当ては、このレイヤーに書いてあると誤りになります' },
      '大きさ ', h('label', null, '列 ', num(w, (n) => this.setSpan(n, hh), 'span-w')), h('label', null, ' 行 ', num(hh, (n) => this.setSpan(w, n), 'span-h')))
  }

  private viewComboEditor(own: ActionSpec): HTMLElement {
    const text = own.key ?? ''
    const combo = parseCombo(text)
    const mods = new Set(combo?.mods ?? [])
    const main = combo?.keys[0] ?? ''
    const set = (m: Set<Modifier>, k: string, rest: string[] = combo?.keys.slice(1) ?? []) =>
      this.patchAction({ key: formatCombo({ mods: [...m], keys: k ? [k, ...rest] : rest }) })
    return h(
      'div',
      { class: 'combo' },
      h('div', { class: 'mods' }, MODIFIERS.map((m) => h('label', { class: 'mod' },
        h('input', { type: 'checkbox', checked: mods.has(m), onchange: (e: Event) => {
          const n = new Set(mods)
          if ((e.target as HTMLInputElement).checked) n.add(m)
          else n.delete(m)
          set(n, main)
        } }), ` ${m}`))),
      h('label', { class: 'row' }, 'キー ', h('select', { 'data-focus': 'main-key', id: 'main-key', onchange: (e: Event) => set(mods, (e.target as HTMLSelectElement).value) },
        h('option', { value: '', selected: !main }, '（修飾キーだけ）'),
        KEY_GROUPS.map((g) => h('optgroup', { label: g.title }, g.keys.map((k) => h('option', { value: k, selected: main === k }, keyTitle(k))))))),
      h('div', { class: 'row' },
        this.capturing
          ? [h('span', { class: 'capturing' }, 'PC のキーボードで押してください…'), h('button', { onclick: () => this.cancelCapture() }, 'やめる')]
          : h('button', { id: 'capture', onclick: () => this.startCapture(), title: '例：Ctrl+Shift+Z を押すと LCTRL+LSHIFT+Z になります。Ctrl+W など、ブラウザが先に使うキーは取り込めません' }, 'PC のキーで入力')),
      h('label', { class: 'row' }, '送るキー ', h('input', { value: text, 'data-focus': 'combo-text', id: 'combo-text', class: combo || !text ? '' : 'bad',
        onchange: (e: Event) => this.patchAction({ key: (e.target as HTMLInputElement).value.toUpperCase().replace(/\s+/g, '') }) }),
        text ? h('span', { class: 'pretty' }, ` → ${prettyCombo(text)}`) : null),
      combo && combo.keys.length > 6 ? h('div', { class: 'warn' }, '同時に送れる通常キーは 6 つまでです') : null,
    )
  }

  // ---------- そのほかの表示 ----------

  private viewProblems(): HTMLElement {
    const label = {
      idle: '', pending: '検証中…', ok: '✔ 誤りはありません', invalid: `✖ 誤りが ${this.problems.length} 件あります`,
      offline: 'Brain に接続していないので、検証していません', error: '検証できませんでした',
    }[this.validation]
    return h(
      'section',
      { class: 'panel problems' },
      h('h2', null, '検証'),
      h('div', { class: `vstate ${this.validation}` }, label),
      h('ul', null, this.problems.map((p) => h('li', null, h('a', { href: '#', onclick: (e: Event) => {
        e.preventDefault()
        this.goTo(p)
      } }, p.message), p.path ? h('code', null, ` ${p.path}`) : null))),
      this.warnings.length ? h('ul', { class: 'warnings' }, this.warnings.map((w) => h('li', null, `注意：${w}`))) : null,
    )
  }

  private viewDeviceInfo(): HTMLElement {
    const c = this.cfg!
    return h(
      'details',
      { class: 'panel info' },
      h('summary', null, 'キーボードの制約と、そのほかの設定'),
      h('ul', null, this.keymap.constraints.map((s) => h('li', null, s))),
      h('p', { class: 'hint' }, '次の項目は、デーモンを再起動しないと変えられないため、この画面では変えられません。変えるときは /etc/lefthand/config.yaml を直接編集し、サービスを再起動します。display のうち press_style（押したときの見せ方）だけは、タッチの欄で変えられます。'),
      h('table', { class: 'kv' },
        [['hid_device', c.hid_device], ['keyboard', c.keyboard], ['touch.device', c.touch?.device], ['display', c.display ? JSON.stringify(c.display) : '']]
          .map(([k, v]) => h('tr', null, h('th', null, k as string), h('td', null, (v as string) ?? '（既定）')))),
    )
  }

  private viewDiff(): HTMLElement {
    const d = this.diff!
    const cell = (s: string | null, cls: string) => h('td', { class: cls }, s === null ? '—' : s)
    return h(
      'div',
      { class: 'modal-back', onclick: (e: Event) => {
        if (e.target === e.currentTarget) {
          this.diff = null
          this.render()
        }
      } },
      h('div', { class: 'modal', role: 'dialog', 'aria-label': '保存する変更' },
        h('h2', null, `Brain に保存する変更（${d.length} 件）`),
        h('div', { class: 'diff-wrap' }, h('table', { class: 'diff' },
          h('thead', null, h('tr', null, h('th', null, '場所'), h('th', null, '今'), h('th', null, '変更後'))),
          h('tbody', null, d.map((c) => h('tr', { class: c.before === null ? 'added' : c.after === null ? 'removed' : 'changed' },
            h('td', null, c.where), cell(c.before, 'before'), cell(c.after, 'after')))))),
        this.problems.length ? h('p', { class: 'err' }, `誤りが ${this.problems.length} 件あります。直してから保存してください。`) : null,
        h('p', { class: 'hint' }, '保存すると、押しているキーをすべて離してから、すぐに反映します。前の設定は config.yaml.prev に残ります。ファイルのコメントは消えます。'),
        h('div', { class: 'buttons' },
          h('button', { onclick: () => { this.diff = null; this.render() } }, 'やめる'),
          h('button', { class: 'primary', id: 'confirm-save', disabled: this.saving || this.problems.length > 0 || this.validation === 'pending', onclick: () => void this.save() }, this.saving ? '保存中…' : '保存して反映する'))),
    )
  }
}

// shortAction はキーやセルに小さく出す割り当ての説明。
export function shortAction(cfg: Config, a: ActionSpec | null): string {
  if (!a) return ''
  if (a.widget) return a.label ? `${WIDGET_LABELS[a.widget] ?? a.widget}：${a.label.replace(/\n/g, ' ')}` : (WIDGET_LABELS[a.widget] ?? a.widget)
  const k = actionKind(a)
  if (k === 'none') return '✕'
  if (k === 'key') return a.label ? a.label.replace(/\n/g, ' ') : prettyCombo(a.key ?? '')
  const t = actionTarget(a)
  const dest = cfg.layers.find((l) => l.name === t)
  return `${LAYER_VERB[k as LayerKind]}→${dest ? layerTitle(dest) : (t ?? '?')}`
}

// symbolName は「記号」を押しながらのコードを短く書く（KEY_GRAVE → `）。
function symbolName(code: string): string {
  const n = code.replace(/^KEY_/, '')
  return PRETTY[n] ?? n
}

// formatOffset は、時刻のずれを「1 日 13 時間」のように書く。
export function formatOffset(ms: number): string {
  let s = Math.round(Math.abs(ms) / 1000)
  const parts: string[] = []
  for (const [n, u] of [[86400, '日'], [3600, '時間'], [60, '分']] as const) {
    if (s >= n) {
      parts.push(`${Math.floor(s / n)} ${u}`)
      s %= n
    }
  }
  if (!parts.length || (parts.length === 1 && s)) parts.push(`${s} 秒`)
  return parts.slice(0, 2).join(' ')
}

// utcOffset は、UTC からのずれを「UTC+9」「UTC-7」「UTC+5:30」のように書く。
export function utcOffset(sec: number): string {
  const a = Math.abs(sec)
  const m = Math.floor(a / 60) % 60
  return `UTC${sec < 0 ? '-' : '+'}${Math.floor(a / 3600)}${m ? `:${String(m).padStart(2, '0')}` : ''}`
}
