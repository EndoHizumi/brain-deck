package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------- Todo の項目 ----------
//
// 項目は設定ファイルには書かず、/var/lib/lefthand/todo.json に置く。設定 GUI で設定を保存しても消えない。
// Brain のデータを正とする。項目ごとに ID と更新番号（rev）を持ち、設定 GUI からの書き換えは、
// GUI が見ていた rev と今の rev が違えば conflict で断る（GUI は読み直す）。
// 保存は返事を待たせない（1 つの goroutine がまとめて書く）。Brain で切り替えたことは、通知で GUI に知らせる。

const (
	todoStoreName = "todo"
	todoMaxRunes  = 200 // 1 項目の長さの上限（文字数）。改行は書けない（空白にする）
)

// TodoItem は 1 つの項目。
type TodoItem struct {
	ID        string     `json:"id"`
	Text      string     `json:"text"`
	Done      bool       `json:"done"`
	Rev       uint64     `json:"rev"` // 最後に変えたときの、一覧の rev
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
	Source    string     `json:"source,omitempty"` // 最後に変えた側（gui、brain-deck、brain）
}

// TodoList は一覧の写し。Items は並べた順（GUI での順）で、完了したものも混ざっている。
// 画面には todoOrder の順（未完了のあとに完了）で出す。書き換えるたびに新しいスライスを作るので、写しは変えない。
type TodoList struct {
	Rev   uint64     `json:"rev"` // 一覧が変わるたびに増える
	Items []TodoItem `json:"items"`
}

// todoFile は todo.json の形。
type todoFile struct {
	Rev    uint64     `json:"rev"`
	NextID uint64     `json:"next_id"`
	Items  []TodoItem `json:"items"`
}

// Todo の操作の誤り。プロトコルのエラーの種類に対応する
var (
	errBadTodo      = errors.New("bad todo")
	errTodoNotFound = errors.New("not found")
	errTodoConflict = errors.New("conflict")
)

// todoOrder は、画面と brain-deck todo list に出す順。未完了を並べた順に、そのあとに完了したものを並べた順に。
func todoOrder(items []TodoItem) []TodoItem {
	out := make([]TodoItem, 0, len(items))
	for _, done := range []bool{false, true} {
		for _, it := range items {
			if it.Done == done {
				out = append(out, it)
			}
		}
	}
	return out
}

// TodoService は Todo の一覧を持ち、保存する。
type TodoService struct {
	mu       sync.Mutex
	list     TodoList // Items は書き換えずに差し替える（Snapshot が返したものを変えない）
	nextID   uint64
	store    *Store
	onChange func()         // 描き直し。待たずに返ること
	onNotify func(TodoList) // 設定 GUI への通知。notifier の goroutine から呼ぶ
	dirty    chan struct{}
	notify   chan struct{}
	saved    chan struct{} // テスト用：保存を 1 回終えるたびに送る
}

