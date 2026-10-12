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
disable) sed -i "s/^$2 .*/$2 disabled/" $db ;;
enable) sed -i "s/^$2 .*/$2 enabled/" $db ;;
esac
`), 0o755)
	os.WriteFile(db, []byte("accounts-daemon.service enabled\nudisks2.service enabled\ne2scrub_reap.service enabled\ntriggerhappy.service enabled\ntriggerhappy.socket enabled\navahi-daemon.service enabled\navahi-daemon.socket enabled\nly.service enabled\nNetworkManager.service enabled\n"), 0o644)
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

	if out := run("plan"); !strings.Contains(out, "accounts") || strings.Contains(out, "ly.service") {
		t.Errorf("plan: ly は既定で選ばない:\n%s", out)
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
	run("apply", "ly")
	if !strings.Contains(units(), "ly.service disabled") {
		t.Error("apply ly")
	}
	if out := run("status"); !strings.Contains(out, "ly.service：前は enabled、今は disabled") {
		t.Errorf("status:\n%s", out)
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
