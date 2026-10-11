// 設定ファイルの構造（docs/config.md）。デーモンの get_config が返す JSON と同じ形。
// 割り当ては、GUI の中ではいつもオブジェクトの形（{ key: "B" }）で持つ。

export interface ActionSpec {
  key?: string
  layer_hold?: string
  layer_toggle?: string
  layer_oneshot?: string
  layer_to?: string
  mouse?: MouseAction // マウスの操作。押しているあいだボタンを押す
  media?: MediaAction // メディアキー。キーボードとマウスの形（usb_mode: mouse）のときだけ送れる
  usb_mode?: UsbMode // USB の形の切り替え（USB を付け直すので 2〜3 秒切れる）
  terminal?: TermMode // 端末モード（Brain の画面とキーボードで PC にログインする）の切り替え
  rotate?: string // 画面の向き（next で 90 度ずつ、0、90、180、270。rotate.go）
  input?: InputKind // Brain のキーボードで文字を入れる欄を開く（todo で Todo を足す、clock で時刻を合わせる。prompt.go）
  calibrate?: CalibrateKind // タッチのキャリブレーションを始める（touch で 5 点、touch9 で 9 点。calibui.go）
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
  page_reset?: string // todo、calendar、month：触らなければ最初のページ（今月と今日）に戻るまでの時間（30s、2m など）。off で戻らない
  stale?: string // calendar、weather：最終更新がこれより古ければ、古いと出す（3h など）
  calendars?: string[] // calendar：出すカレンダーの名前。month：印を付けるカレンダー。省略するとすべて
  follow?: string // calendar：同じレイヤーの月の表示（その id）で選んだ日の予定を出す
  week_start?: 'sunday' | 'monday' // month：週の始まり。省略すると日曜
  work?: string // pomodoro：作業の時間（25m など）
  break?: string // pomodoro：休憩の時間（5m など）
  states?: PresenceSpec[] // presence：タップで順に切り替える状態。省略すると在席、集中、会議中、休憩
  // graph：グラフ（graphwidget.go）
  unit?: string // 値のあとに付ける単位
  min?: number // 縦の範囲の下。省略すると値から決める
  max?: number // 縦の範囲の上。省略すると値から決める
  chart?: 'line' | 'bar' // 折れ線（既定）か棒
  points?: number // 描く値の数（2〜120、既定 60）
  decimals?: number // 小数点以下の桁数。省略すると値の大きさで決める
  // presenter：プレゼン用タイマー（presenter.go）
  duration?: string // 持ち時間（20m など）
  colors?: PresenterColorSpec[] // 残り時間に応じた色。省略すると残り 5 分で黄、1 分で赤
  prefix?: boolean // calculator：16 進と 2 進の結果を、0x と 0b を付けて打ち込む
  // trackpad：トラックパッド（trackpad.go）。省略すると既定値
  speed?: number // 感度（画面の 1 ドットを、PC のマウスの何カウントにするか）
  accel?: number // 加速（0 で加速しない）
  scroll_width?: number // 右端のスクロールの帯の幅（ドット）。0 で帯なし
  scroll_direction?: 'natural' | 'traditional'
  scroll_step?: number // ホイール 1 段に当たる指の動き（ドット）
  settle_ms?: number // 触れた直後に捨てる時間
  smooth?: number // 平均を取るサンプルの数
  deadzone?: number // これより小さな動きを無視する（ドット）
  min_pressure?: number // 押す強さがこれより弱いサンプルを捨てる
  tap_ms?: number // これより短く触れて離せばタップ
  tap_move?: number // タップとみなす動きの上限（ドット）
  drag_ms?: number // タップのあと、これより早く触れればドラッグ
  long_press?: 'none' | 'right' // 長押しで右クリック
  // pentablet：ペンタブレット（pen.go）。memo（memo.go）も min_pressure、settle_ms、smooth を使う
  map?: 'full' | 'aspect' | 'region' // PC の画面のどの範囲に当てはめるか
  pc_screen?: string // PC の画面の大きさ（1920x1080 など）。aspect と region で使う
  region?: [number, number, number, number] // map: region の範囲 [x, y, 幅, 高さ]（PC の画面の画素）
  pressure?: boolean // 筆圧を送る（既定 true）
  pressure_range?: [number, number] // 筆圧の 0 と最大にする押す強さ（既定 [400, 2400]）
}

export type WidgetKind = 'clock' | 'text' | 'todo' | 'calendar' | 'trackpad' | 'battery' | 'stopwatch' | 'pomodoro' | 'counter' | 'qr' | 'presence' | 'month' | 'weather' | 'graph' | 'presenter' | 'calculator' | 'pentablet' | 'memo' | 'reader' | 'files' | 'settings' | 'help'

// プレゼン用タイマーの色の区切り 1 つ（presenter.go の PresenterColorSpec）
export interface PresenterColorSpec {
  remaining: string
  color: string
}

// 在席ランプの状態 1 つ（gadgetwidget.go の PresenceSpec）
export interface PresenceSpec {
  name: string
  color?: string // #rrggbb。省略すると順に決まった色
}

