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
	u, err := user.Lookup(tc.User)
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
	name := "/dev/pts/" + strconv.Itoa(int(n))
	pts, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, err
	}
	// 疑似端末をそのユーザーのものにする（ssh が端末を読み書きできるように）
	os.Chown(name, uid, gid)

	path, err := exec.LookPath(tc.Command[0])
	if err != nil {
		pts.Close()
		ptmx.Close()
		return nil, err
	}
	cmd := exec.Command(path, tc.Command[1:]...)
	cmd.Dir = u.HomeDir
	cmd.Env = []string{"TERM=xterm-256color", "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username,
		"LANG=en_US.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin", "SHELL=/bin/sh", "BRAIN_TERMINAL=1"}
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
