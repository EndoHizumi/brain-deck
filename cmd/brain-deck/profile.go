package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// ---------- profile（PC ごとの設定） ----------
//
// Brain の設定の profiles に、PC の名前ごとの設定を書く（docs/config.md の「PC ごとの設定（profiles）」）。
// brain-deck sync（つないだときの自動の同期）が、最初に PC の名前を知らせる（set_pc）。名前は sync.yaml の pc_name、
// 書いていなければホスト名（最初の . まで）。

var pcNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// pcName は、Brain に知らせる、この PC の名前。
func pcName(cfg syncConfig) (string, error) {
	name := cfg.PCName
	if name == "" {
		h, err := os.Hostname()
		if err != nil {
			return "", err
		}
		name, _, _ = strings.Cut(h, ".")
	}
	if !pcNameRE.MatchString(name) {
		return "", fmt.Errorf("PC の名前 %q は、英数字と _ . - で書きます（sync.yaml の pc_name で決められます）", name)
	}
	return name, nil
}

type profileInfo struct {
	Active   string    `json:"active"`
	Label    string    `json:"label"`
	PC       string    `json:"pc"`
	PCAt     time.Time `json:"pc_at"`
	Manual   string    `json:"manual"`
	Waiting  bool      `json:"waiting"`
	Profiles []struct {
		Name  string   `json:"name"`
		Label string   `json:"label"`
		PCs   []string `json:"pcs"`
	} `json:"profiles"`
}

func (p profileInfo) describe() string {
	name := "既定（設定ファイルのとおり）"
	if p.Active != "" {
		name = p.Active
		if p.Label != "" && p.Label != p.Active {
			name += "（" + p.Label + "）"
		}
	}
	switch {
	case p.Manual == "none":
		return name + "。手で既定を選んだ"
	case p.Manual != "":
		return name + "。手で選んだ（別の PC が名乗るまで。brain-deck profile auto でやめる）"
	case p.PC != "":
		return name + "。PC " + p.PC + " が名乗った"
	case p.Waiting:
		return name + "。PC が名乗るのを待っている"
	}
	return name + "。名乗った PC はない"
}

func getProfile(c *Client) (profileInfo, error) {
	var p profileInfo
	if err := need(c, "get_profile"); err != nil {
		return p, err
	}
	raw, err := c.Call("get_profile", nil, callTimeout())
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}

// profileCommand は、brain-deck profile [show | list | use <名前> | auto | announce [<名前>]]。
func profileCommand(o *options) (func(*Client) (string, error), error) {
	a := o.args[1:]
	sub := "show"
	if len(a) > 0 {
		sub = a[0]
	}
	switch {
	case sub == "show" && len(a) <= 1:
		return func(c *Client) (string, error) {
			p, err := getProfile(c)
			if err != nil {
				return "", err
			}
			out := "PC ごとの設定\t" + p.describe()
			if len(p.Profiles) == 0 {
				out += "\n（Brain の設定に profiles がありません。書き方は docs/config.md の「PC ごとの設定（profiles）」）"
			}
			return out, nil
		}, nil
	case sub == "list" && len(a) == 1:
		return func(c *Client) (string, error) {
			p, err := getProfile(c)
			if err != nil {
				return "", err
			}
			if len(p.Profiles) == 0 {
				return "Brain の設定に profiles がありません", nil
			}
			var b strings.Builder
			for _, pr := range p.Profiles {
				mark := " "
				if pr.Name == p.Active {
					mark = "*"
				}
				fmt.Fprintf(&b, "%s %s\t%s\tPC：%s\n", mark, pr.Name, or(pr.Label, "-"), strings.Join(pr.PCs, "、"))
			}
			return strings.TrimSuffix(b.String(), "\n"), nil
		}, nil
	case sub == "use" && len(a) == 2, sub == "auto" && len(a) == 1:
		name := "auto"
		if sub == "use" {
			name = a[1]
		}
		return func(c *Client) (string, error) {
			if err := need(c, "set_profile"); err != nil {
				return "", err
			}
			raw, err := c.Call("set_profile", map[string]any{"name": name, "source": "brain-deck"}, callTimeout())
			if err != nil {
				return "", err
			}
			var p profileInfo
			json.Unmarshal(raw, &p)
			return "PC ごとの設定\t" + p.describe(), nil
		}, nil
	case sub == "announce" && len(a) <= 2:
		return func(c *Client) (string, error) {
			name := ""
			if len(a) == 2 {
				name = a[1]
			} else {
				cfg, _, err := loadSyncConfig()
				if err != nil {
					return "", err
				}
				if name, err = pcName(cfg); err != nil {
					return "", err
				}
			}
			p, err := announcePC(c, name)
			if err != nil {
				return "", err
			}
			return "PC ごとの設定\t" + p.describe(), nil
		}, nil
	}
	return nil, usageError("profile のあとには show、list、use <名前>（none で既定）、auto、announce [<PC の名前>] を書きます")
}

// announcePC は、Brain に PC の名前を知らせる。
func announcePC(c *Client, name string) (profileInfo, error) {
	var p profileInfo
	if err := need(c, "set_pc"); err != nil {
		return p, err
	}
	raw, err := c.Call("set_pc", map[string]any{"name": name, "source": "brain-deck"}, callTimeout())
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}
