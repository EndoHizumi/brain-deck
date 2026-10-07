package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------- カレンダーの予定 ----------
//
// 予定は PC の brain-deck が ICS から取ってきて、set_calendar でまとめて送る（Brain はインターネットに出ない）。
// 繰り返しの展開とタイムゾーンの解決は PC で済ませ、Brain には「時刻の決まった予定」と「日付だけの終日の予定」を送る。
// 設定ファイルには書かず、/var/lib/lefthand/calendar.json に置く。設定 GUI で設定を保存しても消えない。
// 保存は返事を待たせない（1 つの goroutine がまとめて書く）。

const (
	calStoreName    = "calendar"
	calMaxCalendars = 16
	calMaxEvents    = 1000 // すべてのカレンダーの予定の合計
	calMaxTitle     = 200  // 予定の名前の長さ（文字数）
	calMaxName      = 32   // カレンダーの名前の長さ
	calMaxError     = 200
	calDayLayout    = "2006-01-02"
	defaultCalStale = 3 * time.Hour // 最終更新がこれより古ければ、古いと出す（widget の stale で変えられる）
)

// calPalette は、色を書かなかったカレンダーの色（順に使う）。
var calPalette = []string{"#4f9dff", "#50d880", "#ff8a50", "#c080ff", "#ffd040", "#40d0d0", "#ff6090", "#a0a8b0"}

var calColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// CalEvent は予定 1 つ。時刻の決まった予定は start と end、終日の予定は day と end_day（end_day の日は含まない）。
type CalEvent struct {
	Title    string     `json:"title"`
	Start    *time.Time `json:"start,omitempty"`
	End      *time.Time `json:"end,omitempty"`
	Day      string     `json:"day,omitempty"`
	EndDay   string     `json:"end_day,omitempty"`
	Location string     `json:"location,omitempty"`

	s, e time.Time // 始まりと終わり。終日の予定は Brain のタイムゾーンでの 0 時（prepare で決める）
}

// AllDay は、日付だけの終日の予定か。
func (ev *CalEvent) AllDay() bool { return ev.Start == nil }

// Calendar は、カレンダー 1 つの予定。
type Calendar struct {
	Name      string     `json:"name"`
	Color     string     `json:"color"`                // #rrggbb
	FetchedAt *time.Time `json:"fetched_at,omitempty"` // 予定を取ってきた時刻（PC の時刻）。nil なら一度も取れていない
	Error     string     `json:"error,omitempty"`      // 最後の取得の失敗。前に取れた予定を残している
	Events    []CalEvent `json:"events"`
}

// CalendarData は calendar.json の形と、get_calendar の結果。受け取ったら書き換えずに差し替える。
type CalendarData struct {
	Rev        uint64     `json:"rev"`
	ReceivedAt time.Time  `json:"received_at"`    // Brain が受け取った時刻
	From       string     `json:"from,omitempty"` // 送った範囲の最初の日（PC の日付）
	Days       int        `json:"days,omitempty"` // 何日先まで送ったか
	Source     string     `json:"source,omitempty"`
	Calendars  []Calendar `json:"calendars"`
}

var errBadCalendar = errors.New("bad calendar")

func parseCalDay(s string) (time.Time, error) {
	return time.ParseInLocation(calDayLayout, s, time.Local)
}

// prepare は、描くときに使う始まりと終わりを決める。終日の予定は、Brain のタイムゾーンでの日付にする。
func (d *CalendarData) prepare() error {
	for ci := range d.Calendars {
		c := &d.Calendars[ci]
		for i := range c.Events {
			ev := &c.Events[i]
			if ev.Start != nil {
				if ev.End == nil {
					return fmt.Errorf("%w: %q: start without end", errBadCalendar, ev.Title)
				}
				ev.s, ev.e = *ev.Start, *ev.End
				continue
			}
			a, err := parseCalDay(ev.Day)
			if err != nil {
				return fmt.Errorf("%w: %q: day %q is not YYYY-MM-DD", errBadCalendar, ev.Title, ev.Day)
			}
			b := a.AddDate(0, 0, 1)
			if ev.EndDay != "" {
				if b, err = parseCalDay(ev.EndDay); err != nil {
					return fmt.Errorf("%w: %q: end_day %q is not YYYY-MM-DD", errBadCalendar, ev.Title, ev.EndDay)
				}
			}
			ev.s, ev.e = a, b
		}
	}
	return nil
}

