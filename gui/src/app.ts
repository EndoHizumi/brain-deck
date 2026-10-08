// 設定 GUI の画面。状態は App が持ち、変わるたびに描き直す。

import { diffConfigs, type Change } from './diff'
import { h, replaceKeepingFocus } from './dom'
import { loadFont, type BitmapFont } from './font'
import {
  ComboCapture, KEY_GROUPS, MODIFIERS, PRETTY, formatCombo, keyTitle, parseCombo, prettyCombo, type Modifier,
} from './keys'
import defaultKeymap from './keymap-pwsh2.json'
import {
  CLOCK_FIELDS, DEFAULT_CLOCK_FORMAT, DEFAULT_DATE_FORMAT, KIND_LABELS, LAYER_KINDS, LAYER_VERB, MOUSE_LABELS, PAD_DEFAULTS, PAD_FIELDS,
  PAD_NUM_FIELDS, TERM_LABELS, USB_LABELS, WIDGET_FIELDS, ACTION_FIELDS, WIDGET_LABELS, ownsTouch,
  actionKind, actionTarget, addLayer, anchorOf, cellKey, cellsOutside, clean, deleteLayer, describeAction, editStack,
  isIncomplete, gridSize, layerTitle, normalizeConfig, parsePath, references, renameLayer, resolveCell, resolveGrid,
  resolveKey, resolveSoft, setCellAction, setKeyAction, setSoftAction, spanOf, touchCell, type ActionKind, type LayerKind,
  type Location, type ResolvedAction,
} from './model'
import { hasSeconds } from './clock'
import { CELL_GAP, cellSpan, renderPreview, type Mode } from './preview'
import {
  DEFAULT_BRIGHTNESS, DITHER_LABELS, MAX_ZOOM, checkTarget, convert, decodeFile, decodeImageFile, encodeImageFile, fitCrop, fromBase64,
  sha256Hex, toBase64, type ConvertParams, type Crop, type Dither, type Image565, type SourceImage,
} from './image'
import { TEXT_ID_PATTERN, TEXT_STYLES, textExpired } from './textwidget'
import { TODO_MAX_ROWS, TODO_MAX_RUNES, todoOrder } from './todowidget'
import { DEFAULT_PAGE_RESET, calShown, calWidgetOf, parseDuration } from './calwidget'
import { Client, PROTOCOL_VERSION, ProtocolError, type Transport } from './protocol'
import { BRAIN_FILTER, WebSerialTransport, serialSupported } from './serial'
import { PortRoles, isBrainPort, looksLikeConsole, looksLikePrompt } from './ports'
import { ConsoleSession, sttyCommand, type TermLike } from './console'
import type {
  ActionSpec, BrainImage, Config, EngineStatus, GetTextResult, ImageListResult, HelloResult, InputEvent, KeymapInfo, LayerConfig, Notification, PhysKey,
  PressStyle, Problem, SetTimeResult, TextEntry, CalendarData, TodoItem, TodoList, TodoResult, ValidateResult, WidgetKind, MouseAction, UsbMode, TermInfo, TermMode,
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

// 背景画像を置く場所：セルか、レイヤーの壁紙
export type BgTarget = { kind: 'cell'; layer: number; col: number; row: number } | { kind: 'wallpaper'; layer: number }

// このタブで持っている背景画像。file は Brain に送るファイル（このタブで変換したもの、または Brain から読んだもの）
interface LocalImage {
  id: string
  name: string
  img: Image565
  file?: Uint8Array
  source?: { src: SourceImage; params: ConvertParams; name: string } // 切り抜きを直すための元の画像
}

// 切り抜きの画面の状態
interface ImageEdit {
  target: BgTarget
  src: SourceImage
  name: string
  tw: number
  th: number
  params: ConvertParams
  result: { img: Image565; file: Uint8Array; crop: Crop } | null
  showPressed: boolean
}

// referencedImages は、設定で使っている背景画像の id。
export function referencedImages(cfg: Config | null): Set<string> {
  const out = new Set<string>()
  for (const l of cfg?.layers ?? []) {
    if (l.touch?.background) out.add(l.touch.background)
    for (const a of Object.values(l.touch?.cells ?? {})) if (a.background) out.add(a.background)
  }
  return out
}

export function kb(n: number): string {
  return n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.ceil(n / 1024)} KB`
}

export interface AppDeps {
  serial?: Serial | null // navigator.serial。テストではモック
  decodeImage?: (file: Blob) => Promise<SourceImage> // 画像ファイルを読む。テストではモック
  openTransport?: (port: SerialPort) => Promise<Transport>
  loadFont?: () => Promise<BitmapFont>
  confirm?: (msg: string) => boolean
  validateDelayMs?: number
  helloTimeoutMs?: number
  keepaliveMs?: number
  createTerminal?: () => Promise<TermLike> // コンソールのタブの端末（xterm.js）。テストでは模擬
  // 一覧から選んだポートに hello を送る前に、何も書かずに待つ時間。コンソール（ログイン画面）の文字が届いたら、送らない
  sniffMs?: number
}

// ソフトキーの名前と、帯に印刷された文字
const SOFT_TITLES: Record<string, string> = {
  home: 'HOME', up: '▲', down: '▼', right: '▶', left: '◀', enter: '決定', back: '戻る', menu: '操作機能',
}

type TryResult = { result: 'ok' } | { result: 'no_answer' } | { result: 'console' } | { result: 'open_failed'; error: string }

// openFailedMessage は、ポートを開けなかったときの案内。Linux では権限がないことが多い。
function openFailedMessage(err: string): string {
  return (
    `シリアルポートを開けません（${err}）。` +
    'Linux では、/dev/ttyACM* を開く権限が要ります。' +
    '「sudo usermod -aG dialout $USER」のあとログインし直すか、今だけなら「sudo setfacl -m u:$USER:rw /dev/ttyACM1」を実行してください' +
    '（setfacl はケーブルを抜き差しすると消えます）。ほかのタブやアプリがポートを使っているときも開けません。' +
    'brain-deck コマンドが動いているあいだ（ふつうは 1 秒以内）も開けないので、少し待ってからもう一度押してください'
  )
}

export class App {
  // 接続
  client: Client | null = null
  hello: HelloResult | null = null
  connecting = false
  // Brain の 2 つのシリアルの、どちらが設定用でどちらがコンソール用か（ports.ts）。コンソールのタブと共有する
  ports = new PortRoles()
  private settingsPort: SerialPort | null = null
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
  brainMouse: boolean | null = null // Brain のガジェットにマウスがあるか（get_status の hid.mouse。古い lefthand なら null）
  brainTerm: TermInfo | null = null // Brain の端末モード（get_status の terminal。古い lefthand なら null）
  flash: Selection | null = null
  // プレビューで、マウスで押さえているセル（"列,行"）。押したときの見た目で描く
  previewPress: string | null = null
  // テキストのタイルの中身（接続したときに Brain から読む）。GUI の接続中は brain-deck が書けないので、読み直さない
  texts: Record<string, TextEntry> = {}
  // 画面の切り替え：設定（キーとタッチ）か、Todo か、コンソールか
  section: 'config' | 'todo' | 'console' = 'config'
  // コンソールのタブ（Brain の 1 つ目のシリアル。設定のタブとは別のポートで、同時に開いておける）
  console: ConsoleSession
  private main: HTMLElement // 作り直す画面。端末（console.host）はその外に置き、作り直さない
  // Todo の一覧（Brain のデータが正。接続したときに読み、Brain で変わると通知が届く）
  todo: TodoList | null = null
  // カレンダーの予定（Brain のデータ。接続したときに読む。brain-deck calendar sync が送る）
  calendar: CalendarData | null = null
  todoBusy = false
  todoNew = '' // 追加の欄に書きかけの文
  private todoDrafts: Record<string, string> = {} // 書き換えている途中の項目の文（通知で描き直しても消えない）
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
  // 背景画像
  images = new Map<string, LocalImage>()
  brainImages: ImageListResult | null = null // Brain にある画像（list_images）
  imageEdit: ImageEdit | null = null
  imageBusy: string | null = null // 画像を送っているあいだの表示
  private imageFetching = new Set<string>()
  private editSrcCanvas: HTMLCanvasElement | null = null
  private editDrag: { x: number; y: number; cx: number; cy: number; scale: number } | null = null
  private editFrame = 0
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
      sniffMs: deps.sniffMs ?? 300,
      createTerminal: deps.createTerminal ?? (() => import('./terminal').then((m) => m.createXterm())),
      decodeImage: deps.decodeImage ?? decodeFile,
    }
    this.main = document.createElement('div')
    this.console = new ConsoleSession({
      serial: this.deps.serial,
      openTransport: this.deps.openTransport,
      ports: this.ports,
      createTerminal: this.deps.createTerminal,
      onChange: () => this.render(),
    })
    this.root.replaceChildren(this.main, this.console.host)
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
    this.deps.serial?.addEventListener?.('disconnect', (e: Event) => {
      // ケーブルを抜くと、つなぎ直したときには別の SerialPort になるので、覚えた役割を忘れる。
      // 接続していれば、読み込みループの終わりで onClose が呼ばれる
      if (e.target) {
        this.ports.forget(e.target as SerialPort)
        this.console.forget(e.target as SerialPort)
      }
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
      // 設定用だと分かっているポート（ports.ts）だけを、聞かずに使う。分からないポートには、何も書かない
      const brain = (await serial.getPorts()).filter(isBrainPort)
      const known = this.ports.pick('settings', brain)
      if (known) {
        const r = await this.tryPort(known, false)
        if (r.result === 'open_failed') this.say(openFailedMessage(r.error), 'error')
        else if (r.result === 'no_answer')
          this.say('設定用のポートは開けましたが、lefthand が答えません。lefthand.service が動いているかを確かめてください（ssh brain systemctl status lefthand）', 'error')
        return
      }
      this.say(
        brain.length
          ? 'Brain のシリアルは 2 つあり、ブラウザからはどちらが設定用か見分けられません。一覧から設定用（2 つ目。Linux では ttyACM1、macOS では番号の大きい cu.usbmodem…）を選んでください'
          : 'ポートの一覧から、Brain の設定用のポート（2 つ目。Linux では ttyACM1）を選んでください',
      )
      let port: SerialPort
      try {
        port = await serial.requestPort({ filters: [BRAIN_FILTER] })
      } catch {
        this.say('ポートが選ばれませんでした')
        return
      }
      const conflict = this.ports.conflict(port, 'settings')
      if (conflict) {
        this.say(`${conflict}。もう一度「Brain に接続」を押して、もう一方を選んでください`, 'error')
        return
      }
      const r = await this.tryPort(port, true)
      if (r.result === 'open_failed') this.say(openFailedMessage(r.error), 'error')
      else if (r.result === 'console')
        this.say('このポートからは、lefthand ではない文字（ログイン画面やシェル）が届きました。コンソール用のポートかもしれないので、何も送っていません。もう一度「Brain に接続」を押して、もう一方を選んでください', 'error')
      else if (r.result === 'no_answer')
        this.say(
          'このポートは開けましたが、lefthand が答えません。Brain のシリアルは 2 つあり、もう一方（コンソール用）を選んだかもしれません。' +
            'もう一度「接続」を押して、別のポートを選んでください（コンソール用に送った文字は、ログイン画面に入力されています。コンソールのタブで Enter を押すか、60 秒待つと消えます）',
          'error',
        )
    } finally {
      this.connecting = false
      this.render()
    }
  }

  // tryPort はポートを開いて hello を送る。lefthand が答えたら、そのまま使う。
  // sniff のときは、送る前に少し待ち、コンソールの文字（ログイン画面など）が届いたら送らない。
  private async tryPort(port: SerialPort, sniff: boolean): Promise<TryResult> {
    let t: Transport
    try {
      t = await this.deps.openTransport(port)
    } catch (e: any) {
      return { result: 'open_failed', error: String(e?.message ?? e) }
    }
    this.ports.use(port, 'settings')
    if (sniff && this.deps.sniffMs > 0) {
      const dec = new TextDecoder()
      let got = ''
      t.onData = (b) => (got += dec.decode(b, { stream: true }))
      t.onClose = () => {}
      await new Promise((r) => setTimeout(r, this.deps.sniffMs))
      // lefthand は、聞かれるまで何も送らない。JSON ではない文字が届いたら送らない（コンソールの可能性がある）。
      // 役割を覚えるのは、ログイン画面かシェルのプロンプトが見えたときだけ
      if (looksLikeConsole(got)) {
        if (looksLikePrompt(got)) this.ports.learn(port, 'console')
        this.ports.release(port)
        await t.close().catch(() => {})
        return { result: 'console' }
      }
    }
    const c = new Client(t)
    try {
      await c.start()
      const hello = await c.request<HelloResult>('hello', {}, this.deps.helloTimeoutMs)
      if (hello.daemon !== 'lefthand') throw new Error('not lefthand')
      this.ports.learn(port, 'settings')
      if (hello.protocol !== PROTOCOL_VERSION) {
        this.say(`プロトコルの版が違います（Brain: ${hello.protocol}、GUI: ${PROTOCOL_VERSION}）。どちらかを更新してください`, 'error')
        this.ports.release(port)
        await c.close()
        return { result: 'ok' } // 設定用のポートではあった
      }
      c.maxLine = hello.max_line
      this.settingsPort = port
      this.attach(c, hello)
      await this.syncTime()
      await this.loadFromBrain()
      return { result: 'ok' }
    } catch {
      this.ports.release(port)
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
      this.releaseSettingsPort()
      this.client = null
      this.hello = null
      this.stopLearning(false)
      this.brainStatus = null
      this.brainMouse = null
      this.brainTerm = null
      this.todo = null
      this.calendar = null
      this.brainImages = null
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

  private releaseSettingsPort(): void {
    if (this.settingsPort) this.ports.release(this.settingsPort)
    this.settingsPort = null
  }

  async disconnect(): Promise<void> {
    this.stopLearning(true)
    const c = this.client
    this.releaseSettingsPort()
    this.client = null
    this.hello = null
    this.brainStatus = null
    this.brainMouse = null
    this.todo = null
    this.calendar = null
    this.brainImages = null
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
    const st = await c.request<{ status: EngineStatus; hid?: { mouse: boolean }; terminal?: TermInfo }>('get_status')
    this.brainStatus = st.status
    this.brainMouse = st.hid ? st.hid.mouse : null
    this.brainTerm = st.terminal ?? null
    if (this.hello?.commands?.includes('get_text')) this.texts = (await c.request<GetTextResult>('get_text')).texts ?? {}
    await this.loadTodo()
    this.calendar = this.hello?.commands?.includes('get_calendar') ? await c.request<CalendarData>('get_calendar') : null
    this.render()
    this.scheduleValidate(0)
    void this.loadImages()
  }

  // ---------- 背景画像 ----------

  get imagesSupported(): boolean {
    return !!this.hello?.commands?.includes('image_begin')
  }

  // loadImages は Brain にある画像の一覧を読み、設定で使っていてこのタブにない画像を、プレビューのために読む。
  async loadImages(): Promise<void> {
    if (!this.connected || !this.imagesSupported) {
      this.brainImages = null
      return
    }
    try {
      this.brainImages = await this.client!.request<ImageListResult>('list_images')
    } catch (e: any) {
      this.say(`Brain の背景画像の一覧を読めません：${e?.message ?? e}`, 'error')
      return
    }
    this.render()
    const want = new Set([...referencedImages(this.cfg), ...referencedImages(this.saved)])
    for (const id of want) {
      if (this.images.has(id) || this.imageFetching.has(id) || !this.onBrain(id)) continue
      await this.fetchImage(id)
    }
  }

  // fetchImage は Brain の画像を get_image で分けて読む（プレビュー用）。
  private async fetchImage(id: string): Promise<void> {
    this.imageFetching.add(id)
    try {
      const parts: Uint8Array[] = []
      let off = 0
      for (;;) {
        const r = await this.client!.request<{ data: string; bytes: number; sha256: string }>('get_image', { image: id, offset: off }, 15000)
        const d = fromBase64(r.data)
        parts.push(d)
        off += d.length
        if (!d.length || off >= r.bytes) break
      }
      const file = new Uint8Array(off)
      let o = 0
      for (const p of parts) {
        file.set(p, o)
        o += p.length
      }
      const img = decodeImageFile(file)
      if (!img || (await sha256Hex(file)).slice(0, 16) !== id) throw new Error('壊れています')
      this.images.set(id, { id, name: this.brainImage(id)?.name ?? '', img, file })
      this.render()
    } catch (e: any) {
      this.say(`背景画像 ${id} を Brain から読めません（プレビューには出ません）：${e?.message ?? e}`, 'error')
    } finally {
      this.imageFetching.delete(id)
    }
  }

  brainImage(id: string): BrainImage | undefined {
    return this.brainImages?.images.find((i) => i.id === id)
  }

  onBrain(id: string): boolean {
    return !!this.brainImage(id)
  }

  imageName(id: string): string {
    return this.images.get(id)?.name || this.brainImage(id)?.name || id
  }

  // bgTargetSize は、背景画像の大きさ（セルの箱か、画面全体）。display.go と同じ範囲。
  bgTargetSize(t: BgTarget): { w: number; h: number } {
    const W = this.keymap.screen.w
    const H = this.keymap.screen.h
    if (t.kind === 'wallpaper') return { w: W, h: H }
    const g = resolveGrid(this.cfg!, editStack(t.layer))
    const [sw, sh] = spanOf(this.cfg!.layers[t.layer].touch?.cells?.[cellKey(t.col, t.row)])
    const [x0] = cellSpan(t.col, g.cols, W)
    const [, x1] = cellSpan(Math.min(t.col + sw, g.cols) - 1, g.cols, W)
    const [y0] = cellSpan(t.row, g.rows, H)
    const [, y1] = cellSpan(Math.min(t.row + sh, g.rows) - 1, g.rows, H)
    return { w: x1 - x0 - 2 * CELL_GAP, h: y1 - y0 - 2 * CELL_GAP }
  }

  bgOf(t: BgTarget): string | undefined {
    const l = this.cfg?.layers[t.layer]
    if (t.kind === 'wallpaper') return l?.touch?.background
    return l?.touch?.cells?.[cellKey(t.col, t.row)]?.background
  }

  // setBg は背景画像を設定する（id が undefined なら外す）。描き直しと検証は呼び出し側で。
  setBg(t: BgTarget, id: string | undefined): void {
    const l = this.cfg?.layers[t.layer]
    if (!l) return
    if (t.kind === 'cell') {
      const a = l.touch?.cells?.[cellKey(t.col, t.row)]
      if (!a) return
      if (id) a.background = id
      else delete a.background
      return
    }
    if (id) (l.touch ??= {}).background = id
    else if (l.touch) {
      delete l.touch.background
      // 壁紙のためだけに作った touch は消す（base と同じ大きさで、セルもない）
      if (t.layer !== 0 && Object.keys(l.touch).length === 0) delete l.touch
    }
  }

  // openBgFile は画像ファイルを読み、切り抜きの画面を開く。
  async openBgFile(t: BgTarget, file: Blob & { name?: string }): Promise<void> {
    const { w, h: hh } = this.bgTargetSize(t)
    const bad = checkTarget(w, hh)
    if (bad) return this.say(bad, 'error')
    let src: SourceImage
    try {
      src = await this.deps.decodeImage(file)
    } catch (e: any) {
      return this.say(`画像を読み込めません（PNG、JPEG、WebP が使えます）：${e?.message ?? e}`, 'error')
    }
    this.startImageEdit(t, src, file.name || '画像', { cx: src.w / 2, cy: src.h / 2, zoom: 1, brightness: DEFAULT_BRIGHTNESS, dither: 'fs' })
  }

  // reEditBg は、このタブで選んだ画像の切り抜きを直す。
  reEditBg(t: BgTarget): void {
    const id = this.bgOf(t)
    const s = id ? this.images.get(id)?.source : undefined
    if (s) this.startImageEdit(t, s.src, s.name, { ...s.params })
  }

  private startImageEdit(t: BgTarget, src: SourceImage, name: string, params: ConvertParams): void {
    const { w, h: hh } = this.bgTargetSize(t)
    this.imageEdit = { target: t, src, name, tw: w, th: hh, params, result: null, showPressed: t.kind === 'cell' }
    this.editSrcCanvas = null
    this.updateImageEdit({})
    this.render()
  }

  // updateImageEdit は、切り抜きの設定を変えて変換し直す。DOM は作り直さず、絵だけを描き直す。
  updateImageEdit(p: Partial<ConvertParams>): void {
    const e = this.imageEdit
    if (!e) return
    Object.assign(e.params, p)
    e.params.zoom = Math.min(Math.max(e.params.zoom, 1), MAX_ZOOM)
    const c = fitCrop(e.src.w, e.src.h, e.tw, e.th, e.params.zoom, e.params.cx, e.params.cy)
    e.params.cx = c.x + c.w / 2
    e.params.cy = c.y + c.h / 2
    e.result = convert(e.src, e.tw, e.th, e.params)
    this.drawImageEdit()
  }

  // applyImageEdit は、切り抜いた画像をセルかレイヤーの背景にする。Brain には保存するときに送る。
  async applyImageEdit(): Promise<void> {
    const e = this.imageEdit
    if (!e?.result) return
    const file = e.result.file
    const id = (await sha256Hex(file)).slice(0, 16)
    this.images.set(id, { id, name: e.name, img: e.result.img, file, source: { src: e.src, params: { ...e.params }, name: e.name } })
    this.setBg(e.target, id)
    this.imageEdit = null
    this.editSrcCanvas = null
    this.say(`背景画像「${e.name}」を${e.target.kind === 'wallpaper' ? '壁紙に' : 'セルに'}しました。「Brain に保存」で送ります`, 'ok')
    this.changed()
  }

  cancelImageEdit(): void {
    this.imageEdit = null
    this.editSrcCanvas = null
    this.render()
  }

  // uploadImages は、設定で使っていて Brain にない画像を送る。送れなければ false（保存しない）。
  private async uploadImages(cfg: Config): Promise<boolean> {
    if (!this.imagesSupported) return true
    const c = this.client!
    this.brainImages = await c.request<ImageListResult>('list_images')
    const need = [...referencedImages(cfg)].filter((id) => !this.onBrain(id))
    let n = 0
    try {
      for (const id of need) {
        const local = this.images.get(id)
        const file = local?.file ?? (local ? encodeImageFile(local.img) : null)
        if (!file) {
          this.say(`背景画像 ${id} のデータがこのタブにも Brain にもありません。背景なしで描かれます（画像を選び直してください）`, 'error')
          continue
        }
        n++
        this.imageBusy = `背景画像を送っています（${n}/${need.length}）：${this.imageName(id)}`
        this.render()
        const sha = await sha256Hex(file)
        const img = local!.img
        const b = await c.request<{ exists: boolean; upload: string; chunk_bytes: number }>('image_begin',
          { sha256: sha, bytes: file.length, w: img.w, h: img.h, name: local!.name, source: 'gui' })
        if (b.exists) continue
        try {
          for (let off = 0; off < file.length; off += b.chunk_bytes) {
            const end = Math.min(off + b.chunk_bytes, file.length)
            await c.request('image_chunk', { upload: b.upload, offset: off, data: toBase64(file.subarray(off, end)) }, 15000)
          }
          await c.request('image_end', { upload: b.upload }, 30000)
        } catch (e) {
          await c.request('image_abort', { upload: b.upload }).catch(() => {})
          throw e
        }
      }
    } catch (e: any) {
      const why = e instanceof ProtocolError && (e.code === 'quota_exceeded' || e.code === 'no_space')
        ? `${e.message}。「Brain の背景画像」で使っていない画像を消すか、画像を減らしてください` : (e?.message ?? String(e))
      this.say(`背景画像を送れなかったので、設定は保存していません：${why}`, 'error')
      return false
    } finally {
      this.imageBusy = null
    }
    return true
  }

  // pruneImages は、編集中の設定と Brain の設定のどちらでも使っていない画像を、Brain から消す。
  async pruneImages(): Promise<void> {
    if (!this.connected || !this.imagesSupported) return
    const keep = [...referencedImages(this.cfg)]
    try {
      const dry = await this.client!.request<{ removed: string[]; freed_bytes: number }>('prune_images', { dry_run: true, keep })
      if (!dry.removed.length) return this.say('使っていない背景画像はありません')
      const names = dry.removed.map((id) => this.imageName(id)).join('、')
      if (!this.deps.confirm(`使っていない背景画像 ${dry.removed.length} 枚（${kb(dry.freed_bytes)}）を Brain から消します。よいですか？\n${names}`)) return
      const r = await this.client!.request<{ removed: string[]; freed_bytes: number }>('prune_images', { keep })
      this.say(`背景画像を ${r.removed.length} 枚消しました（${kb(r.freed_bytes)}）`, 'ok')
    } catch (e: any) {
      this.say(`背景画像を消せませんでした：${e?.message ?? e}`, 'error')
    }
    await this.loadImages()
  }

  // ---------- Todo ----------

  get todoSupported(): boolean {
    return !!this.hello?.commands?.includes('get_todo')
  }

  // loadTodo は Todo の一覧を読み、Brain で変わったら知らせてもらう（subscribe_data）。
  async loadTodo(): Promise<void> {
    if (!this.connected || !this.todoSupported) {
      this.todo = null
      return
    }
    const c = this.client!
    const r = await c.request<TodoResult>('get_todo')
    this.todo = { rev: r.rev, items: r.items ?? [] }
    if (this.hello?.commands?.includes('subscribe_data')) await c.request('subscribe_data', { enable: true })
  }

  // applyTodo は、返事や通知で届いた一覧を使う。届く順は前後することがあるので、古い rev のものは捨てる。
  private applyTodo(l: TodoList): void {
    if (this.todo && l.rev < this.todo.rev) return
    this.todo = { rev: l.rev, items: l.items ?? [] }
    for (const id of Object.keys(this.todoDrafts)) if (!this.todo.items.some((i) => i.id === id)) delete this.todoDrafts[id]
    this.render()
  }

  // todoCall は Todo のコマンドを送る。項目が Brain で変わっていたら（conflict、not_found）、読み直して知らせる。
  async todoCall(cmd: string, params: Record<string, unknown>): Promise<TodoResult | null> {
    if (!this.connected) return null
    this.todoBusy = true
    this.render()
    try {
      const r = await this.client!.request<TodoResult>(cmd, { ...params, source: 'gui' })
      this.applyTodo(r)
      return r
    } catch (e: any) {
      if (e instanceof ProtocolError && (e.code === 'conflict' || e.code === 'not_found')) {
        this.say('Brain で項目が変わっていたので、変更しませんでした。最新の一覧を読み直しました', 'error')
        await this.loadTodo().catch(() => {})
      } else {
        this.say(`Todo を変更できませんでした：${e?.message ?? e}`, 'error')
      }
      return null
    } finally {
      this.todoBusy = false
      this.render()
    }
  }

  async addTodo(text: string, top = false): Promise<void> {
    const t = text.replace(/\s+/g, ' ').trim()
    if (!t) return
    const r = await this.todoCall('todo_add', top ? { text: t, index: 0 } : { text: t })
    if (r) this.todoNew = ''
    this.render()
  }

  async setTodoDone(it: TodoItem, done: boolean): Promise<void> {
    await this.todoCall('todo_update', { item: it.id, rev: it.rev, done })
  }

  async editTodo(it: TodoItem, text: string): Promise<void> {
    const t = text.replace(/\s+/g, ' ').trim()
    delete this.todoDrafts[it.id]
    if (!t || t === it.text) {
      this.render()
      return
    }
    await this.todoCall('todo_update', { item: it.id, rev: it.rev, text: t })
  }

  async deleteTodo(it: TodoItem): Promise<void> {
    delete this.todoDrafts[it.id]
    await this.todoCall('todo_delete', { item: it.id, rev: it.rev })
  }

  // moveTodo は、画面の順で 1 つ上（-1）か下（+1）の項目と入れ替える。未完了と完了の境はまたがない。
  async moveTodo(it: TodoItem, dir: -1 | 1): Promise<void> {
    const items = this.todo?.items ?? []
    const ord = todoOrder(items)
    const i = ord.findIndex((x) => x.id === it.id)
    const other = ord[i + dir]
    if (i < 0 || !other || other.done !== it.done) return
    const rest = items.filter((x) => x.id !== it.id)
    const j = rest.findIndex((x) => x.id === other.id)
    await this.todoCall('todo_move', { item: it.id, rev: it.rev, index: dir < 0 ? j : j + 1 })
  }

  async clearDoneTodo(): Promise<void> {
    const n = this.todo?.items.filter((i) => i.done).length ?? 0
    if (!n || !this.deps.confirm(`完了した項目 ${n} 件を Brain から消します。よいですか？`)) return
    const r = await this.todoCall('todo_clear_done', {})
    if (r) this.say(`完了した項目を ${r.removed ?? n} 件消しました`, 'ok')
  }

  todoInConfig(cfg: Config | null = this.cfg): boolean {
    return !!cfg?.layers.some((l) => Object.values(l.touch?.cells ?? {}).some((a) => a.widget === 'todo'))
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
      case 'mouse':
        a = { mouse: cur?.mouse ?? 'left' }
        break
      case 'usb_mode':
        a = { usb_mode: cur?.usb_mode ?? 'toggle' }
        break
      case 'terminal':
        a = { terminal: cur?.terminal ?? 'toggle' }
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
    if (a && cur?.background && kind !== 'none') a.background = cur.background
    this.setAction(a)
  }

  // setTap は、ウィジェットのセルをタップしたときの動きを変える（ウィジェットの項目と大きさは残す）。
  setTap(kind: ActionKind): void {
    const cur = this.ownAction()
    if (!cur?.widget) return
    const a: ActionSpec = {}
    for (const f of [...WIDGET_FIELDS, 'label', 'span', 'background'] as const) if (cur[f] !== undefined) (a as any)[f] = cur[f]
    if (kind === 'key') a.key = cur.key && cur.key.toLowerCase() !== 'none' ? cur.key : ''
    else if (kind === 'mouse') a.mouse = cur.mouse ?? 'left'
    else if (kind === 'usb_mode') a.usb_mode = cur.usb_mode ?? 'toggle'
    else if (kind === 'terminal') a.terminal = cur.terminal ?? 'toggle'
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
    for (const f of ['format', 'date_format', 'tz', 'id', 'page_reset', 'stale'] as const) if (a[f] === '') delete a[f]
    for (const f of PAD_NUM_FIELDS) if (a[f] === undefined || !Number.isFinite(a[f])) delete a[f]
    if (!a.scroll_direction) delete a.scroll_direction
    if (!a.long_press) delete a.long_press
    if (!(Number.isInteger(a.rows) && a.rows! > 0)) delete a.rows
    if (a.calendars && !a.calendars.length) delete a.calendars
    this.setAction(a)
  }

  // setWidgetKind は、ウィジェットの種類を変える。ほかの種類の項目は消し、見出し、大きさ、タップの動きは残す。
  setWidgetKind(w: WidgetKind): void {
    const cur = this.ownAction()
    if (!cur?.widget || cur.widget === w) return
    const a: ActionSpec = { ...cur, widget: w }
    for (const f of CLOCK_FIELDS) if (w !== 'clock') delete a[f]
    if (w !== 'text') delete a.id
    else a.id = cur.id || this.suggestTextId()
    const paged = w === 'todo' || w === 'calendar'
    if (!paged) {
      delete a.rows
      delete a.page_reset
    }
    if (w !== 'calendar') {
      delete a.stale
      delete a.calendars
    }
    if (w !== 'trackpad') for (const f of PAD_FIELDS) delete a[f]
    // Todo、カレンダー、トラックパッドは押した位置で働く（長押しで完了、▲▼ でページ送り、タップでクリック）ので、
    // タップしたときのキーやレイヤーは書けない
    if (ownsTouch(w)) for (const k of ACTION_FIELDS) delete a[k]
    this.setAction(a)
  }

  // suggestTextId は、まだ使っていないテキストの id を作る。
  suggestTextId(): string {
    const used = new Set(this.textIdsInConfig())
    for (const id of Object.keys(this.texts).sort()) if (!used.has(id)) return id
    for (let i = 1; ; i++) if (!used.has(`text${i}`)) return `text${i}`
  }

  textIdsInConfig(): string[] {
    const ids = new Set<string>()
    for (const l of this.cfg?.layers ?? []) for (const a of Object.values(l.touch?.cells ?? {})) if (a.widget === 'text' && a.id) ids.add(a.id)
    return [...ids].sort()
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
    if (n.event === 'todo') {
      this.applyTodo(n)
      return
    }
    if (n.event === 'terminal') {
      const { event: _, ...t } = n
      this.brainTerm = t
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
    const changes = diffConfigs(clean(this.saved), this.forBrain(), this.keymap, (id) => this.imageName(id))
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
      const cfg = this.forBrain()
      // 背景画像は、設定より先に送る（設定を保存した瞬間に、画像が Brain にあるように）
      if (!(await this.uploadImages(cfg))) {
        this.diff = null
        return
      }
      const r = await this.client!.request<{ saved: string; previous: string; warnings: string[] }>(
        'set_config',
        { config: cfg },
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
      void this.loadImages()
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
    replaceKeepingFocus(this.main, this.view())
    const showConsole = this.section === 'console'
    this.console.host.hidden = !showConsole
    if (showConsole) void this.console.shown()
    this.drawPreview()
    this.drawImageEdit()
  }

  private view(): HTMLElement {
    return h(
      'div',
      { class: 'app' },
      this.viewHeader(),
      this.viewMessages(),
      this.section === 'console' ? this.viewConsole() : this.cfg || this.section === 'todo' ? this.viewEditor() : this.viewStart(),
      this.diff ? this.viewDiff() : null,
      this.imageEdit ? this.viewImageEdit() : null,
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
    return h('div', { class: 'messages' },
      this.imageBusy ? h('div', { class: 'msg info', id: 'image-busy' }, this.imageBusy) : null,
      this.messages.map((m) => h('div', { class: `msg ${m.level}` }, m.text)))
  }

  private viewStart(): HTMLElement {
    return h(
      'main',
      { class: 'start' },
      this.viewSections(),
      h('h2', null, 'はじめに'),
      h('ol', null,
        h('li', null, 'Brain と PC を USB ケーブルでつなぎ、「Brain に接続」を押します。'),
        h('li', null, 'ポートを選ぶ画面で、Brain（USB 1d6b:0104）の設定用のポートを選びます。Brain には 2 つのシリアルがあり、設定用は 2 つ目です（Linux では ttyACM1）。1 つ目はコンソール（ログイン画面）用で、ブラウザからは見分けられないので、名前で選んでください。選んだポートは、ページを開いているあいだ覚えています。'),
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
      this.viewSections(),
      this.section === 'todo'
        ? this.viewTodo()
        : [
            h('div', { class: 'source' }, `編集中：${this.source}`, this.dirty ? h('span', { class: 'dirty' }, '（未保存の変更あり）') : null),
            this.viewTabs(),
            this.viewLayerProps(),
            h('div', { class: 'columns' },
              h('div', { class: 'left' }, this.viewKeyboard(), this.viewTouch()),
              h('div', { class: 'right' }, this.viewInspector(), this.viewProblems(), this.viewDeviceInfo()),
            ),
          ],
    )
  }

  showSection(s: 'config' | 'todo' | 'console'): void {
    this.section = s
    this.render()
    if (s === 'console') this.console.focus()
  }

  // viewSections は、設定（キーとタッチ）と Todo の切り替え。
  private viewSections(): HTMLElement {
    const open = this.todo?.items.filter((i) => !i.done).length
    return h('nav', { class: 'sections', role: 'tablist' },
      h('button', { role: 'tab', id: 'section-config', class: ['section', this.section === 'config' && 'active'],
        'aria-selected': this.section === 'config' ? 'true' : 'false', onclick: () => this.showSection('config') }, 'キーとタッチ'),
      h('button', { role: 'tab', id: 'section-todo', class: ['section', this.section === 'todo' && 'active'],
        'aria-selected': this.section === 'todo' ? 'true' : 'false', onclick: () => this.showSection('todo') },
        'Todo', open ? h('span', { class: 'count' }, String(open)) : null),
      h('button', { role: 'tab', id: 'section-console', class: ['section', this.section === 'console' && 'active'],
        'aria-selected': this.section === 'console' ? 'true' : 'false', onclick: () => this.showSection('console') },
        'コンソール', this.console.open ? h('span', { class: 'count ok' }, '接続中') : null))
  }

  // viewConsole は「コンソール」タブの上の帯。端末そのもの（console.host）は、作り直さない別の要素に置く。
  private viewConsole(): HTMLElement {
    const c = this.console
    const size = c.size
    const synced = !!size && !!c.sentSize && size.rows === c.sentSize.rows && size.cols === c.sentSize.cols
    return h(
      'main',
      { class: 'console-main' },
      this.viewSections(),
      h('section', { class: 'panel console-bar' },
        h('div', { class: 'panel-head' },
          h('h2', null, 'Brain のコンソール'),
          h('span', { class: ['conn', c.open ? 'ok' : 'off'], id: 'console-state' },
            c.open ? '● 接続中' : c.state === 'connecting' ? '… 接続中' : '○ 未接続'),
          c.open
            ? h('button', { id: 'console-disconnect', onclick: () => void c.disconnect() }, '切断')
            : h('button', { class: 'primary', id: 'console-connect', disabled: c.state !== 'idle' || !this.deps.serial || !!this.brainTerm?.active,
              title: this.brainTerm?.active ? 'Brain が端末モードのあいだ、Brain のログイン画面は止めてあります' : undefined,
              onclick: () => void c.connect() }, '接続'),
          h('button', { id: 'console-size', disabled: !c.open || !size, onclick: () => void c.sendSize(),
            title: 'シリアルでは端末の大きさが伝わらないので、シェルのプロンプトが出ているときに押して伝えます' },
            size ? `大きさを合わせる（${sttyCommand(size)} を送る）` : '大きさを合わせる'),
          size ? h('span', { class: 'hint', id: 'console-size-state' },
            `端末 ${size.cols}×${size.rows}　`, c.open ? (synced ? 'Brain に伝えてあります' : 'Brain にはまだ伝えていません') : '') : null,
        ),
        c.status ? h('div', { class: `msg ${c.status.level}`, id: 'console-status' }, c.status.text) : null,
        this.viewTermPanel(),
        h('p', { class: 'hint' },
          'Brain の 1 つ目のシリアル（/dev/ttyGS0）のログイン画面です。ユーザー名とパスワードでログインします。',
          '「大きさを合わせる」は、シェルのプロンプトが出ているときだけ押してください（ログイン画面やエディタの中では、そのまま入力されます）。',
          'Ctrl+W、Ctrl+T、Ctrl+N などはブラウザが先に使うので、Brain には届きません。'),
      ),
    )
  }

  // viewTodo は「Todo」タブ。項目は Brain のデータで、ここで変えるとすぐ Brain に書く（設定の保存とは別）。
  private viewTodo(): HTMLElement {
    const panel = (...c: (HTMLElement | null)[]) => h('section', { class: 'panel todo-panel' }, h('h2', null, 'Todo（Brain に保存）'), c)
    if (!this.connected) return panel(h('p', { class: 'hint' }, 'Brain に接続すると、Todo を編集できます。項目は Brain に保存され、設定ファイルとは別です。'))
    if (!this.todoSupported) return panel(h('div', { class: 'warn' }, 'Brain の lefthand が Todo に対応していません。lefthand を新しくしてください'))
    const items = todoOrder(this.todo?.items ?? [])
    const open = items.filter((i) => !i.done).length
    const busy = this.todoBusy
    const addInput = h('input', { id: 'todo-new', 'data-focus': 'todo-new', value: this.todoNew, maxlength: TODO_MAX_RUNES, placeholder: '新しい項目（Enter で追加）',
      oninput: (e: Event) => (this.todoNew = (e.target as HTMLInputElement).value),
      onkeydown: (e: KeyboardEvent) => {
        if (e.key === 'Enter' && !e.isComposing) void this.addTodo(this.todoNew, e.shiftKey)
      } })
    const row = (it: TodoItem, i: number) => {
      const prev = items[i - 1]
      const next = items[i + 1]
      return h('li', { class: ['todo-item', it.done && 'done'], dataset: { id: it.id } },
        h('input', { type: 'checkbox', checked: it.done, disabled: busy, title: it.done ? '未完了に戻す' : '完了にする',
          onchange: (e: Event) => void this.setTodoDone(it, (e.target as HTMLInputElement).checked) }),
        h('input', { class: 'todo-text', value: this.todoDrafts[it.id] ?? it.text, maxlength: TODO_MAX_RUNES, 'data-focus': `todo-${it.id}`,
          oninput: (e: Event) => (this.todoDrafts[it.id] = (e.target as HTMLInputElement).value),
          onchange: (e: Event) => void this.editTodo(it, (e.target as HTMLInputElement).value),
          onkeydown: (e: KeyboardEvent) => {
            if (e.key === 'Enter' && !e.isComposing) (e.target as HTMLInputElement).blur()
          } }),
        h('button', { class: 'small up', title: '上へ', disabled: busy || !prev || prev.done !== it.done, onclick: () => void this.moveTodo(it, -1) }, '↑'),
        h('button', { class: 'small down', title: '下へ', disabled: busy || !next || next.done !== it.done, onclick: () => void this.moveTodo(it, 1) }, '↓'),
        h('button', { class: 'small danger del', title: '消す', disabled: busy, onclick: () => void this.deleteTodo(it) }, '削除'),
        it.source === 'brain' ? h('span', { class: 'hint', title: 'Brain で切り替えた項目' }, 'Brain') : null)
    }
    return panel(
      h('div', { class: 'todo-add' }, addInput,
        h('button', { class: 'primary', id: 'todo-add', disabled: busy, onclick: () => void this.addTodo(this.todoNew) }, '追加'),
        h('button', { id: 'todo-add-top', disabled: busy, onclick: () => void this.addTodo(this.todoNew, true),
          title: 'Shift+Enter でも先頭に足せます' }, '先頭に追加')),
      items.length ? h('ul', { class: 'todo-list' }, items.map(row)) : h('p', { class: 'hint' }, 'まだ項目がありません'),
      h('div', { class: 'todo-foot' },
        h('span', null, `未完了 ${open} 件、完了 ${items.length - open} 件`),
        h('button', { id: 'todo-clear-done', disabled: busy || open === items.length, onclick: () => void this.clearDoneTodo() }, '完了した項目を消す')),
      this.todoInConfig()
        ? null
        : h('p', { class: 'warn' }, '今の設定には Todo のセルがないので、Brain の画面には出ません。「キーとタッチ」で、セルの種類をウィジェットにし、ウィジェットを Todo にしてください。'),
      h('p', { class: 'hint' },
        'ここでの変更は、すぐ Brain に書きます（「Brain に保存」は要りません）。完了した項目は消さずに薄く表示し、未完了の下に並べます。' +
        'Brain では、項目を 0.5 秒押し続けると完了を切り替えます（押しているあいだ黄色になります）。入りきらないときは、セルの下の ▲ ▼ でページを送ります。' +
        'Brain で切り替えると、ここにもすぐ反映します。PC のターミナルからは brain-deck todo add "…" などで書き換えられます（この画面が接続しているあいだは使えません）。'),
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
      this.cfg!.touch ? this.viewBgEditor({ kind: 'wallpaper', layer: this.layer }) : null,
      errs.map((p) => h('div', { class: 'err' }, p.message)),
    )
  }

  // ---------- 背景画像の欄 ----------

  // viewBgEditor は、セルの背景画像かレイヤーの壁紙を選ぶ欄。画像ファイルはドロップしてもよい。
  private viewBgEditor(t: BgTarget): HTMLElement {
    const id = this.bgOf(t)
    const { w, h: hh } = this.bgTargetSize(t)
    const what = t.kind === 'wallpaper' ? '壁紙' : '背景画像'
    const input = h('input', { type: 'file', accept: 'image/png,image/jpeg,image/webp,image/*', class: 'hidden', id: `bg-file-${t.kind}`,
      onchange: (e: Event) => {
        const el = e.target as HTMLInputElement
        const f = el.files?.[0]
        el.value = ''
        if (f) void this.openBgFile(t, f)
      } })
    let state = ''
    if (id) {
      if (this.connected && this.imagesSupported && this.brainImages) state = this.onBrain(id) ? 'Brain にあります' : this.images.get(id) ? '保存すると Brain に送ります' : 'Brain にありません（背景なしで描かれます）'
      const img = this.images.get(id)?.img
      if (img && (img.w !== w || img.h !== hh)) state += `。大きさが違います（画像 ${img.w}×${img.h}、${what}の範囲 ${w}×${hh}）。中央に置き、はみ出す分は切れます`
    }
    return h('div', { class: ['bg-editor', `bg-${t.kind}`],
      ondragover: (e: DragEvent) => e.preventDefault(),
      ondrop: (e: DragEvent) => {
        e.preventDefault()
        const f = e.dataTransfer?.files?.[0]
        if (f) void this.openBgFile(t, f)
      } },
      h('span', { class: 'bg-label' }, `${what} `),
      id ? h('span', { class: 'bg-name', title: id }, this.imageName(id)) : h('span', { class: 'hint' }, 'なし'),
      input,
      h('button', { class: 'small', id: `bg-choose-${t.kind}`, onclick: () => input.click(),
        title: 'PNG、JPEG、WebP。ファイルをこの欄にドロップしてもよい' }, '画像を選ぶ…'),
      id && this.images.get(id)?.source ? h('button', { class: 'small', id: `bg-recrop-${t.kind}`, onclick: () => this.reEditBg(t) }, '切り抜きを直す') : null,
      id ? h('button', { class: 'small danger', id: `bg-remove-${t.kind}`, onclick: () => { this.setBg(t, undefined); this.changed() } }, '外す') : null,
      this.connected && !this.imagesSupported ? h('div', { class: 'warn' }, 'この Brain の lefthand は背景画像に対応していません。lefthand を新しくしてください') : null,
      state ? h('div', { class: 'hint' }, state) : null,
      h('div', { class: 'hint' }, t.kind === 'wallpaper'
        ? `格子全体（${w}×${hh}）に敷きます。セルの背景画像があれば、そちらを上に描きます。`
        : `セルの枠の内側（${w}×${hh}）に合わせて切り抜きます。`),
    )
  }

  // viewImageEdit は、背景画像を切り抜く画面。元の画像をドラッグで動かし、ホイールか「拡大」で範囲を決める。
  private viewImageEdit(): HTMLElement {
    const e = this.imageEdit!
    const p = e.params
    const pct = Math.round(Math.abs(p.brightness) * 100)
    const brightText = p.brightness < 0 ? `暗く ${pct}%` : p.brightness > 0 ? `明るく ${pct}%` : 'そのまま'
    const size = e.result ? kb(e.result.file.length) : ''
    return h('div', { class: 'modal-back' },
      h('div', { class: 'modal image-edit', role: 'dialog', 'aria-label': '背景画像を切り抜く' },
        h('h2', null, `${e.target.kind === 'wallpaper' ? '壁紙' : 'セルの背景画像'}を切り抜く（${e.name}）`),
        h('div', { class: 'crop-row' },
          h('div', { class: 'crop-box' },
            h('canvas', { class: 'crop-src', width: 480, height: 320,
              onpointerdown: (ev: PointerEvent) => this.cropPointer(ev, 'down'),
              onpointermove: (ev: PointerEvent) => this.cropPointer(ev, 'move'),
              onpointerup: (ev: PointerEvent) => this.cropPointer(ev, 'up'),
              onpointercancel: (ev: PointerEvent) => this.cropPointer(ev, 'up'),
              onwheel: (ev: WheelEvent) => {
                ev.preventDefault()
                this.updateImageEdit({ zoom: p.zoom * Math.exp(-ev.deltaY * 0.0015) })
                this.syncEditControls()
              } }),
            h('p', { class: 'hint' }, `明るい枠の中が使われます。ドラッグで動かし、ホイールか「拡大」で大きさを変えます。縦横比は${e.target.kind === 'wallpaper' ? '画面' : 'セル'}（${e.tw}×${e.th}）に合わせます。`)),
          h('div', { class: 'crop-controls' },
            h('label', { class: 'row' }, '拡大 ', h('input', { type: 'range', id: 'img-zoom', min: 1, max: MAX_ZOOM, step: 0.01, value: p.zoom,
              oninput: (ev: Event) => this.updateImageEdit({ zoom: Number((ev.target as HTMLInputElement).value) }) })),
            h('label', { class: 'row', title: '文字が読みやすいよう、画像を暗く（明るく）します。画像そのものに焼き込みます' }, '明るさ ',
              h('input', { type: 'range', id: 'img-bright', min: -100, max: 100, step: 5, value: Math.round(p.brightness * 100),
                oninput: (ev: Event) => {
                  this.updateImageEdit({ brightness: Number((ev.target as HTMLInputElement).value) / 100 })
                  this.syncEditControls()
                } }),
              h('span', { class: 'bright-text' }, brightText)),
            h('label', { class: 'row', title: 'Brain の画面は 65536 色（RGB565）。ディザリングで、グラデーションの段を目立たなくします' }, 'ディザリング ',
              h('select', { id: 'img-dither', onchange: (ev: Event) => { this.updateImageEdit({ dither: (ev.target as HTMLSelectElement).value as Dither }); this.render() } },
                (Object.keys(DITHER_LABELS) as Dither[]).map((d) => h('option', { value: d, selected: p.dither === d }, DITHER_LABELS[d])))),
            e.target.kind === 'cell'
              ? h('label', { class: 'row' }, h('input', { type: 'checkbox', id: 'img-pressed', checked: e.showPressed,
                onchange: (ev: Event) => { e.showPressed = (ev.target as HTMLInputElement).checked; this.drawImageEdit() } }), ' 押したときの枠を重ねる')
              : null,
            h('p', { class: 'hint' }, `Brain に送る大きさ：${e.tw}×${e.th}、${size}`))),
        h('canvas', { class: 'crop-preview', width: this.keymap.screen.w, height: this.keymap.screen.h }),
        h('p', { class: 'hint' }, '下は Brain の画面のプレビュー（ラベル、文字の縁取り、押したときの枠も Brain と同じに描きます）。'),
        h('div', { class: 'buttons' },
          h('button', { id: 'img-cancel', onclick: () => this.cancelImageEdit() }, 'やめる'),
          h('button', { class: 'primary', id: 'img-apply', disabled: !e.result, onclick: () => void this.applyImageEdit() }, 'この範囲にする'))))
  }

  // syncEditControls は、DOM を作り直さずに、切り抜きの画面の欄を今の値にする（ドラッグ中など）。
  private syncEditControls(): void {
    const e = this.imageEdit
    if (!e) return
    const z = this.root.querySelector<HTMLInputElement>('#img-zoom')
    if (z) z.value = String(e.params.zoom)
    const t = this.root.querySelector('.bright-text')
    const pct = Math.round(Math.abs(e.params.brightness) * 100)
    if (t) t.textContent = e.params.brightness < 0 ? `暗く ${pct}%` : e.params.brightness > 0 ? `明るく ${pct}%` : 'そのまま'
  }

  // cropPointer は、元の画像の上のドラッグで、切り抜く範囲を動かす。
  private cropPointer(ev: PointerEvent, phase: 'down' | 'move' | 'up'): void {
    const e = this.imageEdit
    const c = ev.currentTarget as HTMLCanvasElement
    if (!e) return
    if (phase === 'down') {
      const rect = c.getBoundingClientRect()
      const fit = Math.min(c.width / e.src.w, c.height / e.src.h)
      // 画面上の 1 ピクセルが、元の画像の何ピクセルか
      const scale = (c.width / Math.max(rect.width, 1)) / fit
      this.editDrag = { x: ev.clientX, y: ev.clientY, cx: e.params.cx, cy: e.params.cy, scale }
      c.setPointerCapture?.(ev.pointerId)
    } else if (phase === 'move' && this.editDrag) {
      const d = this.editDrag
      this.updateImageEdit({ cx: d.cx + (ev.clientX - d.x) * d.scale, cy: d.cy + (ev.clientY - d.y) * d.scale })
    } else if (phase === 'up') this.editDrag = null
  }

  // drawImageEdit は、切り抜きの画面の 2 つの絵（元の画像と範囲、Brain の画面のプレビュー）を描く。
  private drawImageEdit(): void {
    const e = this.imageEdit
    if (!e) return
    cancelAnimationFrame?.(this.editFrame)
    const draw = () => {
      const src = this.root.querySelector<HTMLCanvasElement>('canvas.crop-src')
      const ctx = src?.getContext('2d')
      if (src && ctx) {
        if (!this.editSrcCanvas) {
          const c = document.createElement('canvas')
          c.width = e.src.w
          c.height = e.src.h
          c.getContext('2d')?.putImageData(new ImageData(e.src.data as any, e.src.w, e.src.h), 0, 0)
          this.editSrcCanvas = c
        }
        const fit = Math.min(src.width / e.src.w, src.height / e.src.h)
        const dw = e.src.w * fit
        const dh = e.src.h * fit
        const ox = (src.width - dw) / 2
        const oy = (src.height - dh) / 2
        ctx.fillStyle = '#222'
        ctx.fillRect(0, 0, src.width, src.height)
        ctx.drawImage(this.editSrcCanvas, ox, oy, dw, dh)
        const cr = e.result?.crop ?? fitCrop(e.src.w, e.src.h, e.tw, e.th, e.params.zoom, e.params.cx, e.params.cy)
        const x = ox + cr.x * fit
        const y = oy + cr.y * fit
        const w = cr.w * fit
        const hh = cr.h * fit
        ctx.fillStyle = 'rgba(0,0,0,0.6)'
        ctx.fillRect(ox, oy, dw, y - oy)
        ctx.fillRect(ox, y + hh, dw, oy + dh - y - hh)
        ctx.fillRect(ox, y, x - ox, hh)
        ctx.fillRect(x + w, y, ox + dw - x - w, hh)
        ctx.strokeStyle = '#ffd040'
        ctx.lineWidth = 2
        ctx.strokeRect(x, y, w, hh)
      }
      const pv = this.root.querySelector<HTMLCanvasElement>('canvas.crop-preview')
      const pctx = pv?.getContext('2d')
      if (pv && pctx && this.font && this.cfg && e.result) {
        const cfg = structuredClone(this.cfg)
        const t = e.target
        const l = cfg.layers[t.layer]
        if (t.kind === 'wallpaper') (l.touch ??= {}).background = '__edit__'
        else if (l.touch?.cells?.[cellKey(t.col, t.row)]) l.touch.cells[cellKey(t.col, t.row)].background = '__edit__'
        const img = e.result.img
        const pressed = new Set(t.kind === 'cell' && e.showPressed ? [cellKey(t.col, t.row)] : [])
        const { pixels } = renderPreview(this.font, { cfg, stack: editStack(t.layer), mode: this.previewMode(t.layer), pressed,
          w: pv.width, h: pv.height, now: new Date(), texts: this.texts, todo: this.todo ?? undefined, calendar: this.calendar,
          images: (id) => (id === '__edit__' ? img : this.images.get(id)?.img) })
        pctx.putImageData(new ImageData(pixels as any, pv.width, pv.height), 0, 0)
      }
    }
    if (typeof requestAnimationFrame === 'function') this.editFrame = requestAnimationFrame(draw)
    else draw()
  }

  // viewImagesPanel は、Brain にある背景画像の一覧と、使っていない画像を消すボタン。
  private viewImagesPanel(): HTMLElement | null {
    if (!this.connected || !this.imagesSupported || !this.brainImages) return null
    const l = this.brainImages
    const editing = referencedImages(this.cfg)
    return h('details', { class: 'images-panel' },
      h('summary', null, `Brain の背景画像（${l.images.length} 枚、${kb(l.total_bytes)} / 上限 ${kb(l.limit_bytes)}）`),
      l.images.length
        ? h('table', { class: 'images' },
          h('thead', null, h('tr', null, ['名前', '大きさ', '容量', '使っている場所', 'id'].map((x) => h('th', null, x)))),
          h('tbody', null, l.images.map((im) => h('tr', { dataset: { image: im.id } },
            h('td', null, im.name || '（名前なし）'),
            h('td', null, `${im.w}×${im.h}`),
            h('td', null, kb(im.bytes)),
            h('td', null, im.refs.length ? `${im.refs.length} か所` : editing.has(im.id) ? '編集中の設定で使用' : '使っていない'),
            h('td', { class: 'mono' }, im.id)))))
        : h('p', { class: 'hint' }, 'まだありません。セルかレイヤーを選んで「画像を選ぶ…」から足します。'),
      l.missing.length ? h('p', { class: 'warn' }, `Brain の設定で使っているのに、Brain にない画像：${l.missing.join(' ')}（背景なしで描いています）`) : null,
      h('p', { class: 'hint' }, `1 枚は画面いっぱい（${l.max_side}×${Math.floor(l.max_pixels / l.max_side)}、${kb(l.max_image_bytes)}）まで。SD カードの空き ${kb(l.free_bytes)}（保存したあとも ${kb(l.reserve_bytes)} は残します）。`),
      h('button', { class: 'small danger', id: 'prune-images', onclick: () => void this.pruneImages() }, '使っていない画像を Brain から消す'))
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
          ondragover: (e: DragEvent) => e.preventDefault(),
          ondrop: (e: DragEvent) => {
            // 画像ファイルをセルにドロップすると、そのセルの背景画像にする
            e.preventDefault()
            const f = e.dataTransfer?.files?.[0]
            this.sel = sel
            if (f && this.ownAction(sel)) void this.openBgFile({ kind: 'cell', layer: this.layer, col: c, row: r }, f)
            else if (f) this.say('このレイヤーに割り当てのあるセルにだけ、背景画像を置けます。先に割り当ててください', 'error')
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
      this.viewImagesPanel(),
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
    const mode = this.previewMode(li)
    const pressed = new Set(this.previewPress ? [this.previewPress] : [])
    const now = new Date()
    const { pixels } = renderPreview(this.font, { cfg: this.cfg, stack: editStack(li), mode, pressed, w: canvas.width, h: canvas.height, now,
      texts: this.texts, todo: this.todo ?? undefined, calendar: this.calendar, images: (id) => this.images.get(id)?.img,
      mouseOff: this.brainMouse === false })
    ctx.putImageData(new ImageData(pixels as any, canvas.width, canvas.height), 0, 0)
    this.schedulePreviewTick(now)
  }

  // previewMode は、レイヤーの入り方で枠の色を決める（切り替えたままなら緑、一時的なら橙）。
  private previewMode(li: number): Mode {
    const refs = li === 0 ? [] : references(this.cfg!, this.cfg!.layers[li].name)
    return li === 0 ? 'base' : refs.some((r) => r.kind === 'layer_toggle' || r.kind === 'layer_to') || !refs.length ? 'latched' : 'temp'
  }

  // schedulePreviewTick は、時計が出ていれば、表示が変わる時刻（次の秒か分）にプレビューを描き直す。
  private schedulePreviewTick(now: Date): void {
    if (this.previewTimer) clearTimeout(this.previewTimer)
    this.previewTimer = null
    const g = resolveGrid(this.cfg!, editStack(this.layer))
    const clocks = g.cells.filter((c) => c?.action.widget === 'clock').map((c) => c!.action)
    let wait = Infinity
    if (clocks.length) {
      const sec = clocks.some((a) => hasSeconds(a.format || DEFAULT_CLOCK_FORMAT) || hasSeconds(a.date_format && a.date_format !== 'none' ? a.date_format : ''))
      const unit = sec ? 1000 : 60000
      wait = unit - (now.getTime() % unit) + 5
    }
    // カレンダーは、予定の始まりと終わりで描き直す（1 分ごとに見直せば十分）
    if (g.cells.some((c) => c?.action.widget === 'calendar')) wait = Math.min(wait, 60000 - (now.getTime() % 60000) + 5)
    // テキストの有効期限が切れたら、薄く描き直す
    for (const c of g.cells) {
      const e = c?.action.widget === 'text' && c.action.id ? this.texts[c.action.id] : undefined
      if (e?.expires_at && !textExpired(e, now)) wait = Math.min(wait, Date.parse(e.expires_at) - now.getTime() + 5)
    }
    if (wait === Infinity) return
    this.previewTimer = setTimeout(() => this.drawPreview(), Math.min(wait, 2 ** 31 - 1))
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
    const tap: ActionKind | null = own?.widget !== undefined && !ownsTouch(own.widget) ? actionKind(own) : null
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
        ([...ACTION_FIELDS] as ActionKind[]).map((k) => h('option', { value: k, selected: tap === k }, KIND_LABELS[k])))))
    }
    const act = tap ?? kind
    if (own && act === 'key') body.push(this.viewComboEditor(own))
    if (own && act === 'mouse') body.push(...this.viewMouseEditor(own))
    if (own && act === 'usb_mode') body.push(...this.viewUsbModeEditor(own))
    if (own && act === 'terminal') body.push(...this.viewTermEditor(own))
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
    if (own && kind !== 'inherit' && kind !== 'none' && sel.kind === 'cell') {
      body.push(this.viewSpanEditor(own))
      body.push(this.viewBgEditor({ kind: 'cell', layer: this.layer, col: sel.col, row: sel.row }))
    }
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
        onchange: (e: Event) => this.setWidgetKind((e.target as HTMLSelectElement).value as WidgetKind) },
        (Object.keys(WIDGET_LABELS) as WidgetKind[]).map((w) => h('option', { value: w, selected: own.widget === w }, WIDGET_LABELS[w])))),
      own.widget === 'text' ? this.viewTextEditor(own) : own.widget === 'todo' ? this.viewTodoEditor(own)
        : own.widget === 'calendar' ? this.viewCalendarEditor(own) : own.widget === 'trackpad' ? this.viewPadEditor(own)
        : this.viewClockEditor(text, missing),
    )
  }

  // viewTextEditor は、テキストのタイルの欄。中身は設定ではなく Brain のデータで、PC から brain-deck で書き換える。
  private viewTextEditor(own: ActionSpec): HTMLElement[] {
    const id = own.id ?? ''
    const known = [...new Set([...Object.keys(this.texts), ...this.textIdsInConfig()])].sort()
    const e = id ? this.texts[id] : undefined
    const out: HTMLElement[] = [
      h('label', { class: 'row', title: '英数字と _ . -（32 文字まで）。brain-deck text <id> で中身を書き換えます' }, 'id ',
        h('input', { value: id, 'data-focus': 'id', id: 'text-id', list: 'text-ids', placeholder: '例：build',
          onchange: (ev: Event) => this.patchAction({ id: (ev.target as HTMLInputElement).value.trim() }) }),
        h('datalist', { id: 'text-ids' }, known.map((k) => h('option', { value: k })))),
    ]
    if (id && !TEXT_ID_PATTERN.test(id)) out.push(h('div', { class: 'err' }, 'id は、英数字と _ . - の 32 文字までです'))
    if (this.connected && this.hello?.commands?.includes('get_text')) {
      out.push(h('p', { class: 'hint text-now' }, e
        ? `今の中身：「${e.text.replace(/\n/g, '⏎')}」（${TEXT_STYLES[e.style] ?? e.style}${e.expires_at ? `、期限 ${new Date(e.expires_at).toLocaleString()}${textExpired(e, new Date()) ? '（切れています。薄く表示）' : ''}` : ''}）`
        : '今の中身：まだありません（Brain では「未設定」と出ます）'))
    } else if (this.connected) {
      out.push(h('div', { class: 'warn' }, 'Brain の lefthand がテキストに対応していません。lefthand を新しくしてください'))
    }
    out.push(h('p', { class: 'hint' },
      `中身は設定ファイルには入らず、Brain に別に保存します（設定を保存しても消えません）。PC で「brain-deck text ${id || '<id>'} "ビルド成功" --style ok --ttl 10m」のように書き換えます。` +
      '色は通常・成功（ok）・失敗（error）・警告（warn）。有効期限を過ぎると、消さずに薄く表示します。設定 GUI が接続しているあいだは、brain-deck から書き換えられません。'))
    return out
  }

  // viewTodoEditor は、Todo のセルの欄。項目は設定ではなく Brain のデータで、「Todo」タブか brain-deck todo で書き換える。
  private viewTodoEditor(own: ActionSpec): HTMLElement[] {
    const n = this.todo ? `今は ${this.todo.items.filter((i) => !i.done).length} 件が未完了です。` : ''
    return [
      ...this.viewPagerFields(own, '項目', 'todo'),
      h('p', { class: 'hint' },
        `項目は設定ファイルには入らず、Brain に別に保存します（設定を保存しても消えません）。上の「Todo」タブか、PC で「brain-deck todo add "牛乳を買う"」のように書き換えます。${n}` +
        'Brain では、項目を 0.5 秒押し続けると完了を切り替えます。入りきらないときは、セルの下の ▲ ▼ でページを送ります。見出しには残りの件数を出します。'),
      h('button', { class: 'small', onclick: () => this.showSection('todo') }, 'Todo タブを開く'),
    ]
  }

  // viewPagerFields は、ページを送るウィジェット（Todo、カレンダー）の、1 ページの行数と、最初のページに戻るまでの時間の欄。
  private viewPagerFields(own: ActionSpec, what: string, idp: string): HTMLElement[] {
    const pr = own.page_reset ?? ''
    const prBad = pr !== '' && pr !== 'off' && !(parseDuration(pr) !== null && parseDuration(pr)! >= 5000 && parseDuration(pr)! <= 86400_000)
    return [
      h('label', { class: 'row', title: `1 ページに並べる${what}の数。空なら、セルの高さに入るだけ並べます（1 行 52 ドット）` }, '1 ページの行数 ',
        h('input', { type: 'number', min: 1, max: TODO_MAX_ROWS, value: own.rows ?? '', placeholder: '自動', 'data-focus': 'rows', id: `${idp}-rows`, class: 'num',
          onchange: (e: Event) => {
            const v = (e.target as HTMLInputElement).value
            this.patchAction({ rows: v === '' ? undefined : Math.min(Math.max(Math.trunc(Number(v)) || 1, 1), TODO_MAX_ROWS) })
          } })),
      h('label', { class: 'row', title: 'ページを送ってから、この時間だれも触らなければ最初のページに戻ります（30s、2m など。5 秒〜24 時間）。off で戻りません' },
        '最初のページに戻るまで ',
        h('input', { value: pr, placeholder: DEFAULT_PAGE_RESET, 'data-focus': 'page_reset', id: `${idp}-page-reset`, class: 'num',
          onchange: (e: Event) => this.patchAction({ page_reset: (e.target as HTMLInputElement).value.trim() }) })),
      ...(prBad ? [h('div', { class: 'err' }, '「最初のページに戻るまで」は、30s、2m のような 5 秒から 24 時間の時間か、off です')] : []),
    ]
  }

  // viewCalendarEditor は、カレンダーのセルの欄。予定は設定ではなく Brain のデータで、PC の brain-deck calendar sync が送る。
  private viewCalendarEditor(own: ActionSpec): HTMLElement[] {
    const names = this.calendar?.calendars.map((c) => c.name) ?? []
    const stale = own.stale ?? ''
    const staleBad = stale !== '' && !(parseDuration(stale) !== null && parseDuration(stale)! >= 60_000 && parseDuration(stale)! <= 720 * 3600_000)
    const out: HTMLElement[] = [
      ...this.viewPagerFields(own, '予定', 'cal'),
      h('label', { class: 'row', title: '最終更新がこれより古いと、セルの下の「更新」を橙色にして「古い」と出します（1m〜720h。既定 3h）' }, '古いとみなすまで ',
        h('input', { value: stale, placeholder: '3h', 'data-focus': 'stale', id: 'cal-stale', class: 'num',
          onchange: (e: Event) => this.patchAction({ stale: (e.target as HTMLInputElement).value.trim() }) })),
      h('label', { class: 'row', title: '出すカレンダーの名前（brain-deck の calendars.yaml の name）。読点かカンマで区切ります。空ならすべて' }, '出すカレンダー ',
        h('input', { value: (own.calendars ?? []).join('、'), placeholder: names.length ? `すべて（${names.join('、')}）` : 'すべて', 'data-focus': 'calendars', id: 'cal-names',
          onchange: (e: Event) => this.patchAction({ calendars: (e.target as HTMLInputElement).value.split(/[,、]/).map((x) => x.trim()).filter(Boolean) }) })),
    ]
    if (staleBad) out.push(h('div', { class: 'err' }, '「古いとみなすまで」は、3h、90m のような 1 分から 720 時間の時間です'))
    const unknown = (own.calendars ?? []).filter((n) => this.calendar && !names.includes(n))
    if (unknown.length) out.push(h('div', { class: 'warn' }, `Brain にないカレンダーです：${unknown.join('、')}（brain-deck の calendars.yaml の name と合わせてください）`))
    if (this.connected && this.hello?.commands?.includes('get_calendar')) {
      const shown = calShown(calWidgetOf(own), this.calendar)
      const total = shown.reduce((n, c) => n + (c.events?.length ?? 0), 0)
      const at = shown.map((c) => c.fetched_at).filter(Boolean).sort()[0]
      const failed = shown.filter((c) => c.error).map((c) => `${c.name}（${c.error}）`)
      out.push(h('p', { class: 'hint cal-now' }, shown.length
        ? `今の予定：${shown.map((c) => c.name).join('、')} の ${total} 件。最終更新 ${at ? new Date(at).toLocaleString() : 'なし'}` +
            (failed.length ? `。取得に失敗：${failed.join('、')}` : '')
        : '今の予定：まだありません（Brain では「予定を受け取っていません」と出ます）'))
    } else if (this.connected) {
      out.push(h('div', { class: 'warn' }, 'Brain の lefthand がカレンダーに対応していません。lefthand を新しくしてください'))
    }
    out.push(h('p', { class: 'hint' },
      '予定は設定ファイルには入らず、Brain に別に保存します（設定を保存しても消えません）。PC で「brain-deck calendar sync」を実行すると、' +
        '~/.config/brain-deck/calendars.yaml に書いたカレンダー（ICS の URL）から予定を取ってきて送ります。今日の予定と次の予定を出し、' +
        '今の予定は行を塗って目立たせます。入りきらないときは、セルの下の ▲ ▼ でページを送ります。設定 GUI が接続しているあいだは、brain-deck から送れません。'))
    return out
  }

  private viewClockEditor(text: (f: 'format' | 'date_format' | 'tz', label: string, placeholder: string, title: string) => HTMLElement, missing: string[]): (HTMLElement | null)[] {
    return [
      text('format', '時刻の書式 ', DEFAULT_CLOCK_FORMAT, '例：15:04（24 時間）、15:04:05（秒も出す。1 秒ごとに描き直す）、3:04 PM'),
      text('date_format', '日付の書式 ', DEFAULT_DATE_FORMAT, '例：1月2日({wday})、2006/01/02 Mon。none で日付を出さない。{wday} は日本語の曜日'),
      text('tz', 'タイムゾーン ', 'Brain のタイムゾーン', '例：Asia/Tokyo、America/Los_Angeles、UTC。省略すると Brain のタイムゾーン'),
      h('p', { class: 'hint' }, '書式は Go の書き方です（2006=年、01 か 1=月、02 か 2=日、15=時、04=分、05=秒、Mon=曜日、{wday}=日本語の曜日）。時刻を一度も合わせていないあいだは、Brain では「時刻未設定」と橙色で出ます。'),
      missing.length ? h('div', { class: 'warn' }, `Brain のフォントにない文字があります（□ になります）：${missing.join(' ')}`) : null,
    ]
  }

  // mouseWarning は、Brain の USB にマウスがないとき（ブートキーボードの形）の注意と、切り替えるボタン。
  private mouseWarning(): HTMLElement | null {
    if (!this.connected || this.brainMouse !== false) return null
    const can = !!this.hello?.commands?.includes('set_usb_mode')
    return h('div', { class: 'warn' },
      'Brain の USB は今、キーボードだけ（ブートキーボード）の形で、マウスは PC に届きません。',
      can ? h('button', { class: 'small', id: 'usb-mouse-on', onclick: () => void this.setUsbMode('mouse') }, 'マウスをオンにする') : null,
      can ? ' USB を付け直すので、2〜3 秒、キー入力と SSH が切れ、この GUI の接続も切れます（つなぎ直してください）。'
        : ' Brain の lefthand を新しくしてください（README の「マウスとトラックパッド」）。')
  }

  // setUsbMode は、Brain の USB の形を切り替える。返事のあとに USB が付け直され、シリアルも切れる。
  async setUsbMode(mode: UsbMode): Promise<void> {
    if (!this.client) return
    try {
      const r = await this.client.request<{ mouse: boolean; switching: boolean }>('set_usb_mode', { mode })
      this.say(r.switching ? `Brain の USB を${r.mouse ? 'キーボードとマウス' : 'キーボードだけ'}の形に切り替えます。数秒後に接続し直してください` : 'すでにその形です')
    } catch (e: any) {
      this.say(`切り替えられません：${e?.message ?? e}`, 'error')
    }
  }

  // viewUsbModeEditor は、USB の形の切り替えの欄。
  private viewUsbModeEditor(own: ActionSpec): HTMLElement[] {
    return [
      h('label', { class: 'row' }, '切り替え ', h('select', { 'data-focus': 'usb_mode', id: 'usb_mode',
        onchange: (e: Event) => this.patchAction({ usb_mode: (e.target as HTMLSelectElement).value as UsbMode }) },
        ([['toggle', '押すたびにオンとオフ（toggle）'], ['mouse', 'マウスをオン（mouse）'], ['keyboard', 'マウスをオフ（keyboard）']] as const)
          .map(([v, t]) => h('option', { value: v, selected: own.usb_mode === v }, t)))),
      h('p', { class: 'hint' }, 'Brain は起動したとき、USB をキーボードだけ（ブートキーボード。BIOS でも使える形）にします。マウス（トラックパッド、マウスの割り当て）を使うときに、' +
        'キーボードとマウスの形に切り替えます。USB を付け直すので、2〜3 秒、キー入力、SSH、設定 GUI の接続が切れます。'),
    ]
  }

  // viewTermEditor は、端末モードの切り替えの欄。
  private viewTermEditor(own: ActionSpec): HTMLElement[] {
    return [
      h('label', { class: 'row' }, '切り替え ', h('select', { 'data-focus': 'terminal', id: 'terminal',
        onchange: (e: Event) => this.patchAction({ terminal: (e.target as HTMLSelectElement).value as TermMode }) },
        ([['toggle', '押すたびに入る・抜ける（toggle）'], ['on', '入る（on）'], ['off', '抜ける（off）']] as const)
          .map(([v, t]) => h('option', { value: v, selected: own.terminal === v }, t)))),
      h('p', { class: 'hint' }, '端末モードでは、Brain の画面とキーボードで PC にログインして操作します（PC 側の設定が要ります。README の「端末モード」）。' +
        '端末モードのあいだ、本体キーは PC にキーとして送らず、端末の入力になります。抜けるのは、画面右の帯の HOME か、文字切り替え + 戻る。' +
        '入るとき・抜けるときに USB を付け直すので、数秒、キー入力、SSH、設定 GUI の接続が切れます。'),
    ]
  }

  // setTerminal は、Brain の端末モードを切り替える。シリアルの端末モードでは、返事のあとに USB が付け直される。
  async setTerminal(mode: TermMode): Promise<void> {
    if (!this.client) return
    try {
      const r = await this.client.request<{ terminal: boolean; info: TermInfo }>('set_terminal', { mode })
      this.brainTerm = { ...(this.brainTerm ?? { state: 'off' }), active: r.terminal } as TermInfo
      this.say(r.terminal ? 'Brain を端末モードにします。USB を付け直すので、数秒後に接続し直してください'
        : 'Brain の端末モードを抜けます。USB を付け直すので、数秒後に接続し直してください')
      this.render()
    } catch (e: any) {
      this.say(`切り替えられません：${e?.message ?? e}`, 'error')
    }
  }

  // viewTermPanel は、コンソールのタブに出す、端末モードの状態と切り替えのボタン。
  private viewTermPanel(): HTMLElement | null {
    if (!this.connected || !this.hello?.commands?.includes('set_terminal')) return null
    const t = this.brainTerm
    const on = !!t?.active
    const state = !t || t.state === 'off' ? 'オフ' : t.state === 'entering' ? '入っている途中' : t.state === 'leaving' ? '抜けている途中'
      : `オン（${t.transport === 'command' ? 'コマンド' : 'シリアル'}${t.cols ? ` ${t.cols}×${t.rows}` : ''}）${t.status ? '：' + t.status : ''}`
    return h('div', { class: on ? 'warn' : 'hint', id: 'terminal-panel' },
      h('strong', null, '端末モード '), h('span', { id: 'terminal-state' }, state), ' ',
      h('button', { class: 'small', id: 'terminal-toggle', onclick: () => void this.setTerminal(on ? 'off' : 'on') },
        on ? '端末モードを抜ける' : '端末モードに入る'),
      on ? ' 端末モードのあいだ、Brain のログイン画面（このタブの接続先）は止めてあります。抜けると戻ります。'
        : ' Brain の画面とキーボードで、この PC（または USB でつないだ PC）にログインします。入るとき・抜けるときに USB を付け直すので、数秒、この GUI の接続が切れます。')
  }

  // viewMouseEditor は、マウスの操作の欄。
  private viewMouseEditor(own: ActionSpec): HTMLElement[] {
    return [
      h('label', { class: 'row' }, 'マウスの操作 ', h('select', { 'data-focus': 'mouse', id: 'mouse',
        onchange: (e: Event) => this.patchAction({ mouse: (e.target as HTMLSelectElement).value as MouseAction }) },
        (Object.keys(MOUSE_LABELS) as MouseAction[]).map((m) => h('option', { value: m, selected: own.mouse === m }, `${MOUSE_LABELS[m]}（${m}）`)))),
      h('p', { class: 'hint' }, 'ボタンは押しているあいだ押したままになります（ドラッグにも使えます）。スクロールは 1 段送り、押し続けると繰り返します。' +
        'レイヤーが変わると、押しているボタンは離します。'),
      ...[this.mouseWarning()].filter((x): x is HTMLElement => !!x),
    ]
  }

  // viewPadEditor は、トラックパッドの欄。空の欄は既定値（placeholder に出す）。
  private viewPadEditor(own: ActionSpec): HTMLElement[] {
    type NumField = (typeof PAD_NUM_FIELDS)[number]
    const num = (f: NumField, label: string, min: number, max: number, step: number, title: string, int = false) => {
      const v = own[f]
      const bad = v !== undefined && (v < min || v > max || (int && !Number.isInteger(v)))
      return [
        h('label', { class: 'row', title }, label,
          h('input', { type: 'number', min, max, step, value: v ?? '', placeholder: String(PAD_DEFAULTS[f]), 'data-focus': f, id: `pad-${f}`, class: 'num',
            onchange: (e: Event) => {
              const t = (e.target as HTMLInputElement).value.trim()
              this.patchAction({ [f]: t === '' ? undefined : Number(t) })
            } })),
        bad ? h('div', { class: 'err' }, `${label.trim()}は ${min}〜${max}${int ? 'の整数' : ''}です`) : null,
      ]
    }
    const sel = <F extends 'scroll_direction' | 'long_press'>(f: F, label: string, opts: [string, string][], title: string) =>
      h('label', { class: 'row', title }, label, h('select', { 'data-focus': f, id: `pad-${f}`,
        onchange: (e: Event) => this.patchAction({ [f]: (e.target as HTMLSelectElement).value || undefined }) },
        opts.map(([v, t]) => h('option', { value: v, selected: (own[f] ?? '') === v }, t))))
    const group = (title: string, ...items: (HTMLElement | null | (HTMLElement | null)[])[]) =>
      h('fieldset', { class: 'pad-group' }, h('legend', null, title), items.flat())
    return [
      group('動き',
        num('speed', '感度 ', 0.05, 20, 0.1, '画面の 1 ドットの動きを、PC のマウスの何カウントにするか（ゆっくり動かしたとき）'),
        num('accel', '加速 ', 0, 10, 0.1, '速く動かしたときに大きく動かす強さ。0 で加速しない。速さ 1000 ドット/秒で (1 + 加速) 倍、3000 ドット/秒以上で (1 + 3×加速) 倍')),
      group('スクロール（右端の帯）',
        num('scroll_width', '帯の幅 ', 0, 400, 1, 'セルの右端の、スクロールに使う帯の幅（ドット）。0 で帯なし', true),
        sel('scroll_direction', '向き ', [['', `既定（${PAD_DEFAULTS.scroll_direction === 'natural' ? 'ナチュラル' : 'ホイールと同じ'}）`], ['natural', 'ナチュラル（指と同じ向きに中身が動く）'], ['traditional', 'ホイールと同じ（指を下へ＝下へスクロール）']],
          'ナチュラル：スマホと同じく、指を下へ動かすと中身が下へ動く（上へスクロール）'),
        num('scroll_step', '1 段の動き ', 2, 400, 1, 'ホイール 1 段に当たる指の動き（ドット）。小さいほど速くスクロールする')),
      group('タップ',
        num('tap_ms', 'タップの長さ ', 30, 1000, 10, 'これより短く触れて離せばタップ（ミリ秒）', true),
        num('tap_move', 'タップの動き ', 0, 200, 1, 'タップとみなす動きの上限（ドット）'),
        num('drag_ms', 'ドラッグの待ち ', 0, 1000, 10, 'タップで離してから、この時間のうちに触れて動かすとドラッグ（ミリ秒）。0 にすると、タップしたら（着地の跳ねを待つ 0.11 秒あとに）クリックし、タップでのドラッグはしない', true),
        sel('long_press', '長押し ', [['', '既定（何もしない）'], ['none', '何もしない'], ['right', '右クリック（0.6 秒）']],
          '指を止めて 0.6 秒押すと右クリック。指を止めたまま考えているときにも右クリックになるので、既定では使わない')),
      group('ブレ対策',
        num('settle_ms', '触れた直後に捨てる ', 0, 500, 5, '触れてから、この時間のサンプルを使わない（ミリ秒）。触れた瞬間の座標の跳ねを捨てる', true),
        num('smooth', '平均するサンプル数 ', 1, 10, 1, '最後のこの数のサンプルの平均を使う。大きいほど滑らかで、少し遅れる', true),
        num('deadzone', '無視する動き ', 0, 50, 0.5, '止めているときの揺れとして無視する動き（ドット）'),
        num('min_pressure', '押す強さの下限 ', 0, 4095, 10, 'ABS_PRESSURE がこれより小さいサンプルを捨てる。0 で見ない', true)),
      h('p', { class: 'hint' }, '指を動かすとカーソルが動きます。短いタップで左クリック、タップしてすぐ触れて動かすとドラッグ、右端の帯をなぞるとスクロールします。' +
        '右クリックなどのボタンは、ほかのセルに「マウス」の割り当てで置きます。空の欄は既定値（薄く出している値）を使います。値の決め方は README の「トラックパッドの調整」。'),
      ...[this.mouseWarning()].filter((x): x is HTMLElement => !!x),
    ]
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
  if (k === 'mouse') return a.label ? a.label.replace(/\n/g, ' ') : (MOUSE_LABELS[a.mouse!] ?? a.mouse ?? '')
  if (k === 'usb_mode') return a.label ? a.label.replace(/\n/g, ' ') : (USB_LABELS[a.usb_mode!] ?? a.usb_mode ?? '')
  if (k === 'terminal') return a.label ? a.label.replace(/\n/g, ' ') : (TERM_LABELS[a.terminal!] ?? a.terminal ?? '')
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
