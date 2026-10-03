package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// ---------- 仮想端末（VT） ----------
//
// デーモン専用の VT に切り替えて KD_GRAPHICS にすると、カーネルのコンソール
// （fbcon）はその VT の画面を描かなくなる。元の VT（ly や getty）は自分の VT が
// 表示されていないあいだ画面に触れないので、取り合いが起きない。
// VT_PROCESS モードにして、ほかのプロセスが VT を切り替えたときは描画を止める。

const (
	ioctlVT_OPENQRY     = 0x5600
	ioctlVT_SETMODE     = 0x5602
	ioctlVT_GETSTATE    = 0x5603
	ioctlVT_RELDISP     = 0x5605
	ioctlVT_ACTIVATE    = 0x5606
	ioctlVT_WAITACTIVE  = 0x5607
	ioctlVT_DISALLOCATE = 0x5608
	ioctlKDSETMODE      = 0x4B3A

	vtAuto    = 0
	vtProcess = 1
	vtAckAcq  = 2
	kdText    = 0
	kdGraph   = 1

	// 異常終了したときに ExecStopPost（lefthand -restore-console）が読む
	vtStateFile = "/run/lefthand-vt"
)

type vtMode struct {
	mode, waitv           int8
	relsig, acqsig, frsig int16
}

type vtStat struct{ active, signal, state uint16 }

type VT struct {
	num, orig int
	fd        int // /dev/ttyN
	tty0      int
}

func getVTState(tty0 int) (vtStat, error) {
	var st vtStat
	err := ioctl(tty0, ioctlVT_GETSTATE, uintptr(unsafe.Pointer(&st)))
	return st, err
}

// pickVT は使われていない VT を選ぶ。logind が自動で getty を起こす
// tty1〜6 を避けて、8 以上を使う。
func pickVT(tty0 int, st vtStat) (int, error) {
	for n := 8; n < 16; n++ {
		if st.state&(1<<n) == 0 {
			return n, nil
		}
	}
	var n int32
	if err := ioctl(tty0, ioctlVT_OPENQRY, uintptr(unsafe.Pointer(&n))); err != nil {
		return 0, fmt.Errorf("VT_OPENQRY: %w", err)
	}
	if n <= 0 {
		return 0, errors.New("no free VT")
	}
	return int(n), nil
}

