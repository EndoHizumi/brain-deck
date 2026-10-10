package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lefthand/internal/imgconv"
)

// ---------- photo（フォトフレームの写真） ----------
//
// 写真の変換（切り抜き、縮小、RGB565、ディザリング）は、設定 GUI と同じ計算（internal/imgconv）で PC で行い、
// できたファイル（800×480 か 480×800 の LHI1）を upload_*（kind: photo）で送る。

const photoUsage = `photo のあとには、list（省略可）、add <ファイル>... [--portrait] [--of <番号|id>] [--zoom 1.5] [--center x,y] [--brightness -20] [--no-dither]、
rm <番号|id>...、get <番号|id> -o <ファイル.png> を書きます`

type photoInfo struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	W     int       `json:"w"`
	H     int       `json:"h"`
	Bytes int64     `json:"bytes"`
	Added time.Time `json:"added"`
	Of    string    `json:"of"`
}

type photoList struct {
	Photos     []photoInfo `json:"photos"`
	Max        int         `json:"max"`
	TotalBytes int64       `json:"total_bytes"`
	FreeBytes  int64       `json:"free_bytes"`
	Reserve    int64       `json:"reserve_bytes"`
	Showing    string      `json:"showing"`
}

// photoAdd は、photo add の 1 つのファイル。
type photoAdd struct {
	path, name string
	data       []byte
	w, h       int
	crop       imgconv.Crop
}

func photoCommand(o *options) (func(*Client) (string, error), error) {
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
		if len(a) != 0 {
			return nil, usageError("photo list は引数を取りません")
		}
		return func(c *Client) (string, error) { return listPhotos(c, o.json) }, nil
	case "add":
		if err := o.checkSub("portrait of zoom center brightness no-dither"); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("photo add のあとに、写真のファイル（PNG、JPEG、GIF、WebP）を書きます")
		}
		if o.vals["of"] != "" && len(a) != 1 {
			return nil, usageError("--of を書くときは、ファイルを 1 つだけ書きます")
		}
		p, portrait, err := photoParams(o)
		if err != nil {
			return nil, err
		}
		tw, th := 800, 480
		if portrait {
			tw, th = 480, 800
		}
		var adds []photoAdd
		for _, f := range a {
			src, err := decodeImageFile(f)
			if err != nil {
				return nil, usageError(f + ": " + err.Error())
			}
			pix, crop := imgconv.Convert(src, tw, th, p)
			name := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
			if portrait {
				name += "（縦）"
			}
			adds = append(adds, photoAdd{path: f, name: name, data: imgconv.Encode(tw, th, pix), w: tw, h: th, crop: crop})
		}
		if o.timeout == defaultTimeout {
			o.timeout = time.Duration(len(adds)) * imagePutTimeout
		}
		return func(c *Client) (string, error) { return addPhotos(c, adds, o.vals["of"]) }, nil
	case "rm", "delete":
		if err := o.checkSub(""); err != nil {
			return nil, err
		}
		if len(a) == 0 {
			return nil, usageError("photo rm のあとに、消す写真の番号か id（photo list）を書きます")
		}
		return func(c *Client) (string, error) { return rmPhotos(c, a) }, nil
	case "get":
		if err := o.checkSub("o"); err != nil {
			return nil, err
		}
		if len(a) != 1 || o.vals["o"] == "" {
			return nil, usageError("photo get <番号|id> -o <ファイル.png> と書きます")
		}
		return func(c *Client) (string, error) { return getPhoto(c, a[0], o.vals["o"]) }, nil
	}
	return nil, usageError(photoUsage)
}

// photoParams は、photo add のオプションを変換の設定にする。
func photoParams(o *options) (imgconv.Params, bool, error) {
	p := imgconv.Params{CX: -1, CY: -1, ZoomMilli: 1000, Dither: !o.flags["no-dither"]}
	portrait := o.flags["portrait"] || o.vals["of"] != ""
	if z := o.vals["zoom"]; z != "" {
		f, err := strconv.ParseFloat(z, 64)
		if err != nil || f < 1 || f > 8 {
			return p, false, usageError("--zoom は 1〜8 の数です")
		}
		p.ZoomMilli = int(f*1000 + 0.5)
	}
	if c := o.vals["center"]; c != "" {
		if _, err := fmt.Sscanf(c, "%d,%d", &p.CX, &p.CY); err != nil || p.CX < 0 || p.CY < 0 {
			return p, false, usageError("--center は、元の画像の画素の位置を x,y で書きます（例 1200,800）")
		}
	}
	if b := o.vals["brightness"]; b != "" {
		n, err := strconv.Atoi(b)
		if err != nil || n < -100 || n > 100 {
			return p, false, usageError("--brightness は -100〜100（百分率。負なら暗く）です")
		}
		p.Brightness = n
	}
	return p, portrait, nil
}

