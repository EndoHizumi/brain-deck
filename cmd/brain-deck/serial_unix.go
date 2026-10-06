//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// ---------- シリアルポート ----------
//
// 設定 GUI と同じポート（Brain 側 /dev/ttyGS1）を使う。同じポートを 2 つのプロセスが開くと、
// 返事がどちらに届くか決まらず、両方の通信が壊れる。そこで次のようにして、同時には使わない。
//
//   - 開いたらすぐ TIOCEXCL を設定する。以後、ほかのプロセス（root を除く）がこのポートを開こうとすると EBUSY になる。
//     Chrome の WebSerial も、Linux と macOS ではポートを開くと TIOCEXCL を設定する（Chromium の
//     services/device/serial/serial_io_handler_posix.cc の PostOpen）。そのため、設定 GUI が接続しているあいだは、
//     brain-deck がポートを開くと EBUSY になる。これを「設定 GUI が接続中」として扱う。
//   - brain-deck どうし（cron とビルドのスクリプトが同時に動いたなど）は、ロックファイルの flock で順番を待つ。
//     TIOCEXCL だけでは、相手が GUI か brain-deck か区別できないため。
//   - Linux では、開けたときも /proc を見て、ほかにこのポートを開いているプロセス（TIOCEXCL を使わないプログラム、
//     root で動くプログラム）がいれば使わない。

// errBusy は、ほかのプロセスがポートを使っていること。
var errBusy = errors.New("port is busy")

// openPort はポートを生のモードで開き、ほかのプロセスが開けないようにする。
func openPort(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("%s: %w", path, errBusy)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// 開いてから TIOCEXCL を設定するまでのあいだに、ほかのプロセスが開いていないかは、あとで holders で確かめる
	if err := ioctl(fd, syscall.TIOCEXCL, 0); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("%s: TIOCEXCL: %w", path, err)
	}
	if err := makeRaw(fd); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("%s: set raw mode: %w", path, err)
	}
	// O_NONBLOCK のまま os.NewFile にすると、Go の poller（epoll、kqueue）に載り、読み書きにデッドラインを使える
	return os.NewFile(uintptr(fd), path), nil
}

// closePort は TIOCEXCL を外してから閉じる。
// ttyACM では最後に閉じたときに外れるが、PTY などでは閉じても残り、開き直せなくなるため。
func closePort(f *os.File) error {
	if rc, err := f.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { ioctl(int(fd), syscall.TIOCNXCL, 0) })
	}
	return f.Close()
}

func ioctl(fd int, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg); e != 0 {
		return e
	}
	return nil
}

// makeRaw は、エコーなし、行の編集なし、改行の変換なしにする（デーモン側の makeRaw と同じ）。
func makeRaw(fd int) error {
	var t syscall.Termios
	if err := ioctl(fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t))); err != nil {
		return err
	}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return ioctl(fd, ioctlSetTermios, uintptr(unsafe.Pointer(&t)))
}

// ---------- brain-deck どうしの順番待ち ----------

// lockPath は、brain-deck どうしで順番を待つためのロックファイル。
func lockPath() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "brain-deck.lock")
	}
	return filepath.Join(os.TempDir(), "brain-deck-"+strconv.Itoa(os.Getuid())+".lock")
}

// acquireLock は、ほかの brain-deck が終わるのを deadline まで待つ。返した関数で離す。
func acquireLock(deadline time.Time) (func(), error) {
	p := lockPath()
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return func() {}, nil // ロックファイルを作れない環境でも、TIOCEXCL で守られる
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return func() {}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another brain-deck is still running (%s)", p)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
