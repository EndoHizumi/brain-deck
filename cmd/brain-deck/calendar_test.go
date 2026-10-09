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
// 20 日前（先月の初めより後）の予定は送る範囲に入り、70 日前と 100 日先（先月の初めより前、来月の終わりより後）は入らない。
func todayICS() string {
	now := time.Now()
	d := func(off int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day()+off, 0, 0, 0, 0, time.Local)
	}
	s := d(0).Add(10 * time.Hour).UTC()
	return fmt.Sprintf("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:%s\r\nDTEND:%s\r\nSUMMARY:定例\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:b\r\nDTSTART;VALUE=DATE:%s\r\nSUMMARY:休み\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:c\r\nDTSTART;VALUE=DATE:%s\r\nSUMMARY:遠い先\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:d\r\nDTSTART;VALUE=DATE:%s\r\nSUMMARY:少し前\r\nEND:VEVENT\r\n"+
		"BEGIN:VEVENT\r\nUID:e\r\nDTSTART;VALUE=DATE:%s\r\nSUMMARY:ずっと前\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		s.Format("20060102T150405Z"), s.Add(time.Hour).Format("20060102T150405Z"), d(1).Format("20060102"), d(100).Format("20060102"),
		d(-20).Format("20060102"), d(-70).Format("20060102"))
}

