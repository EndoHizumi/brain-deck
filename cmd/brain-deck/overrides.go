package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------- brain-deck overrides（Brain の設定のアプリで変えた上書きの値） ----------
//
// Brain の設定のアプリで変えた値は、設定ファイル（/etc/lefthand/config.yaml）を書き換えずに、Brain の
// /var/lib/lefthand/overrides.json に「上書き」として置く（lefthand の overrides.go）。ここで見る・消す・置く。
//
//	brain-deck overrides                         上書きの一覧（設定ファイルの値と並べる）
//	brain-deck overrides clear <名前>... | all   上書きを消して、設定ファイルの値に戻す
//	brain-deck overrides set <名前> <値>         上書きを置く（Brain の設定のアプリと同じ）

// overrideInfo は lefthand の OverrideInfo。
type overrideInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Shown string `json:"shown"`
	File  string `json:"file"`
	Used  bool   `json:"used"`
	Note  string `json:"note"`
}

func (o overrideInfo) describe() string {
	s := fmt.Sprintf("%s：%s（設定ファイル：%s）", o.Label, o.Shown, o.File)
	if !o.Used {
		s += "　使っていない：" + o.Note
	} else if o.Note != "" {
		s += "　" + o.Note
	}
	return s
}

// describeOverrides は、brain-deck status の「上書き」の行。
func describeOverrides(list []overrideInfo) string {
	if len(list) == 0 {
		return "なし（設定ファイルの値で動いている）"
	}
	var out []string
	for _, o := range list {
		out = append(out, o.describe())
	}
	return strings.Join(out, "\n\t") + "\n\t（消すには brain-deck overrides clear all、または Brain の設定のアプリ）"
}

var errNoOverrides = errors.New("Brain の lefthand が古く、上書きの値（設定のアプリ）がありません。brain-deck update で新しくする")

func overridesCommand(o *options) (func(*Client) (string, error), error) {
	args := o.args[1:]
	list := func(c *Client) (string, error) {
		raw, err := c.Call("get_overrides", nil, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Overrides []overrideInfo `json:"overrides"`
			Keys      []string       `json:"keys"`
		}
		json.Unmarshal(raw, &r)
		if len(r.Overrides) == 0 {
			return "上書きはありません（設定ファイルの値で動いています）。置ける名前：" + strings.Join(r.Keys, "、"), nil
		}
		var b strings.Builder
		for _, x := range r.Overrides {
			fmt.Fprintf(&b, "%s\t%s\n", x.Key, x.describe())
		}
		return strings.TrimSuffix(b.String(), "\n"), nil
	}
	need := func(c *Client) error {
		if !c.Hello.has("get_overrides") {
			return errNoOverrides
		}
		return nil
	}
	switch {
	case len(args) == 0 || args[0] == "list":
		if len(args) > 1 {
			return nil, usageError("overrides list は引数を取りません")
		}
		return func(c *Client) (string, error) {
			if err := need(c); err != nil {
				return "", err
			}
			return list(c)
		}, nil
	case args[0] == "clear":
		keys := args[1:]
		if len(keys) == 0 {
			return nil, usageError("overrides clear のあとに、消す名前か all を書きます（brain-deck overrides clear all）")
		}
		if len(keys) == 1 && keys[0] == "all" {
			keys = []string{}
		}
		return func(c *Client) (string, error) {
			if err := need(c); err != nil {
				return "", err
			}
			raw, err := c.Call("clear_overrides", map[string]any{"keys": keys, "source": "brain-deck"}, callTimeout())
			if err != nil {
				return "", err
			}
			var r struct {
				Cleared []string `json:"cleared"`
			}
			json.Unmarshal(raw, &r)
			if len(r.Cleared) == 0 {
				return "消す上書きはありませんでした", nil
			}
			return fmt.Sprintf("上書きを消しました（設定ファイルの値に戻しました）：%s", strings.Join(r.Cleared, "、")), nil
		}, nil
	case args[0] == "set":
		if len(args) != 3 {
			return nil, usageError("overrides set のあとに、名前と値を書きます（brain-deck overrides set backlight.level 5）")
		}
		key, val := args[1], args[2]
		var v json.RawMessage
		if json.Valid([]byte(val)) {
			v = json.RawMessage(val) // 5、true など
		} else {
			b, _ := json.Marshal(val) // 3m、none など
			v = b
		}
		return func(c *Client) (string, error) {
			if err := need(c); err != nil {
				return "", err
			}
			if _, err := c.Call("set_override", map[string]any{"key": key, "value": v, "source": "brain-deck"}, callTimeout()); err != nil {
				return "", err
			}
			return list(c)
		}, nil
	}
	return nil, usageError("overrides のあとには、list、clear、set のどれかを書きます（" + args[0] + "）")
}
