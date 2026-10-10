// 設定の操作。透過の解決は layer.go の view / lookupKey と同じ規則にする。

import type { ActionSpec, Config, GridConfig, InputKind, LayerConfig, MediaAction, MouseAction, TermMode, TouchConfig, UsbMode, WidgetKind } from './types'

export type ActionKind = 'key' | 'none' | 'layer_hold' | 'layer_toggle' | 'layer_oneshot' | 'layer_to' | 'mouse' | 'media' | 'usb_mode' | 'terminal' | 'rotate' | 'input' | 'widget'
export const LAYER_KINDS = ['layer_hold', 'layer_toggle', 'layer_oneshot', 'layer_to'] as const
export type LayerKind = (typeof LAYER_KINDS)[number]

export const KIND_LABELS: Record<ActionKind, string> = {
  key: 'キーを送る',
  none: '何もしない（none）',
  layer_hold: '押しているあいだ（layer_hold）',
  layer_toggle: '押すたびに出し入れ（layer_toggle）',
  layer_oneshot: '次の 1 キーだけ（layer_oneshot）',
  layer_to: 'そのレイヤーへ移る（layer_to）',
  mouse: 'マウス（ボタン、スクロール）',
  media: 'メディアキー（再生、音量など）',
  usb_mode: 'USB の形の切り替え（マウスのオンとオフ）',
  terminal: '端末モード（Brain で PC にログインする）',
  rotate: '画面の向きを変える（rotate）',
  input: 'Brain のキーボードで入れる（Todo を足す、時刻を合わせる）',
  widget: 'ウィジェット（時計、テキスト、Todo、カレンダー、トラックパッドなど）',
}

// マウスの操作と、画面に出す名前（mouse.go の mouseActions）
export const MOUSE_LABELS: Record<MouseAction, string> = {
  left: '左クリック',
  right: '右クリック',
  middle: '中クリック',
  scroll_up: 'スクロール↑',
  scroll_down: 'スクロール↓',
  scroll_left: 'スクロール←',
  scroll_right: 'スクロール→',
}

// メディアキーと、画面に出す名前（media.go の mediaActions。並びは mediaActionNames）
export const MEDIA_LABELS: Record<MediaAction, string> = {
  play_pause: '再生/停止',
  stop: '停止',
  next: '次の曲',
  previous: '前の曲',
  fast_forward: '早送り',
  rewind: '巻き戻し',
  volume_up: '音量+',
  volume_down: '音量-',
  mute: 'ミュート',
  brightness_up: '明るさ+',
  brightness_down: '明るさ-',
  calculator: '電卓',
  browser: 'ブラウザ',
  mail: 'メール',
}

// メディアキーを送れない形（ブートキーボード）のときに、セルの下に出す文字（media.go の mediaOffLabel）
export const MEDIA_OFF_LABEL = 'マウスオフでは無効'

// USB の形の切り替えと、画面に出す名前（usbmode.go の usbLabels）
export const USB_LABELS: Record<UsbMode, string> = { keyboard: 'マウスオフ', mouse: 'マウスオン', toggle: 'マウス切替' }

// 端末モードの切り替えと、画面に出す名前（termmode.go の termLabels）
export const TERM_LABELS: Record<TermMode, string> = { on: '端末', off: '端末を出る', toggle: '端末' }

// ACTION_FIELDS は、割り当ての種類を決める項目（ちょうど 1 つを書く）。ウィジェットのセルでは省略できる
export const ACTION_FIELDS = ['key', ...LAYER_KINDS, 'mouse', 'media', 'usb_mode', 'terminal', 'rotate', 'input'] as const

// Brain のキーボードで文字を入れる欄（prompt.go の inputLabels）。セルに出す名前
export const INPUT_LABELS: Record<InputKind, string> = { todo: 'Todo追加', clock: '時刻合わせ' }

