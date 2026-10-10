// デモ用の簡易デーモンと、WebSerial のモック。
// Brain がなくても GUI を試せるようにする（?demo を付けて開く）。テストでも使う。
// 検証は lefthand の一部だけをまねたもので、本物の検証は Brain に接続して行う。

import { ALL_KEYS, MODIFIERS } from './keys'
import keymapJSON from './keymap-pwsh2.json'
import { ACTION_FIELDS, GRAPH_FIELDS, MOUSE_LABELS, PAD_FIELDS, actionKind, actionTarget, hasStateId, isTapWidget, parseCellKey, spanOf } from './model'
import type { Transport } from './protocol'
import { TEXT_ID_PATTERN } from './textwidget'
import { parseDurationMs, presenceStates } from './gadgetwidget'
import type { ActionSpec, BatteryInfo, CalendarData, Config, GraphSeries, KeymapInfo, PresenceSpec, Problem, TextEntry, TodoItem, TodoList, WeatherData, WidgetsState } from './types'

const keymap = keymapJSON as KeymapInfo
const SOURCE_KEYS = new Set(keymap.keys.flatMap((k) => [k.code, k.symbol]).filter(Boolean) as string[])

export class FakeDaemon {
  config: Config
  path = '/etc/lefthand/config.yaml'
  stack: { layer: string; kind: string }[] = []
  subscribed = false
  mouse = true // USB にマウスがあるか（set_usb_mode で切り替える）
  terminal = false // 端末モード（set_terminal で切り替える）
  suppress = false
  received: any[] = [] // 受け取ったリクエスト（テスト用）
  failApply = false // true なら set_config の反映に失敗したことにする（テスト用）
  clockOffsetMs = 0 // Brain の時計の遅れ（set_time で 0 になる）
  timeSynced = false
  texts: Record<string, TextEntry> = {} // テキストのタイルの中身
  qr: Record<string, TextEntry> = {} // QR コードのウィジェットの中身
  widgets: WidgetsState = { stopwatch: {}, pomodoro: {}, counter: {}, presence: {}, presenter: {} } // ストップウォッチなどの状態
  battery: BatteryInfo | null = { percent: 87, voltage_mv: 4040, plugged: true, charging: true, source: 'demo', estimated: true }
  todo: TodoList = { rev: 0, items: [] } // Todo の一覧
  calendar: CalendarData = { rev: 0, calendars: [] } // カレンダーの予定（brain-deck calendar sync が送るもの）
  weather: WeatherData = { rev: 0, days: [] } // 天気予報（brain-deck weather sync が送るもの。デモにアイコンはない）
  graph: Record<string, GraphSeries> = {} // グラフの値（brain-deck graph が足すもの）
  // 背景画像（id → 名前と中身）。image_begin〜image_end で受け取る
  images = new Map<string, { name: string; w: number; h: number; data: Uint8Array; added: string }>()
  private upload: { token: string; id: string; name: string; w: number; h: number; bytes: number; parts: Uint8Array[]; got: number } | null = null
  quota = 16 << 20
  private nextTodo = 1
  dataSubscribed = false
  // 通知を送る先（FakeTransport が設定する）
  emit: (line: string) => void = () => {}

  constructor(config: Config) {
    this.config = structuredClone(config)
  }

  termInfo() {
    return this.terminal
      ? { active: true, state: 'on', transport: 'serial', status: 'PC のログイン画面', cols: 100, rows: 31 }
      : { active: false, state: 'off' }
  }

  status() {
    const top = this.stack.length ? this.stack[this.stack.length - 1].layer : this.config.layers[0].name
    const l = this.config.layers.find((x) => x.name === top) ?? this.config.layers[0]
    const g = l.touch?.cols ? l.touch : this.config.layers[0].touch
    const mode = !this.stack.length ? 'base' : ['layer_hold', 'layer_oneshot'].includes(this.stack[this.stack.length - 1].kind) ? 'temp' : 'latched'
    return { layer: l.name, label: l.label || l.name, mode, stack: this.stack, cols: g?.cols ?? 0, rows: g?.rows ?? 0,
      rotate: this.rotate, portrait: this.rotate === 90 || this.rotate === 270 }
  }

