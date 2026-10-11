package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------- PC ごとの設定（profiles） ----------
//
// Brain からは、つながれた PC を見分けられない。PC の brain-deck が、つないだときの自動の同期（brain-deck sync）で
// PC の名前を知らせる（set_pc）。設定の profiles に、PC の名前ごとに、設定ファイルの値に重ねる内容を書いておく：
//   - start_layer：最初に開くレイヤー（設定を反映したときと、プロファイルが変わったとき）
//   - modifiers：割り当ての修飾キーを入れ替える（{ctrl: gui, gui: ctrl} で、Linux 用の割り当てを Mac 用にするなど）
//   - home：ホーム画面を差し替える
//   - layers：レイヤーごとに、書いたキー・セル・ソフトキーだけを差し替える（名前で、設定ファイルのレイヤーに重ねる）
// 知らない PC、名乗らない PC（brain-deck が入っていない PC）のときは、設定ファイルのとおり（既定）。
// 手で選ぶこともできる（brain-deck profile use、設定のアプリ）。手で選んだものは、別の名前の PC が名乗るまで続く。
//
// 状態は <data-dir>/profile.json（最後に名乗った PC、手で選んだもの）。デーモンを起動し直しても同じ PC とみなす。
// USB を抜いてつなぎ直したあと、profileWait のあいだに名乗らなければ、名乗らない PC とみなして既定に戻す。

const (
	profileStateName = "profile.json"
	profileNone      = "none" // 手で「既定（プロファイルを使わない）」を選んだ
	profileAuto      = "auto" // 手で選んだものをやめて、PC の名前で決める
	profileWait      = 60 * time.Second
)

// ProfileConfig は設定の profiles の 1 つ。
type ProfileConfig struct {
	Name       string            `yaml:"name" json:"name"`
	Label      string            `yaml:"label,omitempty" json:"label,omitempty"`             // 画面に出す名前。省略すると name
	PCs        []string          `yaml:"pcs,omitempty" json:"pcs,omitempty"`                 // この名前の PC で使う（大文字と小文字は区別しない）。省略すると name と同じ名前の PC
	StartLayer string            `yaml:"start_layer,omitempty" json:"start_layer,omitempty"` // 最初に開くレイヤー
	Modifiers  map[string]string `yaml:"modifiers,omitempty" json:"modifiers,omitempty"`     // 修飾キーの入れ替え（ctrl、shift、alt、gui）
	Home       *HomeConfig       `yaml:"home,omitempty" json:"home,omitempty"`               // ホーム画面を差し替える
	Layers     []LayerConfig     `yaml:"layers,omitempty" json:"layers,omitempty"`           // レイヤーに重ねる（name で選ぶ）
}

// profileTitle は、今の設定に重ねたプロファイルの、画面に出す名前（重ねていなければ空）。
func (c *Config) profileTitle() string {
	if c == nil || c.profile == "" {
		return ""
	}
	for i := range c.Profiles {
		if c.Profiles[i].Name == c.profile {
			return c.Profiles[i].title()
		}
	}
	return c.profile
}

func (p *ProfileConfig) title() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

// matches は、PC の名前 pc でこのプロファイルを使うか。
func (p *ProfileConfig) matches(pc string) bool {
	if pc == "" {
		return false
	}
	if len(p.PCs) == 0 {
		return strings.EqualFold(p.Name, pc)
	}
	for _, n := range p.PCs {
		if strings.EqualFold(n, pc) {
			return true
		}
	}
	return false
}

var (
	profileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)
	pcNameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	modNames      = map[string]bool{"ctrl": true, "shift": true, "alt": true, "gui": true}
)

// profileByName は、設定の profiles のうち name のもの（なければ nil）。
func (c *Config) profileByName(name string) *ProfileConfig {
	for i := range c.Profiles {
		if c.Profiles[i].Name == name {
			return &c.Profiles[i]
		}
	}
	return nil
}

// profileForPC は、PC の名前 pc で使うプロファイル（なければ nil）。
func (c *Config) profileForPC(pc string) *ProfileConfig {
	for i := range c.Profiles {
		if c.Profiles[i].matches(pc) {
			return &c.Profiles[i]
		}
	}
	return nil
}

