package main

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- USB のネットワークの方式の切り替え（NCM ⇔ ECM ⇔ RNDIS）と、相手から見た MAC ----------
//
// 既定は NCM（gadget-setup.sh の NET_MODE=ncm）。Linux、macOS、Windows 10 以降の PC はこれで使える。
// NCM を持たないホスト（古い Windows など）のために、RNDIS（と、カーネルにあれば ECM）に切り替えられる。
// どの方式もインターフェイスは 2 つなので、HID と ACM の番号（PC の -if03、-if05）は変わらない。
//
// 相手から見た Brain の MAC（host_addr）は 2 つから選ぶ（NET_MAC）。
//   pc    8a:15:8b:44:3a:01（既定。Brainux と同じ値。PC のネットワークの名前は enx8a158b443a01）
//   phone 88:15:8b:44:3a:01（「ローカルに割り当てた」印のない値）
// 相手の Linux のカーネル（usbnet）は、印のある MAC の機器を usb0、ない機器を eth0 と名付ける。PC は udev が
// MAC から名前を付け直すが、Android は付け直さず、イーサネットテザリングは eth0 のような名前しか扱わない。
// 2026-10-15、Xperia 1 IV（Android 15）で、NCM と phone の組み合わせでだけテザリングの対象になった
// （NCM と pc、RNDIS と pc、RNDIS と phone では、スマホから 1 つもパケットが来なかった）。
//
// 切り替えは USB の付け直しで、ネットワークのファンクションは作り直す（Brain の usb0 も一度消えて、でき直す）。
// 起動し直すと元に戻る（起動したときの値は /etc/lefthand/gadget.env の NET_MODE と NET_MAC）。

const (
	netNCM   = "ncm"
	netECM   = "ecm"
	netRNDIS = "rndis"

	netMACPC    = "pc"
	netMACPhone = "phone"
)

// netMACAddr は、NET_MAC ごとの host_addr（gadget-setup.sh と同じ）。
var netMACAddr = map[string]string{netMACPC: "8a:15:8b:44:3a:01", netMACPhone: "88:15:8b:44:3a:01"}

// netChoice は、ネットワークの方式と、相手から見た MAC（pc、phone）。空の項目は「今のまま」。
type netChoice struct {
	Mode string
	MAC  string
}

// netKernelOption は、方式ごとの、カーネルの configfs のファンクションの設定の名前。
var netKernelOption = map[string]string{netNCM: "CONFIG_USB_CONFIGFS_NCM", netECM: "CONFIG_USB_CONFIGFS_ECM", netRNDIS: "CONFIG_USB_CONFIGFS_RNDIS"}

// procConfig は、カーネルの設定（テストで差し替える）。
var procConfig = "/proc/config.gz"

// detectNetMode は、ガジェットの構成にリンクしているネットワークのファンクションから、今の方式と MAC を返す。
// 分からなければ空。MAC が pc でも phone でもなければ、その値をそのまま返す。
func detectNetMode() netChoice {
	for _, m := range []string{netNCM, netECM, netRNDIS} {
		l, _ := filepath.Glob(filepath.Join(configfsGadgets, "*", "configs", "*", m+".*"))
		if len(l) == 0 {
			continue
		}
		c := netChoice{Mode: m}
		// 構成のリンクはファンクションを指す。host_addr は最後に NUL が付く
		if b, err := os.ReadFile(filepath.Join(l[0], "host_addr")); err == nil {
			addr := strings.TrimSpace(strings.TrimRight(string(b), "\x00\n"))
			c.MAC = addr
			for k, v := range netMACAddr {
				if strings.EqualFold(addr, v) {
					c.MAC = k
				}
			}
		}
		return c
	}
	return netChoice{}
}

// NetMode は、今のネットワークの方式（ncm、ecm、rndis）と MAC（pc、phone）。分からなければ空。
func (u *USBMode) NetMode() netChoice {
	if u == nil || u.netMode == nil {
		return netChoice{}
	}
	return u.netMode()
}

// netModeSupported は、カーネルがその方式のファンクションを作れるか。カーネルの設定を読めなければ、作れるとする
// （作れなければ gadget-setup.sh が今の方式のままにする）。
func netModeSupported(mode string) error {
	f, err := os.Open(procConfig)
	if err != nil {
		return nil
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	opt := netKernelOption[mode]
	sc := bufio.NewScanner(z)
	for sc.Scan() {
		if sc.Text() == opt+"=y" || sc.Text() == opt+"=m" {
			return nil
		}
	}
	if sc.Err() != nil {
		return nil
	}
	return fmt.Errorf("this kernel cannot make %s (%s is not set); the kernel must be rebuilt", strings.ToUpper(mode), opt)
}

// RequestNet は、ネットワークの方式を mode（ncm、ecm、rndis）に、MAC を mac（pc、phone。空なら pc）に変える
// 付け直しを始める。待たずに返る。すでにそうなら、何もしない（changed が false）。HID の形と端末モードはそのまま。
func (u *USBMode) RequestNet(mode, mac string) (changed bool, err error) {
	if u == nil {
		return false, errors.New("USB mode switching is not available")
	}
	switch mode {
	case netNCM, netECM, netRNDIS:
	default:
		return false, fmt.Errorf("mode must be %s, %s or %s", netNCM, netECM, netRNDIS)
	}
	if mac == "" {
		mac = netMACPC
	}
	if _, ok := netMACAddr[mac]; !ok {
		return false, fmt.Errorf("mac must be %s or %s", netMACPC, netMACPhone)
	}
	if err := netModeSupported(mode); err != nil {
		return false, err
	}
	want := netChoice{Mode: mode, MAC: mac}
	u.mu.Lock()
	if u.switching {
		u.mu.Unlock()
		return false, errUSBBusy
	}
	if u.NetMode() == want {
		u.mu.Unlock()
		return false, nil
	}
	u.switching = true
	u.netWant = want
	u.mu.Unlock()
	go func() {
		time.Sleep(usbSwitchDelay)
		u.apply(u.Mouse())
	}()
	return true, nil
}