// waitActive は VT_WAITACTIVE を時間制限つきで待つ。
// 前の VT を持つプロセスが切り替えを許さないと、ioctl が戻らないため。
func waitActive(tty0, n int, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		ioctl(tty0, ioctlVT_WAITACTIVE, uintptr(n))
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// OpenVT は専用 VT を確保して KD_GRAPHICS にする（まだ切り替えない）。
// relsig/acqsig は VT_PROCESS で受け取るシグナル。want が 0 なら自動で選ぶ。
func OpenVT(want int, relsig, acqsig syscall.Signal) (*VT, error) {
	// 前回の異常終了で VT が残っていたら、先に戻しておく
	restoreConsole()

	tty0, err := syscall.Open("/dev/tty0", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	st, err := getVTState(tty0)
	if err != nil {
		syscall.Close(tty0)
		return nil, fmt.Errorf("VT_GETSTATE: %w", err)
	}
	n := want
	if n == 0 {
		if n, err = pickVT(tty0, st); err != nil {
			syscall.Close(tty0)
			return nil, err
		}
	}
	fd, err := syscall.Open(fmt.Sprintf("/dev/tty%d", n), syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		syscall.Close(tty0)
		return nil, err
	}
	vt := &VT{num: n, orig: int(st.active), fd: fd, tty0: tty0}
	if vt.orig == n {
		vt.orig = 1
	}
	os.WriteFile(vtStateFile, []byte(fmt.Sprintf("%d %d\n", vt.num, vt.orig)), 0o644)

	// 表示されていない VT のモード変更は記録されるだけなので、切り替え前に設定する。
	// こうすると、切り替えた瞬間に fbcon が描くこともない
	if err := ioctl(fd, ioctlKDSETMODE, kdGraph); err != nil {
		vt.Close()
		return nil, fmt.Errorf("KDSETMODE: %w", err)
	}
	m := vtMode{mode: vtProcess, relsig: int16(relsig), acqsig: int16(acqsig)}
	if err := ioctl(fd, ioctlVT_SETMODE, uintptr(unsafe.Pointer(&m))); err != nil {
		vt.Close()
		return nil, fmt.Errorf("VT_SETMODE: %w", err)
	}
	return vt, nil
}

// Activate は専用 VT に切り替える。
func (vt *VT) Activate() error {
	if err := ioctl(vt.tty0, ioctlVT_ACTIVATE, uintptr(vt.num)); err != nil {
		return fmt.Errorf("VT_ACTIVATE %d: %w", vt.num, err)
	}
	if !waitActive(vt.tty0, vt.num, 3*time.Second) {
		return fmt.Errorf("VT %d did not become active", vt.num)
	}
	return nil
}

func (vt *VT) IsActive() bool { return vt.IsActiveNum(vt.num) }

// IsActiveNum は tty0 を開き直さずに、表示中の VT が n かを調べる。
func (vt *VT) IsActiveNum(n int) bool {
	tty0 := vt.tty0
	if tty0 < 0 {
		return false
	}
	st, err := getVTState(tty0)
	return err == nil && int(st.active) == n
}

// Release はほかの VT への切り替えを許可する（VT_PROCESS の release シグナルへの応答）。
func (vt *VT) Release() { ioctl(vt.fd, ioctlVT_RELDISP, 1) }

// AckAcquire は切り替えを受け入れたことを知らせる。
func (vt *VT) AckAcquire() { ioctl(vt.fd, ioctlVT_RELDISP, vtAckAcq) }

// Close は元の VT とテキストモードに戻す。何度呼んでもよい。
//
// カーネルは KD_GRAPHICS かつ VT_AUTO の VT からの切り替えを無視する。
// そこで VT_PROCESS のまま切り替えを要求し、release に自分で許可を返す
// （X サーバーと同じ手順）。切り替えが済んでからテキストモードに戻すので、
// 空のコンソールが一瞬映ることもない。
func (vt *VT) Close() {
	if vt.fd < 0 {
		return
	}
	if vt.IsActive() {
		if err := ioctl(vt.tty0, ioctlVT_ACTIVATE, uintptr(vt.orig)); err != nil {
			log.Printf("VT_ACTIVATE %d: %v", vt.orig, err)
		}
		// 切り替えはカーネルの workqueue で非同期に進むので、要求が届くまで許可を送り続ける
		deadline := time.Now().Add(2 * time.Second)
		for vt.IsActive() && time.Now().Before(deadline) {
			ioctl(vt.fd, ioctlVT_RELDISP, 1) // 要求がまだ届いていなければ EINVAL
			time.Sleep(5 * time.Millisecond)
		}
		if vt.IsActive() {
			// それでも切り替わらなければ、テキストに戻して自動モードで切り替える
			log.Printf("VT %d did not release, forcing text mode", vt.num)
			vt.forceSwitch()
		}
	}
	m := vtMode{mode: vtAuto}
	ioctl(vt.fd, ioctlVT_SETMODE, uintptr(unsafe.Pointer(&m)))
	ioctl(vt.fd, ioctlKDSETMODE, kdText)
	syscall.Close(vt.fd)
	vt.fd = -1
	// close の後始末はカーネル内で非同期に進むので、しばらくは EBUSY になる
	for i := 0; ; i++ {
		err := ioctl(vt.tty0, ioctlVT_DISALLOCATE, uintptr(vt.num))
		if err == nil || i >= 50 {
			if err != nil {
				vlogf("VT_DISALLOCATE %d: %v", vt.num, err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if vt.IsActive() {
		log.Printf("warning: tty%d is still the active VT", vt.num)
	}
	syscall.Close(vt.tty0)
	vt.tty0 = -1
	os.Remove(vtStateFile)
}

// forceSwitch はテキストモード・自動モードに戻してから元の VT に切り替える。
func (vt *VT) forceSwitch() {
	m := vtMode{mode: vtAuto}
	ioctl(vt.fd, ioctlVT_SETMODE, uintptr(unsafe.Pointer(&m)))
	ioctl(vt.fd, ioctlKDSETMODE, kdText)
	if err := ioctl(vt.tty0, ioctlVT_ACTIVATE, uintptr(vt.orig)); err != nil {
		log.Printf("VT_ACTIVATE %d: %v", vt.orig, err)
	} else if !waitActive(vt.tty0, vt.orig, 2*time.Second) {
		log.Printf("VT %d did not become active", vt.orig)
	}
}

// restoreConsole は、異常終了で残った専用 VT を状態ファイルから元に戻す。
// lefthand -restore-console（systemd の ExecStopPost）と起動時に呼ぶ。
func restoreConsole() {
	b, err := os.ReadFile(vtStateFile)
	if err != nil {
		return
	}
	var num, orig int
	if _, err := fmt.Sscanf(string(b), "%d %d", &num, &orig); err != nil || num <= 0 || orig <= 0 {
		os.Remove(vtStateFile)
		return
	}
	tty0, err := syscall.Open("/dev/tty0", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		log.Printf("restore console: %v", err)
		return
	}
	fd, err := syscall.Open(fmt.Sprintf("/dev/tty%d", num), syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		syscall.Close(tty0)
		log.Printf("restore console: %v", err)
		return
	}
	vt := &VT{num: num, orig: orig, fd: fd, tty0: tty0}
	log.Printf("restoring console: tty%d -> tty%d", num, orig)
	// 持ち主のプロセスはもういないので、release の応答は待たない
	if vt.IsActive() {
		vt.forceSwitch()
	}
	vt.Close()
}