// PC から外れているあいだ、休みにしたセルの下とタイルの隅に出す文字（offline.go の offlineLabel）
export const OFFLINE_LABEL = 'PCなし'

// linkOnline は、PC とつながっている（スリープ中を含む。分からないときも）か（link.go の linkOnline）。
export function linkOnline(link: string): boolean {
  return link === '' || link === 'configured' || link === 'suspended'
}

// needsPC は、PC とつながっていないと働かない割り当てか、PC から受け取るタイルか（offline.go の needsPC と fromPC）。
export function needsPC(a: ActionSpec): boolean {
  const k = actionKind(a)
  return k === 'key' || k === 'mouse' || k === 'media' || k === 'terminal' || a.widget === 'trackpad' || a.widget === 'pentablet' || a.widget === 'text' || a.widget === 'graph'
}

// 画面の向き（rotate.go）。セルに出す名前
export const ROTATE_VALUES = ['next', '0', '90', '180', '270'] as const
export const ROTATE_LABELS: Record<string, string> = { next: '回す', '0': '横向き', '90': '縦向き', '180': '横向き(逆)', '270': '縦向き(逆)' }

// ウィジェットの種類と、画面に出す名前（widget.go の widgetKinds）
export const WIDGET_LABELS: Record<WidgetKind, string> = { clock: '時計', text: 'テキスト', todo: 'Todo', calendar: 'カレンダー', trackpad: 'トラックパッド',
  battery: '電池', stopwatch: 'ストップウォッチ', pomodoro: 'ポモドーロ', counter: 'カウンター', qr: 'QR コード', presence: '在席ランプ', month: '月の表示', weather: '天気', graph: 'グラフ', presenter: 'プレゼン用タイマー', calculator: '電卓',
  pentablet: 'ペンタブレット', memo: '手書きメモ' }
// トラックパッドの項目（数）
export const PAD_NUM_FIELDS = ['speed', 'accel', 'scroll_width', 'scroll_step', 'settle_ms', 'smooth', 'deadzone', 'min_pressure', 'tap_ms', 'tap_move', 'drag_ms'] as const
// トラックパッドの項目
export const PAD_FIELDS = [...PAD_NUM_FIELDS, 'scroll_direction', 'long_press'] as const
// トラックパッドの既定値（trackpad.go の defaultPad。gui/test/fixtures/pad-defaults.json で Go と比べる）
export const PAD_DEFAULTS = {
  speed: 1.2,
  accel: 2.0,
  scroll_width: 96,
  scroll_direction: 'natural',
  scroll_step: 24,
  settle_ms: 80,
  smooth: 3,
  deadzone: 1.5,
  min_pressure: 500,
  tap_ms: 250,
  tap_move: 8,
  drag_ms: 300,
  long_press: 'none',
} as const
// ウィジェットが押した位置で働く（タップしたときのキーやレイヤーを書けない）
export function ownsTouch(w: WidgetKind | undefined): boolean {
  return w === 'todo' || w === 'calendar' || w === 'trackpad' || w === 'month' || w === 'calculator' || w === 'pentablet' || w === 'memo' || isTapWidget(w)
}
// タップと長押しを自分で処理する（タップしたときのキーやレイヤーを書けない。押すと枠が光る）
export function isTapWidget(w: WidgetKind | undefined): boolean {
  return w === 'stopwatch' || w === 'pomodoro' || w === 'counter' || w === 'presence' || w === 'presenter'
}
// id を書けるウィジェット（text と qr は必須、ほかは省略すると種類の名前）
export function hasStateId(w: WidgetKind | undefined): boolean {
  return w === 'text' || w === 'qr' || w === 'month' || w === 'graph' || w === 'calculator' || isTapWidget(w)
}
// widget.go の既定の書式
export const DEFAULT_CLOCK_FORMAT = '15:04'
export const DEFAULT_DATE_FORMAT = '1月2日({wday})'
// グラフの項目（数）
export const GRAPH_NUM_FIELDS = ['min', 'max', 'points', 'decimals'] as const
// グラフの項目
export const GRAPH_FIELDS = ['unit', ...GRAPH_NUM_FIELDS, 'chart'] as const
// ウィジェットにだけ書ける項目
export const WIDGET_FIELDS = ['widget', 'format', 'date_format', 'tz', 'id', 'rows', 'page_reset', 'stale', 'calendars', 'follow', 'week_start', 'work', 'break', 'states', 'duration', 'colors', 'prefix', ...GRAPH_FIELDS, ...PAD_FIELDS] as const
// ウィジェットの種類ごとの項目
export const CLOCK_FIELDS = ['format', 'date_format', 'tz'] as const

