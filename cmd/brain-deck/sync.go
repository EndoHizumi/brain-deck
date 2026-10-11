package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------- sync（PC と Brain のあいだのやり取りをまとめて） ----------
//
// 1 つのコマンドで、PC の名前を知らせる（PC ごとの設定）、時刻合わせ、カレンダー、天気、読み物の RSS、手書きメモの取り出し、
// バックアップを順に行う。
// 何をするかは ~/.config/brain-deck/sync.yaml（ユーザーが作る。なければ既定）。どれかが失敗しても、ほかは続け、
// 最後に結果をまとめて書く。終了コードは、すべて成功（飛ばしたものを含む）なら 0、失敗したものがあれば 9、
// Brain が見つからなければ 3。Brain には、始めと終わりに set_sync を送り、画面に「同期中」と最後に同期した時刻を出す。
//
// Brain のデータ（Todo、手書きメモ、しおり）が正。同期は Brain のデータを書き換えない（手書きメモは PC に写すだけ）。

const exitSyncPartial = 9 // 同期で、失敗したものがあった（ほかは行った）

type syncConfig struct {
	Time     *bool `yaml:"time"`     // 時刻を合わせる（既定 true）。内蔵 RTC にも書き戻す（clock.rtc: write のとき）
	Calendar *bool `yaml:"calendar"` // calendar sync（既定は calendars.yaml があれば）
	Weather  *bool `yaml:"weather"`  // weather sync（既定は weather.yaml があれば）
	Feeds    *bool `yaml:"feeds"`    // read feed sync（既定は feeds.yaml があれば）
	Memo     struct {
		Dir string `yaml:"dir"` // 手書きメモを PNG と JSON で置く PC のフォルダ。書かなければ取り出さない
	} `yaml:"memo"`
	Wait    string           `yaml:"wait"`    // Brain の準備を待つ時間（--wait と同じ。つないだときの自動の同期）
	PCName  string           `yaml:"pc_name"` // Brain に知らせる、この PC の名前（PC ごとの設定。既定はホスト名）
	Profile *bool            `yaml:"profile"` // PC の名前を知らせる（既定 true）
	Backup  syncBackupConfig `yaml:"backup"`  // バックアップ（既定は 1 日 1 回、7 個残す）
}

const syncExample = `# ~/.config/brain-deck/sync.yaml の例（brain-deck sync で行うこと。このファイルはなくてもよい）
time: true          # 時刻を合わせる（内蔵 RTC にも書き戻す）
calendar: true      # 予定（~/.config/brain-deck/calendars.yaml）
weather: true       # 天気（~/.config/brain-deck/weather.yaml）
feeds: true         # 読み物の RSS（~/.config/brain-deck/feeds.yaml）
memo:
  dir: ~/Documents/Brain-memo   # 手書きメモを PNG と JSON で写すフォルダ（書かなければ取り出さない）
wait: 90s           # Brain の準備を待つ（つないだときの自動の同期で使う）
pc_name: my-laptop  # Brain に知らせる PC の名前（Brain の設定の profiles で選ぶ。書かなければホスト名）
backup:
  keep: 7           # 残すバックアップの数（0 で取らない）
  every: 20h        # 前に取ってからこれだけたっていれば取る（0 なら毎回）
  dir: ~/.local/share/brain-deck/backups   # 置き場所（個人のデータが入る。自分だけが読める場所に）`

func syncPath() string { return filepath.Join(filepath.Dir(calendarsPath()), "sync.yaml") }

func loadSyncConfig() (syncConfig, bool, error) {
	var c syncConfig
	b, err := os.ReadFile(syncPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, true, fmt.Errorf("%s: %w", syncPath(), err)
	}
	return c, true, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// syncStep は、同期の 1 つの段階。
type syncStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
	skip bool
}