// validateProfiles は、profiles の書き方を確かめる（重ねた設定が使えるかは compileKeymap が確かめる）。
func (c *Config) validateProfiles(fail func(path, format string, args ...any)) {
	seen := map[string]bool{}
	pcs := map[string]string{}
	for i, p := range c.Profiles {
		at := fmt.Sprintf("/profiles/%d", i)
		switch {
		case !profileNameRE.MatchString(p.Name) || p.Name == profileNone || p.Name == profileAuto:
			fail(at+"/name", "profiles[%d].name must be letters, digits, _ . - (not %q or %q), got %q", i, profileNone, profileAuto, p.Name)
		case seen[p.Name]:
			fail(at+"/name", "profile %q is defined twice", p.Name)
		}
		seen[p.Name] = true
		names := p.PCs
		if len(names) == 0 {
			names = []string{p.Name}
		}
		for j, n := range names {
			if !pcNameRE.MatchString(n) {
				fail(fmt.Sprintf("%s/pcs/%d", at, j), "profile %q: PC name %q must be letters, digits, _ . -", p.Name, n)
				continue
			}
			if o, dup := pcs[strings.ToLower(n)]; dup && o != p.Name {
				fail(fmt.Sprintf("%s/pcs/%d", at, j), "PC %q is used by both profile %q and %q", n, o, p.Name)
			}
			pcs[strings.ToLower(n)] = p.Name
		}
		for k, v := range p.Modifiers {
			if !modNames[k] || !modNames[v] {
				fail(at+"/modifiers", "profile %q: modifiers maps ctrl, shift, alt or gui to one of them, got %s: %s", p.Name, k, v)
			}
		}
	}
}

// applyProfile は、設定ファイルの値 file に、プロファイル name を重ねた設定を返す（file は書き換えない）。
// name が空なら file をそのまま返す。
func applyProfile(file *Config, name string) (*Config, error) {
	if name == "" {
		return file, nil
	}
	p := file.profileByName(name)
	if p == nil {
		return nil, fmt.Errorf("profile %q is not in the config", name)
	}
	eff := *file
	eff.Profiles = nil // 重ねたものは、もう一度は重ねない（compileKeymap も確かめない）
	eff.profile = name
	if p.StartLayer != "" {
		eff.StartLayer = p.StartLayer
	}
	if p.Home != nil {
		eff.Home = p.Home
	}
	eff.Layers = append([]LayerConfig(nil), file.Layers...)
	for _, pl := range p.Layers {
		i := -1
		for j := range eff.Layers {
			if eff.Layers[j].Name == pl.Name {
				i = j
			}
		}
		if i < 0 {
			return nil, fmt.Errorf("profile %q: layer %q is not in the config (a profile can only change existing layers)", name, pl.Name)
		}
		lc := eff.Layers[i]
		if pl.Label != "" {
			lc.Label = pl.Label
		}
		lc.Keys = mergeSpecs(lc.Keys, pl.Keys)
		lc.SoftKeys = mergeSpecs(lc.SoftKeys, pl.SoftKeys)
		lc.Touch = mergeGrid(lc.Touch, pl.Touch)
		lc.TouchPortrait = mergeGrid(lc.TouchPortrait, pl.TouchPortrait)
		if pl.StatusBar != nil {
			lc.StatusBar = pl.StatusBar
		}
		eff.Layers[i] = lc
	}
	if len(p.Modifiers) > 0 {
		remapConfigMods(&eff, p.Modifiers)
	}
	return &eff, nil
}

