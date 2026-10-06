package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)

// candidatePorts は Brain のシリアルポートの候補を、試す順に返す。
// /dev/ttyACM の番号は、つなぎ直すと変わることがあるので、/dev/serial/by-id/ の名前で探す。
// Brain は ACM を 2 つ持ち（コンソール用と設定 GUI 用）、どちらも名前に Brain が入る。
// 設定 GUI 用はインターフェイス番号が大きい（-if05）ので、名前の逆順に試す。
func candidatePorts() []string {
	ps, _ := filepath.Glob("/dev/serial/by-id/*Brain*")
	sort.Sort(sort.Reverse(sort.StringSlice(ps)))
	return ps
}

const portHint = "/dev/serial/by-id/ に Brain のポートがありません。USB ケーブルと、Brain の起動を確かめてください"

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