func runSync(o *options, stdout, stderr io.Writer) int {
	if err := o.checkSub("wait if-older"); err != nil {
		return report(stderr, err)
	}
	if len(o.args) > 1 {
		return report(stderr, usageError("sync は引数を取りません（何をするかは "+syncPath()+"）"))
	}
	cfg, found, err := loadSyncConfig()
	if err != nil {
		return report(stderr, err)
	}
	wait := cfg.Wait
	if v := o.vals["wait"]; v != "" {
		wait = v
	}
	if wait != "" {
		d, err := time.ParseDuration(wait)
		if err != nil || d < 0 {
			return report(stderr, usageError("--wait は 90s のような時間です"))
		}
		if err := waitForBrain(o, d, stderr); err != nil {
			return report(stderr, err)
		}
	}
	// --if-older：Brain が最後に同期してからこの時間がたっていなければ、何もしない（USB の付け直しのたびに同期しない）
	var older time.Duration
	if v := o.vals["if-older"]; v != "" {
		if older, err = time.ParseDuration(v); err != nil || older <= 0 {
			return report(stderr, usageError("--if-older は 10m のような時間です"))
		}
	}
	recent := false
	on := func(p *bool, def bool) bool {
		if p == nil {
			return def
		}
		return *p
	}
	// 始めたことを Brain に知らせる（古いデーモンなら飛ばす）
	if code := connectAndRun(o, io.Discard, stderr, func(c *Client) (string, error) {
		if older > 0 {
			raw, err := c.Call("get_status", nil, callTimeout())
			if err != nil {
				return "", err
			}
			var st struct {
				Sync struct {
					Running bool `json:"running"`
					Last    *struct {
						At time.Time `json:"at"`
					} `json:"last"`
				} `json:"sync"`
			}
			json.Unmarshal(raw, &st)
			if st.Sync.Running || st.Sync.Last != nil && time.Since(st.Sync.Last.At) < older {
				recent = true
				// 同期は飛ばしても、PC の名前は知らせる（USB を付け直したあと名乗らないと、Brain は既定の設定に戻すので）
				if on(cfg.Profile, true) && c.Hello.has("set_pc") {
					if name, err := pcName(cfg); err == nil {
						announcePC(c, name)
					}
				}
				return "", nil
			}
		}
		if need(c, "set_sync") == nil {
			_, err := c.Call("set_sync", map[string]any{"state": "start", "source": "brain-deck"}, callTimeout())
			return "", err
		}
		return "", nil
	}); code != exitOK {
		return code
	}
	if recent {
		if !o.quiet {
			fmt.Fprintf(stdout, "Brain は %v のうちに同期しているので、何もしません（--if-older）\n", older)
		}
		return exitOK
	}
	var steps []syncStep
	sub := func(name string, args ...string) {
		argv := []string{}
		if o.port != "" {
			argv = append(argv, "--port", o.port)
		}
		if o.verbose {
			argv = append(argv, "-v")
		}
		argv = append(argv, "--fetch-timeout", o.fetchTimeout.String())
		argv = append(argv, args...)
		var out, errb bytes.Buffer
		code := run(argv, strings.NewReader(""), &out, &errb)
		note := strings.TrimSpace(out.String())
		if e := strings.TrimSpace(errb.String()); e != "" {
			note = strings.TrimSpace(note + "\n" + strings.ReplaceAll(e, "brain-deck: ", ""))
		}
		steps = append(steps, syncStep{Name: name, OK: code == exitOK, Note: note})
	}
	skip := func(name, why string) { steps = append(steps, syncStep{Name: name, OK: true, Note: why, skip: true}) }
	if on(cfg.Profile, true) {
		name, err := pcName(cfg)
		var note string
		code := exitOK
		if err == nil {
			code = connectAndRun(o, io.Discard, stderr, func(c *Client) (string, error) {
				if !c.Hello.has("set_pc") {
					note = "Brain の lefthand が PC ごとの設定に対応していない"
					return "", nil
				}
				p, err := announcePC(c, name)
				if err == nil {
					note = name + "：" + p.describe()
				}
				return "", err
			})
		}
		steps = append(steps, syncStep{Name: "PC の名前", OK: err == nil && code == exitOK, Note: note})
		if err != nil {
			steps[len(steps)-1].Note = err.Error()
		}
	} else {
		skip("PC の名前", "sync.yaml で profile: false")
	}
	if on(cfg.Time, true) {
		sub("時刻", "time", "sync")
	} else {
		skip("時刻", "sync.yaml で time: false")
	}
	switch {
	case !on(cfg.Calendar, true):
		skip("予定", "sync.yaml で calendar: false")
	case !fileExists(calendarsPath()):
		skip("予定", calendarsPath()+" がない")
	default:
		sub("予定", "calendar", "sync")
	}
	switch {
	case !on(cfg.Weather, true):
		skip("天気", "sync.yaml で weather: false")
	case !fileExists(weatherPath()):
		skip("天気", weatherPath()+" がない")
	default:
		sub("天気", "weather", "sync")
	}
	fp, _ := feedsPath()
	switch {
	case !on(cfg.Feeds, true):
		skip("読み物の RSS", "sync.yaml で feeds: false")
	case !fileExists(fp):
		skip("読み物の RSS", fp+" がない")
	default:
		sub("読み物の RSS", "read", "feed", "sync")
	}
	if cfg.Memo.Dir == "" {
		skip("手書きメモ", "sync.yaml に memo.dir がない")
	} else {
		dir := expandHome(cfg.Memo.Dir)
		var msg string
		code := connectAndRun(o, io.Discard, stderr, func(c *Client) (string, error) {
			s, err := exportMemos(c, dir)
			msg = s
			return "", err
		})
		steps = append(steps, syncStep{Name: "手書きメモ", OK: code == exitOK, Note: msg})
	}
	{
		var note string
		var skipped bool
		code := connectAndRun(o, io.Discard, stderr, func(c *Client) (string, error) {
			var err error
			note, skipped, err = syncBackup(c, cfg.Backup, time.Now())
			if err != nil {
				note = err.Error()
			}
			return "", err
		})
		if skipped {
			skip("バックアップ", note)
		} else {
			steps = append(steps, syncStep{Name: "バックアップ", OK: code == exitOK, Note: note})
		}
	}
	// 結果をまとめる
	failed := 0
	done := 0
	var b strings.Builder
	for _, s := range steps {
		mark := "成功"
		switch {
		case s.skip:
			mark = "飛ばした"
		case !s.OK:
			mark = "失敗"
			failed++
		default:
			done++
		}
		fmt.Fprintf(&b, "%-8s %s", s.Name, mark)
		if s.Note != "" {
			fmt.Fprintf(&b, "：%s", strings.ReplaceAll(s.Note, "\n", "\n           "))
		}
		b.WriteByte('\n')
	}
	summary := fmt.Sprintf("%d 件を行った", done)
	if failed > 0 {
		summary += fmt.Sprintf("、%d 件が失敗", failed)
	}
	if !found {
		fmt.Fprintf(&b, "（%s がないので、既定のとおり行いました。書き方は README の「同期」）\n", syncPath())
	}
	// 終わったことを Brain に知らせる
	ok := failed == 0
	brainSteps := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		if !s.skip {
			first, _, _ := strings.Cut(s.Note, "\n")
			brainSteps = append(brainSteps, map[string]any{"name": s.Name, "ok": s.OK, "note": firstN(first, 120)})
		}
	}
	connectAndRun(o, io.Discard, stderr, func(c *Client) (string, error) {
		if need(c, "set_sync") == nil {
			_, err := c.Call("set_sync", map[string]any{"state": "done", "ok": ok, "summary": summary, "steps": brainSteps, "source": "brain-deck"}, callTimeout())
			return "", err
		}
		return "", nil
	})
	if !o.quiet {
		fmt.Fprint(stdout, b.String())
		fmt.Fprintln(stdout, "同期："+summary)
	}
	if !ok {
		return exitSyncPartial
	}
	return exitOK
}

