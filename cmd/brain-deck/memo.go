package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"
	"time"

	"lefthand/internal/ink"
)

// ---------- memo（手書きメモ） ----------
//
// Brain の手書きメモ（widget: memo）の一覧、取り出し（PNG）、削除。線は Brain に点の列で置いてあり、
// PNG は PC で描く（Brain と同じ描き方。internal/ink）。Brain のデータが正で、PC に写しは持たない。

const memoUsage = `memo のあとには、list（省略可）、get <番号|id> -o <ファイル.png> [--page N]、rm <番号|id>... を書きます`

type memoInfo struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Pages   int       `json:"pages"`
	Strokes int       `json:"strokes"`
	Bytes   int64     `json:"bytes"`
}

type memoDoc struct {
	ID      string     `json:"id"`
	Created time.Time  `json:"created"`
	Updated time.Time  `json:"updated"`
	Pages   []ink.Page `json:"pages"`
}

func memoCommand(o *options) (func(*Client) (string, error), error) {
	a := o.args[1:]
	sub := "list"
	if len(a) > 0 {
		sub, a = a[0], a[1:]
	}
	switch sub {
	case "list", "ls":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		return func(c *Client) (string, error) { return listMemos(c, o.json) }, nil
	case "get":
		if err := o.checkSub("o page"); err != nil {
			return nil, err
		}
		if len(a) != 1 || o.vals["o"] == "" {
			return nil, usageError("memo get <番号|id> -o <ファイル.png> と書きます")
		}
		page := 0
		if p := o.vals["page"]; p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 {
				return nil, usageError("--page は 1 からのページの番号です")
			}
			page = n
		}
		return func(c *Client) (string, error) { return getMemo(c, a[0], o.vals["o"], page) }, nil
	case "rm", "delete":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("memo rm のあとに、消すメモの番号か id（memo list）を書きます")
		}
		return func(c *Client) (string, error) { return rmMemos(c, a) }, nil
	}
	return nil, usageError(memoUsage)
}

func fetchMemos(c *Client) ([]memoInfo, error) {
	if err := need(c, "list_memos"); err != nil {
		return nil, err
	}
	raw, err := c.Call("list_memos", nil, callTimeout())
	if err != nil {
		return nil, err
	}
	var r struct {
		Memos []memoInfo `json:"memos"`
	}
	err = json.Unmarshal(raw, &r)
	return r.Memos, err
}

func listMemos(c *Client, asJSON bool) (string, error) {
	l, err := fetchMemos(c)
	if err != nil {
		return "", err
	}
	if asJSON {
		b, _ := json.MarshalIndent(l, "", "  ")
		return string(b), nil
	}
	if len(l) == 0 {
		return "Brain に手書きメモはありません", nil
	}
	var b strings.Builder
	for i, m := range l {
		fmt.Fprintf(&b, "%2d  %-4s  %d ページ、%d 本  %s 更新  %s\n", i+1, m.ID, m.Pages, m.Strokes, m.Updated.Local().Format("2006-01-02 15:04"), kb(m.Bytes))
	}
	fmt.Fprintf(&b, "%d 個", len(l))
	return b.String(), nil
}

func memoRef(l []memoInfo, ref string) (string, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		if n < 1 || n > len(l) {
			return "", usageError(fmt.Sprintf("メモの番号 %d はありません（1〜%d。brain-deck memo list）", n, len(l)))
		}
		return l[n-1].ID, nil
	}
	for _, m := range l {
		if m.ID == ref {
			return m.ID, nil
		}
	}
	return "", usageError("メモ " + ref + " はありません（brain-deck memo list）")
}

// readMemo は、get_memo でメモの JSON を読む。
func readMemo(c *Client, id string) (*memoDoc, error) {
	var buf []byte
	for {
		raw, err := c.Call("get_memo", map[string]any{"memo": id, "offset": len(buf)}, callTimeout())
		if err != nil {
			return nil, err
		}
		var r struct {
			Total int64  `json:"total"`
			Data  string `json:"data"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		b, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			return nil, err
		}
		buf = append(buf, b...)
		if int64(len(buf)) >= r.Total || len(b) == 0 {
			break
		}
	}
	var m memoDoc
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, fmt.Errorf("memo %s: %w", id, err)
	}
	return &m, nil
}

func getMemo(c *Client, ref, out string, page int) (string, error) {
	l, err := fetchMemos(c)
	if err != nil {
		return "", err
	}
	id, err := memoRef(l, ref)
	if err != nil {
		return "", err
	}
	m, err := readMemo(c, id)
	if err != nil {
		return "", err
	}
	pages := m.Pages
	if page > 0 {
		if page > len(pages) {
			return "", usageError(fmt.Sprintf("メモ %s のページは 1〜%d です", id, len(pages)))
		}
		pages = pages[page-1 : page]
	}
	if err := writeMemoPNG(out, pages); err != nil {
		return "", err
	}
	return fmt.Sprintf("メモ %s（%d ページ）を %s に書きました", id, len(pages), out), nil
}

// memoPageGap は、ページを縦に並べるときのあいだ（灰色）。
const memoPageGap = 8

// writeMemoPNG は、ページを縦に並べて 1 つの PNG にする（Brain の画面と同じ描き方）。
func writeMemoPNG(path string, pages []ink.Page) error {
	w, h := 1, 0
	for i, p := range pages {
		w = max(w, p.W)
		h += p.H
		if i > 0 {
			h += memoPageGap
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, max(h, 1)))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	y0 := 0
	for _, p := range pages {
		rgb := ink.Render(&p)
		for i := 0; i < p.W*p.H; i++ {
			img.Set(i%p.W, y0+i/p.W, color.RGBA{rgb[i*3], rgb[i*3+1], rgb[i*3+2], 255})
		}
		y0 += p.H + memoPageGap
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func rmMemos(c *Client, refs []string) (string, error) {
	l, err := fetchMemos(c)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, r := range refs {
		id, err := memoRef(l, r)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if _, err := c.Call("delete_memo", map[string]any{"memo": id}, callTimeout()); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%d 個のメモを消しました", len(ids)), nil
}
