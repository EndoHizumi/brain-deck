package main

import (
	"path/filepath"
	"sort"
	"syscall"
)

const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)

// candidatePorts は Brain のシリアルポートの候補を、試す順に返す。
// macOS では、USB の CDC ACM は /dev/cu.usbmodem<シリアル番号><番号> になる。
// 名前では Brain かどうか分からないので、すべて試し、hello に答えたものを使う。
// 設定 GUI 用（インターフェイス番号が大きい）を先に試すため、名前の逆順にする。
func candidatePorts() []string {
	ps, _ := filepath.Glob("/dev/cu.usbmodem*")
	sort.Sort(sort.Reverse(sort.StringSlice(ps)))
	return ps
}

const portHint = "/dev/cu.usbmodem* がありません。USB ケーブルと、Brain の起動を確かめてください"

// holders は、ほかに開いているプロセスを返す。macOS では調べない（/proc がない）。
// 設定 GUI（Chrome）は TIOCEXCL を使うので、開くときの EBUSY で分かる。
func holders(path string) []string { return nil }

const permissionHint = "ポートを開く権限がありません"