export type UsbMode = 'keyboard' | 'mouse' | 'toggle'

export type TermMode = 'on' | 'off' | 'toggle' | 'pc' | 'brain'

// 設定の terminal（termmode.go の TerminalConfig）。GUI では編集せず、そのまま残す
export interface TerminalConfig {
  port?: string
  getty?: string
  command?: string[]
  user?: string
  font?: 'wide' | 'narrow'
  scrollback?: number
}

// get_status の terminal（termmode.go の TermInfo）
export interface TermInfo {
  active: boolean
  state: 'off' | 'entering' | 'on' | 'leaving'
  transport?: 'serial' | 'command'
  status?: string
  cols?: number
  rows?: number
}

export type MouseAction = 'left' | 'right' | 'middle' | 'scroll_up' | 'scroll_down' | 'scroll_left' | 'scroll_right'

// メディアキー（media.go の mediaActions）
export type MediaAction = 'play_pause' | 'stop' | 'next' | 'previous' | 'fast_forward' | 'rewind' | 'volume_up' | 'volume_down' | 'mute'
  | 'brightness_up' | 'brightness_down' | 'calculator' | 'browser' | 'mail'

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

// list_photos の結果（photo.go の PhotoList）
export interface PhotoInfo {
  id: string
  name?: string
  w: number
  h: number
  bytes: number
  added: string
  source?: string
  of?: string // 縦向きに切り抜き直した写真なら、もとの写真の id
}

export interface PhotoListResult {
  photos: PhotoInfo[]
  max: number
  total_bytes: number
  free_bytes: number
  reserve_bytes: number
  showing?: string
}

// list_reads の結果（readdoc.go の ReadInfo）
export interface ReadInfo {
  id: string
  title: string
  author?: string
  kind: string
  source?: string
  date?: string
  bytes: number
  chars: number
  added: string
  pos?: { b: number; c: number }
  progress?: number
}

export interface ReadListResult {
  reads: ReadInfo[]
  total_bytes: number
  limit_bytes: number
  free_bytes: number
  reserve_bytes: number
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
  touch_portrait?: GridConfig // 縦向き（480x800）の格子。省略すると display.portrait_grid のとおり横向きの格子から作る
  status_bar?: boolean // 上に状態の帯を出すか。省略すると、格子を持つレイヤーなら status_bar.show、持たなければ下のレイヤー
}

export interface DisplayConfig {
  enabled?: boolean
  device?: string
  vt?: number
  rotate?: number
  press_style?: PressStyle // 押しているセルの見せ方。省略すると border
  portrait_grid?: 'transpose' | 'stretch' // touch_portrait を書いていないレイヤーの縦向きの格子。省略すると transpose
}

export type PressStyle = 'border' | 'fill'

export interface Config {
  hid_device?: string
  keyboard?: string
  keys?: Record<string, ActionSpec> // 旧形式
  touch?: TouchConfig
  layers: LayerConfig[]
  display?: DisplayConfig
  terminal?: TerminalConfig
  backlight?: BacklightConfig
  photo?: PhotoConfig
  files?: { roots?: { name: string; path: string; delete?: boolean }[] } // ファイルエクスプローラの見られる場所（files.go）
  home?: HomeConfig // ホーム画面（home.go）
  status_bar?: StatusBarConfig // 状態の帯（statusbar.go）
  offline?: { home?: boolean } // PC から外したときの動き（offline.go）
  battery?: BatteryConfig // 電池が少ないときの知らせと電源断、記録（batteryguard.go）
  clock?: { rtc?: 'read' | 'write' | 'off' } // 内蔵 RTC の使い方（rtc.go）
  lid?: { close?: 'off' | 'none' } // ふたを閉じたときの動き（lid.go）
  ime?: { method?: string; user?: string; idle?: string; terminal?: boolean } // 日本語の入力（uim-fep。ime.go）
}

// OverrideInfo は、Brain の設定のアプリで変えた上書きの値 1 つ（overrides.go。get_config、get_overrides）。
export interface OverrideInfo {
  key: string
  label: string
  value: unknown
  shown: string // 上書きの値（画面に出す形）
  file: string // 設定ファイルの値（画面に出す形）
  used: boolean
  note?: string
}

// InputKind は input の欄の種類。
export type InputKind = 'todo' | 'clock'

// CalibrateKind は calibrate の点の数（touch は 5 点、touch9 は 9 点）。
export type CalibrateKind = 'touch' | 'touch9'

// BatteryConfig は設定の battery。
export interface BatteryConfig {
  warn?: number
  critical?: number
  poweroff?: number
  warn_mv?: number // 電圧で決める（mV）
  poweroff_mv?: number
  poweroff_plugged?: boolean
  log?: boolean
  curve?: [number, number][]
}

// HomeApp はホーム画面のアプリ 1 つ。
export interface HomeApp {
  name: string
  icon?: string // 組み込みのアイコンの名前か、画像の id。省略すると app
  layer: string
}

