package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	evdev "github.com/holoplot/go-evdev"
)

// ---------- 設定 GUI との通信（USB シリアル /dev/ttyGS1） ----------
//
// 1 行に 1 つの JSON（改行区切り、UTF-8）。仕様は docs/protocol.md。
// 入力の処理（キーボードとタッチの goroutine）は、ここを一切待たない。
// 通信が詰まったり切れたりしても、キー入力の変換は止まらない。

const (
	protocolVersion = 1
	maxLineBytes    = 256 << 10        // 1 行（リクエスト 1 つ）の上限
	suppressLease   = 30 * time.Second // 学習モードで入力を止めておく時間（リクエストのたびに延びる）
	eventQueueLen   = 64               // 入力通知の待ち行列。あふれたら捨てる
	reopenDelay     = time.Second      // 開けなかった・切れたときに開き直すまでの時間
	missingLogEvery = 10 * time.Minute // デバイスがないことをログに出す間隔
	ioChunk         = 4096             // 1 回の read と write の大きさ
)

// テストで短くするので変数にしてある
var (
	partialTimeout = 2 * time.Second // 改行が来ないまま止まった行を捨てるまでの時間
	writeTimeout   = 5 * time.Second // PC が読まないときに、書き込みをあきらめるまでの時間
)

// エラーの種類（response.error.code）
const (
	errParse      = "parse_error"     // JSON として読めない
	errBadRequest = "bad_request"     // id や cmd がない、引数がおかしい
	errUnknownCmd = "unknown_command" //
	errTooLarge   = "too_large"       // 行が maxLineBytes を超えた
	errIncomplete = "incomplete_line" // 改行が来ないまま partialTimeout が過ぎた
	errInvalid    = "invalid_config"  // 設定の誤り。problems に場所付きで入る
	errApply      = "apply_failed"    // 保存したが反映に失敗し、前の設定に戻した
	errInternal   = "internal_error"  //
)

// 通知の種類（event）
const (
	notifyInput = "input"
	notifyLayer = "layer"
)

// ---------- 行の組み立て ----------

// lineSplitter は受け取ったバイト列を行に分ける。上限を超えた行は、次の改行まで読み捨てる。
type lineSplitter struct {
	max        int
	buf        []byte
	discarding bool // 上限を超えた行の残りを読み捨てている
}

// feed は chunk を加え、そろった行ごとに line を、上限を超えた行ごとに tooLong を呼ぶ。
func (s *lineSplitter) feed(chunk []byte, line func([]byte), tooLong func()) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		part := chunk
		if i >= 0 {
			part = chunk[:i]
		}
		if !s.discarding {
			if len(s.buf)+len(part) > s.max {
				s.discarding = true
				s.buf = s.buf[:0]
				tooLong()
			} else {
				s.buf = append(s.buf, part...)
			}
		}
		if i < 0 {
			return
		}
		if !s.discarding {
			l := bytes.TrimRight(s.buf, "\r")
			if len(bytes.TrimSpace(l)) > 0 { // 空行は無視する（接続直後の区切りに使う）
				line(append([]byte(nil), l...))
			}
		}
		s.buf = s.buf[:0]
		s.discarding = false
		chunk = chunk[i+1:]
	}
}

// pending は、改行を待っている途中の行があるかどうか。
func (s *lineSplitter) pending() bool { return len(s.buf) > 0 || s.discarding }

// drop は途中の行を捨てる。
func (s *lineSplitter) drop() { s.buf, s.discarding = s.buf[:0], false }

// ---------- メッセージ ----------

type request struct {
	ID       json.RawMessage `json:"id"`
	Cmd      string          `json:"cmd"`
	Config   json.RawMessage `json:"config,omitempty"` // validate、set_config：設定（JSON のオブジェクト）
	Text     *string         `json:"text,omitempty"`   // validate：YAML か JSON の文字列（ファイルの読み込み）
	Enable   *bool           `json:"enable,omitempty"` // subscribe_input
	Suppress bool            `json:"suppress,omitempty"`
	UnixMS   *int64          `json:"unix_ms,omitempty"` // set_time：合わせる時刻（UNIX 時間のミリ秒）
	Source   string          `json:"source,omitempty"`  // set_time：送った側の名前（記録用）
}

