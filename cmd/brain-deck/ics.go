package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // OS にタイムゾーンの表がなくても、IANA の名前を読めるようにする
	"unicode"
	"unicode/utf8"

	"github.com/teambition/rrule-go"
)

// ---------- iCalendar（ICS、RFC 5545）の読み込みと、繰り返しの展開 ----------
//
// 予定（VEVENT）を読み、決めた範囲に入る回を、時刻の決まった予定（UTC の時刻）か、日付だけの終日の予定にする。
// Brain には展開したあとの予定だけを送るので、繰り返し、例外（EXDATE、RECURRENCE-ID）、タイムゾーンはここで解決する。
//
// 時刻の扱い：
//   - 繰り返しは、予定のタイムゾーンでの「壁時計の時刻」で展開する（RFC 5545 の決まり。夏時間をまたいでも 10:00 は 10:00）。
//     壁時計の時刻は、time.UTC の Time に年月日時分秒を入れて表す。
//   - TZID は、IANA の名前（Google、iCloud）、Windows の名前（Outlook。表で IANA に直す）、ファイルの VTIMEZONE の順に解決する。
//   - TZID のない時刻（floating）は、X-WR-TIMEZONE か、PC のタイムゾーンとみなす。

// icsProp は 1 つのプロパティ（DTSTART;TZID=Asia/Tokyo:20261006T100000 など）。
type icsProp struct {
	Name   string
	Params map[string]string // 名前は大文字。値の引用符は外す
	Value  string
}

// icsComp は BEGIN と END で囲まれた 1 つの部品（VCALENDAR、VEVENT、VTIMEZONE など）。
type icsComp struct {
	Name  string
	Props []icsProp
	Subs  []*icsComp
}

func (c *icsComp) prop(name string) *icsProp {
	for i := range c.Props {
		if c.Props[i].Name == name {
			return &c.Props[i]
		}
	}
	return nil
}

func (c *icsComp) props(name string) []icsProp {
	var out []icsProp
	for _, p := range c.Props {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

var errNotICS = errors.New("not an iCalendar file (no BEGIN:VCALENDAR)")

// parseICS は ICS の文を読み、VCALENDAR を返す（いくつあれば、中身をまとめる）。
func parseICS(b []byte) (*icsComp, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
	// 折り返し（次の行が空白かタブで始まる）を戻す
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += l[1:]
			continue
		}
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	root := &icsComp{Name: "VCALENDAR"}
	var stack []*icsComp
	found := false
	for n, l := range lines {
		p, err := parseICSLine(l)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		switch p.Name {
		case "BEGIN":
			name := strings.ToUpper(strings.TrimSpace(p.Value))
			if len(stack) == 0 {
				if name != "VCALENDAR" {
					return nil, errNotICS
				}
				found = true
				stack = append(stack, root)
				continue
			}
			c := &icsComp{Name: name}
			top := stack[len(stack)-1]
			top.Subs = append(top.Subs, c)
			stack = append(stack, c)
		case "END":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.Props = append(top.Props, p)
			}
		}
	}
	if !found {
		return nil, errNotICS
	}
	return root, nil
}

// parseICSLine は 1 行を、名前、引数、値に分ける。引用符の中の ; と : は区切りにしない。
func parseICSLine(l string) (icsProp, error) {
	p := icsProp{Params: map[string]string{}}
	inQ := false
	start := 0
	var parts []string
	for i := 0; i < len(l); i++ {
		switch c := l[i]; {
		case c == '"':
			inQ = !inQ
		case !inQ && c == ';':
			parts = append(parts, l[start:i])
			start = i + 1
		case !inQ && c == ':':
			parts = append(parts, l[start:i])
			p.Value = l[i+1:]
			p.Name = strings.ToUpper(strings.TrimSpace(parts[0]))
			for _, a := range parts[1:] {
				k, v, _ := strings.Cut(a, "=")
				p.Params[strings.ToUpper(k)] = strings.Trim(v, `"`)
			}
			return p, nil
		}
	}
	return p, fmt.Errorf("no ':' in %q", truncate(l, 40))
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// icsText は TEXT の値のエスケープ（\n \, \; \\）を戻す。
func icsText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------- タイムゾーン ----------

// zone は、あるタイムゾーンでの壁時計の時刻と、実際の時刻（instant）を行き来する。
type zone interface {
	instant(wall time.Time) time.Time
	wall(t time.Time) time.Time
}

// wallOf は、t の年月日時分秒を、time.UTC の壁時計の時刻にする。
func wallOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

type locZone struct{ loc *time.Location }

func (z locZone) instant(w time.Time) time.Time {
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, z.loc)
}
func (z locZone) wall(t time.Time) time.Time { return wallOf(t.In(z.loc)) }

