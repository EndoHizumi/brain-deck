package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------- 入れ替えの係（lefthand -update-apply <plan.json>） ----------
//
// デーモンが systemd-run で、別のユニット（lefthand-update）として起動する。デーモンを止めても係は止まらない。
// 進み具合は <data-dir>/update/state.json に書き、ログは journal（brain-deck logs）に出る。

// helper は係の 1 回の実行。システムの操作は sys（テストで差し替える）。
type helper struct {
	p     updatePlan
	sys   sysOps
	st    updateState
	sleep func(time.Duration)
	now   func() time.Time
	logf  func(format string, args ...any)
}

func runUpdateHelper(planPath string) int {
	b, err := os.ReadFile(planPath)
	if err != nil {
		log.Printf("update-apply: %v", err)
		return 1
	}
	var p updatePlan
	if err := json.Unmarshal(b, &p); err != nil {
		log.Printf("update-apply: %s: %v", planPath, err)
		return 1
	}
	h := &helper{p: p, sys: realSys, sleep: time.Sleep, now: time.Now, logf: log.Printf}
	if !h.run() {
		return 1
	}
	return 0
}

func (h *helper) phase(name, msg string) {
	h.st.Phase, h.st.Message = name, msg
	if msg != "" {
		h.logf("update-apply: %s: %s", name, msg)
	} else {
		h.logf("update-apply: %s", name)
	}
	saveUpdateState(h.p.Dir, h.st)
}

// finish は終わりを書く。ステージのディレクトリは消す（state.json は残す）。
func (h *helper) finish(ok bool, phase, msg string) bool {
	h.st.Done, h.st.OK, h.st.Finished = true, ok, h.now()
	h.phase(phase, msg)
	os.RemoveAll(filepath.Join(h.p.Dir, h.p.Stage))
	return ok
}

func (h *helper) run() bool {
	p := h.p
	h.st = updateState{Stage: p.Stage, Action: p.Action, From: p.From, To: p.Version, Started: h.now()}
	h.sleep(updateHelperDelay) // デーモンが PC に返事を送り終えるのを待つ
	stage := filepath.Join(p.Dir, p.Stage)

	h.phase("stopping", p.Unit)
	if _, err := h.sys.systemctl("stop", p.Unit); err != nil {
		h.sys.systemctl("start", p.Unit)
		return h.finish(false, "failed", "stopping "+p.Unit+": "+err.Error())
	}
	os.Remove(p.ReadyFile) // 前のデーモンの知らせと取り違えない

	// 入れ替える
	if p.Action == "restore" {
		return h.restore(stage)
	}
	var done []installed
	rebind := p.Rebind
	if p.Action != "restart" {
		h.phase("installing", "")
		pairs := [][2]string{{p.StagedBin, p.Bin}}
		if p.StagedGad != "" {
			pairs = append(pairs, [2]string{p.StagedGad, p.Gadget})
		}
		pairs = append(pairs, p.Files...) // 読み物の文字（フェーズ K7）
		for i, pr := range pairs {
			in, err := install(pr[0], pr[1], stage)
			if err != nil {
				h.undo(done, false)
				return h.finish(false, "rolled_back", err.Error())
			}
			if i >= len(pairs)-len(p.Files) {
				os.Chmod(pr[1], 0o644) // 実行ファイルではない
			}
			done = append(done, in)
		}
	}
	gadgetChanged := p.StagedGad != "" && len(done) >= 2 && done[1].changed

	// USB の付け直し。gadget-setup.sh が変わったときは実行する（違うところがあるときだけ付け直す）
	if rebind || gadgetChanged {
		h.phase("rebinding", "")
		changed, err := h.gadget(rebind)
		if err != nil {
			h.undo(done, gadgetChanged)
			return h.finish(false, "rolled_back", err.Error())
		}
		h.st.Rebound = changed
	}

	if err := h.startAndWait(stage); err != nil {
		if p.Action == "restart" {
			if p.Config != "" && h.restoreConfig() {
				return h.finish(false, "rolled_back", err.Error()+"; the previous config was restored")
			}
			return h.finish(false, "failed", err.Error())
		}
		h.undo(done, gadgetChanged)
		return h.finish(false, "rolled_back", err.Error())
	}
	return h.finish(true, "done", "")
}

