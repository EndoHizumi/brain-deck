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

// ---------- USB のネットワークの方式の切り替え（NCM ⇔ ECM ⇔ RNDIS） ----------
//
// 既定は NCM（gadget-setup.sh の NET_MODE=ncm）。Linux、macOS、Windows 10 以降の PC はこれで使える。
// Android のイーサネットテザリングや古い Windows のように NCM を持たないホストのために、RNDIS（と、カーネルに
// あれば ECM）に切り替えられる。どの方式もインターフェイスは 2 つなので、HID と ACM の番号（PC の -if03、-if05）は
// 変わらず、MAC も同じなので PC の usb0 の名前（enx8a158b443a01）も変わらない。
// 切り替えは USB の付け直しで、ネットワークのファンクションは作り直す（Brain の usb0 も一度消えて、でき直す）。
// 切り替えは起動し直すと元に戻る（起動したときの方式は /etc/lefthand/gadget.env の NET_MODE）。

const (
	netNCM   = "ncm"
	netECM   = "ecm"
	netRNDIS = "rndis"
)

// netKernelOption は、方式ごとの、カーネルの configfs のファンクションの設定の名前。
var netKernelOption = map[string]string{netNCM: "CONFIG_USB_CONFIGFS_NCM", netECM: "CONFIG_USB_CONFIGFS_ECM", netRNDIS: "CONFIG_USB_CONFIGFS_RNDIS"}

// procConfig は、カーネルの設定（テストで差し替える）。
var procConfig = "/proc/config.gz"

// detectNetMode は、ガジェットの構成にリンクしているネットワークのファンクションから、今の方式を返す。分からなければ空。
func detectNetMode() string {
	for _, m := range []string{netNCM, netECM, netRNDIS} {
		if l, _ := filepath.Glob(filepath.Join(configfsGadgets, "*", "configs", "*", m+".*")); len(l) > 0 {
			return m
		}
	}
	return ""
}

// NetMode は、今のネットワークの方式（ncm、ecm、rndis。分からなければ空）。
func (u *USBMode) NetMode() string {
	if u == nil || u.netMode == nil {
		return ""
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

// RequestNet は、ネットワークの方式を mode（ncm、ecm、rndis）に変える付け直しを始める。待たずに返る。
// すでにその方式なら、何もしない（changed が false）。HID の形と端末モードはそのまま。
func (u *USBMode) RequestNet(mode string) (changed bool, err error) {
	if u == nil {
		return false, errors.New("USB mode switching is not available")
	}
	switch mode {
	case netNCM, netECM, netRNDIS:
	default:
		return false, fmt.Errorf("mode must be %s, %s or %s", netNCM, netECM, netRNDIS)
	}
	if err := netModeSupported(mode); err != nil {
		return false, err
	}
	u.mu.Lock()
	if u.switching {
		u.mu.Unlock()
		return false, errUSBBusy
	}
	if u.NetMode() == mode {
		u.mu.Unlock()
		return false, nil
	}
	u.switching = true
	u.netWant = mode
	u.mu.Unlock()
	go func() {
		time.Sleep(usbSwitchDelay)
		u.apply(u.Mouse())
	}()
	return true, nil
}
