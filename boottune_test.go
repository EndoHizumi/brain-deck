package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// contrib/boot/boot-tune.sh を、偽の systemctl で動かす（PC のサービスには触れない）。
func TestBootTuneScript(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "units")
	// 偽の systemctl：units の「ユニット 状態」を読み書きし、呼ばれた操作を log に足す
	fake := filepath.Join(dir, "systemctl")
	os.WriteFile(fake, []byte(`#!/bin/sh
db=`+db+`
echo "$*" >> `+filepath.Join(dir, "log")+`
case $1 in
is-enabled) s=$(grep "^$2 " $db | cut -d' ' -f2); [ -n "$s" ] && echo $s || { echo not-found; exit 1; }; [ "$s" = enabled ] ;;
disable) sed -i "s/^$2 .*/$2 disabled/" $db; [ "$2" != e2scrub_reap.service ] ;; # 実機の e2scrub_reap は、止めても 1 を返した
enable) sed -i "s/^$2 .*/$2 enabled/" $db ;;
esac
`), 0o755)
	os.WriteFile(db, []byte("accounts-daemon.service enabled\nudisks2.service enabled\ne2scrub_reap.service enabled\ntriggerhappy.service enabled\ntriggerhappy.socket enabled\navahi-daemon.service enabled\navahi-daemon.socket enabled\nly.service enabled\nNetworkManager.service enabled\nNetworkManager-wait-online.service enabled\nNetworkManager-dispatcher.service enabled\nsystemd-timesyncd.service enabled\nnetworking.service enabled\napt-daily.timer enabled\napt-daily-upgrade.timer enabled\nssh.service enabled\nserial-getty@ttyGS0.service enabled\n"), 0o644)
	state := filepath.Join(dir, "etc", "boot-tune.state")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{"contrib/boot/boot-tune.sh"}, args...)...)
		cmd.Env = append(os.Environ(), "SYSTEMCTL="+fake, "BOOT_TUNE_STATE="+state, "BOOT_TUNE_NOROOT=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	units := func() string { b, _ := os.ReadFile(db); return string(b) }

	if out := run("plan"); !strings.Contains(out, "[段階 1] accounts") || strings.Contains(out, "ly.service") || strings.Contains(out, "NetworkManager") {
		t.Errorf("plan: 省略すると段階 1 だけ:\n%s", out)
	}
	if out := run("plan", "--all"); !strings.Contains(out, "ly.service") {
		t.Errorf("plan --all:\n%s", out)
	}
	run("apply")
	u := units()
	for _, want := range []string{"accounts-daemon.service disabled", "udisks2.service disabled", "triggerhappy.socket disabled", "avahi-daemon.service disabled", "ly.service enabled", "NetworkManager.service enabled"} {
		if !strings.Contains(u, want) {
			t.Errorf("after apply: want %q in\n%s", want, u)
		}
	}
	// 2 回目は記録にあるものを飛ばす（前の状態を disabled で上書きしない）
	run("apply")
	st, _ := os.ReadFile(state)
	if strings.Contains(string(st), "disabled") || strings.Count(string(st), "\n") != 7 {
		t.Errorf("state:\n%s", st)
	}
	// 段階 2：ネットにつなぐためのもの
	if out := run("plan", "2"); !strings.Contains(out, "[段階 2] nm") || !strings.Contains(out, "apt-daily.timer") || strings.Contains(out, "accounts") {
		t.Errorf("plan 2:\n%s", out)
	}
	run("apply", "2")
	u = units()
	for _, want := range []string{"NetworkManager.service disabled", "NetworkManager-wait-online.service disabled", "systemd-timesyncd.service disabled", "networking.service disabled", "apt-daily.timer disabled", "apt-daily-upgrade.timer disabled", "ly.service enabled"} {
		if !strings.Contains(u, want) {
			t.Errorf("after apply 2: want %q in\n%s", want, u)
		}
	}
	run("apply", "stage3")
	if !strings.Contains(units(), "ly.service disabled") {
		t.Error("apply 3")
	}
	if out := run("status"); !strings.Contains(out, "ly.service：前は enabled、今は disabled") {
		t.Errorf("status:\n%s", out)
	}
	// ssh とシリアルのログインは、どの段階でも止めない
	for _, keep := range []string{"ssh.service enabled", "serial-getty@ttyGS0.service enabled"} {
		if !strings.Contains(units(), keep) {
			t.Errorf("%s must stay", keep)
		}
	}
	// 段階 3 だけを戻す：ほかは記録に残る
	run("revert", "3")
	u = units()
	if !strings.Contains(u, "ly.service enabled") || !strings.Contains(u, "NetworkManager.service disabled") || !strings.Contains(u, "udisks2.service disabled") {
		t.Errorf("after revert 3:\n%s", u)
	}
	st, _ = os.ReadFile(state)
	if strings.Contains(string(st), "ly.service") || !strings.Contains(string(st), "networking.service enabled") {
		t.Errorf("state after revert 3:\n%s", st)
	}
	run("revert", "2")
	if u := units(); !strings.Contains(u, "NetworkManager.service enabled") || !strings.Contains(u, "accounts-daemon.service disabled") {
		t.Errorf("after revert 2:\n%s", u)
	}
	run("revert")
	if strings.Contains(units(), "disabled") {
		t.Errorf("after revert:\n%s", units())
	}
	if _, err := os.Stat(state + ".reverted"); err != nil {
		t.Error(err)
	}
	if out := run("revert"); !strings.Contains(out, "戻すものはありません") {
		t.Errorf("second revert:\n%s", out)
	}
	cmd := exec.Command("sh", "contrib/boot/boot-tune.sh", "apply", "nope")
	cmd.Env = append(os.Environ(), "SYSTEMCTL="+fake, "BOOT_TUNE_STATE="+state, "BOOT_TUNE_NOROOT=1")
	if err := cmd.Run(); err == nil {
		t.Error("unknown item must fail")
	}
}

