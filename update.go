package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- 更新、再起動、電源（PC の brain-deck から。ssh を使わない） ----------
//
// 更新は、デーモン（lefthand）と gadget-setup.sh を組で入れ替える。手順は次のとおり。
//
//  1. 受け取る：PC は「更新のファイル」（tools/mkupdate が作る tar.gz。manifest.json、lefthand、gadget-setup.sh）を
//     upload_*（kind: update）で分けて送る。デーモンは SHA-256 を確かめ、<data-dir>/update/<stage>/ に展開する
//  2. 確かめる：manifest の SHA-256、新しい lefthand の -version（manifest の版と同じか）、-check-gadget（新しい lefthand の
//     ディスクリプタと gadget-setup.sh のものが同じか）、-check（今の設定を新しい lefthand が読めるか）。
//     gadget-setup.sh が変わるときは、GADGET_CHECK=1 で USB の付け直しが要るかを調べる。結果を PC に返す（まだ何も変えない）
//  3. 入れ替える（update_apply）：デーモンは systemd-run で「入れ替えの係」（lefthand -update-apply。デーモンとは別のユニット）を
//     起動して返事をする。係は、サービスを止め、今の版を .prev に残して入れ替え、gadget-setup.sh が変わっていれば実行し
//     （要れば USB を付け直す）、サービスを起動する
//  4. 確かめる：新しいデーモンが起動を知らせる（-ready-file）のを待ち、PC の brain-deck がつなぎ直して update_confirm を
//     送るのを待つ。どちらかが時間内に来なければ、前の版に戻してサービスを起動し直す（自動で戻す）
//
// 前の版に戻す（update_rollback）のも、.prev を「受け取ったもの」として同じ手順で行う（今の版が .prev になる）。
// 再起動（restart）と USB の付け直し（restart の usb）も、同じ係が行う（デーモンは自分を止められないため）。
// 係の進み具合は <data-dir>/update/state.json に書き、update_status で読む。

const (
	updateFormat       = 1                 // 更新のファイルの manifest.json の format
	updateProtocol     = 1                 // -version の update（この仕組みの版）
	updateHelperUnit   = "lefthand-update" // 係のユニットの名前
	updateMaxBundle    = 24 << 20          // 更新のファイル（tar.gz）の大きさの上限
	updateMaxBinary    = 32 << 20          // 展開した lefthand の上限
	updateMaxScript    = 1 << 20           // gadget-setup.sh の上限
	updateMaxManifest  = 64 << 10          //
	updateStateFile    = "state.json"      // 係の進み具合
	updatePlanFile     = "plan.json"       // 係への指示
	updateConfirmFile  = "confirmed"       // 新しいデーモンが update_confirm を受けたしるし
	updateBinName      = "lefthand"        // 更新のファイルの中の名前
	updateGadgetName   = "gadget-setup.sh" //
	updateManifestName = "manifest.json"   //
	defaultReadyFile   = "/run/lefthand-ready"
)

// テストで短くする
var (
	updateReserve        int64 = 16 << 20         // 展開したあとも残しておく空き（ファイルシステムの 10% のほうが小さければ、そちら）
	updateReadyTimeout         = 45 * time.Second // 新しいデーモンが起動を知らせるまで
	updateConfirmTimeout       = 90 * time.Second // PC がつなぎ直して update_confirm を送るまで
	updateLegacyStable         = 10 * time.Second // 起動を知らせない古い版に戻したとき、落ちずに動き続けることを確かめる時間
	updateStable               = 2 * time.Second  // update_confirm のあと、落ちずに動いていることを確かめる時間
	updateHelperDelay          = time.Second      // 係が、サービスを止める前に待つ時間（PC への返事を送り終えるため）
	powerDelay                 = 500 * time.Millisecond
)

var stageRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ---------- 版（-version） ----------

// versionInfo は lefthand -version の出力。
type versionInfo struct {
	Version string `json:"version"`
	Update  int    `json:"update"` // この仕組み（update_*、-update-apply、-ready-file）の版。古い lefthand にはない
}

func printVersion() {
	b, _ := json.Marshal(versionInfo{Version: daemonVersion(), Update: updateProtocol})
	fmt.Println(string(b))
}

// ---------- gadget-setup.sh とディスクリプタの組（-check-gadget） ----------