type response struct {
	ID     json.RawMessage `json:"id"`
	OK     bool            `json:"ok"`
	Result any             `json:"result,omitempty"`
	Error  *protoError     `json:"error,omitempty"`
}

type protoError struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Problems Problems `json:"problems,omitempty"`
}

var nullID = json.RawMessage("null")

// ---------- 入力の通知（学習モード） ----------

// InputEvent は Brain のキーとタッチの通知。
type InputEvent struct {
	Event string `json:"event"`          // "input"
	Type  string `json:"type"`           // "key" か "touch"
	Code  string `json:"code,omitempty"` // key：KEY_* 名
	X     int32  `json:"x,omitempty"`    // touch：生の座標
	Y     int32  `json:"y,omitempty"`
	Col   *int   `json:"col,omitempty"`  // touch：今の格子でのセル
	Row   *int   `json:"row,omitempty"`  //
	Soft  string `json:"soft,omitempty"` // touch：ソフトキーの範囲に入っていればその名前
	Layer string `json:"layer"`          // 今のレイヤー
	// Suppressed は、学習モードのため PC に送らなかったこと
	Suppressed bool `json:"suppressed,omitempty"`
}

// Monitor は入力を設定 GUI に知らせる。入力の goroutine からは、待たずに返る。
type Monitor struct {
	mu            sync.Mutex
	ch            chan []byte // 購読中の通知先。nil なら購読していない
	suppressUntil time.Time   // この時刻まで、押した入力を PC に送らない（学習モード）
}

// subscribe は通知先を設定する。ch が nil なら購読をやめる。
func (m *Monitor) subscribe(ch chan []byte, suppress bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ch = ch
	m.suppressUntil = time.Time{}
	if ch != nil && suppress {
		m.suppressUntil = time.Now().Add(suppressLease)
	}
}

// renew は学習モードの期限を延ばす（GUI から何かリクエストが来るたびに呼ぶ）。
func (m *Monitor) renew() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.suppressUntil.IsZero() {
		m.suppressUntil = time.Now().Add(suppressLease)
	}
}

// suppressing は、押した入力を PC に送らずに知らせるだけにするかどうか。
func (m *Monitor) suppressing() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ch != nil && time.Now().Before(m.suppressUntil)
}

func (m *Monitor) subscribed() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ch != nil
}

func (m *Monitor) publish(v any) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ch == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case m.ch <- b:
	default: // PC が読まないときは捨てる。入力の処理は待たせない
	}
}

// KeyPressed は本体キーが押されたときに、エンジンより先に呼ぶ。
// true を返したら、エンジンに渡さない（学習モード）。離したときは呼ばない（いつもエンジンに渡す）。
func (m *Monitor) KeyPressed(e *Engine, code evdev.EvCode) bool {
	if !m.subscribed() {
		return false
	}
	sup := m.suppressing()
	m.publish(InputEvent{Event: notifyInput, Type: "key", Code: evdev.CodeName(evdev.EV_KEY, code),
		Layer: e.Status().Layer, Suppressed: sup})
	return sup
}

// TouchPressed はタッチしたときに、エンジンより先に呼ぶ。true ならエンジンに渡さない。
func (m *Monitor) TouchPressed(e *Engine, x, y int32) bool {
	if !m.subscribed() {
		return false
	}
	sup := m.suppressing()
	h := e.HitTest(x, y)
	ev := InputEvent{Event: notifyInput, Type: "touch", X: x, Y: y, Soft: h.Soft,
		Layer: e.Status().Layer, Suppressed: sup}
	if h.Col >= 0 {
		ev.Col, ev.Row = &h.Col, &h.Row
	}
	m.publish(ev)
	return sup
}

// LayerChanged はレイヤーが変わったときに呼ぶ。
func (m *Monitor) LayerChanged(st EngineStatus) {
	m.publish(struct {
		Event string `json:"event"`
		EngineStatus
	}{notifyLayer, st})
}

// ---------- コマンドの処理 ----------

// Controller は設定 GUI からのリクエストを処理する。
type Controller struct {
	store   *configStore
	engine  *Engine
	monitor *Monitor
	clock   *TimeService
	started time.Time
}

