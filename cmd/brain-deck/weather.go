package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------- weather ----------
//
// PC で天気予報 API（livedoor 天気互換、https://weather.tsukumijima.net/ 。データの元は気象庁）から予報を取ってきて、
// Brain に set_weather で送る。地点は、地点定義表（primary_area.xml）の一次細分区域の ID で、
// PC の ~/.config/brain-deck/weather.yaml に書く（リポジトリには入れない）。
//
// API のお願いを守る：
//   - 独自の User-Agent（brain-deck/<版>）を付ける
//   - 続けてアクセスするときは 0.5 秒以上あける（前のアクセスの時刻を、PC のキャッシュの場所に残す）
//   - 予報の発表は 1 日に数回なので、取りに行くのは 1 時間に 1 回程度（contrib/systemd のタイマーの例）
//
// 天気アイコンは、API が返す気象庁の SVG の URL から取ってきて、PC のキャッシュ（~/.cache/brain-deck/weather-icons）に残す。
// Brain が set_weather の返事で知らせる「まだないアイコン」（番号と箱の大きさ）だけを、透明度付きの RGB565 にして送る。
// API は正確さや継続が保証されていないので、取れなかったときは Brain に失敗だけを知らせ（Brain は前の予報を残す）、
// 終了コード 7 で終わる。

const (
	defaultWeatherAPI   = "https://weather.tsukumijima.net/api/forecast/city/"
	weatherAPIGap       = 600 * time.Millisecond // 続けてアクセスするときの間（API のお願いは 0.5 秒以上）
	iconFetchGap        = 600 * time.Millisecond // 気象庁のアイコンを続けて取るときの間
	maxWeatherBytes     = 1 << 20
	maxIconSVGBytes     = 1 << 20
	weatherIconUpload   = "weather_icon"
	weatherIconMaxSide  = 480 // Brain の上限（weathericon.go の iconMaxSide）
	weatherCityPattern  = `^[0-9]{6}$`
	weatherIconHostPath = "https://www.jma.go.jp/bosai/forecast/img/"
)

var weatherCityRE = regexp.MustCompile(weatherCityPattern)

// weatherAPIBase は、API の URL の前半（テストで差し替える。環境変数 BRAIN_DECK_WEATHER_API でも変えられる）。
var weatherAPIBase = func() string {
	if v := os.Getenv("BRAIN_DECK_WEATHER_API"); v != "" {
		return v
	}
	return defaultWeatherAPI
}

// weatherFile は weather.yaml の形。
type weatherFile struct {
	City string `yaml:"city"`
}

// weatherPath は weather.yaml の場所（calendars.yaml と同じディレクトリ）。
func weatherPath() string {
	return filepath.Join(filepath.Dir(calendarsPath()), "weather.yaml")
}

const weatherExample = `# ~/.config/brain-deck/weather.yaml の例
# city は、地点定義表 https://weather.tsukumijima.net/primary_area.xml の、一次細分区域（city タグ）の id（6 桁）
city: "<一次細分区域の ID>"`

// loadWeatherCity は、--city か weather.yaml から地点の ID を読む。
func loadWeatherCity(o *options) (string, error) {
	if o.city != "" {
		if !weatherCityRE.MatchString(o.city) {
			return "", usageError("--city は、地点定義表の一次細分区域の id（6 桁の数字）です")
		}
		return o.city, nil
	}
	path := o.config
	if path == "" {
		path = weatherPath()
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", usageError(fmt.Sprintf("天気の設定 %s がありません。--city <ID> で地点を指定するか、次のように書いてください：\n%s", path, weatherExample))
	}
	if err != nil {
		return "", err
	}
	var f weatherFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return "", usageError(fmt.Sprintf("%s：%v", path, err))
	}
	f.City = strings.TrimSpace(f.City)
	if !weatherCityRE.MatchString(f.City) {
		// ID そのものは出さない（地点が分かるので）
		return "", usageError(fmt.Sprintf("%s の city は、地点定義表の一次細分区域の id（6 桁の数字）にします：\n%s", path, weatherExample))
	}
	return f.City, nil
}

// cacheDir は、PC の brain-deck のキャッシュの場所（~/.cache/brain-deck、macOS は ~/Library/Caches/brain-deck）。
func cacheDir() string {
	if d := os.Getenv("BRAIN_DECK_CACHE"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "brain-deck")
}

