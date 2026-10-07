package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Google カレンダーの「iCal 形式の非公開 URL」と同じ形（VTIMEZONE 付き、IANA の名前、UTC の UNTIL、例外）
const googleICS = "BEGIN:VCALENDAR\r\nPRODID:-//Google Inc//Google Calendar 70.9054//EN\r\nVERSION:2.0\r\n" +
	"CALSCALE:GREGORIAN\r\nX-WR-CALNAME:仕事\r\nX-WR-TIMEZONE:Asia/Tokyo\r\n" + `BEGIN:VTIMEZONE
TZID:Asia/Tokyo
X-LIC-LOCATION:Asia/Tokyo
BEGIN:STANDARD
TZOFFSETFROM:+0900
TZOFFSETTO:+0900
TZNAME:JST
DTSTART:19700101T000000
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
DTSTART;TZID=Asia/Tokyo:20260907T100000
DTEND;TZID=Asia/Tokyo:20260907T103000
RRULE:FREQ=WEEKLY;WKST=SU;BYDAY=MO,WE
EXDATE;TZID=Asia/Tokyo:20261007T100000
UID:standup@google.com
SUMMARY:朝会
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Asia/Tokyo:20261012T140000
DTEND;TZID=Asia/Tokyo:20261012T150000
RECURRENCE-ID;TZID=Asia/Tokyo:20261012T100000
UID:standup@google.com
SUMMARY:朝会（午後に変更）
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Asia/Tokyo:20261014T100000
RECURRENCE-ID;TZID=Asia/Tokyo:20261014T100000
STATUS:CANCELLED
UID:standup@google.com
END:VEVENT
BEGIN:VEVENT
DTSTART;VALUE=DATE:20261005
DTEND;VALUE=DATE:20261010
UID:week@google.com
SUMMARY:社内イベント週間
DESCRIPTION:長い説明\nは送らない
END:VEVENT
BEGIN:VEVENT
DTSTART;VALUE=DATE:19900610
DTEND;VALUE=DATE:19900611
RRULE:FREQ=YEARLY
UID:bday@google.com
SUMMARY:誕生日
END:VEVENT
BEGIN:VEVENT
DTSTART;VALUE=DATE:20261009
DTEND;VALUE=DATE:20261010
RRULE:FREQ=YEARLY
UID:anniv@google.com
SUMMARY:記念日
END:VEVENT
BEGIN:VEVENT
DTSTART:20261001T010000Z
DTEND:20261001T020000Z
RRULE:FREQ=DAILY;UNTIL=20261007T010000Z
UID:daily@google.com
SUMMARY:日次の確認\, 速報
LOCATION:会議室A\; 2F
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Asia/Tokyo:20261006T230000
DTEND;TZID=Asia/Tokyo:20261007T013000
UID:late@google.com
SUMMARY:夜間メンテ
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Asia/Tokyo:20261008T090000
DURATION:PT45M
RRULE:FREQ=DAILY;COUNT=2
UID:count@google.com
SUMMARY:研修
END:VEVENT
BEGIN:VEVENT
DTSTART:20261009T120000
DTEND:20261009T130000
UID:floating@google.com
SUMMARY:昼食（floating）
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Asia/Tokyo:20261020T100000
DTEND;TZID=Asia/Tokyo:20261020T110000
UID:far@google.com
SUMMARY:範囲の外
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Asia/Tokyo:20261008T170000
DTEND;TZID=Asia/Tokyo:20261008T180000
UID:gone@google.com
STATUS:CANCELLED
SUMMARY:中止になった会議
END:VEVENT
END:VCALENDAR
`

