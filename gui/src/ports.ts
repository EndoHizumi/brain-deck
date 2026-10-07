// Brain の 2 つのシリアル（コンソール用と設定用）を、どちらのタブで使うかを覚える。
//
// WebSerial の getInfo() で分かるのは USB の ID（1d6b:0104）だけで、2 つの ACM は同じに見える
// （インターフェイス番号もデバイスの名前も、ページからは見えない）。
// コンソール用のポートに hello を書くと、ログイン画面にユーザー名として入力されてしまうので、
// どちらか分からないポートには、設定のタブから自動では何も書かない。次のどれかで分かったポートだけを使う。
//   - 設定のタブ：hello に lefthand が答えた（そのポートは設定用）
//   - コンソールのタブ：lefthand ではない文字（ログイン画面など）が届いた（そのポートはコンソール用）。
//     lefthand の JSON が届いたら、設定用だと分かる
//   - Brain のポートが 2 つで、一方が分かっていれば、もう一方はその逆
// 分からないときは、ポートの一覧（ブラウザの画面。Linux なら ttyACM0 / ttyACM1 の名前が出る）で選んでもらう。
// ブラウザの SerialPort はケーブルを抜き差しすると別のものになり、ページを開き直しても覚えておけないので、
// 覚えるのはページを開いているあいだ、ケーブルが抜けるまで。

import { BRAIN_FILTER } from './serial'

export type PortRole = 'settings' | 'console'

export const ROLE_NAME: Record<PortRole, string> = { settings: '設定用', console: 'コンソール用' }

export function isBrainPort(p: SerialPort): boolean {
  const i = p.getInfo()
  return i.usbVendorId === BRAIN_FILTER.usbVendorId && i.usbProductId === BRAIN_FILTER.usbProductId
}

// otherRole は、もう一方の役割。
export function otherRole(r: PortRole): PortRole {
  return r === 'settings' ? 'console' : 'settings'
}

export class PortRoles {
  // 確かめた役割
  private roles = new Map<SerialPort, PortRole>()
  // 今どちらのタブが開いているか
  private using = new Map<SerialPort, PortRole>()

  role(p: SerialPort): PortRole | undefined {
    return this.roles.get(p)
  }

  // learn は、確かめた役割を覚える。
  learn(p: SerialPort, r: PortRole): void {
    this.roles.set(p, r)
  }

  // forget は、ケーブルが抜けたポートを忘れる（つなぎ直すと、別の SerialPort になる）。
  forget(p: SerialPort): void {
    this.roles.delete(p)
    this.using.delete(p)
  }

  use(p: SerialPort, r: PortRole): void {
    this.using.set(p, r)
  }

  release(p: SerialPort): void {
    this.using.delete(p)
  }

  user(p: SerialPort): PortRole | undefined {
    return this.using.get(p)
  }

  // pick は、role のタブで使えるポートを、確かめた役割から選ぶ。分からなければ null。
  // ports は許可済みの Brain のポート（getPorts の結果）。
  pick(role: PortRole, ports: SerialPort[]): SerialPort | null {
    const known = ports.find((p) => this.roles.get(p) === role && !this.using.has(p))
    if (known) return known
    // Brain のポートは 2 つ。一方の役割が分かっていれば、もう一方はその逆
    if (ports.length !== 2) return null
    const [a, b] = ports
    const ra = this.roles.get(a)
    const rb = this.roles.get(b)
    const other = otherRole(role)
    const p = ra === other && rb === undefined ? b : rb === other && ra === undefined ? a : null
    if (!p || this.using.has(p)) return null
    return p
  }

  // conflict は、ユーザーが一覧から選んだポートを role のタブで使えないときの理由。使えるなら null。
  conflict(p: SerialPort, role: PortRole): string | null {
    const u = this.using.get(p)
    if (u === role) return null
    if (u) return `そのポートは${u === 'console' ? 'コンソール' : '設定'}のタブで使っています`
    if (this.roles.get(p) === otherRole(role)) return `そのポートは${ROLE_NAME[otherRole(role)]}です`
    return null
  }
}

// looksLikeLefthand は、届いた文字が lefthand の返事（1 行の JSON）に見えるか。
export function looksLikeLefthand(text: string): boolean {
  for (const line of text.split('\n')) {
    const s = line.trim()
    if (!s) continue
    if (!s.startsWith('{')) return false
    try {
      const m = JSON.parse(s)
      return typeof m === 'object' && m !== null && ('ok' in m || 'event' in m || 'error' in m || 'result' in m)
    } catch {
      return false // 行の途中で切れた JSON かもしれないので、決めない
    }
  }
  return false
}

// looksLikeConsole は、届いた文字が lefthand ではないもの（ログイン画面、シェル）に見えるか。
// 行の途中で切れた JSON を、コンソールと間違えないようにする。
export function looksLikeConsole(text: string): boolean {
  const s = text.replace(/^[\s\0]+/, '')
  if (!s) return false
  return !s.startsWith('{')
}

// looksLikePrompt は、届いた文字にログイン画面かシェルのプロンプトがあるか。コンソール用だと決めるのは、これが見えたときだけ。
// 設定のタブは、この判定をもとにもう一方のポートへ hello を送るので、lefthand の返事の切れ端（`ok":true}` など）と
// 間違えないよう、はっきりしたものだけにする。
export function looksLikePrompt(text: string): boolean {
  return looksLikeConsole(text) && /(login|Password): ?$|\S+@\S+:[^\r\n]*[$#] ?$/m.test(text)
}
