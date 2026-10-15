package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 上書きの値（/var/lib/lefthand/overrides.json） ----------
//
// Brain の設定のアプリ（settingswidget.go）で変えた値は、設定ファイル（config.yaml）を書き換えずに、ここに置く。
// コメント付きの設定ファイルを壊さないため。デーモンは、設定ファイルを読んだあとに上書きを重ねて使う（設定 GUI と
// brain-deck config put で設定ファイルを変えても、上書きは残る）。上書きを消せば、設定ファイルの値に戻る。
//
// 上書きできるのは、Brain だけでよく変える値に限る（overrideKeys）。どれも設定ファイルの値の代わりに使い、
// 上書きを重ねた設定が正しくないとき（ほかの項目と合わないとき）は、その上書きを使わずに、そのことを info に書く。
//
// ファイルの形：{"format":1,"values":{"backlight.level":5,"lid.close":"none"},"updated":"…","source":"brain"}

const (
	overridesName   = "overrides.json"
	overridesFormat = 1
)

var (
	errOverrideKey   = errors.New("unknown override")
	errOverrideValue = errors.New("bad override value")
)

// overrideKey は、上書きできる項目 1 つ。
type overrideKey struct {
	Key   string
	Label string // 画面に出す名前
	// parse は、値を確かめて、ファイルに書く形（JSON）にそろえる
	parse func(raw json.RawMessage) (any, error)
	// apply は、cfg（写し）に値を重ねる。file は設定ファイルの値（暗くなるまでの時間を延ばすときの、元の間隔に使う）
	apply func(cfg, file *Config, v any)
	// fileValue は、設定ファイルの値を、画面に出す形で返す
	fileValue func(file *Config) string
	// show は、上書きの値を、画面に出す形で返す
	show func(v any) string
}

