package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// ---------- データの置き場所（/var/lib/lefthand） ----------
//
// ウィジェットのデータ（Todo、テキスト、予定、時刻合わせの記録）を、設定ファイルとは別に置く。
// 設定 GUI で設定を保存しても、ここは変わらない。
// ファイルは名前ごとに 1 つの JSON で、書き込みは一時ファイルと rename で行う（途中で電源が切れても壊れない）。

const defaultDataDir = "/var/lib/lefthand"

// Store は、データのディレクトリ。作れなかったときは、読み書きがエラーになるだけで、デーモンは動き続ける。
type Store struct {
	mu  sync.Mutex
	dir string
	err error // ディレクトリを作れなかった理由
}

// OpenStore は dir を（なければ作って）使う。
func OpenStore(dir string) *Store {
	s := &Store{dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.err = err
		log.Printf("data: %v (widget data will not be saved)", err)
	}
	return s
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".json") }

// Load は name のデータを v に読む。ないときは os.ErrNotExist を包んだエラーを返す。
func (s *Store) Load(name string, v any) error {
	if s == nil {
		return os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", s.path(name), err)
	}
	return nil
}

// Save は v を name のデータとして書く。
func (s *Store) Save(name string, v any) error {
	if s == nil {
		return nil
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return writeFileAtomic(s.path(name), append(b, '\n'), false)
}
