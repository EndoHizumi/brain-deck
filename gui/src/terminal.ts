// xterm.js の端末。コンソールのタブを開いたときに読み込む（設定だけを使うときは読まない）。

import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import type { TermLike } from './console'

export function createXterm(): TermLike {
  const term = new Terminal({
    cursorBlink: true,
    convertEol: false, // getty とシェルは \r\n を送る
    scrollback: 5000,
    fontSize: 14,
    // 日本語は全角で表示する。等幅の和文フォントがあれば使う
    fontFamily: '"Noto Sans Mono CJK JP", "Source Han Code JP", "BIZ UDGothic", "MS Gothic", "Osaka-Mono", Menlo, Consolas, "DejaVu Sans Mono", monospace',
    theme: { background: '#10161f', foreground: '#dfe6ee', cursor: '#7fe0a0' },
  })
  const fit = new FitAddon()
  term.loadAddon(fit)
  return {
    open: (el) => term.open(el),
    write: (s) => term.write(s),
    onData: (f) => void term.onData(f),
    // 端末に貼り付けたバイナリ（マウスの報告など）。UTF-8 にしないで、そのまま送る
    onBinary: (f) => void term.onBinary((s) => f(Uint8Array.from(s, (c) => c.charCodeAt(0) & 0xff))),
    fit: () => {
      try {
        fit.fit()
      } catch {}
      return { rows: term.rows, cols: term.cols }
    },
    focus: () => term.focus(),
    dispose: () => term.dispose(),
  }
}
