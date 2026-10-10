package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- graph（グラフのセル） ----------
//
// brain-deck graph <id> <値> [<id> <値>...] で、グラフのセル（widget: graph）に値を足す。
// 1 回の呼び出しで、いくつもの id に足せる（contrib/graph/linux-stats.sh は CPU、メモリ、温度を 1 回で送る）。
// Brain は id ごとに新しい 120 個を覚え、SD カードには間引いて書く（数分に 1 回。README の「グラフ」）。

const graphUsage = `graph のあとには、<id> <値> の組（いくつでも）、<id> --clear、--list のどれかを書きます（例：brain-deck graph cpu 12.5 mem 48）`

type graphPoint struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

type graphInfo struct {
	Points []graphPoint `json:"points"`
	Count  int          `json:"count"`
	Last   *graphPoint  `json:"last"`
	Source string       `json:"source"`
}

func graphCommand(o *options) (func(*Client) (string, error), error) {
	a := o.args[1:]
	if o.list || (len(a) == 0 && o.json) {
		if len(a) != 0 || o.clear {
			return nil, usageError("graph --list は、ほかの引数を取りません")
		}
		return func(c *Client) (string, error) { return listGraphs(c, o.json) }, nil
	}
	if o.json {
		return nil, usageError("--json は graph --list で使います")
	}
	if o.clear {
		if len(a) != 1 {
			return nil, usageError("graph <id> --clear は、id を 1 つだけ取ります")
		}
		return func(c *Client) (string, error) {
			if err := need(c, "clear_graph"); err != nil {
				return "", err
			}
			raw, err := c.Call("clear_graph", map[string]any{"name": a[0]}, callTimeout())
			if err != nil {
				return "", err
			}
			var r struct {
				Cleared bool `json:"cleared"`
			}
			json.Unmarshal(raw, &r)
			if !r.Cleared {
				return a[0] + ": 値はありませんでした", nil
			}
			return a[0] + ": 値を消しました", nil
		}, nil
	}
	if len(a) == 0 || len(a)%2 != 0 {
		return nil, usageError(graphUsage)
	}
	var points []map[string]any
	var shown []string
	for i := 0; i < len(a); i += 2 {
		v, err := strconv.ParseFloat(a[i+1], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, usageError(fmt.Sprintf("%s の値 %q が数ではありません（12.5、-3、1e3 など）", a[i], a[i+1]))
		}
		points = append(points, map[string]any{"name": a[i], "value": v})
		shown = append(shown, a[i]+": "+a[i+1])
	}
	return func(c *Client) (string, error) {
		if err := need(c, "add_graph"); err != nil {
			return "", err
		}
		if !o.noTimeSync {
			if err := syncIfNeeded(c); err != nil {
				return "", err
			}
		}
		raw, err := c.Call("add_graph", map[string]any{"points": points, "source": "brain-deck"}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Unknown []string `json:"unknown"`
		}
		json.Unmarshal(raw, &r)
		if len(r.Unknown) > 0 {
			fmt.Fprintf(warnOut, "brain-deck: 注意：今の設定には、id が %s のグラフのセル（widget: graph）がありません。値は Brain に覚えました\n", strings.Join(r.Unknown, "、"))
		}
		return strings.Join(shown, "\n"), nil
	}, nil
}

func listGraphs(c *Client, asJSON bool) (string, error) {
	if err := need(c, "get_graph"); err != nil {
		return "", err
	}
	raw, err := c.Call("get_graph", nil, callTimeout())
	if err != nil {
		return "", err
	}
	if asJSON {
		return strings.TrimRight(string(raw), "\n"), nil
	}
	var r struct {
		Series map[string]graphInfo `json:"series"`
		IDs    []string             `json:"ids"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	inCfg := map[string]bool{}
	for _, id := range r.IDs {
		inCfg[id] = true
		if _, ok := r.Series[id]; !ok {
			r.Series[id] = graphInfo{}
		}
	}
	ids := make([]string, 0, len(r.Series))
	for id := range r.Series {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("id\t値の数\t最新\t時刻\t\n")
	for _, id := range ids {
		s := r.Series[id]
		last, at := "-", "-"
		if s.Last != nil {
			last = strconv.FormatFloat(s.Last.V, 'g', -1, 64)
			at = s.Last.T.Local().Format("01/02 15:04:05")
		}
		note := ""
		if !inCfg[id] {
			note = "セルなし"
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%s\n", id, s.Count, last, at, note)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