  rotate = 0 // 画面の向き（set_rotate）

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
        return ok({ protocol: 1, daemon: 'lefthand', version: 'demo', max_line: 262144, config_path: this.path,
          commands: ['hello', 'get_config', 'validate', 'set_config', 'get_keymap', 'get_status', 'subscribe_input', 'set_time', 'set_text', 'get_text',
            'get_todo', 'todo_add', 'todo_update', 'todo_delete', 'todo_move', 'todo_clear_done', 'subscribe_data',
            'set_calendar', 'get_calendar', 'set_weather', 'get_weather', 'get_weather_icon', 'list_images', 'image_begin', 'image_chunk', 'image_end', 'image_abort', 'get_image', 'prune_images', 'set_usb_mode', 'set_terminal', 'set_rotate',
            'set_qr', 'get_qr', 'get_widgets', 'set_presence', 'add_graph', 'clear_graph', 'get_graph'] })
      case 'get_config':
        return ok({ config: this.config, path: this.path })
      case 'get_keymap':
        return ok(keymap)
      case 'get_status':
        return ok({ status: this.status(), uptime_sec: 1, subscribed: this.subscribed, suppressing: this.suppress, time: this.timeInfo(), hid: { mouse: this.mouse, switching: false, usb_state: 'configured' },
          terminal: this.termInfo(), battery: this.battery, backlight: { enabled: false } })
      case 'set_rotate': {
        const r = req.mode === 'next' ? (this.rotate + 90) % 360 : Number(req.mode)
        if (![0, 90, 180, 270].includes(r)) return err('bad_request', '"mode" must be next, 0, 90, 180 or 270')
        this.rotate = r
        return ok({ status: this.status() })
      }
      case 'set_terminal': {
        const want = req.mode === 'toggle' ? !this.terminal : req.mode === 'on' ? true : req.mode === 'off' ? false : null
        if (want === null) return err('bad_request', 'mode must be "on", "off" or "toggle"')
        this.terminal = want
        return ok({ terminal: want, info: this.termInfo() })
      }
      case 'set_usb_mode': {
        const want = req.mode === 'toggle' ? !this.mouse : req.mode === 'mouse' ? true : req.mode === 'keyboard' ? false : null
        if (want === null) return err('bad_request', 'mode must be keyboard, mouse or toggle')
        const switching = want !== this.mouse
        this.mouse = want
        return ok({ mouse: want, switching })
      }
      case 'set_time': {
        if (typeof req.unix_ms !== 'number') return err('bad_request', '"unix_ms" is required')
        const offset = req.unix_ms - (Date.now() - this.clockOffsetMs)
        this.clockOffsetMs = 0
        this.timeSynced = true
        return ok({ stepped: Math.abs(offset) >= 500, offset_ms: offset, ...this.timeInfo() })
      }
      case 'get_text': {
        const ids = [...new Set(this.config.layers.flatMap((l) => Object.values(l.touch?.cells ?? {})).filter((a) => a.widget === 'text' && a.id).map((a) => a.id!))].sort()
        return ok({ texts: this.texts, ids })
      }
      case 'set_text':
        if (!TEXT_ID_PATTERN.test(req.name ?? '')) return err('bad_request', 'bad name')
        if (req.clear) delete this.texts[req.name]
        else this.texts[req.name] = { text: String(req.text), style: req.style ?? 'normal', set_at: new Date().toISOString(),
          ...(req.ttl_sec ? { expires_at: new Date(Date.now() + req.ttl_sec * 1000).toISOString() } : {}) }
        return ok({ name: req.name, cleared: !!req.clear, shown: true })
      case 'get_qr':
        return ok({ texts: this.qr, ids: this.widgetIds('qr') })
      case 'set_qr':
        if (!TEXT_ID_PATTERN.test(req.name ?? '')) return err('bad_request', 'bad name')
        if (req.clear) delete this.qr[req.name]
        else this.qr[req.name] = { text: String(req.text), style: 'normal', set_at: new Date().toISOString(),
          ...(req.ttl_sec ? { expires_at: new Date(Date.now() + req.ttl_sec * 1000).toISOString() } : {}) }
        return ok({ name: req.name, cleared: !!req.clear, shown: this.widgetIds('qr').includes(req.name) })
      case 'get_widgets': {
        const ids: Record<string, string[]> = {}
        for (const k of ['stopwatch', 'pomodoro', 'counter', 'presence', 'qr', 'presenter']) ids[k] = this.widgetIds(k)
        const presence_states: Record<string, PresenceSpec[]> = {}
        for (const pid of ids.presence) presence_states[pid] = this.presenceStates(pid)
        return ok({ widgets: this.widgets, ids, presence_states, battery: this.battery, now: new Date().toISOString() })
      }
      case 'set_presence': {
        const name = req.name || 'presence'
        const states = this.presenceStates(name)
        if (!states.length) return err('not_found', `the config has no presence cell with id "${name}"`)
        let state = req.state as string
        if (state) {
          if (!states.some((x) => x.name === state)) return err('bad_request', `unknown state "${state}"`)
        } else {
          const cur = states.findIndex((x) => x.name === this.widgets.presence?.[name]?.state)
          const i = cur < 0 ? (req.step > 0 ? 0 : states.length - 1) : (cur + (req.step > 0 ? 1 : -1) + states.length) % states.length
          state = states[i].name
        }
        ;(this.widgets.presence ??= {})[name] = { state, updated_at: new Date().toISOString(), source: req.source }
        if (this.dataSubscribed) setTimeout(() => this.emit(JSON.stringify({ event: 'widgets', ...this.widgets })), 0)
        return ok({ name, state })
      }
      case 'get_todo':
      case 'todo_add':
      case 'todo_update':
      case 'todo_delete':
      case 'todo_move':
      case 'todo_clear_done': {
        const r = this.todoCmd(req)
        if (typeof r === 'string') return err(r, r)
        return ok({ ...r, rev: this.todo.rev, items: this.todo.items })
      }
      case 'get_calendar':
        return ok({ ...this.calendar, shown: this.config.layers.some((l) => Object.values(l.touch?.cells ?? {}).some((a) => a.widget === 'calendar' || a.widget === 'month')) })
      case 'set_calendar':
        if (!Array.isArray(req.calendars)) return err('bad_request', '"calendars" (an array) is required')
        this.calendar = { rev: this.calendar.rev + 1, received_at: new Date().toISOString(), from: req.from, days: req.days, calendars: req.calendars }
        return ok({ rev: this.calendar.rev, calendars: req.calendars.map((c: any) => ({ name: c.name, events: c.events?.length ?? 0 })) })
      case 'get_weather':
        return ok({ ...this.weather, shown: this.config.layers.some((l) => Object.values(l.touch?.cells ?? {}).some((a) => a.widget === 'weather')),
          icon_boxes: [], icons: [], icons_bytes: 0, icons_missing: [] })
      case 'set_weather': {
        const w = req.weather
        if (!w || typeof w !== 'object') return err('bad_request', '"weather" (an object) is required')
        if (!w.days?.length) {
          if (!w.error) return err('bad_request', 'bad weather: "days" (the forecast) or "error" (why it could not be fetched) is required')
          this.weather = { ...this.weather, rev: this.weather.rev + 1, error: w.error, error_at: new Date().toISOString() }
          return ok({ rev: this.weather.rev, kept: this.weather.days.length > 0, icons_missing: [] })
        }
        this.weather = { rev: this.weather.rev + 1, received_at: new Date().toISOString(), area: w.area, office: w.office, public_time: w.public_time,
          fetched_at: w.fetched_at ?? new Date().toISOString(), days: w.days }
        return ok({ rev: this.weather.rev, kept: false, icons_missing: [] })
      }
      case 'get_graph': {
        const ids = [...new Set(this.config.layers.flatMap((l) => Object.values(l.touch?.cells ?? {}).filter((a) => a.widget === 'graph').map((a) => a.id ?? '')))].sort()
        return ok({ series: this.graph, ids, now: new Date().toISOString() })
      }
      case 'add_graph': {
        if (!Array.isArray(req.points) || !req.points.length) return err('bad_request', '"points" (an array of {"name": <id>, "value": <number>}) is required')
        for (const p of req.points) {
          if (!TEXT_ID_PATTERN.test(p?.name ?? '') || typeof p.value !== 'number' || !Number.isFinite(p.value)) return err('bad_request', 'bad graph value')
        }
        for (const p of req.points) {
          const pts = [...(this.graph[p.name]?.points ?? []), { t: new Date().toISOString(), v: p.value }].slice(-120)
          this.graph[p.name] = { points: pts, count: pts.length, last: pts[pts.length - 1], source: req.source }
        }
        return ok({ added: req.points.length, unknown: [] })
      }
      case 'clear_graph': {
        const had = Object.hasOwn(this.graph, req.name)
        delete this.graph[req.name]
        return ok({ name: req.name, cleared: had })
      }
      case 'get_weather_icon':
        return err('not_found', `weather icon ${req.name} is not on this Brain`)
      case 'subscribe_data':
        this.dataSubscribed = req.enable !== false
        return ok({ subscribed: this.dataSubscribed, events: ['todo', 'widgets'] })
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
      case 'list_images':
      case 'image_begin':
      case 'image_chunk':
      case 'image_end':
      case 'image_abort':
      case 'get_image':
      case 'prune_images': {
        const r = this.imageCmd(req)
        return Array.isArray(r) ? err(r[0], r[1]) : ok(r)
      }
      case 'subscribe_input':
        this.subscribed = req.enable !== false
        this.suppress = this.subscribed && !!req.suppress
        return ok({ subscribed: this.subscribed, suppress: this.suppress, lease_sec: 30 })
      default:
        return err('unknown_command', `unknown command ${JSON.stringify(req.cmd)}`)
    }
  }

  // imageCmd は image.go の ImageStore をまねる（SHA-256 は確かめず、送る側が言った値を信じる）。誤りなら [code, message]。
  imageCmd(req: any): Record<string, unknown> | [string, string] {
    const refs = (): Map<string, string[]> => {
      const m = new Map<string, string[]>()
      this.config.layers.forEach((l, i) => {
        if (l.touch?.background) m.set(l.touch.background, [...(m.get(l.touch.background) ?? []), `/layers/${i}/touch/background`])
        for (const [k, a] of Object.entries(l.touch?.cells ?? {}))
          if (a.background) m.set(a.background, [...(m.get(a.background) ?? []), `/layers/${i}/touch/cells/${k}`])
      })
      return m
    }
    const total = () => [...this.images.values()].reduce((a, x) => a + x.data.length, 0)
    switch (req.cmd) {
      case 'list_images': {
        const r = refs()
        return { images: [...this.images].map(([id, x]) => ({ id, name: x.name, w: x.w, h: x.h, bytes: x.data.length, added: x.added, refs: r.get(id) ?? [] })),
          total_bytes: total(), limit_bytes: this.quota, max_image_bytes: 768008, max_side: 800, max_pixels: 384000,
          free_bytes: 140 << 20, reserve_bytes: 64 << 20, chunk_bytes: 98304, missing: [...r.keys()].filter((id) => !this.images.has(id)) }
      }
      case 'image_begin': {
        if (!/^[0-9a-f]{64}$/.test(req.sha256 ?? '')) return ['bad_request', '"sha256" must be the 64-digit hex SHA-256 of the file']
        if (req.bytes !== 8 + req.w * req.h * 2) return ['bad_request', '"bytes" does not match w and h']
        const id = req.sha256.slice(0, 16)
        this.upload = null
        if (this.images.has(id)) return { id, exists: true }
        if (total() + req.bytes > this.quota) return ['quota_exceeded', `the images would take ${total() + req.bytes} bytes, over the limit of ${this.quota}`]
        this.upload = { token: `u${Date.now()}`, id, name: req.name ?? '', w: req.w, h: req.h, bytes: req.bytes, parts: [], got: 0 }
        return { id, exists: false, upload: this.upload.token, chunk_bytes: 98304 }
      }
      case 'image_chunk': {
        const u = this.upload
        if (!u || u.token !== req.upload) return ['bad_request', 'upload is not in progress']
        if (req.offset !== u.got) return ['bad_request', `offset ${req.offset}: expected ${u.got}`]
        const d = Uint8Array.from(atob(req.data), (c) => c.charCodeAt(0))
        u.parts.push(d)
        u.got += d.length
        return { received: u.got }
      }
      case 'image_end': {
        const u = this.upload
        this.upload = null
        if (!u || u.token !== req.upload) return ['bad_request', 'upload is not in progress']
        if (u.got !== u.bytes) return ['bad_request', `received ${u.got} of ${u.bytes} bytes`]
        const data = new Uint8Array(u.got)
        let o = 0
        for (const p of u.parts) {
          data.set(p, o)
          o += p.length
        }
        this.images.set(u.id, { name: u.name, w: u.w, h: u.h, data, added: new Date().toISOString() })
        return { id: u.id }
      }
      case 'image_abort': {
        const had = !!this.upload && (!req.upload || this.upload.token === req.upload)
        if (had) this.upload = null
        return { aborted: had }
      }
      case 'get_image': {
        const x = this.images.get(req.image)
        if (!x) return ['not_found', `image ${req.image} is not on this Brain`]
        const off = req.offset ?? 0
        const part = x.data.subarray(off, Math.min(off + 98304, x.data.length))
        let bin = ''
        for (const b of part) bin += String.fromCharCode(b)
        return { image: req.image, offset: off, bytes: x.data.length, data: btoa(bin) }
      }
      case 'prune_images': {
        const keep = new Set([...refs().keys(), ...(req.keep ?? [])])
        const removed = [...this.images.keys()].filter((id) => !keep.has(id)).sort()
        const freed = removed.reduce((a, id) => a + this.images.get(id)!.data.length, 0)
        if (!req.dry_run) for (const id of removed) this.images.delete(id)
        return { removed, freed_bytes: freed, dry_run: !!req.dry_run }
      }
    }
    return ['unknown_command', String(req.cmd)]
  }

  timeInfo() {
    return { now: new Date(Date.now() - this.clockOffsetMs).toISOString(), timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      utc_offset_sec: -new Date().getTimezoneOffset() * 60, synced: this.timeSynced, ntp_synced: false }
  }

  // todoCmd は todo.go の TodoService をまねる。誤りならエラーの種類を返す。
  todoCmd(req: any): Record<string, unknown> | string {
    const items = [...this.todo.items]
    const now = new Date().toISOString()
    const find = () => {
      const i = items.findIndex((x) => x.id === req.item)
      if (i < 0) return 'not_found'
      if (typeof req.rev === 'number' && items[i].rev !== req.rev) return 'conflict'
      return i
    }
    const rev = this.todo.rev + 1
    const extra: Record<string, unknown> = {}
    switch (req.cmd) {
      case 'get_todo':
        return { shown: true }
      case 'todo_add': {
        const text = String(req.text ?? '').replace(/\s+/g, ' ').trim()
        if (!text) return 'bad_request'
        const it: TodoItem = { id: `t${this.nextTodo++}`, text, done: false, rev, created_at: now, updated_at: now, source: req.source }
        const at = typeof req.index === 'number' ? req.index : items.length
        items.splice(at, 0, it)
        extra.item = it
        break
      }
      case 'todo_update': {
        const i = find()
        if (typeof i === 'string') return i
        const it = { ...items[i], rev, updated_at: now, source: req.source }
        if (typeof req.text === 'string') it.text = req.text
        if (typeof req.done === 'boolean') {
          it.done = req.done
          if (req.done) it.done_at = now
          else delete it.done_at
        }
        items[i] = it
        extra.item = it
        break
      }
      case 'todo_delete': {
        const i = find()
        if (typeof i === 'string') return i
        items.splice(i, 1)
        break
      }
      case 'todo_move': {
        const i = find()
        if (typeof i === 'string') return i
        if (typeof req.index !== 'number' || req.index < 0 || req.index >= items.length) return 'bad_request'
        const [it] = items.splice(i, 1)
        items.splice(req.index, 0, it)
        break
      }
      case 'todo_clear_done': {
        const kept = items.filter((x) => !x.done)
        extra.removed = items.length - kept.length
        if (!extra.removed) return extra
        items.splice(0, items.length, ...kept)
        break
      }
    }
    this.todo = { rev, items }
    this.emitTodo()
    return extra
  }

  // widgetIds は、設定で kind のウィジェットに使われている id（gadgets.go の gadgetIDs）。
  private widgetIds(kind: string): string[] {
    return [...new Set(this.config.layers.flatMap((l) => Object.values(l.touch?.cells ?? {})).filter((a) => a.widget === kind).map((a) => a.id || kind))].sort()
  }

  private presenceStates(id: string): PresenceSpec[] {
    const out: PresenceSpec[] = []
    for (const l of this.config.layers) {
      for (const k of Object.keys(l.touch?.cells ?? {}).sort()) {
        const a = l.touch!.cells![k]
        if (a.widget !== 'presence' || (a.id || 'presence') !== id) continue
        presenceStates(a.states).forEach((d) => {
          if (!out.some((x) => x.name === d.name)) out.push({ name: d.name, color: '#' + d.color.map((v) => v.toString(16).padStart(2, '0')).join('') })
        })
      }
    }
    return out
  }

  private emitTodo(): void {
    if (this.dataSubscribed) setTimeout(() => this.emit(JSON.stringify({ event: 'todo', ...this.todo })), 0)
  }

  // toggleTodo は、Brain で長押しして完了を切り替えたことにする。
  toggleTodo(id: string): void {
    const it = this.todo.items.find((x) => x.id === id)
    if (it) this.todoCmd({ cmd: 'todo_update', item: id, done: !it.done, source: 'brain' })
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
  const action = (path: string, where: string, a: ActionSpec, cell = false) => {
    const n = ACTION_FIELDS.filter((k) => a[k]).length
    if (n > 1 || (n === 0 && !a.widget))
      return out.push({ path, message: `${where}: write exactly one of key, layer_hold, layer_toggle, layer_oneshot, layer_to, mouse, usb_mode, terminal (a widget cell may omit them)` })
    if (a.usb_mode !== undefined && !['keyboard', 'mouse', 'toggle'].includes(a.usb_mode))
      out.push({ path, message: `${where}: usb_mode must be keyboard, mouse or toggle` })
    if (a.terminal !== undefined && !['on', 'off', 'toggle'].includes(a.terminal))
      out.push({ path, message: `${where}: terminal must be on, off or toggle` })
    if (a.mouse !== undefined && !Object.hasOwn(MOUSE_LABELS, a.mouse))
      out.push({ path, message: `${where}: unknown mouse action "${a.mouse}" (${Object.keys(MOUSE_LABELS).join(', ')})` })
    if (a.widget === 'trackpad' && n > 0)
      out.push({ path, message: `${where}: widget: trackpad handles taps itself (tap to click); remove key, layer_* and mouse (put mouse buttons in other cells)` })
    if (a.widget !== 'trackpad' && PAD_FIELDS.some((f) => a[f] !== undefined))
      out.push({ path, message: `${where}: speed, accel, scroll_*, settle_ms, smooth, deadzone, min_pressure, tap_*, drag_ms and long_press are for widget: trackpad` })
    if (!cell && (a.widget || a.span || a.background)) return out.push({ path, message: `${where}: widget, span and background can be used only in touch cells` })
    if (a.background !== undefined && !/^[0-9a-f]{16}$/.test(a.background))
      out.push({ path, message: `${where}: background must be the 16-digit lowercase hex id of an image (as the settings GUI writes it), got "${a.background}"` })
    if (a.widget && !['clock', 'text', 'todo', 'calendar', 'trackpad', 'battery', 'stopwatch', 'pomodoro', 'counter', 'qr', 'presence', 'month', 'weather', 'graph', 'presenter', 'calculator'].includes(a.widget))
      out.push({ path, message: `${where}: unknown widget "${a.widget}" (clock, text, todo, calendar, trackpad, battery, stopwatch, pomodoro, counter, qr, presence, month)` })
    if (isTapWidget(a.widget) && n > 0)
      out.push({ path, message: `${where}: widget: ${a.widget} handles taps itself; remove key, layer_*, mouse, usb_mode and terminal` })
    if ((a.work !== undefined || a.break !== undefined) && a.widget !== 'pomodoro') out.push({ path, message: `${where}: work and break are for widget: pomodoro` })
    for (const f of ['work', 'break'] as const) {
      const d = a[f] !== undefined ? parseDurationMs(a[f]!) : 60000
      if (d === null || d < 60000 || d > 4 * 3600000) out.push({ path, message: `${where}: ${f} must be a whole number of seconds from 1m0s to 4h0m0s (such as 25m or 5m)` })
    }
    if ((a.duration !== undefined || a.colors !== undefined) && a.widget !== 'presenter') out.push({ path, message: `${where}: duration and colors are for widget: presenter` })
    if (a.widget === 'presenter' && a.duration !== undefined) {
      const d = parseDurationMs(a.duration)
      if (d === null || d < 10_000 || d > 86400_000 || d % 1000) out.push({ path, message: `${where}: duration must be a whole number of seconds from 10s to 24h0m0s (such as 20m or 5m)` })
    }
    for (const [i, c] of (a.colors ?? []).entries()) {
      if (!/^#[0-9a-fA-F]{6}$/.test(c.color ?? '')) out.push({ path, message: `${where}: colors[${i}]: color must be #rrggbb, got "${c.color}"` })
      if (!(parseDurationMs(c.remaining ?? '') ?? 0)) out.push({ path, message: `${where}: colors[${i}]: remaining must be a whole number of seconds such as 5m or 30s, got "${c.remaining}"` })
    }
    if (a.states !== undefined && a.widget !== 'presence') out.push({ path, message: `${where}: states is for widget: presence` })
    if (a.widget === 'todo' && n > 0) out.push({ path, message: `${where}: widget: todo handles taps itself (long press an item to check it, ▲▼ to turn pages); remove key and layer_*` })
    if (a.widget === 'calendar' && n > 0) out.push({ path, message: `${where}: widget: calendar handles taps itself (▲▼ to turn pages); remove key and layer_*` })
    if (a.widget === 'calculator' && n > 0) out.push({ path, message: `${where}: widget: calculator handles taps itself (tap the buttons); remove key and layer_*` })
    if (a.prefix !== undefined && a.widget !== 'calculator') out.push({ path, message: `${where}: prefix is for widget: calculator` })
    if (a.widget === 'month' && n > 0) out.push({ path, message: `${where}: widget: month handles taps itself (▲▼ to change the month, tap a day to select it); remove key and layer_*` })
    const paged = a.widget === 'todo' || a.widget === 'calendar'
    if (a.rows !== undefined && !paged) out.push({ path, message: `${where}: rows is for widget: todo and calendar` })
    if (a.page_reset !== undefined && !paged && a.widget !== 'month') out.push({ path, message: `${where}: page_reset is for widget: todo, calendar and month` })
    if (a.stale !== undefined && a.widget !== 'calendar' && a.widget !== 'weather' && a.widget !== 'graph') out.push({ path, message: `${where}: stale is for widget: calendar, weather and graph` })
    if (a.widget !== 'graph' && GRAPH_FIELDS.some((f) => a[f] !== undefined)) out.push({ path, message: `${where}: unit, min, max, chart, points and decimals are for widget: graph` })
    if (a.widget === 'graph') {
      if (a.min !== undefined && a.max !== undefined && a.min >= a.max) out.push({ path, message: `${where}: min (${a.min}) must be less than max (${a.max})` })
      if (a.points !== undefined && !(Number.isInteger(a.points) && a.points >= 2 && a.points <= 120)) out.push({ path, message: `${where}: points must be 2..120 (the number of values to draw), got ${a.points}` })
      if (a.decimals !== undefined && !(Number.isInteger(a.decimals) && a.decimals >= 0 && a.decimals <= 6)) out.push({ path, message: `${where}: decimals must be 0..6 (omit it to choose by the size of the value), got ${a.decimals}` })
      if (a.chart !== undefined && a.chart !== 'line' && a.chart !== 'bar') out.push({ path, message: `${where}: chart must be line or bar, got "${a.chart}"` })
    }
    if (a.calendars !== undefined && a.widget !== 'calendar' && a.widget !== 'month') out.push({ path, message: `${where}: calendars is for widget: calendar and month` })
    if (a.follow !== undefined && a.widget !== 'calendar') out.push({ path, message: `${where}: follow is for widget: calendar (the id of a widget: month cell in the same layer)` })
    if (a.week_start !== undefined && a.widget !== 'month') out.push({ path, message: `${where}: week_start is for widget: month` })
    if (a.week_start !== undefined && a.week_start !== 'sunday' && a.week_start !== 'monday') out.push({ path, message: `${where}: week_start must be sunday or monday, got "${a.week_start}"` })
    if (a.rows !== undefined && !(Number.isInteger(a.rows) && a.rows >= 1 && a.rows <= 20)) out.push({ path, message: `${where}: rows must be 1..20 (omit it to fit the cell height)` })
    if ((a.widget === 'text' || a.widget === 'qr' || a.widget === 'graph') && !TEXT_ID_PATTERN.test(a.id ?? ''))
      out.push({ path, message: `${where}: widget: ${a.widget} needs id (1-32 characters of A-Z a-z 0-9 _ . -), got "${a.id ?? ''}"` })
    if (!hasStateId(a.widget) && a.id) out.push({ path, message: `${where}: id is for widget: text, qr, graph, stopwatch, pomodoro, counter, presence, presenter, calculator and month` })
    if (!a.widget && (a.format || a.date_format || a.tz)) out.push({ path, message: `${where}: format, date_format and tz need widget: clock` })
    if (a.tz) {
      try {
        new Intl.DateTimeFormat('en-US', { timeZone: a.tz })
      } catch {
        out.push({ path, message: `${where}: unknown tz "${a.tz}" (use an IANA name such as Asia/Tokyo)` })
      }
    }
    const k = actionKind(a)
    if (k === 'key') {
      for (const p of a.key!.split('+')) {
        const u = p.trim().toUpperCase()
        if (!ALL_KEYS.has(u) && !(MODIFIERS as readonly string[]).includes(u))
          out.push({ path, message: `${where}: unknown key "${u}" in "${a.key}"` })
      }
    } else if (k === 'rotate') {
      if (!['next', '0', '90', '180', '270'].includes(a.rotate!)) out.push({ path, message: `${where}: rotate must be next, 0, 90, 180 or 270` })
    } else if (k !== 'none' && k !== 'widget' && k !== 'mouse' && k !== 'usb_mode' && k !== 'terminal') {
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
        const [w, h] = spanOf(a)
        if (!p || p[0] >= cols || p[1] >= rows) out.push({ path: `/layers/${i}/touch/cells/${k}`, message: `${where}: out of the ${cols}x${rows} grid` })
        else if (p[0] + w > cols || p[1] + h > rows)
          out.push({ path: `/layers/${i}/touch/cells/${k}`, message: `${where}: span ${w}x${h} goes out of the ${cols}x${rows} grid` })
        else action(`/layers/${i}/touch/cells/${k}`, where, a, true)
      }
      // follow は、同じレイヤーの月の表示の id を指す
      const months = Object.values(l.touch.cells ?? {}).filter((a) => a.widget === 'month').map((a) => a.id || 'month')
      for (const [k, a] of Object.entries(l.touch.cells ?? {})) {
        if (a.widget === 'calendar' && a.follow && !months.includes(a.follow))
          out.push({ path: `/layers/${i}/touch/cells/${k}`, message: `layer "${l.name}" touch cell "${k}": follow: no widget: month with id "${a.follow}" in this layer` })
      }
    }
    if (l.touch_portrait) {
      const p0 = l.touch_portrait
      if (!l.touch) out.push({ path: `/layers/${i}/touch_portrait`, message: `layer "${l.name}": touch_portrait needs touch (the landscape grid) in the same layer` })
      const cols = p0.cols ?? 0
      const rows = p0.rows ?? 0
      for (const [k, a] of Object.entries(p0.cells ?? {})) {
        const p = parseCellKey(k)
        const where = `layer "${l.name}" touch_portrait cell "${k}"`
        if (cols && (!p || p[0] >= cols || p[1] >= rows)) out.push({ path: `/layers/${i}/touch_portrait/cells/${k}`, message: `${where}: out of the ${cols}x${rows} grid` })
        else action(`/layers/${i}/touch_portrait/cells/${k}`, where, a, true)
      }
    }
    for (const [n, a] of Object.entries(l.soft_keys ?? {})) {
      const where = `layer "${l.name}" soft key "${n}"`
      if (!cfg.touch?.soft_areas?.[n]) out.push({ path: `/layers/${i}/soft_keys/${n}`, message: `${where}: not defined in touch.soft_areas` })
      else action(`/layers/${i}/soft_keys/${n}`, where, a)
    }
  })
  if (cfg.home) {
    if (names.has('home')) out.push({ path: '/home', message: 'home: a layer named "home" is reserved for the home screen; rename that layer' })
    if (!cfg.home.apps?.length) out.push({ path: '/home/apps', message: 'home.apps must have 1..64 apps' })
    cfg.home.apps?.forEach((a, i) => {
      if (!a.name) out.push({ path: `/home/apps/${i}/name`, message: `home.apps[${i}]: name must be 1..16 characters` })
      if (!names.has(a.layer)) out.push({ path: `/home/apps/${i}/layer`, message: `home.apps[${i}] "${a.name}": unknown layer "${a.layer}"` })
    })
    if (!cfg.touch?.soft_areas?.[cfg.home.soft_key ?? 'home'])
      out.push({ path: '/home/soft_key', message: `home: soft key "${cfg.home.soft_key ?? 'home'}" (the way back to the home screen) is not defined in touch.soft_areas` })
  }
  for (const [i, it] of (cfg.status_bar?.items ?? []).entries())
    if (!['time', 'battery', 'pc', 'mouse', 'sync'].includes(it)) out.push({ path: `/status_bar/items/${i}`, message: `status_bar.items must be distinct names from time, battery, pc, mouse and sync, got "${it}"` })
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
// fakeSerial は模擬の navigator.serial。Brain と同じく、ポートは 2 つ（設定用の port と、コンソール用の consolePort）。
// requestPort（ポートの一覧で選ぶ）は、choose が返すポートを選んだことにする（既定は設定用）。
export function fakeSerial(choose?: () => 'settings' | 'console'): { serial: Serial; port: SerialPort; consolePort: SerialPort } {
  const mk = () => ({ getInfo: () => ({ usbVendorId: 0x1d6b, usbProductId: 0x0104 }) }) as unknown as SerialPort
  const port = mk()
  const consolePort = mk()
  const granted: SerialPort[] = []
  const serial = {
    getPorts: async () => [...granted],
    requestPort: async () => {
      const p = choose?.() === 'console' ? consolePort : port
      if (!granted.includes(p)) granted.push(p)
      return p
    },
    addEventListener: () => {},
    removeEventListener: () => {},
  } as unknown as Serial
  return { serial, port, consolePort }
}

