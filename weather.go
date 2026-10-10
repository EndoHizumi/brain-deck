package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"
)

// ---------- 天気予報 ----------
//
// 予報は PC の brain-deck weather sync が天気予報 API（livedoor 天気互換。データの元は気象庁）から取ってきて、
// set_weather で送る（Brain はインターネットに出ない）。設定ファイルには書かず、/var/lib/lefthand/weather.json に置く。
// 予報は日付（PC の日付。API の date）ごとに持ち、ウィジェットは Brain の今日と明日の日付のものを出す。
// API は 0 時〜5 時に、前の日に発表した「明日」を「今日」として返すが、日付は今日なので、そのまま今日として出す
// （発表の日時を小さく出すので、前の日の発表だと分かる）。
// 取得に失敗したときは、前の予報を残して、失敗したことと時刻を覚える。保存は返事を待たせない。

const (
	weatherStoreName = "weather"
	weatherMaxDays   = 7
	weatherMaxText   = 40 // 天気の文（telop）の長さ
	weatherMaxArea   = 40
	weatherMaxError  = 200
	defaultWthStale  = 3 * time.Hour // 最終更新がこれより古ければ、古いと出す（widget の stale で変えられる）
)

var weatherIconRE = regexp.MustCompile(`^[0-9]{3}$`)

// WeatherDay は 1 日の予報。気温と降水確率の null は、API で取れなかったもの（「--」と出す）。
type WeatherDay struct {
	Date  string  `json:"date"`           // YYYY-MM-DD
	Telop string  `json:"telop"`          // 天気（晴れ、曇時々雨など）
	Icon  string  `json:"icon,omitempty"` // 気象庁の天気アイコンの番号（100 など）。アイコンは別に送る（weathericon.go）
	Max   *int    `json:"max"`            // 最高気温（℃）
	Min   *int    `json:"min"`            // 最低気温（℃）
	Rain  [4]*int `json:"rain"`           // 0〜6、6〜12、12〜18、18〜24 時の降水確率（%）
}

// WeatherData は weather.json の形と、get_weather の結果。受け取ったら書き換えずに差し替える。
type WeatherData struct {
	Rev        uint64       `json:"rev"`
	ReceivedAt time.Time    `json:"received_at"`           // Brain が受け取った時刻
	Area       string       `json:"area,omitempty"`        // 地点（東京都 東京 など）
	Office     string       `json:"office,omitempty"`      // 予報を発表した気象台
	PublicTime *time.Time   `json:"public_time,omitempty"` // 予報の発表の時刻
	FetchedAt  *time.Time   `json:"fetched_at,omitempty"`  // 最後に取れた時刻（PC の時刻）
	Error      string       `json:"error,omitempty"`       // 最後の取得の失敗（取れたら消す）
	ErrorAt    *time.Time   `json:"error_at,omitempty"`    // その時刻
	Source     string       `json:"source,omitempty"`
	Days       []WeatherDay `json:"days"`
}

// day は、日付 d（YYYY-MM-DD）の予報。なければ nil。
func (d *WeatherData) day(date string) *WeatherDay {
	if d == nil {
		return nil
	}
	for i := range d.Days {
		if d.Days[i].Date == date {
			return &d.Days[i]
		}
	}
	return nil
}

// failed は、最後の取得が失敗したか（取れた時刻より、失敗した時刻があと）。
func (d *WeatherData) failed() bool {
	return d != nil && d.Error != "" && d.ErrorAt != nil && (d.FetchedAt == nil || !d.ErrorAt.Before(*d.FetchedAt))
}

var errBadWeather = errors.New("bad weather")

// WeatherRequest は set_weather の引数（weather の中身）。Error があり Days が空なら、取得の失敗だけを知らせる。
type WeatherRequest struct {
	Area       string       `json:"area"`
	Office     string       `json:"office"`
	PublicTime *time.Time   `json:"public_time"`
	FetchedAt  *time.Time   `json:"fetched_at"`
	Error      string       `json:"error"`
	Days       []WeatherDay `json:"days"`
}

// validate は、受け取った予報を確かめてそろえる。
func (req *WeatherRequest) validate() error {
	var err error
	if req.Area, err = normalizeCalText(req.Area, weatherMaxArea); err != nil {
		return fmt.Errorf("%w: area: %v", errBadWeather, err)
	}
	if req.Office, err = normalizeCalText(req.Office, weatherMaxArea); err != nil {
		return fmt.Errorf("%w: office: %v", errBadWeather, err)
	}
	if req.Error, err = normalizeCalText(req.Error, weatherMaxError); err != nil {
		return fmt.Errorf("%w: error: %v", errBadWeather, err)
	}
	if len(req.Days) == 0 && req.Error == "" {
		return fmt.Errorf(`%w: "days" (the forecast) or "error" (why it could not be fetched) is required`, errBadWeather)
	}
	if len(req.Days) > weatherMaxDays {
		return fmt.Errorf("%w: %d days (at most %d)", errBadWeather, len(req.Days), weatherMaxDays)
	}
	seen := map[string]bool{}
	for i := range req.Days {
		d := &req.Days[i]
		if _, err := time.Parse(calDayLayout, d.Date); err != nil {
			return fmt.Errorf("%w: days[%d]: date %q is not YYYY-MM-DD", errBadWeather, i, d.Date)
		}
		if seen[d.Date] {
			return fmt.Errorf("%w: days[%d]: date %s appears twice", errBadWeather, i, d.Date)
		}
		seen[d.Date] = true
		if d.Telop, err = normalizeCalText(d.Telop, weatherMaxText); err != nil {
			return fmt.Errorf("%w: days[%d]: telop: %v", errBadWeather, i, err)
		}
		if d.Icon != "" && !weatherIconRE.MatchString(d.Icon) {
			return fmt.Errorf("%w: days[%d]: icon %q must be 3 digits (the number of the JMA icon, such as 100)", errBadWeather, i, d.Icon)
		}
		for _, t := range []*int{d.Max, d.Min} {
			if t != nil && (*t < -99 || *t > 99) {
				return fmt.Errorf("%w: days[%d]: temperature %d is out of range (-99..99)", errBadWeather, i, *t)
			}
		}
		for _, r := range d.Rain {
			if r != nil && (*r < 0 || *r > 100) {
				return fmt.Errorf("%w: days[%d]: chance of rain %d is out of range (0..100)", errBadWeather, i, *r)
			}
		}
	}
	sort.Slice(req.Days, func(i, j int) bool { return req.Days[i].Date < req.Days[j].Date })
	return nil
}