// holidayICS は、明日を祝日にする ICS（Google の「日本の祝日」の形）。明後日の「祭日」（祝日でない行事）は送らない。
func holidayICS() string {
	now := time.Now()
	d := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.Local)
	return "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:h\r\nDTSTART;VALUE=DATE:" + d.Format("20060102") +
		"\r\nDTEND;VALUE=DATE:" + d.AddDate(0, 0, 1).Format("20060102") + "\r\nDESCRIPTION:祝日\r\nSUMMARY:祝日\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:o\r\nDTSTART;VALUE=DATE:" + d.AddDate(0, 0, 1).Format("20060102") +
		"\r\nDESCRIPTION:祭日\\n祭日を非表示にするには、Google カレンダーの [設定] > [日本の祝日] に移動してください\r\nSUMMARY:七五三\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func TestCalendarSync(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret-token/basic.ics":
			w.Write([]byte(todayICS()))
		case "/holiday.ics":
			w.Write([]byte(holidayICS()))
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
		"  - name: 家\n    url: %s/missing-token.ics\n  - name: 壊れ\n    url: %s/html\n    color: \"#abc\"\n"+
		"  - name: 祝日\n    url: %s/holiday.ics\n    color: red\n    holiday: true\n", srv.URL, srv.URL, srv.URL, srv.URL)), 0o644)

	// --dry-run は Brain に送らない
	code, out, errs := runCLI(t, "calendar", "sync", "--dry-run")
	if code != exitFetch || !strings.Contains(out, "定例  [仕事]") || !strings.Contains(out, "休み  [仕事]") || !strings.Contains(out, "少し前  [仕事]") ||
		strings.Contains(out, "遠い先") || strings.Contains(out, "ずっと前") || !strings.Contains(out, "祝日  [祝日]") || strings.Contains(out, "七五三") {
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
	if code != exitFetch || !strings.Contains(out, "仕事：3 件") || !strings.Contains(out, "家：取得できませんでした") {
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
	win := newSyncWindow(time.Now(), syncRange{days: 7, past: 1, future: 1})
	if len(cals) != 4 || cals[0].Color != "#4f9dff" || len(cals[0].Events) != 3 || cals[0].FetchedAt == nil || cals[0].Holiday ||
		cals[1].Error != "取得できません（HTTP 404）" || len(cals[1].Events) != 0 || cals[2].Color != "#aabbcc" ||
		!cals[3].Holiday || cals[3].Color != "#ff5858" || len(cals[3].Events) != 1 ||
		set["days"] != float64(win.days()) || set["from"] != win.from.Format("2006-01-02") || win.days() < 58 {
		t.Errorf("set_calendar = %s (from %v days %v)", b, set["from"], set["days"])
	}
	if strings.Contains(string(b), "token") {
		t.Errorf("the URL was sent to the Brain: %s", b)
	}

	// Brain にある予定
	code, out, _ = runCLI(t, "--port", f.slave, "calendar")
	if code != exitOK || !strings.Contains(out, "定例  [仕事]") || !strings.Contains(out, "家（") || !strings.Contains(out, "取得に失敗：取得できません（HTTP 404）") ||
		!strings.Contains(out, "祝日（#ff5858、祝日）：1 件") {
		t.Errorf("list: exit %d\n%s", code, out)
	}
	// --ics で直接指定（設定ファイルを使わない）
	code, out, _ = runCLI(t, "--port", f.slave, "calendar", "sync", "--ics", srv.URL+"/secret-token/basic.ics", "--days", "1", "--no-time-sync")
	if code != exitOK || !strings.Contains(out, "カレンダー：3 件") {
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
		{"past_months: 4\ncalendars:\n  - {name: a, url: x}\n", "past_months は 0〜3"},
		{"future_months: -1\ncalendars:\n  - {name: a, url: x}\n", "future_months は 0〜3"},
		{"calendars:\n  - {name: a, url: x, holiday: yes please}\n", "into bool"},
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

func TestCalendarRangeConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	for _, c := range []struct {
		yaml string
		want syncRange
	}{
		{"calendars:\n  - {name: a, url: x}\n", syncRange{7, 1, 1}},
		{"past_months: 0\nfuture_months: 3\ndays: 3\ncalendars:\n  - {name: a, url: x, holiday: true}\n", syncRange{3, 0, 3}},
	} {
		os.WriteFile(p, []byte(c.yaml), 0o600)
		cs, r, err := loadCalendars(&options{config: p}, os.Stderr)
		if err != nil || r != c.want {
			t.Errorf("%q: %+v %v", c.yaml, r, err)
		}
		if strings.Contains(c.yaml, "holiday") && !cs[0].Holiday {
			t.Errorf("holiday not read")
		}
	}
	// --ics でも、先月の初めから来月の終わりまで
	if _, r, err := loadCalendars(&options{ics: []string{"x"}, days: 2}, os.Stderr); err != nil || r != (syncRange{2, 1, 1}) {
		t.Errorf("--ics: %+v %v", r, err)
	}
}

func TestSyncWindow(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	for _, c := range []struct {
		now      time.Time
		r        syncRange
		from, to string // to は含まない
		days     int
	}{
		{time.Date(2026, 10, 9, 21, 0, 0, 0, loc), syncRange{7, 1, 1}, "2026-09-01", "2026-12-01", 90},
		{time.Date(2026, 1, 15, 0, 0, 0, 0, loc), syncRange{7, 1, 1}, "2025-12-01", "2026-03-01", 89}, // 年をまたぐ
		{time.Date(2026, 12, 31, 23, 0, 0, 0, loc), syncRange{7, 1, 1}, "2026-11-01", "2027-02-01", 91},
		{time.Date(2026, 11, 28, 9, 0, 0, 0, loc), syncRange{7, 0, 0}, "2026-11-01", "2026-12-06", 34}, // days が月末を越える
		{time.Date(2026, 10, 9, 9, 0, 0, 0, loc), syncRange{7, 0, 0}, "2026-10-01", "2026-11-01", 30},
		{time.Date(2028, 3, 31, 9, 0, 0, 0, loc), syncRange{7, 1, 0}, "2028-02-01", "2028-04-08", 66}, // うるう年の 2 月から
		{time.Date(2026, 10, 9, 9, 0, 0, 0, loc), syncRange{31, 3, 3}, "2026-07-01", "2027-02-01", 214},
	} {
		w := newSyncWindow(c.now, c.r)
		if day(w.from) != c.from || day(w.to) != c.to || day(w.fromDay) != c.from || day(w.toDay) != c.to || w.days() != c.days || day(w.today) != day(c.now) {
			t.Errorf("%v %+v: %s..%s (%s..%s) days %d today %s", c.now, c.r, day(w.from), day(w.to), day(w.fromDay), day(w.toDay), w.days(), day(w.today))
		}
	}
}

func TestFitCalendars(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	today := time.Date(2026, 10, 9, 0, 0, 0, 0, loc)
	at := func(off int, title string) calEvent {
		s := today.AddDate(0, 0, off).Add(10 * time.Hour)
		e := s.Add(time.Hour)
		return calEvent{Title: title, Start: &s, End: &e}
	}
	mk := func() []calendarOut {
		var a, b []calEvent
		for i := -30; i <= 30; i++ {
			a = append(a, at(i, fmt.Sprintf("a%d", i)))
		}
		for i := -5; i <= 5; i++ {
			b = append(b, calEvent{Title: fmt.Sprintf("b%d", i), Day: today.AddDate(0, 0, i).Format("2006-01-02")})
		}
		return []calendarOut{{Name: "a", Events: a}, {Name: "b", Events: b, Holiday: true}}
	}
	// 上限の中なら何もしない
	out := mk()
	if notes := fitCalendars(out, today, 1000, 1<<20); notes != nil || len(out[0].Events) != 61 || len(out[1].Events) != 11 {
		t.Fatalf("within limits: %v", notes)
	}
	// 件数：今日に近い 20 件を残す（過ぎた予定は終わりから、先の予定は始まりまでの遠さ）。並びは元のまま
	out = mk()
	notes := fitCalendars(out, today, 20, 1<<20)
	if len(notes) != 1 || !strings.Contains(notes[0], "72 件") || !strings.Contains(notes[0], "52 件を送りませんでした") {
		t.Errorf("notes = %q", notes)
	}
	var titles []string
	for _, c := range out {
		for _, e := range c.Events {
			titles = append(titles, e.Title)
		}
	}
	if got := strings.Join(titles, " "); got != "a-5 a-4 a-3 a-2 a-1 a0 a1 a2 a3 a4 b-5 b-4 b-3 b-2 b-1 b0 b1 b2 b3 b4" {
		t.Errorf("kept %s", got)
	}
	// 大きさ：JSON が上限に入るまで、遠いものから外す
	out = mk()
	notes = fitCalendars(out, today, 1000, 2000)
	b, _ := json.Marshal(out)
	if len(b) > 2000 || len(b) < 1500 || len(notes) != 1 || !strings.Contains(notes[0], "上限（1 KiB）") {
		t.Errorf("bytes: %d %q", len(b), notes)
	}
	if !strings.Contains(string(b), `"a0"`) || strings.Contains(string(b), `"a30"`) {
		t.Errorf("kept the wrong events: %s", b)
	}
}

func TestHolidayObservances(t *testing.T) {
	win := newWindow(time.Now(), 7)
	all, _, _, err := expandICS([]byte(holidayICS()), win)
	if err != nil || len(all) != 2 {
		t.Fatalf("expandICS: %d %v", len(all), err)
	}
	evs, _, _, err := expandICSWith([]byte(holidayICS()), win, isObservance)
	if err != nil || len(evs) != 1 || evs[0].Title != "祝日" {
		t.Errorf("without observances: %+v %v", evs, err)
	}
}