// politeWait は、同じ相手（name）に、前のアクセスから gap たつまで待つ。前のアクセスの時刻はキャッシュの場所のファイルに残す
// （brain-deck を続けて実行したときも、間をあける）。
func politeWait(name string, gap time.Duration) {
	p := filepath.Join(cacheDir(), name+".last")
	if st, err := os.Stat(p); err == nil {
		if wait := gap - time.Since(st.ModTime()); wait > 0 && wait <= gap {
			time.Sleep(wait)
		}
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		os.WriteFile(p, nil, 0o644)
	}
}

// httpGet は、User-Agent を付けて url を取ってくる。エラーに URL は含めない（地点の ID が入るので）。
func httpGet(ctx context.Context, u, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &fetchError{"URL の形が違います"}
	}
	req.Header.Set("User-Agent", "brain-deck/"+versionString()+" (+https://github.com/endohizumi/brain-deck)")
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &fetchError{fmt.Sprintf("取得の途中で切れました（%v）", err)}
	}
	if int64(len(b)) > limit {
		return nil, &fetchError{"大きすぎます"}
	}
	return b, nil
}

// ---------- API の返事 ----------

// apiForecast は、API の返事のうち使うところ。気温と降水確率は文字列か null。
type apiForecast struct {
	PublicTime       string `json:"publicTime"`
	PublishingOffice string `json:"publishingOffice"`
	Title            string `json:"title"`
	Location         struct {
		Prefecture string `json:"prefecture"`
		City       string `json:"city"`
	} `json:"location"`
	Forecasts []struct {
		Date        string `json:"date"`
		DateLabel   string `json:"dateLabel"`
		Telop       string `json:"telop"`
		Temperature struct {
			Max struct {
				Celsius *string `json:"celsius"`
			} `json:"max"`
			Min struct {
				Celsius *string `json:"celsius"`
			} `json:"min"`
		} `json:"temperature"`
		ChanceOfRain map[string]*string `json:"chanceOfRain"`
		Image        struct {
			URL string `json:"url"`
		} `json:"image"`
	} `json:"forecasts"`
}

// weatherDay は、Brain に送る 1 日の予報（weather.go の WeatherDay と同じ形）。
type weatherDay struct {
	Date    string  `json:"date"`
	Telop   string  `json:"telop"`
	Icon    string  `json:"icon,omitempty"`
	Max     *int    `json:"max"`
	Min     *int    `json:"min"`
	Rain    [4]*int `json:"rain"`
	iconURL string
}

// weatherOut は、set_weather で送る予報。
type weatherOut struct {
	Area       string       `json:"area,omitempty"`
	Office     string       `json:"office,omitempty"`
	PublicTime *time.Time   `json:"public_time,omitempty"`
	FetchedAt  *time.Time   `json:"fetched_at,omitempty"`
	Error      string       `json:"error,omitempty"`
	Days       []weatherDay `json:"days"`
}

var iconURLRE = regexp.MustCompile(`/([0-9]{3})\.svg$`)

// parseNumberPtr は、"26" や "10%" を数にする。null、"--"、"--%" は nil。
func parseNumberPtr(s *string) (*int, error) {
	if s == nil {
		return nil, nil
	}
	v := strings.TrimSuffix(strings.TrimSpace(*s), "%")
	if v == "" || strings.Trim(v, "-") == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, fmt.Errorf("数ではありません（%q）", *s)
	}
	n := int(math.Round(f))
	return &n, nil
}

