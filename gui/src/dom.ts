// DOM を組み立てる小さな補助。画面は状態が変わるたびに作り直す（要素の数は数百程度）。

type Child = Node | string | number | null | undefined | false | Child[]
type Props = Record<string, any> | null

export function h<K extends keyof HTMLElementTagNameMap>(tag: K, props?: Props, ...children: Child[]): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag)
  for (const [k, v] of Object.entries(props ?? {})) {
    if (v === undefined || v === null || v === false) continue
    if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v)
    else if (k === 'class') el.className = Array.isArray(v) ? v.filter(Boolean).join(' ') : String(v)
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v)
    else if (k === 'value' || k === 'checked' || k === 'selected' || k === 'disabled') (el as any)[k] = v
    else if (k === 'dataset') Object.assign(el.dataset, v)
    else el.setAttribute(k, v === true ? '' : String(v))
  }
  append(el, children)
  return el
}

function append(el: Node, children: Child[]): void {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue
    if (Array.isArray(c)) append(el, c)
    else el.appendChild(c instanceof Node ? c : document.createTextNode(String(c)))
  }
}

// replaceKeepingFocus は root の中身を作り直す。入力中の欄（data-focus を付けたもの）の
// フォーカスとカーソル位置を引き継ぐ。
export function replaceKeepingFocus(root: HTMLElement, content: Node): void {
  const active = document.activeElement as HTMLInputElement | null
  const key = active && root.contains(active) ? active.dataset?.focus : undefined
  let start: number | null = null
  let end: number | null = null
  try {
    start = active?.selectionStart ?? null
    end = active?.selectionEnd ?? null
  } catch {}
  root.replaceChildren(content)
  if (!key) return
  const next = root.querySelector<HTMLInputElement>(`[data-focus="${key.replace(/["\\]/g, '\\$&')}"]`)
  if (!next) return
  next.focus()
  try {
    if (start !== null) next.setSelectionRange(start, end)
  } catch {}
}