func tokyo(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// describe は、予定を Asia/Tokyo の時刻で 1 行ずつ書く。
func describe(evs []calEvent, loc *time.Location) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Start != nil {
			fmt.Fprintf(&b, "%s-%s %s", e.Start.In(loc).Format("01/02 15:04"), e.End.In(loc).Format("01/02 15:04"), e.Title)
		} else {
			fmt.Fprintf(&b, "%s..%s %s", e.Day, e.EndDay, e.Title)
		}
		if e.Location != "" {
			fmt.Fprintf(&b, " @%s", e.Location)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestExpandGoogle(t *testing.T) {
	loc := tokyo(t)
	old := time.Local
	time.Local = loc
	defer func() { time.Local = old }()
	win := newWindow(time.Date(2026, 10, 6, 9, 41, 0, 0, loc), 7) // 10/6 から 10/13 まで
	evs, name, warns, err := expandICS([]byte(googleICS), win)
	if err != nil {
		t.Fatal(err)
	}
	if name != "仕事" || len(warns) != 0 {
		t.Errorf("name %q warnings %q", name, warns)
	}
	want := `2026-10-05..2026-10-10 社内イベント週間
10/06 10:00-10/06 11:00 日次の確認, 速報 @会議室A; 2F
10/06 23:00-10/07 01:30 夜間メンテ
10/07 10:00-10/07 11:00 日次の確認, 速報 @会議室A; 2F
10/08 09:00-10/08 09:45 研修
2026-10-09.. 記念日
10/09 09:00-10/09 09:45 研修
10/09 12:00-10/09 13:00 昼食（floating）
10/12 14:00-10/12 15:00 朝会（午後に変更）
`
	// 朝会：10/7（水）は EXDATE、10/12（月）は午後に移動、10/14 は範囲の外。10/12 の元の 10:00 は出さない
	if got := describe(evs, loc); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// 範囲を広げると、中止した回（10/14）は出ない。10/19 の朝会は出る。誕生日は毎年
	win = newWindow(time.Date(2026, 10, 13, 0, 0, 0, 0, loc), 7)
	evs, _, _, _ = expandICS([]byte(googleICS), win)
	got := describe(evs, loc)
	for _, s := range []string{"10/19 10:00-10/19 10:30 朝会", "10/20 10:00-10/20 11:00 範囲の外"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in\n%s", s, got)
		}
	}
	if strings.Contains(got, "10/14") || strings.Contains(got, "中止") {
		t.Errorf("cancelled instance is shown:\n%s", got)
	}
	win = newWindow(time.Date(2027, 6, 9, 0, 0, 0, 0, loc), 3)
	evs, _, _, _ = expandICS([]byte(googleICS), win)
	if got := describe(evs, loc); !strings.Contains(got, "2027-06-10.. 誕生日") {
		t.Errorf("yearly all-day:\n%s", got)
	}
}

// 夏時間をまたぐ繰り返しは、その土地の壁時計の時刻のまま（UTC ではずれる）。
func TestExpandDST(t *testing.T) {
	ics := `BEGIN:VCALENDAR
BEGIN:VEVENT
DTSTART;TZID=America/New_York:20261026T100000
DTEND;TZID=America/New_York:20261026T110000
RRULE:FREQ=WEEKLY
UID:ny
SUMMARY:NY weekly
END:VEVENT
END:VCALENDAR
`
	utc := time.UTC
	win := newWindow(time.Date(2026, 10, 25, 0, 0, 0, 0, utc), 15)
	evs, _, _, err := expandICS([]byte(ics), win)
	if err != nil {
		t.Fatal(err)
	}
	if got := describe(evs, utc); got != "10/26 14:00-10/26 15:00 NY weekly\n11/02 15:00-11/02 16:00 NY weekly\n11/09 15:00-11/09 16:00 NY weekly\n" {
		t.Errorf("DST:\n%s", got)
	}
}

// Outlook（Exchange）の形：Windows のタイムゾーンの名前と、IANA にない名前の VTIMEZONE。
func TestExpandOutlookZones(t *testing.T) {
	ics := `BEGIN:VCALENDAR
PRODID:Microsoft Exchange Server 2010
BEGIN:VTIMEZONE
TZID:Tokyo Standard Time
BEGIN:STANDARD
DTSTART:16010101T000000
TZOFFSETFROM:+0900
TZOFFSETTO:+0900
END:STANDARD
END:VTIMEZONE
BEGIN:VTIMEZONE
TZID:Custom Eastern
BEGIN:STANDARD
DTSTART:16011104T020000
RRULE:FREQ=YEARLY;BYDAY=1SU;BYMONTH=11
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010311T020000
RRULE:FREQ=YEARLY;BYDAY=2SU;BYMONTH=3
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
DTSTART;TZID=Tokyo Standard Time:20261006T150000
DTEND;TZID=Tokyo Standard Time:20261006T160000
UID:a
SUMMARY:Windows の名前
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Custom Eastern:20261026T100000
DTEND;TZID=Custom Eastern:20261026T103000
RRULE:FREQ=WEEKLY;COUNT=3
UID:b
SUMMARY:VTIMEZONE だけ
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID="/mozilla.org/20050126_1/Europe/Berlin":20261027T090000
DTEND;TZID="/mozilla.org/20050126_1/Europe/Berlin":20261027T100000
UID:c
SUMMARY:古い Mozilla
END:VEVENT
BEGIN:VEVENT
DTSTART;TZID=Mars/Olympus:20261028T090000
DTEND;TZID=Mars/Olympus:20261028T100000
UID:d
SUMMARY:知らない
END:VEVENT
END:VCALENDAR
`
	old := time.Local
	time.Local = time.UTC
	defer func() { time.Local = old }()
	win := newWindow(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), 40)
	evs, _, warns, err := expandICS([]byte(ics), win)
	if err != nil {
		t.Fatal(err)
	}
	want := `10/06 06:00-10/06 07:00 Windows の名前
10/26 14:00-10/26 14:30 VTIMEZONE だけ
10/27 08:00-10/27 09:00 古い Mozilla
10/28 09:00-10/28 10:00 知らない
11/02 15:00-11/02 15:30 VTIMEZONE だけ
11/09 15:00-11/09 15:30 VTIMEZONE だけ
`
	if got := describe(evs, time.UTC); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "Mars/Olympus") {
		t.Errorf("warnings = %q", warns)
	}
}