// WeatherService は予報を持ち、保存する。
type WeatherService struct {
	mu       sync.Mutex
	data     *WeatherData // nil なら一度も受け取っていない。差し替えるだけで、中は書き換えない
	store    *Store
	onChange func()
	dirty    chan struct{}
	saved    chan struct{} // テスト用：保存を 1 回終えるたびに送る
}

func NewWeatherService(store *Store) *WeatherService {
	ws := &WeatherService{store: store, dirty: make(chan struct{}, 1)}
	var d WeatherData
	if err := store.Load(weatherStoreName, &d); err == nil {
		ws.data = &d
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("weather: %v", err)
	}
	go ws.saver()
	return ws
}

// SetOnChange は、予報が変わったときに呼ぶ関数（画面の描き直し）を設定する。
func (ws *WeatherService) SetOnChange(f func()) {
	ws.mu.Lock()
	ws.onChange = f
	ws.mu.Unlock()
}

// Snapshot は今の予報を返す。返したものは書き換えない。nil なら一度も受け取っていない。
func (ws *WeatherService) Snapshot() *WeatherData {
	if ws == nil {
		return nil
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.data
}

// Set は予報を差し替える。Days が空で Error があれば、前の予報を残して失敗だけを覚える（kept が true）。
func (ws *WeatherService) Set(req WeatherRequest, source string, now time.Time) (*WeatherData, bool, error) {
	if err := req.validate(); err != nil {
		return nil, false, err
	}
	ws.mu.Lock()
	prev := ws.data
	var rev uint64
	if prev != nil {
		rev = prev.Rev
	}
	d := &WeatherData{Rev: rev + 1, ReceivedAt: now.UTC(), Source: source}
	kept := false
	if len(req.Days) == 0 {
		// 失敗だけを知らせてきた：前の予報（地点、発表、取れた時刻も）を残す
		if prev != nil {
			p := *prev
			p.Rev, p.ReceivedAt, p.Source = d.Rev, d.ReceivedAt, source
			d = &p
		} else {
			d.Days = []WeatherDay{}
		}
		kept = prev != nil && len(prev.Days) > 0
		t := now.UTC()
		d.Error, d.ErrorAt = req.Error, &t
	} else {
		d.Area, d.Office, d.Days = req.Area, req.Office, req.Days
		if req.PublicTime != nil {
			t := req.PublicTime.UTC()
			d.PublicTime = &t
		}
		t := now.UTC()
		if req.FetchedAt != nil {
			t = req.FetchedAt.UTC()
		}
		d.FetchedAt = &t
		if req.Error != "" { // 取れたが、一部に問題があった
			d.Error, d.ErrorAt = req.Error, &t
		}
	}
	ws.data = d
	cb := ws.onChange
	ws.mu.Unlock()
	select {
	case ws.dirty <- struct{}{}:
	default:
	}
	if cb != nil {
		cb()
	}
	return d, kept, nil
}

// saver は、変わるたびに weather.json を書く。書いているあいだの変更は、次の 1 回にまとめる。
func (ws *WeatherService) saver() {
	for range ws.dirty {
		if d := ws.Snapshot(); d != nil {
			t0 := time.Now()
			if err := ws.store.Save(weatherStoreName, d); err != nil {
				log.Printf("weather: save %s: %v", weatherStoreName, err)
			} else if el := time.Since(t0); el >= calSaveSlow {
				log.Printf("weather: saving %s took %v", weatherStoreName, el.Round(time.Millisecond))
			} else {
				vlogf("weather: saved %s in %v", weatherStoreName, el.Round(time.Millisecond))
			}
		}
		if ws.saved != nil {
			ws.saved <- struct{}{}
		}
	}
}

// weatherShown は、設定のどこかに天気のセルがあるか。
func weatherShown(cfg *Config) bool {
	for _, l := range cfg.Layers {
		if l.Touch == nil {
			continue
		}
		for _, a := range l.Touch.Cells {
			if a.Widget == widgetWeather {
				return true
			}
		}
	}
	return false
}

// weatherJSON は、予報を get_weather の結果の形にする。一度も受け取っていなければ空の予報。
func weatherJSON(d *WeatherData) map[string]any {
	if d == nil {
		return map[string]any{"rev": uint64(0), "days": []WeatherDay{}}
	}
	b, _ := json.Marshal(d)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}
