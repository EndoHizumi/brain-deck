import { App } from './app'
import { parseConfigText } from './yamlio'
import './style.css'

const root = document.getElementById('app')!

// ?demo を付けて開くと、Brain なしで試せる（設定の例 config.yaml を持った簡易デーモンにつなぐ）
if (new URLSearchParams(location.search).has('demo')) {
  Promise.all([import('./demo'), import('../../config.yaml?raw')]).then(([demo, sample]) => {
    const daemon = new demo.FakeDaemon(parseConfigText(sample.default))
    // テキストのタイルを試すための中身（brain-deck text build "ビルド成功" --style ok と同じ）
    daemon.texts = { build: { text: 'ビルド成功', style: 'ok', set_at: new Date().toISOString() } }
    // Todo を試すための項目（brain-deck todo add と同じ）。lefthandDemo.toggleTodo('t1') で、Brain で長押ししたことにできる
    for (const text of ['牛乳を買う', 'PR #42 のレビュー', '歯医者の予約']) daemon.todoCmd({ cmd: 'todo_add', text, source: 'brain-deck' })
    // カレンダーを試すための予定（brain-deck calendar sync で送るものと同じ形）
    daemon.calendar = demo.demoCalendar()
    // ポートの一覧では、開いているタブに合わせて、設定用かコンソール用を選んだことにする
    const { serial, consolePort } = demo.fakeSerial(() => (app.section === 'console' ? 'console' : 'settings'))
    const app: App = new App(root, {
      serial,
      openTransport: async (p) => (p === consolePort ? new demo.FakeConsole() : new demo.FakeTransport(daemon)),
    })
    document.title = 'lefthand 設定（デモ）'
    // コンソールから lefthandDemo.pressKey('KEY_Q') などで、Brain で押したことにできる
    Object.assign(window as any, { lefthand: app, lefthandDemo: daemon })
  })
} else {
  ;(window as any).lefthand = new App(root)
}
