package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ---------- todo ----------
//
// 項目の番号は、Brain の画面と同じ順（未完了のあと、完了）で 1 から数える。`todo list` で確かめられる。
// 番号の代わりに ID（t12 など。`todo list --json` で分かる）も使える。
// 番号で指定した項目は、読んだときの rev を付けて書き換えるので、そのあいだに Brain で変わっていれば書き換えない。

type todoItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Done   bool   `json:"done"`
	Rev    uint64 `json:"rev"`
	Source string `json:"source"`
}

type todoList struct {
	Rev   uint64     `json:"rev"`
	Items []todoItem `json:"items"`
	Shown *bool      `json:"shown"`
}

// ordered は画面と同じ順（未完了を並べた順に、そのあとに完了したもの）。
func (l todoList) ordered() []todoItem {
	var out []todoItem
	for _, done := range []bool{false, true} {
		for _, it := range l.Items {
			if it.Done == done {
				out = append(out, it)
			}
		}
	}
	return out
}

const todoUsage = "todo のあとには list、add、done、undo、edit、rm、clear-done のどれかを書きます（brain-deck --help）"

func todoCommand(o *options, stdin io.Reader) (func(*Client) (string, error), error) {
	a := o.args[1:]
	sub := "list"
	if len(a) > 0 {
		sub, a = a[0], a[1:]
	}
	if o.top && sub != "add" {
		return nil, usageError("--top は todo add だけで使えます")
	}
	if o.json && sub != "list" {
		return nil, usageError("--json は todo list だけで使えます")
	}
	switch sub {
	case "list", "ls":
		if len(a) != 0 {
			return nil, usageError("todo list は引数を取りません")
		}
		return func(c *Client) (string, error) { return listTodo(c, o.json) }, nil
	case "add":
		if len(a) != 1 {
			return nil, usageError("todo add には項目を 1 つの引数で書きます（空白を含むときは \"\" で囲む。- なら標準入力の 1 行ごとに足す）")
		}
		texts := []string{a[0]}
		if a[0] == "-" {
			var err error
			if texts, err = readLines(stdin); err != nil {
				return nil, err
			}
			if len(texts) == 0 {
				return nil, usageError("標準入力に項目がありません")
			}
		}
		return func(c *Client) (string, error) { return addTodo(c, texts, o.top) }, nil
	case "done", "undo":
		if len(a) == 0 {
			return nil, usageError("todo " + sub + " のあとに、項目の番号か ID を書きます（brain-deck todo list で確かめる）")
		}
		return func(c *Client) (string, error) { return setTodoDone(c, a, sub == "done") }, nil
	case "edit":
		if len(a) != 2 {
			return nil, usageError("todo edit <番号> <新しい文> の形で書きます")
		}
		return func(c *Client) (string, error) { return editTodo(c, a[0], a[1]) }, nil
	case "rm", "delete":
		if len(a) == 0 {
			return nil, usageError("todo rm のあとに、項目の番号か ID を書きます")
		}
		return func(c *Client) (string, error) { return removeTodo(c, a) }, nil
	case "clear-done":
		if len(a) != 0 {
			return nil, usageError("todo clear-done は引数を取りません")
		}
		return clearDoneTodo, nil
	}
	return nil, usageError(todoUsage)
}

