package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------- calendar ----------
//
// PC で ICS（iCalendar）の URL から予定を取ってきて、繰り返しとタイムゾーンを解決し、Brain に set_calendar で送る。
// ICS の URL は秘密の情報（知っていればだれでも予定を読める）なので、リポジトリには入れず、
// PC の ~/.config/brain-deck/calendars.yaml に書く。エラーやログにも URL を出さない（ホスト名だけ）。
// 取ってくるのは、ポートを開く前に行う（取得に時間がかかっても、そのあいだ設定 GUI を締め出さない）。

const (
	defaultCalDays      = 7
	maxCalDays          = 31
	defaultFetchTimeout = 30 * time.Second
	maxICSBytes         = 20 << 20
	maxCalEvents        = 1000 // Brain の上限（calendar.go の calMaxEvents）
	maxCalNameRunes     = 32
)

// calendarsFile は calendars.yaml の形。
type calendarsFile struct {
	Days      int           `yaml:"days"`
	Calendars []calendarCfg `yaml:"calendars"`
}

type calendarCfg struct {
	Name  string `yaml:"name"`
	URL   string `yaml:"url"`
	Color string `yaml:"color"`
}

// colorNames は、calendars.yaml の color に書ける名前（#rrggbb も書ける）。
var colorNames = map[string]string{
	"blue": "#4f9dff", "green": "#50d880", "orange": "#ff8a50", "purple": "#c080ff", "yellow": "#ffd040",
	"cyan": "#40d0d0", "pink": "#ff6090", "gray": "#a0a8b0", "grey": "#a0a8b0", "red": "#ff5858", "white": "#ffffff",
}

var calPalette = []string{"#4f9dff", "#50d880", "#ff8a50", "#c080ff", "#ffd040", "#40d0d0", "#ff6090", "#a0a8b0"}

var hexColorRe = regexp.MustCompile(`^#([0-9a-fA-F]{6}|[0-9a-fA-F]{3})$`)

// normColor は、色を #rrggbb にする。
func normColor(s string, i int) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch {
	case s == "":
		return calPalette[i%len(calPalette)], nil
	case colorNames[s] != "":
		return colorNames[s], nil
	case hexColorRe.MatchString(s):
		if len(s) == 4 {
			s = "#" + strings.Repeat(s[1:2], 2) + strings.Repeat(s[2:3], 2) + strings.Repeat(s[3:4], 2)
		}
		return s, nil
	}
	names := make([]string, 0, len(colorNames))
	for n := range colorNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", fmt.Errorf("color %q: #rrggbb か、%s のどれかを書きます", s, strings.Join(names, "、"))
}

// calendarsPath は calendars.yaml の場所（$XDG_CONFIG_HOME/brain-deck/、なければ ~/.config/brain-deck/）。macOS でも同じ。
func calendarsPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "brain-deck", "calendars.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "brain-deck", "calendars.yaml")
}

const calendarsExample = `# ~/.config/brain-deck/calendars.yaml の例（chmod 600 にする。URL は秘密の情報）
days: 7                       # 今日から何日先までを送るか（1〜31）
calendars:
  - name: 仕事                # Brain の画面と、セルの calendars: で使う名前
    url: https://calendar.google.com/calendar/ical/xxxx/private-xxxx/basic.ics
    color: blue               # #rrggbb か、blue green orange purple yellow cyan pink gray red white
  - name: 家
    url: webcal://p00-caldav.icloud.com/published/2/xxxx
    color: green`

