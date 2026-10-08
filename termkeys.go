package main

import (
	"strconv"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 端末モードのキー入力 ----------
//
// 端末モードのあいだ、本体キーは PC に HID のキーとしては送らず、ここで文字（バイト列）にして端末に書く。
// 文字は US 配列で決める（カーネルのキーマップは通らない）。Brain のカーネルは「記号」を押しているあいだ
// D〜L、N、M、− を KEY_GRAVE などの記号のキーコードで出すので、記号とシフトの組み合わせで、
// US 配列のすべての記号が打てる（docs/keymap-pwsh2.md、README の「端末モード」の表）。
//
// 修飾キー：ページアップ（KEY_LEFTCTRL）が Ctrl、文字切り替え（KEY_LEFTALT）が Alt（ESC を前に付ける）、
// シフト（KEY_LEFTSHIFT）が Shift。タッチの Ctrl と Alt は、次の 1 キーだけに効く（sticky）。
// 抜ける：文字切り替え + 調べる/戻る（Alt+Esc）。

// termKeyResult は、キーを押したときにすること。
type termKeyResult struct {
	out    []byte // 端末に書くバイト列
	exit   bool   // 端末モードを抜ける
	scroll int    // 履歴を見る（ページ。正で古いほうへ）
}

// usKeys は US 配列で、キーコード → シフトなし、シフトありの文字。
var usKeys = map[evdev.EvCode][2]byte{
	evdev.KEY_1: {'1', '!'}, evdev.KEY_2: {'2', '@'}, evdev.KEY_3: {'3', '#'}, evdev.KEY_4: {'4', '$'},
	evdev.KEY_5: {'5', '%'}, evdev.KEY_6: {'6', '^'}, evdev.KEY_7: {'7', '&'}, evdev.KEY_8: {'8', '*'},
	evdev.KEY_9: {'9', '('}, evdev.KEY_0: {'0', ')'},
	evdev.KEY_MINUS: {'-', '_'}, evdev.KEY_EQUAL: {'=', '+'}, evdev.KEY_LEFTBRACE: {'[', '{'},
	evdev.KEY_RIGHTBRACE: {']', '}'}, evdev.KEY_BACKSLASH: {'\\', '|'}, evdev.KEY_SEMICOLON: {';', ':'},
	evdev.KEY_APOSTROPHE: {'\'', '"'}, evdev.KEY_GRAVE: {'`', '~'}, evdev.KEY_COMMA: {',', '<'},
	evdev.KEY_DOT: {'.', '>'}, evdev.KEY_SLASH: {'/', '?'}, evdev.KEY_SPACE: {' ', ' '},
}

func init() {
	letters := []evdev.EvCode{evdev.KEY_A, evdev.KEY_B, evdev.KEY_C, evdev.KEY_D, evdev.KEY_E, evdev.KEY_F,
		evdev.KEY_G, evdev.KEY_H, evdev.KEY_I, evdev.KEY_J, evdev.KEY_K, evdev.KEY_L, evdev.KEY_M,
		evdev.KEY_N, evdev.KEY_O, evdev.KEY_P, evdev.KEY_Q, evdev.KEY_R, evdev.KEY_S, evdev.KEY_T,
		evdev.KEY_U, evdev.KEY_V, evdev.KEY_W, evdev.KEY_X, evdev.KEY_Y, evdev.KEY_Z}
	for i, c := range letters {
		usKeys[c] = [2]byte{'a' + byte(i), 'A' + byte(i)}
	}
}

// csiKeys は、修飾キーを付けると CSI 1;<mod> の形になるキー（矢印、Home、End）。
var csiKeys = map[evdev.EvCode]byte{
	evdev.KEY_UP: 'A', evdev.KEY_DOWN: 'B', evdev.KEY_RIGHT: 'C', evdev.KEY_LEFT: 'D',
	evdev.KEY_HOME: 'H', evdev.KEY_END: 'F',
}

// tildeKeys は CSI <n> ~ の形のキー。
var tildeKeys = map[evdev.EvCode]int{
	evdev.KEY_INSERT: 2, evdev.KEY_DELETE: 3, evdev.KEY_PAGEUP: 5, evdev.KEY_PAGEDOWN: 6,
	evdev.KEY_F1: 11, evdev.KEY_F2: 12, evdev.KEY_F3: 13, evdev.KEY_F4: 14, evdev.KEY_F5: 15,
	evdev.KEY_F6: 17, evdev.KEY_F7: 18, evdev.KEY_F8: 19, evdev.KEY_F9: 20, evdev.KEY_F10: 21,
	evdev.KEY_F11: 23, evdev.KEY_F12: 24,
}

// termKeys は修飾キーの状態。
type termKeys struct {
	shift, ctrl, alt int  // 押している数
	oneCtrl, oneAlt  bool // タッチの Ctrl、Alt（次の 1 キーだけ）

	lastUp   evdev.EvCode          // 最後に離したキー（記号の押し直しを見分ける）
	lastUpAt time.Time             // その時刻
	ghost    map[evdev.EvCode]bool // 記号の押し直しで届いた「押す」（指は同じキーのまま。文字にしない）
}

// symbolGhost は、記号の押し直しとみなす、離してから押すまでの時間の上限。
// カーネルは同じ割り込みの中で「離す」と「押す」を続けて送るので、ふつうは 1 ミリ秒もかからない。
// 人が 2 つのキーを続けて打つときは、これよりずっと長い
const symbolGhost = 15 * time.Millisecond

// symbolPairs は、記号を押しながらのときと、そうでないときとで、同じ物理キーが出すキーコードの組（両方向）。
// keymap_pwsh2.go の表から作る（Q と KEY_1、G と KEY_BACKSLASH など）。
var symbolPairs = func() map[evdev.EvCode]evdev.EvCode {
	m := map[evdev.EvCode]evdev.EvCode{}
	for _, k := range pwsh2Keymap().Keys {
		a, okA := evdev.KEYFromString[k.Code]
		b, okB := evdev.KEYFromString[k.Symbol]
		if okA && okB && a != b {
			m[a], m[b] = b, a
		}
	}
	return m
}()

func isModifier(code evdev.EvCode) bool {
	switch code {
	case evdev.KEY_LEFTSHIFT, evdev.KEY_RIGHTSHIFT, evdev.KEY_LEFTCTRL, evdev.KEY_RIGHTCTRL,
		evdev.KEY_LEFTALT, evdev.KEY_RIGHTALT:
		return true
	}
	return false
}

// reset は修飾キーの状態を消す（端末モードに入るとき、抜けるとき）。
func (k *termKeys) reset() { *k = termKeys{} }

// event はキーのイベント（value：1 押す、0 離す）を処理する。t はイベントを受け取った時刻。
// カーネルのオートリピート（value 2）は使わない（Brain は 250 ミリ秒で繰り返し始め、パスワードを打つときに
// 気づかないまま同じ文字が入る）。繰り返しは termmode.go が、長めの間を置いて自分で行う。
//
// 記号の押し直し：Brain のドライバは、キーを押したまま「記号」を押す・離すと、次にそのキーが変わったときに
// 前のコードの「離す」と新しいコードの状態を送る。ソースでは新しく「押す」を送ることはないが、念のため、
// 離した直後（symbolGhost 以内）にその組のキーが押されたら、同じ指のままとみなして文字にしない。
func (k *termKeys) event(code evdev.EvCode, value int32, appCursor bool, t time.Time) termKeyResult {
	if isModifier(code) {
		d := map[int32]int{1: 1, 0: -1}[value]
		switch code {
		case evdev.KEY_LEFTSHIFT, evdev.KEY_RIGHTSHIFT:
			k.shift = max(k.shift+d, 0)
		case evdev.KEY_LEFTCTRL, evdev.KEY_RIGHTCTRL:
			k.ctrl = max(k.ctrl+d, 0)
		case evdev.KEY_LEFTALT, evdev.KEY_RIGHTALT:
			k.alt = max(k.alt+d, 0)
		}
		return termKeyResult{}
	}
	switch value {
	case 0:
		delete(k.ghost, code)
		k.lastUp, k.lastUpAt = code, t
		return termKeyResult{}
	case 1:
		if p, ok := symbolPairs[code]; ok && p == k.lastUp && t.Sub(k.lastUpAt) < symbolGhost {
			if k.ghost == nil {
				k.ghost = map[evdev.EvCode]bool{}
			}
			k.ghost[code] = true
			k.lastUp = 0
			return termKeyResult{}
		}
		k.lastUp = 0
	default:
		return termKeyResult{}
	}
	ctrl, alt, shift := k.ctrl > 0 || k.oneCtrl, k.alt > 0 || k.oneAlt, k.shift > 0
	if value == 1 {
		k.oneCtrl, k.oneAlt = false, false
	}
	// 抜ける：文字切り替え + 調べる/戻る（本体の Alt と Esc。タッチの Alt では抜けない）
	if code == evdev.KEY_ESC && k.alt > 0 {
		if value == 1 {
			return termKeyResult{exit: true}
		}
		return termKeyResult{}
	}
	// Shift + PageUp/PageDown：履歴を見る（xterm と同じ）
	if shift && !ctrl && !alt && (code == evdev.KEY_PAGEUP || code == evdev.KEY_PAGEDOWN) {
		if code == evdev.KEY_PAGEUP {
			return termKeyResult{scroll: 1}
		}
		return termKeyResult{scroll: -1}
	}
	return termKeyResult{out: keyBytes(code, shift, ctrl, alt, appCursor)}
}

// keyBytes は、キーと修飾キーの組み合わせを、端末に送るバイト列にする（xterm と同じ）。
func keyBytes(code evdev.EvCode, shift, ctrl, alt, appCursor bool) []byte {
	mod := 1
	if shift {
		mod += 1
	}
	if alt {
		mod += 2
	}
	if ctrl {
		mod += 4
	}
	var out []byte
	switch code {
	case evdev.KEY_ENTER, evdev.KEY_KPENTER:
		out = []byte{'\r'}
	case evdev.KEY_BACKSPACE:
		out = []byte{0x7f}
		if ctrl {
			out = []byte{0x08}
		}
	case evdev.KEY_TAB:
		if shift {
			return []byte("\x1b[Z")
		}
		out = []byte{'\t'}
	case evdev.KEY_ESC:
		out = []byte{0x1b}
	}
	if out != nil {
		if alt {
			return append([]byte{0x1b}, out...)
		}
		return out
	}
	if f, ok := csiKeys[code]; ok {
		switch {
		case mod > 1:
			return []byte("\x1b[1;" + strconv.Itoa(mod) + string(f))
		case appCursor:
			return []byte{0x1b, 'O', f}
		}
		return []byte{0x1b, '[', f}
	}
	if n, ok := tildeKeys[code]; ok {
		s := "\x1b[" + strconv.Itoa(n)
		if mod > 1 {
			s += ";" + strconv.Itoa(mod)
		}
		return []byte(s + "~")
	}
	ch, ok := usKeys[code]
	if !ok {
		return nil
	}
	c := ch[0]
	if shift {
		c = ch[1]
	}
	if ctrl {
		switch {
		case c >= 'a' && c <= 'z':
			c -= 'a' - 1
		case c >= '@' && c <= '_': // @ A-Z [ \ ] ^ _
			c -= '@'
		case c == ' ' || c == '2':
			c = 0
		case c == '/' || c == '-' || c == '7':
			c = 0x1f
		case c == '3':
			c = 0x1b
		case c == '4':
			c = 0x1c
		case c == '5':
			c = 0x1d
		case c == '6' || c == '~' || c == '`':
			c = 0x1e
		case c == '8' || c == '?':
			c = 0x7f
		}
	}
	if alt {
		return []byte{0x1b, c}
	}
	return []byte{c}
}

// ---------- タッチのキー（端末モードの画面の下の 2 段） ----------

// termPadKey はタッチのキー 1 つ。
type termPadKey struct {
	Label string
	Send  string // 送るバイト列
	Mod   string // "ctrl" か "alt"（次の 1 キーだけ）
}

// termPadPages は、タッチのキーのページ。1 ページは 12 列 × 2 段。◀ ▶ のソフトキーで切り替える。
// 1 ページ目は、本体では「記号」とシフトを同時に押す必要がある、シェルでよく使う記号。
// 2 ページ目は、括弧と、本体にない（押しにくい）操作のキー。
var termPadPages = []struct {
	Name string
	Keys [2][12]termPadKey
}{
	{"記号", [2][12]termPadKey{
		{pk("|"), pk("~"), pk("\\"), pk("/"), pk("-"), pk("_"), pk("="), pk("+"), pk("*"), pk("&"), pk(";"), pk(":")},
		{pk("$"), pk("#"), pk("^"), pk("%"), pk("@"), pk("!"), pk("?"), pk("'"), pk("\""), pk("`"), pk("<"), pk(">")},
	}},
	{"操作", [2][12]termPadKey{
		{{Label: "Esc", Send: "\x1b"}, {Label: "Tab", Send: "\t"}, pk("("), pk(")"), pk("["), pk("]"), pk("{"), pk("}"),
			{Label: "Home", Send: "\x1b[H"}, {Label: "End", Send: "\x1b[F"}, {Label: "PgUp", Send: "\x1b[5~"}, {Label: "PgDn", Send: "\x1b[6~"}},
		{{Label: "^C", Send: "\x03"}, {Label: "^D", Send: "\x04"}, {Label: "^Z", Send: "\x1a"}, {Label: "^L", Send: "\x0c"},
			{Label: "^R", Send: "\x12"}, {Label: "Ctrl", Mod: "ctrl"}, {Label: "Alt", Mod: "alt"},
			{Label: "←", Send: "\x1b[D"}, {Label: "↓", Send: "\x1b[B"}, {Label: "↑", Send: "\x1b[A"}, {Label: "→", Send: "\x1b[C"}, {Label: "Del", Send: "\x1b[3~"}},
	}},
}

func pk(s string) termPadKey { return termPadKey{Label: s, Send: s} }

// padBytes は、タッチのキーで送るバイト列。sticky の Ctrl と Alt を当てる（当てたら消す）。
// 矢印は、端末がアプリケーションのカーソルキーの形を頼んでいれば ESC O の形にする。
func (k *termKeys) padBytes(key termPadKey, appCursor bool) []byte {
	s := []byte(key.Send)
	if appCursor && len(s) == 3 && s[0] == 0x1b && s[1] == '[' && s[2] >= 'A' && s[2] <= 'D' {
		s = []byte{0x1b, 'O', s[2]}
	}
	if k.oneCtrl && len(s) == 1 {
		c := s[0]
		switch {
		case c >= 'a' && c <= 'z':
			s = []byte{c - 'a' + 1}
		case c >= '@' && c <= '_':
			s = []byte{c - '@'}
		case c == '?':
			s = []byte{0x7f}
		case c == ' ':
			s = []byte{0}
		}
	}
	if k.oneAlt || k.alt > 0 {
		s = append([]byte{0x1b}, s...)
	}
	k.oneCtrl, k.oneAlt = false, false
	return s
}
