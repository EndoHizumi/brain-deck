package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ---------- HID の構成（キーボードとマウス） ----------
//
// Brain の USB コントローラ（ci_hdrc）は IN のエンドポイントが 7 本しかなく、
// NCM（2 本）、HID（1 本）、ACM 2 つ（4 本）ですべて使っている。マウスのために別の HID のファンクションを
// 足すとエンドポイントが足りず、ガジェット全体（NCM も）がつながらなくなる。
// そのため、今の HID（hid.usb0、/dev/hidg0）1 つの中に、レポート ID でキーボード（1）とマウス（2）を入れる。
// その形はブートキーボードではなく、BIOS や UEFI の画面では使えないので、起動したときはキーボードだけ
// （ブートキーボード）の形にし、マウスを使うときだけ切り替える（usbmode.go）。
//
// デーモンは、起動したときに configfs の report_desc を読んで、どちらの形かを決める（detectHID）。
// 形を間違えると、PC には別のキーやマウスの動きとして届くので、分からないときはキーボードだけの形として扱う。

// hidKeyMax は、キーボードのレポートで送れるキーの番号の上限（F24、0x73）。
// HID の仕様書の例のブートキーボードは 0x65（Keyboard Application）までで、そのままでは F13（0x68）以降を
// PC が捨てる（Linux の hid-input は、Logical Maximum を超えた値を無視する）。
// 符号付きの 1 バイト（0x25）で書けるように 0x7f 以下にする。BIOS や UEFI はブートプロトコル（決まった 8 バイト）で
// 読み、このディスクリプタは読まないので、上限を上げても BIOS や UEFI での入力は変わらない。
const hidKeyMax = 0x73

// hidDescKeyboard は、キーボードだけの形（ブートキーボード、8 バイトのレポート）。gadget-setup.sh の KBD_DESC と同じ。
var hidDescKeyboard = []byte{
	0x05, 0x01, 0x09, 0x06, 0xa1, 0x01, 0x05, 0x07, 0x19, 0xe0, 0x29, 0xe7, 0x15, 0x00, 0x25, 0x01,
	0x75, 0x01, 0x95, 0x08, 0x81, 0x02, 0x95, 0x01, 0x75, 0x08, 0x81, 0x03, 0x95, 0x05, 0x75, 0x01,
	0x05, 0x08, 0x19, 0x01, 0x29, 0x05, 0x91, 0x02, 0x95, 0x01, 0x75, 0x03, 0x91, 0x03, 0x95, 0x06,
	0x75, 0x08, 0x15, 0x00, 0x25, hidKeyMax, 0x05, 0x07, 0x19, 0x00, 0x29, hidKeyMax, 0x81, 0x00, 0xc0,
}

const (
	hidKeyboardID = 1 // キーボードのレポート ID（キーボードとマウスの形）
	hidMouseID    = 2 // マウスのレポート ID
	hidMediaID    = 3 // メディアキー（Consumer Control）のレポート ID（media.go）
	hidPenID      = 4 // ペン（デジタイザ）のレポート ID（pen.go）
)

