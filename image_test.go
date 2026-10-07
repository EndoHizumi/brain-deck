package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testImage は、座標で色が変わる w×h の画像ファイルを作る（seed で模様を変える）。
func testImage(w, h, seed int) []byte {
	pix := make([]byte, w*h*2)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint16((x*7+seed)%32)<<11 | uint16((y*3+seed*5)%64)<<5 | uint16((x+y+seed)%32)
			binary.LittleEndian.PutUint16(pix[(y*w+x)*2:], v)
		}
	}
	return encodeImage(w, h, pix)
}

// mapImages は、id から画像を返す ImageSource（描画のテスト用）。
type mapImages map[string]*Image

func (m mapImages) Image(id string) *Image { return m[id] }

func mustParse(t *testing.T, b []byte) *Image {
	t.Helper()
	img, err := parseImage(b)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// upload は d に画像 b を image_begin、image_chunk、image_end で送り、id を返す。
func (d *testDaemon) upload(b []byte, name string) string {
	d.t.Helper()
	id, sum := imageIDOf(b)
	img := mustParse(d.t, b)
	r := result(d.t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": img.W, "h": img.H, "name": name, "source": "test"}))
	if r["exists"] == true {
		return id
	}
	tok := r["upload"].(string)
	chunk := int(r["chunk_bytes"].(float64))
	for off := 0; off < len(b); off += chunk {
		end := min(off+chunk, len(b))
		req, _ := json.Marshal(map[string]any{"id": 1, "cmd": "image_chunk", "upload": tok, "offset": off, "data": base64.StdEncoding.EncodeToString(b[off:end])})
		if len(req) > maxLineBytes {
			d.t.Fatalf("an image_chunk line is %d bytes, over the %d-byte line limit", len(req), maxLineBytes)
		}
		if got := result(d.t, d.call("image_chunk", map[string]any{"upload": tok, "offset": off,
			"data": base64.StdEncoding.EncodeToString(b[off:end])}))["received"]; int(got.(float64)) != end {
			d.t.Fatalf("received %v, want %d", got, end)
		}
	}
	if got := result(d.t, d.call("image_end", map[string]any{"upload": tok}))["id"]; got != id {
		d.t.Fatalf("image_end id %v, want %s", got, id)
	}
	return id
}

// parts は、受け取りの途中のファイルの数。
func parts(t *testing.T, dir string) int {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, imagePartGlob))
	return len(m)
}

func TestImageFileFormat(t *testing.T) {
	b := testImage(3, 2, 1)
	img := mustParse(t, b)
	if img.W != 3 || img.H != 2 || len(img.Pix) != 12 {
		t.Fatalf("parsed %dx%d %d bytes", img.W, img.H, len(img.Pix))
	}
	if _, err := parseImage(append([]byte("XXXX"), b[4:]...)); err == nil {
		t.Error("wrong magic was accepted")
	}
	if _, err := parseImage(b[:len(b)-1]); err == nil {
		t.Error("short file was accepted")
	}
	for _, sz := range [][2]int{{0, 1}, {801, 1}, {800, 481}, {480, 800}, {800, 480}} {
		err := checkImageSize(sz[0], sz[1])
		ok := sz[0]*sz[1] <= imageMaxPixels && sz[0] >= 1 && sz[1] >= 1 && sz[0] <= 800 && sz[1] <= 800
		if (err == nil) != ok {
			t.Errorf("checkImageSize(%dx%d) = %v", sz[0], sz[1], err)
		}
	}
	if imageMaxBytes != 768008 {
		t.Errorf("imageMaxBytes = %d", imageMaxBytes)
	}
}