// 段階 4〜6（フェーズ K6）：ファイルを足す、/dev/null で隠す、mask する。revert で消し、unmask する。
func TestBootTuneFiles(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "units")
	log := filepath.Join(dir, "log")
	fake := filepath.Join(dir, "systemctl")
	os.WriteFile(fake, []byte(`#!/bin/sh
db=`+db+`
echo "$*" >> `+log+`
case $1 in
is-enabled) s=$(grep "^$2 " $db | cut -d' ' -f2); [ -n "$s" ] && echo $s || { echo not-found; exit 1; }; [ "$s" = enabled ] ;;
mask) sed -i "s/^$2 .*/$2 masked/" $db ;;
unmask) sed -i "s/^$2 .*/$2 static/" $db ;;
esac
`), 0o755)
	os.WriteFile(db, []byte("modprobe@drm.service static\nmodprobe@efi_pstore.service static\nmodprobe@configfs.service static\nmodprobe@fuse.service static\n"), 0o644)
	root := filepath.Join(dir, "root")
	// 前からある udev の規則のフォルダ（revert でフォルダごと消さない）
	os.MkdirAll(filepath.Join(root, "etc/udev/rules.d"), 0o755)
	state := filepath.Join(root, "etc/lefthand/boot-tune.state")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{"contrib/boot/boot-tune.sh"}, args...)...)
		cmd.Env = append(os.Environ(), "SYSTEMCTL="+fake, "BOOT_TUNE_STATE="+state, "BOOT_TUNE_NOROOT=1", "BOOT_TUNE_ROOT="+root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	exists := func(p string) bool { _, err := os.Lstat(filepath.Join(root, p)); return err == nil }
	early := "etc/systemd/system/lefthand.service.d/boot-early.conf"

	if out := run("plan", "4"); !strings.Contains(out, "[段階 4] early") || !strings.Contains(out, "boot-early.conf（今：absent）") {
		t.Errorf("plan 4:\n%s", out)
	}
	run("apply", "4")
	b, _ := os.ReadFile(filepath.Join(root, early))
	for _, want := range []string{"DefaultDependencies=no", "After=systemd-remount-fs.service fake-hwclock-load.service", "Conflicts=shutdown.target", "Before=shutdown.target"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("boot-early.conf: want %q in\n%s", want, b)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/systemd/system/ethernet_gadget.service.d/boot-early.conf")); !strings.Contains(string(b), "After=sys-kernel-config.mount") {
		t.Errorf("gadget drop-in:\n%s", b)
	}
	if l, _ := os.ReadFile(log); !strings.Contains(string(l), "daemon-reload") {
		t.Error("apply must daemon-reload after adding a drop-in")
	}
	run("apply", "5")
	for _, p := range []string{"etc/systemd/system.conf.d/brain-deck-boot.conf", "etc/sysctl.d/90-brain-deck-console.conf"} {
		if !exists(p) {
			t.Errorf("%s missing", p)
		}
	}
	if l, err := os.Readlink(filepath.Join(root, "etc/udev/rules.d/60-triggerhappy.rules")); err != nil || l != "/dev/null" {
		t.Errorf("triggerhappy rule: %q %v", l, err)
	}
	u, _ := os.ReadFile(db)
	// configfs と fuse は、マウントが Requires= するので mask しない（mask すると configfs がなく、USB の構成が作れない）
	for _, want := range []string{"modprobe@drm.service masked", "modprobe@efi_pstore.service masked", "modprobe@configfs.service static", "modprobe@fuse.service static"} {
		if !strings.Contains(string(u), want) {
			t.Errorf("want %q in\n%s", want, u)
		}
	}
	// brain-deck が置いたのではないファイルには触らない
	os.MkdirAll(filepath.Join(root, "etc/systemd/journald.conf.d"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/systemd/journald.conf.d/brain-deck.conf"), []byte("mine\n"), 0o644)
	if out := run("apply", "journal"); !strings.Contains(out, "そのまま") {
		t.Errorf("existing file:\n%s", out)
	}
	os.RemoveAll(filepath.Join(root, "etc/systemd/journald.conf.d"))
	run("apply", "6")
	if b, _ := os.ReadFile(filepath.Join(root, "etc/systemd/journald.conf.d/brain-deck.conf")); !strings.Contains(string(b), "SystemMaxUse=32M") {
		t.Errorf("journald: %s", b)
	}
	if out := run("status"); !strings.Contains(out, "boot-early.conf：前は absent、今は present（file）") || !strings.Contains(out, "modprobe@drm.service：前は static、今は masked（mask）") {
		t.Errorf("status:\n%s", out)
	}
	// 段階 5 だけ戻す
	run("revert", "5")
	if exists("etc/udev/rules.d/60-triggerhappy.rules") || exists("etc/systemd/system.conf.d") || !exists("etc/udev/rules.d") || !exists(early) {
		t.Error("revert 5")
	}
	if u, _ := os.ReadFile(db); !strings.Contains(string(u), "modprobe@drm.service static") {
		t.Errorf("unmask:\n%s", u)
	}
	run("revert")
	if exists(early) || exists("etc/systemd/system/lefthand.service.d") || exists("etc/systemd/journald.conf.d/brain-deck.conf") {
		t.Error("revert all")
	}
}