// restore は、バックアップを入れ替えて起動し直す。起動しなければ（PC がつなぎ直せなければ）、元に戻して起動し直す。
func (h *helper) restore(stage string) bool {
	rp := h.p.Restore
	if rp == nil {
		h.sys.systemctl("start", h.p.Unit)
		return h.finish(false, "failed", "the plan has no restore section")
	}
	h.phase("restoring", restoreItemsLabel(rp.RestoreOptions))
	u, err := applyRestore(*rp)
	if err != nil {
		h.sys.systemctl("start", h.p.Unit)
		return h.finish(false, "rolled_back", "restoring: "+err.Error())
	}
	if err := h.startAndWait(stage); err != nil {
		h.phase("rolling_back", "")
		h.sys.systemctl("stop", h.p.Unit)
		msg := err.Error()
		if uerr := u.undo(); uerr != nil {
			msg += "; undoing the restore also failed: " + uerr.Error()
		}
		if _, err := h.sys.systemctl("start", h.p.Unit); err != nil {
			h.logf("update-apply: starting %s after undoing the restore: %v", h.p.Unit, err)
		}
		return h.finish(false, "rolled_back", msg)
	}
	u.finish()
	return h.finish(true, "done", "")
}

// gadget は gadget-setup.sh を実行する。force なら、先に UDC から切り離す（必ず付け直す）。
// 付け直したか（force か、スクリプトがガジェットを変えたと書いたか）を返す。
func (h *helper) gadget(force bool) (bool, error) {
	b := map[bool]string{false: "0", true: "1"}
	env := []string{"HID_MOUSE=" + b[h.p.HIDMouse], "GADGET_TERMINAL=0", "LEFTHAND_SELF=1"}
	if force {
		if out, code, err := h.sys.command(append(env, "GADGET_UNBIND_ONLY=1"), "bash", h.p.Gadget); err != nil || code != 0 {
			return false, fmt.Errorf("gadget-setup (unbind): exit %d %v: %s", code, err, strings.TrimSpace(out))
		}
	}
	out, code, err := h.sys.command(env, "bash", h.p.Gadget)
	changed := force
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			h.logf("update-apply: gadget-setup: %s", l)
		}
		// gadget-setup.sh は、ガジェットを変えたとき（UDC から切り離したとき）にこれらを書く
		for _, p := range []string{"created:", "added:", "updated:", "configuration:", "bmAttributes:"} {
			changed = changed || strings.HasPrefix(l, p)
		}
	}
	if err != nil || code != 0 {
		return changed, fmt.Errorf("gadget-setup.sh failed (exit %d %v): %s", code, err, lastLine(out))
	}
	return changed, nil
}

