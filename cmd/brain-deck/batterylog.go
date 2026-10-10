package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- battery log（電池だけで動かしたときの記録） ----------
//
// Brain の lefthand は、電池だけで動いているあいだ、30 秒ごとに電圧を battery-log.csv に書く（battery.go と batteryguard.go）。
// brain-deck battery log はそれを取り出す。--fit は、いちばん新しい「外してから止まるまで」の記録を使い、
// 記録の最後を 0%、外したときを 100% として、時間の割合から電圧と残りの表（設定の battery.curve）を作る
// （画面の明るさなどの使い方が、ふだんと同じときに役に立つ）。

func batteryLog(c *Client, o *options) (string, error) {
	if !c.Hello.has("get_battery_log") {
		return "", errors.New("Brain の lefthand が古く、電池の記録を取り出せません（brain-deck update で新しくする）")
	}
	params := map[string]any{}
	if o.lines > 0 && !o.fit {
		params["lines"] = o.lines
	}
	raw, err := c.Call("get_battery_log", params, callTimeout())
	if err != nil {
		return "", err
	}
	var res struct {
		CSV string `json:"csv"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	if !o.fit {
		return strings.TrimRight(res.CSV, "\n"), nil
	}
	return fitBatteryCurve(res.CSV)
}

// batteryRow は記録の 1 行。
type batteryRow struct {
	t       time.Time
	avgMV   int
	pct     int
	plugged bool
	event   string
}

func parseBatteryLog(csv string) ([]batteryRow, error) {
	var rows []batteryRow
	for i, ln := range strings.Split(strings.TrimSpace(csv), "\n") {
		if i == 0 || ln == "" {
			continue
		}
		f := strings.Split(ln, ",")
		if len(f) < 9 {
			return nil, fmt.Errorf("line %d: %d fields (want 9)", i+1, len(f))
		}
		t, err := time.Parse(time.RFC3339, f[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		avg, _ := strconv.Atoi(f[3])
		pct, _ := strconv.Atoi(f[5])
		rows = append(rows, batteryRow{t: t, avgMV: avg, pct: pct, plugged: f[6] == "true", event: f[8]})
	}
	return rows, nil
}

// fitBatteryCurve は、いちばん新しい放電の記録から、電圧と残りの表を作る。
func fitBatteryCurve(csv string) (string, error) {
	rows, err := parseBatteryLog(csv)
	if err != nil {
		return "", err
	}
	// いちばん新しい「外した」から、次につないだ（なければ記録の最後）までの、電圧のある行
	end := len(rows)
	start := -1
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.plugged && i < end-1 && start < 0 {
			end = i
			continue
		}
		if !r.plugged && (strings.Contains(r.event, "unplugged") || strings.Contains(r.event, "start")) {
			start = i
			break
		}
	}
	if start < 0 {
		return "", errors.New("記録に「外した」（unplugged）がありません。USB を抜いて電池だけで動かしてから、もう一度実行してください")
	}
	var seg []batteryRow
	for _, r := range rows[start:end] {
		if !r.plugged && r.avgMV > 0 {
			seg = append(seg, r)
		}
	}
	if len(seg) < 20 {
		return "", fmt.Errorf("外してからの記録が %d 行しかありません（30 秒ごとなので、少なくとも 10 分は電池だけで動かしてください）", len(seg))
	}
	t0, t1 := seg[0].t, seg[len(seg)-1].t
	total := t1.Sub(t0)
	// 残り p% の時刻の前後 2 分の電圧（移動平均）の中央値
	at := func(p int) int {
		switch p { // 両端は、外したときと最後の値（平均の窓が片側にしかない）
		case 0:
			return seg[len(seg)-1].avgMV
		case 100:
			return seg[0].avgMV
		}
		mid := t1.Add(-time.Duration(float64(total) * float64(p) / 100))
		var vs []int
		for _, r := range seg {
			if d := r.t.Sub(mid); d >= -2*time.Minute && d <= 2*time.Minute {
				vs = append(vs, r.avgMV)
			}
		}
		if len(vs) == 0 {
			return 0
		}
		sort.Ints(vs)
		return vs[len(vs)/2]
	}
	var pts [][2]int
	for _, p := range []int{0, 5, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100} {
		v := at(p)
		if v == 0 || (len(pts) > 0 && v <= pts[len(pts)-1][0]) {
			continue // 電圧が上がらないところは飛ばす（表は電圧の順でなければならない）
		}
		pts = append(pts, [2]int{v, p})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s から %s まで（%v、%d 行）の記録から。記録の最後を 0%%、外したときを 100%% とみなしました。\n",
		t0.Local().Format("2006-01-02 15:04"), t1.Local().Format("2006-01-02 15:04"), total.Round(time.Minute), len(seg))
	fmt.Fprintf(&b, "# 最後に Brain が見積もっていた残り：%d%%（%d mV）\n", seg[len(seg)-1].pct, seg[len(seg)-1].avgMV)
	if !strings.Contains(rows[end-1].event, "poweroff") && end == len(rows) {
		b.WriteString("# 記録の最後に poweroff がありません。電池が切れて止まったか、まだ動いているかを確かめてください。\n")
	}
	b.WriteString("# 設定に書くときは、次の 2 行を config.yaml に足します（brain-deck config put）。\n")
	b.WriteString("battery:\n  curve: [")
	for i, p := range pts {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "[%d, %d]", p[0], p[1])
	}
	b.WriteString("]")
	return b.String(), nil
}
