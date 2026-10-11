package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 起動にかかった時間（フェーズ K4） ----------
//
// デーモンの起動の段階ごとの時間を覚え、起動を終えたとき（/run/lefthand-ready を書いたとき）に 1 行でログに出す。
// 「proc」はプロセスができてから main に入るまで（Go のランタイムと、パッケージの初期化）。

type startupTrace struct {
	mu    sync.Mutex
	start time.Time // main に入ったとき
	last  time.Time
	marks []string
	done  bool
}

var bootTrace = newStartupTrace()

func newStartupTrace() *startupTrace {
	t := time.Now()
	return &startupTrace{start: t, last: t}
}

// mark は、前の mark からの時間を name として覚える。
func (t *startupTrace) mark(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	now := time.Now()
	t.marks = append(t.marks, fmt.Sprintf("%s %s", name, fmtMs(now.Sub(t.last))))
	t.last = now
}

// finish は、起動を終えたときに、段階ごとの時間を 1 行でログに出す。
func (t *startupTrace) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	pre := ""
	if d, ok := sinceProcStart(); ok {
		pre = fmt.Sprintf("total %s since exec (proc %s), ", fmtMs(d), fmtMs(d-time.Since(t.start)))
	}
	log.Printf("startup: %sready %s after main: %s", pre, fmtMs(time.Since(t.start)), strings.Join(t.marks, ", "))
}

func fmtMs(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// sinceProcStart は、このプロセスができてからの時間（/proc/self/stat の starttime と /proc/uptime から）。
func sinceProcStart() (time.Duration, bool) {
	st, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	up, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	// comm に空白が入ることがあるので、最後の ')' のあとから数える。starttime は 22 番目
	s := string(st)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseFloat(f[19], 64)
	if err != nil {
		return 0, false
	}
	u := strings.Fields(string(up))
	if len(u) == 0 {
		return 0, false
	}
	now, err := strconv.ParseFloat(u[0], 64)
	if err != nil {
		return 0, false
	}
	const hz = 100 // USER_HZ（Linux の既定）
	return time.Duration((now - ticks/hz) * float64(time.Second)), true
}