func fetchPhotos(c *Client) (photoList, error) {
	var l photoList
	if err := need(c, "list_photos"); err != nil {
		return l, err
	}
	raw, err := c.Call("list_photos", nil, callTimeout())
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(raw, &l)
	return l, err
}

func listPhotos(c *Client, asJSON bool) (string, error) {
	if asJSON {
		if err := need(c, "list_photos"); err != nil {
			return "", err
		}
		raw, err := c.Call("list_photos", nil, callTimeout())
		return string(raw), err
	}
	l, err := fetchPhotos(c)
	if err != nil {
		return "", err
	}
	num := map[string]int{}
	for i, p := range l.Photos {
		num[p.ID] = i + 1
	}
	var b strings.Builder
	for i, p := range l.Photos {
		kind := "横"
		if p.H > p.W {
			kind = "縦"
		}
		extra := ""
		if p.Of != "" {
			if n, ok := num[p.Of]; ok {
				extra = fmt.Sprintf("（%d を縦向きに切り抜いたもの）", n)
			} else {
				extra = "（もとの写真は消してある）"
			}
		}
		mark := " "
		if p.ID == l.Showing {
			mark = "*"
		}
		fmt.Fprintf(&b, "%s%2d  %s  %s %3dx%-3d  %s  %s%s\n", mark, i+1, p.ID, kind, p.W, p.H, p.Added.Local().Format("2006-01-02 15:04"), p.Name, extra)
	}
	fmt.Fprintf(&b, "%d 枚（上限 %d 枚）、%s。SD カードの空き %s（%s は残す）", len(l.Photos), l.Max, kb(l.TotalBytes), kb(l.FreeBytes), kb(l.Reserve))
	if l.Showing != "" {
		b.WriteString("。* は今出している写真")
	}
	return b.String(), nil
}

// photoRef は、番号か id を id にする。
func photoRef(l photoList, ref string) (string, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		if n < 1 || n > len(l.Photos) {
			return "", usageError(fmt.Sprintf("写真の番号 %d はありません（1〜%d。brain-deck photo list）", n, len(l.Photos)))
		}
		return l.Photos[n-1].ID, nil
	}
	for _, p := range l.Photos {
		if p.ID == ref {
			return p.ID, nil
		}
	}
	return "", usageError("写真 " + ref + " はありません（brain-deck photo list）")
}

func addPhotos(c *Client, adds []photoAdd, of string) (string, error) {
	if err := need(c, "list_photos"); err != nil {
		return "", err
	}
	extra := map[string]any{}
	if of != "" {
		l, err := fetchPhotos(c)
		if err != nil {
			return "", err
		}
		id, err := photoRef(l, of)
		if err != nil {
			return "", err
		}
		extra["of"] = id
	}
	var b strings.Builder
	for _, a := range adds {
		raw, err := uploadFileWith(c, "photo", a.name, a.data, extra)
		if err != nil {
			return b.String(), fmt.Errorf("%s: %w", a.path, err)
		}
		var r struct {
			ID    string `json:"id"`
			Count int    `json:"count"`
			Max   int    `json:"max"`
		}
		json.Unmarshal(raw, &r)
		fmt.Fprintf(&b, "%s → %s（%dx%d、元の画像の %d,%d から %dx%d を使った）。%d 枚 / 上限 %d 枚\n",
			a.path, r.ID, a.w, a.h, a.crop.X, a.crop.Y, a.crop.W, a.crop.H, r.Count, r.Max)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func rmPhotos(c *Client, refs []string) (string, error) {
	l, err := fetchPhotos(c)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, r := range refs {
		id, err := photoRef(l, r)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if _, err := c.Call("delete_photo", map[string]any{"photo": id}, callTimeout()); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%d 枚を消しました", len(ids)), nil
}

// readPhoto は、写真のファイルを get_photo で読む。
func readPhoto(c *Client, id string) ([]byte, error) {
	var out []byte
	for {
		raw, err := c.Call("get_photo", map[string]any{"photo": id, "offset": len(out)}, callTimeout())
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
		out = append(out, b...)
		if int64(len(out)) >= r.Total || len(b) == 0 {
			return out, nil
		}
	}
}

func getPhoto(c *Client, ref, outPath string) (string, error) {
	l, err := fetchPhotos(c)
	if err != nil {
		return "", err
	}
	id, err := photoRef(l, ref)
	if err != nil {
		return "", err
	}
	b, err := readPhoto(c, id)
	if err != nil {
		return "", err
	}
	if err := write565PNG(b, outPath); err != nil {
		return "", err
	}
	return "写真 " + id + " を " + outPath + " に書きました", nil
}

// write565PNG は、LHI1 のファイルを PNG にする（Brain の画面と同じ色）。
func write565PNG(b []byte, outPath string) error {
	w, h, pix, err := imgconv.Decode(b)
	if err != nil {
		return err
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, v := range pix {
		r, g, bl := imgconv.Unpack(v)
		img.Set(i%w, i/w, color.RGBA{r, g, bl, 255})
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