// overrideKeys は、上書きできる項目（並べる順）。
var overrideKeys = []*overrideKey{
	{Key: "backlight.level", Label: "明るさ",
		parse: func(raw json.RawMessage) (any, error) {
			var n int
			if err := json.Unmarshal(raw, &n); err != nil || n < 1 || n > 7 {
				return nil, fmt.Errorf("%w: backlight.level must be 1..7", errOverrideValue)
			}
			return n, nil
		},
		apply: func(c, _ *Config, v any) {
			n := v.(int)
			c.Backlight.Level = &n
			if o := c.Backlight.Offline; o != nil && o.Level != nil {
				o.Level = &n // 外しているときの明るさを書いていれば、それも同じにする
			}
		},
		fileValue: func(f *Config) string {
			if f.Backlight == nil || f.Backlight.Level == nil {
				return "起動したときの明るさ"
			}
			return fmt.Sprint(*f.Backlight.Level)
		},
		show: func(v any) string { return fmt.Sprint(v) }},
	{Key: "backlight.dim_after", Label: "暗くなるまで（PC あり）",
		parse: parseDimOverride("backlight.dim_after"),
		apply: func(c, f *Config, v any) {
			b, fb := c.Backlight, f.Backlight
			if fb == nil {
				fb = &BacklightConfig{}
			}
			setDimAfter(&b.DimAfter, &b.OffAfter, v.(string), fb.DimAfter, fb.OffAfter)
			if v.(string) == durationOff {
				off := false
				if b.Photo != nil && *b.Photo {
					b.Photo = &off // 暗くしないなら、写真も出さない（写真は暗くする段階の代わりに出す）
				}
				if b.Offline != nil && b.Offline.DimAfter == "" && b.Offline.Photo == nil {
					b.Offline.Photo = &off
				}
			}
		},
		fileValue: func(f *Config) string {
			if f.Backlight == nil {
				return durLabel("")
			}
			return durLabel(f.Backlight.DimAfter)
		},
		show: func(v any) string { return durLabel(v.(string)) }},
	{Key: "backlight.offline.dim_after", Label: "暗くなるまで（PC なし）",
		parse: parseDimOverride("backlight.offline.dim_after"),
		apply: func(c, f *Config, v any) {
			b := c.Backlight
			if b.Offline == nil {
				b.Offline = &BacklightOffline{}
			} else {
				o := *b.Offline
				b.Offline = &o
			}
			o := b.Offline
			// 元の間隔は、設定ファイルの、外しているときの値（書いていなければ、つないでいるときの値）
			fd, fo := "", ""
			if fb := f.Backlight; fb != nil {
				fd, fo = fb.DimAfter, fb.OffAfter
				if fb.Offline != nil {
					fd, fo = or(fb.Offline.DimAfter, fd), or(fb.Offline.OffAfter, fo)
				}
			}
			off := or(o.OffAfter, b.OffAfter)
			setDimAfter(&o.DimAfter, &off, v.(string), fd, fo)
			if off != or(o.OffAfter, b.OffAfter) {
				o.OffAfter = off
			}
			if v.(string) == durationOff {
				no := false
				if (o.Photo != nil && *o.Photo) || (o.Photo == nil && b.Photo != nil && *b.Photo) {
					o.Photo = &no
				}
			}
		},
		fileValue: func(f *Config) string {
			if f.Backlight == nil {
				return durLabel("")
			}
			if f.Backlight.Offline != nil && f.Backlight.Offline.DimAfter != "" {
				return durLabel(f.Backlight.Offline.DimAfter)
			}
			return durLabel(f.Backlight.DimAfter)
		},
		show: func(v any) string { return durLabel(v.(string)) }},
	{Key: "backlight.photo", Label: "フォトフレーム",
		parse: func(raw json.RawMessage) (any, error) {
			var b bool
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, fmt.Errorf("%w: backlight.photo must be true or false", errOverrideValue)
			}
			return b, nil
		},
		apply: func(c, _ *Config, v any) {
			on := v.(bool)
			c.Backlight.Photo = &on
			if o := c.Backlight.Offline; o != nil && o.Photo != nil {
				o.Photo = &on // PC から外しているときも同じにする
			}
		},
		fileValue: func(f *Config) string {
			return onOff(f.Backlight != nil && f.Backlight.Photo != nil && *f.Backlight.Photo)
		},
		show: func(v any) string { return onOff(v.(bool)) }},
	{Key: "ime.method", Label: "日本語入力",
		parse: func(raw json.RawMessage) (any, error) {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil || (s != imeAuto && s != imeOff && !imeMethodName.MatchString(s)) {
				return nil, fmt.Errorf("%w: ime.method must be auto, off, or a uim input method name", errOverrideValue)
			}
			return s, nil
		},
		apply: func(c, _ *Config, v any) {
			ic := IMEConfig{}
			if c.IME != nil {
				ic = *c.IME
			}
			ic.Method = v.(string)
			c.IME = &ic
		},
		fileValue: func(f *Config) string { return imeMethodLabel(f.IME.method()) },
		show:      func(v any) string { return imeMethodLabel(v.(string)) }},
	{Key: "lid.close", Label: "ふたを閉じたとき",
		parse: func(raw json.RawMessage) (any, error) {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil || (s != lidOff && s != lidNone) {
				return nil, fmt.Errorf("%w: lid.close must be off or none", errOverrideValue)
			}
			return s, nil
		},
		apply: func(c, _ *Config, v any) {
			lc := LidConfig{}
			if c.Lid != nil {
				lc = *c.Lid
			}
			lc.Close = v.(string)
			c.Lid = &lc
		},
		fileValue: func(f *Config) string { return lidLabel(f.Lid.action()) },
		show:      func(v any) string { return lidLabel(v.(string)) }},
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func onOff(b bool) string {
	if b {
		return "使う"
	}
	return "使わない"
}

func overrideKeyOf(key string) *overrideKey {
	for _, k := range overrideKeys {
		if k.Key == key {
			return k
		}
	}
	return nil
}

// parseDimOverride は、暗くなるまでの時間（5s〜24h か off）を確かめる。
func parseDimOverride(name string) func(json.RawMessage) (any, error) {
	return func(raw json.RawMessage) (any, error) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%w: %s must be a duration such as \"3m\", or \"off\"", errOverrideValue, name)
		}
		if s == durationOff {
			return s, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < minDimAfter || d > maxDimAfter {
			return nil, fmt.Errorf("%w: %s must be a duration from %v to %v, or off", errOverrideValue, name, minDimAfter, maxDimAfter)
		}
		return fmtDur(d), nil
	}
}

