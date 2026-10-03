// 設定ファイルの読み込みと書き出し（PC にバックアップを残す）。

import { Document, isMap, isPair, isScalar, parseDocument, visit } from 'yaml'
import { clean, normalizeConfig } from './model'
import type { Config } from './types'

export class FileError extends Error {}

// parseConfigText は YAML か JSON の文字列を読む。JSON は YAML の一部なので、どちらも同じ関数で読める。
export function parseConfigText(text: string): Config {
  let raw: unknown
  try {
    const doc = parseDocument(text, { prettyErrors: true, uniqueKeys: true })
    if (doc.errors.length) throw doc.errors[0]
    raw = doc.toJS()
  } catch (e: any) {
    throw new FileError(`読み込めません：${e?.message ?? e}`)
  }
  if (raw === null || raw === undefined) throw new FileError('ファイルが空です')
  if (typeof raw !== 'object' || Array.isArray(raw)) throw new FileError('設定はオブジェクト（項目: 値）で書きます')
  return normalizeConfig(raw)
}

export function toJSON(cfg: Config): string {
  return JSON.stringify(clean(cfg), null, 2) + '\n'
}

const ACTION_PARENTS = new Set(['keys', 'cells', 'soft_keys'])

// toYAML は、lefthand が保存するのと同じ見た目の YAML にする。
// 割り当ては 1 行（キーだけなら `LCTRL+Z`、ほかは `{ key: B, label: ブラシ }`）、セルの番号は引用符付き。
export function toYAML(cfg: Config): string {
  const c = clean(cfg)
  // キーだけの割り当ては文字列にする
  for (const l of c.layers) {
    for (const m of [l.keys, l.touch?.cells, l.soft_keys]) {
      if (!m) continue
      for (const [k, a] of Object.entries(m)) {
        if (a.key && Object.keys(a).length === 1) (m as Record<string, unknown>)[k] = a.key
      }
    }
  }
  const doc = new Document(c)
  doc.commentBefore =
    ' lefthand の設定（設定 GUI が書き出したファイル）\n 書き方は docs/config.md、本体のキーの名前は docs/keymap-pwsh2.md を参照'
  visit(doc, {
    Pair(_, pair, path) {
      if (!isScalar(pair.key)) return
      const name = String(pair.key.value)
      const parent = path[path.length - 2]
      const parentKey = isPair(parent) && isScalar(parent.key) ? String(parent.key.value) : ''
      if (ACTION_PARENTS.has(parentKey) && isMap(pair.value)) pair.value.flow = true
      if (parentKey === 'soft_areas' && isMap(pair.value)) pair.value.flow = true
      if (/^\d+,\d+$/.test(name)) pair.key.type = 'QUOTE_DOUBLE'
    },
  })
  return doc.toString({ indent: 2, lineWidth: 0 })
}

// sameConfig は 2 つの設定が同じかどうか（保存の前後や、読み込んだファイルとの比較）。
export function sameConfig(a: Config, b: Config): boolean {
  return JSON.stringify(sortKeys(clean(a))) === JSON.stringify(sortKeys(clean(b)))
}

export function sortKeys(v: any): any {
  if (Array.isArray(v)) return v.map(sortKeys)
  if (v && typeof v === 'object') {
    const out: any = {}
    for (const k of Object.keys(v).sort()) if (v[k] !== undefined) out[k] = sortKeys(v[k])
    return out
  }
  return v
}
