package main

import (
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// ---------- i.MX28 の POWER のレジスタ（読むだけ） ----------
//
// PW-SH2 の Brainux のカーネルには、i.MX28 の電源と充電のドライバがない（POWER のブロックは電源を切る syscon だけ）。
// 充電しているかを知るため、HW_POWER_CHARGE を /dev/mem から読む（rtc.go と同じ。書かない）。
// 実機（2026-10-11）では CHARGE が 0x00010000：内蔵の充電器は止まっていて（PWD_BATTCHRG が 1）、充電の電流は 0。
// USB につないでいても電池は充電されず、電圧は下がり続けた（README の「充電」）。

const (
	powerBase             = 0x80044000
	powerMapSize          = 0x1000
	powerCharge           = 0x030
	powerChargePwdBattchg = 1 << 16
	powerChargeIMask      = 0x3f
)

// readPowerCharge は HW_POWER_CHARGE を読む（テストで差し替える）。
var readPowerCharge = func() (uint32, error) {
	f, err := os.OpenFile(rtcMemPath, os.O_RDONLY|syscall.O_SYNC, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	mem, err := syscall.Mmap(int(f.Fd()), powerBase, powerMapSize, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return 0, err
	}
	defer syscall.Munmap(mem)
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&mem[powerCharge]))), nil
}

// chargerOff は、HW_POWER_CHARGE で内蔵の充電器が止まっている（電流 0 か、電源が切ってある）か。
func chargerOff(charge uint32) bool {
	return charge&powerChargePwdBattchg != 0 || charge&powerChargeIMask == 0
}