// vtzZone は、ファイルの VTIMEZONE で決めたタイムゾーン（IANA の名前でないとき）。
type vtzZone struct {
	trans []vtzTrans // 切り替わり。at の順
	first int        // 最初の切り替わりより前のずれ（秒）
}

type vtzTrans struct {
	at  time.Time // 切り替わる実際の時刻（UTC）
	off int       // 切り替わったあとの UTC からのずれ（秒）
}

func (z *vtzZone) offsetAt(t time.Time) int {
	i := sort.Search(len(z.trans), func(i int) bool { return z.trans[i].at.After(t) })
	if i == 0 {
		return z.first
	}
	return z.trans[i-1].off
}

func (z *vtzZone) instant(w time.Time) time.Time {
	// 壁時計の時刻から、ずれを 2 回あてて決める（切り替わりの前後でも、どちらかに寄る）
	t := w.Add(-time.Duration(z.offsetAt(w)) * time.Second)
	return w.Add(-time.Duration(z.offsetAt(t)) * time.Second)
}

func (z *vtzZone) wall(t time.Time) time.Time {
	return t.UTC().Add(time.Duration(z.offsetAt(t)) * time.Second)
}

func parseOffset(s string) (int, bool) {
	m := regexp.MustCompile(`^([+-])(\d{2})(\d{2})(\d{2})?$`).FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[2])
	mi, _ := strconv.Atoi(m[3])
	sec, _ := strconv.Atoi(m[4])
	v := h*3600 + mi*60 + sec
	if m[1] == "-" {
		v = -v
	}
	return v, true
}

// buildVTZ は VTIMEZONE の STANDARD と DAYLIGHT から、1970 年から 2100 年までの切り替わりを作る。
func buildVTZ(c *icsComp) (*vtzZone, error) {
	z := &vtzZone{}
	lo := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	firstAt := hi
	for _, ob := range c.Subs {
		if ob.Name != "STANDARD" && ob.Name != "DAYLIGHT" {
			continue
		}
		from, ok1 := parseOffset(propValue(ob, "TZOFFSETFROM"))
		to, ok2 := parseOffset(propValue(ob, "TZOFFSETTO"))
		ds := ob.prop("DTSTART")
		if !ok1 || !ok2 || ds == nil {
			return nil, fmt.Errorf("VTIMEZONE: %s needs TZOFFSETFROM, TZOFFSETTO and DTSTART", ob.Name)
		}
		start, _, _, err := parseICSDateTime(ds.Value)
		if err != nil {
			return nil, err
		}
		onsets := []time.Time{start}
		if rr := ob.prop("RRULE"); rr != nil {
			// Outlook は DTSTART を 1601 年にする。rrule-go は 1900 年より前から数えると途中で止まるので、
			// 1970 年に寄せる（切り替わりの日は BYMONTH と BYDAY で決まるので、変わらない）
			if start.Year() < 1970 {
				start = time.Date(1970, start.Month(), min(start.Day(), 28), start.Hour(), start.Minute(), start.Second(), 0, time.UTC)
			}
			r, err := buildRRule(rr.Value, start, func(u time.Time) time.Time { return u.Add(time.Duration(from) * time.Second) })
			if err != nil {
				return nil, fmt.Errorf("VTIMEZONE RRULE: %w", err)
			}
			onsets = rruleBetween(r, lo, hi, 5000)
		}
		for _, rd := range ob.props("RDATE") {
			for _, v := range strings.Split(rd.Value, ",") {
				if t, _, _, err := parseICSDateTime(v); err == nil {
					onsets = append(onsets, t)
				}
			}
		}
		for _, o := range onsets {
			at := o.Add(-time.Duration(from) * time.Second) // 切り替わる前のずれでの壁時計の時刻
			z.trans = append(z.trans, vtzTrans{at: at, off: to})
			if at.Before(firstAt) {
				firstAt, z.first = at, from
			}
		}
	}
	if len(z.trans) == 0 {
		return nil, errors.New("VTIMEZONE has no STANDARD or DAYLIGHT")
	}
	sort.Slice(z.trans, func(i, j int) bool { return z.trans[i].at.Before(z.trans[j].at) })
	return z, nil
}

func propValue(c *icsComp, name string) string {
	if p := c.prop(name); p != nil {
		return p.Value
	}
	return ""
}