// FakeConsole はコンソール用のポート（getty とシェル）のまね。デモで、コンソールのタブを試すためのもの。
// ログイン画面、パスワード（何でも通す）、いくつかのコマンドの出力（日本語を含む）だけを返す。
export class FakeConsole implements Transport {
  onData: (chunk: Uint8Array) => void = () => {}
  onClose: (reason: string) => void = () => {}
  private enc = new TextEncoder()
  private dec = new TextDecoder()
  private stage: 'login' | 'password' | 'shell' = 'login'
  private line = ''
  private rows = 24
  private cols = 80
  private closed = false

  constructor() {
    setTimeout(() => this.out('\r\nDebian GNU/Linux 13 brain ttyGS0（デモ）\r\n\r\nbrain login: '), 50)
  }

  private out(s: string): void {
    if (!this.closed) this.onData(this.enc.encode(s))
  }

  private prompt(): string {
    return '\x1b[1;32muser@brain\x1b[0m:\x1b[1;34m~\x1b[0m$ '
  }

  async send(bytes: Uint8Array): Promise<void> {
    if (this.closed) throw new Error('closed')
    for (const ch of this.dec.decode(bytes, { stream: true })) {
      if (ch === '\r') {
        this.enter(this.line)
        this.line = ''
      } else if (ch === '\x7f') {
        if (this.line) {
          this.line = this.line.slice(0, -1)
          if (this.stage !== 'password') this.out('\b \b')
        }
      } else if (ch === '\x03') {
        this.line = ''
        this.out('^C\r\n' + (this.stage === 'shell' ? this.prompt() : 'brain login: '))
        if (this.stage === 'password') this.stage = 'login'
      } else if (ch >= ' ') {
        this.line += ch
        if (this.stage !== 'password') this.out(ch)
      }
    }
  }