// setDimAfter は、暗くなるまでの時間を v にする。消えるまでの時間が、それより短くなるときは、
// 設定ファイルの「暗くなってから消えるまで」の間隔（書いていなければ、消えるまでの時間）だけ、暗くなったあとに延ばす。
func setDimAfter(dim, off *string, v, fileDim, fileOff string) {
	*dim = v
	if v == durationOff {
		return
	}
	d, _ := time.ParseDuration(v)
	o, err := time.ParseDuration(*off)
	if err != nil || o <= 0 || o > d {
		return
	}
	gap := time.Duration(0)
	fd, e1 := time.ParseDuration(fileDim)
	fo, e2 := time.ParseDuration(fileOff)
	switch {
	case e2 != nil:
		gap = 10 * time.Minute
	case e1 == nil && fo > fd:
		gap = fo - fd
	default:
		gap = fo
	}
	*off = fmtDur(min(d+gap, maxDimAfter))
	if *off == v { // 24 時間で暗くするときは、消さない
		*off = durationOff
	}
}

// fmtDur は、時間を短く書く（3m、1h30m、45s）。
func fmtDur(d time.Duration) string {
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	if s == "" {
		return "0s"
	}
	return s
}

// durLabel は、時間（設定の書き方）を日本語にする（3m → 3分、off か空 → しない）。
func durLabel(s string) string {
	if s == "" || s == durationOff {
		return "しない"
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%d時間", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%d分", m)
	}
	if sec > 0 {
		fmt.Fprintf(&b, "%d秒", sec)
	}
	return b.String()
}

// OverrideInfo は、上書き 1 つの様子（get_config、get_overrides、設定のアプリ）。
type OverrideInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value any    `json:"value"`          // 上書きの値（ファイルの形）
	Shown string `json:"shown"`          // 上書きの値（画面に出す形）
	File  string `json:"file"`           // 設定ファイルの値（画面に出す形）
	Used  bool   `json:"used"`           // 使っている（false なら、ほかの項目と合わないので使っていない）
	Note  string `json:"note,omitempty"` // 使っていない理由など
}

// applyOverrides は、設定ファイルの値 file に、上書き values を重ねた写しを返す。file は書き換えない。
// 重ねた結果が正しくない上書きは使わず、info にそう書く。
func applyOverrides(file *Config, values map[string]any) (*Config, []OverrideInfo) {
	if len(values) == 0 {
		return file, nil
	}
	eff := *file
	if file.Backlight != nil {
		b := *file.Backlight
		if b.Offline != nil {
			o := *b.Offline
			b.Offline = &o
		}
		eff.Backlight = &b
	} else {
		eff.Backlight = &BacklightConfig{}
	}
	var infos []OverrideInfo
	for _, k := range overrideKeys {
		v, ok := values[k.Key]
		if !ok {
			continue
		}
		in := OverrideInfo{Key: k.Key, Label: k.Label, Value: v, Shown: k.show(v), File: k.fileValue(file), Used: true}
		before := eff
		bl := *eff.Backlight
		if bl.Offline != nil {
			o := *bl.Offline
			bl.Offline = &o
		}
		before.Backlight = &bl
		k.apply(&eff, file, v)
		if err := checkOverridden(&eff); err != nil {
			eff = before
			in.Used, in.Note = false, err.Error()
		}
		infos = append(infos, in)
	}
	if file.Backlight == nil && values["backlight.level"] == nil && values["backlight.dim_after"] == nil &&
		values["backlight.offline.dim_after"] == nil && values["backlight.photo"] == nil {
		eff.Backlight = nil // backlight を上書きしていなければ、設定ファイルのとおり（自動減光をしない）
	}
	// 暗くしないので写真が出ないときは、そう書く
	if p, err := eff.Backlight.compile(); err == nil && p != nil {
		for i := range infos {
			if infos[i].Key == "backlight.photo" && infos[i].Used && infos[i].Value == true && !p.photo && (p.offline == nil || !p.offline.photo) {
				infos[i].Note = "暗くしない設定なので、写真は出ません"
			}
		}
	}
	return &eff, infos
}

