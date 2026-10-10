package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------- 読み物（/var/lib/lefthand/reader） ----------
//
// PC（brain-deck read add）が、青空文庫のテキスト、EPUB、Web の記事を、下の簡単な形（ReadDoc）にまとめて、
// gzip した JSON で送る（upload_* の kind: read）。Brain は確かめて <id>.json.gz に置き、読むときに戻す。
// 重い処理（文字コード、青空文庫の注記、EPUB の HTML、記事の本文の取り出し）は PC で行う。
// id はファイルの SHA-256 の先頭 16 文字。index.json に題名などを置く。
// 合計の上限は readQuota（既定 32 MiB）。SD カードの空きが 64 MiB を下回るときは断る（背景画像と同じ）。

const (
	readIndexFile   = "index.json"
	readExt         = ".json.gz"
	readMaxFileGz   = 8 << 20  // 1 つのファイル（gzip）の上限
	readMaxPlain    = 32 << 20 // 戻したときの上限
	readMaxBlocks   = 200000
	readTitleMax    = 200
	readDefaultSize = 24
)

var readQuota int64 = 32 << 20

// ReadDoc は 1 つの読み物。
type ReadDoc struct {
	Format int       `json:"format"` // 1
	Title  string    `json:"title"`
	Author string    `json:"author,omitempty"`
	Kind   string    `json:"kind"`             // aozora、epub、web、rss、text
	Source string    `json:"source,omitempty"` // 取り込んだファイルの名前か URL
	Flow   string    `json:"flow,omitempty"`   // book（段落のあいだを空けない。既定）か article（段落のあいだを 1 行空ける）
	Date   string    `json:"date,omitempty"`   // 記事の日付など（表示だけ）
	Blocks []RBlock  `json:"blocks"`
	Added  time.Time `json:"added,omitempty"` // Brain が受け取った時刻（index に書く）
}

// RBlock は、段落、見出し、改ページ。
type RBlock struct {
	T      string `json:"t"`           // p（段落）、h（見出し）、pb（改ページ）、info（底本などの情報）
	L      int    `json:"l,omitempty"` // 見出しの大きさ（1 が大）
	Indent int    `json:"i,omitempty"` // 字下げ（文字の数。すべての行）
	R      []RRun `json:"r,omitempty"`
}

// RRun は、段落の中の文字の続き。
type RRun struct {
	S    string `json:"s"`
	Ruby string `json:"ruby,omitempty"` // S の上（縦書きでは右）に付けるルビ
	Em   bool   `json:"em,omitempty"`   // 傍点
	TCY  bool   `json:"tcy,omitempty"`  // 縦中横（縦書きで、横に並べて 1 文字の箱に入れる）
}

// validate は、受け取った読み物を確かめる。
func (d *ReadDoc) validate() error {
	if d.Format != 1 {
		return fmt.Errorf("format must be 1, got %d", d.Format)
	}
	if strings.TrimSpace(d.Title) == "" || utf8.RuneCountInString(d.Title) > readTitleMax {
		return fmt.Errorf("title must be 1..%d characters", readTitleMax)
	}
	if len(d.Blocks) == 0 || len(d.Blocks) > readMaxBlocks {
		return fmt.Errorf("blocks must be 1..%d", readMaxBlocks)
	}
	for i, b := range d.Blocks {
		switch b.T {
		case "p", "h", "info":
		case "pb":
			if len(b.R) > 0 {
				return fmt.Errorf("blocks[%d]: a page break has no text", i)
			}
		default:
			return fmt.Errorf("blocks[%d]: unknown block type %q (p, h, pb, info)", i, b.T)
		}
		if b.Indent < 0 || b.Indent > 20 {
			return fmt.Errorf("blocks[%d]: indent must be 0..20", i)
		}
	}
	return nil
}

// readMeta は index.json の 1 つ。
type readMeta struct {
	Title  string    `json:"title"`
	Author string    `json:"author,omitempty"`
	Kind   string    `json:"kind"`
	Source string    `json:"source,omitempty"`
	Date   string    `json:"date,omitempty"`
	Bytes  int64     `json:"bytes"`
	Chars  int       `json:"chars"`
	Added  time.Time `json:"added"`
}

// ReadInfo は list_reads の 1 つ。
type ReadInfo struct {
	ID string `json:"id"`
	readMeta
	Pos      *ReadPos `json:"pos,omitempty"`      // しおり（読みかけの位置）
	Progress float64  `json:"progress,omitempty"` // しおりの位置（0〜1。文字の数で）
}