// windowsZones は、Outlook（Exchange）が使う Windows のタイムゾーンの名前と、IANA の名前の対応（よく使うもの）。
var windowsZones = map[string]string{
	"Tokyo Standard Time": "Asia/Tokyo", "Korea Standard Time": "Asia/Seoul", "China Standard Time": "Asia/Shanghai",
	"Taipei Standard Time": "Asia/Taipei", "Singapore Standard Time": "Asia/Singapore", "India Standard Time": "Asia/Kolkata",
	"SE Asia Standard Time": "Asia/Bangkok", "AUS Eastern Standard Time": "Australia/Sydney", "New Zealand Standard Time": "Pacific/Auckland",
	"UTC": "UTC", "Coordinated Universal Time": "UTC", "GMT Standard Time": "Europe/London", "Greenwich Standard Time": "Atlantic/Reykjavik",
	"W. Europe Standard Time": "Europe/Berlin", "Central Europe Standard Time": "Europe/Budapest", "Romance Standard Time": "Europe/Paris",
	"Central European Standard Time": "Europe/Warsaw", "E. Europe Standard Time": "Europe/Chisinau", "FLE Standard Time": "Europe/Kiev",
	"GTB Standard Time": "Europe/Bucharest", "Russian Standard Time": "Europe/Moscow", "Turkey Standard Time": "Europe/Istanbul",
	"Israel Standard Time": "Asia/Jerusalem", "Arabian Standard Time": "Asia/Dubai", "Eastern Standard Time": "America/New_York",
	"Central Standard Time": "America/Chicago", "Mountain Standard Time": "America/Denver", "US Mountain Standard Time": "America/Phoenix",
	"Pacific Standard Time": "America/Los_Angeles", "Alaskan Standard Time": "America/Anchorage", "Hawaiian Standard Time": "Pacific/Honolulu",
	"Atlantic Standard Time": "America/Halifax", "E. South America Standard Time": "America/Sao_Paulo", "SA Pacific Standard Time": "America/Bogota",
	"Canada Central Standard Time": "America/Regina", "Mexico Standard Time": "America/Mexico_City", "Central America Standard Time": "America/Guatemala",
}

// ---------- 日付と時刻の値 ----------

// icsTime は DTSTART などの値。date なら終日（日付だけ）。
type icsTime struct {
	wall time.Time // 壁時計の時刻（time.UTC に入れたもの）。date なら 0 時
	date bool
	z    zone // date なら nil
}

func (t icsTime) instant() time.Time { return t.z.instant(t.wall) }

var dateRe = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})(?:T(\d{2})(\d{2})(\d{2})(Z)?)?$`)

// parseICSDateTime は 20261006、20261006T100000、20261006T010000Z を読む。
func parseICSDateTime(s string) (wall time.Time, date, utc bool, err error) {
	m := dateRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return time.Time{}, false, false, fmt.Errorf("bad date-time %q", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	if m[4] == "" {
		return time.Date(n(1), time.Month(n(2)), n(3), 0, 0, 0, 0, time.UTC), true, false, nil
	}
	return time.Date(n(1), time.Month(n(2)), n(3), n(4), n(5), n(6), 0, time.UTC), false, m[7] == "Z", nil
}

// calParser は、1 つのカレンダーを読むときの状態（タイムゾーンの解決など）。
type calParser struct {
	root     *icsComp
	floating zone // TZID のない時刻のタイムゾーン
	zones    map[string]zone
	warnings []string
	warned   map[string]bool
}

func (p *calParser) warn(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if !p.warned[s] {
		p.warned[s] = true
		p.warnings = append(p.warnings, s)
	}
}

// loadIANA は IANA の名前を読む。Mozilla の古い形（/mozilla.org/20050126_1/Europe/Berlin）も、後ろから読む。
func loadIANA(name string) *time.Location {
	name = strings.Trim(strings.TrimSpace(name), `"`)
	if name == "" || name == "Local" {
		return nil
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	parts := strings.Split(strings.Trim(name, "/"), "/")
	for i := 1; i < len(parts); i++ {
		if loc, err := time.LoadLocation(strings.Join(parts[i:], "/")); err == nil {
			return loc
		}
	}
	return nil
}