func NewTodoService(store *Store) *TodoService {
	ts := &TodoService{store: store, nextID: 1, dirty: make(chan struct{}, 1), notify: make(chan struct{}, 1)}
	var f todoFile
	if err := store.Load(todoStoreName, &f); err == nil {
		ts.list.Rev, ts.nextID = f.Rev, max(f.NextID, 1)
		seen := map[string]bool{}
		for _, it := range f.Items {
			if it.ID == "" || seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			ts.list.Items = append(ts.list.Items, it)
			ts.list.Rev = max(ts.list.Rev, it.Rev)
			if n, err := strconv.ParseUint(strings.TrimPrefix(it.ID, "t"), 10, 64); err == nil {
				ts.nextID = max(ts.nextID, n+1)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("todo: %v", err)
	}
	go ts.saver()
	go ts.notifier()
	return ts
}

// SetOnChange は、一覧が変わったときに呼ぶ関数（画面の描き直し）を設定する。
func (ts *TodoService) SetOnChange(f func()) {
	ts.mu.Lock()
	ts.onChange = f
	ts.mu.Unlock()
}

// SetOnNotify は、一覧が変わったときに、変わったあとの一覧を渡す関数（設定 GUI への通知）を設定する。
// 続けて変わったときは、最後の一覧だけを渡すことがある。
func (ts *TodoService) SetOnNotify(f func(TodoList)) {
	ts.mu.Lock()
	ts.onNotify = f
	ts.mu.Unlock()
}

// Snapshot は今の一覧を返す。ロックしてスライスを返すだけなので、描画のたびに呼んでよい。
func (ts *TodoService) Snapshot() TodoList {
	if ts == nil {
		return TodoList{}
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.list
}

// normalizeTodoText は、1 行の項目にそろえる。改行とタブは空白に、前後の空白は除く。空は誤り。
func normalizeTodoText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: text is not valid UTF-8", errBadTodo)
	}
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(s)
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: text contains a control character %U", errBadTodo, r)
		}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: text is empty", errBadTodo)
	}
	if n := utf8.RuneCountInString(s); n > todoMaxRunes {
		return "", fmt.Errorf("%w: text is %d characters (at most %d)", errBadTodo, n, todoMaxRunes)
	}
	return s, nil
}

// TodoEdit は 1 つの項目の書き換え。nil の項目は変えない。
type TodoEdit struct {
	Text *string
	Done *bool
}

// update は、ロックを持って items を書き換える。f が誤りを返したら何も変えない。
func (ts *TodoService) update(f func(items []TodoItem, rev uint64) ([]TodoItem, error)) (TodoList, error) {
	ts.mu.Lock()
	items := append([]TodoItem(nil), ts.list.Items...)
	items, err := f(items, ts.list.Rev+1)
	if err != nil {
		ts.mu.Unlock()
		return TodoList{}, err
	}
	ts.list = TodoList{Rev: ts.list.Rev + 1, Items: items}
	l := ts.list
	cb := ts.onChange
	ts.mu.Unlock()
	for _, ch := range []chan struct{}{ts.dirty, ts.notify} {
		select {
		case ch <- struct{}{}:
		default: // すでに頼んである。受け取る側は、そのときの最新を使う
		}
	}
	if cb != nil {
		cb()
	}
	return l, nil
}

func todoIndex(items []TodoItem, id string) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// find は id の項目を探し、rev を指定していれば一致するかを確かめる。
func findTodo(items []TodoItem, id string, rev *uint64) (int, error) {
	i := todoIndex(items, id)
	if i < 0 {
		return -1, fmt.Errorf("%w: no item %q", errTodoNotFound, id)
	}
	if rev != nil && items[i].Rev != *rev {
		return -1, fmt.Errorf("%w: item %q was changed (rev %d, you had %d)", errTodoConflict, id, items[i].Rev, *rev)
	}
	return i, nil
}

// Add は項目を足す。index は並べた順（Items）での位置で、負か数より大きければ最後。
func (ts *TodoService) Add(text string, index int, now time.Time, source string) (TodoItem, TodoList, error) {
	text, err := normalizeTodoText(text)
	if err != nil {
		return TodoItem{}, TodoList{}, err
	}
	var added TodoItem
	l, err := ts.update(func(items []TodoItem, rev uint64) ([]TodoItem, error) {
		added = TodoItem{ID: "t" + strconv.FormatUint(ts.nextID, 10), Text: text, Rev: rev,
			CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Source: source}
		ts.nextID++
		if index < 0 || index > len(items) {
			index = len(items)
		}
		return append(items[:index], append([]TodoItem{added}, items[index:]...)...), nil
	})
	return added, l, err
}