func TestImageUploadAndRead(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	b := testImage(500, 300, 3) // 300,008 バイト。4 回に分けて送る
	id := d.upload(b, "テスト\x01画像.png")
	saved, err := os.ReadFile(d.images.path(id))
	if err != nil || !bytes.Equal(saved, b) {
		t.Fatalf("saved file differs (err %v)", err)
	}
	if n := parts(t, d.images.dir); n != 0 {
		t.Fatalf("%d part files left", n)
	}
	l := result(t, d.call("list_images", nil))
	imgs := l["images"].([]any)
	if len(imgs) != 1 {
		t.Fatalf("list_images: %v", l)
	}
	m := imgs[0].(map[string]any)
	if m["id"] != id || m["name"] != "テスト 画像.png" || m["w"] != 500.0 || m["bytes"] != float64(len(b)) || len(m["refs"].([]any)) != 0 {
		t.Fatalf("list entry %v", m)
	}
	if l["total_bytes"] != float64(len(b)) || l["limit_bytes"] != float64(imageQuota) || l["max_image_bytes"] != float64(imageMaxBytes) {
		t.Fatalf("list totals %v", l)
	}
	// 同じ中身は送らなくてよい
	_, sum := imageIDOf(b)
	r := result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 500, "h": 300}))
	if r["exists"] != true || r["id"] != id {
		t.Fatalf("second image_begin: %v", r)
	}
	// get_image で分けて読み戻す
	var back []byte
	for off := 0; ; {
		g := result(t, d.call("get_image", map[string]any{"image": id, "offset": off}))
		data, _ := base64.StdEncoding.DecodeString(g["data"].(string))
		if g["sha256"] != sum || g["bytes"] != float64(len(b)) {
			t.Fatalf("get_image: %v", g["sha256"])
		}
		back = append(back, data...)
		off += len(data)
		if len(data) == 0 || off >= len(b) {
			break
		}
	}
	if !bytes.Equal(back, b) {
		t.Fatal("get_image returned different bytes")
	}
	if c := errCode(d.call("get_image", map[string]any{"image": "0123456789abcdef"})); c != errNotFound {
		t.Fatalf("get_image of a missing image: %s", c)
	}
}

func TestImageUploadHashMismatch(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	b := testImage(100, 100, 1)
	id, sum := imageIDOf(b)
	r := result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 100, "h": 100}))
	bad := append([]byte(nil), b...)
	bad[5000] ^= 0xff
	result(t, d.call("image_chunk", map[string]any{"upload": r["upload"], "offset": 0, "data": base64.StdEncoding.EncodeToString(bad)}))
	if c := errCode(d.call("image_end", map[string]any{"upload": r["upload"]})); c != errHash {
		t.Fatalf("image_end with corrupted data: %s", c)
	}
	if d.images.Has(id) || parts(t, d.images.dir) != 0 {
		t.Fatal("a corrupted upload left a file")
	}
	// 終わった受け取りには、もう送れない
	if c := errCode(d.call("image_chunk", map[string]any{"upload": r["upload"], "offset": 0, "data": "AAAA"})); c != errBadRequest {
		t.Fatalf("chunk after a failed end: %s", c)
	}
}

