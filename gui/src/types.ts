// 設定ファイルの構造（docs/config.md）。デーモンの get_config が返す JSON と同じ形。
// 割り当ては、GUI の中ではいつもオブジェクトの形（{ key: "B" }）で持つ。

export interface ActionSpec {
  key?: string
  layer_hold?: string
  layer_toggle?: string
  layer_oneshot?: string
  layer_to?: string
  label?: string
  span?: [number, number] // セルの大きさ [列数, 行数]。タッチのセルだけ
  background?: string // セルの背景画像の id（Brain の /var/lib/lefthand/images/<id>.565）。タッチのセルだけ
  // ウィジェット（タッチのセルだけ）。key と layer_* は省略でき、書けばタップしたときに働く
  widget?: WidgetKind
  format?: string // clock：時刻の行（Go の書式）
  date_format?: string // clock：日付の行。none で出さない
  tz?: string // clock：タイムゾーン（IANA の名前）
  id?: string // text：中身の名前（set_text の name）
  rows?: number // todo、calendar：1 ページの行数。省略すると高さで決める
  page_reset?: string // todo、calendar：触らなければ最初のページに戻るまでの時間（30s、2m など）。off で戻らない
  stale?: string // calendar：最終更新がこれより古ければ、古いと出す（3h など）
  calendars?: string[] // calendar：出すカレンダーの名前。省略するとすべて
}

export type WidgetKind = 'clock' | 'text' | 'todo' | 'calendar'

export interface SoftArea {
  x: [number, number]
  y: [number, number]
}

export interface TouchConfig {
  device?: string
  swap_xy?: boolean
  min_x: number
  max_x: number
  min_y: number
  max_y: number
  soft_areas?: Record<string, SoftArea>
  // 旧形式。読み込むときに base レイヤーへ移す
  cols?: number
  rows?: number
  cells?: Record<string, ActionSpec>
}

export interface GridConfig {
  cols?: number
  rows?: number
  cells?: Record<string, ActionSpec>
  background?: string // 格子全体に敷く壁紙の id。セルの background があれば、そちらを上に描く
}

// list_images の結果（image.go の ImageList）
export interface BrainImage {
  id: string
  name?: string
  w: number
  h: number
  bytes: number
  sha256?: string
  added?: string
  source?: string
  refs: string[] // 今の設定で使っている場所（JSON Pointer）
}

export interface ImageListResult {
  images: BrainImage[]
  total_bytes: number
  limit_bytes: number
  max_image_bytes: number
  max_side: number
  max_pixels: number
  free_bytes: number
  reserve_bytes: number
  chunk_bytes: number
  missing: string[]
}

export interface LayerConfig {
  name: string
  label?: string
  keys?: Record<string, ActionSpec>
  touch?: GridConfig
  soft_keys?: Record<string, ActionSpec>
}

export interface DisplayConfig {
  enabled?: boolean
  device?: string
  vt?: number
  rotate?: number
  press_style?: PressStyle // 押しているセルの見せ方。省略すると border
}

export type PressStyle = 'border' | 'fill'

export interface Config {
  hid_device?: string
  keyboard?: string
  keys?: Record<string, ActionSpec> // 旧形式
  touch?: TouchConfig
  layers: LayerConfig[]
  display?: DisplayConfig
}

// 検証の誤り。path は JSON Pointer（例：/layers/0/keys/KEY_Q）
export interface Problem {
  path: string
  message: string
}

// get_keymap の結果（keymap_pwsh2.go）
export interface PhysKey {
  id: string
  label: string
  code?: string
  symbol?: string
  row: number
  x: number
  w: number
  note?: string
}

export interface KeymapInfo {
  model: string
  keys: PhysKey[]
  max_rollover: number
  blocked: string[][]
  constraints: string[]
  screen: { w: number; h: number }
}

export interface EngineStatus {
  layer: string
  label: string
  mode: 'base' | 'latched' | 'temp'
  stack: { layer: string; kind: string }[]
  cols: number
  rows: number
}

export interface HelloResult {
  protocol: number
  daemon: string
  version: string
  max_line: number
  config_path: string
  commands: string[]
}

// Brain の時刻の状態（get_status の time、set_time の結果）
export interface TimeInfo {
  now: string
  timezone: string
  utc_offset_sec: number
  synced: boolean
  ntp_synced: boolean
  last_set?: string
  last_source?: string
}

export interface SetTimeResult extends TimeInfo {
  stepped: boolean
  offset_ms: number
}

// テキストのタイルの中身（get_text、set_text。text.go の TextEntry）
export type TextStyle = 'normal' | 'ok' | 'error' | 'warn'

export interface TextEntry {
  text: string
  style: TextStyle
  set_at: string
  expires_at?: string // これを過ぎたら薄く表示する
  source?: string
  expired?: boolean // 読んだときに期限が切れていたか
}

export interface GetTextResult {
  texts: Record<string, TextEntry>
  ids: string[] // 設定で使われている id
}

// Todo の項目（get_todo、todo_*、todo の通知。todo.go の TodoItem）
export interface TodoItem {
  id: string
  text: string
  done: boolean
  rev: number // 最後に変えたときの、一覧の rev
  created_at: string
  updated_at: string
  done_at?: string
  source?: string // 最後に変えた側（gui、brain-deck、brain）
}

// Todo の一覧。items は並べた順で、完了したものも混ざっている（画面には未完了のあとに完了を出す）
export interface TodoList {
  rev: number
  items: TodoItem[]
}

export interface TodoResult extends TodoList {
  item?: TodoItem
  shown?: boolean // 設定に Todo のセルがあるか（get_todo、todo_add）
  removed?: number // todo_clear_done
}

// カレンダーの予定（get_calendar。calendar.go の CalendarData）
export interface CalEvent {
  title: string
  start?: string // 時刻の決まった予定（RFC 3339）
  end?: string
  day?: string // 終日の予定（YYYY-MM-DD）
  end_day?: string // この日は含まない。省略すると day の次の日
  location?: string
}

export interface Calendar {
  name: string
  color: string // #rrggbb
  fetched_at?: string // 予定を取ってきた時刻（PC の時刻）
  error?: string // 最後の取得の失敗（前の予定を残している）
  events: CalEvent[]
}

export interface CalendarData {
  rev: number
  received_at?: string
  from?: string
  days?: number
  source?: string
  calendars: Calendar[]
  shown?: boolean // 設定にカレンダーのセルがあるか（get_calendar）
}

export interface ValidateResult {
  valid: boolean
  errors: Problem[]
  warnings: string[]
  config?: Config
}

export interface InputEvent {
  event: 'input'
  type: 'key' | 'touch'
  code?: string
  x?: number
  y?: number
  col?: number
  row?: number
  soft?: string
  layer: string
  suppressed?: boolean
}

export interface LayerEvent extends EngineStatus {
  event: 'layer'
}

// Brain で Todo が変わったとき（subscribe_data のあと）
export interface TodoEvent extends TodoList {
  event: 'todo'
}

export type Notification = InputEvent | LayerEvent | TodoEvent