// loadCalendars は、--ics か calendars.yaml から、取ってくるカレンダーを決める。
func loadCalendars(o *options, stderr io.Writer) ([]calendarCfg, int, error) {
	days := o.days
	if len(o.ics) > 0 {
		var cs []calendarCfg
		for i, u := range o.ics {
			name := "カレンダー"
			if len(o.ics) > 1 {
				name = fmt.Sprintf("カレンダー%d", i+1)
			}
			cs = append(cs, calendarCfg{Name: name, URL: u})
		}
		if days == 0 {
			days = defaultCalDays
		}
		return cs, days, nil
	}
	path := o.config
	if path == "" {
		path = calendarsPath()
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, usageError(fmt.Sprintf("カレンダーの設定 %s がありません。--ics <URL> で URL を指定するか、次のように書いてください：\n%s", path, calendarsExample))
	}
	if err != nil {
		return nil, 0, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(stderr, "brain-deck: 注意：%s はほかのユーザーも読めます。ICS の URL は秘密の情報なので、chmod 600 %s にしてください\n", path, path)
	}
	var f calendarsFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, usageError(fmt.Sprintf("%s：%v", path, err))
	}
	if len(f.Calendars) == 0 {
		return nil, 0, usageError(path + " に calendars がありません：\n" + calendarsExample)
	}
	seen := map[string]bool{}
	for i := range f.Calendars {
		c := &f.Calendars[i]
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" || len([]rune(c.Name)) > maxCalNameRunes || seen[c.Name] {
			return nil, 0, usageError(fmt.Sprintf("%s：%d 番目のカレンダーの name は、1〜%d 文字で、ほかと違う名前にします", path, i+1, maxCalNameRunes))
		}
		seen[c.Name] = true
		if strings.TrimSpace(c.URL) == "" {
			return nil, 0, usageError(fmt.Sprintf("%s：カレンダー「%s」に url がありません", path, c.Name))
		}
	}
	if days == 0 {
		days = f.Days
	}
	if days == 0 {
		days = defaultCalDays
	}
	if days < 1 || days > maxCalDays {
		return nil, 0, usageError(fmt.Sprintf("days は 1〜%d です（%d）", maxCalDays, days))
	}
	return f.Calendars, days, nil
}

// redactURL は、秘密の URL をエラーやログに出さないよう、スキームとホストだけにする。
func redactURL(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || p.Host == "" {
		if strings.HasPrefix(u, "/") || strings.HasPrefix(u, "file:") {
			return "ファイル " + filepath.Base(strings.TrimPrefix(u, "file://"))
		}
		return "（URL）"
	}
	return p.Scheme + "://" + p.Host + "/…"
}

// fetchError は、取得の失敗。URL は含めない。
type fetchError struct{ msg string }

func (e *fetchError) Error() string { return e.msg }

// fetchICS は、URL（https、http、webcal）かファイルから ICS を読む。
func fetchICS(ctx context.Context, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "file://") || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "~/") {
		path := strings.TrimPrefix(raw, "file://")
		if strings.HasPrefix(path, "~/") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, path[2:])
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, &fetchError{fmt.Sprintf("ファイルを読めません（%v）", errors.Unwrap(err))}
		}
		return b, nil
	}
	u := raw
	if strings.HasPrefix(strings.ToLower(u), "webcal://") {
		u = "https://" + u[len("webcal://"):]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
		return nil, &fetchError{"URL の形が違います（https:// か webcal:// で始まる ICS の URL を書きます）"}
	}
	req.Header.Set("User-Agent", "brain-deck/"+versionString())
	req.Header.Set("Accept", "text/calendar, */*;q=0.5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error は URL を含むので外す
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &fetchError{"時間内に取得できませんでした"}
		}
		return nil, &fetchError{fmt.Sprintf("取得できません（%v）", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &fetchError{fmt.Sprintf("取得できません（HTTP %d）", resp.StatusCode)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxICSBytes+1))
	if err != nil {
		return nil, &fetchError{fmt.Sprintf("取得の途中で切れました（%v）", err)}
	}
	if len(b) > maxICSBytes {
		return nil, &fetchError{fmt.Sprintf("大きすぎます（%d MiB を超える）", maxICSBytes>>20)}
	}
	return b, nil
}