  private enter(cmd: string): void {
    if (this.stage === 'login') {
      if (!cmd) return this.out('\r\nbrain login: ')
      this.stage = 'password'
      return this.out('\r\nPassword: ')
    }
    if (this.stage === 'password') {
      this.stage = 'shell'
      return this.out('\r\nLinux brain 6.1.0（デモ）\r\n' + this.prompt())
    }
    const [name, ...args] = cmd.trim().split(/\s+/)
    let r = ''
    if (!name) r = ''
    else if (name === 'ls') r = 'lefthand  メモ.txt  写真  設定のバックアップ.yaml\r\n'
    else if (name === 'cat') r = '牛乳を買う\r\n歯医者の予約（金曜 18:00）\r\n'
    else if (name === 'stty' && args[0] === 'size') r = `${this.rows} ${this.cols}\r\n`
    else if (name === 'stty' && args[0] === 'rows') {
      this.rows = Number(args[1]) || this.rows
      this.cols = Number(args[3]) || this.cols
    } else if (name === 'exit' || name === 'logout') {
      this.stage = 'login'
      return this.out('\r\n\r\nDebian GNU/Linux 13 brain ttyGS0（デモ）\r\n\r\nbrain login: ')
    } else r = `${name}: デモでは ls、cat、stty size、exit だけが使えます\r\n`
    this.out('\r\n' + r + this.prompt())
  }