// HomeConfig は設定の home。
export interface HomeConfig {
  cols?: number
  rows?: number
  portrait?: { cols: number; rows: number }
  label?: string
  start?: 'home' | 'base'
  soft_key?: string
  status_bar?: boolean
  background?: string
  apps: HomeApp[]
}

export type BarItem = 'time' | 'battery' | 'pc' | 'mouse' | 'sync'

// StatusBarConfig は設定の status_bar。
export interface StatusBarConfig {
  show?: boolean
  items?: BarItem[]
}

// 設定の backlight（backlight.go の BacklightConfig）。GUI では編集せず、そのまま残す
export interface BacklightConfig {
  device?: string
  dim_after?: string
  off_after?: string
  level?: number
  dim_level?: number
  wake_on_key?: boolean
  in_terminal?: boolean
  photo?: boolean // 暗くする代わりに写真を出す（photo.go）
  offline?: { dim_after?: string; off_after?: string; level?: number; dim_level?: number; photo?: boolean } // PC から外しているあいだ
}

// 設定の photo（photo.go の PhotoConfig）。GUI では編集せず、そのまま残す
export interface PhotoConfig {
  interval?: string
  order?: 'sequential' | 'random'
  night?: string
  level?: number
  max?: number
}

// ストップウォッチ、ポモドーロ、カウンター、在席ランプの状態（get_widgets、widgets の通知。gadgets.go）
export interface StopwatchState {
  elapsed_ms: number
  started_at?: string
  updated_at?: string
}

export interface PomodoroState {
  running?: boolean
  anchor?: string
  work_ms?: number
  break_ms?: number
  done?: number
  phase?: 'work' | 'break'
  remaining_ms?: number
  ack_at?: string
  updated_at?: string
}

export interface CounterState {
  value: number
  updated_at?: string
}

export interface PresenceState {
  state: string
  updated_at?: string
  source?: string
}

export interface WidgetsState {
  stopwatch?: Record<string, StopwatchState>
  pomodoro?: Record<string, PomodoroState>
  counter?: Record<string, CounterState>
  presence?: Record<string, PresenceState>
  presenter?: Record<string, StopwatchState>
}

// グラフの値（get_graph。graph.go の GraphPoint、GraphSeries）
export interface GraphPoint {
  t: string
  v: number
}

export interface GraphSeries {
  points: GraphPoint[]
  count?: number
  last?: GraphPoint
  source?: string
}

export interface GetGraphResult {
  series: Record<string, GraphSeries>
  ids: string[]
  now?: string
}

// 電池の状態（get_status の battery。battery.go の BatteryInfo）
export interface BatteryInfo {
  percent: number
  voltage_mv?: number
  plugged: boolean
  charging: boolean
  source?: string
  estimated?: boolean
  charger_off?: boolean // USB の 5V があるのに、i.MX28 の内蔵の充電器が止まっている（充電していない）
}

export interface GetWidgetsResult {
  widgets: WidgetsState
  calculator?: Record<string, import('./calc').CalcState>
  ids: Record<string, string[]>
  presence_states: Record<string, PresenceSpec[]>
  battery: BatteryInfo | null
  now?: string
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
  rotate?: number // 画面の向き（P1 より前の lefthand にはない）
  portrait?: boolean
  status_bar?: boolean
  home?: boolean // ホーム画面を出している
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
  holiday?: boolean // 祝日のカレンダー（月の表示で日付を赤くし、予定の印は付けない）
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

// 天気予報（Brain の get_weather。weather.go の WeatherData）
export interface WeatherDay {
  date: string // YYYY-MM-DD
  telop: string
  icon?: string // 気象庁の天気アイコンの番号
  max: number | null
  min: number | null
  rain: (number | null)[] // 0〜6、6〜12、12〜18、18〜24 時の降水確率（%）
}

export interface WeatherData {
  rev: number
  received_at?: string
  area?: string
  office?: string
  public_time?: string
  fetched_at?: string
  error?: string
  error_at?: string
  source?: string
  days: WeatherDay[]
  shown?: boolean // 設定に天気のセルがあるか（get_weather）
  icon_boxes?: [number, number][] // 今の設定で使うアイコンの箱
  icons?: { name: string; w: number; h: number; bytes: number }[] // Brain にあるアイコン
  icons_missing?: { name: string; icon: string; w: number; h: number }[]
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

// 端末モードが変わったとき（subscribe_input のあと）
export interface TerminalEvent extends TermInfo {
  event: 'terminal'
}

// Brain でストップウォッチなどの状態が変わったとき（subscribe_data のあと）
export interface WidgetsEvent extends WidgetsState {
  event: 'widgets'
}

export type Notification = InputEvent | LayerEvent | TodoEvent | TerminalEvent | WidgetsEvent

// list_files の 1 行（ファイルエクスプローラ。files.go）
export interface FileEntry {
  name: string
  path: string
  dir?: boolean
  link?: boolean
  size: number
  mtime: string
}

export interface FileListResult {
  path: string // 空なら場所の一覧
  root?: string
  delete?: boolean
  put?: boolean
  parent?: string
  entries: FileEntry[]
  truncated?: boolean
  partial?: boolean
  free_bytes: number
  total_bytes: number
}
