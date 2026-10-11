package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// ---------- 設定の検証・保存・反映 ----------
//
// 設定 GUI の set_config は、次の順に行う。
//  1. 検証する（-check と同じ。加えて、再起動が要る項目が変わっていないこと）
//  2. YAML にして一時ファイルに書き、config.yaml と置き換える。前の版は config.yaml.prev に残す
//  3. デーモンを再起動せずに、動いているエンジンに反映する（押しているキーはすべて離す）
//  4. 反映に失敗したら、ファイルと動作を前の設定に戻す

// checkConfig は設定を読み、-check と同じ検証をする。
func checkConfig(raw []byte) (*Config, *Keymap, []string, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	km, warns, err := compileKeymap(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, km, warns, nil
}

// restartProblems は、動いているデーモンでは変えられない項目（開いているデバイス）の変更を返す。
// これらは設定ファイルを直接書き換え、サービスを再起動して反映する。
func restartProblems(cur, next *Config) Problems {
	var p Problems
	add := func(path, name string) {
		p = append(p, Problem{path, name + " cannot be changed while the daemon is running " +
			"(edit /etc/lefthand/config.yaml and restart lefthand.service)"})
	}
	if cur.HIDDevice != next.HIDDevice {
		add("/hid_device", "hid_device")
	}
	if cur.Keyboard != next.Keyboard {
		add("/keyboard", "keyboard")
	}
	switch {
	case (cur.Touch == nil) != (next.Touch == nil):
		add("/touch", "adding or removing the touch section")
	case cur.Touch != nil && cur.Touch.Device != next.Touch.Device:
		add("/touch/device", "touch.device")
	}
	cd, nd := *cur.Display, *next.Display
	if cur.displayEnabled() != next.displayEnabled() || cd.Device != nd.Device || cd.VT != nd.VT || cd.Rotate != nd.Rotate {
		add("/display", "display")
	}
	return p
}

// marshalConfig は設定を、手で編集できる YAML にする。元のファイルのコメントは残らない。
func marshalConfig(cfg *Config) ([]byte, error) {
	var doc yaml.Node
	if err := doc.Encode(cfg); err != nil {
		return nil, err
	}
	quoteCellKeys(&doc)
	doc.HeadComment = "/etc/lefthand/config.yaml\n" +
		"設定 GUI が書き出したファイル。手で編集してもよいが、GUI で保存するとコメントは消える。\n" +
		"書き方は docs/config.md、本体のキーの名前は docs/keymap-pwsh2.md を参照"
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// quoteCellKeys は、セルの番号 "列,行" を引用符で囲む（手で書くときと同じ見た目にする）。
func quoteCellKeys(n *yaml.Node) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i]; cellKeyRE.MatchString(k.Value) {
				k.Style = yaml.DoubleQuotedStyle
			}
		}
	}
	for _, c := range n.Content {
		quoteCellKeys(c)
	}
}

var cellKeyRE = regexp.MustCompile(`^\d+,\d+$`)

// writeFileAtomic は path を data で置き換える。途中で電源が切れても、
// path は前の内容か新しい内容のどちらかになる。keepPrev なら前の内容を path.prev に残す。
func writeFileAtomic(path string, data []byte, keepPrev bool) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // 置き換えたあとは存在しないので何もしない
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if keepPrev {
		if _, err := os.Stat(path); err == nil {
			// .prev もリンクと rename で置き換え、どの時点でも壊れた .prev を残さない
			ptmp := path + ".prev.tmp"
			os.Remove(ptmp)
			if err := os.Link(path, ptmp); err != nil {
				return fmt.Errorf("keep previous version: %w", err)
			}
			if err := os.Rename(ptmp, path+".prev"); err != nil {
				os.Remove(ptmp)
				return fmt.Errorf("keep previous version: %w", err)
			}
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// configStore は動いているデーモンの設定。設定 GUI からの読み書きを受け持つ。
// 設定ファイルの値（file）に、Brain で変えた上書きの値（ov、overrides.go）を重ねたもの（cfg）で動く。
// 設定 GUI と brain-deck が読み書きするのは設定ファイルの値で、上書きは別に読み書きする。
type configStore struct {
	mu   sync.Mutex
	path string
	file *Config // 設定ファイルの値。nil なら cfg と同じ（上書きがない）
	cfg  *Config // 動いている設定（上書きを重ねたもの）
	km   *Keymap
	ov   *OverrideStore  // nil なら上書きを使わない
	prof *ProfileService // nil なら PC ごとの設定を使わない
	// profName は、今の設定に重ねているプロファイル（変わったときだけ割り当てを反映し直す）
	profName string
	// apply は新しい割り当てを動いているデーモンに反映する（Engine.Reload と画面の描き直し）。
	apply func(*Config, *Keymap) error
	// light は、上書きだけが変わったときに、上書きで変わる項目（自動減光、ふた）だけを反映する。nil なら apply を使う
	light func(*Config)
	// onOverrides は、上書きが変わったとき（設定のアプリの描き直し）。待たずに返ること
	onOverrides func()
}

// Current は今の設定（上書きを重ねたもの）を返す。呼び出し側は書き換えないこと。
func (s *configStore) Current() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// File は設定ファイルの値（上書きを重ねる前）を返す。get_config はこちらを返す（設定 GUI が編集して保存するので）。
func (s *configStore) File() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file
	}
	return s.cfg
}