func TestImageUploadOrderAndSize(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	b := testImage(300, 200, 2)
	_, sum := imageIDOf(b)
	r := result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 300, "h": 200}))
	tok := r["upload"]
	enc := func(p []byte) string { return base64.StdEncoding.EncodeToString(p) }
	// 順番が違えば断るが、受け取りは続く
	if c := errCode(d.call("image_chunk", map[string]any{"upload": tok, "offset": 10, "data": enc(b[10:20])})); c != errBadRequest {
		t.Fatalf("out-of-order chunk: %s", c)
	}
	result(t, d.call("image_chunk", map[string]any{"upload": tok, "offset": 0, "data": enc(b[:1000])}))
	// 足りないまま終えると、捨てる
	if c := errCode(d.call("image_end", map[string]any{"upload": tok})); c != errBadRequest {
		t.Fatalf("short image_end: %s", c)
	}
	if parts(t, d.images.dir) != 0 {
		t.Fatal("short upload left a part file")
	}
	// 言った大きさより多く送ると、捨てる
	r = result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 300, "h": 200}))
	tok = r["upload"]
	off := 0
	for off+imageChunkMax <= len(b) {
		result(t, d.call("image_chunk", map[string]any{"upload": tok, "offset": off, "data": enc(b[off : off+imageChunkMax])}))
		off += imageChunkMax
	}
	extra := append(append([]byte(nil), b[off:]...), 1, 2, 3)
	if c := errCode(d.call("image_chunk", map[string]any{"upload": tok, "offset": off, "data": enc(extra)})); c != errBadRequest {
		t.Fatalf("oversized data: %s", c)
	}
	if parts(t, d.images.dir) != 0 {
		t.Fatal("oversized upload left a part file")
	}
	// base64 でないデータ
	r = result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 300, "h": 200}))
	if c := errCode(d.call("image_chunk", map[string]any{"upload": r["upload"], "offset": 0, "data": "@@@"})); c != errBadRequest {
		t.Fatalf("bad base64: %s", c)
	}
	if parts(t, d.images.dir) != 0 {
		t.Fatal("bad base64 left a part file")
	}
	// 引数の誤り
	for _, p := range []map[string]any{
		{"sha256": "xyz", "bytes": len(b), "w": 300, "h": 200},
		{"sha256": sum, "bytes": len(b) - 1, "w": 300, "h": 200},
		{"sha256": sum, "bytes": imageHeader + 801*2, "w": 801, "h": 1},
		{"sha256": sum, "bytes": imageHeader + 800*481*2, "w": 800, "h": 481},
	} {
		if c := errCode(d.call("image_begin", p)); c != errBadRequest {
			t.Errorf("image_begin %v: %s", p, c)
		}
	}
}

func TestImageUploadAbortAndDisconnect(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	b := testImage(300, 200, 4)
	_, sum := imageIDOf(b)
	r := result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 300, "h": 200}))
	result(t, d.call("image_chunk", map[string]any{"upload": r["upload"], "offset": 0, "data": base64.StdEncoding.EncodeToString(b[:5000])}))
	if parts(t, d.images.dir) != 1 {
		t.Fatal("no part file while receiving")
	}
	if a := result(t, d.call("image_abort", map[string]any{"upload": r["upload"]})); a["aborted"] != true || parts(t, d.images.dir) != 0 {
		t.Fatalf("image_abort: %v, %d part files", a, parts(t, d.images.dir))
	}
	// 別の image_begin は、前の受け取りをやめる
	r1 := result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 300, "h": 200}))
	r2 := result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 300, "h": 200}))
	if c := errCode(d.call("image_chunk", map[string]any{"upload": r1["upload"], "offset": 0, "data": "AAAA"})); c != errBadRequest || parts(t, d.images.dir) != 1 {
		t.Fatalf("the replaced upload still works (%s) or files are left (%d)", c, parts(t, d.images.dir))
	}
	_ = r2
	// 接続が切れたら、途中のファイルを消す
	d.conn.Close()
	select {
	case <-d.served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if n := parts(t, d.images.dir); n != 0 {
		t.Fatalf("%d part files left after the connection closed", n)
	}
}

func TestImageUploadIdleTimeout(t *testing.T) {
	old := imageUploadIdle
	imageUploadIdle = 50 * time.Millisecond
	defer func() { imageUploadIdle = old }()
	d := newTestDaemon(t, testConfig)
	b := testImage(50, 50, 5)
	_, sum := imageIDOf(b)
	r := result(t, d.call("image_begin", map[string]any{"sha256": sum, "bytes": len(b), "w": 50, "h": 50}))
	time.Sleep(200 * time.Millisecond)
	if parts(t, d.images.dir) != 0 {
		t.Fatal("an idle upload was not discarded")
	}
	if c := errCode(d.call("image_chunk", map[string]any{"upload": r["upload"], "offset": 0, "data": base64.StdEncoding.EncodeToString(b)})); c != errBadRequest {
		t.Fatalf("chunk after timeout: %s", c)
	}
}