// waitForBrain は、Brain が答えるまで（USB をつないだ直後は、ポートができて lefthand が答えるまでに時間がかかる）待つ。
func waitForBrain(o *options, d time.Duration, stderr io.Writer) error {
	deadline := time.Now().Add(d)
	var last error
	for {
		c, unlock, err := connect(connectOptions{port: o.port, timeout: 5 * time.Second, verbose: o.verbose, deadline: time.Now().Add(5 * time.Second)})
		if err == nil {
			unlock()
			c.Close()
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("Brain が %v のうちに答えませんでした: %w", d, last)
		}
		if o.verbose {
			fmt.Fprintf(stderr, "brain-deck: Brain を待っています（%v）\n", err)
		}
		time.Sleep(3 * time.Second)
	}
}

// exportMemos は、Brain の手書きメモを、dir に <id>.png（ページを縦に並べる）と <id>.json（線の点の列）で写す。
// 前に写したもの（<id>.json の updated が同じ）は書き直さない。Brain のメモは消さない（Brain のデータが正）。
func exportMemos(c *Client, dir string) (string, error) {
	l, err := fetchMemos(c)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	n, same := 0, 0
	for _, m := range l {
		jp := filepath.Join(dir, m.ID+".json")
		if b, err := os.ReadFile(jp); err == nil {
			var old memoDoc
			if json.Unmarshal(b, &old) == nil && old.Updated.Equal(m.Updated) && fileExists(filepath.Join(dir, m.ID+".png")) {
				same++
				continue
			}
		}
		doc, err := readMemo(c, m.ID)
		if err != nil {
			return "", err
		}
		if err := writeMemoPNG(filepath.Join(dir, m.ID+".png"), doc.Pages); err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(doc, "", " ")
		if err := os.WriteFile(jp, b, 0o644); err != nil {
			return "", err
		}
		n++
	}
	return fmt.Sprintf("%d 個を %s に写した（%d 個は前と同じ）", n, dir, same), nil
}
