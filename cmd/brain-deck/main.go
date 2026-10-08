// brain-deck は、PC から Brain（左手デバイス lefthand）のウィジェットを書き換えるコマンド。
// 設定 GUI と同じ USB シリアル（Brain 側 /dev/ttyGS1）と、同じプロトコル（docs/protocol.md）を使う。
// Linux と macOS 用。使い方は README の「brain-deck」。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 終了コード（README に同じ表がある）
const (
	exitOK         = 0
	exitInternal   = 1 // 予期しないエラー
	exitUsage      = 2 // 使い方の誤り
	exitNoBrain    = 3 // Brain が見つからない、返事がない
	exitBusy       = 4 // 設定 GUI（ほかのプログラム）が接続中、ほかの brain-deck が終わらない
	exitBrainError = 5 // Brain がエラーを返した（引数の誤り、古いデーモンなど）
	exitPermission = 6 // ポートを開く権限がない（Linux の dialout）
	// exitFetch = 7（calendar.go）：予定の取得に失敗した（取れたカレンダーは送った）
)

const defaultTimeout = 5 * time.Second

// timeSyncThreshold より Brain の時刻がずれていたら、text の前に時刻を合わせる。
const timeSyncThreshold = 2 * time.Second

var version = "" // -ldflags "-X main.version=..."

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if rev != "" {
			if dirty {
				rev += "+dirty"
			}
			return rev
		}
	}
	return "dev"
}

const usage = `使い方：
  brain-deck text <id> <テキスト> [--style normal|ok|error|warn] [--ttl 10m]
  brain-deck text <id> -              テキストを標準入力から読む
  brain-deck text <id> --clear        テキストを消す（「未設定」に戻す）
  brain-deck text --list              Brain にあるテキストの一覧
  brain-deck todo [list] [--json]     Todo の一覧（番号は Brain の画面と同じ順）
  brain-deck todo add <項目> [--top]  Todo を足す（- なら標準入力の 1 行ごとに足す。--top で先頭に）
  brain-deck todo done <番号|ID>...   完了にする（undo で未完了に戻す）
  brain-deck todo edit <番号|ID> <文> 項目の文を書き換える
  brain-deck todo rm <番号|ID>...     項目を消す
  brain-deck todo clear-done          完了した項目をまとめて消す
  brain-deck calendar sync            予定を ICS の URL から取ってきて Brain に送る（~/.config/brain-deck/calendars.yaml）
  brain-deck calendar sync --ics <URL> [--days 7]   設定ファイルを使わず、URL を直接指定する（何度でも書ける）
  brain-deck calendar sync --dry-run  取ってきた予定を表示するだけで、Brain には送らない
  brain-deck calendar [list] [--json] Brain にある予定
  brain-deck calendar clear           Brain の予定を消す
  brain-deck images [list] [--json]   Brain にある背景画像の一覧（使っている場所の数、名前）
  brain-deck images put <ファイル.565|ディレクトリ>...  変換済みの画像を送る（PNG などは設定 GUI で変換する）
  brain-deck images prune [--dry-run] 今の設定で使っていない背景画像を消す
  brain-deck time sync                PC の時刻を Brain に送る
  brain-deck status                   Brain の状態（版、時刻、レイヤー、USB の形）
  brain-deck usb-mode [keyboard|mouse|toggle]  USB の形を見る・切り替える。keyboard はキーボードだけ（ブートキーボード。
                                      起動したときの形）、mouse はキーボードとマウス。USB を付け直すので、2〜3 秒切れる
  brain-deck terminal [on|off|toggle] 端末モード（Brain の画面とキーボードで PC にログインする）を見る・切り替える。
                                      シリアルの端末モードでは、入るとき・抜けるときに USB を付け直す
  brain-deck version

共通のオプション：
  --port <パス>      ポートを指定する（例：/dev/serial/by-id/usb-SHARP_Brain_0123456789-if05、/dev/cu.usbmodem01234567895）。
                     環境変数 BRAIN_DECK_PORT でも指定できる。省略すると探す
  --timeout <時間>   全体の時間の上限（既定 5s）
  --no-time-sync     text と calendar sync の前に、Brain の時刻を合わせない
  --config <パス>    calendar sync の設定ファイル（既定 ~/.config/brain-deck/calendars.yaml）
  --fetch-timeout <時間>  calendar sync で予定を取ってくる時間の上限（既定 30s。--timeout とは別）
  -q, --quiet        成功したときに何も出さない
  -v, --verbose      通信の内容を標準エラーに出す

終了コード：0 成功、1 予期しないエラー、2 使い方の誤り、3 Brain が見つからない・返事がない、
           4 設定 GUI が接続中（ほかのプログラムがポートを使用中）、5 Brain がエラーを返した、6 ポートを開く権限がない、
           7 予定の取得に失敗した（calendar sync。取れたカレンダーは送る）
`