func mergeSpecs(base, over map[string]ActionSpec) map[string]ActionSpec {
	if len(over) == 0 {
		return base
	}
	out := make(map[string]ActionSpec, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// mergeGrid は、格子 over を base に重ねる。over に cols か rows があれば、格子ごと差し替える。
func mergeGrid(base, over *GridConfig) *GridConfig {
	if over == nil {
		return base
	}
	if base == nil || over.Cols != 0 || over.Rows != 0 {
		g := *over
		return &g
	}
	g := *base
	g.Cells = mergeSpecs(base.Cells, over.Cells)
	if over.Background != "" {
		g.Background = over.Background
	}
	return &g
}

// remapConfigMods は、すべての割り当てのキー（key: LCTRL+C など）の修飾キーを m で入れ替える（左右はそのまま）。
// 割り当ての地図は写してから書き換える（設定ファイルの値は変えない）。
func remapConfigMods(c *Config, m map[string]string) {
	remap := func(specs map[string]ActionSpec) map[string]ActionSpec {
		if len(specs) == 0 {
			return specs
		}
		out := make(map[string]ActionSpec, len(specs))
		for k, s := range specs {
			if s.Key != "" {
				s.Key = remapComboMods(s.Key, m)
			}
			out[k] = s
		}
		return out
	}
	grid := func(g *GridConfig) *GridConfig {
		if g == nil {
			return nil
		}
		n := *g
		n.Cells = remap(g.Cells)
		return &n
	}
	c.Keys = remap(c.Keys)
	if c.Touch != nil && len(c.Touch.Cells) > 0 {
		t := *c.Touch
		t.Cells = remap(t.Cells)
		c.Touch = &t
	}
	for i := range c.Layers {
		l := &c.Layers[i]
		l.Keys = remap(l.Keys)
		l.SoftKeys = remap(l.SoftKeys)
		l.Touch = grid(l.Touch)
		l.TouchPortrait = grid(l.TouchPortrait)
	}
}

// remapComboMods は、"LCTRL+C" の修飾キーを m で入れ替える（{ctrl: gui} なら "LGUI+C"）。修飾キーでない部分は変えない。
func remapComboMods(s string, m map[string]string) string {
	parts := strings.Split(s, "+")
	for i, p := range parts {
		u := strings.ToUpper(strings.TrimSpace(p))
		if _, ok := modBits[u]; !ok || len(u) < 2 {
			continue
		}
		side, base := u[:1], strings.ToLower(u[1:])
		if to, ok := m[base]; ok {
			parts[i] = side + strings.ToUpper(to)
		}
	}
	return strings.Join(parts, "+")
}

// ---------- 状態（どの PC につながっているか、手で選んだもの） ----------

type profileState struct {
	PC       string    `json:"pc,omitempty"`        // 最後に名乗った PC（名乗らない PC なら空）
	PCAt     time.Time `json:"pc_at,omitzero"`      // 名乗った時刻
	Manual   string    `json:"manual,omitempty"`    // 手で選んだプロファイル（none は既定を選んだ）
	ManualPC string    `json:"manual_pc,omitempty"` // 手で選んだときの PC（別の PC が名乗ったら、手で選んだものをやめる）
}

// ProfileService は、今どのプロファイルを使うかを決める。設定の反映は onChange（configStore.reprofile）に任せる。
type ProfileService struct {
	mu       sync.Mutex
	path     string
	st       profileState
	wait     *time.Timer
	waitFor  time.Duration
	onChange func() // 使うプロファイルが変わったかもしれない。待たずに返ること
	now      func() time.Time
}

func NewProfileService(dataDir string) *ProfileService {
	s := &ProfileService{waitFor: profileWait, now: time.Now}
	if dataDir != "" {
		s.path = filepath.Join(dataDir, profileStateName)
		if b, err := os.ReadFile(s.path); err == nil {
			if err := json.Unmarshal(b, &s.st); err != nil {
				log.Printf("profile: %s: %v (ignored)", s.path, err)
				s.st = profileState{}
			}
		}
	}
	return s
}

func (s *ProfileService) SetOnChange(f func()) { s.mu.Lock(); s.onChange = f; s.mu.Unlock() }

func (s *ProfileService) saveLocked() {
	if s.path == "" {
		return
	}
	b, _ := json.MarshalIndent(s.st, "", " ")
	if err := writeFileAtomic(s.path, append(b, '\n'), false); err != nil {
		log.Printf("profile: %v", err)
	}
}

func (s *ProfileService) changedLocked() {
	if f := s.onChange; f != nil {
		go f()
	}
}

// Resolve は、設定 cfg（設定ファイルの値）のうち、今使うプロファイルの名前（既定なら空）。
func (s *ProfileService) Resolve(cfg *Config) string {
	if s == nil || cfg == nil || len(cfg.Profiles) == 0 {
		return ""
	}
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	return resolveProfile(cfg, st)
}

func resolveProfile(cfg *Config, st profileState) string {
	switch {
	case st.Manual == profileNone:
		return ""
	case st.Manual != "" && cfg.profileByName(st.Manual) != nil:
		return st.Manual
	}
	if p := cfg.profileForPC(st.PC); p != nil {
		return p.Name
	}
	return ""
}

var errProfileBad = errors.New("bad profile")

// SetPC は、PC が名乗った名前（set_pc）。別の PC なら、手で選んだものをやめる。
func (s *ProfileService) SetPC(name string) error {
	if !pcNameRE.MatchString(name) {
		return fmt.Errorf("%w: the PC name must be letters, digits, _ . - (up to 63), got %q", errProfileBad, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wait != nil {
		s.wait.Stop()
		s.wait = nil
	}
	if s.st.Manual != "" && !strings.EqualFold(s.st.ManualPC, name) {
		log.Printf("profile: PC %q announced itself; the profile chosen by hand (%s, on %q) is no longer used", name, s.st.Manual, s.st.ManualPC)
		s.st.Manual, s.st.ManualPC = "", ""
	}
	if !strings.EqualFold(s.st.PC, name) {
		log.Printf("profile: connected to PC %q", name)
	}
	s.st.PC, s.st.PCAt = name, s.now()
	s.saveLocked()
	s.changedLocked()
	return nil
}

// Use は、手で選ぶ（name はプロファイルの名前、none で既定、auto で手で選んだものをやめる）。cfg で名前を確かめる。
func (s *ProfileService) Use(name string, cfg *Config) error {
	if name != profileNone && name != profileAuto && (cfg == nil || cfg.profileByName(name) == nil) {
		var names []string
		if cfg != nil {
			for _, p := range cfg.Profiles {
				names = append(names, p.Name)
			}
		}
		return fmt.Errorf("%w: no profile %q in the config (profiles: %s; or none, auto)", errProfileBad, name, strings.Join(names, ", "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == profileAuto {
		s.st.Manual, s.st.ManualPC = "", ""
	} else {
		s.st.Manual, s.st.ManualPC = name, s.st.PC
	}
	s.saveLocked()
	s.changedLocked()
	return nil
}

// LinkChanged は、PC とのつながりが変わったとき（link.go）。外してつなぎ直したら、名乗るのを profileWait 待ち、
// 名乗らなければ名乗らない PC（既定）とみなす。
func (s *ProfileService) LinkChanged(online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wait != nil {
		s.wait.Stop()
		s.wait = nil
	}
	if !online {
		return
	}
	s.wait = time.AfterFunc(s.waitFor, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.wait == nil {
			return
		}
		s.wait = nil
		if s.st.PC != "" {
			log.Printf("profile: the PC did not announce itself within %v after connecting; using the default (not %q)", s.waitFor, s.st.PC)
			s.st.PC, s.st.PCAt = "", time.Time{}
			s.saveLocked()
			s.changedLocked()
		}
	})
}

// ProfileInfo は、get_profile と get_status の profile。
type ProfileInfo struct {
	Active   string        `json:"active"`              // 使っているプロファイル（既定なら空）
	Label    string        `json:"label,omitempty"`     // その表示名
	PC       string        `json:"pc,omitempty"`        // 最後に名乗った PC
	PCAt     time.Time     `json:"pc_at,omitzero"`      //
	Manual   string        `json:"manual,omitempty"`    // 手で選んだもの（none は既定）
	ManualPC string        `json:"manual_pc,omitempty"` //
	Waiting  bool          `json:"waiting,omitempty"`   // つなぎ直して、PC が名乗るのを待っている
	Profiles []ProfileName `json:"profiles"`            // 設定にあるプロファイル
}

// ProfileName は、設定にあるプロファイル 1 つ。
type ProfileName struct {
	Name  string   `json:"name"`
	Label string   `json:"label,omitempty"`
	PCs   []string `json:"pcs,omitempty"`
}

func (s *ProfileService) Info(cfg *Config) ProfileInfo {
	in := ProfileInfo{Profiles: []ProfileName{}}
	if s == nil {
		return in
	}
	s.mu.Lock()
	st := s.st
	in.Waiting = s.wait != nil
	s.mu.Unlock()
	in.PC, in.PCAt, in.Manual, in.ManualPC = st.PC, st.PCAt, st.Manual, st.ManualPC
	if cfg != nil {
		in.Active = resolveProfile(cfg, st)
		for _, p := range cfg.Profiles {
			pcs := p.PCs
			if len(pcs) == 0 {
				pcs = []string{p.Name}
			}
			in.Profiles = append(in.Profiles, ProfileName{Name: p.Name, Label: p.Label, PCs: pcs})
			if p.Name == in.Active {
				in.Label = p.title()
			}
		}
	}
	return in
}

// SetActive は、実際に重ねているプロファイル（使えないプロファイルなら既定のまま）にする。
func (in *ProfileInfo) SetActive(name string) {
	in.Active, in.Label = name, ""
	for _, p := range in.Profiles {
		if p.Name == name {
			in.Label = or(p.Label, p.Name)
		}
	}
}

// profileLabel は、設定のアプリと brain-deck に出す今のプロファイル。
func profileLabel(in ProfileInfo) string {
	name := "既定"
	if in.Active != "" {
		name = in.Label
	}
	switch {
	case in.Manual != "":
		return name + "（手で選んだ）"
	case in.PC != "":
		return name + "（" + in.PC + "）"
	case in.Waiting:
		return name + "（PC が名乗るのを待っている）"
	}
	return name + "（名乗らない PC か、PC なし）"
}