// CalendarRequest は set_calendar の引数。
type CalendarRequest struct {
	Calendars []Calendar
	From      string
	Days      int
	Source    string
}

// CalendarSetResult は、set_calendar で受け取ったカレンダーごとの結果。
type CalendarSetResult struct {
	Name      string     `json:"name"`
	Events    int        `json:"events"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	Kept      bool       `json:"kept,omitempty"` // 取得に失敗したので、前の予定を残した
}

// normalizeCalText は、1 行の文にそろえる（改行とタブは空白に、前後の空白は除き、長すぎれば切る）。
func normalizeCalText(s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: text is not valid UTF-8", errBadCalendar)
	}
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(s)
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: text contains a control character %U", errBadCalendar, r)
		}
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s, nil
}

// validate は、受け取ったカレンダーを確かめてそろえる。
func (req *CalendarRequest) validate() error {
	if len(req.Calendars) > calMaxCalendars {
		return fmt.Errorf("%w: %d calendars (at most %d)", errBadCalendar, len(req.Calendars), calMaxCalendars)
	}
	if req.Days < 0 || req.Days > 366 {
		return fmt.Errorf("%w: days must be 0..366", errBadCalendar)
	}
	if req.From != "" {
		if _, err := parseCalDay(req.From); err != nil {
			return fmt.Errorf("%w: from %q is not YYYY-MM-DD", errBadCalendar, req.From)
		}
	}
	seen := map[string]bool{}
	total := 0
	for ci := range req.Calendars {
		c := &req.Calendars[ci]
		name, err := normalizeCalText(c.Name, 1000)
		if err != nil {
			return err
		}
		if name == "" || utf8.RuneCountInString(name) > calMaxName {
			return fmt.Errorf("%w: calendar name %q must be 1-%d characters", errBadCalendar, c.Name, calMaxName)
		}
		if seen[name] {
			return fmt.Errorf("%w: calendar %q appears twice", errBadCalendar, name)
		}
		seen[name] = true
		c.Name = name
		switch {
		case c.Color == "":
			c.Color = calPalette[ci%len(calPalette)]
		case !calColorPattern.MatchString(c.Color):
			return fmt.Errorf("%w: calendar %q: color %q must be #rrggbb", errBadCalendar, name, c.Color)
		}
		c.Color = strings.ToLower(c.Color)
		if c.Error, err = normalizeCalText(c.Error, calMaxError); err != nil {
			return err
		}
		if c.Events == nil {
			c.Events = []CalEvent{}
		}
		total += len(c.Events)
		if total > calMaxEvents {
			return fmt.Errorf("%w: more than %d events in total", errBadCalendar, calMaxEvents)
		}
		for i := range c.Events {
			ev := &c.Events[i]
			if ev.Title, err = normalizeCalText(ev.Title, calMaxTitle); err != nil {
				return err
			}
			if ev.Location, err = normalizeCalText(ev.Location, calMaxTitle); err != nil {
				return err
			}
			switch {
			case ev.Start != nil:
				if ev.Day != "" || ev.EndDay != "" {
					return fmt.Errorf("%w: event %q has both start and day", errBadCalendar, ev.Title)
				}
				if ev.End == nil {
					t := *ev.Start
					ev.End = &t
				}
				if ev.End.Before(*ev.Start) {
					return fmt.Errorf("%w: event %q ends before it starts", errBadCalendar, ev.Title)
				}
				s, e := ev.Start.UTC(), ev.End.UTC()
				ev.Start, ev.End = &s, &e
			case ev.Day != "":
				a, err := parseCalDay(ev.Day)
				if err != nil {
					return fmt.Errorf("%w: event %q: day %q is not YYYY-MM-DD", errBadCalendar, ev.Title, ev.Day)
				}
				if ev.EndDay != "" {
					b, err := parseCalDay(ev.EndDay)
					if err != nil || !b.After(a) {
						return fmt.Errorf("%w: event %q: end_day %q must be a date after day", errBadCalendar, ev.Title, ev.EndDay)
					}
				}
			default:
				return fmt.Errorf("%w: event %q needs start (a time) or day (a date)", errBadCalendar, ev.Title)
			}
		}
	}
	return nil
}

// CalendarService は予定を持ち、保存する。
type CalendarService struct {
	mu       sync.Mutex
	data     *CalendarData // nil なら一度も受け取っていない。差し替えるだけで、中は書き換えない
	store    *Store
	onChange func()
	dirty    chan struct{}
	saved    chan struct{} // テスト用：保存を 1 回終えるたびに送る
}

func NewCalendarService(store *Store) *CalendarService {
	cs := &CalendarService{store: store, dirty: make(chan struct{}, 1)}
	var d CalendarData
	if err := store.Load(calStoreName, &d); err == nil {
		if err := d.prepare(); err != nil {
			log.Printf("calendar: %v (ignoring %s.json)", err, calStoreName)
		} else {
			cs.data = &d
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("calendar: %v", err)
	}
	go cs.saver()
	return cs
}

// SetOnChange は、予定が変わったときに呼ぶ関数（画面の描き直し）を設定する。
func (cs *CalendarService) SetOnChange(f func()) {
	cs.mu.Lock()
	cs.onChange = f
	cs.mu.Unlock()
}

// Snapshot は今の予定を返す。返したものは書き換えない。nil なら一度も受け取っていない。
func (cs *CalendarService) Snapshot() *CalendarData {
	if cs == nil {
		return nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.data
}

// Set は予定を差し替える。取得に失敗したカレンダー（error があり、events が空）は、前に受け取った予定を残す。
// 送られてこなかったカレンダーは消す（PC の設定から外したもの）。
func (cs *CalendarService) Set(req CalendarRequest, now time.Time) ([]CalendarSetResult, *CalendarData, error) {
	if err := req.validate(); err != nil {
		return nil, nil, err
	}
	cs.mu.Lock()
	prev := map[string]Calendar{}
	var rev uint64
	if cs.data != nil {
		rev = cs.data.Rev
		for _, c := range cs.data.Calendars {
			prev[c.Name] = c
		}
	}
	d := &CalendarData{Rev: rev + 1, ReceivedAt: now.UTC(), From: req.From, Days: req.Days, Source: req.Source,
		Calendars: make([]Calendar, 0, len(req.Calendars))}
	res := make([]CalendarSetResult, 0, len(req.Calendars))
	for _, c := range req.Calendars {
		r := CalendarSetResult{Name: c.Name, Error: c.Error}
		if p, ok := prev[c.Name]; ok && c.Error != "" && len(c.Events) == 0 {
			c.Events, c.FetchedAt, r.Kept = p.Events, p.FetchedAt, true
		}
		r.Events, r.FetchedAt = len(c.Events), c.FetchedAt
		d.Calendars = append(d.Calendars, c)
		res = append(res, r)
	}
	if err := d.prepare(); err != nil {
		cs.mu.Unlock()
		return nil, nil, err
	}
	cs.data = d
	cb := cs.onChange
	cs.mu.Unlock()
	select {
	case cs.dirty <- struct{}{}:
	default:
	}
	if cb != nil {
		cb()
	}
	return res, d, nil
}

// saver は、変わるたびに calendar.json を書く。書いているあいだの変更は、次の 1 回にまとめる。
func (cs *CalendarService) saver() {
	for range cs.dirty {
		d := cs.Snapshot()
		if d != nil {
			if err := cs.store.Save(calStoreName, d); err != nil {
				log.Printf("calendar: save %s: %v", calStoreName, err)
			}
		}
		if cs.saved != nil {
			cs.saved <- struct{}{}
		}
	}
}

// calendarShown は、設定のどこかにカレンダーのセルがあるか。
func calendarShown(cfg *Config) bool {
	for _, l := range cfg.Layers {
		if l.Touch == nil {
			continue
		}
		for _, a := range l.Touch.Cells {
			if a.Widget == widgetCal {
				return true
			}
		}
	}
	return false
}

// calendarJSON は get_calendar の結果の形。
func calendarJSON(d *CalendarData, shown bool) map[string]any {
	res := map[string]any{"shown": shown, "rev": uint64(0), "calendars": []Calendar{}}
	if d != nil {
		b, _ := json.Marshal(d)
		var m map[string]any
		json.Unmarshal(b, &m)
		m["shown"] = shown
		return m
	}
	return res
}