// ReadPos は、読み物の中の位置（ブロックの番号と、その中の文字の番号）。
type ReadPos struct {
	B int `json:"b"`
	C int `json:"c"`
}

func (p ReadPos) less(q ReadPos) bool { return p.B < q.B || p.B == q.B && p.C < q.C }

// ReadStore は、読み物の置き場所と、しおり、読み方の設定。
type ReadStore struct {
	dir string

	mu     sync.Mutex
	idx    map[string]readMeta
	state  readState
	cache  map[string]*ReadDoc // 戻した読み物（今読んでいるものだけ）
	saveAt *time.Timer
}

// readState は reader.json（しおりと設定）。
type readState struct {
	Pos  map[string]ReadPos `json:"pos"`
	Prog map[string]float64 `json:"progress,omitempty"` // しおりの進み具合（0〜1）
	Size int                `json:"size,omitempty"`     // 24 か 32
	Dir  string             `json:"dir,omitempty"`      // v（縦書き。既定）か h
	Open string             `json:"open,omitempty"`     // 開いている読み物
}

// OpenReadStore は dir を使う（ディレクトリは足すときに作る）。
func OpenReadStore(dir string) *ReadStore {
	s := &ReadStore{dir: dir, idx: map[string]readMeta{}, cache: map[string]*ReadDoc{}}
	removeParts(dir)
	var x struct {
		Docs map[string]readMeta `json:"docs"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, readIndexFile)); err == nil {
		json.Unmarshal(b, &x)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), readExt)
		if !ok || !validImageID(id) {
			continue
		}
		m, ok := x.Docs[id]
		if !ok {
			fi, _ := e.Info()
			m = readMeta{Title: id, Kind: "?", Added: fi.ModTime().UTC(), Bytes: fi.Size()}
		}
		s.idx[id] = m
	}
	if b, err := os.ReadFile(filepath.Join(dir, "reader.json")); err == nil {
		json.Unmarshal(b, &s.state)
	}
	if s.state.Pos == nil {
		s.state.Pos = map[string]ReadPos{}
	}
	if s.state.Prog == nil {
		s.state.Prog = map[string]float64{}
	}
	return s
}

func (s *ReadStore) path(id string) string { return filepath.Join(s.dir, id+readExt) }

func (s *ReadStore) saveIndexLocked() error {
	b, _ := json.MarshalIndent(map[string]any{"docs": s.idx}, "", "  ")
	return writeFileAtomic(filepath.Join(s.dir, readIndexFile), append(b, '\n'), false)
}

// saveStateLocked は、しおりと設定を少しあとに書く（ページをめくるたびに SD カードに書かない）。
func (s *ReadStore) saveStateLocked() {
	if s.saveAt != nil {
		return
	}
	s.saveAt = time.AfterFunc(5*time.Second, s.FlushState)
}

// FlushState は、しおりと設定を書く（少しあとと、デーモンが終わるとき）。
func (s *ReadStore) FlushState() {
	s.mu.Lock()
	s.saveAt = nil
	b, _ := json.MarshalIndent(s.state, "", "  ")
	s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	if err := writeFileAtomic(filepath.Join(s.dir, "reader.json"), append(b, '\n'), false); err != nil {
		log.Printf("reader: %v", err)
	}
}

// List は、足した順の一覧。
func (s *ReadStore) List() []ReadInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ReadInfo, 0, len(s.idx))
	for id, m := range s.idx {
		in := ReadInfo{ID: id, readMeta: m}
		if p, ok := s.state.Pos[id]; ok {
			pp := p
			in.Pos = &pp
			in.Progress = s.state.Prog[id]
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Added.Equal(out[j].Added) {
			return out[i].Added.After(out[j].Added) // 新しいものを先に
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// TotalBytes は、置いている読み物の合計。
func (s *ReadStore) TotalBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, m := range s.idx {
		n += m.Bytes
	}
	return n
}

var errReadNotFound = errors.New("reading not found")

// Load は、id の読み物を戻す（今読んでいるものだけを覚える）。
func (s *ReadStore) Load(id string) (*ReadDoc, error) {
	s.mu.Lock()
	if d, ok := s.cache[id]; ok {
		s.mu.Unlock()
		return d, nil
	}
	_, ok := s.idx[id]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", errReadNotFound, id)
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	d, err := decodeReadDoc(b)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache = map[string]*ReadDoc{id: d}
	s.mu.Unlock()
	return d, nil
}

func decodeReadDoc(gz []byte) (*ReadDoc, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("not gzip: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(zr, readMaxPlain+1))
	if err != nil {
		return nil, err
	}
	if len(b) > readMaxPlain {
		return nil, fmt.Errorf("the reading is over %d MiB", readMaxPlain>>20)
	}
	var d ReadDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, d.validate()
}

// Delete は id の読み物としおりを消す。
func (s *ReadStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idx[id]; !ok {
		return fmt.Errorf("%w: %s", errReadNotFound, id)
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(s.idx, id)
	delete(s.cache, id)
	delete(s.state.Pos, id)
	delete(s.state.Prog, id)
	if s.state.Open == id {
		s.state.Open = ""
	}
	s.saveIndexLocked()
	s.saveStateLocked()
	log.Printf("reader: removed %s", id)
	return nil
}

// UploadKind は、upload_* の kind: read（gzip した ReadDoc の JSON）。
func (s *ReadStore) UploadKind() UploadKind {
	return UploadKind{Dir: s.dir, MaxBytes: readMaxFileGz,
		Check: func(b UploadBegin) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			var total int64
			for _, m := range s.idx {
				total += m.Bytes
			}
			if total+b.Bytes > readQuota {
				return imageErr(errImageQuota, "the readings would take %s, over the limit of %s (now %s in %d); remove some first (brain-deck read rm)",
					kib(total+b.Bytes), kib(readQuota), kib(total), len(s.idx))
			}
			if err := os.MkdirAll(s.dir, 0o755); err != nil {
				return err
			}
			free, size, err := imageFreeSpace(s.dir)
			if err == nil && free-b.Bytes < reserveFor(size) {
				return imageErr(errImageNoSpace, "the SD card has %s free; saving %s would leave less than %s", kib(free), kib(b.Bytes), kib(reserveFor(size)))
			}
			return nil
		},
		Done: func(b UploadBegin, f *os.File, part string) (any, error) {
			data, err := io.ReadAll(io.NewSectionReader(f, 0, b.Bytes))
			f.Close()
			var d *ReadDoc
			if err == nil {
				d, err = decodeReadDoc(data)
			}
			if err != nil {
				os.Remove(part)
				return nil, imageErr(errImageBad, "not a reading (gzip of a ReadDoc JSON): %v; the upload was discarded", err)
			}
			id := b.SHA256[:imageIDLen]
			if err := os.Rename(part, s.path(id)); err != nil {
				os.Remove(part)
				return nil, err
			}
			chars := 0
			for _, bl := range d.Blocks {
				for _, r := range bl.R {
					chars += utf8.RuneCountInString(r.S)
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.idx[id] = readMeta{Title: d.Title, Author: d.Author, Kind: d.Kind, Source: d.Source, Date: d.Date, Bytes: b.Bytes, Chars: chars, Added: time.Now().UTC()}
			if err := s.saveIndexLocked(); err != nil {
				log.Printf("reader: index: %v", err)
			}
			log.Printf("reader: saved %s %q (%d chars, %d bytes)", id, d.Title, chars, b.Bytes)
			return map[string]any{"id": id, "title": d.Title, "chars": chars}, nil
		}}
}

// Bookmark は id のしおり。SetBookmark で変える。
func (s *ReadStore) Bookmark(id string) ReadPos {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Pos[id]
}

func (s *ReadStore) SetBookmark(id string, p ReadPos, progress float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idx[id]; !ok {
		return
	}
	if s.state.Pos[id] != p {
		s.state.Pos[id] = p
		s.state.Prog[id] = progress
		s.saveStateLocked()
	}
}

// Settings は、文字の大きさと書く向き、開いている読み物。
func (s *ReadStore) Settings() (size int, vertical bool, open string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size = s.state.Size
	if size != 24 && size != 32 {
		size = readDefaultSize
	}
	if _, ok := s.idx[s.state.Open]; !ok {
		s.state.Open = ""
	}
	return size, s.state.Dir != "h", s.state.Open
}

func (s *ReadStore) SetSettings(size int, vertical bool, open string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := "v"
	if !vertical {
		dir = "h"
	}
	if s.state.Size != size || s.state.Dir != dir || s.state.Open != open {
		s.state.Size, s.state.Dir, s.state.Open = size, dir, open
		s.saveStateLocked()
	}
}

// meta は id の情報。
func (s *ReadStore) meta(id string) (readMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.idx[id]
	return m, ok
}