func TestImageQuotaAndFreeSpace(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	a := testImage(200, 200, 1) // 80,008 バイト
	d.upload(a, "a")
	b := testImage(200, 200, 2)
	_, sum := imageIDOf(b)
	begin := map[string]any{"sha256": sum, "bytes": len(b), "w": 200, "h": 200}

	oldQ := imageQuota
	imageQuota = int64(len(a) + len(b) - 1)
	m := d.call("image_begin", begin)
	imageQuota = oldQ
	if c := errCode(m); c != errQuota {
		t.Fatalf("over the quota: %s %v", c, m)
	}

	oldF := imageFreeSpace
	imageFreeSpace = func(string) (int64, error) { return imageReserve + int64(len(b)) - 1, nil }
	m = d.call("image_begin", begin)
	imageFreeSpace = oldF
	if c := errCode(m); c != errNoSpace || !strings.Contains(m["error"].(map[string]any)["message"].(string), "SD card") {
		t.Fatalf("low free space: %s %v", c, m)
	}
	if parts(t, d.images.dir) != 0 || d.images.Has(sum[:16]) {
		t.Fatal("a refused upload left a file")
	}
	d.upload(b, "b") // 上限の中なら保存できる
}

func TestImagePruneAndRefs(t *testing.T) {
	d := newTestDaemon(t, testConfig)
	used := d.upload(testImage(10, 10, 1), "used")
	wall := d.upload(testImage(10, 10, 2), "wall")
	kept := d.upload(testImage(10, 10, 3), "kept")
	unused := d.upload(testImage(10, 10, 4), "unused")
	cfg := d.currentJSON()
	l0 := cfg["layers"].([]any)[0].(map[string]any)
	touch := l0["touch"].(map[string]any)
	touch["background"] = wall
	touch["cells"].(map[string]any)["0,0"].(map[string]any)["background"] = used
	res := result(t, d.call("set_config", map[string]any{"config": cfg}))
	if w := res["warnings"].([]any); len(w) == 0 || !strings.Contains(w[0].(string), "but the area is") {
		t.Fatalf("size mismatch warnings: %v", w) // 10x10 の画像はセルの大きさと違う
	}
	l := result(t, d.call("list_images", nil))
	refs := map[string]int{}
	for _, x := range l["images"].([]any) {
		m := x.(map[string]any)
		refs[m["id"].(string)] = len(m["refs"].([]any))
	}
	if refs[used] != 1 || refs[wall] != 1 || refs[unused] != 0 {
		t.Fatalf("refs %v", refs)
	}
	dry := result(t, d.call("prune_images", map[string]any{"dry_run": true, "keep": []string{kept}}))
	if rm := dry["removed"].([]any); len(rm) != 1 || rm[0] != unused || !d.images.Has(unused) {
		t.Fatalf("dry run: %v", dry)
	}
	p := result(t, d.call("prune_images", map[string]any{"keep": []string{kept}}))
	if rm := p["removed"].([]any); len(rm) != 1 || rm[0] != unused || d.images.Has(unused) || !d.images.Has(kept) || !d.images.Has(used) {
		t.Fatalf("prune: %v", p)
	}
	if p["freed_bytes"] != float64(imageHeader+200) {
		t.Fatalf("freed %v", p["freed_bytes"])
	}
	idx, _ := os.ReadFile(filepath.Join(d.images.dir, imageIndexFile))
	if strings.Contains(string(idx), unused) || !strings.Contains(string(idx), `"name": "kept"`) {
		t.Fatalf("index.json after prune: %s", idx)
	}
	// 設定にあるのに Brain にない画像
	os.Remove(d.images.path(used))
	if miss := result(t, d.call("list_images", nil))["missing"].([]any); len(miss) != 1 || miss[0] != used {
		t.Fatalf("missing %v", miss)
	}
	v := result(t, d.call("validate", map[string]any{"config": cfg}))
	if !strings.Contains(strings.Join(toStrings(v["warnings"]), "\n"), "is not on this Brain") {
		t.Fatalf("validate warnings %v", v["warnings"])
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestOpenImageStoreRemovesParts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".upload-abc.part"), []byte("half"), 0o644)
	os.WriteFile(filepath.Join(dir, "0123456789abcdef.565"), testImage(2, 2, 1), 0o644)
	s := OpenImageStore(dir)
	if parts(t, dir) != 0 || !s.Has("0123456789abcdef") {
		t.Fatal("OpenImageStore did not clean up part files, or removed an image")
	}
}

