package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---------- calibrate（タッチのキャリブレーション） ----------
//
// Brain の画面に十字を出し、順に押してもらう（lefthand の calibui.go）。brain-deck は始めて、終わるまで待ち、
// 前と後のずれを出す。保存するかは、Brain の確かめの画面でユーザーが決める。

type calErr struct {
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
}

type calStatus struct {
	State  string  `json:"state"`
	Point  int     `json:"point"`
	Points int     `json:"points"`
	Result string  `json:"result"`
	Before *calErr `json:"before"`
	After  *calErr `json:"after"`
	Gen    int     `json:"gen"`
}

type calInfo struct {
	Source string `json:"source"` // calibration か config
	Config *struct {
		MinX   int32 `json:"min_x"`
		MaxX   int32 `json:"max_x"`
		MinY   int32 `json:"min_y"`
		MaxY   int32 `json:"max_y"`
		SwapXY bool  `json:"swap_xy"`
	} `json:"config"`
	File *struct {
		Points  []json.RawMessage `json:"points"`
		Before  calErr            `json:"before"`
		After   calErr            `json:"after"`
		Created time.Time         `json:"created"`
	} `json:"file"`
}

func (e *calErr) String() string {
	if e == nil {
		return "-"
	}
	return fmt.Sprintf("平均 %.1f、最大 %.1f ドット", e.Mean, e.Max)
}

func (in calInfo) describe() string {
	if in.Source == "calibration" && in.File != nil {
		return fmt.Sprintf("キャリブレーション（%s、%d 点。ずれ：前 %s → 後 %s）", in.File.Created.Local().Format("2006-01-02 15:04"),
			len(in.File.Points), in.File.Before.String(), in.File.After.String())
	}
	if c := in.Config; c != nil {
		return fmt.Sprintf("設定の min と max（x %d〜%d、y %d〜%d）", c.MinX, c.MaxX, c.MinY, c.MaxY)
	}
	return "設定の min と max"
}

func runCalibrate(o *options, stdout, stderr io.Writer) int {
	if err := o.checkSub("points reset"); err != nil {
		return report(stderr, err)
	}
	if len(o.args) > 1 {
		return report(stderr, usageError("calibrate は引数を取りません（--points 9、--reset、--status）"))
	}
	kind := "touch"
	switch o.vals["points"] {
	case "", "5":
	case "9":
		kind = "touch9"
	default:
		return report(stderr, usageError("--points は 5 か 9 です"))
	}
	reset := o.flags["reset"]
	if reset && (o.status || o.vals["points"] != "") {
		return report(stderr, usageError("--reset は --points、--status と一緒には使えません"))
	}
	return connectAndRun(o, stdout, stderr, func(c *Client) (string, error) {
		if err := need(c, "calibrate"); err != nil {
			return "", err
		}
		call := func(p map[string]any) (calStatus, calInfo, bool, error) {
			raw, err := c.Call("calibrate", p, callTimeout())
			if err != nil {
				return calStatus{}, calInfo{}, false, err
			}
			var r struct {
				Status      calStatus `json:"status"`
				Calibration calInfo   `json:"calibration"`
				Reset       bool      `json:"reset"`
			}
			err = json.Unmarshal(raw, &r)
			return r.Status, r.Calibration, r.Reset, err
		}
		switch {
		case o.status:
			st, in, _, err := call(map[string]any{"mode": "status"})
			if err != nil {
				return "", err
			}
			s := "今の変換：" + in.describe()
			if st.State != "off" {
				s += fmt.Sprintf("\nキャリブレーションの最中（%s、%d/%d）", st.State, st.Point, st.Points)
			}
			return s, nil
		case reset:
			_, in, had, err := call(map[string]any{"mode": "reset"})
			if err != nil {
				return "", err
			}
			if !had {
				return "保存したキャリブレーションはありません。今の変換：" + in.describe(), nil
			}
			return "キャリブレーションをやめ、設定の min と max に戻しました（前のものは touch-cal.json.prev）。今の変換：" + in.describe(), nil
		}
		_, before, _, err := call(map[string]any{"mode": "status"})
		if err != nil {
			return "", err
		}
		st, _, _, err := call(map[string]any{"mode": "start", "kind": kind})
		if err != nil {
			return "", err
		}
		fmt.Fprintf(stderr, "brain-deck: Brain の画面の十字（%d 点）を、ペン先か爪で順に押してください。終わったら、確かめの画面で「保存」か「やめる」\n", st.Points)
		if o.noWait {
			return "始めました（今の変換：" + before.describe() + "）", nil
		}
		gen, last := st.Gen, ""
		deadline := time.Now().Add(10 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(time.Second)
			st, in, _, err := call(map[string]any{"mode": "status"})
			if err != nil {
				return "", err
			}
			if st.Gen != gen {
				return "", fmt.Errorf("ほかのところでキャリブレーションが始まりました")
			}
			now := st.State
			if st.State == "target" {
				now = fmt.Sprintf("十字 %d/%d", st.Point, st.Points)
			} else if st.State == "verify" {
				now = fmt.Sprintf("確かめの画面（ずれ：前 %s → 後 %s）", st.Before, st.After)
			}
			if now != last && st.State != "off" {
				fmt.Fprintln(stderr, "brain-deck: "+now)
				last = now
			}
			if st.State != "off" {
				continue
			}
			var b strings.Builder
			switch st.Result {
			case "saved":
				fmt.Fprintf(&b, "保存しました。十字と押した位置のずれ：前 %s → 後 %s\n", st.Before, st.After)
			case "timeout":
				b.WriteString("3 分さわらなかったので、やめました（前のまま）\n")
			default:
				b.WriteString("やめました（前のまま）\n")
				if st.Before != nil {
					fmt.Fprintf(&b, "（測ったずれ：前 %s → 後 %s）\n", st.Before, st.After)
				}
			}
			b.WriteString("今の変換：" + in.describe())
			return b.String(), nil
		}
		return "", fmt.Errorf("10 分たっても終わりませんでした（Brain の画面で続けられます）")
	})
}