// Edit は、項目の文と完了を書き換える。rev を指定すれば、その rev のときだけ書き換える。
func (ts *TodoService) Edit(id string, rev *uint64, e TodoEdit, now time.Time, source string) (TodoItem, TodoList, error) {
	var text string
	if e.Text != nil {
		var err error
		if text, err = normalizeTodoText(*e.Text); err != nil {
			return TodoItem{}, TodoList{}, err
		}
	}
	var out TodoItem
	l, err := ts.update(func(items []TodoItem, next uint64) ([]TodoItem, error) {
		i, err := findTodo(items, id, rev)
		if err != nil {
			return nil, err
		}
		it := items[i]
		if e.Text != nil {
			it.Text = text
		}
		if e.Done != nil && *e.Done != it.Done {
			it.Done = *e.Done
			it.DoneAt = nil
			if it.Done {
				t := now.UTC()
				it.DoneAt = &t
			}
		}
		it.Rev, it.UpdatedAt, it.Source = next, now.UTC(), source
		items[i], out = it, it
		return items, nil
	})
	return out, l, err
}

// Toggle は完了を切り替える（Brain で長押ししたとき）。
func (ts *TodoService) Toggle(id string, now time.Time, source string) (TodoItem, error) {
	ts.mu.Lock()
	i := todoIndex(ts.list.Items, id)
	done := i >= 0 && !ts.list.Items[i].Done
	ts.mu.Unlock()
	if i < 0 {
		return TodoItem{}, fmt.Errorf("%w: no item %q", errTodoNotFound, id)
	}
	it, _, err := ts.Edit(id, nil, TodoEdit{Done: &done}, now, source)
	return it, err
}

// Delete は項目を消す。
func (ts *TodoService) Delete(id string, rev *uint64) (TodoList, error) {
	return ts.update(func(items []TodoItem, _ uint64) ([]TodoItem, error) {
		i, err := findTodo(items, id, rev)
		if err != nil {
			return nil, err
		}
		return append(items[:i], items[i+1:]...), nil
	})
}

// Move は項目を、並べた順（Items）の index の位置に移す（ほかの項目をよけたあとの位置）。
func (ts *TodoService) Move(id string, rev *uint64, index int) (TodoList, error) {
	return ts.update(func(items []TodoItem, _ uint64) ([]TodoItem, error) {
		i, err := findTodo(items, id, rev)
		if err != nil {
			return nil, err
		}
		if index < 0 || index >= len(items) {
			return nil, fmt.Errorf("%w: index %d is out of 0..%d", errBadTodo, index, len(items)-1)
		}
		it := items[i]
		items = append(items[:i], items[i+1:]...)
		return append(items[:index], append([]TodoItem{it}, items[index:]...)...), nil
	})
}

// ClearDone は完了した項目をすべて消し、消した数を返す。消すものがなければ一覧は変えない。
func (ts *TodoService) ClearDone() (int, TodoList, error) {
	n := 0
	l, err := ts.update(func(items []TodoItem, _ uint64) ([]TodoItem, error) {
		kept := items[:0]
		for _, it := range items {
			if !it.Done {
				kept = append(kept, it)
			}
		}
		n = len(items) - len(kept)
		if n == 0 {
			return nil, errNothing
		}
		return kept, nil
	})
	if errors.Is(err, errNothing) {
		return 0, ts.Snapshot(), nil
	}
	return n, l, err
}

var errNothing = errors.New("nothing to change")

// saver は、変わるたびに todo.json を書く。書いているあいだの変更は、次の 1 回にまとめる。
func (ts *TodoService) saver() {
	for range ts.dirty {
		ts.mu.Lock()
		f := todoFile{Rev: ts.list.Rev, NextID: ts.nextID, Items: ts.list.Items}
		ts.mu.Unlock()
		if f.Items == nil {
			f.Items = []TodoItem{}
		}
		if err := ts.store.Save(todoStoreName, f); err != nil {
			log.Printf("todo: save %s: %v", todoStoreName, err)
		}
		if ts.saved != nil {
			ts.saved <- struct{}{}
		}
	}
}

// notifier は、変わるたびに設定 GUI に知らせる。保存（SD カード）とは別に動き、保存を待たない。
func (ts *TodoService) notifier() {
	for range ts.notify {
		ts.mu.Lock()
		f, l := ts.onNotify, ts.list
		ts.mu.Unlock()
		if f != nil {
			f(l)
		}
	}
}