func TestImageCache(t *testing.T) {
	dir := t.TempDir()
	s := NewImageStore(dir)
	var ids []string
	for i := 0; i < 3; i++ {
		b := testImage(100, 100, i) // 20,008 バイト
		id, _ := imageIDOf(b)
		os.WriteFile(s.path(id), b, 0o644)
		ids = append(ids, id)
	}
	old := imageCacheBytes
	imageCacheBytes = 45000 // 2 枚まで
	defer func() { imageCacheBytes = old }()
	a := s.Image(ids[0])
	if a == nil || s.Image(ids[0]) != a {
		t.Fatal("not cached")
	}
	s.Image(ids[1])
	s.Image(ids[0]) // ids[1] がいちばん古くなる
	s.Image(ids[2])
	if s.CacheBytes() > imageCacheBytes {
		t.Fatalf("cache %d over the limit", s.CacheBytes())
	}
	if _, ok := s.cache[ids[1]]; ok {
		t.Fatal("the least recently used image was kept")
	}
	if s.Image(ids[0]) != a {
		t.Fatal("a recently used image was evicted")
	}
	if s.Image("0123456789abcdef") != nil || s.Image("../etc/passwd") != nil {
		t.Fatal("missing or invalid ids must return nil")
	}
}

func TestConfigBackgroundValidation(t *testing.T) {
	const id = "0123456789abcdef"
	ok := `
touch: { min_x: 0, max_x: 100, min_y: 0, max_y: 100, soft_areas: { home: { x: [0, 1], y: [0, 1] } } }
layers:
  - name: base
    touch:
      cols: 2
      rows: 1
      background: ` + id + `
      cells:
        "0,0": { key: A, background: ` + id + ` }
        "1,0": { widget: clock, background: ` + id + ` }
`
	if _, _, err := compileYAML(t, ok); err != nil {
		t.Fatalf("valid backgrounds: %v", err)
	}
	for name, src := range map[string]string{
		"bad cell id":   strings.Replace(ok, `key: A, background: `+id, `key: A, background: ABC`, 1),
		"bad wallpaper": strings.Replace(ok, `      background: `+id, `      background: nope`, 1),
		"on a key": ok + `    keys:
      KEY_A: { key: B, background: ` + id + ` }
`,
		"on a soft key": ok + `    soft_keys:
      home: { key: B, background: ` + id + ` }
`,
	} {
		if _, _, err := compileYAML(t, src); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 書き出して読み直しても同じ
	cfg, err := parseConfig([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	y, err := marshalConfig(cfg)
	if err != nil || !strings.Contains(string(y), "background: "+id) {
		t.Fatalf("marshal: %v\n%s", err, y)
	}
	if _, _, err := compileYAML(t, string(y)); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}

func TestWallpaperFollowsGridTransparency(t *testing.T) {
	src := `
touch: { min_x: 0, max_x: 100, min_y: 0, max_y: 100 }
layers:
  - name: base
    touch: { cols: 2, rows: 2, background: aaaaaaaaaaaaaaaa, cells: { "0,0": A } }
    keys: { KEY_A: { layer_hold: same }, KEY_B: { layer_hold: own }, KEY_C: { layer_hold: small }, KEY_D: { layer_hold: nogrid } }
  - name: same
    touch: { cells: { "1,1": B } }
  - name: own
    touch: { background: bbbbbbbbbbbbbbbb }
  - name: small
    touch: { cols: 1, rows: 1 }
  - name: nogrid
    keys: { KEY_Q: B }
`
	km, _, err := compileYAML(t, src)
	if err != nil {
		t.Fatal(err)
	}
	for li, want := range []string{"aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "", "aaaaaaaaaaaaaaaa"} {
		st := []int{0}
		if li > 0 {
			st = append(st, li)
		}
		if got := km.view(st).Wallpaper; got != want {
			t.Errorf("layer %s: wallpaper %q, want %q", km.Layers[li].Name, got, want)
		}
	}
}

// bgLayout は、背景画像の描画のテスト用の格子。壁紙、セルの画像、大きさの違う画像、ない画像を含む。
func bgLayout(w, h int, press string) (*Layout, mapImages) {
	l := pressTestLayout(w, h, press)
	box := l.rect(0, 0).Inset(cellGap)
	imgs := mapImages{
		"aaaaaaaaaaaaaaaa": nil,
		"bbbbbbbbbbbbbbbb": nil,
		"cccccccccccccccc": nil,
	}
	wall, _ := parseImage(testImage(w, h, 1))
	cell, _ := parseImage(testImage(box.Dx(), box.Dy(), 2))
	small, _ := parseImage(testImage(box.Dx()-40, box.Dy()-30, 3))
	imgs["aaaaaaaaaaaaaaaa"], imgs["bbbbbbbbbbbbbbbb"], imgs["cccccccccccccccc"] = wall, cell, small
	l.Wallpaper = "aaaaaaaaaaaaaaaa"
	l.Cells[0].Background = "bbbbbbbbbbbbbbbb"
	l.Cells[3].Background = "cccccccccccccccc"  // 札に重なるセル。小さい画像（まわりは壁紙）
	l.Cells[11].Background = "dddddddddddddddd" // ない画像（壁紙だけ）
	l.Images = imgs
	return l, imgs
}

func px565(img *Image, x, y int) RGB {
	c := rgb565.unpack(uint32(binary.LittleEndian.Uint16(img.Pix[(y*img.W+x)*2:])))
	return RGB{c.R, c.G, c.B}
}

func TestDrawBackground(t *testing.T) {
	cv := NewCanvas(800, 480, 1600, rgb565, 0)
	l, imgs := bgLayout(cv.W, cv.H, pressBorder)
	drawAll(cv, l, make([]bool, len(l.Cells)))
	got := cv.Image()
	at := func(x, y int) RGB { c := got.RGBAAt(x, y); return RGB{c.R, c.G, c.B} }
	wall, cell, small := imgs["aaaaaaaaaaaaaaaa"], imgs["bbbbbbbbbbbbbbbb"], imgs["cccccccccccccccc"]

	// セル 0,0 の箱の中（枠の内側、文字のない隅）は、セルの画像
	box := l.rect(0, 0).Inset(cellGap)
	for _, p := range []image.Point{{box.Min.X + borderW, box.Min.Y + borderW}, {box.Max.X - borderW - 1, box.Max.Y - 30}} {
		if at(p.X, p.Y) != px565(cell, p.X-box.Min.X, p.Y-box.Min.Y) {
			t.Errorf("cell image pixel at %v = %v", p, at(p.X, p.Y))
		}
	}
	// セルとセルの隙間は壁紙
	gx, gy := l.rect(1, 1).Min.X+1, l.rect(1, 1).Min.Y+1
	if at(gx, gy) != px565(wall, gx, gy) {
		t.Errorf("gap pixel = %v, want the wallpaper", at(gx, gy))
	}
	// 背景画像のないセル（1,1）は、箱の中も壁紙
	b11 := l.rect(1, 1).Inset(cellGap)
	if p := (image.Point{b11.Min.X + 3, b11.Min.Y + 3}); at(p.X, p.Y) != px565(wall, p.X, p.Y) {
		t.Errorf("cell without its own image shows %v, want the wallpaper", at(p.X, p.Y))
	}
	// 小さい画像は中央に置き、まわりは壁紙
	b3 := l.rect(3, 0).Inset(cellGap)
	off := image.Pt((b3.Dx()-small.W)/2, (b3.Dy()-small.H)/2)
	if p := b3.Min.Add(off).Add(image.Pt(1, small.H-2)); at(p.X, p.Y) != px565(small, 1, small.H-2) {
		t.Errorf("centered small image at %v = %v", p, at(p.X, p.Y))
	}
	if p := b3.Min.Add(image.Pt(borderW+1, b3.Dy()-borderW-2)); at(p.X, p.Y) != px565(wall, p.X, p.Y) {
		t.Errorf("around the small image at %v = %v, want the wallpaper", p, at(p.X, p.Y))
	}
	// 文字の縁取り：文字の白い点の隣（文字でない点）は黒になっている
	inner := box.Inset(textMargin)
	white, halo := 0, 0
	for y := inner.Min.Y + 1; y < inner.Max.Y-1; y++ {
		for x := inner.Min.X + 1; x < inner.Max.X-1; x++ {
			if at(x, y) != colText {
				continue
			}
			white++
			for _, d := range []image.Point{{-1, 0}, {1, 0}, {0, -1}, {0, 1}, {-1, -1}, {1, 1}} {
				if c := at(x+d.X, y+d.Y); c != colText {
					if c != colHalo {
						t.Fatalf("pixel next to text at %d,%d is %v, not the halo", x+d.X, y+d.Y, c)
					}
					halo++
				}
			}
		}
	}
	if white == 0 || halo == 0 {
		t.Fatalf("no text or no halo (%d, %d)", white, halo)
	}

	// 画像がない（読み込めない）ときは、背景画像を書かなかったのと同じ見た目
	l2 := pressTestLayout(cv.W, cv.H, pressBorder)
	l2.Cells[0].Background, l2.Wallpaper, l2.Images = "eeeeeeeeeeeeeeee", "ffffffffffffffff", mapImages{}
	a := NewCanvas(800, 480, 1600, rgb565, 0)
	drawAll(a, l2, make([]bool, 12))
	b := NewCanvas(800, 480, 1600, rgb565, 0)
	drawAll(b, pressTestLayout(cv.W, cv.H, pressBorder), make([]bool, 12))
	if !bytes.Equal(a.pix, b.pix) {
		t.Fatal("missing images must draw as if there were no background")
	}
}

// 背景画像があっても、枠だけの描き直しは全体の描き直しと同じになる（4 つの回転、border と fill）。
func TestPressWithBackgroundMatchesFullRedraw(t *testing.T) {
	for _, press := range []string{pressBorder, pressFill} {
		for _, rot := range []int{0, 90, 180, 270} {
			cv := NewCanvas(800, 480, 1600, rgb565, rot)
			l, _ := bgLayout(cv.W, cv.H, press)
			state := make([]bool, len(l.Cells))
			drawAll(cv, l, state)
			ref := NewCanvas(800, 480, 1600, rgb565, rot)
			rng := rand.New(rand.NewSource(int64(rot)))
			for step := 0; step < 40; step++ {
				i := rng.Intn(len(l.Cells))
				state[i] = !state[i]
				if press == pressFill {
					drawCell(cv, l, i%l.Cols, i/l.Cols, state[i])
				} else {
					drawPress(cv, l, i%l.Cols, i/l.Cols, state[i])
				}
				drawAll(ref, l, state)
				if !bytes.Equal(cv.pix, ref.pix) {
					t.Fatalf("%s rot %d step %d cell %d: incremental redraw differs from full redraw", press, rot, step, i)
				}
			}
		}
	}
}

// 回転した画面でも、画像は論理座標の向きで描く（RGB565 以外の画面でも同じ色）。
func TestBlitImageRotatedAndOtherFormats(t *testing.T) {
	img := mustParse(t, testImage(30, 20, 7))
	dst := image.Rect(5, 7, 35, 27)
	ref := NewCanvas(100, 60, 200, rgb565, 0)
	ref.blitImage(img, dst, image.Rect(0, 0, 100, 60))
	want := ref.Image()
	rgb888 := PixelFormat{Bpp: 4, R: bitfield{16, 8}, G: bitfield{8, 8}, B: bitfield{0, 8}}
	for _, c := range []*Canvas{NewCanvas(60, 100, 120, rgb565, 90), NewCanvas(100, 60, 200, rgb565, 180), NewCanvas(100, 60, 400, rgb888, 0)} {
		c.blitImage(img, dst, image.Rect(0, 0, c.W, c.H))
		got := c.Image()
		for y := dst.Min.Y; y < dst.Max.Y; y++ {
			for x := dst.Min.X; x < dst.Max.X; x++ {
				if got.RGBAAt(x, y) != want.RGBAAt(x, y) {
					t.Fatalf("rot %d bpp %d: pixel %d,%d = %v, want %v", c.rot, c.pf.Bpp, x, y, got.RGBAAt(x, y), want.RGBAAt(x, y))
				}
			}
		}
	}
}

// 背景画像のあるセルの描き直しの時間（メモリ上。実機の値は README に書く）。
func BenchmarkPressRingBackground(b *testing.B) {
	for _, bg := range []bool{false, true} {
		name := "plain"
		if bg {
			name = "background"
		}
		b.Run(name, func(b *testing.B) {
			cv := NewCanvas(800, 480, 1600, rgb565, 0)
			l := pressTestLayout(cv.W, cv.H, pressBorder)
			if bg {
				l, _ = bgLayout(cv.W, cv.H, pressBorder)
			}
			drawAll(cv, l, make([]bool, 12))
			for i := 0; i < b.N; i++ {
				drawPress(cv, l, 0, 0, i%2 == 0)
			}
		})
	}
}

func BenchmarkDrawAllBackground(b *testing.B) {
	for _, bg := range []bool{false, true} {
		name := "plain"
		if bg {
			name = "background"
		}
		b.Run(name, func(b *testing.B) {
			cv := NewCanvas(800, 480, 1600, rgb565, 0)
			l := pressTestLayout(cv.W, cv.H, pressBorder)
			if bg {
				l, _ = bgLayout(cv.W, cv.H, pressBorder)
			}
			st := make([]bool, 12)
			for i := 0; i < b.N; i++ {
				drawAll(cv, l, st)
			}
		})
	}
}

func TestImagePreload(t *testing.T) {
	dir := t.TempDir()
	s := NewImageStore(dir)
	var ids []string
	for i := 0; i < 3; i++ {
		b := testImage(50, 50, i)
		id, _ := imageIDOf(b)
		os.WriteFile(s.path(id), b, 0o644)
		ids = append(ids, id)
	}
	cfg := &Config{Layers: []LayerConfig{
		{Name: "base", Touch: &GridConfig{Background: ids[1], Cells: map[string]ActionSpec{"1,0": {Key: "A", Background: ids[2]}, "0,0": {Key: "B", Background: ids[1]}}}},
		{Name: "x", Touch: &GridConfig{Cells: map[string]ActionSpec{"0,0": {Key: "C", Background: ids[0]}}}},
	}}
	order := preloadOrder(cfg)
	if strings.Join(order, ",") != strings.Join([]string{ids[1], ids[2], ids[0]}, ",") {
		t.Fatalf("preload order %v", order)
	}
	s.Preload(order)
	deadline := time.Now().Add(5 * time.Second)
	for s.CacheBytes() < 3*(imageHeader+50*50*2) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.CacheBytes() != 3*(imageHeader+50*50*2) {
		t.Fatalf("preloaded %d bytes", s.CacheBytes())
	}
}