// Keymap は今の割り当てを返す。呼び出し側は書き換えないこと。
func (s *configStore) Keymap() *Keymap {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.km
}

// effective は、設定ファイルの値 file に、今の PC ごとの設定（プロファイル）と上書きを重ねる。
func (s *configStore) effective(file *Config) (*Config, []OverrideInfo) {
	base := file
	if name := s.prof.Resolve(file); name != "" {
		if c, err := applyProfile(file, name); err == nil {
			base = c
		} else {
			log.Printf("profile: %v (using the config without the profile)", err)
		}
	}
	return applyOverrides(base, s.ov.Values())
}

// check は raw を読み、上書きを重ね、-check と同じ検証をする。
func (s *configStore) check(raw []byte) (file, eff *Config, km *Keymap, warns []string, err error) {
	file, err = parseConfig(raw)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if s.ov == nil && s.prof == nil {
		km, warns, err = compileKeymap(file)
		return file, file, km, warns, err
	}
	// 上書きで直る誤り（写真を出すのに暗くしないなど）も、設定ファイルの誤りとして断る
	if _, err := file.Backlight.compile(); err != nil {
		return nil, nil, nil, nil, Problems{{Path: "/backlight", Message: "backlight: " + err.Error()}}
	}
	eff, _ = s.effective(file)
	km, warns, err = compileKeymap(eff)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return file, eff, km, warns, nil
}

// Validate は raw（YAML か JSON）を、適用せずに検証する。返すのは設定ファイルの値（layers の形にそろえたもの）。
func (s *configStore) Validate(raw []byte) (*Config, *Keymap, []string, error) {
	file, eff, km, warns, err := s.check(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mu.Lock()
	cur := s.cfg
	s.mu.Unlock()
	if p := restartProblems(cur, eff); len(p) > 0 {
		return nil, nil, nil, p
	}
	return file, km, warns, nil
}

// errRolledBack は、保存はできたが反映に失敗し、前の設定に戻したことを表す。
var errRolledBack = errors.New("rolled back to the previous config")

// Set は raw を検証し、保存して反映する。失敗したときは前の設定のまま動き続ける。
func (s *configStore) Set(raw []byte) ([]string, error) {
	file, eff, km, warns, err := s.check(raw)
	if err != nil {
		return nil, err
	}
	if p := restartProblems(s.Current(), eff); len(p) > 0 {
		return nil, p
	}
	out, err := marshalConfig(file)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	// 書き出した YAML を読み直し、同じ設定になることを確かめてから置き換える
	again, _, _, err := checkConfig(out)
	if err != nil || !reflect.DeepEqual(again, file) {
		return nil, fmt.Errorf("internal error: the YAML to be saved does not read back the same (%v)", err)
	}

	return warns, s.save(out, file, eff, km)
}

// save は out を設定ファイルに書き（前の版は .prev）、eff と km を反映する。反映に失敗したらファイルも戻す。
func (s *configStore) save(out []byte, file, eff *Config, km *Keymap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read current config: %w", err)
	}
	if err := writeFileAtomic(s.path, out, true); err != nil {
		return fmt.Errorf("save %s: %w", s.path, err)
	}
	if err := s.apply(eff, km); err != nil {
		aerr := err
		// ファイルを元に戻し、前の割り当てで動かす
		if err := writeFileAtomic(s.path, old, false); err != nil {
			return fmt.Errorf("apply failed (%v), and restoring %s also failed: %v", aerr, s.path, err)
		}
		if err := s.apply(s.cfg, s.km); err != nil {
			return fmt.Errorf("apply failed (%v), and re-applying the previous config also failed: %v", aerr, err)
		}
		return fmt.Errorf("apply failed: %v: %w", aerr, errRolledBack)
	}
	s.file, s.cfg, s.km, s.profName = file, eff, km, eff.profile
	return nil
}

// Overrides は、今の上書きの様子（項目の順）。
func (s *configStore) Overrides() []OverrideInfo {
	if s.ov == nil {
		return []OverrideInfo{}
	}
	_, info := s.effective(s.File())
	if info == nil {
		info = []OverrideInfo{}
	}
	return info
}