// parseForecast は、API の返事を Brain に送る形にする。
func parseForecast(b []byte, fetched time.Time) (*weatherOut, error) {
	var a apiForecast
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("予報の形が違います（%v）", err)
	}
	if len(a.Forecasts) == 0 {
		return nil, errors.New("予報が空です")
	}
	out := &weatherOut{Office: a.PublishingOffice}
	out.Area = strings.TrimSpace(a.Location.Prefecture + " " + a.Location.City)
	if out.Area == "" {
		out.Area = strings.TrimSuffix(strings.TrimSpace(a.Title), " の天気")
	}
	if t, err := time.Parse(time.RFC3339, a.PublicTime); err == nil {
		out.PublicTime = &t
	}
	f := fetched.UTC()
	out.FetchedAt = &f
	for i, fc := range a.Forecasts {
		if _, err := time.Parse("2006-01-02", fc.Date); err != nil {
			return nil, fmt.Errorf("%d 日目の日付 %q が読めません", i+1, fc.Date)
		}
		d := weatherDay{Date: fc.Date, Telop: strings.TrimSpace(fc.Telop), iconURL: fc.Image.URL}
		if m := iconURLRE.FindStringSubmatch(fc.Image.URL); m != nil {
			d.Icon = m[1]
		}
		var err error
		if d.Max, err = parseNumberPtr(fc.Temperature.Max.Celsius); err != nil {
			return nil, fmt.Errorf("%s の最高気温：%v", fc.Date, err)
		}
		if d.Min, err = parseNumberPtr(fc.Temperature.Min.Celsius); err != nil {
			return nil, fmt.Errorf("%s の最低気温：%v", fc.Date, err)
		}
		for k, key := range []string{"T00_06", "T06_12", "T12_18", "T18_24"} {
			if d.Rain[k], err = parseNumberPtr(fc.ChanceOfRain[key]); err != nil {
				return nil, fmt.Errorf("%s の降水確率：%v", fc.Date, err)
			}
		}
		out.Days = append(out.Days, d)
	}
	return out, nil
}

// fetchForecast は、地点 city の予報を取ってくる。
func fetchForecast(ctx context.Context, city string) (*weatherOut, error) {
	politeWait("weather-api", weatherAPIGap)
	b, err := httpGet(ctx, weatherAPIBase()+city, "application/json", maxWeatherBytes)
	if err != nil {
		return nil, err
	}
	w, err := parseForecast(b, time.Now())
	if err != nil {
		return nil, &fetchError{err.Error()}
	}
	return w, nil
}

// ---------- アイコン ----------

// iconSVG は、天気の番号 code の SVG を、キャッシュか URL から読む。取ってきたらキャッシュに残す。
func iconSVG(ctx context.Context, code, u string) ([]byte, bool, error) {
	p := filepath.Join(cacheDir(), "weather-icons", code+".svg")
	if b, err := os.ReadFile(p); err == nil {
		if _, _, err := SVGAspect(b); err == nil {
			return b, false, nil
		}
	}
	if u == "" {
		u = weatherIconHostPath + code + ".svg"
	}
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || !iconURLRE.MatchString(pu.Path) {
		return nil, false, &fetchError{"アイコンの URL の形が違います"}
	}
	politeWait("weather-icons", iconFetchGap)
	b, err := httpGet(ctx, u, "image/svg+xml", maxIconSVGBytes)
	if err != nil {
		return nil, false, err
	}
	if _, _, err := SVGAspect(b); err != nil {
		return nil, false, &fetchError{"アイコンが SVG として読めません"}
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		os.Rename(tmp, p)
	}
	return b, true, nil
}

// fitBox は、w×h の絵を、縦横比を保って box に収めた大きさ。
func fitBox(w, h float64, box image.Point) image.Point {
	s := math.Min(float64(box.X)/w, float64(box.Y)/h)
	iw := max(1, min(box.X, int(math.Round(w*s))))
	ih := max(1, min(box.Y, int(math.Round(h*s))))
	return image.Pt(iw, ih)
}

// encodeIconFile は、透明度付きの画像を Brain のアイコンのファイル（LHA1）にする。
// 色は Floyd–Steinberg のディザリングで RGB565 にする（背景画像と同じ方式。透明な画素には誤差を配らない）。
func encodeIconFile(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	rgb := make([]float64, w*h*3)
	alpha := make([]byte, w*h)
	for i := 0; i < w*h; i++ {
		rgb[i*3], rgb[i*3+1], rgb[i*3+2] = float64(img.Pix[i*4]), float64(img.Pix[i*4+1]), float64(img.Pix[i*4+2])
		alpha[i] = img.Pix[i*4+3]
	}
	pix := ditherRGB565(rgb, w, h, func(i int) bool { return alpha[i] == 0 })
	b := make([]byte, 8, 8+len(pix)+len(alpha))
	copy(b, "LHA1")
	binary.LittleEndian.PutUint16(b[4:], uint16(w))
	binary.LittleEndian.PutUint16(b[6:], uint16(h))
	b = append(b, pix...)
	return append(b, alpha...)
}