// 画面に出す、レイヤー切り替えの種類（main.go の layerVerb と同じ）
export const LAYER_VERB: Record<LayerKind, string> = {
  layer_hold: '押す間',
  layer_toggle: '切替',
  layer_oneshot: '1回',
  layer_to: '移動',
}

// actionKind は割り当ての種類を返す。選びかけ（key や行き先が空文字）でも、書いてある項目で決める。
// ウィジェットのセルでも key や layer_* があれば、その種類（タップしたときの動き）を返す。
export function actionKind(a: ActionSpec): ActionKind {
  if (a.key !== undefined) return a.key.toLowerCase() === 'none' ? 'none' : 'key'
  for (const k of LAYER_KINDS) if (a[k] !== undefined) return k
  if (a.mouse !== undefined) return 'mouse'
  if (a.media !== undefined) return 'media'
  if (a.usb_mode !== undefined) return 'usb_mode'
  if (a.terminal !== undefined) return 'terminal'
  if (a.rotate !== undefined) return 'rotate'
  if (a.input !== undefined) return 'input'
  if (a.widget !== undefined) return 'widget'
  return 'none'
}

// isIncomplete は、送るキーや行き先をまだ選んでいない割り当てかどうか。
export function isIncomplete(a: ActionSpec): boolean {
  const fields = ACTION_FIELDS.filter((k) => a[k] !== undefined)
  if (a.widget !== undefined) return !a.widget || ((a.widget === 'text' || a.widget === 'qr') && !a.id) || fields.some((k) => a[k] === '')
  return fields.length === 0 || fields.some((k) => a[k] === '')
}

// spanOf はセルの大きさ [列数, 行数]。書いていない、おかしいときは [1, 1]。
export function spanOf(a: ActionSpec | null | undefined): [number, number] {
  const s = a?.span
  return Array.isArray(s) && s[0] >= 1 && s[1] >= 1 ? [s[0], s[1]] : [1, 1]
}

export function actionTarget(a: ActionSpec): string | undefined {
  for (const k of LAYER_KINDS) if (a[k] !== undefined) return a[k]
  return undefined
}

export function isLayerAction(a: ActionSpec | null | undefined): boolean {
  return !!a && LAYER_KINDS.some((k) => !!a[k])
}

// describeAction は割り当てを短く書く（一覧や差分の表示用）。
export function describeAction(a: ActionSpec | undefined | null): string {
  if (!a) return '（透過）'
  const k = actionKind(a)
  let s: string
  if (k === 'key') s = a.key!
  else if (k === 'none') s = 'none'
  else if (k === 'widget') s = ''
  else if (k === 'mouse') s = `mouse: ${a.mouse}`
  else if (k === 'media') s = `media: ${a.media}`
  else if (k === 'usb_mode') s = `usb_mode: ${a.usb_mode}`
  else if (k === 'terminal') s = `terminal: ${a.terminal}`
  else if (k === 'rotate') s = `rotate: ${a.rotate}`
  else if (k === 'input') s = `input: ${a.input}`
  else s = `${k}: ${actionTarget(a)}`
  if (a.widget !== undefined) {
    const opts = (['format', 'date_format', 'tz', 'id', 'rows', 'work', 'break'] as const).filter((f) => a[f]).map((f) => `${f}=${a[f]}`)
    if (a.states) opts.push(`states=${a.states.map((s) => s.name).join('/')}`)
    const w = `widget: ${a.widget}${opts.length ? `（${opts.join(', ')}）` : ''}`
    s = s ? `${w}、タップで ${s}` : w
  }
  const [w, h] = spanOf(a)
  if (w !== 1 || h !== 1) s += ` [${w}×${h}]`
  return a.label ? `${s}「${a.label.replace(/\n/g, '⏎')}」` : s
}

