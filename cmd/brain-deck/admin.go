package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ---------- 更新、再起動、電源、ログ、設定（ssh を使わずに Brain を保守する） ----------
//
// デーモンを止めたり USB を付け直したりすると、シリアルも切れる。brain-deck は返事を受けてからポートを閉じ、
// ロック（ほかの brain-deck との順番待ち）は持ったまま、つなぎ直して結果を確かめる（--timeout まで）。

// exitFailed は、更新や再起動が失敗したこと（更新なら、Brain は前の版に戻した）。
const exitFailed = 8

// 時間の上限の既定（--timeout を書かなかったとき）
const (
	updateTimeout  = 5 * time.Minute // 送る、確かめる、入れ替える、つなぎ直す
	restartTimeout = 2 * time.Minute
	rebootTimeout  = 4 * time.Minute // 起動してキーを受け付けるまで約 70 秒
	goneTimeout    = time.Minute     // reboot、poweroff で、Brain のポートが消えるまで
)

// helperSettle は、係がデーモンを止めるまで待つ時間（止める前の古いデーモンと話さないよう）。テストで短くする。
var helperSettle = 1500 * time.Millisecond

// session は、ロックを持ったまま、切れても開き直せる接続。
type session struct {
	o        *options
	c        *Client
	unlock   func()
	deadline time.Time
	stderr   io.Writer
	busySaid bool
}

func openSession(o *options, stderr io.Writer, timeout time.Duration) (*session, error) {
	if o.timeout != defaultTimeout {
		timeout = o.timeout
	}
	deadline := time.Now().Add(timeout)
	c, unlock, err := connect(connectOptions{port: o.port, timeout: timeout, verbose: o.verbose, deadline: deadline})
	if err != nil {
		return nil, err
	}
	callTimeout = func() time.Duration { return max(time.Until(deadline), 100*time.Millisecond) }
	return &session{o: o, c: c, unlock: unlock, deadline: deadline, stderr: stderr}, nil
}

func (s *session) close() {
	s.drop()
	s.unlock()
}

// drop はポートを閉じる（ロックは持ったまま）。
func (s *session) drop() {
	if s.c != nil {
		s.c.Close()
		s.c = nil
	}
}

func (s *session) note(format string, args ...any) {
	if !s.o.quiet {
		fmt.Fprintf(s.stderr, "brain-deck: "+format+"\n", args...)
	}
}

// call は、残りの時間（ただし max まで）で呼ぶ。
func (s *session) call(cmd string, params map[string]any, limit time.Duration) (json.RawMessage, error) {
	return s.c.Call(cmd, params, min(limit, max(time.Until(s.deadline), 100*time.Millisecond)))
}

