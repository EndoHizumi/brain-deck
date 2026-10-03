// PC に送れるキーの名前（main.go の modBits と hidUsage と同じ）と、その組み立て。

export const MODIFIERS = ['LCTRL', 'LSHIFT', 'LALT', 'LGUI', 'RCTRL', 'RSHIFT', 'RALT', 'RGUI'] as const
export type Modifier = (typeof MODIFIERS)[number]

const range = (n: number, f: (i: number) => string) => Array.from({ length: n }, (_, i) => f(i))

// 一覧から選ぶときのグループ
export const KEY_GROUPS: { title: string; keys: string[] }[] = [
  { title: '文字', keys: range(26, (i) => String.fromCharCode(65 + i)) },
  { title: '数字', keys: [...range(9, (i) => String(i + 1)), '0'] },
  { title: 'ファンクション', keys: range(12, (i) => `F${i + 1}`) },
  {
    title: '編集',
    keys: ['ENTER', 'ESC', 'BACKSPACE', 'TAB', 'SPACE', 'INSERT', 'DELETE', 'HOME', 'END', 'PAGEUP', 'PAGEDOWN'],
  },
  { title: '矢印', keys: ['UP', 'DOWN', 'LEFT', 'RIGHT'] },
  {
    title: 'テンキー（配列によらない）',
    keys: [...range(10, (i) => `KP${i}`), 'KPPLUS', 'KPMINUS', 'KPASTERISK', 'KPSLASH', 'KPDOT', 'KPENTER'],
  },
  {
    title: '記号（US 配列の位置）',
    keys: ['MINUS', 'EQUAL', 'LEFTBRACE', 'RIGHTBRACE', 'BACKSLASH', 'SEMICOLON', 'APOSTROPHE', 'GRAVE', 'COMMA', 'DOT', 'SLASH'],
  },
]

export const ALL_KEYS = new Set(KEY_GROUPS.flatMap((g) => g.keys))
const MOD_SET = new Set<string>(MODIFIERS)

// 画面に出す短い名前（main.go の prettyKey と同じ）
export const PRETTY: Record<string, string> = {
  LCTRL: 'Ctrl', RCTRL: 'Ctrl', LSHIFT: 'Shift', RSHIFT: 'Shift',
  LALT: 'Alt', RALT: 'Alt', LGUI: 'Win', RGUI: 'Win',
  ENTER: 'Enter', ESC: 'Esc', BACKSPACE: 'BS', TAB: 'Tab', SPACE: 'Space',
  MINUS: '-', EQUAL: '=', LEFTBRACE: '[', RIGHTBRACE: ']',
  BACKSLASH: '\\', SEMICOLON: ';', APOSTROPHE: "'", GRAVE: '`',
  COMMA: ',', DOT: '.', SLASH: '/',
  INSERT: 'Ins', HOME: 'Home', PAGEUP: 'PgUp', DELETE: 'Del',
  END: 'End', PAGEDOWN: 'PgDn',
  RIGHT: '→', LEFT: '←', DOWN: '↓', UP: '↑',
  KPSLASH: '/', KPASTERISK: '*', KPMINUS: '-', KPPLUS: '+', KPENTER: 'Enter', KPDOT: '.',
}

// prettyCombo は "LCTRL+LSHIFT+Z" を "Ctrl+Shift+Z" にする（Brain の画面と同じ）。
export function prettyCombo(s: string): string {
  return s
    .split('+')
    .map((p) => {
      const u = p.trim().toUpperCase()
      return PRETTY[u] ?? u
    })
    .join('+')
}

// 一覧に出す説明（テンキーなど、短い名前だけでは分かりにくいもの）
export function keyTitle(k: string): string {
  if (k.startsWith('KP')) return `テンキー ${PRETTY[k] ?? k.slice(2)}`
  return PRETTY[k] && PRETTY[k] !== k ? `${PRETTY[k]}（${k}）` : k
}

export interface Combo {
  mods: Modifier[]
  keys: string[]
}