func TestICSParseEdges(t *testing.T) {
	if _, _, _, err := expandICS([]byte("<html>login</html>"), newWindow(time.Now(), 7)); err == nil {
		t.Error("HTML must be rejected")
	}
	// 折り返し、BOM、LF だけの改行、引用符の中の : と ;
	ics := "\xef\xbb\xbfBEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20261006T010000Z\nDTEND:20261006T020000Z\nSUMMARY:とても長い\n  名前の予定\nX-PARAM;FOO=\"a:b;c\":値\nEND:VEVENT\nEND:VCALENDAR\n"
	evs, _, _, err := expandICS([]byte(ics), newWindow(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), 1))
	if err != nil || len(evs) != 1 || evs[0].Title != "とても長い 名前の予定" {
		t.Errorf("folded: %v %+v", err, evs)
	}
	for _, c := range []struct {
		in        string
		days      int
		d         time.Duration
		shouldErr bool
	}{{"P1D", 1, 0, false}, {"PT1H30M", 0, 90 * time.Minute, false}, {"P1W", 7, 0, false}, {"P1DT2H", 1, 2 * time.Hour, false}, {"P", 0, 0, true}, {"1H", 0, 0, true}} {
		days, d, err := parseICSDuration(c.in)
		if (err != nil) != c.shouldErr || days != c.days || d != c.d {
			t.Errorf("duration %q = %d %v %v", c.in, days, d, err)
		}
	}
	if got := cleanText("a\tb\n\x07c  d"); got != "a b c d" {
		t.Errorf("cleanText = %q", got)
	}
	// 1 秒ごとの繰り返しでも止まらない
	ics = "BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART:20200101T000000Z\nRRULE:FREQ=SECONDLY\nSUMMARY:x\nEND:VEVENT\nEND:VCALENDAR\n"
	start := time.Now()
	expandICS([]byte(ics), newWindow(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), 1))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("SECONDLY took %v", d)
	}
}