func (p *calParser) zone(tzid string) zone {
	if z, ok := p.zones[tzid]; ok {
		return z
	}
	var z zone
	if loc := loadIANA(tzid); loc != nil {
		z = locZone{loc}
	} else if iana, ok := windowsZones[strings.TrimSpace(tzid)]; ok {
		z = locZone{loadIANA(iana)}
	} else {
		for _, c := range p.root.Subs {
			if c.Name == "VTIMEZONE" && propValue(c, "TZID") == tzid {
				if v, err := buildVTZ(c); err == nil {
					z = v
				} else {
					p.warn("タイムゾーン %q を読めません（%v）", tzid, err)
				}
			}
		}
	}
	if z == nil {
		p.warn("知らないタイムゾーン %q は、PC のタイムゾーンとみなしました", tzid)
		z = locZone{time.Local}
	}
	p.zones[tzid] = z
	return z
}

// timeOf は DTSTART などのプロパティの値を読む。
func (p *calParser) timeOf(prop *icsProp) (icsTime, error) {
	v := prop.Value
	if i := strings.IndexByte(v, '/'); i >= 0 { // PERIOD（開始/終了）の開始だけを使う
		v = v[:i]
	}
	w, date, utc, err := parseICSDateTime(v)
	if err != nil {
		return icsTime{}, err
	}
	switch {
	case date || strings.EqualFold(prop.Params["VALUE"], "DATE"):
		return icsTime{wall: time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, time.UTC), date: true}, nil
	case utc:
		return icsTime{wall: w, z: locZone{time.UTC}}, nil
	case prop.Params["TZID"] != "":
		return icsTime{wall: w, z: p.zone(prop.Params["TZID"])}, nil
	}
	return icsTime{wall: w, z: p.floating}, nil
}

// timesOf は EXDATE、RDATE の値（カンマ区切り）を読む。
func (p *calParser) timesOf(prop icsProp) []icsTime {
	var out []icsTime
	for _, v := range strings.Split(prop.Value, ",") {
		q := prop
		q.Value = v
		if t, err := p.timeOf(&q); err == nil {
			out = append(out, t)
		}
	}
	return out
}