type options struct {
	port       string
	timeout    time.Duration
	style      string
	ttl        time.Duration
	clear      bool
	list       bool
	noTimeSync bool
	top        bool // todo add：先頭に足す
	json       bool // todo list、calendar list：JSON で出す
	// calendar sync
	ics          []string // --ics（何度でも）
	days         int
	config       string
	dryRun       bool
	fetchTimeout time.Duration
	quiet        bool
	verbose      bool
	args         []string
}

type usageError string

func (e usageError) Error() string { return string(e) }

// parseArgs は、オプションをどこに書いても受け付ける（`text build "x" --style ok` も `--style ok text build "x"` も）。
// `--` のあとは、すべてふつうの引数にする（`-` で始まるテキストを渡すとき）。
func parseArgs(argv []string) (*options, error) {
	o := &options{timeout: defaultTimeout, port: os.Getenv("BRAIN_DECK_PORT"), fetchTimeout: defaultFetchTimeout}
	withValue := map[string]bool{"port": true, "timeout": true, "style": true, "ttl": true, "ics": true, "days": true, "config": true, "fetch-timeout": true}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			o.args = append(o.args, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			o.args = append(o.args, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if withValue[name] && !hasVal {
			if i+1 >= len(argv) {
				return nil, usageError("--" + name + " に値がありません")
			}
			i++
			val = argv[i]
		} else if !withValue[name] && hasVal {
			return nil, usageError("--" + name + " は値を取りません")
		}
		var err error
		switch name {
		case "port":
			o.port = val
		case "timeout":
			o.timeout, err = time.ParseDuration(val)
			if err == nil && o.timeout <= 0 {
				err = errors.New("must be positive")
			}
		case "style":
			o.style = val
		case "ttl":
			o.ttl, err = parseTTL(val)
		case "clear":
			o.clear = true
		case "list":
			o.list = true
		case "no-time-sync":
			o.noTimeSync = true
		case "ics":
			o.ics = append(o.ics, val)
		case "days":
			o.days, err = strconv.Atoi(val)
			if err == nil && (o.days < 1 || o.days > maxCalDays) {
				err = fmt.Errorf("must be 1..%d", maxCalDays)
			}
		case "config":
			o.config = val
		case "fetch-timeout":
			o.fetchTimeout, err = time.ParseDuration(val)
			if err == nil && o.fetchTimeout <= 0 {
				err = errors.New("must be positive")
			}
		case "dry-run":
			o.dryRun = true
		case "top":
			o.top = true
		case "json":
			o.json = true
		case "q", "quiet":
			o.quiet = true
		case "v", "verbose":
			o.verbose = true
		case "h", "help":
			return nil, usageError("")
		default:
			return nil, usageError("知らないオプション " + a)
		}
		if err != nil {
			return nil, usageError(fmt.Sprintf("--%s %q: %v", name, val, err))
		}
	}
	return o, nil
}

// parseTTL は Go の時間の書き方（90s、10m、1h30m）に加えて、日数（2d）を受け付ける。
func parseTTL(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil || n <= 0 {
			return 0, errors.New("use a duration such as 90s, 10m, 2h or 1d")
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	t, err := time.ParseDuration(s)
	if err != nil || t <= 0 {
		return 0, errors.New("use a duration such as 90s, 10m, 2h or 1d")
	}
	return t, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseArgs(argv)
	if err != nil {
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(stderr, "brain-deck: "+msg+"（使い方は brain-deck --help）")
			return exitUsage
		}
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	if len(o.args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	hint := "（使い方は brain-deck --help）"
	if (o.top || o.json) && o.args[0] != "todo" && !(o.json && (o.args[0] == "calendar" || o.args[0] == "images")) {
		fmt.Fprintln(stderr, "brain-deck: --top は todo で、--json は todo、calendar、images で使います"+hint)
		return exitUsage
	}
	if o.args[0] != "calendar" && (len(o.ics) > 0 || o.days != 0 || o.config != "" || (o.dryRun && o.args[0] != "images")) {
		fmt.Fprintln(stderr, "brain-deck: --ics、--days、--config、--dry-run は calendar sync で使います"+hint)
		return exitUsage
	}
	var cmd func(*Client) (string, error)
	switch o.args[0] {
	case "version":
		fmt.Fprintln(stdout, "brain-deck", versionString())
		return exitOK
	case "text":
		cmd, err = textCommand(o, stdin)
	case "todo":
		cmd, err = todoCommand(o, stdin)
	case "calendar":
		return runCalendar(o, stdout, stderr)
	case "images":
		cmd, err = imagesCommand(o)
	case "time":
		if len(o.args) != 2 || o.args[1] != "sync" {
			err = usageError("time のあとには sync を書きます（brain-deck time sync）")
		}
		cmd = timeSync
	case "status":
		if len(o.args) != 1 {
			err = usageError("status は引数を取りません")
		}
		cmd = status
	case "usb-mode":
		cmd, err = usbModeCommand(o)
	case "terminal":
		cmd, err = terminalCommand(o)
	default:
		err = usageError("知らないコマンド " + strconv.Quote(o.args[0]))
	}
	if err != nil {
		fmt.Fprintln(stderr, "brain-deck: "+err.Error()+hint)
		return exitUsage
	}

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
	if out != "" && !o.quiet {
		fmt.Fprintln(stdout, out)
	}
	return exitOK
}

// callTimeout は、残りの時間（全体の --timeout まで）。
var callTimeout = func() time.Duration { return defaultTimeout }

// report は、エラーを分かりやすく書き、終了コードを返す。
func report(w io.Writer, err error) int {
	var be *busyError
	var br *brainError
	var ue usageError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintln(w, "brain-deck: "+ue.Error())
		return exitUsage
	case errors.As(err, &be):
		fmt.Fprintln(w, "brain-deck: "+describeBusy(be))
		return exitBusy
	case errors.Is(err, errPermission):
		fmt.Fprintf(w, "brain-deck: %v\n%s\n", err, permissionHint)
		return exitPermission
	case errors.Is(err, errNotFound):
		fmt.Fprintf(w, "brain-deck: Brain が見つかりません（%v）\n%s。--port で指定することもできます\n", err, portHint)
		return exitNoBrain
	case errors.Is(err, errNoReply):
		fmt.Fprintf(w, "brain-deck: Brain から返事がありません（%v）\nlefthand.service が動いているかを確かめてください（ssh brain systemctl status lefthand）\n", err)
		return exitNoBrain
	case errors.As(err, &br):
		fmt.Fprintf(w, "brain-deck: Brain がエラーを返しました：%s（%s）\n", br.Message, br.Code)
		if br.Code == "conflict" || br.Code == "not_found" {
			fmt.Fprintln(w, "読んだあとに Brain で項目が変わりました。brain-deck todo list で確かめてから、もう一度実行してください")
		}
		return exitBrainError
	}
	fmt.Fprintln(w, "brain-deck: "+err.Error())
	return exitInternal
}

// need は、デーモンが cmd に対応しているかを確かめる。
func need(c *Client, cmd string) error {
	if !c.Hello.has(cmd) {
		return &brainError{Code: "unknown_command", Message: fmt.Sprintf(
			"Brain の lefthand（%s）は %s に対応していません。lefthand を新しくしてください", c.Hello.Version, cmd)}
	}
	return nil
}

// ---------- text ----------

func textCommand(o *options, stdin io.Reader) (func(*Client) (string, error), error) {
	a := o.args[1:]
	if o.list {
		if len(a) != 0 || o.clear {
			return nil, usageError("text --list は、ほかの引数を取りません")
		}
		return listTexts, nil
	}
	if len(a) == 0 {
		return nil, usageError("text のあとに id を書きます（brain-deck text build \"ビルド成功\"）")
	}
	params := map[string]any{"name": a[0], "source": "brain-deck"}
	switch {
	case o.clear:
		if len(a) != 1 || o.style != "" || o.ttl != 0 {
			return nil, usageError("text <id> --clear は、テキストや --style、--ttl を取りません")
		}
		params["clear"] = true
	case len(a) == 2:
		text := a[1]
		if text == "-" {
			b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
			if err != nil {
				return nil, err
			}
			text = strings.TrimRight(string(b), "\r\n")
		}
		params["text"] = text
		if o.style != "" {
			params["style"] = o.style
		}
		if o.ttl > 0 {
			params["ttl_sec"] = o.ttl.Seconds()
		}
	case len(a) == 1:
		return nil, usageError("テキストがありません（消すときは --clear）")
	default:
		return nil, usageError("テキストは 1 つの引数にします（空白を含むときは \"\" で囲む）")
	}
	return func(c *Client) (string, error) {
		if err := need(c, "set_text"); err != nil {
			return "", err
		}
		if !o.noTimeSync {
			if err := syncIfNeeded(c); err != nil {
				return "", err
			}
		}
		raw, err := c.Call("set_text", params, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Cleared bool            `json:"cleared"`
			Shown   bool            `json:"shown"`
			Entry   json.RawMessage `json:"entry"`
		}
		json.Unmarshal(raw, &r)
		id := a[0]
		if !r.Shown {
			fmt.Fprintf(os.Stderr, "brain-deck: 注意：今の設定には、id が %s のテキストのセル（widget: text）がありません。中身は Brain に保存しました\n", id)
		}
		if r.Cleared {
			return id + ": 消しました", nil
		}
		msg := fmt.Sprintf("%s: %s", id, params["text"])
		var extra []string
		if s, _ := params["style"].(string); s != "" {
			extra = append(extra, s)
		}
		if o.ttl > 0 {
			extra = append(extra, o.ttl.String()+"で期限切れ")
		}
		if len(extra) > 0 {
			msg += "（" + strings.Join(extra, "、") + "）"
		}
		return msg, nil
	}, nil
}

type textInfo struct {
	Text      string     `json:"text"`
	Style     string     `json:"style"`
	SetAt     time.Time  `json:"set_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	Source    string     `json:"source"`
	Expired   bool       `json:"expired"`
}

func listTexts(c *Client) (string, error) {
	if err := need(c, "get_text"); err != nil {
		return "", err
	}
	raw, err := c.Call("get_text", nil, callTimeout())
	if err != nil {
		return "", err
	}
	var r struct {
		Texts map[string]textInfo `json:"texts"`
		IDs   []string            `json:"ids"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	inCfg := map[string]bool{}
	for _, id := range r.IDs {
		inCfg[id] = true
		if _, ok := r.Texts[id]; !ok {
			r.Texts[id] = textInfo{Style: "-"}
		}
	}
	ids := make([]string, 0, len(r.Texts))
	for id := range r.Texts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("id\t色\t状態\t書いた時刻\tテキスト\n")
	for _, id := range ids {
		t := r.Texts[id]
		state := "表示中"
		switch {
		case t.SetAt.IsZero():
			state = "未設定"
		case t.Expired:
			state = "期限切れ（薄く表示）"
		case t.ExpiresAt != nil:
			state = "期限 " + t.ExpiresAt.Local().Format("01/02 15:04")
		}
		if !inCfg[id] {
			state += "、セルなし"
		}
		at := ""
		if !t.SetAt.IsZero() {
			at = t.SetAt.Local().Format("01/02 15:04")
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n", id, t.Style, state, at, strings.ReplaceAll(t.Text, "\n", "⏎"))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------- time ----------

type timeInfo struct {
	Now    time.Time `json:"now"`
	Synced bool      `json:"synced"`
	TZ     string    `json:"timezone"`
}

// syncIfNeeded は、Brain の時刻が合っていなければ合わせる（テキストの有効期限を正しくするため）。
func syncIfNeeded(c *Client) error {
	if !c.Hello.has("set_time") {
		return nil
	}
	t0 := time.Now()
	raw, err := c.Call("get_status", nil, callTimeout())
	if err != nil {
		return err
	}
	rtt := time.Since(t0)
	var st struct {
		Time timeInfo `json:"time"`
	}
	json.Unmarshal(raw, &st)
	off := t0.Add(rtt / 2).Sub(st.Time.Now)
	if st.Time.Synced && off.Abs() < timeSyncThreshold {
		return nil
	}
	_, err = setTime(c)
	return err
}

func setTime(c *Client) (string, error) {
	if err := need(c, "set_time"); err != nil {
		return "", err
	}
	raw, err := c.Call("set_time", map[string]any{"unix_ms": time.Now().UnixMilli(), "source": "brain-deck"}, callTimeout())
	if err != nil {
		return "", err
	}
	var r struct {
		Stepped  bool  `json:"stepped"`
		OffsetMS int64 `json:"offset_ms"`
		timeInfo
	}
	json.Unmarshal(raw, &r)
	off := (time.Duration(r.OffsetMS) * time.Millisecond).Round(time.Millisecond)
	if !r.Stepped {
		return fmt.Sprintf("Brain の時刻は合っています（ずれ %v）", off), nil
	}
	dir := "遅れていた"
	if off < 0 {
		dir, off = "進んでいた", -off
	}
	return fmt.Sprintf("Brain の時刻を合わせました（%v %s。今 %s）", off, dir, r.Now.Local().Format("2006-01-02 15:04:05")), nil
}

func timeSync(c *Client) (string, error) { return setTime(c) }

// ---------- status ----------

func status(c *Client) (string, error) {
	raw, err := c.Call("get_status", nil, callTimeout())
	if err != nil {
		return "", err
	}
	var st struct {
		Status struct {
			Layer string `json:"layer"`
			Label string `json:"label"`
		} `json:"status"`
		Uptime int       `json:"uptime_sec"`
		Time   timeInfo  `json:"time"`
		HID    *usbHID   `json:"hid"`
		Term   *termInfo `json:"terminal"`
	}
	json.Unmarshal(raw, &st)
	sync := "合わせてある"
	if !st.Time.Synced {
		sync = "未設定（brain-deck time sync で合わせる）"
	}
	off := time.Until(st.Time.Now).Round(time.Second)
	out := fmt.Sprintf("ポート\t%s\nlefthand\t%s（起動から %v）\nレイヤー\t%s\nBrain の時刻\t%s（%s、PC との差 %v）",
		c.Port, c.Hello.Version, time.Duration(st.Uptime)*time.Second, st.Status.Label,
		st.Time.Now.Format("2006-01-02 15:04:05 MST"), sync, off)
	if st.HID != nil {
		out += "\nUSB\t" + st.HID.describe()
	}
	if st.Term != nil {
		out += "\n端末モード\t" + st.Term.describe()
	}
	return out, nil
}

// termInfo は get_status の terminal（lefthand の termmode.go の TermInfo）。
type termInfo struct {
	Active    bool   `json:"active"`
	State     string `json:"state"`
	Transport string `json:"transport"`
	Status    string `json:"status"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

func (t termInfo) describe() string {
	switch t.State {
	case "off":
		return "オフ"
	case "entering":
		return "入っている途中"
	case "leaving":
		return "抜けている途中"
	}
	s := "オン"
	if t.Transport == "command" {
		s += "（コマンド）"
	} else {
		s += "（シリアル）"
	}
	if t.Cols > 0 {
		s += fmt.Sprintf(" %d×%d", t.Cols, t.Rows)
	}
	if t.Status != "" {
		s += "：" + t.Status
	}
	return s
}

// terminalCommand は terminal。引数がなければ今の状態を出し、あれば切り替える。
func terminalCommand(o *options) (func(*Client) (string, error), error) {
	if len(o.args) > 2 {
		return nil, usageError("terminal の引数は on、off、toggle のどれか 1 つです")
	}
	old := errors.New("Brain の lefthand が古く、端末モードがありません")
	if len(o.args) == 1 {
		return func(c *Client) (string, error) {
			raw, err := c.Call("get_status", nil, callTimeout())
			if err != nil {
				return "", err
			}
			var st struct {
				Term *termInfo `json:"terminal"`
			}
			json.Unmarshal(raw, &st)
			if st.Term == nil {
				return "", old
			}
			return "端末モードは " + st.Term.describe(), nil
		}, nil
	}
	mode := o.args[1]
	switch mode {
	case "on", "off", "toggle":
	default:
		return nil, usageError("terminal の引数は on、off、toggle のどれかです（" + strconv.Quote(mode) + "）")
	}
	return func(c *Client) (string, error) {
		if !c.Hello.has("set_terminal") {
			return "", old
		}
		raw, err := c.Call("set_terminal", map[string]any{"mode": mode}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Terminal bool     `json:"terminal"`
			Info     termInfo `json:"info"`
		}
		json.Unmarshal(raw, &r)
		if r.Terminal {
			if r.Info.State == "on" {
				return "端末モードに入っています", nil
			}
			return "端末モードに入ります。シリアルの端末モードでは USB を付け直すので、数秒、キー入力、SSH、シリアルが切れます", nil
		}
		if r.Info.State == "off" {
			return "端末モードではありません", nil
		}
		return "端末モードを抜けます。シリアルの端末モードでは USB を付け直すので、数秒、SSH とシリアルが切れます", nil
	}, nil
}

// usbHID は get_status の hid。
type usbHID struct {
	Mouse     bool `json:"mouse"`
	Switching bool `json:"switching"`
}

func (h usbHID) describe() string {
	s := "キーボードだけ（ブートキーボード）"
	if h.Mouse {
		s = "キーボードとマウス"
	}
	if h.Switching {
		s += "（切り替え中）"
	}
	return s
}

// usbModeCommand は usb-mode。引数がなければ今の形を出し、あれば切り替える。
func usbModeCommand(o *options) (func(*Client) (string, error), error) {
	if len(o.args) > 2 {
		return nil, usageError("usb-mode の引数は keyboard、mouse、toggle のどれか 1 つです")
	}
	if len(o.args) == 1 {
		return func(c *Client) (string, error) {
			raw, err := c.Call("get_status", nil, callTimeout())
			if err != nil {
				return "", err
			}
			var st struct {
				HID *usbHID `json:"hid"`
			}
			json.Unmarshal(raw, &st)
			if st.HID == nil {
				return "", errors.New("Brain の lefthand が古く、USB の形を切り替えられません")
			}
			return "USB は " + st.HID.describe(), nil
		}, nil
	}
	mode := o.args[1]
	switch mode {
	case "keyboard", "mouse", "toggle":
	default:
		return nil, usageError("usb-mode の引数は keyboard、mouse、toggle のどれかです（" + strconv.Quote(mode) + "）")
	}
	return func(c *Client) (string, error) {
		if !c.Hello.has("set_usb_mode") {
			return "", errors.New("Brain の lefthand が古く、USB の形を切り替えられません")
		}
		raw, err := c.Call("set_usb_mode", map[string]any{"mode": mode}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Mouse     bool `json:"mouse"`
			Switching bool `json:"switching"`
		}
		json.Unmarshal(raw, &r)
		h := usbHID{Mouse: r.Mouse}
		if !r.Switching {
			return "USB はすでに " + h.describe() + " の形です", nil
		}
		return "USB を " + h.describe() + " の形に切り替えます。2〜3 秒、キー入力、SSH、シリアルが切れます", nil
	}, nil
}