// ditherRGB565 は、色（0〜255 の小数、r, g, b の並び）を Floyd–Steinberg のディザリングで RGB565 にする
// （tools/mkbg と、設定 GUI の image.ts の既定と同じ方式）。skip が true の画素は 0 にし、誤差を配らない。
func ditherRGB565(buf []float64, w, h int, skip func(i int) bool) []byte {
	bits := [3]int{5, 6, 5}
	out := make([]byte, w*h*2)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := y*w + x
			if skip != nil && skip(p) {
				continue
			}
			var q [3]int
			for c := 0; c < 3; c++ {
				i := p*3 + c
				m := float64(int(1)<<bits[c] - 1)
				v := math.Max(0, math.Min(255, buf[i]))
				q[c] = int(math.Round(v * m / 255))
				back := float64(q[c] * 255 / int(m)) // Brain の表示と同じ戻し方（fb.go の unpack）
				e := v - back
				add := func(dx, dy int, k float64) {
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= w || ny >= h || (skip != nil && skip(ny*w+nx)) {
						return
					}
					buf[(ny*w+nx)*3+c] += e * k
				}
				add(1, 0, 7.0/16)
				add(-1, 1, 3.0/16)
				add(0, 1, 5.0/16)
				add(1, 1, 1.0/16)
			}
			binary.LittleEndian.PutUint16(out[p*2:], uint16(q[0]<<11|q[1]<<5|q[2]))
		}
	}
	return out
}

// makeIcon は、SVG を box に収まる大きさに描いて、アイコンのファイルにする。
func makeIcon(svg []byte, box image.Point) ([]byte, image.Point, error) {
	if box.X < 1 || box.Y < 1 || box.X > weatherIconMaxSide || box.Y > weatherIconMaxSide {
		return nil, image.Point{}, fmt.Errorf("アイコンの箱 %dx%d が範囲の外です", box.X, box.Y)
	}
	w, h, err := SVGAspect(svg)
	if err != nil {
		return nil, image.Point{}, err
	}
	sz := fitBox(w, h, box)
	img, err := RenderSVG(svg, sz.X, sz.Y)
	if err != nil {
		return nil, image.Point{}, err
	}
	return encodeIconFile(img), sz, nil
}

// uploadFile は、kind のファイルを upload_* で送り、upload_end の結果を返す。
func uploadFile(c *Client, kind, name string, data []byte) (json.RawMessage, error) {
	return uploadFileWith(c, kind, name, data, nil)
}

// uploadFileWith は、upload_begin に extra の項目（photo の of など）を足して送る。
func uploadFileWith(c *Client, kind, name string, data []byte, extra map[string]any) (json.RawMessage, error) {
	sum := sha256.Sum256(data)
	begin := map[string]any{"kind": kind, "sha256": hex.EncodeToString(sum[:]), "bytes": len(data), "name": name, "source": "brain-deck"}
	for k, v := range extra {
		begin[k] = v
	}
	raw, err := c.Call("upload_begin", begin, callTimeout())
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
	for off := 0; off < len(data); off += r.Chunk {
		end := min(off+r.Chunk, len(data))
		if _, err := c.Call("upload_chunk", map[string]any{"upload": r.Upload, "offset": off,
			"data": base64.StdEncoding.EncodeToString(data[off:end])}, callTimeout()); err != nil {
			c.Call("upload_abort", map[string]any{"upload": r.Upload}, time.Second)
			return nil, err
		}
	}
	return c.Call("upload_end", map[string]any{"upload": r.Upload}, callTimeout())
}

// missingIcon は、set_weather の返事の icons_missing の 1 つ。
type missingIcon struct {
	Name string `json:"name"`
	Icon string `json:"icon"`
	W    int    `json:"w"`
	H    int    `json:"h"`
}

// ---------- コマンド ----------

const weatherUsage = "weather のあとには sync か list を書きます（brain-deck --help）"