  async close(): Promise<void> {
    if (this.closed) return
    this.closed = true
    this.onClose('closed by user')
  }
}

// demoCalendar は、デモで見せる予定（今日の今の前後と、明日）。brain-deck calendar sync が送るものと同じ形。
export function demoCalendar(now = new Date()): CalendarData {
  const at = (dayOff: number, h: number, m = 0) => new Date(now.getFullYear(), now.getMonth(), now.getDate() + dayOff, h, m).toISOString()
  const ev = (title: string, d: number, h0: number, m0: number, h1: number, m1: number) => ({ title, start: at(d, h0, m0), end: at(d, h1, m1) })
  const hr = now.getHours()
  const day = (off: number) => {
    const d = new Date(now.getFullYear(), now.getMonth(), now.getDate() + off)
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
  }
  return { rev: 1, received_at: now.toISOString(), from: day(0), days: 7, source: 'brain-deck', calendars: [
    { name: '仕事', color: '#4f9dff', fetched_at: new Date(now.getTime() - 5 * 60000).toISOString(), events: [
      ev('朝会', 0, Math.max(hr - 2, 0), 0, Math.max(hr - 2, 0), 30), ev('設計レビュー', 0, hr, 0, Math.min(hr + 1, 23), 0),
      ev('1on1', 0, Math.min(hr + 2, 23), 0, Math.min(hr + 2, 23), 30), ev('定例', 1, 10, 0, 11, 0)] },
    { name: '家', color: '#50d880', fetched_at: new Date(now.getTime() - 5 * 60000).toISOString(), events: [
      { title: '燃えないごみ', day: day(1) }, ev('ジム', 0, Math.min(hr + 4, 23), 0, Math.min(hr + 5, 23), 30)] },
  ] }
}

// demoGraph は、デモで見せるグラフの値（10 秒ごとに 60 個）。brain-deck graph が足すものと同じ形。
export function demoGraph(now = new Date()): Record<string, GraphSeries> {
  const series = (f: (i: number) => number) => {
    const points = Array.from({ length: 60 }, (_, i) => ({ t: new Date(now.getTime() - (59 - i) * 10_000).toISOString(), v: Math.round(f(i) * 10) / 10 }))
    return { points, count: points.length, last: points[points.length - 1], source: 'brain-deck' }
  }
  return { cpu: series((i) => 30 + 25 * Math.sin(i / 6) + (i % 7) * 2.5), mem: series((i) => 55 + i * 0.4 + (i % 5)), temp: series((i) => 48 + 6 * Math.sin(i / 15)) }
}