func (c *Controller) handle(line []byte, events chan []byte) response {
	if !utf8.Valid(line) {
		return errResp(nullID, errParse, "the line is not valid UTF-8", nil)
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errResp(nullID, errParse, "invalid JSON: "+err.Error(), nil)
	}
	id := req.ID
	if len(id) == 0 || !(id[0] == '"' || id[0] == '-' || (id[0] >= '0' && id[0] <= '9')) {
		return errResp(nullID, errBadRequest, `"id" (a number or a string) is required`, nil)
	}
	c.monitor.renew()
	ok := func(v any) response { return response{ID: id, OK: true, Result: v} }

	switch req.Cmd {
	case "hello":
		return ok(map[string]any{
			"protocol": protocolVersion, "daemon": "lefthand", "version": daemonVersion(),
			"max_line": maxLineBytes, "config_path": c.store.path,
			"commands": []string{"hello", "get_config", "validate", "set_config", "get_keymap", "get_status", "subscribe_input", "set_time"},
		})
	case "get_config":
		return ok(map[string]any{"config": c.store.Current(), "path": c.store.path})
	case "validate":
		raw, err := configArg(req)
		if err != nil {
			return errResp(id, errBadRequest, err.Error(), nil)
		}
		cfg, _, warns, err := c.store.Validate(raw)
		res := map[string]any{"valid": err == nil, "errors": Problems{}, "warnings": nonNil(warns)}
		if err != nil {
			res["errors"] = asProblems(err)
		} else {
			res["config"] = cfg // 旧形式や YAML を、layers の形にそろえたもの
		}
		return ok(res)
	case "set_config":
		if req.Text != nil {
			return errResp(id, errBadRequest, `set_config takes "config" (a JSON object), not "text"`, nil)
		}
		raw, err := configArg(req)
		if err != nil {
			return errResp(id, errBadRequest, err.Error(), nil)
		}
		warns, err := c.store.Set(raw)
		var p Problems
		switch {
		case err == nil:
			return ok(map[string]any{"saved": c.store.path, "previous": c.store.path + ".prev",
				"warnings": nonNil(warns), "status": c.engine.Status()})
		case errors.As(err, &p):
			return errResp(id, errInvalid, "the config is not valid; nothing was changed", p)
		case errors.Is(err, errRolledBack):
			return errResp(id, errApply, err.Error(), nil)
		default:
			return errResp(id, errInternal, err.Error(), nil)
		}
	case "get_keymap":
		return ok(pwsh2Keymap())
	case "get_status":
		return ok(map[string]any{"status": c.engine.Status(), "uptime_sec": int(time.Since(c.started).Seconds()),
			"subscribed": c.monitor.subscribed(), "suppressing": c.monitor.suppressing(), "time": c.clock.Info()})
	case "set_time":
		if req.UnixMS == nil {
			return errResp(id, errBadRequest, `"unix_ms" (milliseconds since 1970-01-01 UTC) is required`, nil)
		}
		src := req.Source
		if src == "" {
			src = "unknown"
		}
		res, err := c.clock.Set(time.UnixMilli(*req.UnixMS), src)
		switch {
		case errors.Is(err, errBadTime):
			return errResp(id, errBadRequest, err.Error(), nil)
		case err != nil:
			return errResp(id, errInternal, "setting the system clock failed: "+err.Error(), nil)
		}
		return ok(res)
	case "subscribe_input":
		on := req.Enable == nil || *req.Enable
		if on {
			c.monitor.subscribe(events, req.Suppress)
		} else {
			c.monitor.subscribe(nil, false)
		}
		return ok(map[string]any{"subscribed": on, "suppress": on && req.Suppress,
			"lease_sec": int(suppressLease.Seconds())})
	case "":
		return errResp(id, errBadRequest, `"cmd" is required`, nil)
	default:
		return errResp(id, errUnknownCmd, fmt.Sprintf("unknown command %q", req.Cmd), nil)
	}
}