// scriptDescriptor は、gadget-setup.sh の NAME='...'（'...' が \ と改行で続けて書いてあれば、つなげたもの）の \xNN を
// バイト列にする。bash の printf と同じ結果になる（\xNN 以外の書き方は使っていない）。
func scriptDescriptor(script []byte, name string) ([]byte, error) {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=((?:'[^']*'\\\n)*'[^']*')`).FindSubmatch(script)
	if m == nil {
		return nil, fmt.Errorf("%s not found", name)
	}
	lit := strings.NewReplacer("\\\n", "", "'", "").Replace(string(m[1]))
	var out []byte
	for i := 0; i < len(lit); {
		if !strings.HasPrefix(lit[i:], `\x`) || i+4 > len(lit) {
			return nil, fmt.Errorf("%s: unexpected %q at %d (only \\xNN is supported)", name, lit[i:min(i+4, len(lit))], i)
		}
		v, err := strconv.ParseUint(lit[i+2:i+4], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		out = append(out, byte(v))
		i += 4
	}
	return out, nil
}

// checkGadgetScript は、gadget-setup.sh のディスクリプタが、この lefthand のもの（hid.go）と同じかを確かめる。
func checkGadgetScript(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, d := range []struct {
		name string
		want []byte
	}{{"KBD_DESC", hidDescKeyboard}, {"COMBO_DESC", hidDescCombo}} {
		got, err := scriptDescriptor(b, d.name)
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		if !bytes.Equal(got, d.want) {
			return fmt.Errorf("%s: %s differs from this lefthand (%s): the pair does not match", path, d.name, daemonVersion())
		}
	}
	return nil
}

// ---------- 起動の知らせ（-ready-file） ----------

// readyInfo は、デーモンが起動を終えたときに書くファイル。係は、版と PID で、新しいデーモンが動いたことを確かめる。
type readyInfo struct {
	Version string    `json:"version"`
	PID     int       `json:"pid"`
	At      time.Time `json:"at"`
}

// writeReady は起動の知らせを書く。終わるときに消す。
func writeReady(path string) {
	if path == "" {
		return
	}
	b, _ := json.Marshal(readyInfo{Version: daemonVersion(), PID: os.Getpid(), At: time.Now()})
	if err := writeFileAtomic(path, b, false); err != nil {
		log.Printf("update: %s: %v", path, err)
		return
	}
	addAtExit(func() {
		var r readyInfo
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &r) == nil && r.PID == os.Getpid() {
			os.Remove(path)
		}
	})
}

// ---------- システムの操作（テストで差し替える） ----------

// sysOps は、systemctl などの外のコマンド。
type sysOps struct {
	// systemctl は systemctl を実行し、標準出力を返す
	systemctl func(args ...string) (string, error)
	// systemdRun は、係を別のユニットとして起動する（systemd-run）
	systemdRun func(unit string, argv ...string) error
	// command は、env を足してコマンドを実行し、出力（標準出力と標準エラー）と終了コードを返す
	command func(env []string, name string, args ...string) (string, int, error)
}

var realSys = sysOps{
	systemctl: func(args ...string) (string, error) {
		out, err := exec.Command("systemctl", args...).Output()
		return strings.TrimSpace(string(out)), err
	},
	systemdRun: func(unit string, argv ...string) error {
		args := append([]string{"--unit=" + unit, "--collect", "--quiet", "-p", "RuntimeMaxSec=600"}, argv...)
		out, err := exec.Command("systemd-run", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	},
	command: runCommand,
}

func runCommand(env []string, name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), nil
	}
	if err != nil {
		return string(out), -1, err
	}
	return string(out), 0, nil
}

// unitOf は、/proc/self/cgroup から、自分が動いている systemd のサービスの名前を返す（なければ空）。
func unitOf(cgroup []byte) string {
	for _, l := range strings.Split(string(cgroup), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			if b := filepath.Base(p); strings.HasSuffix(b, ".service") {
				return b
			}
		}
	}
	return ""
}

// ---------- 更新の受け取りと確かめ（デーモンの中） ----------

// Updater は、更新・再起動・電源のコマンドを受け持つ。
type Updater struct {
	Dir       string // <data-dir>/update
	Bin       string // 入れ替える lefthand（/usr/local/bin/lefthand）
	Gadget    string // 入れ替える gadget-setup.sh（/usr/local/sbin/lefthand-gadget-setup）
	Unit      string // デーモンのサービス（lefthand.service）
	Helper    string // 係として実行する lefthand（空なら、動いている自分）
	CfgPath   string
	ImagesDir string
	ReadyFile string
	usb       *USBMode
	term      *TermMode
	sys       sysOps

	mu     sync.Mutex
	staged map[string]*StageInfo // 確かめ終えたもの（stage → 中身）
}

// StageInfo は、確かめ終えた更新（upload_end と update_rollback の結果）。
type StageInfo struct {
	Stage          string   `json:"stage"`
	Action         string   `json:"action"` // update か rollback
	Version        string   `json:"version"`
	CurrentVersion string   `json:"current_version"`
	BinChanged     bool     `json:"bin_changed"`
	GadgetChanged  bool     `json:"gadget_changed"`
	HasGadget      bool     `json:"has_gadget"`
	Rebind         string   `json:"rebind"` // no、yes、maybe（調べられない）
	RebindReasons  []string `json:"rebind_reasons,omitempty"`
	Legacy         bool     `json:"legacy,omitempty"` // 起動の知らせと update_confirm がない古い版（戻すときだけ）
	Warnings       []string `json:"warnings"`
	BuiltAt        string   `json:"built_at,omitempty"`
}

func NewUpdater(dataDir, bin, gadget, unit, cfgPath, imagesDir, ready string, usb *USBMode, term *TermMode) *Updater {
	u := &Updater{Dir: filepath.Join(dataDir, "update"), Bin: bin, Gadget: gadget, Unit: unit, CfgPath: cfgPath,
		ImagesDir: imagesDir, ReadyFile: ready, usb: usb, term: term, sys: realSys, staged: map[string]*StageInfo{}}
	if u.Unit == "" {
		if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
			u.Unit = unitOf(b)
		}
	}
	return u
}

var (
	errUpdateBusy  = errors.New("busy")      // conflict
	errUpdateBad   = errors.New("bad")       // bad_request
	errUpdateCheck = errors.New("rejected")  // update_rejected：確かめて断った
	errNoService   = errors.New("no unit")   // internal_error（サービスとして動いていない）
	errUpdateGone  = errors.New("not found") // not_found
)

// UploadKind は、upload_* の kind: update。
func (u *Updater) UploadKind() UploadKind {
	return UploadKind{Dir: u.Dir, MaxBytes: updateMaxBundle,
		Check: func(b UploadBegin) error {
			if err := u.canChange(); err != nil {
				return err
			}
			free, size, err := statfsFree(filepath.Dir(u.Dir))
			if err != nil {
				return nil // 空きを読めないときは、展開で失敗したら分かる
			}
			// 受け取ったファイルと、展開した lefthand（tar.gz のおよそ 2 倍）が同時に置かれる
			reserve := min(updateReserve, size/10)
			if need := b.Bytes * 3; free-need < reserve {
				return fmt.Errorf("%w: %s has %s free; receiving and unpacking %s would leave less than %s",
					errImageNoSpace, filepath.Dir(u.Dir), kib(free), kib(need), kib(reserve))
			}
			return nil
		},
		Done: u.receive}
}

// canChange は、今、更新や再起動を始めてよいか。
func (u *Updater) canChange() error {
	if u.Unit == "" {
		return fmt.Errorf("%w: lefthand is not running as a systemd service, so it cannot update or restart itself", errNoService)
	}
	if u.term != nil && u.term.Active() {
		return fmt.Errorf("%w: the terminal mode is on; leave it first (brain-deck terminal off)", errUpdateBusy)
	}
	if u.usb.Switching() {
		return fmt.Errorf("%w: the USB mode is being switched; try again in a few seconds", errUpdateBusy)
	}
	if st, _ := u.sys.systemctl("is-active", updateHelperUnit+".service"); st == "active" || st == "activating" {
		return fmt.Errorf("%w: another update or restart is in progress (%s)", errUpdateBusy, updateHelperUnit)
	}
	return nil
}

// clearStages は、ほかの確かめ終えた更新を消す（同時に 1 つだけ）。
func (u *Updater) clearStagesLocked(keep string) {
	ents, _ := os.ReadDir(u.Dir)
	for _, e := range ents {
		if e.IsDir() && stageRE.MatchString(e.Name()) && e.Name() != keep {
			os.RemoveAll(filepath.Join(u.Dir, e.Name()))
			delete(u.staged, e.Name())
		}
	}
}

// updateManifest は、更新のファイルの manifest.json。
type updateManifest struct {
	Format  int                     `json:"format"`
	Version string                  `json:"version"`
	Built   string                  `json:"built,omitempty"`
	Files   map[string]manifestFile `json:"files"`
}

type manifestFile struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// receive は、確かめた更新のファイル（tar.gz）を展開して確かめる。
func (u *Updater) receive(b UploadBegin, f *os.File, path string) (any, error) {
	defer os.Remove(path)
	defer f.Close()
	stage := newToken()
	dir := filepath.Join(u.Dir, stage)
	u.mu.Lock()
	u.clearStagesLocked("")
	u.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fail := func(err error) (any, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if err := unpackUpdate(f, dir); err != nil {
		return fail(err)
	}
	f.Close()
	os.Remove(path) // 展開したら、もとのファイルは要らない（SD カードの空きを残す）
	mb, err := os.ReadFile(filepath.Join(dir, updateManifestName))
	if err != nil {
		return fail(fmt.Errorf("%w: the update file has no %s", errUpdateCheck, updateManifestName))
	}
	var m updateManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return fail(fmt.Errorf("%w: %s: %v", errUpdateCheck, updateManifestName, err))
	}
	if m.Format != updateFormat {
		return fail(fmt.Errorf("%w: %s format %d is not supported (this lefthand reads %d)", errUpdateCheck, updateManifestName, m.Format, updateFormat))
	}
	for _, name := range []string{updateBinName, updateGadgetName} {
		mf, ok := m.Files[name]
		if !ok {
			return fail(fmt.Errorf("%w: the update file must contain both %s and %s (they are replaced as a pair)", errUpdateCheck, updateBinName, updateGadgetName))
		}
		sum, n, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return fail(fmt.Errorf("%w: %s: %v", errUpdateCheck, name, err))
		}
		if sum != mf.SHA256 || n != mf.Bytes {
			return fail(fmt.Errorf("%w: %s does not match %s (SHA-256 %s, %d bytes)", errUpdateCheck, name, updateManifestName, sum[:16], n))
		}
		os.Chmod(filepath.Join(dir, name), 0o755)
	}
	info, err := u.verify(dir, "update", m.Version, false)
	if err != nil {
		return fail(err)
	}
	info.Stage, info.BuiltAt = stage, m.Built
	u.mu.Lock()
	u.staged[stage] = info
	u.mu.Unlock()
	log.Printf("update: received %s (%s → %s, gadget-setup.sh changed=%v, rebind=%s) from %s", stage, info.CurrentVersion, info.Version, info.GadgetChanged, info.Rebind, b.Source)
	return info, nil
}

// unpackUpdate は tar.gz を dir に展開する。入っていてよいのは、manifest.json、lefthand、gadget-setup.sh だけ。
func unpackUpdate(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: the update file is not a .tar.gz made by tools/mkupdate (%v)", errUpdateCheck, err)
	}
	limits := map[string]int64{updateManifestName: updateMaxManifest, updateBinName: updateMaxBinary, updateGadgetName: updateMaxScript}
	seen := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: reading the update file: %v", errUpdateCheck, err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir && (name == "" || name == ".") {
			continue
		}
		max, ok := limits[name]
		if !ok || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%w: unexpected entry %q in the update file", errUpdateCheck, h.Name)
		}
		if seen[name] {
			return fmt.Errorf("%w: %q appears twice in the update file", errUpdateCheck, name)
		}
		seen[name] = true
		if h.Size > max {
			return fmt.Errorf("%w: %s is too large (%d bytes, at most %d)", errUpdateCheck, name, h.Size, max)
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, max+1))
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("unpacking %s: %w", name, err)
		}
	}
	return nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// verify は、展開した lefthand と gadget-setup.sh（dir の中）を確かめる。want は manifest の版（空なら比べない）。
// legacyOK なら、-version に答えない古い lefthand も受け付ける（前の版に戻すとき）。
func (u *Updater) verify(dir, action, want string, legacyOK bool) (*StageInfo, error) {
	bin, script := filepath.Join(dir, updateBinName), filepath.Join(dir, updateGadgetName)
	info := &StageInfo{Action: action, CurrentVersion: daemonVersion(), Warnings: []string{}, Rebind: "no"}
	_, hasGadget := os.Stat(script)
	info.HasGadget = hasGadget == nil

	// 版（新しい lefthand を実際に実行する。CPU の種類が違えば、ここで分かる）
	out, code, err := u.sys.command(nil, bin, "-version")
	var vi versionInfo
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: cannot run the new lefthand: %v (is it built for ARMv5? GOARCH=arm GOARM=5)", errUpdateCheck, err)
	case code == 0 && json.Unmarshal([]byte(lastLine(out)), &vi) == nil && vi.Version != "":
	case legacyOK:
		info.Legacy, vi.Version = true, "(-version なし)"
	default:
		return nil, fmt.Errorf("%w: the new lefthand did not answer -version (exit %d): %s", errUpdateCheck, code, strings.TrimSpace(out))
	}
	info.Version = vi.Version
	if want != "" && vi.Version != want {
		return nil, fmt.Errorf("%w: the new lefthand reports version %s, but the update file says %s", errUpdateCheck, vi.Version, want)
	}
	if !info.Legacy && vi.Update < 1 && !legacyOK {
		return nil, fmt.Errorf("%w: the new lefthand (%s) cannot be updated with brain-deck afterwards", errUpdateCheck, vi.Version)
	}
	if !info.Legacy && vi.Update < 1 {
		info.Legacy = true
	}
	if sum, _, err := fileSHA256(bin); err == nil {
		cur, _, _ := fileSHA256(u.Bin)
		info.BinChanged = sum != cur
	}

	// 組（ディスクリプタが同じか）。古い lefthand は -check-gadget を知らないので、調べない
	if info.HasGadget && !info.Legacy {
		if out, code, err := u.sys.command(nil, bin, "-check-gadget", script); err != nil || code != 0 {
			return nil, fmt.Errorf("%w: the new lefthand and gadget-setup.sh do not match: %s", errUpdateCheck, strings.TrimSpace(out))
		}
	}
	if info.HasGadget {
		if out, code, err := u.sys.command(nil, "bash", "-n", script); err != nil || code != 0 {
			return nil, fmt.Errorf("%w: gadget-setup.sh has a syntax error: %s", errUpdateCheck, strings.TrimSpace(out))
		}
	}

	// 今の設定を、新しい lefthand が読めるか
	args := []string{"-check"}
	if u.ImagesDir != "" {
		args = append(args, "-render-images", u.ImagesDir)
	}
	out, code, err = u.sys.command(nil, bin, append(args, u.CfgPath)...)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("%w: the new lefthand cannot read the current config %s: %s", errUpdateCheck, u.CfgPath, strings.TrimSpace(out))
	}
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "warning: "); i >= 0 {
			info.Warnings = append(info.Warnings, l[i+len("warning: "):])
		}
	}

	// USB の付け直しが要るか
	if info.HasGadget {
		sum, _, _ := fileSHA256(script)
		cur, _, _ := fileSHA256(u.Gadget)
		info.GadgetChanged = sum != cur
	}
	if info.GadgetChanged {
		info.Rebind, info.RebindReasons = u.gadgetCheck(script)
	}
	return info, nil
}

// gadgetCheck は、gadget-setup.sh を GADGET_CHECK=1 で実行し、USB の付け直しが要るかを調べる。
// GADGET_CHECK を知らない古いスクリプトは、実行すると本当に付け直すので、実行しない（maybe）。
func (u *Updater) gadgetCheck(script string) (string, []string) {
	b, err := os.ReadFile(script)
	if err != nil || !bytes.Contains(b, []byte("GADGET_CHECK")) {
		return "maybe", []string{"gadget-setup.sh cannot tell in advance; it rebinds USB only if needed"}
	}
	out, code, err := u.sys.command(u.gadgetEnv(), "bash", script)
	var reasons []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if r, ok := strings.CutPrefix(l, "rebind: "); ok {
			reasons = append(reasons, r)
		}
	}
	switch {
	case err == nil && code == 0:
		return "no", nil
	case err == nil && code == 10:
		return "yes", reasons
	}
	return "maybe", []string{strings.TrimSpace(out)}
}

// gadgetEnv は、gadget-setup.sh に渡す環境変数（今の USB の形を保つ）。
func (u *Updater) gadgetEnv() []string {
	b := map[bool]string{false: "0", true: "1"}
	return []string{"GADGET_CHECK=1", "HID_MOUSE=" + b[u.usb.Mouse()], "GADGET_TERMINAL=0", "LEFTHAND_SELF=1"}
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}

// PrepareRollback は、.prev（前の版）を「受け取ったもの」として確かめる。
func (u *Updater) PrepareRollback() (*StageInfo, error) {
	if err := u.canChange(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(u.Bin + ".prev"); err != nil {
		return nil, fmt.Errorf("%w: there is no previous version (%s.prev)", errUpdateGone, u.Bin)
	}
	stage := newToken()
	dir := filepath.Join(u.Dir, stage)
	u.mu.Lock()
	u.clearStagesLocked("")
	u.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// 中身は写さず、リンクにする（入れ替えは rename なので、.prev のほうは残る）
	link := func(src, dst string) error {
		if err := os.Link(src, dst); err != nil {
			return copyFile(src, dst, 0o755)
		}
		return nil
	}
	if err := link(u.Bin+".prev", filepath.Join(dir, updateBinName)); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if _, err := os.Stat(u.Gadget + ".prev"); err == nil {
		if err := link(u.Gadget+".prev", filepath.Join(dir, updateGadgetName)); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	info, err := u.verify(dir, "rollback", "", true)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	info.Stage = stage
	u.mu.Lock()
	u.staged[stage] = info
	u.mu.Unlock()
	return info, nil
}

// Discard は、確かめ終えた更新を入れ替えずに消す（brain-deck update --dry-run）。
func (u *Updater) Discard(stage string) bool {
	if !stageRE.MatchString(stage) {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.staged[stage]
	delete(u.staged, stage)
	os.RemoveAll(filepath.Join(u.Dir, stage))
	return ok
}

// updatePlan は、係（-update-apply）への指示。<stage>/plan.json に書く。
type updatePlan struct {
	Stage     string `json:"stage"`
	Action    string `json:"action"` // update、rollback、restart
	Dir       string `json:"dir"`    // <data-dir>/update
	Unit      string `json:"unit"`
	Bin       string `json:"bin"`
	Gadget    string `json:"gadget"`
	StagedBin string `json:"staged_bin,omitempty"`
	StagedGad string `json:"staged_gadget,omitempty"`
	Version   string `json:"version,omitempty"` // 新しいデーモンが知らせるはずの版（空ならどれでもよい）
	From      string `json:"from"`              // 今の版
	Legacy    bool   `json:"legacy,omitempty"`  // 起動の知らせと update_confirm を待たない
	Confirm   bool   `json:"confirm"`           // PC からの update_confirm を待つ
	Rebind    bool   `json:"rebind,omitempty"`  // restart：USB を付け直す
	HIDMouse  bool   `json:"hid_mouse"`
	ReadyFile string `json:"ready_file"`
	Config    string `json:"config,omitempty"` // restart：起動しなければ、この設定を .prev に戻す（put_config_text で再起動するとき）
	// Restore は、restore：デーモンを止めているあいだに入れ替えるバックアップ（backup.go）
	Restore *restorePlan `json:"restore,omitempty"`
}

// Apply は、確かめ終えた更新の入れ替えを、係に始めさせる。
func (u *Updater) Apply(stage string) (*updatePlan, error) {
	u.mu.Lock()
	info, ok := u.staged[stage]
	u.mu.Unlock()
	if !ok || !stageRE.MatchString(stage) {
		return nil, fmt.Errorf("%w: stage %q is not ready (send the update again)", errUpdateGone, stage)
	}
	if err := u.canChange(); err != nil {
		return nil, err
	}
	dir := filepath.Join(u.Dir, stage)
	p := &updatePlan{Stage: stage, Action: info.Action, Dir: u.Dir, Unit: u.Unit, Bin: u.Bin, Gadget: u.Gadget,
		StagedBin: filepath.Join(dir, updateBinName), Version: info.Version, From: daemonVersion(),
		Legacy: info.Legacy, Confirm: !info.Legacy, HIDMouse: u.usb.Mouse(), ReadyFile: u.ReadyFile}
	if info.Legacy {
		p.Version = ""
	}
	if info.HasGadget {
		p.StagedGad = filepath.Join(dir, updateGadgetName)
	}
	if err := u.start(p); err != nil {
		return nil, err
	}
	u.mu.Lock()
	delete(u.staged, stage)
	u.mu.Unlock()
	return p, nil
}

// Restart は、デーモンの再起動（rebind なら USB の付け直しも）を、係に始めさせる。
// config が空でなければ、新しいデーモンが起動しないとき、その設定を .prev に戻して起動し直す。
func (u *Updater) Restart(rebind bool, config string) (*updatePlan, error) {
	if err := u.canChange(); err != nil {
		return nil, err
	}
	stage := newToken()
	u.mu.Lock()
	u.clearStagesLocked("")
	u.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(u.Dir, stage), 0o755); err != nil {
		return nil, err
	}
	p := &updatePlan{Stage: stage, Action: "restart", Dir: u.Dir, Unit: u.Unit, Bin: u.Bin, Gadget: u.Gadget,
		From: daemonVersion(), Rebind: rebind, HIDMouse: u.usb.Mouse(), ReadyFile: u.ReadyFile, Config: config}
	if err := u.start(p); err != nil {
		os.RemoveAll(filepath.Join(u.Dir, stage))
		return nil, err
	}
	return p, nil
}

// startRestore は、バックアップの復元（stage に置いたもの）を、係に始めさせる。PC からの update_confirm を待ち、
// 新しいデーモンが起動しなければ、入れ替えたものを元に戻す。
func (u *Updater) startRestore(stage string, rp *restorePlan) (*updatePlan, error) {
	if err := u.canChange(); err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.clearStagesLocked(stage)
	u.mu.Unlock()
	p := &updatePlan{Stage: stage, Action: "restore", Dir: u.Dir, Unit: u.Unit, Bin: u.Bin, Gadget: u.Gadget,
		From: daemonVersion(), Confirm: true, HIDMouse: u.usb.Mouse(), ReadyFile: u.ReadyFile, Restore: rp}
	if err := u.start(p); err != nil {
		return nil, err
	}
	return p, nil
}

// start は plan.json を書き、係を別のユニットで起動する。
func (u *Updater) start(p *updatePlan) error {
	b, _ := json.MarshalIndent(p, "", "  ")
	planPath := filepath.Join(u.Dir, p.Stage, updatePlanFile)
	if err := writeFileAtomic(planPath, b, false); err != nil {
		return err
	}
	saveUpdateState(u.Dir, updateState{Stage: p.Stage, Action: p.Action, Phase: "starting_helper", From: p.From, To: p.Version, Started: time.Now()})
	helper := u.Helper
	if helper == "" {
		helper = selfExe(filepath.Join(u.Dir, p.Stage))
	}
	if err := u.sys.systemdRun(updateHelperUnit, helper, "-update-apply", planPath); err != nil {
		saveUpdateState(u.Dir, updateState{Stage: p.Stage, Action: p.Action, Phase: "failed", Done: true, From: p.From,
			Message: err.Error(), Started: time.Now()})
		return err
	}
	log.Printf("update: %s %s started (%s)", p.Action, p.Stage, updateHelperUnit)
	return nil
}

// selfExe は、係として実行する自分の実行ファイル。消されていれば（入れ替えたあとなど）、dir に写す。
func selfExe(dir string) string {
	p, err := os.Executable()
	if err == nil && !strings.HasSuffix(p, " (deleted)") {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	dst := filepath.Join(dir, "helper")
	if err := copyFile("/proc/self/exe", dst, 0o755); err != nil {
		log.Printf("update: copying the helper: %v", err)
	}
	return dst
}

// Confirm は、PC の update_confirm を受けたしるしを書く（新しいデーモンで呼ばれる）。
func (u *Updater) Confirm(stage string) error {
	if !stageRE.MatchString(stage) {
		return fmt.Errorf("%w: bad stage %q", errUpdateBad, stage)
	}
	dir := filepath.Join(u.Dir, stage)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%w: stage %s is not being applied", errUpdateGone, stage)
	}
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "version": daemonVersion(), "at": time.Now()})
	return writeFileAtomic(filepath.Join(dir, updateConfirmFile), b, false)
}

// Status は、係の進み具合と、係が動いているか。
func (u *Updater) Status() map[string]any {
	st, err := loadUpdateState(u.Dir)
	running, _ := u.sys.systemctl("is-active", updateHelperUnit+".service")
	res := map[string]any{"version": daemonVersion(), "helper": running, "unit": u.Unit,
		"bin": u.Bin, "gadget": u.Gadget}
	if _, err := os.Stat(u.Bin + ".prev"); err == nil {
		res["has_prev"] = true
	}
	if err == nil {
		res["state"] = st
	}
	return res
}

// Power は、少し待ってから再起動（reboot）か電源断（poweroff）を頼む。返事を送り終えるため、待たずに返る。
func (u *Updater) Power(action string) error {
	if action != "reboot" && action != "poweroff" {
		return fmt.Errorf(`%w: "action" must be reboot or poweroff`, errUpdateBad)
	}
	if st, _ := u.sys.systemctl("is-active", updateHelperUnit+".service"); st == "active" || st == "activating" {
		return fmt.Errorf("%w: an update or restart is in progress (%s)", errUpdateBusy, updateHelperUnit)
	}
	log.Printf("power: %s requested", action)
	go func() {
		time.Sleep(powerDelay)
		if _, err := u.sys.systemctl("--no-block", action); err != nil {
			log.Printf("power: systemctl %s: %v", action, err)
		}
	}()
	return nil
}

// ---------- ログ（get_logs） ----------

const (
	logMaxLines = 2000
	logMaxBytes = 200 << 10
)

// Logs は journalctl でデーモンと係のログを読む。cursor があれば、そのあとだけ。結果と次の cursor を返す。
func (u *Updater) Logs(lines int, cursor string) (string, string, error) {
	if lines <= 0 {
		lines = 100
	}
	lines = min(lines, logMaxLines)
	unit := u.Unit
	if unit == "" {
		unit = "lefthand.service"
	}
	args := []string{"-u", unit, "-u", updateHelperUnit + ".service", "--no-pager", "-o", "short-iso", "--show-cursor", "-q"}
	if cursor != "" {
		args = append(args, "--after-cursor="+cursor)
	} else {
		args = append(args, "-n", strconv.Itoa(lines))
	}
	out, code, err := u.sys.command(nil, "journalctl", args...)
	if err != nil || code != 0 {
		return "", cursor, fmt.Errorf("journalctl: %v %s", err, strings.TrimSpace(out))
	}
	next := cursor
	var keep []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if c, ok := strings.CutPrefix(l, "-- cursor: "); ok {
			next = c
			continue
		}
		if l != "" {
			keep = append(keep, l)
		}
	}
	text := strings.Join(keep, "\n")
	if len(text) > logMaxBytes { // 長いときは、新しいほうを残す
		text = text[len(text)-logMaxBytes:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return text, next, nil
}

// ---------- 係の進み具合（state.json） ----------

type updateState struct {
	Stage    string    `json:"stage"`
	Action   string    `json:"action"`
	Phase    string    `json:"phase"` // starting_helper、stopping、installing、rebinding、starting、waiting_ready、waiting_confirm、done、rolling_back、rolled_back、failed
	Done     bool      `json:"done"`  // 終わった（成功・失敗とも）
	OK       bool      `json:"ok"`
	From     string    `json:"from,omitempty"`
	To       string    `json:"to,omitempty"`
	Rebound  bool      `json:"rebound,omitempty"` // USB を付け直した
	Message  string    `json:"message,omitempty"`
	Started  time.Time `json:"started"`
	Updated  time.Time `json:"updated"`
	Finished time.Time `json:"finished,omitzero"`
}

func saveUpdateState(dir string, st updateState) {
	st.Updated = time.Now()
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(dir, 0o755); err == nil {
		writeFileAtomic(filepath.Join(dir, updateStateFile), b, false)
	}
}

func loadUpdateState(dir string) (updateState, error) {
	var st updateState
	b, err := os.ReadFile(filepath.Join(dir, updateStateFile))
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	return st, err
}

// ---------- ファイルの入れ替え ----------

// copyFile は src を dst に写す（fsync してから rename）。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(mode)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// moveFile は src を dst に置き換える（同じファイルシステムなら rename、違えば写して消す）。
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if errors.Is(err, syscall.EXDEV) {
		if err = copyFile(src, dst, 0o755); err == nil {
			os.Remove(src)
		}
	}
	return err
}

// linkOrCopy は、dst に src と同じ中身を置く（できればハードリンク）。dst は置き換える。
func linkOrCopy(src, dst string) error {
	tmp := dst + ".lntmp"
	os.Remove(tmp)
	if err := os.Link(src, tmp); err != nil {
		return copyFile(src, dst, 0o755)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func syncDir(path string) {
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
}

// installed は、入れ替えた 1 つのファイル。元に戻すために、前の .prev（stash）を覚える。
type installed struct {
	target  string
	stash   string // 入れ替える前の target.prev（なければ空）
	changed bool   // 中身が変わった
}

// install は staged を target に入れ替え、今の target を target.prev に残す。前の target.prev は stash に移す。
func install(staged, target, stashDir string) (installed, error) {
	in := installed{target: target}
	newSum, _, _ := fileSHA256(staged)
	oldSum, _, _ := fileSHA256(target)
	in.changed = newSum != oldSum
	if _, err := os.Stat(target + ".prev"); err == nil {
		in.stash = filepath.Join(stashDir, "prev-"+filepath.Base(target))
		if err := linkOrCopy(target+".prev", in.stash); err != nil {
			return in, fmt.Errorf("keeping %s.prev: %w", target, err)
		}
	}
	if _, err := os.Stat(target); err == nil {
		if err := linkOrCopy(target, target+".prev"); err != nil {
			return in, fmt.Errorf("keeping the previous version as %s.prev: %w", target, err)
		}
	}
	os.Chmod(staged, 0o755)
	if err := moveFile(staged, target); err != nil {
		return in, fmt.Errorf("installing %s: %w", target, err)
	}
	syncDir(target)
	return in, nil
}

// restore は install を取り消す：target を target.prev（入れ替える前の版）に戻し、target.prev を stash に戻す。
func restore(in installed) error {
	if err := linkOrCopy(in.target+".prev", in.target); err != nil {
		return fmt.Errorf("restoring %s: %w", in.target, err)
	}
	if in.stash != "" {
		if err := moveFile(in.stash, in.target+".prev"); err != nil {
			return fmt.Errorf("restoring %s.prev: %w", in.target, err)
		}
	} else {
		os.Remove(in.target + ".prev")
	}
	syncDir(in.target)
	return nil
}