// redial は、つながるまで開き直す（deadline まで）。設定 GUI が接続中のあいだも待つ。
func (s *session) redial() error {
	s.drop()
	var last error = errNoReply
	for time.Now().Before(s.deadline) {
		p := s.o.port
		if p == "" {
			var err error
			if p, err = settingsPort(); err != nil {
				last = err
				time.Sleep(time.Second)
				continue
			}
		}
		c, err := tryPort(p, connectOptions{port: p, verbose: s.o.verbose, deadline: time.Now().Add(helloTimeout)})
		if err == nil {
			s.c = c
			return nil
		}
		var be *busyError
		if errors.As(err, &be) && !s.busySaid {
			s.note("%s。終わるのを待っています", strings.Split(describeBusy(be), "\n")[0])
			s.busySaid = true
		}
		last = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%w (Brain につなぎ直せませんでした：%v)", errNoReply, last)
}

// portPresent は、Brain の設定用のポートがあるか。
func (s *session) portPresent() bool {
	if s.o.port != "" {
		_, err := os.Stat(s.o.port)
		return err == nil
	}
	_, err := settingsPort()
	return err == nil
}

// ---------- 係の進み具合 ----------

type updateState struct {
	Stage   string `json:"stage"`
	Action  string `json:"action"`
	Phase   string `json:"phase"`
	Done    bool   `json:"done"`
	OK      bool   `json:"ok"`
	From    string `json:"from"`
	To      string `json:"to"`
	Rebound bool   `json:"rebound"`
	Message string `json:"message"`
}

var phaseText = map[string]string{
	"starting_helper": "係を起動しています", "stopping": "lefthand を止めています", "installing": "入れ替えています",
	"rebinding": "USB を付け直しています", "starting": "lefthand を起動しています", "waiting_ready": "新しい lefthand の起動を待っています",
	"waiting_confirm": "つなぎ直しました。確かめています", "rolling_back": "前の版に戻しています",
	"restoring": "入れ替えています（復元）",
}

// waitHelper は、Brain の係（stage）が終わるまで、つなぎ直しながら待つ。confirm なら、係が待っているときに
// update_confirm を送る（新しいデーモンと話せたしるし）。
func (s *session) waitHelper(stage string, confirm bool, legacyVersion string) (updateState, error) {
	s.drop()
	time.Sleep(helperSettle)
	lastPhase := ""
	for {
		if s.c == nil {
			if err := s.redial(); err != nil {
				return updateState{}, err
			}
		}
		if !s.c.Hello.has("update_status") {
			// 起動を知らせない古い版に戻した。版で確かめ、動き続けるのを待つ
			if legacyVersion != "" && s.c.Hello.Version != "" {
				s.note("前の版（%s）が答えました。動き続けるか確かめています", s.c.Hello.Version)
				time.Sleep(12 * time.Second)
				if err := s.redial(); err != nil {
					return updateState{}, err
				}
				return updateState{Stage: stage, Done: true, OK: true, To: s.c.Hello.Version, Message: "legacy"}, nil
			}
			return updateState{}, errors.New("Brain の lefthand が update_status に対応していません")
		}
		raw, err := s.call("update_status", nil, 5*time.Second)
		if err != nil {
			s.drop()
			time.Sleep(time.Second)
			continue
		}
		var st struct {
			State   *updateState `json:"state"`
			Version string       `json:"version"`
		}
		json.Unmarshal(raw, &st)
		if st.State != nil && st.State.Stage == stage {
			if st.State.Done {
				return *st.State, nil
			}
			if st.State.Phase != lastPhase {
				if t, ok := phaseText[st.State.Phase]; ok {
					s.note("%s", t)
				}
				lastPhase = st.State.Phase
			}
			if confirm && st.State.Phase == "waiting_confirm" {
				if _, err := s.call("update_confirm", map[string]any{"stage": stage, "source": "brain-deck"}, 5*time.Second); err != nil {
					s.drop()
				}
			}
		}
		if time.Now().After(s.deadline) {
			return updateState{}, fmt.Errorf("%w（%v たっても終わりませんでした。brain-deck update --status で確かめてください）", errNoReply, s.o.timeout)
		}
		time.Sleep(time.Second)
	}
}

// ---------- update ----------

type stageInfo struct {
	Stage          string   `json:"stage"`
	Action         string   `json:"action"`
	Version        string   `json:"version"`
	CurrentVersion string   `json:"current_version"`
	BinChanged     bool     `json:"bin_changed"`
	GadgetChanged  bool     `json:"gadget_changed"`
	HasGadget      bool     `json:"has_gadget"`
	Rebind         string   `json:"rebind"`
	RebindReasons  []string `json:"rebind_reasons"`
	Legacy         bool     `json:"legacy"`
	Warnings       []string `json:"warnings"`
	BuiltAt        string   `json:"built_at"`
}

func (i stageInfo) describe() string {
	var b strings.Builder
	if i.Action == "rollback" {
		fmt.Fprintf(&b, "前の版に戻します：lefthand %s → %s", i.CurrentVersion, i.Version)
	} else {
		fmt.Fprintf(&b, "確かめました：lefthand %s → %s", i.CurrentVersion, i.Version)
		if !i.BinChanged {
			b.WriteString("（同じ実行ファイル）")
		}
	}
	switch {
	case !i.HasGadget:
		b.WriteString("。gadget-setup.sh は入れ替えません")
	case !i.GadgetChanged:
		b.WriteString("。gadget-setup.sh は同じ")
	case i.Rebind == "yes":
		b.WriteString("。gadget-setup.sh が変わり、USB の付け直しが要ります（" + strings.Join(i.RebindReasons, "、") + "）。数秒、キー入力と SSH とシリアルが切れます")
	case i.Rebind == "maybe":
		b.WriteString("。gadget-setup.sh が変わります（要れば USB を付け直します）")
	default:
		b.WriteString("。gadget-setup.sh が変わりますが、USB の付け直しは要りません")
	}
	for _, w := range i.Warnings {
		b.WriteString("\n注意（新しい版で今の設定を確かめた結果）：" + w)
	}
	return b.String()
}

func runUpdate(o *options, stdout, stderr io.Writer) int {
	a := o.args[1:]
	switch {
	case o.status:
		if len(a) != 0 {
			return report(stderr, usageError("update --status は引数を取りません"))
		}
		return connectAndRun(o, stdout, stderr, updateStatus)
	case o.rollback && len(a) != 0:
		return report(stderr, usageError("update --rollback はファイルを取りません"))
	case !o.rollback && len(a) != 1:
		return report(stderr, usageError("update のあとに、更新のファイル（go run ./tools/mkupdate が作る .update.tar.gz）を 1 つ書きます"))
	}
	var data []byte
	if !o.rollback {
		var err error
		if data, err = os.ReadFile(a[0]); err != nil {
			return report(stderr, usageError(err.Error()))
		}
		if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
			return report(stderr, usageError(a[0]+" は更新のファイル（.update.tar.gz）ではありません。go run ./tools/mkupdate で作ります"))
		}
	}
	s, err := openSession(o, stderr, updateTimeout)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	if err := need(s.c, "update_apply"); err != nil {
		return report(stderr, &brainError{Code: "unknown_command", Message: fmt.Sprintf(
			"Brain の lefthand（%s）は brain-deck update に対応していません。最初の一度だけ、README の「インストール」の手順で入れ替えてください", s.c.Hello.Version)})
	}

	var raw json.RawMessage
	if o.rollback {
		raw, err = s.call("update_rollback", nil, 60*time.Second)
	} else {
		raw, err = s.upload("update", data, a[0])
	}
	if err != nil {
		return report(stderr, err)
	}
	var info stageInfo
	json.Unmarshal(raw, &info)
	s.note("%s", info.describe())
	if o.dryRun {
		s.call("update_discard", map[string]any{"stage": info.Stage}, 5*time.Second)
		fmt.Fprintln(stdout, "--dry-run なので、入れ替えずに消しました")
		return exitOK
	}
	if _, err := s.call("update_apply", map[string]any{"stage": info.Stage}, 10*time.Second); err != nil {
		return report(stderr, err)
	}
	s.note("入れ替えます。lefthand を止めるので、シリアルが一度切れます（つなぎ直して確かめます）")
	legacy := ""
	if info.Legacy {
		legacy = info.Version
	}
	st, err := s.waitHelper(info.Stage, !info.Legacy, legacy)
	if err != nil {
		return report(stderr, err)
	}
	if !st.OK {
		fmt.Fprintf(stderr, "brain-deck: 入れ替えに失敗したので、前の版（%s）に戻しました：%s\n詳しくは brain-deck logs で見られます\n", st.From, st.Message)
		return exitFailed
	}
	msg := fmt.Sprintf("更新しました：lefthand %s（前の版 %s は .prev に残しました。戻すときは brain-deck update --rollback）", info.Version, info.CurrentVersion)
	if info.Action == "rollback" {
		msg = fmt.Sprintf("前の版に戻しました：lefthand %s（%s は .prev に残しました。もう一度 --rollback で戻せます）", info.Version, info.CurrentVersion)
	}
	if st.Rebound {
		msg += "。USB を付け直しました"
	}
	if !o.quiet {
		fmt.Fprintln(stdout, msg)
	}
	return exitOK
}