func readLines(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

func getTodo(c *Client) (todoList, error) {
	var l todoList
	if err := need(c, "get_todo"); err != nil {
		return l, err
	}
	raw, err := c.Call("get_todo", nil, callTimeout())
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(raw, &l)
	return l, err
}

// call は Todo のコマンドを送り、変わったあとの一覧を返す。
func callTodo(c *Client, cmd string, params map[string]any) (todoList, json.RawMessage, error) {
	var l todoList
	if err := need(c, cmd); err != nil {
		return l, nil, err
	}
	params["source"] = "brain-deck"
	raw, err := c.Call(cmd, params, callTimeout())
	if err != nil {
		return l, nil, err
	}
	err = json.Unmarshal(raw, &l)
	return l, raw, err
}

func summary(l todoList) string {
	n := 0
	for _, it := range l.Items {
		if !it.Done {
			n++
		}
	}
	return fmt.Sprintf("未完了 %d 件、完了 %d 件", n, len(l.Items)-n)
}

func listTodo(c *Client, asJSON bool) (string, error) {
	l, err := getTodo(c)
	if err != nil {
		return "", err
	}
	if asJSON {
		b, err := json.MarshalIndent(struct {
			Rev   uint64     `json:"rev"`
			Items []todoItem `json:"items"`
		}{l.Rev, l.ordered()}, "", "  ")
		return string(b), err
	}
	var b strings.Builder
	for i, it := range l.ordered() {
		mark := "[ ]"
		if it.Done {
			mark = "[x]"
		}
		fmt.Fprintf(&b, "%3d %s %s\n", i+1, mark, it.Text)
	}
	if len(l.Items) == 0 {
		b.WriteString("項目はありません\n")
	}
	b.WriteString(summary(l))
	if l.Shown != nil && !*l.Shown {
		b.WriteString("\n注意：今の設定には Todo のセル（widget: todo）がないので、Brain の画面には出ていません")
	}
	return b.String(), nil
}

func addTodo(c *Client, texts []string, top bool) (string, error) {
	var l todoList
	for i, t := range texts {
		p := map[string]any{"text": t}
		if top {
			p["index"] = i // 標準入力から読んだときも、読んだ順に先頭に並べる
		}
		var err error
		if l, _, err = callTodo(c, "todo_add", p); err != nil {
			return "", err
		}
	}
	msg := fmt.Sprintf("%d 件追加しました（%s）", len(texts), summary(l))
	if len(texts) == 1 {
		msg = fmt.Sprintf("追加しました：%s（%s）", strings.TrimSpace(texts[0]), summary(l))
	}
	if l.Shown != nil && !*l.Shown {
		fmt.Fprintln(os.Stderr, "brain-deck: 注意：今の設定には Todo のセル（widget: todo）がありません。項目は Brain に保存しました")
	}
	return msg, nil
}

// resolve は、番号か ID を項目にする。番号は画面と同じ順で 1 から。
func resolve(l todoList, refs []string) ([]todoItem, error) {
	ord := l.ordered()
	var out []todoItem
	seen := map[string]bool{}
	for _, r := range refs {
		var it *todoItem
		if n, err := strconv.Atoi(r); err == nil {
			if n < 1 || n > len(ord) {
				return nil, usageError(fmt.Sprintf("番号 %d の項目はありません（1〜%d。brain-deck todo list で確かめる）", n, len(ord)))
			}
			it = &ord[n-1]
		} else {
			for i := range ord {
				if ord[i].ID == r {
					it = &ord[i]
				}
			}
			if it == nil {
				return nil, usageError(fmt.Sprintf("%q という番号か ID の項目はありません（brain-deck todo list で確かめる）", r))
			}
		}
		if !seen[it.ID] {
			seen[it.ID] = true
			out = append(out, *it)
		}
	}
	return out, nil
}

func setTodoDone(c *Client, refs []string, done bool) (string, error) {
	l, err := getTodo(c)
	if err != nil {
		return "", err
	}
	items, err := resolve(l, refs)
	if err != nil {
		return "", err
	}
	var names []string
	for _, it := range items {
		if l, _, err = callTodo(c, "todo_update", map[string]any{"item": it.ID, "rev": it.Rev, "done": done}); err != nil {
			return "", err
		}
		names = append(names, it.Text)
	}
	verb := "完了にしました"
	if !done {
		verb = "未完了に戻しました"
	}
	return fmt.Sprintf("%s：%s（%s）", verb, strings.Join(names, "、"), summary(l)), nil
}

func editTodo(c *Client, ref, text string) (string, error) {
	l, err := getTodo(c)
	if err != nil {
		return "", err
	}
	items, err := resolve(l, []string{ref})
	if err != nil {
		return "", err
	}
	if _, _, err = callTodo(c, "todo_update", map[string]any{"item": items[0].ID, "rev": items[0].Rev, "text": text}); err != nil {
		return "", err
	}
	return fmt.Sprintf("書き換えました：%s → %s", items[0].Text, strings.TrimSpace(text)), nil
}

func removeTodo(c *Client, refs []string) (string, error) {
	l, err := getTodo(c)
	if err != nil {
		return "", err
	}
	items, err := resolve(l, refs)
	if err != nil {
		return "", err
	}
	var names []string
	for _, it := range items {
		if l, _, err = callTodo(c, "todo_delete", map[string]any{"item": it.ID, "rev": it.Rev}); err != nil {
			return "", err
		}
		names = append(names, it.Text)
	}
	return fmt.Sprintf("消しました：%s（%s）", strings.Join(names, "、"), summary(l)), nil
}

func clearDoneTodo(c *Client) (string, error) {
	l, raw, err := callTodo(c, "todo_clear_done", map[string]any{})
	if err != nil {
		return "", err
	}
	var r struct {
		Removed int `json:"removed"`
	}
	json.Unmarshal(raw, &r)
	return fmt.Sprintf("完了した項目を %d 件消しました（%s）", r.Removed, summary(l)), nil
}