// runWeather は brain-deck weather を実行する。sync は、ポートを開く前に予報とアイコンの SVG を取ってくる。
func runWeather(o *options, stdout, stderr io.Writer) int {
	a := o.args[1:]
	sub := "list"
	if len(a) > 0 {
		sub, a = a[0], a[1:]
	}
	if len(a) > 0 {
		return report(stderr, usageError("weather "+sub+" は引数を取りません（地点は --city <ID> か weather.yaml）"))
	}
	if sub != "sync" && (o.city != "" || o.config != "" || o.dryRun) {
		return report(stderr, usageError("--city、--config、--dry-run は weather sync で使います"))
	}
	switch sub {
	case "list", "ls":
		return connectAndRun(o, stdout, stderr, func(c *Client) (string, error) { return listWeather(c, o.json) })
	case "sync":
	default:
		return report(stderr, usageError(weatherUsage))
	}
	city, err := loadWeatherCity(o)
	if err != nil {
		return report(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.fetchTimeout)
	defer cancel()
	t0 := time.Now()
	w, ferr := fetchForecast(ctx, city)
	if ferr != nil {
		fmt.Fprintf(stderr, "brain-deck: 予報を取得できませんでした：%v\n", ferr)
		if o.dryRun {
			return exitFetch
		}
		// Brain には失敗だけを知らせる（前の予報を残し、「失敗」と出す）
		code := connectAndRun(o, stdout, stderr, func(c *Client) (string, error) {
			if err := need(c, "set_weather"); err != nil {
				return "", err
			}
			raw, err := c.Call("set_weather", map[string]any{"weather": weatherOut{Error: ferr.Error()}, "source": "brain-deck"}, callTimeout())
			if err != nil {
				return "", err
			}
			var r struct {
				Kept bool `json:"kept"`
			}
			json.Unmarshal(raw, &r)
			if r.Kept {
				return "Brain には、取得に失敗したことを知らせました（前の予報を残し、「失敗」と出します）", nil
			}
			return "Brain には、取得に失敗したことを知らせました（Brain にはまだ予報がありません）", nil
		})
		if code == exitOK {
			return exitFetch
		}
		return code
	}
	if o.verbose {
		fmt.Fprintf(stderr, "brain-deck: 予報を取得しました（%d 日分、%v）\n", len(w.Days), time.Since(t0).Round(time.Millisecond))
	}
	// アイコンの SVG を先に（ポートを開く前に）そろえる。多くはキャッシュにある
	svgs := map[string][]byte{}
	var iconErrs []string
	for _, d := range w.Days {
		if d.Icon == "" || svgs[d.Icon] != nil {
			continue
		}
		b, fetched, err := iconSVG(ctx, d.Icon, d.iconURL)
		if err != nil {
			iconErrs = append(iconErrs, fmt.Sprintf("%s：%v", d.Icon, err))
			continue
		}
		if fetched && o.verbose {
			fmt.Fprintf(stderr, "brain-deck: アイコン %s.svg を取得しました（%d バイト）\n", d.Icon, len(b))
		}
		svgs[d.Icon] = b
	}
	for _, e := range iconErrs {
		fmt.Fprintln(stderr, "brain-deck: 注意：アイコンを取得できませんでした（"+e+"）。文字だけで出します")
	}
	if o.dryRun {
		if !o.quiet {
			fmt.Fprintln(stdout, formatWeather(w, time.Now()))
			fmt.Fprintln(stdout, "（Brain には送っていません）")
		}
		return exitOK
	}
	return connectAndRun(o, stdout, stderr, func(c *Client) (string, error) {
		if err := need(c, "set_weather"); err != nil {
			return "", err
		}
		var msgs []string
		if !o.noTimeSync { // 「今日」と最終更新が Brain でずれないように
			m, err := setTime(c)
			if err != nil {
				return "", err
			}
			msgs = append(msgs, m)
		}
		raw, err := c.Call("set_weather", map[string]any{"weather": w, "source": "brain-deck"}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Shown   bool          `json:"shown"`
			Missing []missingIcon `json:"icons_missing"`
		}
		json.Unmarshal(raw, &r)
		if !r.Shown {
			fmt.Fprintln(stderr, "brain-deck: 注意：今の設定には、天気のセル（widget: weather）がありません。予報は Brain に保存しました")
		}
		sent := 0
		for _, m := range r.Missing {
			svg := svgs[m.Icon]
			if svg == nil {
				continue // 取得できなかったアイコン
			}
			data, sz, err := makeIcon(svg, image.Pt(m.W, m.H))
			if err != nil {
				fmt.Fprintf(stderr, "brain-deck: 注意：アイコン %s を描けませんでした（%v）\n", m.Name, err)
				continue
			}
			if _, err := uploadFile(c, weatherIconUpload, m.Name, data); err != nil {
				var be *brainError
				if errors.As(err, &be) {
					fmt.Fprintf(stderr, "brain-deck: 注意：アイコン %s を Brain が受け取りませんでした（%s）\n", m.Name, be.Message)
					continue
				}
				return "", err
			}
			sent++
			if o.verbose {
				fmt.Fprintf(stderr, "brain-deck: アイコン %s（%dx%d、%d バイト）を送りました\n", m.Name, sz.X, sz.Y, len(data))
			}
		}
		line := fmt.Sprintf("%s の予報を送りました（%s）", w.Area, summarizeWeather(w))
		if sent > 0 {
			line += fmt.Sprintf("。アイコン %d 個を送りました", sent)
		}
		return line + "\n" + strings.Join(msgs, "\n"), nil
	})
}

// summarizeWeather は、今日と明日の天気を短く書く。
func summarizeWeather(w *weatherOut) string {
	var parts []string
	for i, d := range w.Days {
		if i >= 2 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s", d.Date[5:], d.Telop))
	}
	return strings.Join(parts, "、")
}

// formatWeather は、予報を表にして書く（--dry-run と list）。
func formatWeather(w *weatherOut, now time.Time) string {
	var b strings.Builder
	if w.Area != "" {
		b.WriteString(w.Area)
	}
	if w.PublicTime != nil {
		fmt.Fprintf(&b, "（%s %s発表）", w.PublicTime.Local().Format("01/02 15:04"), w.Office)
	}
	b.WriteString("\n")
	num := func(p *int, unit string) string {
		if p == nil {
			return "--"
		}
		return strconv.Itoa(*p) + unit
	}
	for _, d := range w.Days {
		t, _ := time.ParseInLocation("2006-01-02", d.Date, time.Local)
		icon := d.Icon
		if icon == "" {
			icon = "---"
		}
		fmt.Fprintf(&b, "%s(%s) %-3s %s  最高 %s 最低 %s  降水 %s/%s/%s/%s\n", t.Format("01/02"), jaWday[t.Weekday()], icon, d.Telop,
			num(d.Max, "℃"), num(d.Min, "℃"), num(d.Rain[0], "%"), num(d.Rain[1], "%"), num(d.Rain[2], "%"), num(d.Rain[3], "%"))
	}
	if w.FetchedAt != nil {
		fmt.Fprintf(&b, "取得 %s", w.FetchedAt.Local().Format("01/02 15:04"))
	}
	if w.Error != "" {
		fmt.Fprintf(&b, "（最後の取得に失敗：%s）", w.Error)
	}
	return strings.TrimRight(b.String(), "\n")
}

// listWeather は、Brain にある予報（get_weather）を出す。
func listWeather(c *Client, asJSON bool) (string, error) {
	if err := need(c, "get_weather"); err != nil {
		return "", err
	}
	raw, err := c.Call("get_weather", nil, callTimeout())
	if err != nil {
		return "", err
	}
	if asJSON {
		return string(raw), nil
	}
	var r struct {
		weatherOut
		ErrorAt *time.Time `json:"error_at"`
		Shown   bool       `json:"shown"`
		Icons   []struct {
			Name string `json:"name"`
		} `json:"icons"`
		IconsBytes int64         `json:"icons_bytes"`
		Missing    []missingIcon `json:"icons_missing"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if len(r.Days) == 0 && r.Error == "" {
		return "Brain に予報はありません（brain-deck weather sync で送ります）", nil
	}
	s := formatWeather(&r.weatherOut, time.Now())
	s += fmt.Sprintf("\nアイコン：%d 個（%.1f KiB）", len(r.Icons), float64(r.IconsBytes)/1024)
	if len(r.Missing) > 0 {
		s += fmt.Sprintf("、まだないもの %d 個（brain-deck weather sync で送ります）", len(r.Missing))
	}
	if !r.Shown {
		s += "\n今の設定には、天気のセル（widget: weather）がありません"
	}
	return s, nil
}