// hidDescCombo は、キーボード（レポート ID 1、9 バイト）とマウス（レポート ID 2、6 バイト）の形。
// gadget-setup.sh の COMBO_DESC と同じ。
// マウスは、ボタン 3 つ、X、Y、縦のホイール、横のホイール（AC Pan）。移動とホイールは -127〜127 の相対値。
var hidDescCombo = []byte{
	// キーボード（hidDescKeyboard に Report ID 1 を入れたもの）
	0x05, 0x01, 0x09, 0x06, 0xa1, 0x01, 0x85, hidKeyboardID, 0x05, 0x07, 0x19, 0xe0, 0x29, 0xe7, 0x15, 0x00,
	0x25, 0x01, 0x75, 0x01, 0x95, 0x08, 0x81, 0x02, 0x95, 0x01, 0x75, 0x08, 0x81, 0x03, 0x95, 0x05,
	0x75, 0x01, 0x05, 0x08, 0x19, 0x01, 0x29, 0x05, 0x91, 0x02, 0x95, 0x01, 0x75, 0x03, 0x91, 0x03,
	0x95, 0x06, 0x75, 0x08, 0x15, 0x00, 0x25, hidKeyMax, 0x05, 0x07, 0x19, 0x00, 0x29, hidKeyMax, 0x81, 0x00,
	0xc0,
	// マウス
	0x05, 0x01, 0x09, 0x02, 0xa1, 0x01, 0x85, hidMouseID, 0x09, 0x01, 0xa1, 0x00,
	0x05, 0x09, 0x19, 0x01, 0x29, 0x03, 0x15, 0x00, 0x25, 0x01, 0x95, 0x03, 0x75, 0x01, 0x81, 0x02, // ボタン 3 つ
	0x95, 0x01, 0x75, 0x05, 0x81, 0x03, // 残りの 5 ビット
	0x05, 0x01, 0x09, 0x30, 0x09, 0x31, 0x09, 0x38, 0x15, 0x81, 0x25, 0x7f, 0x75, 0x08, 0x95, 0x03, 0x81, 0x06, // X、Y、ホイール
	0x05, 0x0c, 0x0a, 0x38, 0x02, 0x15, 0x81, 0x25, 0x7f, 0x75, 0x08, 0x95, 0x01, 0x81, 0x06, // 横のホイール
	0xc0, 0xc0,
	// メディアキー（Consumer Control）。Consumer の番号（0〜0x3ff）を 16 ビットで 2 つ（配列）
	0x05, 0x0c, 0x09, 0x01, 0xa1, 0x01, 0x85, hidMediaID,
	0x15, 0x00, 0x26, 0xff, 0x03, 0x19, 0x00, 0x2a, 0xff, 0x03, 0x75, 0x10, 0x95, mediaSlots, 0x81, 0x00,
	0xc0,
	// ペン（Digitizer の Stylus。pen.go）。アプリケーションは Digitizer（0x01）で、PC の画面の外のタブレット（INPUT_PROP_DIRECT なし）。
	// Pen（0x02）にすると、Linux は画面に重なったペン（INPUT_PROP_DIRECT）として扱う（2026-10-11 に PC で確かめた）。Tip Switch、In Range、X、Y（0〜32767 の絶対座標）、Tip Pressure（0〜1023）。
	// 物理の大きさは PW-SH2 の画面（5 型、108×65 mm。0.1 mm の単位）
	0x05, 0x0d, 0x09, 0x01, 0xa1, 0x01, 0x85, hidPenID, 0x09, 0x20, 0xa1, 0x00,
	0x09, 0x42, 0x09, 0x32, 0x15, 0x00, 0x25, 0x01, 0x75, 0x01, 0x95, 0x02, 0x81, 0x02, // Tip Switch、In Range
	0x95, 0x06, 0x81, 0x03, // 残りの 6 ビット
	0x05, 0x01, 0x26, 0xff, 0x7f, 0x75, 0x10, 0x95, 0x01, 0x55, 0x0e, 0x65, 0x11, 0x35, 0x00,
	0x46, 0x38, 0x04, 0x09, 0x30, 0x81, 0x02, // X（物理 108.0 mm）
	0x46, 0x8a, 0x02, 0x09, 0x31, 0x81, 0x02, // Y（物理 65.0 mm）
	0x45, 0x00, 0x65, 0x00, 0x55, 0x00,
	0x05, 0x0d, 0x09, 0x30, 0x26, 0xff, 0x03, 0x81, 0x02, // Tip Pressure
	0xc0, 0xc0,
}

// hidDescComboNoPen は、前の版（フェーズ G まで）のキーボードとマウスとメディアキーの形（ペンがない）。
var hidDescComboNoPen = hidDescCombo[:bytes.Index(hidDescCombo, []byte{0x05, 0x0d, 0x09, 0x01, 0xa1, 0x01})]

// hidDescComboNoMedia は、前の版のキーボードとマウスの形（メディアキーがない）。
var hidDescComboNoMedia = hidDescCombo[:bytes.Index(hidDescCombo, []byte{0xc0, 0xc0, 0x05, 0x0c, 0x09, 0x01})+2]

