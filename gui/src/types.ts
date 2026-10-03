// 設定ファイルの構造（docs/config.md）。デーモンの get_config が返す JSON と同じ形。
// 割り当ては、GUI の中ではいつもオブジェクトの形（{ key: "B" }）で持つ。

export interface ActionSpec {
  key?: string
  layer_hold?: string
  layer_toggle?: string
  layer_oneshot?: string
  layer_to?: string
  label?: string
}

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
}

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

export type Notification = InputEvent | LayerEvent