// errNoOverrides は、上書きを使わないデーモン（-data-dir がない）で上書きを変えようとしたこと。
var errNoOverrides = errors.New("overrides are not available")

// SetOverride は、上書き key を raw にして、すぐ反映する。
func (s *configStore) SetOverride(key string, raw json.RawMessage, source string) error {
	if s.ov == nil {
		return errNoOverrides
	}
	if err := s.ov.Set(key, raw, source); err != nil {
		return err
	}
	s.reapply()
	log.Printf("overrides: %s = %s (by %s)", key, raw, source)
	return nil
}

// ClearOverrides は、keys の上書きを消して（空ならすべて）、設定ファイルの値に戻す。
func (s *configStore) ClearOverrides(keys []string, source string) ([]string, error) {
	if s.ov == nil {
		return nil, errNoOverrides
	}
	done, err := s.ov.Clear(keys, source)
	if err != nil {
		return nil, err
	}
	if len(done) > 0 {
		s.reapply()
		log.Printf("overrides: cleared %s (by %s)", strings.Join(done, ", "), source)
	}
	return done, nil
}

// reapply は、上書きが変わったあとに、設定ファイルの値に重ね直して反映する。
func (s *configStore) reapply() {
	s.mu.Lock()
	file := s.file
	if file == nil {
		file = s.cfg
	}
	eff, _ := s.effective(file)
	s.file, s.cfg = file, eff
	light, apply, km, on := s.light, s.apply, s.km, s.onOverrides
	s.mu.Unlock()
	if light != nil {
		light(eff)
	} else if apply != nil {
		if err := apply(eff, km); err != nil {
			log.Printf("overrides: apply: %v", err)
		}
	}
	if on != nil {
		on()
	}
}

// Reprofile は、使うプロファイルが変わったかもしれないとき（PC が名乗った、手で選んだ、つなぎ直して名乗らなかった）に呼ぶ。
// 変わっていれば、プロファイルを重ね直して割り当てごと反映する（最初に開くレイヤーも開き直す）。
func (s *configStore) Reprofile() {
	s.mu.Lock()
	file := s.file
	if file == nil {
		file = s.cfg
	}
	name := s.prof.Resolve(file)
	if name == s.profName {
		s.mu.Unlock()
		return
	}
	eff, _ := s.effective(file)
	km, _, err := compileKeymap(eff)
	if err != nil {
		s.mu.Unlock()
		log.Printf("profile: %q cannot be used: %v (keeping %q)", name, err, s.profName)
		return
	}
	if err := s.apply(eff, km); err != nil {
		s.mu.Unlock()
		log.Printf("profile: applying %q: %v", name, err)
		return
	}
	old := s.profName
	s.file, s.cfg, s.km, s.profName = file, eff, km, name
	on := s.onOverrides
	s.mu.Unlock()
	log.Printf("profile: %q -> %q", old, name)
	if on != nil {
		on()
	}
}

// Profile は、今重ねているプロファイル（既定なら空）。
func (s *configStore) Profile() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profName
}

// errNeedsRestart は、put_config_text で、再起動が要る項目が変わったのに、再起動を許されていないこと。
var errNeedsRestart = errors.New("the config changes items that need a restart")

// PutText は、YAML のテキスト（手で書いたもの）を検証し、コメントもそのままにファイルを置き換えて、反映する。
// 前の版は .prev に残す。再起動が要る項目（hid_device など）が変わるときは、allowRestart なら書くだけで反映せず
// restart を true で返す（呼ぶ側がデーモンを再起動する）。許されていなければ、何も書かずに errNeedsRestart を返す。
// dryRun なら、検証だけをする。
func (s *configStore) PutText(raw []byte, allowRestart, dryRun bool) (warns []string, restart Problems, err error) {
	file, cfg, km, warns, err := s.check(raw)
	if err != nil {
		return nil, nil, asProblems(err) // YAML の文法の誤りなども、場所付きの誤りの形にする
	}
	rp := restartProblems(s.Current(), cfg)
	if len(rp) > 0 && !allowRestart && !dryRun {
		return warns, rp, errNeedsRestart
	}
	if dryRun {
		return warns, rp, nil
	}
	if len(rp) > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := writeFileAtomic(s.path, raw, true); err != nil {
			return nil, nil, fmt.Errorf("save %s: %w", s.path, err)
		}
		return warns, rp, nil
	}
	return warns, nil, s.save(raw, file, cfg, km)
}

// Text は設定ファイルの中身（prev なら .prev）を返す。
func (s *configStore) Text(prev bool) ([]byte, error) {
	p := s.path
	if prev {
		p += ".prev"
	}
	return os.ReadFile(p)
}