var durRe = regexp.MustCompile(`^([+-])?P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// parseICSDuration は DURATION（P1D、PT1H30M、P1W）を、日数と、それ以外の時間に分けて読む。
func parseICSDuration(s string) (days int, d time.Duration, err error) {
	m := durRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || s == "P" || strings.HasSuffix(s, "T") {
		return 0, 0, fmt.Errorf("bad duration %q", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	days = n(2)*7 + n(3)
	d = time.Duration(n(4))*time.Hour + time.Duration(n(5))*time.Minute + time.Duration(n(6))*time.Second
	if m[1] == "-" {
		days, d = -days, -d
	}
	return days, d, nil
}

// ---------- 繰り返し ----------

var untilRe = regexp.MustCompile(`(?i)(^|;)UNTIL=([0-9TZ]+)`)

// buildRRule は RRULE を、壁時計の時刻 dtstart から展開できるようにする。
// UNTIL が UTC（Z）のときは、toWall で、予定のタイムゾーンの壁時計の時刻に直す。
func buildRRule(s string, dtstart time.Time, toWall func(time.Time) time.Time) (*rrule.RRule, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "RRULE:")
	var until time.Time
	if m := untilRe.FindStringSubmatch(s); m != nil {
		w, date, utc, err := parseICSDateTime(m[2])
		if err != nil {
			return nil, err
		}
		switch {
		case date:
			until = w.Add(24*time.Hour - time.Second) // その日のうちは含む
		case utc:
			until = toWall(w)
		default:
			until = w
		}
		s = untilRe.ReplaceAllString(s, "$1")
		s = strings.Trim(strings.ReplaceAll(s, ";;", ";"), ";")
	}
	opt, err := rrule.StrToROptionInLocation(s, time.UTC)
	if err != nil {
		return nil, err
	}
	opt.Dtstart = dtstart
	if !until.IsZero() {
		opt.Until = until
	}
	return rrule.NewRRule(*opt)
}

// rruleBetween は、lo 以上 hi 以下の回を返す。数えすぎないよう、limit 回で打ち切る。
func rruleBetween(r *rrule.RRule, lo, hi time.Time, limit int) []time.Time {
	var out []time.Time
	next := r.Iterator()
	for i := 0; i < limit; i++ {
		t, ok := next()
		if !ok || t.After(hi) {
			break
		}
		if !t.Before(lo) {
			out = append(out, t)
		}
	}
	return out
}

// rruleLimit は、1 つの予定の繰り返しを数える上限（1 分ごとの繰り返しなどで止まらないように）。
const rruleLimit = 200000

// ---------- 予定 ----------

// calEvent は Brain に送る予定 1 つ（calendar.go の CalEvent と同じ形）。
type calEvent struct {
	Title    string     `json:"title"`
	Start    *time.Time `json:"start,omitempty"`
	End      *time.Time `json:"end,omitempty"`
	Day      string     `json:"day,omitempty"`
	EndDay   string     `json:"end_day,omitempty"`
	Location string     `json:"location,omitempty"`
}

func (e calEvent) sortKey() time.Time {
	if e.Start != nil {
		return *e.Start
	}
	t, _ := time.ParseInLocation("2006-01-02", e.Day, time.Local)
	return t
}

// window は、送る予定の範囲。実際の時刻 [from, to) と、日付 [fromDay, toDay)（終日の予定用）。
type window struct {
	from, to       time.Time
	fromDay, toDay time.Time // time.UTC の 0 時
}

func newWindow(now time.Time, days int) window {
	y, m, d := now.Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	to := time.Date(y, m, d+days+1, 0, 0, 0, 0, now.Location())
	return window{from: from, to: to, fromDay: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), toDay: time.Date(y, m, d+days+1, 0, 0, 0, 0, time.UTC)}
}

// cleanText は、Brain に送れる 1 行の文にする（改行は空白に、制御文字は除く、200 文字まで）。
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r) || r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return truncate200(s)
}

func truncate200(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}

type vevent struct {
	c       *icsComp
	uid     string
	start   icsTime
	recurID *icsTime
	status  string
}

// expandICS は、ICS の中の予定のうち、範囲 win に重なる回を返す。warnings は、読めなかった予定など。
func expandICS(b []byte, win window) (events []calEvent, name string, warnings []string, err error) {
	root, err := parseICS(b)
	if err != nil {
		return nil, "", nil, err
	}
	p := &calParser{root: root, zones: map[string]zone{}, warned: map[string]bool{}, floating: locZone{time.Local}}
	if loc := loadIANA(propValue(root, "X-WR-TIMEZONE")); loc != nil {
		p.floating = locZone{loc}
	}
	name = icsText(propValue(root, "X-WR-CALNAME"))
	var evs []vevent
	overridden := map[string]bool{} // UID と RECURRENCE-ID（実際の時刻か日付）
	key := func(uid string, t icsTime) string {
		if t.date {
			return uid + "\x00" + t.wall.Format("2006-01-02")
		}
		return uid + "\x00" + t.instant().UTC().Format(time.RFC3339)
	}
	for _, c := range root.Subs {
		if c.Name != "VEVENT" {
			continue
		}
		ds := c.prop("DTSTART")
		if ds == nil {
			p.warn("DTSTART のない予定を飛ばしました（%s）", truncate(icsText(propValue(c, "SUMMARY")), 20))
			continue
		}
		st, err := p.timeOf(ds)
		if err != nil {
			p.warn("予定の時刻を読めません（%v）", err)
			continue
		}
		ev := vevent{c: c, uid: propValue(c, "UID"), start: st, status: strings.ToUpper(propValue(c, "STATUS"))}
		if r := c.prop("RECURRENCE-ID"); r != nil {
			rt, err := p.timeOf(r)
			if err != nil {
				p.warn("RECURRENCE-ID を読めません（%v）", err)
				continue
			}
			ev.recurID = &rt
			overridden[key(ev.uid, rt)] = true
		}
		evs = append(evs, ev)
	}
	for _, ev := range evs {
		if ev.status == "CANCELLED" {
			continue
		}
		out, err := p.occurrences(ev, win, func(t icsTime) bool { return ev.recurID == nil && overridden[key(ev.uid, t)] })
		if err != nil {
			p.warn("予定「%s」を展開できません（%v）", truncate(icsText(propValue(ev.c, "SUMMARY")), 20), err)
			continue
		}
		events = append(events, out...)
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i].sortKey(), events[j].sortKey()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return events[i].Title < events[j].Title
	})
	return events, name, p.warnings, nil
}

// occurrences は、1 つの VEVENT の、範囲に重なる回を作る。skip が true の回（RECURRENCE-ID で置き換えた回）は出さない。
func (p *calParser) occurrences(ev vevent, win window, skip func(icsTime) bool) ([]calEvent, error) {
	c, st := ev.c, ev.start
	// 長さ：DTEND、DURATION、どちらもなければ終日は 1 日、時刻は 0
	days, dur := 0, time.Duration(0)
	if st.date {
		days = 1
	}
	if de := c.prop("DTEND"); de != nil {
		en, err := p.timeOf(de)
		if err != nil {
			return nil, err
		}
		if st.date {
			days = int(en.wall.Sub(st.wall).Hours()/24 + 0.5)
		} else if en.date {
			dur = en.wall.Sub(st.wall)
		} else {
			dur = en.instant().Sub(st.instant())
		}
	} else if du := c.prop("DURATION"); du != nil {
		d, t, err := parseICSDuration(du.Value)
		if err != nil {
			return nil, err
		}
		if st.date {
			days = max(d+int(t/(24*time.Hour)), 1)
		} else {
			days, dur = d, t
		}
	}
	if st.date {
		days = max(days, 1)
	}
	if dur < 0 {
		dur = 0
	}

	base := calEvent{Title: cleanText(icsText(propValue(c, "SUMMARY"))), Location: cleanText(icsText(propValue(c, "LOCATION")))}
	// make は、始まり t の回を作る。範囲に重ならなければ ok=false
	mk := func(t icsTime) (calEvent, bool) {
		e := base
		if t.date {
			a := t.wall
			b := a.AddDate(0, 0, days)
			if !a.Before(win.toDay) || !b.After(win.fromDay) {
				return e, false
			}
			e.Day, e.EndDay = a.Format("2006-01-02"), b.Format("2006-01-02")
			if days == 1 {
				e.EndDay = ""
			}
			return e, true
		}
		s := t.instant()
		end := s.Add(dur)
		if st.date == false && days != 0 { // DURATION の日数は、壁時計の日付で足す
			end = t.z.instant(t.wall.AddDate(0, 0, days)).Add(dur)
		}
		if !s.Before(win.to) || !(end.After(win.from) || (end.Equal(s) && !s.Before(win.from))) {
			return e, false
		}
		s, end = s.UTC(), end.UTC()
		e.Start, e.End = &s, &end
		return e, true
	}

	rr := c.prop("RRULE")
	rdates := c.props("RDATE")
	if ev.recurID != nil || (rr == nil && len(rdates) == 0) {
		if e, ok := mk(st); ok {
			return []calEvent{e}, nil
		}
		return nil, nil
	}

	// 繰り返し：壁時計の時刻で展開する。範囲より長さのぶん前から、1 日の余裕を持って探す
	z := st.z
	toWall := func(u time.Time) time.Time { return wallOf(u) }
	var lo, hi time.Time
	span := time.Duration(days)*24*time.Hour + dur + 24*time.Hour
	if st.date {
		lo, hi = win.fromDay.Add(-span), win.toDay.Add(24*time.Hour)
	} else {
		toWall = z.wall
		lo, hi = z.wall(win.from).Add(-span), z.wall(win.to).Add(24*time.Hour)
	}
	var starts []time.Time
	seen := map[time.Time]bool{}
	add := func(w time.Time) {
		if !seen[w] {
			seen[w] = true
			starts = append(starts, w)
		}
	}
	if rr != nil {
		r, err := buildRRule(rr.Value, st.wall, toWall)
		if err != nil {
			return nil, fmt.Errorf("RRULE %q: %w", truncate(rr.Value, 60), err)
		}
		for _, w := range rruleBetween(r, lo, hi, rruleLimit) {
			add(w)
		}
	}
	if !st.wall.Before(lo) && !st.wall.After(hi) {
		add(st.wall) // DTSTART は、規則に合わなくても最初の回（RFC 5545）
	}
	for _, rd := range rdates {
		for _, t := range p.timesOf(rd) {
			w := t.wall
			if !t.date && !st.date {
				w = z.wall(t.instant())
			}
			if !w.Before(lo) && !w.After(hi) {
				add(w)
			}
		}
	}
	// EXDATE：実際の時刻で比べる。終日の予定と、日付だけの EXDATE（規格外だが使われる）は日付で比べる
	ex := map[string]bool{}
	for _, xp := range c.props("EXDATE") {
		for _, t := range p.timesOf(xp) {
			if t.date || st.date {
				ex[t.wall.Format("2006-01-02")] = true
			} else {
				ex[t.instant().UTC().Format(time.RFC3339)] = true
			}
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	var out []calEvent
	for _, w := range starts {
		t := icsTime{wall: w, date: st.date, z: z}
		k := w.Format("2006-01-02")
		if !st.date && !ex[k] {
			k = t.instant().UTC().Format(time.RFC3339)
		}
		if ex[k] || skip(t) {
			continue
		}
		if e, ok := mk(t); ok {
			out = append(out, e)
		}
	}
	return out, nil
}
