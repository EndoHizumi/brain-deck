package main

import (
	"path/filepath"
	"syscall"
)

const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)

// settingsPort は Brain の設定用のポートを返す。
// macOS では、USB の CDC ACM は /dev/cu.usbmodem<シリアル番号><番号> になる。
// コンソール用と設定用は、名前の末尾の番号で見分ける（ports.go）。
func settingsPort() (string, error) {
	ps, _ := filepath.Glob("/dev/cu.usbmodem*")
	return pickUsbmodem(ps)
}

const portHint = "/dev/cu.usbmodem0123456789* がありません（ls /dev/cu.usbmodem* で確かめてください）。USB ケーブルと、Brain の起動を確かめてください"

// holders は、ほかに開いているプロセスを返す。macOS では調べない（/proc がない）。
// 設定 GUI（Chrome）は TIOCEXCL を使うので、開くときの EBUSY で分かる。
func holders(path string) []string { return nil }

const permissionHint = "ポートを開く権限がありません"
