package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)

// settingsPort は Brain の設定用のポートを返す。
// /dev/ttyACM の番号は、つなぎ直すと変わることがあるので、/dev/serial/by-id/ の名前で探す。
// コンソール用（-if03）と設定用（-if05）は、名前のインターフェイス番号で見分ける（ports.go）。
func settingsPort() (string, error) {
	ps, _ := filepath.Glob("/dev/serial/by-id/*")
	return pickByID(ps)
}

const portHint = "ls /dev/serial/by-id/ で、Brain のポート（usb-SHARP_Brain_…-if03 と -if05）があるか確かめてください。USB ケーブルと、Brain の起動も確かめてください"

// holders は、path（シンボリックリンクでもよい）を開いている、自分以外のプロセスを返す。
// /proc から見えるのは、同じユーザーのプロセスだけ（root で実行したときはすべて）。
func holders(path string) []string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	dirs, _ := filepath.Glob("/proc/[0-9]*/fd")
	var out []string
	for _, d := range dirs {
		pid, _ := strconv.Atoi(strings.Split(d, "/")[2])
		if pid == self {
			continue
		}
		fds, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if l, err := os.Readlink(filepath.Join(d, fd.Name())); err == nil && l == real {
				comm, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
				out = append(out, strings.TrimSpace(string(comm))+" (pid "+strconv.Itoa(pid)+")")
				break
			}
		}
	}
	return out
}

const permissionHint = "ポートを開く権限がありません。実行するユーザーを dialout グループに入れてください：\n" +
	"  sudo usermod -aG dialout $USER\n" +
	"そのあと、ログインし直してください（cron から使うときは、cron のデーモンを再起動するか、PC を再起動）"
