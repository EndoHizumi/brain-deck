// コンソールのタブ：Brain の 1 つ目のシリアル（/dev/ttyGS0 の getty）を、ブラウザの端末で使う。
//
// - 設定のタブとは別のポートなので、両方を同時に開いておける（ports.ts で、どちらのタブが使っているかを共有する）。
// - 自分からは何も送らない。送るのは、端末で打った文字と、「大きさを合わせる」を押したときの stty だけ。
//   ログイン画面に勝手に文字が入らないようにするため。
// - 文字コードは UTF-8。行の途中や文字の途中で切れて届いても、TextDecoder の stream で続けて読む。

import type { Transport } from './protocol'
import { BRAIN_FILTER } from './serial'
import { PortRoles, isBrainPort, looksLikeLefthand, looksLikePrompt } from './ports'

// TermLike は端末の画面（本物は terminal.ts の xterm.js。テストでは模擬）。
export interface TermLike {
  open(el: HTMLElement): void
  write(s: string): void
  onData(f: (s: string) => void): void
  onBinary(f: (b: Uint8Array) => void): void
  // fit は端末を枠の大きさに合わせ、行と桁の数を返す
  fit(): { rows: number; cols: number }
  focus(): void
  dispose(): void
}

export interface ConsoleDeps {
  serial: Serial | null
  openTransport: (port: SerialPort) => Promise<Transport>
  ports: PortRoles
  createTerminal: () => Promise<TermLike>
  onChange: () => void
}

export type ConsoleStatus = { text: string; level: 'info' | 'ok' | 'error' }

export interface TermSize {
  rows: number
  cols: number
}

// sttyCommand は、端末の大きさをシェルに伝えるコマンド（シリアルでは自動で伝わらない）。
export function sttyCommand(s: TermSize): string {
  return `stty rows ${s.rows} cols ${s.cols}`
}

// 端末に出す、GUI からの知らせ（Brain には送らない）。黄色で出す
function note(s: string): string {
  return `\r\n\x1b[33m[${s}]\x1b[0m\r\n`
}

export class ConsoleSession {
  // 端末を置く要素。画面を作り直しても、同じ要素を使い続ける（作り直すと、端末の中身とフォーカスが消える）
  readonly host: HTMLElement
  state: 'idle' | 'connecting' | 'open' = 'idle'
  status: ConsoleStatus | null = null
  size: TermSize | null = null // 今の端末の大きさ
  sentSize: TermSize | null = null // 最後に stty で伝えた大きさ
  private term: TermLike | null = null
  private termLoading: Promise<TermLike> | null = null
  private t: Transport | null = null
  private port: SerialPort | null = null
  private lastPort: SerialPort | null = null // 前に選んだポート（ケーブルを抜くまで）
  private dec = new TextDecoder()
  private enc = new TextEncoder()
  private classify = false // ポートの役割がまだ分からず、届いた文字で決めるか
  private sniff = '' // 役割を決めるために、届いた文字を少し覚える
  private closing = false

  constructor(private deps: ConsoleDeps) {
    this.host = document.createElement('div')
    this.host.className = 'console-host'
    if (typeof ResizeObserver !== 'undefined') new ResizeObserver(() => this.fit()).observe(this.host)
  }

  get open(): boolean {
    return this.state === 'open'
  }

  private set(status: ConsoleStatus | null): void {
    this.status = status
    this.deps.onChange()
  }

  // shown は、タブを表示したときに呼ぶ。端末をまだ作っていなければ作り、枠に合わせる。
  async shown(): Promise<void> {
    await this.terminal()
    this.fit()
  }

  private terminal(): Promise<TermLike> {
    if (this.term) return Promise.resolve(this.term)
    if (!this.termLoading)
      this.termLoading = this.deps.createTerminal().then((term) => {
        term.open(this.host)
        term.onData((s) => void this.sendBytes(this.enc.encode(s)))
        term.onBinary((b) => void this.sendBytes(b))
        term.write('\x1b[90mBrain のコンソール（/dev/ttyGS0）。「接続」を押してください。\x1b[0m\r\n')
        this.term = term
        return term
      })
    return this.termLoading
  }

  fit(): void {
    if (!this.term || !this.host.isConnected) return
    const s = this.term.fit()
    if (s.rows > 0 && s.cols > 0 && (s.rows !== this.size?.rows || s.cols !== this.size?.cols)) {
      this.size = s
      this.deps.onChange()
    }
  }

  focus(): void {
    this.term?.focus()
  }

