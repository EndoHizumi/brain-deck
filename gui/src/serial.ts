// WebSerial（Chrome / Edge）のトランスポート。

import type { Transport } from './protocol'

// gadget-setup.sh の idVendor / idProduct（Linux Foundation の Multifunction Composite Gadget）
export const BRAIN_FILTER: SerialPortFilter = { usbVendorId: 0x1d6b, usbProductId: 0x0104 }

export function serialSupported(): boolean {
  return typeof navigator !== 'undefined' && 'serial' in navigator
}

export class WebSerialTransport implements Transport {
  onData: (chunk: Uint8Array) => void = () => {}
  onClose: (reason: string) => void = () => {}
  private reader: ReadableStreamDefaultReader<Uint8Array> | null = null
  private writer: WritableStreamDefaultWriter<Uint8Array> | null = null
  private closing = false

  private constructor(public port: SerialPort) {}

  static async open(port: SerialPort): Promise<WebSerialTransport> {
    // ACM なので速度は使われないが、指定は必須。バッファは大きな設定を一度に受けられる大きさにする
    await port.open({ baudRate: 115200, bufferSize: 64 * 1024 })
    const t = new WebSerialTransport(port)
    t.writer = port.writable!.getWriter()
    t.readLoop()
    return t
  }

  private async readLoop(): Promise<void> {
    let reason = 'the serial port was closed'
    try {
      while (this.port.readable && !this.closing) {
        this.reader = this.port.readable.getReader()
        try {
          for (;;) {
            const { value, done } = await this.reader.read()
            if (done) break
            if (value && value.length) this.onData(value)
          }
        } finally {
          this.reader.releaseLock()
          this.reader = null
        }
      }
    } catch (e: any) {
      reason = `serial error: ${e?.message ?? e}` // ケーブルが抜けたときなど
    }
    this.onClose(this.closing ? 'closed by user' : reason)
  }

  async send(bytes: Uint8Array): Promise<void> {
    if (!this.writer) throw new Error('not open')
    await this.writer.write(bytes)
  }

  async close(): Promise<void> {
    this.closing = true
    try {
      await this.reader?.cancel()
    } catch {}
    try {
      this.writer?.releaseLock()
    } catch {}
    try {
      await this.port.close()
    } catch {}
  }
}

// portLabel はポートを人が見分けるための短い説明。
export function portLabel(port: SerialPort): string {
  const i = port.getInfo()
  if (i.usbVendorId === undefined) return 'シリアルポート'
  const hex = (n?: number) => (n ?? 0).toString(16).padStart(4, '0')
  return `USB ${hex(i.usbVendorId)}:${hex(i.usbProductId)}`
}
