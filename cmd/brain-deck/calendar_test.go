//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// todayICS は、今日の 10:00 から 1 時間の予定と、明日の終日の予定を持つ ICS を作る（PC のタイムゾーン）。
func todayICS() string {
	now := time.Now()
	d := func(off int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day()+off, 0, 0, 0, 0, time.Local)
	}
	s := d(0).Add(10 * time.Hour).UTC()
	return fmt.Sprintf("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:%s\r\nDTEND:%s\r\nSUMMARY:定例\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:b\r\nDTSTART;VALUE=DATE:%s\r\nSUMMARY:休み\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:c\r\nDTSTART;VALUE=DATE:%s\r\nSUMMARY:遠い先\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		s.Format("20060102T150405Z"), s.Add(time.Hour).Format("20060102T150405Z"), d(1).Format("20060102"), d(40).Format("20060102"))
}

func TestCalendarSync(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret-token/basic.ics":
			w.Write([]byte(todayICS()))
		case "/html":
			w.Write([]byte("<html>sign in</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := newFakeBrain(t)
	go f.serve([]string{"hello", "get_status", "set_time", "set_calendar", "get_calendar"})

	// 設定ファイルがなければ、例を出して使い方の誤り
	code, _, errs := runCLI(t, "--port", f.slave, "calendar", "sync")
	if code != exitUsage || !strings.Contains(errs, "calendars.yaml") || !strings.Contains(errs, "basic.ics") {
		t.Fatalf("no config: exit %d %s", code, errs)
	}
	path := filepath.Join(cfgDir, "brain-deck", "calendars.yaml")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(fmt.Sprintf("days: 7\ncalendars:\n  - name: 仕事\n    url: %s/secret-token/basic.ics\n    color: blue\n"+
		"  - name: 家\n    url: %s/missing-token.ics\n  - name: 壊れ\n    url: %s/html\n    color: \"#abc\"\n", srv.URL, srv.URL, srv.URL)), 0o644)

	// --dry-run は Brain に送らない
	code, out, errs := runCLI(t, "calendar", "sync", "--dry-run")
	if code != exitFetch || !strings.Contains(out, "定例  [仕事]") || !strings.Contains(out, "休み  [仕事]") || strings.Contains(out, "遠い先") {
		t.Fatalf("dry-run: exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, "chmod 600") || !strings.Contains(errs, "HTTP 404") || !strings.Contains(errs, "ICS として読めません") {
		t.Errorf("dry-run stderr: %s", errs)
	}
	if strings.Contains(errs, "secret-token") || strings.Contains(errs, "missing-token") || strings.Contains(out, "token") {
		t.Errorf("the secret URL leaked:\n%s\n%s", out, errs)
	}
	for len(f.got) > 0 {
		<-f.got
	}

	os.Chmod(path, 0o600)
	code, out, errs = runCLI(t, "--port", f.slave, "calendar", "sync")
	if code != exitFetch || !strings.Contains(out, "仕事：2 件") || !strings.Contains(out, "家：取得できませんでした") {
		t.Fatalf("sync: exit %d\n%s\n%s", code, out, errs)
	}
	if strings.Contains(errs, "chmod") || strings.Contains(errs+out, "token") {
		t.Errorf("sync stderr: %s", errs)
	}
	var cmds []string
	var set map[string]any
	for len(f.got) > 0 {
		r := <-f.got
		cmds = append(cmds, r["cmd"].(string))
		if r["cmd"] == "set_calendar" {
			set = r
		}
	}
	if strings.Join(cmds, ",") != "hello,set_time,set_calendar" {
		t.Errorf("commands = %v (time must be synced before sending the calendar)", cmds)
	}
	b, _ := json.Marshal(set["calendars"])
	var cals []calendarOut
	json.Unmarshal(b, &cals)
	if len(cals) != 3 || cals[0].Color != "#4f9dff" || len(cals[0].Events) != 2 || cals[0].FetchedAt == nil ||
		cals[1].Error != "取得できません（HTTP 404）" || len(cals[1].Events) != 0 || cals[2].Color != "#aabbcc" || set["days"] != 7.0 {
		t.Errorf("set_calendar = %s", b)
	}
	if strings.Contains(string(b), "token") {
		t.Errorf("the URL was sent to the Brain: %s", b)
	}

	// Brain にある予定
	code, out, _ = runCLI(t, "--port", f.slave, "calendar")
	if code != exitOK || !strings.Contains(out, "定例  [仕事]") || !strings.Contains(out, "家（") || !strings.Contains(out, "取得に失敗：取得できません（HTTP 404）") {
		t.Errorf("list: exit %d\n%s", code, out)
	}
	// --ics で直接指定（設定ファイルを使わない）
	code, out, _ = runCLI(t, "--port", f.slave, "calendar", "sync", "--ics", srv.URL+"/secret-token/basic.ics", "--days", "1", "--no-time-sync")
	if code != exitOK || !strings.Contains(out, "カレンダー：2 件") {
		t.Errorf("--ics: exit %d %s", code, out)
	}
	for _, bad := range [][]string{{"calendar", "sync", "x"}, {"calendar", "nope"}, {"calendar", "list", "--days", "3"}, {"text", "a", "b", "--ics", "x"}, {"calendar", "sync", "--days", "40"}} {
		if code, _, _ := runCLI(t, bad...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d", bad, code, exitUsage)
		}
	}
}

func TestCalendarConfigErrors(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ yaml, want string }{
		{"calendars: []\n", "calendars がありません"},
		{"calendars:\n  - name: a\n", "url がありません"},
		{"calendars:\n  - url: x\n", "name"},
		{"calendars:\n  - {name: a, url: x}\n  - {name: a, url: y}\n", "ほかと違う名前"},
		{"calendars:\n  - {name: a, url: x, colour: red}\n", "colour"},
		{"days: 99\ncalendars:\n  - {name: a, url: x}\n", "days は 1〜31"},
	} {
		p := filepath.Join(dir, "c.yaml")
		os.WriteFile(p, []byte(c.yaml), 0o600)
		_, _, err := loadCalendars(&options{config: p}, os.Stderr)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v (want %q)", c.yaml, err, c.want)
		}
	}
	if _, err := normColor("chartreuse", 0); err == nil {
		t.Error("unknown color name must be rejected")
	}
	if got := redactURL("https://calendar.google.com/calendar/ical/me%40gmail.com/private-abc123/basic.ics"); got != "https://calendar.google.com/…" {
		t.Errorf("redact = %q", got)
	}
}