// calendarOut は set_calendar で送る、カレンダー 1 つ。
type calendarOut struct {
	Name      string     `json:"name"`
	Color     string     `json:"color"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	Events    []calEvent `json:"events"`
}

// gatherCalendars は、すべてのカレンダーを並べて取ってきて、範囲の予定にする。失敗したものは Error を入れる。
func gatherCalendars(cfgs []calendarCfg, win window, timeout time.Duration, verbose bool, stderr io.Writer) ([]calendarOut, []string) {
	out := make([]calendarOut, len(cfgs))
	warns := make([][]string, len(cfgs))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var wg sync.WaitGroup
	for i, c := range cfgs {
		color, err := normColor(c.Color, i)
		out[i] = calendarOut{Name: c.Name, Color: color, Events: []calEvent{}}
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		wg.Add(1)
		go func(i int, c calendarCfg) {
			defer wg.Done()
			t0 := time.Now()
			b, err := fetchICS(ctx, c.URL)
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			evs, _, w, err := expandICS(b, win)
			if err != nil {
				out[i].Error = "ICS として読めません（" + err.Error() + "）"
				return
			}
			now := time.Now().UTC()
			out[i].FetchedAt, out[i].Events = &now, evs
			warns[i] = w
			if verbose {
				fmt.Fprintf(stderr, "brain-deck: %s: %s から %d バイト、%d 件（%v）\n", c.Name, redactURL(c.URL), len(b), len(evs), time.Since(t0).Round(time.Millisecond))
			}
		}(i, c)
	}
	wg.Wait()
	var notes []string
	for i, w := range warns {
		for _, s := range w {
			notes = append(notes, out[i].Name+"："+s)
		}
	}
	// Brain の上限を超えたら、始まりの早いものから残す
	total := 0
	for _, c := range out {
		total += len(c.Events)
	}
	if total > maxCalEvents {
		var all []time.Time
		for _, c := range out {
			for _, e := range c.Events {
				all = append(all, e.sortKey())
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
		cut := all[maxCalEvents-1]
		kept := 0
		for i := range out {
			var ev []calEvent
			for _, e := range out[i].Events {
				if !e.sortKey().After(cut) && kept < maxCalEvents {
					ev = append(ev, e)
					kept++
				}
			}
			out[i].Events = ev
		}
		notes = append(notes, fmt.Sprintf("予定が %d 件あり、Brain の上限（%d 件）を超えたので、先の予定を送りませんでした（days を減らしてください）", total, maxCalEvents))
	}
	return out, notes
}

const exitFetch = 7 // 予定の取得に失敗した（ほかのカレンダーは送った）

const calendarUsage = "calendar のあとには sync、list、clear のどれかを書きます（brain-deck --help）"

// runCalendar は brain-deck calendar を実行する。sync は、ポートを開く前に予定を取ってくる。
func runCalendar(o *options, stdout, stderr io.Writer) int {
	a := o.args[1:]
	sub := "list"
	if len(a) > 0 {
		sub, a = a[0], a[1:]
	}
	if len(a) > 0 {
		return report(stderr, usageError("calendar "+sub+" は引数を取りません（URL は --ics <URL> で渡します）"))
	}
	if sub != "sync" && (len(o.ics) > 0 || o.days != 0 || o.config != "" || o.dryRun) {
		return report(stderr, usageError("--ics、--days、--config、--dry-run は calendar sync で使います"))
	}
	switch sub {
	case "list", "ls":
		return connectAndRun(o, stdout, stderr, func(c *Client) (string, error) { return listCalendar(c, o.json) })
	case "clear":
		return connectAndRun(o, stdout, stderr, func(c *Client) (string, error) {
			if err := need(c, "set_calendar"); err != nil {
				return "", err
			}
			if _, err := c.Call("set_calendar", map[string]any{"calendars": []any{}, "source": "brain-deck"}, callTimeout()); err != nil {
				return "", err
			}
			return "Brain の予定を消しました", nil
		})
	case "sync":
	default:
		return report(stderr, usageError(calendarUsage))
	}

	cfgs, days, err := loadCalendars(o, stderr)
	if err != nil {
		return report(stderr, err)
	}
	now := time.Now()
	win := newWindow(now, days)
	cals, notes := gatherCalendars(cfgs, win, o.fetchTimeout, o.verbose, stderr)
	for _, n := range notes {
		fmt.Fprintln(stderr, "brain-deck: 注意："+n)
	}
	failed := 0
	var lines []string
	for i, c := range cals {
		if c.Error != "" {
			failed++
			fmt.Fprintf(stderr, "brain-deck: %s（%s）：%s\n", c.Name, redactURL(cfgs[i].URL), c.Error)
			lines = append(lines, fmt.Sprintf("%s：取得できませんでした（Brain では前の予定を残し、「失敗」と出します）", c.Name))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s：%d 件", c.Name, len(c.Events)))
	}
	rng := fmt.Sprintf("%s〜%s", win.from.Format("1/2"), win.to.Add(-time.Second).Format("1/2"))
	if o.dryRun {
		if !o.quiet {
			fmt.Fprintln(stdout, formatEvents(cals, now))
			fmt.Fprintf(stdout, "%s の予定（%s。Brain には送っていません）\n", rng, strings.Join(lines, "、"))
		}
		if failed > 0 {
			return exitFetch
		}
		return exitOK
	}
	code := connectAndRun(o, stdout, stderr, func(c *Client) (string, error) {
		if err := need(c, "set_calendar"); err != nil {
			return "", err
		}
		var msgs []string
		if !o.noTimeSync { // PC の時刻を正とする。予定の「今」と最終更新が、Brain でずれないように
			m, err := setTime(c)
			if err != nil {
				return "", err
			}
			msgs = append(msgs, m)
		}
		raw, err := c.Call("set_calendar", map[string]any{"calendars": cals, "from": win.from.Format("2006-01-02"), "days": days,
			"source": "brain-deck"}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Shown bool `json:"shown"`
		}
		json.Unmarshal(raw, &r)
		if !r.Shown {
			fmt.Fprintln(stderr, "brain-deck: 注意：今の設定には、カレンダーのセル（widget: calendar）がありません。予定は Brain に保存しました")
		}
		return fmt.Sprintf("%s の予定を送りました（%s）\n%s", rng, strings.Join(lines, "、"), strings.Join(msgs, "\n")), nil
	})
	if code == exitOK && failed > 0 {
		return exitFetch
	}
	return code
}

// connectAndRun は、Brain につないで cmd を実行し、終了コードを返す（run の後半と同じ）。
func connectAndRun(o *options, stdout, stderr io.Writer, cmd func(*Client) (string, error)) int {
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
	if out = strings.TrimSpace(out); out != "" && !o.quiet {
		fmt.Fprintln(stdout, out)
	}
	return exitOK
}

var jaWday = [...]string{"日", "月", "火", "水", "木", "金", "土"}

// formatEvents は、予定を日付ごとに並べて書く（--dry-run と list）。時刻は PC のタイムゾーン。
func formatEvents(cals []calendarOut, now time.Time) string {
	type row struct {
		at   time.Time
		line string
	}
	var rows []row
	for _, c := range cals {
		for _, e := range c.Events {
			var at time.Time
			var when string
			if e.Start != nil {
				at = e.Start.Local()
				when = at.Format("01/02") + "(" + jaWday[at.Weekday()] + ") " + at.Format("15:04") + "-" + e.End.Local().Format("15:04")
				if !sameDay(at, e.End.Local()) {
					when = at.Format("01/02") + "(" + jaWday[at.Weekday()] + ") " + at.Format("15:04") + "-" + e.End.Local().Format("01/02 15:04")
				}
			} else {
				at = e.sortKey()
				when = at.Format("01/02") + "(" + jaWday[at.Weekday()] + ") 終日"
				if e.EndDay != "" {
					end, _ := time.ParseInLocation("2006-01-02", e.EndDay, time.Local)
					when += "（" + end.AddDate(0, 0, -1).Format("01/02") + " まで）"
				}
			}
			title := e.Title
			if title == "" {
				title = "（名前なし）"
			}
			rows = append(rows, row{at, fmt.Sprintf("%s  %s  [%s]", when, title, c.Name)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.line + "\n")
	}
	if len(rows) == 0 {
		b.WriteString("（予定はありません）\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// listCalendar は、Brain にある予定（get_calendar）を出す。
func listCalendar(c *Client, asJSON bool) (string, error) {
	if err := need(c, "get_calendar"); err != nil {
		return "", err
	}
	raw, err := c.Call("get_calendar", nil, callTimeout())
	if err != nil {
		return "", err
	}
	if asJSON {
		return string(raw), nil
	}
	var r struct {
		Rev       uint64        `json:"rev"`
		Shown     bool          `json:"shown"`
		Calendars []calendarOut `json:"calendars"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if len(r.Calendars) == 0 {
		return "Brain に予定はありません（brain-deck calendar sync で送ります）", nil
	}
	var b strings.Builder
	b.WriteString(formatEvents(r.Calendars, time.Now()) + "\n\n")
	for _, cal := range r.Calendars {
		at := "なし"
		if cal.FetchedAt != nil {
			at = cal.FetchedAt.Local().Format("01/02 15:04")
		}
		fmt.Fprintf(&b, "%s（%s）：%d 件、最終更新 %s", cal.Name, cal.Color, len(cal.Events), at)
		if cal.Error != "" {
			fmt.Fprintf(&b, "、取得に失敗：%s", cal.Error)
		}
		b.WriteString("\n")
	}
	if !r.Shown {
		b.WriteString("（今の設定には、カレンダーのセルがありません）\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