// ---------- 読み込んだ設定をそろえる ----------

// normalizeAction は `B`、`1`（YAML の数）、`{ key: B }` を、オブジェクトの形にそろえる。
export function normalizeAction(v: unknown): ActionSpec {
  if (typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean') return { key: String(v) }
  if (v && typeof v === 'object') {
    const out: Record<string, unknown> = {}
    for (const [k, x] of Object.entries(v)) {
      if (x === undefined || x === null) continue
      // 数で書いたキー（`1`）は文字にする。todo の rows、グラフとトラックパッドの項目は数のまま
      out[k] = typeof x === 'number' && k !== 'rows' && !(GRAPH_NUM_FIELDS as readonly string[]).includes(k) && !(PAD_NUM_FIELDS as readonly string[]).includes(k) ? String(x) : x
    }
    return out as ActionSpec
  }
  return {}
}

function normalizeMap(m: unknown): Record<string, ActionSpec> | undefined {
  if (!m || typeof m !== 'object') return undefined
  const out: Record<string, ActionSpec> = {}
  for (const [k, v] of Object.entries(m)) out[String(k)] = normalizeAction(v)
  return out
}

// normalizeConfig は、ファイルから読んだ設定を GUI で扱う形にする。
// 旧形式（トップレベルの keys と touch.cells）は、base レイヤーに移す（config.go の normalize と同じ）。
// 形のおかしいところは、そのまま残してデーモンの検証に任せる。
export function normalizeConfig(raw: any): Config {
  const cfg: any = raw && typeof raw === 'object' && !Array.isArray(raw) ? structuredClone(raw) : {}
  const t: any = cfg.touch
  const legacyTouch = t && (t.cols !== undefined || t.rows !== undefined || t.cells !== undefined)
  if (!Array.isArray(cfg.layers) || cfg.layers.length === 0) {
    if (cfg.keys !== undefined || legacyTouch || !cfg.layers) {
      const base: LayerConfig = { name: 'base' }
      if (cfg.keys) base.keys = normalizeMap(cfg.keys)
      if (t && legacyTouch) {
        base.touch = { cols: t.cols, rows: t.rows, cells: normalizeMap(t.cells) }
        delete t.cols
        delete t.rows
        delete t.cells
      }
      delete cfg.keys
      cfg.layers = [base]
    }
  }
  for (const l of cfg.layers ?? []) {
    if (!l || typeof l !== 'object') continue
    if (l.name !== undefined) l.name = String(l.name)
    if (l.keys) l.keys = normalizeMap(l.keys)
    if (l.soft_keys) l.soft_keys = normalizeMap(l.soft_keys)
    if (l.touch?.cells) l.touch.cells = normalizeMap(l.touch.cells)
    if (l.touch_portrait?.cells) l.touch_portrait.cells = normalizeMap(l.touch_portrait.cells)
  }
  for (const a of cfg.home?.apps ?? []) {
    if (a && typeof a === 'object') for (const f of ['name', 'icon', 'layer']) if (typeof a[f] === 'number') a[f] = String(a[f])
  }
  return cfg as Config
}

// clean は空のオブジェクト（keys: {} など）を取り除き、保存する形にする。
export function clean(cfg: Config): Config {
  const c = structuredClone(cfg)
  for (const l of c.layers) {
    if (l.keys && Object.keys(l.keys).length === 0) delete l.keys
    if (l.soft_keys && Object.keys(l.soft_keys).length === 0) delete l.soft_keys
    if (l.touch?.cells && Object.keys(l.touch.cells).length === 0) delete l.touch.cells
    if (l.touch_portrait?.cells && Object.keys(l.touch_portrait.cells).length === 0) delete l.touch_portrait.cells
    if (l.label === '') delete l.label
  }
  if (c.touch?.soft_areas && Object.keys(c.touch.soft_areas).length === 0) delete c.touch.soft_areas
  return c
}

// ---------- 透過の解決 ----------

export interface ResolvedGrid {
  cols: number
  rows: number
  cells: (ResolvedAction | null)[] // row*cols+col。割り当ての左上のセルにだけ入る。null は割り当てなし
  // anchor は、セルを覆う割り当ての左上のセルの番号。覆うものがなければ -1（span のないセルは自分自身）
  anchor: number[]
  owner: number // 格子の大きさを決めたレイヤー
  // wallpaper は格子に敷く壁紙の id（layer.go の View.Wallpaper と同じ）。格子を作ったレイヤーから下へ、
  // セルと同じく透過する範囲で、いちばん上にある touch.background
  wallpaper?: string
}

export interface ResolvedAction {
  action: ActionSpec
  from: number // 割り当てを書いたレイヤー
}

export function cellKey(col: number, row: number): string {
  return `${col},${row}`
}

export function parseCellKey(k: string): [number, number] | null {
  const m = k.match(/^(\d+),(\d+)$/)
  return m ? [+m[1], +m[2]] : null
}

// Orient は画面の向き（横向きは 800x480、縦向きは 480x800）。
export type Orient = 'landscape' | 'portrait'

// LayerGrid は、向きに合ったレイヤー自身の格子（rotate.go の portraitOf と同じ規則で作る）。
export interface LayerGrid {
  cols: number
  rows: number
  cells: Record<string, ActionSpec>
  background?: string
  own: boolean // 縦向きで touch_portrait を書いた（false なら横向きの格子から作ったもの）
}

// gridSize はレイヤー自身の格子の大きさ。touch がなければ null（下のレイヤーの格子を使う）。
export function gridSize(cfg: Config, li: number, orient: Orient = 'landscape'): { cols: number; rows: number } | null {
  if (orient === 'portrait') {
    const g = layerGrid(cfg, li, 'portrait')
    return g ? { cols: g.cols, rows: g.rows } : null
  }
  const t = cfg.layers[li]?.touch
  if (!t) return null
  if (t.cols && t.rows) return { cols: t.cols, rows: t.rows }
  const b = cfg.layers[0]?.touch
  if (li !== 0 && b?.cols && b?.rows) return { cols: b.cols, rows: b.rows }
  return null
}

// layerGrid は、向き orient でのレイヤー li の格子。touch がなければ null。
export function layerGrid(cfg: Config, li: number, orient: Orient): LayerGrid | null {
  const l = cfg.layers[li]
  const size = gridSize(cfg, li)
  if (!l?.touch || !size) return null
  if (orient === 'landscape') return { ...size, cells: l.touch.cells ?? {}, background: l.touch.background, own: true }
  const p = l.touch_portrait
  if (p) {
    let cols = p.cols ?? 0
    let rows = p.rows ?? 0
    if (!cols || !rows) {
      const b = li !== 0 ? layerGrid(cfg, 0, 'portrait') : null
      if (!b) return null
      ;({ cols, rows } = b)
    }
    return { cols, rows, cells: p.cells ?? {}, background: p.background, own: true }
  }
  if (cfg.display?.portrait_grid === 'stretch') return { ...size, cells: l.touch.cells ?? {}, background: l.touch.background, own: false }
  return { cols: size.rows, rows: size.cols, cells: transposeCells(l.touch.cells ?? {}), background: l.touch.background, own: false }
}

// transposeCells は、セルの列と行を入れ替える（span も入れ替える）。
export function transposeCells(cells: Record<string, ActionSpec>): Record<string, ActionSpec> {
  const out: Record<string, ActionSpec> = {}
  for (const [k, a] of Object.entries(cells)) {
    const p = parseCellKey(k)
    if (!p) continue
    const b = { ...a }
    const [w, h] = spanOf(a)
    if (w !== 1 || h !== 1) b.span = [h, w]
    out[cellKey(p[1], p[0])] = b
  }
  return out
}

// resolveGrid は、レイヤーの番号を下から並べた stack について、透過を解決した格子を返す。
// none は「割り当てなし」になる（下のレイヤーも使わない）。
// span のあるセルは、覆う範囲がすべて上のレイヤーで空いているときだけ使う（layer.go の view と同じ）。
export function resolveGrid(cfg: Config, stack: number[], orient: Orient = 'landscape'): ResolvedGrid {
  let cols = 0
  let rows = 0
  let owner = -1
  let taken: boolean[] = []
  let wallpaper: string | undefined
  const anchors = new Map<number, ResolvedAction>()
  for (let i = stack.length - 1; i >= 0; i--) {
    const li = stack[i]
    const lg = layerGrid(cfg, li, orient)
    if (!lg) continue
    if (owner < 0) {
      ;({ cols, rows } = lg)
      owner = li
      taken = new Array(cols * rows).fill(false)
    } else if (lg.cols !== cols || lg.rows !== rows) {
      break
    }
    wallpaper ||= lg.background || undefined
    const claim: number[] = []
    for (const [k, a] of Object.entries(lg.cells)) {
      const p = parseCellKey(k)
      if (!p || p[0] >= cols || p[1] >= rows) continue
      const [w, h] = spanOf(a)
      let free = true
      for (let r = p[1]; r < Math.min(p[1] + h, rows); r++)
        for (let c = p[0]; c < Math.min(p[0] + w, cols); c++) {
          free &&= !taken[r * cols + c]
          claim.push(r * cols + c)
        }
      if (free) anchors.set(p[1] * cols + p[0], { action: a, from: li })
    }
    for (const j of claim) taken[j] = true
  }
  const cells: (ResolvedAction | null)[] = new Array(cols * rows).fill(null)
  const anchor: number[] = new Array(cols * rows).fill(-1)
  for (const [i, r] of anchors) {
    if (actionKind(r.action) === 'none') continue
    cells[i] = r
    const [w, h] = spanOf(r.action)
    const c0 = i % cols
    const r0 = Math.floor(i / cols)
    for (let y = r0; y < Math.min(r0 + h, rows); y++) for (let x = c0; x < Math.min(c0 + w, cols); x++) anchor[y * cols + x] = i
  }
  return { cols, rows, cells, anchor, owner, wallpaper }
}

// barShown は、stack を重ねたときに状態の帯を出すか（layer.go の view と同じ規則）。
// 上から見て、status_bar を書いたレイヤーか、格子（touch）を持つレイヤーで決める。
export function barShown(cfg: Config, stack: number[]): boolean {
  for (let i = stack.length - 1; i >= 0; i--) {
    const l = cfg.layers[stack[i]]
    if (l?.status_bar !== undefined) return l.status_bar
    if (l?.touch) break
  }
  return cfg.status_bar?.show ?? false
}

export function anchorOf(g: ResolvedGrid, col: number, row: number): [number, number] {
  const i = g.anchor[row * g.cols + col] ?? -1
  return i < 0 ? [col, row] : [i % g.cols, Math.floor(i / g.cols)]
}

// resolveCell は、セルの割り当てを透過も含めて返す（none のときも返す。編集画面用）。
export function resolveCell(cfg: Config, stack: number[], col: number, row: number, orient: Orient = 'landscape'): ResolvedAction | null {
  let size: { cols: number; rows: number } | null = null
  for (let i = stack.length - 1; i >= 0; i--) {
    const li = stack[i]
    const s = layerGrid(cfg, li, orient)
    if (!s) continue
    if (!size) size = s
    else if (s.cols !== size.cols || s.rows !== size.rows) break
    const a = s.cells[cellKey(col, row)]
    if (a) return { action: a, from: li }
  }
  return null
}

export function resolveKey(cfg: Config, stack: number[], code: string): ResolvedAction | null {
  for (let i = stack.length - 1; i >= 0; i--) {
    const a = cfg.layers[stack[i]]?.keys?.[code]
    if (a) return { action: a, from: stack[i] }
  }
  return null
}

export function resolveSoft(cfg: Config, stack: number[], name: string): ResolvedAction | null {
  for (let i = stack.length - 1; i >= 0; i--) {
    const a = cfg.layers[stack[i]]?.soft_keys?.[name]
    if (a) return { action: a, from: stack[i] }
  }
  return null
}

// editStack は、レイヤー li を編集するときに重ねるもの（base の上に li）。
export function editStack(li: number): number[] {
  return li === 0 ? [0] : [0, li]
}

// ---------- タッチの座標 ----------

// cellOf は main.go の cellOf と同じ（min > max なら反転）。
export function cellOf(v: number, min: number, max: number, n: number): number {
  if (max === min || n <= 0) return 0
  let i = Math.trunc(((v - min) * n) / (max - min)) + 0 // -0 を 0 にする
  if (i < 0) i = 0
  if (i >= n) i = n - 1
  return i
}

export function touchCell(t: TouchConfig, cols: number, rows: number, x: number, y: number): [number, number] {
  if (t.swap_xy) [x, y] = [y, x]
  return [cellOf(x, t.min_x, t.max_x, cols), cellOf(y, t.min_y, t.max_y, rows)]
}

// ---------- レイヤーの操作 ----------

export interface Ref {
  layer: number
  where: 'keys' | 'cells' | 'soft_keys' | 'portrait_cells'
  id: string
  kind: LayerKind
}

// references は、name を行き先にしている割り当ての一覧。
export function references(cfg: Config, name: string): Ref[] {
  const out: Ref[] = []
  cfg.layers.forEach((l, li) => {
    const scan = (where: Ref['where'], m?: Record<string, ActionSpec>) => {
      for (const [id, a] of Object.entries(m ?? {}))
        for (const k of LAYER_KINDS) if (a[k] === name) out.push({ layer: li, where, id, kind: k })
    }
    scan('keys', l.keys)
    scan('cells', l.touch?.cells)
    scan('soft_keys', l.soft_keys)
    scan('portrait_cells', l.touch_portrait?.cells)
  })
  return out
}

function mapOf(l: LayerConfig, where: Ref['where']): Record<string, ActionSpec> | undefined {
  return where === 'cells' ? l.touch?.cells : where === 'portrait_cells' ? l.touch_portrait?.cells : l[where]
}

export function uniqueLayerName(cfg: Config, base = 'layer'): string {
  const names = new Set(cfg.layers.map((l) => l.name))
  for (let i = cfg.layers.length; ; i++) {
    const n = `${base}${i}`
    if (!names.has(n)) return n
  }
}

export function addLayer(cfg: Config, name = uniqueLayerName(cfg)): number {
  cfg.layers.push({ name, label: '' })
  return cfg.layers.length - 1
}

// renameLayer は名前を変え、参照している割り当てもすべて書き換える。
export function renameLayer(cfg: Config, li: number, name: string): void {
  const old = cfg.layers[li].name
  if (old === name) return
  for (const r of references(cfg, old)) {
    const a = mapOf(cfg.layers[r.layer], r.where)![r.id]
    a[r.kind] = name
  }
  for (const a of cfg.home?.apps ?? []) if (a.layer === old) a.layer = name
  cfg.layers[li].name = name
}

// deleteLayer はレイヤーを消し、そこを行き先にしていた割り当ても消す（透過に戻す）。
export function deleteLayer(cfg: Config, li: number): Ref[] {
  if (li === 0) throw new Error('base レイヤーは消せません')
  const name = cfg.layers[li].name
  const refs = references(cfg, name).filter((r) => r.layer !== li)
  for (const r of refs) delete mapOf(cfg.layers[r.layer], r.where)![r.id]
  if (cfg.home) cfg.home.apps = cfg.home.apps.filter((a) => a.layer !== name) // そのレイヤーに入るアプリも消す
  cfg.layers.splice(li, 1)
  return refs
}

// setAction は割り当てを書く。a が null なら消す（透過）。
export function setKeyAction(l: LayerConfig, code: string, a: ActionSpec | null): void {
  if (a) (l.keys ??= {})[code] = a
  else if (l.keys) delete l.keys[code]
}

export function setSoftAction(l: LayerConfig, name: string, a: ActionSpec | null): void {
  if (a) (l.soft_keys ??= {})[name] = a
  else if (l.soft_keys) delete l.soft_keys[name]
}

export function setCellAction(l: LayerConfig, col: number, row: number, a: ActionSpec | null, orient: Orient = 'landscape'): void {
  const k = cellKey(col, row)
  const f = orient === 'portrait' ? 'touch_portrait' : 'touch'
  if (a) {
    l[f] ??= {}
    ;(l[f]!.cells ??= {})[k] = a
  } else if (l[f]?.cells) delete l[f]!.cells![k]
}

// cellsOutside は、cols×rows の格子からはみ出すセルの番号を返す。
export function cellsOutside(g: GridConfig | undefined, cols: number, rows: number): string[] {
  return Object.keys(g?.cells ?? {}).filter((k) => {
    const p = parseCellKey(k)
    return !p || p[0] >= cols || p[1] >= rows
  })
}

// ---------- 誤りの場所 ----------

export type Location =
  | { kind: 'key'; layer: number; id: string }
  | { kind: 'cell'; layer: number; id: string; portrait?: boolean }
  | { kind: 'soft'; layer: number; id: string }
  | { kind: 'grid'; layer: number; portrait?: boolean }
  | { kind: 'home'; field?: string }
  | { kind: 'layer'; layer: number }
  | { kind: 'touch'; field?: string }
  | { kind: 'other'; path: string }

function unescape(s: string): string {
  return s.replace(/~1/g, '/').replace(/~0/g, '~')
}

// parsePath は JSON Pointer を、GUI のどこに誤りを出すかに変える。
export function parsePath(path: string): Location {
  const p = path.split('/').slice(1).map(unescape)
  if (p[0] === 'layers' && p.length >= 2 && /^\d+$/.test(p[1])) {
    const layer = +p[1]
    if (p[2] === 'keys' && p[3] !== undefined) return { kind: 'key', layer, id: p[3] }
    if (p[2] === 'touch' && p[3] === 'cells' && p[4] !== undefined) return { kind: 'cell', layer, id: p[4] }
    if (p[2] === 'touch') return { kind: 'grid', layer }
    if (p[2] === 'touch_portrait' && p[3] === 'cells' && p[4] !== undefined) return { kind: 'cell', layer, id: p[4], portrait: true }
    if (p[2] === 'touch_portrait') return { kind: 'grid', layer, portrait: true }
    if (p[2] === 'soft_keys' && p[3] !== undefined) return { kind: 'soft', layer, id: p[3] }
    return { kind: 'layer', layer }
  }
  if (p[0] === 'touch') return { kind: 'touch', field: p[1] }
  if (p[0] === 'home' || p[0] === 'status_bar') return { kind: 'home', field: p.slice(1).join('/') || p[0] }
  return { kind: 'other', path }
}

export function layerTitle(l: LayerConfig): string {
  return l.label || l.name
}