// checkOverridden は、上書きを重ねた設定のうち、上書きで変わる項目を確かめる。
func checkOverridden(c *Config) error {
	if _, err := c.Backlight.compile(); err != nil {
		return fmt.Errorf("backlight: %v", err)
	}
	var msg string
	c.Lid.validate(func(_, f string, a ...any) { msg = fmt.Sprintf(f, a...) })
	if msg != "" {
		return errors.New(msg)
	}
	return nil
}

// OverrideStore は、上書きの値のファイル。
type OverrideStore struct {
	mu      sync.Mutex
	path    string
	values  map[string]any
	updated time.Time
}

type overrideFile struct {
	Format  int                        `json:"format"`
	Values  map[string]json.RawMessage `json:"values"`
	Updated time.Time                  `json:"updated,omitzero"`
	Source  string                     `json:"source,omitempty"`
}

// OpenOverrides は、dir/overrides.json を読む。読めない項目は捨てる（ログに出す）。
func OpenOverrides(dir string) *OverrideStore {
	s := &OverrideStore{path: filepath.Join(dir, overridesName), values: map[string]any{}}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("overrides: %v (not using overrides)", err)
		}
		return s
	}
	var f overrideFile
	if err := json.Unmarshal(b, &f); err != nil || f.Format != overridesFormat {
		log.Printf("overrides: %s is broken (%v); not using it", s.path, err)
		return s
	}
	for key, raw := range f.Values {
		k := overrideKeyOf(key)
		if k == nil {
			log.Printf("overrides: %s: unknown item %q (ignored)", s.path, key)
			continue
		}
		v, err := k.parse(raw)
		if err != nil {
			log.Printf("overrides: %s: %v (ignored)", s.path, err)
			continue
		}
		s.values[key] = v
	}
	s.updated = f.Updated
	if len(s.values) > 0 {
		log.Printf("overrides: %d values from %s", len(s.values), s.path)
	}
	return s
}

// Values は、今の上書きの写し。
func (s *OverrideStore) Values() map[string]any {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// Updated は、最後に変えた時刻。
func (s *OverrideStore) Updated() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updated
}

// Set は、key を raw にする（確かめてから）。保存は待つ（SD カードに書く）。
func (s *OverrideStore) Set(key string, raw json.RawMessage, source string) error {
	k := overrideKeyOf(key)
	if k == nil {
		return fmt.Errorf("%w %q (%s)", errOverrideKey, key, strings.Join(overrideKeyNames(), ", "))
	}
	v, err := k.parse(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.values[key]
	s.values[key] = v
	if err := s.saveLocked(source); err != nil {
		if had {
			s.values[key] = old
		} else {
			delete(s.values, key)
		}
		return err
	}
	return nil
}

// Clear は、keys の上書きを消す（keys が空ならすべて）。消したものを返す。
func (s *OverrideStore) Clear(keys []string, source string) ([]string, error) {
	for _, k := range keys {
		if overrideKeyOf(k) == nil {
			return nil, fmt.Errorf("%w %q (%s)", errOverrideKey, k, strings.Join(overrideKeyNames(), ", "))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := make(map[string]any, len(s.values))
	for k, v := range s.values {
		old[k] = v
	}
	var done []string
	if len(keys) == 0 {
		for k := range s.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	for _, k := range keys {
		if _, ok := s.values[k]; ok {
			delete(s.values, k)
			done = append(done, k)
		}
	}
	if len(done) == 0 {
		return []string{}, nil
	}
	if err := s.saveLocked(source); err != nil {
		s.values = old
		return nil, err
	}
	return done, nil
}

func (s *OverrideStore) saveLocked(source string) error {
	s.updated = time.Now()
	if len(s.values) == 0 {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	f := overrideFile{Format: overridesFormat, Values: map[string]json.RawMessage{}, Updated: s.updated, Source: source}
	for k, v := range s.values {
		b, _ := json.Marshal(v)
		f.Values[k] = b
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(b, '\n'), false)
}

func overrideKeyNames() []string {
	var out []string
	for _, k := range overrideKeys {
		out = append(out, k.Key)
	}
	return out
}