  async connect(): Promise<void> {
    const serial = this.deps.serial
    if (!serial) return this.set({ text: 'このブラウザは WebSerial に対応していません。Chrome か Edge で開いてください', level: 'error' })
    if (this.state !== 'idle') return
    const term = await this.terminal()
    this.state = 'connecting'
    this.set(null)
    try {
      const brain = (await serial.getPorts()).filter(isBrainPort)
      const ports = this.deps.ports
      // 前に選んだポート、コンソール用だと分かっているポート、設定用の逆のポートの順に、聞かずに使う
      let port =
        this.lastPort && brain.includes(this.lastPort) && !ports.user(this.lastPort) && ports.role(this.lastPort) !== 'settings'
          ? this.lastPort
          : ports.pick('console', brain)
      if (!port) {
        this.set({ text: '一覧から、Brain のコンソール用のポート（1 つ目。Linux では ttyACM0、macOS では番号の小さい cu.usbmodem…）を選んでください', level: 'info' })
        try {
          port = await serial.requestPort({ filters: [BRAIN_FILTER] })
        } catch {
          this.state = 'idle'
          return this.set({ text: 'ポートが選ばれませんでした', level: 'info' })
        }
        const conflict = ports.conflict(port, 'console')
        if (conflict) {
          this.state = 'idle'
          return this.set({ text: `${conflict}。もう一度「接続」を押して、もう一方を選んでください`, level: 'error' })
        }
      }
      let t: Transport
      try {
        t = await this.deps.openTransport(port)
      } catch (e: any) {
        this.state = 'idle'
        return this.set({
          text:
            `シリアルポートを開けません（${e?.message ?? e}）。ほかのタブやアプリ（screen、minicom など）が使っていないか、` +
            'Linux なら dialout グループに入っているかを確かめてください。つないだ直後は ModemManager が調べていることがあるので、少し待ってからもう一度押してください',
          level: 'error',
        })
      }
      ports.use(port, 'console')
      this.t = t
      this.port = port
      this.lastPort = port
      this.dec = new TextDecoder()
      this.classify = !ports.role(port) // 役割がまだ分からないときだけ調べる
      this.sniff = ''
      this.closing = false
      t.onData = (b) => this.t === t && this.onData(b)
      t.onClose = (reason) => this.t === t && this.onClose(reason)
      this.state = 'open'
      term.write(note('接続しました。ログイン画面が出ていなければ、Enter を押してください'))
      this.set({ text: '接続しました', level: 'ok' })
      this.fit()
      term.focus()
    } finally {
      if (this.state === 'connecting') this.state = 'idle'
      this.deps.onChange()
    }
  }

  private onData(b: Uint8Array): void {
    const s = this.dec.decode(b, { stream: true })
    if (!s) return
    this.term?.write(s)
    if (this.classify && this.port) {
      this.sniff = (this.sniff + s).slice(-2048)
      if (looksLikeLefthand(this.sniff)) {
        this.deps.ports.learn(this.port, 'settings')
        this.classify = false
        this.term?.write(note('lefthand の返事が届きました。これは設定用のポートです。「切断」して、もう一方のポートを選んでください'))
        this.set({ text: 'これは設定用のポートです。切断して、もう一方（1 つ目）を選んでください', level: 'error' })
      } else if (looksLikePrompt(this.sniff)) {
        this.deps.ports.learn(this.port, 'console')
        this.classify = false
      }
    }
  }

  private onClose(reason: string): void {
    const rest = this.dec.decode()
    if (rest) this.term?.write(rest)
    if (this.port) this.deps.ports.release(this.port)
    const byUser = this.closing || reason === 'closed by user'
    this.t = null
    this.port = null
    this.state = 'idle'
    this.sentSize = null
    if (byUser) {
      this.term?.write(note('切断しました'))
      this.set({ text: '切断しました', level: 'info' })
    } else {
      const lost = /lost|disconnect|device/i.test(reason)
      const why = lost ? 'ケーブルが抜けたか、Brain の USB が付け直されました' : `接続が切れました（${reason}）`
      this.term?.write(note(`${why}。つなぎ直してから「接続」を押してください`))
      this.set({ text: `${why}。つなぎ直してから「接続」を押してください`, level: 'error' })
    }
  }

  async disconnect(): Promise<void> {
    const t = this.t
    if (!t) return
    this.closing = true
    await t.close().catch(() => {})
    if (this.t === t) this.onClose('closed by user') // 読み込みのループが終わるのを待たずに、状態を戻す
  }

  // forget は、抜いたポートを、次の接続で使わないようにする。
  forget(port: SerialPort): void {
    if (this.lastPort === port) this.lastPort = null
  }

  private async sendBytes(b: Uint8Array): Promise<void> {
    if (!this.t || !b.length) return
    try {
      await this.t.send(b)
    } catch {
      /* 切れたときは onClose で知らせる */
    }
  }

  // sendSize は、今の端末の大きさを stty で Brain のシェルに伝える。押したときだけ送る
  // （ログイン画面やエディタの中で送ると、そのまま入力されてしまうため）。
  async sendSize(): Promise<void> {
    if (!this.t) return
    this.fit()
    if (!this.size) return
    await this.sendBytes(this.enc.encode(sttyCommand(this.size) + '\r'))
    this.sentSize = { ...this.size }
    this.deps.onChange()
    this.term?.focus()
  }
}
