package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

// ---------- 疑似端末（terminal.command） ----------
//
// terminal.command のコマンド（ssh など）を、terminal.user の権限で、疑似端末（PTY）につないで動かす。
// ログインはしない（パスワードは聞かない）。root では動かさない（設定の検証で断る）。
// 端末の大きさは TIOCSWINSZ でそのまま伝わる（ssh は PC に伝える）。

type ptyConn struct {
	*os.File
	cmd  *exec.Cmd
	mu   sync.Mutex
	done chan struct{}
	err  error
}

func (*ptyConn) Kind() string { return "command" }

// Close は、コマンドにハングアップを送ってから閉じる。
func (p *ptyConn) Close() error {
	if p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGHUP)
	}
	return p.File.Close()
}

// Read は、コマンドが終わって読めなくなったら io.EOF の代わりにエラーを返す（Linux の PTY は EIO）。
func (p *ptyConn) Read(b []byte) (int, error) {
	n, err := p.File.Read(b)
	if err != nil && errors.Is(err, syscall.EIO) {
		<-p.done
	}
	return n, err
}

func (p *ptyConn) exitText() string {
	select {
	case <-p.done:
	default:
		return ""
	}
	var ee *exec.ExitError
	switch {
	case p.err == nil:
		return "（終了コード 0）"
	case errors.As(p.err, &ee):
		return fmt.Sprintf("（%s）", ee.ProcessState.String())
	}
	return "（" + p.err.Error() + "）"
}

type winsize struct{ Row, Col, X, Y uint16 }

// startPTY はコマンドを PTY につないで起動する。
func startPTY(tc TerminalConfig, cols, rows int) (termConn, error) {
	env := []string{"TERM=xterm-256color", "LANG=en_US.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin", "SHELL=/bin/sh", "BRAIN_TERMINAL=1"}
	return startUserPTY(tc.User, tc.Command, env, "", cols, rows)
}

// startUserPTY は、argv を name の権限で、PTY につないで起動する（root では動かさない）。env に HOME、USER、LOGNAME を足す。
// dir が空ならそのユーザーのホーム。日本語入力の uim-fep（ime.go）も、これで動かす。
func startUserPTY(name string, argv, env []string, dir string, cols, rows int) (*ptyConn, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if uid == 0 {
		return nil, errors.New("terminal.user must not be root")
	}
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, s := range ids {
			if g, err := strconv.Atoi(s); err == nil {
				groups = append(groups, uint32(g))
			}
		}
	}
	ptmx, pts, name, err := openPTY(cols, rows)
	if err != nil {
		return nil, err
	}
	// 疑似端末をそのユーザーのものにする（ssh が端末を読み書きできるように）
	os.Chown(name, uid, gid)

	if len(argv) == 0 {
		pts.Close()
		ptmx.Close()
		return nil, errors.New("no command")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		pts.Close()
		ptmx.Close()
		return nil, err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Dir = u.HomeDir
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(append([]string{}, env...), "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0,
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}}
	if err := cmd.Start(); err != nil {
		pts.Close()
		ptmx.Close()
		return nil, err
	}
	pts.Close()
	p := &ptyConn{File: ptmx, cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// startLoginPTY は、/bin/login <user> を PTY で動かす（Brain 自身にログインする。terminal: brain）。
// login は root で動かす必要があるので、デーモンの権限のまま起動する。login がパスワードを聞き、正しければ
// user のシェルを開く（root のシェルは開かない。-f は使わない）。user が root なら断る。
// openPTY は、cols × rows の疑似端末を開く（親側 ptmx、子側 pts とその名前）。
func openPTY(cols, rows int) (ptmx, pts *os.File, name string, err error) {
	ptmx, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, "", err
	}
	var n uint32
	var unlock int32
	if err := ioctl(int(ptmx.Fd()), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		ptmx.Close()
		return nil, nil, "", fmt.Errorf("unlockpt: %w", err)
	}
	if err := ioctl(int(ptmx.Fd()), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		ptmx.Close()
		return nil, nil, "", fmt.Errorf("ptsname: %w", err)
	}
	ws := winsize{Row: uint16(rows), Col: uint16(cols)}
	ioctl(int(ptmx.Fd()), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	name = "/dev/pts/" + strconv.Itoa(int(n))
	pts, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, "", err
	}
	return ptmx, pts, name, nil
}

// extraEnv は、login -p で user のシェルまで渡す環境変数（日本語入力の BRAIN_IME など。ime.go）。
func startLoginPTY(name string, cols, rows int, extraEnv []string) (termConn, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	if u.Uid == "0" || name == "root" {
		return nil, errors.New("terminal.brain_user must not be root")
	}
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	var n uint32
	var unlock int32
	if err := ioctl(int(ptmx.Fd()), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("unlockpt: %w", err)
	}
	if err := ioctl(int(ptmx.Fd()), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("ptsname: %w", err)
	}
	ws := winsize{Row: uint16(rows), Col: uint16(cols)}
	ioctl(int(ptmx.Fd()), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, err
	}
	path, err := exec.LookPath("login")
	if err != nil {
		pts.Close()
		ptmx.Close()
		return nil, err
	}
	// -p：環境変数を捨てない（login は HOME、SHELL、USER、LOGNAME を自分で決める。ここでは渡さない）。
	// BRAIN_TERMINAL=1 と BRAIN_IME は、/etc/profile.d/lefthand-ime.sh（contrib/brain）が見る
	cmd := exec.Command(path, "-p", "--", u.Username)
	cmd.Dir = "/"
	cmd.Env = append([]string{"TERM=xterm-256color", "LANG=en_US.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin", "BRAIN_TERMINAL=1"}, extraEnv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		pts.Close()
		ptmx.Close()
		return nil, err
	}
	pts.Close()
	p := &ptyConn{File: ptmx, cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}