// mainPID は、サービスの今の PID（動いていなければ 0）。
func (h *helper) mainPID() int {
	out, err := h.sys.systemctl("show", "-p", "MainPID", "--value", h.p.Unit)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// startAndWait はサービスを起動し、新しいデーモンが起動を知らせ（版が合い）、PC が update_confirm を送るのを待つ。
func (h *helper) startAndWait(stage string) error {
	p := h.p
	h.phase("starting", p.Unit)
	if _, err := h.sys.systemctl("start", p.Unit); err != nil {
		return fmt.Errorf("starting %s: %v", p.Unit, err)
	}
	if p.Legacy {
		// 起動を知らせない古い版：落ちずに動き続けることだけを確かめる
		h.phase("waiting_ready", fmt.Sprintf("the version does not report readiness; checking it keeps running for %v", updateLegacyStable))
		pid := h.waitPID(updateReadyTimeout)
		if pid == 0 {
			return errors.New(p.Unit + " did not start")
		}
		h.sleep(updateLegacyStable)
		if now := h.mainPID(); now != pid {
			return fmt.Errorf("%s did not keep running (pid %d → %d)", p.Unit, pid, now)
		}
		return nil
	}

	h.phase("waiting_ready", "")
	deadline := h.now().Add(updateReadyTimeout)
	var pid int
	for {
		var r readyInfo
		if b, err := os.ReadFile(p.ReadyFile); err == nil && json.Unmarshal(b, &r) == nil {
			cur := h.mainPID()
			if cur != 0 && r.PID == cur {
				if p.Version != "" && r.Version != p.Version {
					return fmt.Errorf("the started lefthand reports version %s, not %s", r.Version, p.Version)
				}
				pid = cur
				break
			}
		}
		if h.now().After(deadline) {
			return fmt.Errorf("the new lefthand did not become ready within %v (see brain-deck logs)", updateReadyTimeout)
		}
		h.sleep(200 * time.Millisecond)
	}

	if p.Confirm {
		h.phase("waiting_confirm", "waiting for brain-deck to reconnect")
		deadline = h.now().Add(updateConfirmTimeout)
		for {
			var c struct {
				PID int `json:"pid"`
			}
			if b, err := os.ReadFile(filepath.Join(stage, updateConfirmFile)); err == nil && json.Unmarshal(b, &c) == nil && c.PID == pid {
				break
			}
			if h.mainPID() != pid {
				return errors.New("the new lefthand stopped before brain-deck reconnected")
			}
			if h.now().After(deadline) {
				return fmt.Errorf("brain-deck did not reconnect to the new lefthand within %v", updateConfirmTimeout)
			}
			h.sleep(200 * time.Millisecond)
		}
	}
	h.sleep(updateStable)
	if now := h.mainPID(); now != pid {
		return fmt.Errorf("the new lefthand did not keep running (pid %d → %d)", pid, now)
	}
	return nil
}

// waitPID は、サービスが PID を持つまで待つ。
func (h *helper) waitPID(d time.Duration) int {
	deadline := h.now().Add(d)
	for {
		if pid := h.mainPID(); pid != 0 {
			return pid
		}
		if h.now().After(deadline) {
			return 0
		}
		h.sleep(200 * time.Millisecond)
	}
}

// undo は、入れ替えを取り消して、前の版でサービスを起動し直す。rerun なら、前の gadget-setup.sh を実行し直す。
func (h *helper) undo(done []installed, rerun bool) {
	h.phase("rolling_back", "")
	h.sys.systemctl("stop", h.p.Unit)
	for i := len(done) - 1; i >= 0; i-- {
		if err := restore(done[i]); err != nil {
			h.logf("update-apply: %v", err)
		}
	}
	if rerun {
		if _, err := h.gadget(false); err != nil {
			h.logf("update-apply: rerunning the previous gadget-setup.sh: %v", err)
		}
	}
	if _, err := h.sys.systemctl("start", h.p.Unit); err != nil {
		h.logf("update-apply: starting %s after rolling back: %v", h.p.Unit, err)
	}
}

// restoreConfig は、put_config_text で書いた設定を前の版（.prev）に戻し、サービスを起動し直す。
func (h *helper) restoreConfig() bool {
	h.phase("rolling_back", "restoring "+h.p.Config+".prev")
	prev, err := os.ReadFile(h.p.Config + ".prev")
	if err != nil {
		h.logf("update-apply: %v", err)
		return false
	}
	h.sys.systemctl("stop", h.p.Unit)
	if err := writeFileAtomic(h.p.Config, prev, false); err != nil {
		h.logf("update-apply: %v", err)
		return false
	}
	h.sys.systemctl("reset-failed", h.p.Unit)
	if _, err := h.sys.systemctl("start", h.p.Unit); err != nil {
		h.logf("update-apply: %v", err)
	}
	return true
}
