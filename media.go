package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------- メディアキー（HID の Consumer Control） ----------
//
// メディアキーは、キーボードとマウスの形（usb_mode: mouse）の HID に、レポート ID 3 で入れる（hid.go の hidDescCombo）。
// ブートキーボードの形（起動したとき）は、BIOS や UEFI で使えるように、ディスクリプタを標準のままにしているので、
// メディアキーは送れない。そのときにメディアキーのセルを押しても何も送らず、-v のログに出す。
// 画面では、セルの下の小さな文字を「マウスオフでは無効」にする（display.go の drawCell）。
//
// レポートは 5 バイト：ID、押している Consumer の番号 2 つ（16 ビット、リトルエンディアン。0 は押していない）。
// 押している入力（キー、セル）ごとに覚え、新しく押したものから 2 つまで送る。
// 送れなかった状態は、State.retryLoop で送り直す（離したのが届かず押しっぱなしになるのを防ぐ）。

// mediaSlots は、1 つのレポートで同時に送れるメディアキーの数（hidDescCombo の Report Count）。
const mediaSlots = 2

// mediaAction は、メディアキー 1 つ（media: の値）。
type mediaAction struct {
	Usage  uint16 // Consumer のページ（0x0C）の番号
	Pretty string // 画面に出す名前
}

// mediaActions は、media: に書ける名前。名前と番号は HID Usage Tables の Consumer のページ（0x0C）。
var mediaActions = map[string]mediaAction{
	"play_pause":      {0xcd, "再生/停止"},
	"stop":            {0xb7, "停止"},
	"next":            {0xb5, "次の曲"},
	"previous":        {0xb6, "前の曲"},
	"fast_forward":    {0xb3, "早送り"},
	"rewind":          {0xb4, "巻き戻し"},
	"volume_up":       {0xe9, "音量+"},
	"volume_down":     {0xea, "音量-"},
	"mute":            {0xe2, "ミュート"},
	"brightness_up":   {0x6f, "明るさ+"},
	"brightness_down": {0x70, "明るさ-"},
	"calculator":      {0x192, "電卓"},
	"browser":         {0x196, "ブラウザ"},
	"mail":            {0x18a, "メール"},
}

// mediaActionNames は、エラーのメッセージと文書に出す順。
var mediaActionNames = []string{"play_pause", "stop", "next", "previous", "fast_forward", "rewind",
	"volume_up", "volume_down", "mute", "brightness_up", "brightness_down", "calculator", "browser", "mail"}

// mediaOffLabel は、メディアキーを送れない形（ブートキーボード）のときに、セルの下に出す文字。
const mediaOffLabel = "マウスオフでは無効"

func parseMedia(s string) (mediaAction, error) {
	a, ok := mediaActions[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return a, fmt.Errorf("unknown media key %q (%s)", s, strings.Join(mediaActionNames, ", "))
	}
	return a, nil
}

// Media はメディアキーのレポートを送る。nil でもよい（何もしない）。
type Media struct {
	mu    sync.Mutex
	hid   hidOut
	id    byte
	held  []mediaHeld // 押した順
	dirty bool        // 今の状態が PC に届いていない
	on    atomic.Bool // id が 0 でない
}

type mediaHeld struct {
	src   string
	usage uint16
}

// NewMedia は、レポート ID が id のメディアキーを作る。id が 0（ガジェットにメディアキーがない）なら何も送らない。
func NewMedia(hid hidOut, id byte) *Media {
	m := &Media{hid: hid, id: id}
	m.on.Store(id != 0)
	return m
}

// Available は、PC にメディアキーを送れるか。
func (m *Media) Available() bool { return m != nil && m.on.Load() }

// Press は、入力 src でメディアキーを押す。送れない形なら、何もしない（ログに出す）。
func (m *Media) Press(src string, a mediaAction) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.id == 0 {
		vlogf("media: %s (0x%03x) not sent: the USB is the boot keyboard (no media keys); turn the mouse on with usb_mode", a.Pretty, a.Usage)
		return
	}
	m.remove(src)
	m.held = append(m.held, mediaHeld{src, a.Usage})
	m.send()
	hostWaker.Request()
}

// Release は、入力 src で押したメディアキーを離す。
func (m *Media) Release(src string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remove(src) {
		m.send()
	}
}

// ReleaseAll は、すべて離す（終了、設定の再読み込み、USB の付け直し）。
func (m *Media) ReleaseAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.held) > 0 || m.dirty {
		m.held = nil
		m.send()
	}
}

// reset は、USB を付け直したあとに、レポート ID を変えて、押している状態を忘れる。m.mu を持って呼ぶ
// （付け直しのあいだは、usbmode.go が m.mu を持ち、レポートを書かせない）。
func (m *Media) reset(id byte) {
	m.id = id
	m.on.Store(id != 0)
	m.held = nil
	m.dirty = id != 0 // 付け直したあとに、何も押していない状態を送る
}

// retry は、届いていない状態を送り直す（State.retryLoop から）。
func (m *Media) retry() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dirty {
		m.send()
	}
}

// m.mu を持って呼ぶ
func (m *Media) remove(src string) bool {
	for i, h := range m.held {
		if h.src == src {
			m.held = append(m.held[:i], m.held[i+1:]...)
			return true
		}
	}
	return false
}

// report は、今のレポート。新しく押したものから mediaSlots 個まで入れる。m.mu を持って呼ぶ。
func (m *Media) report() []byte {
	r := make([]byte, 1+2*mediaSlots)
	r[0] = m.id
	for i := 0; i < mediaSlots && i < len(m.held); i++ {
		u := m.held[len(m.held)-1-i].usage
		r[1+2*i], r[2+2*i] = byte(u), byte(u>>8)
	}
	return r
}

// m.mu を持って呼ぶ
func (m *Media) send() {
	if m.id == 0 {
		m.dirty = false
		return
	}
	r := m.report()
	err := m.hid.Write(r)
	m.dirty = err != nil
	if err != nil {
		vlogf("media report % x (not sent: %v)", r, err)
		return
	}
	vlogf("media report % x", r)
}

// logMediaOff は、設定にメディアキーがあるのに送れない形のとき、起動したときに一度知らせる。
func logMediaOff(km *Keymap, l HIDLayout) {
	if l.MediaID == 0 && km.usesMedia() {
		log.Printf("media: the config uses media keys (media:), but the USB is the boot keyboard; they work only with the mouse on (usb_mode, brain-deck usb-mode mouse, or HID_MOUSE=1 in /etc/lefthand/gadget.env)")
	}
}