// upload は、kind のファイルを upload_* で送り、upload_end の結果を返す。
func (s *session) upload(kind string, data []byte, name string) (json.RawMessage, error) {
	ok := false
	for _, k := range s.c.Hello.UploadKinds {
		ok = ok || k == kind
	}
	if !ok {
		return nil, &brainError{Code: "unknown_command", Message: fmt.Sprintf("Brain の lefthand（%s）は %s を受け取れません", s.c.Hello.Version, kind)}
	}
	sum := sha256.Sum256(data)
	raw, err := s.call("upload_begin", map[string]any{"kind": kind, "sha256": hex.EncodeToString(sum[:]), "bytes": len(data),
		"name": name, "source": "brain-deck"}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var r struct {
		Upload string `json:"upload"`
		Chunk  int    `json:"chunk_bytes"`
	}
	json.Unmarshal(raw, &r)
	if r.Chunk <= 0 {
		return nil, errors.New("upload_begin returned no chunk size")
	}
	t0, lastNote := time.Now(), time.Now()
	for off := 0; off < len(data); off += r.Chunk {
		end := min(off+r.Chunk, len(data))
		if _, err := s.call("upload_chunk", map[string]any{"upload": r.Upload, "offset": off,
			"data": base64.StdEncoding.EncodeToString(data[off:end])}, 30*time.Second); err != nil {
			s.c.Call("upload_abort", map[string]any{"upload": r.Upload}, time.Second)
			return nil, err
		}
		if time.Since(lastNote) > 5*time.Second {
			s.note("送っています… %d%%", end*100/len(data))
			lastNote = time.Now()
		}
	}
	s.note("送りました（%.1f MB、%.1f 秒）。Brain で確かめています", float64(len(data))/(1<<20), time.Since(t0).Seconds())
	return s.call("upload_end", map[string]any{"upload": r.Upload}, 2*time.Minute)
}

func updateStatus(c *Client) (string, error) {
	if err := need(c, "update_status"); err != nil {
		return "", err
	}
	raw, err := c.Call("update_status", nil, callTimeout())
	if err != nil {
		return "", err
	}
	var r struct {
		Version string       `json:"version"`
		Helper  string       `json:"helper"`
		Unit    string       `json:"unit"`
		HasPrev bool         `json:"has_prev"`
		State   *updateState `json:"state"`
	}
	json.Unmarshal(raw, &r)
	out := fmt.Sprintf("lefthand\t%s（%s）\n前の版\t%s", r.Version, r.Unit, map[bool]string{true: "あり（brain-deck update --rollback で戻せる）", false: "なし"}[r.HasPrev])
	if st := r.State; st != nil {
		res := "途中（" + st.Phase + "）"
		switch {
		case st.Done && st.OK:
			res = "成功"
		case st.Done:
			res = "失敗（" + st.Phase + "）：" + st.Message
		}
		out += fmt.Sprintf("\n最後の %s\t%s → %s、%s", st.Action, st.From, st.To, res)
	}
	return out, nil
}

// ---------- restart、reboot、poweroff ----------

func runRestart(o *options, stdout, stderr io.Writer) int {
	if len(o.args) != 1 {
		return report(stderr, usageError("restart は引数を取りません（USB も付け直すときは --usb）"))
	}
	s, err := openSession(o, stderr, restartTimeout)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	if err := need(s.c, "restart"); err != nil {
		return report(stderr, err)
	}
	raw, err := s.call("restart", map[string]any{"usb": o.usb}, 10*time.Second)
	if err != nil {
		return report(stderr, err)
	}
	var r struct {
		Stage string `json:"stage"`
	}
	json.Unmarshal(raw, &r)
	if o.usb {
		s.note("lefthand を止め、USB を付け直してから起動します。数秒、キー入力と SSH とシリアルが切れます")
	} else {
		s.note("lefthand を再起動します")
	}
	st, err := s.waitHelper(r.Stage, false, "")
	if err != nil {
		return report(stderr, err)
	}
	if !st.OK {
		fmt.Fprintf(stderr, "brain-deck: 再起動に失敗しました：%s（brain-deck logs で見られます）\n", st.Message)
		return exitFailed
	}
	if !o.quiet {
		msg := "lefthand を再起動しました（" + s.c.Hello.Version + "）"
		if st.Rebound {
			msg += "。USB を付け直しました"
		}
		fmt.Fprintln(stdout, msg)
	}
	return exitOK
}

func runPower(o *options, stdout, stderr io.Writer) int {
	action := o.args[0]
	if len(o.args) != 1 {
		return report(stderr, usageError(action+" は引数を取りません"))
	}
	timeout := rebootTimeout
	if action == "poweroff" {
		timeout = goneTimeout + 10*time.Second
	}
	s, err := openSession(o, stderr, timeout)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	if err := need(s.c, "power"); err != nil {
		return report(stderr, err)
	}
	if _, err := s.call("power", map[string]any{"action": action, "source": "brain-deck"}, 10*time.Second); err != nil {
		return report(stderr, err)
	}
	s.drop()
	if o.noWait {
		if !o.quiet {
			fmt.Fprintf(stdout, "Brain に %s を頼みました\n", action)
		}
		return exitOK
	}
	s.note("Brain が止まるのを待っています")
	gone := time.Now().Add(goneTimeout)
	for s.portPresent() {
		if time.Now().After(gone) {
			fmt.Fprintf(stderr, "brain-deck: %v たっても Brain のポートが消えません。Brain の画面を確かめてください\n", goneTimeout)
			return exitFailed
		}
		time.Sleep(500 * time.Millisecond)
	}
	if action == "poweroff" {
		if !o.quiet {
			fmt.Fprintln(stdout, "Brain の電源を切りました（USB でつないだままです。起動するときは Brain の電源ボタン）")
		}
		return exitOK
	}
	s.note("Brain が起動するのを待っています（1 分ほど）")
	if err := s.redial(); err != nil {
		return report(stderr, err)
	}
	raw, err := s.call("get_status", nil, 5*time.Second)
	if err != nil {
		return report(stderr, err)
	}
	var st struct {
		Uptime int `json:"uptime_sec"`
	}
	json.Unmarshal(raw, &st)
	if !o.quiet {
		fmt.Fprintf(stdout, "Brain を再起動しました（lefthand %s、起動から %v）\n", s.c.Hello.Version, time.Duration(st.Uptime)*time.Second)
	}
	return exitOK
}

// ---------- logs ----------

func runLogs(o *options, stdout, stderr io.Writer) int {
	if len(o.args) != 1 {
		return report(stderr, usageError("logs は引数を取りません（行数は -n、追いかけるときは -f）"))
	}
	timeout := defaultTimeout
	if o.follow {
		timeout = 100 * 365 * 24 * time.Hour // -f は、止めるまで続ける（Ctrl-C）
	}
	s, err := openSession(o, stderr, timeout)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	if err := need(s.c, "get_logs"); err != nil {
		return report(stderr, err)
	}
	n := o.lines
	if n == 0 {
		n = 100
	}
	cursor := ""
	for {
		params := map[string]any{"lines": n}
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := s.call("get_logs", params, 15*time.Second)
		if err != nil {
			var be *brainError
			if !o.follow || errors.As(err, &be) {
				return report(stderr, err)
			}
			// lefthand を再起動した、USB を付け直した：つなぎ直して続ける
			if err := s.redial(); err != nil {
				return report(stderr, err)
			}
			continue
		}
		var r struct {
			Text   string `json:"text"`
			Cursor string `json:"cursor"`
		}
		json.Unmarshal(raw, &r)
		if r.Text != "" {
			fmt.Fprintln(stdout, r.Text)
		}
		if !o.follow {
			return exitOK
		}
		cursor = r.Cursor
		time.Sleep(time.Second)
	}
}

// ---------- config ----------

func runConfig(o *options, stdin io.Reader, stdout, stderr io.Writer) int {
	a := o.args[1:]
	if len(a) == 0 {
		return report(stderr, usageError("config のあとには get、put <ファイル>、check <ファイル> を書きます"))
	}
	sub, a := a[0], a[1:]
	switch sub {
	case "get":
		if len(a) != 0 {
			return report(stderr, usageError("config get は引数を取りません（ファイルに書くときは > config.yaml）"))
		}
		return connectAndRunRaw(o, stdout, stderr, func(c *Client) (string, error) {
			if err := need(c, "get_config_text"); err != nil {
				return "", err
			}
			raw, err := c.Call("get_config_text", map[string]any{"prev": o.prev}, callTimeout())
			if err != nil {
				return "", err
			}
			var r struct {
				Text string `json:"text"`
			}
			json.Unmarshal(raw, &r)
			return r.Text, nil
		})
	case "put", "check":
	default:
		return report(stderr, usageError("config のあとには get、put <ファイル>、check <ファイル> を書きます"))
	}
	if len(a) != 1 {
		return report(stderr, usageError("config "+sub+" のあとに、設定のファイル（- なら標準入力）を 1 つ書きます"))
	}
	var text []byte
	var err error
	if a[0] == "-" {
		text, err = io.ReadAll(io.LimitReader(stdin, 256<<10))
	} else {
		text, err = os.ReadFile(a[0])
	}
	if err != nil {
		return report(stderr, usageError(err.Error()))
	}
	if len(bytes.TrimSpace(text)) == 0 {
		return report(stderr, usageError("設定のファイルが空です"))
	}
	dry := o.dryRun || sub == "check"
	s, err := openSession(o, stderr, restartTimeout)
	if err != nil {
		return report(stderr, err)
	}
	defer s.close()
	if err := need(s.c, "put_config_text"); err != nil {
		return report(stderr, err)
	}
	raw, err := s.call("put_config_text", map[string]any{"text": string(text), "restart": o.restartOK, "dry_run": dry, "source": "brain-deck"}, 30*time.Second)
	var be *brainError
	if errors.As(err, &be) && be.Code == "needs_restart" {
		fmt.Fprintln(stderr, "brain-deck: この設定は、lefthand を再起動しないと反映できない項目を変えています。何も書いていません：")
		for _, p := range be.Problems {
			fmt.Fprintf(stderr, "  %s\n", p.Path)
		}
		fmt.Fprintln(stderr, "再起動してよければ、--restart を付けてもう一度実行してください")
		return exitBrainError
	}
	if err != nil {
		return report(stderr, err)
	}
	var r struct {
		Warnings     []string `json:"warnings"`
		NeedsRestart bool     `json:"needs_restart"`
		Restarting   bool     `json:"restarting"`
		Stage        string   `json:"stage"`
		Saved        string   `json:"saved"`
	}
	json.Unmarshal(raw, &r)
	for _, w := range r.Warnings {
		fmt.Fprintln(stderr, "brain-deck: 注意："+w)
	}
	out := ""
	switch {
	case dry && r.NeedsRestart:
		out = "正しい設定です（反映には lefthand の再起動が要ります。put --restart）。何も書いていません"
	case dry:
		out = "正しい設定です。何も書いていません"
	case r.Restarting:
		s.note("%s に書きました（前の版は .prev）。lefthand を再起動します", r.Saved)
		st, err := s.waitHelper(r.Stage, false, "")
		if err != nil {
			return report(stderr, err)
		}
		if !st.OK {
			fmt.Fprintf(stderr, "brain-deck: 新しい設定で lefthand が起動しなかったので、前の設定に戻しました：%s\n", st.Message)
			return exitFailed
		}
		out = "設定を書いて、lefthand を再起動しました"
	default:
		out = fmt.Sprintf("%s に書いて、反映しました（コメントもそのまま。前の版は %s.prev）", r.Saved, r.Saved)
	}
	if !o.quiet {
		fmt.Fprintln(stdout, out)
	}
	return exitOK
}

// connectAndRunRaw は connectAndRun と同じだが、出力の前後の空白を変えない（設定のファイルを、そのまま書く）。
func connectAndRunRaw(o *options, stdout, stderr io.Writer, cmd func(*Client) (string, error)) int {
	deadline := time.Now().Add(o.timeout)
	c, unlock, err := connect(connectOptions{port: o.port, timeout: o.timeout, verbose: o.verbose, deadline: deadline})
	if err != nil {
		return report(stderr, err)
	}
	defer unlock()
	defer c.Close()
	callTimeout = func() time.Duration { return max(time.Until(deadline), 100*time.Millisecond) }
	out, err := cmd(c)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprint(stdout, out)
	return exitOK
}