func configArg(req request) ([]byte, error) {
	switch {
	case req.Text != nil && req.Config != nil:
		return nil, errors.New(`write either "config" or "text", not both`)
	case req.Text != nil:
		return []byte(*req.Text), nil
	case len(req.Config) > 0 && req.Config[0] == '{':
		return req.Config, nil // JSON は YAML としてそのまま読める
	default:
		return nil, errors.New(`"config" (a JSON object) is required`)
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func errResp(id json.RawMessage, code, msg string, p Problems) response {
	return response{ID: id, Error: &protoError{Code: code, Message: msg, Problems: p}}
}

// ---------- 1 つの接続 ----------

// Serve は rw との間でリクエストを処理する。読み書きのどちらかが失敗したら返る。
// rw がデッドラインに対応していれば（*os.File の tty など）、書き込みが詰まったときにあきらめる。
func (c *Controller) Serve(rw io.ReadWriter) error {
	events := make(chan []byte, eventQueueLen)
	defer c.monitor.unsubscribeIf(events)

	out := make(chan []byte, 16)
	writeErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() { // 書き込みは 1 つの goroutine だけが行い、行が混ざらないようにする
		for {
			var b []byte
			select {
			case b = <-out:
			case b = <-events:
			case <-done:
				return
			}
			if err := writeLine(rw, b); err != nil {
				writeErr <- err
				return
			}
		}
	}()
	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			b, _ = json.Marshal(errResp(nullID, errInternal, err.Error(), nil))
		}
		select {
		case out <- b:
			return nil
		case err := <-writeErr:
			return err
		}
	}

	chunks := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			buf := make([]byte, ioChunk)
			n, err := rw.Read(buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-done:
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	sp := lineSplitter{max: maxLineBytes}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case chunk := <-chunks:
			var lines [][]byte
			tooLong := 0
			sp.feed(chunk, func(l []byte) { lines = append(lines, l) }, func() { tooLong++ })
			for i := 0; i < tooLong; i++ {
				if err := send(errResp(nullID, errTooLarge,
					fmt.Sprintf("the line exceeds %d bytes and was discarded", maxLineBytes), nil)); err != nil {
					return err
				}
			}
			for _, l := range lines {
				if err := send(c.handle(l, events)); err != nil {
					return err
				}
			}
			timer.Stop()
			if sp.pending() {
				timer.Reset(partialTimeout)
			}
		case <-timer.C:
			if sp.pending() {
				sp.drop()
				if err := send(errResp(nullID, errIncomplete,
					fmt.Sprintf("no newline within %v; the incomplete line was discarded", partialTimeout), nil)); err != nil {
					return err
				}
			}
		case err := <-readErr:
			return err
		case err := <-writeErr:
			return err
		}
	}
}

func (m *Monitor) unsubscribeIf(ch chan []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ch == ch {
		m.ch = nil
		m.suppressUntil = time.Time{}
	}
}

type deadliner interface{ SetWriteDeadline(time.Time) error }

// writeLine は b と改行を書く。大きな行も最後まで書くか、エラーで返る。
func writeLine(w io.Writer, b []byte) error {
	b = append(b, '\n')
	for len(b) > 0 {
		n := min(len(b), ioChunk)
		if d, ok := w.(deadliner); ok {
			d.SetWriteDeadline(time.Now().Add(writeTimeout))
		}
		m, err := w.Write(b[:n])
		b = b[m:]
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------- シリアルポート ----------

// openSerial はシリアルを開き、生のモード（エコーなし、行の編集なし）にする。
func openSerial(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err == nil {
		cerr := rc.Control(func(fd uintptr) { err = makeRaw(int(fd)) })
		if err == nil {
			err = cerr
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("set raw mode: %w", err)
	}
	return f, nil
}

func makeRaw(fd int) error {
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t))); err != nil {
		return err
	}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return ioctl(fd, syscall.TCSETS, uintptr(unsafe.Pointer(&t)))
}

// runSerial はシリアルを開いて Serve し、切れたら開き直す。戻らない。
// デバイスがない（ガジェットの ACM が 1 つしかない）ときも、入力の処理には影響しない。
func (c *Controller) runSerial(path string) {
	var lastMissing time.Time
	for {
		f, err := openSerial(path)
		if err != nil {
			if time.Since(lastMissing) > missingLogEvery {
				log.Printf("control: %v (settings GUI unavailable; retrying)", err)
				lastMissing = time.Now()
			}
			time.Sleep(reopenDelay)
			continue
		}
		lastMissing = time.Time{}
		log.Printf("control: listening on %s", path)
		err = c.Serve(f)
		f.Close()
		vlogf("control: %s closed: %v", path, err)
		time.Sleep(reopenDelay)
	}
}