// legacyDesc は、前の版の gadget-setup.sh が書いたディスクリプタ（キーの番号の上限が 0x65）。
// デーモンだけを新しくして、ガジェットを作り直していない（Brain を再起動していない）ときも、レポートの形を間違えない。
// F13〜F24 は、ガジェットを作り直すまで PC に届かない。
func legacyDesc(d []byte) []byte {
	return bytes.ReplaceAll(d, []byte{0x25, hidKeyMax, 0x05, 0x07, 0x19, 0x00, 0x29, hidKeyMax}, []byte{0x25, 0x65, 0x05, 0x07, 0x19, 0x00, 0x29, 0x65})
}

// HIDLayout は、/dev/hidg0 に書くレポートの形。
type HIDLayout struct {
	KeyboardID byte   // 0 ならキーボードのレポートに ID を付けない（キーボードだけの形）
	MouseID    byte   // 0 ならマウスはない
	MediaID    byte   // 0 ならメディアキーはない
	PenID      byte   // 0 ならペンはない
	Source     string // どこで決めたか（ログ用）
}

// configfsGadgets は、USB ガジェットの configfs（テストで差し替える）。
var configfsGadgets = "/sys/kernel/config/usb_gadget"

// detectHID は、HID のデバイス（/dev/hidg0）に対応する configfs のファンクションの report_desc を読み、
// レポートの形を決める。分からなければ、キーボードだけの形（前からの形）とする。
func detectHID(dev string) HIDLayout {
	var st syscall.Stat_t
	if err := syscall.Stat(dev, &st); err != nil {
		return HIDLayout{Source: fmt.Sprintf("%s: %v (keyboard only)", dev, err)}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		return HIDLayout{Source: dev + " is not a character device (keyboard only)"}
	}
	rdev := uint64(st.Rdev)
	major := (rdev>>8)&0xfff | (rdev>>32)&^0xfff
	minor := rdev&0xff | (rdev>>12)&^0xff
	want := fmt.Sprintf("%d:%d", major, minor)
	fns, _ := filepath.Glob(filepath.Join(configfsGadgets, "*", "functions", "hid.*"))
	for _, fn := range fns {
		b, err := os.ReadFile(filepath.Join(fn, "dev"))
		if err != nil || strings.TrimSpace(string(b)) != want {
			continue
		}
		desc, err := os.ReadFile(filepath.Join(fn, "report_desc"))
		if err != nil {
			return HIDLayout{Source: fmt.Sprintf("%s: %v (keyboard only)", fn, err)}
		}
		return layoutOf(desc, fn)
	}
	return HIDLayout{Source: fmt.Sprintf("no configfs HID function for %s (%s) (keyboard only)", dev, want)}
}

// layoutOf は、report_desc の中身からレポートの形を決める。
func layoutOf(desc []byte, where string) HIDLayout {
	switch {
	case bytes.Equal(desc, hidDescCombo):
		return HIDLayout{KeyboardID: hidKeyboardID, MouseID: hidMouseID, MediaID: hidMediaID, PenID: hidPenID, Source: where + ": keyboard + mouse + media keys + pen"}
	case bytes.Equal(desc, hidDescComboNoPen):
		return HIDLayout{KeyboardID: hidKeyboardID, MouseID: hidMouseID, MediaID: hidMediaID, Source: where + ": keyboard + mouse + media keys (old descriptor: the pen tablet does not work until the gadget is set up again)"}
	case bytes.Equal(desc, hidDescKeyboard):
		return HIDLayout{Source: where + ": keyboard only (boot keyboard; usb_mode switches the mouse and the media keys on)"}
	case bytes.Equal(desc, legacyDesc(hidDescComboNoMedia)):
		return HIDLayout{KeyboardID: hidKeyboardID, MouseID: hidMouseID, Source: where + ": keyboard + mouse (old descriptor: F13-F24 and the media keys do not work until the gadget is set up again)"}
	case bytes.Equal(desc, legacyDesc(hidDescKeyboard)):
		return HIDLayout{Source: where + ": keyboard only (old descriptor: F13-F24 do not reach the PC until the gadget is set up again)"}
	}
	return HIDLayout{Source: where + ": unknown report descriptor (keyboard only)"}
}
