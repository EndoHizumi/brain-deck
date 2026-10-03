import { App } from './app'
import { parseConfigText } from './yamlio'
import './style.css'

const root = document.getElementById('app')!

// ?demo を付けて開くと、Brain なしで試せる（設定の例 config.yaml を持った簡易デーモンにつなぐ）
if (new URLSearchParams(location.search).has('demo')) {
  Promise.all([import('./demo'), import('../../config.yaml?raw')]).then(([demo, sample]) => {
    const daemon = new demo.FakeDaemon(parseConfigText(sample.default))
    const { serial } = demo.fakeSerial()
    const app = new App(root, { serial, openTransport: async () => new demo.FakeTransport(daemon) })
    document.title = 'lefthand 設定（デモ）'
    // コンソールから lefthandDemo.pressKey('KEY_Q') などで、Brain で押したことにできる
    Object.assign(window as any, { lefthand: app, lefthandDemo: daemon })
  })
} else {
  ;(window as any).lefthand = new App(root)
}
