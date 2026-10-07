package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
type configStore struct {
	mu   sync.Mutex
	path string
	cfg  *Config
	km   *Keymap
	// apply は新しい割り当てを動いているデーモンに反映する（Engine.Reload と画面の描き直し）。
	apply func(*Config, *Keymap) error
}

// Current は今の設定を返す。呼び出し側は書き換えないこと。
func (s *configStore) Current() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Keymap は今の割り当てを返す。呼び出し側は書き換えないこと。
func (s *configStore) Keymap() *Keymap {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.km
}

// Validate は raw（YAML か JSON）を、適用せずに検証する。
func (s *configStore) Validate(raw []byte) (*Config, *Keymap, []string, error) {
	cfg, km, warns, err := checkConfig(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mu.Lock()
	cur := s.cfg
	s.mu.Unlock()
	if p := restartProblems(cur, cfg); len(p) > 0 {
		return nil, nil, nil, p
	}
	return cfg, km, warns, nil
}

// errRolledBack は、保存はできたが反映に失敗し、前の設定に戻したことを表す。
var errRolledBack = errors.New("rolled back to the previous config")

// Set は raw を検証し、保存して反映する。失敗したときは前の設定のまま動き続ける。
func (s *configStore) Set(raw []byte) ([]string, error) {
	cfg, km, warns, err := s.Validate(raw)
	if err != nil {
		return nil, err
	}
	out, err := marshalConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	// 書き出した YAML を読み直し、同じ設定になることを確かめてから置き換える
	again, _, _, err := checkConfig(out)
	if err != nil || !reflect.DeepEqual(again, cfg) {
		return nil, fmt.Errorf("internal error: the YAML to be saved does not read back the same (%v)", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	if err := writeFileAtomic(s.path, out, true); err != nil {
		return nil, fmt.Errorf("save %s: %w", s.path, err)
	}
	if err := s.apply(cfg, km); err != nil {
		aerr := err
		// ファイルを元に戻し、前の割り当てで動かす
		if err := writeFileAtomic(s.path, old, false); err != nil {
			return nil, fmt.Errorf("apply failed (%v), and restoring %s also failed: %v", aerr, s.path, err)
		}
		if err := s.apply(s.cfg, s.km); err != nil {
			return nil, fmt.Errorf("apply failed (%v), and re-applying the previous config also failed: %v", aerr, err)
		}
		return nil, fmt.Errorf("apply failed: %v: %w", aerr, errRolledBack)
	}
	s.cfg, s.km = cfg, km
	return warns, nil
}
