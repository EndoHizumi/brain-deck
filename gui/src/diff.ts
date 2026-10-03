// 保存の前に見せる差分。割り当て 1 つ、設定の値 1 つを単位に比べる。

import { describeAction } from './model'
import type { ActionSpec, Config, KeymapInfo } from './types'

export interface Change {
  where: string // 人が読む場所。例：レイヤー「基本」/ キー Q（KEY_Q）
  before: string | null // null は「なかった」
  after: string | null // null は「消した」
}

type Leaves = Map<string, { where: string; text: string }>

function keyName(code: string, km?: KeymapInfo): string {
  const labels = km?.keys.filter((k) => k.code === code).map((k) => k.label) ?? []
  const sym = km?.keys.filter((k) => k.symbol === code && k.code !== code).map((k) => `記号+${k.label}`) ?? []
  const all = [...new Set([...labels, ...sym])]
  return all.length ? `キー ${all.join('・')}（${code}）` : `キー ${code}`
}

function leaves(cfg: Config, km?: KeymapInfo): Leaves {
  const out: Leaves = new Map()
  const put = (path: string, where: string, v: unknown) => {
    if (v === undefined) return
    out.set(path, { where, text: typeof v === 'string' ? v : JSON.stringify(v) })
  }
  const act = (path: string, where: string, a: ActionSpec) =>
    out.set(path, { where, text: describeAction(a) })

  put('/hid_device', 'hid_device', cfg.hid_device)
  put('/keyboard', 'keyboard', cfg.keyboard)
  if (cfg.touch) {
    const t = cfg.touch
    for (const f of ['device', 'swap_xy', 'min_x', 'max_x', 'min_y', 'max_y'] as const)
      put(`/touch/${f}`, `タッチパネル / ${f}`, t[f])
    for (const [n, a] of Object.entries(t.soft_areas ?? {}))
      put(`/touch/soft_areas/${n}`, `ソフトキーの区画 ${n}`, `x ${a.x[0]}〜${a.x[1]}, y ${a.y[0]}〜${a.y[1]}`)
  }
  for (const [f, v] of Object.entries(cfg.display ?? {})) put(`/display/${f}`, `display / ${f}`, v)

  // レイヤーは名前で対応づける（順番を変えても、名前が同じなら同じレイヤー）
  cfg.layers.forEach((l, i) => {
    const lp = `/layer:${l.name}`
    const lw = `レイヤー「${l.label || l.name}」`
    put(`${lp}`, lw, i === 0 ? `${l.name}（base）` : l.name)
    put(`${lp}/label`, `${lw} / 表示名`, l.label || undefined)
    for (const [code, a] of Object.entries(l.keys ?? {})) act(`${lp}/keys/${code}`, `${lw} / ${keyName(code, km)}`, a)
    if (l.touch) {
      const size = l.touch.cols && l.touch.rows ? `${l.touch.cols}×${l.touch.rows}` : 'base と同じ大きさ'
      put(`${lp}/touch`, `${lw} / タッチの格子`, size)
      for (const [c, a] of Object.entries(l.touch.cells ?? {})) act(`${lp}/cells/${c}`, `${lw} / セル ${c}`, a)
    }
    for (const [n, a] of Object.entries(l.soft_keys ?? {})) act(`${lp}/soft/${n}`, `${lw} / ソフトキー ${n}`, a)
  })
  return out
}

// diffConfigs は before から after への変更を返す。並びは after の順（消したものは最後）。
export function diffConfigs(before: Config, after: Config, km?: KeymapInfo): Change[] {
  const a = leaves(before, km)
  const b = leaves(after, km)
  const out: Change[] = []
  for (const [p, nv] of b) {
    const ov = a.get(p)
    if (!ov) out.push({ where: nv.where, before: null, after: nv.text })
    else if (ov.text !== nv.text) out.push({ where: nv.where, before: ov.text, after: nv.text })
  }
  for (const [p, ov] of a) if (!b.has(p)) out.push({ where: ov.where, before: ov.text, after: null })
  const order = (x: Config) => x.layers.map((l) => l.name).join('\u0000')
  if (order(before) !== order(after) && before.layers.length === after.layers.length)
    out.push({ where: 'レイヤーの順番', before: before.layers.map((l) => l.name).join(', '), after: after.layers.map((l) => l.name).join(', ') })
  return out
}