// parseCombo は "LCTRL+Z" を分解する。知らない名前があれば null。
export function parseCombo(s: string): Combo | null {
  const c: Combo = { mods: [], keys: [] }
  for (const raw of s.split('+')) {
    const p = raw.trim().toUpperCase()
    if (MOD_SET.has(p)) {
      if (!c.mods.includes(p as Modifier)) c.mods.push(p as Modifier)
    } else if (ALL_KEYS.has(p)) {
      c.keys.push(p)
    } else {
      return null
    }
  }
  return c
}

// formatCombo は修飾キーを決まった順に並べて "+" でつなぐ。
export function formatCombo(c: Combo): string {
  const mods = MODIFIERS.filter((m) => c.mods.includes(m))
  return [...mods, ...c.keys].join('+')
}

// ---------- PC のキーボードからの取り込み ----------

// KeyboardEvent.code（物理位置）→ 送るキーの名前。配列によらず、押した位置のキーになる
const CODE_MAP: Record<string, string> = {
  Enter: 'ENTER', Escape: 'ESC', Backspace: 'BACKSPACE', Tab: 'TAB', Space: 'SPACE',
  Minus: 'MINUS', Equal: 'EQUAL', BracketLeft: 'LEFTBRACE', BracketRight: 'RIGHTBRACE',
  Backslash: 'BACKSLASH', Semicolon: 'SEMICOLON', Quote: 'APOSTROPHE', Backquote: 'GRAVE',
  Comma: 'COMMA', Period: 'DOT', Slash: 'SLASH',
  Insert: 'INSERT', Delete: 'DELETE', Home: 'HOME', End: 'END', PageUp: 'PAGEUP', PageDown: 'PAGEDOWN',
  ArrowUp: 'UP', ArrowDown: 'DOWN', ArrowLeft: 'LEFT', ArrowRight: 'RIGHT',
  NumpadAdd: 'KPPLUS', NumpadSubtract: 'KPMINUS', NumpadMultiply: 'KPASTERISK',
  NumpadDivide: 'KPSLASH', NumpadDecimal: 'KPDOT', NumpadEnter: 'KPENTER',
}

const MOD_CODES: Record<string, Modifier> = {
  ControlLeft: 'LCTRL', ControlRight: 'RCTRL', ShiftLeft: 'LSHIFT', ShiftRight: 'RSHIFT',
  AltLeft: 'LALT', AltRight: 'RALT', MetaLeft: 'LGUI', MetaRight: 'RGUI',
  OSLeft: 'LGUI', OSRight: 'RGUI',
}

export function keyFromCode(code: string): string | null {
  let m: RegExpMatchArray | null
  if ((m = code.match(/^Key([A-Z])$/))) return m[1]
  if ((m = code.match(/^Digit(\d)$/))) return m[1]
  if ((m = code.match(/^Numpad(\d)$/))) return `KP${m[1]}`
  if ((m = code.match(/^F(\d{1,2})$/)) && +m[1] >= 1 && +m[1] <= 12) return `F${m[1]}`
  return CODE_MAP[code] ?? null
}

export function modifierFromCode(code: string): Modifier | null {
  return MOD_CODES[code] ?? null
}

// ComboCapture は keydown / keyup を受け取り、ショートカットを 1 つ組み立てる。
// 修飾キー以外が押されたらそこで決まる。修飾キーだけを押して離したときは、修飾キーだけで決まる。
export class ComboCapture {
  private held = new Set<Modifier>()
  private peak = new Set<Modifier>() // 修飾キーだけのとき、離すまでに押していたもの

  constructor(
    private done: (combo: string) => void,
    private unsupported: (code: string) => void = () => {},
  ) {}

  keydown(e: { code: string; repeat?: boolean }): void {
    const mod = modifierFromCode(e.code)
    if (mod) {
      this.held.add(mod)
      this.peak.add(mod)
      return
    }
    if (e.repeat) return
    const key = keyFromCode(e.code)
    if (!key) {
      this.unsupported(e.code)
      return
    }
    this.done(formatCombo({ mods: [...this.held], keys: [key] }))
    this.held.clear()
    this.peak.clear()
  }

  keyup(e: { code: string }): void {
    const mod = modifierFromCode(e.code)
    if (!mod) return
    this.held.delete(mod)
    if (this.held.size === 0 && this.peak.size > 0) {
      this.done(formatCombo({ mods: [...this.peak], keys: [] }))
      this.peak.clear()
    }
  }
}
