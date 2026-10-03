package main

// ---------- PW-SH2 の物理キー（設定 GUI の get_keymap） ----------
//
// 内容は docs/keymap-pwsh2.md と同じ。位置は Brainux wiki の keymap.png の並びをもとにした概略で、
// 実機のキーの大きさや間隔とは違う。GUI はこの表のとおりにキーボードを描く。

// PhysKey は本体のキー 1 つ。
type PhysKey struct {
	ID     string  `json:"id"`               // 表の中で一意な名前（調べる と 戻る は同じコードなので、コードとは別に持つ）
	Label  string  `json:"label"`            // 刻印
	Code   string  `json:"code,omitempty"`   // 届く KEY_* 名。空ならイベントが出ない
	Symbol string  `json:"symbol,omitempty"` // 「記号」を押しながらのときに届く KEY_* 名。空なら届かない
	Row    int     `json:"row"`              // 段（0 が上）
	X      float64 `json:"x"`                // 左端の位置（キー 1 つの幅を 1 とする）
	W      float64 `json:"w"`                // 幅
	Note   string  `json:"note,omitempty"`
}

// KeymapInfo は get_keymap の結果。
type KeymapInfo struct {
	Model       string    `json:"model"`
	Keys        []PhysKey `json:"keys"`
	MaxRollover int       `json:"max_rollover"` // ドライバが 1 回に読めるキーの数
	// Blocked は、実機で届かなかった同時押しの組み合わせ（最後のキーが届かない）
	Blocked     [][]string `json:"blocked"`
	Constraints []string   `json:"constraints"` // 人が読む説明
	Screen      struct {
		W int `json:"w"`
		H int `json:"h"`
	} `json:"screen"`
}

func k(id, label, code, symbol string, row int, x float64, note string) PhysKey {
	return PhysKey{ID: id, Label: label, Code: code, Symbol: symbol, Row: row, X: x, W: 1, Note: note}
}

func pwsh2Keymap() KeymapInfo {
	keys := []PhysKey{
		k("power", "電源", "", "", 0, -0.5, "イベントが出ない（カーネルが処理する）"),
		k("search", "調べる", "KEY_ESC", "KEY_ESC", 0, 0.5, "「戻る」と同じコード。区別できない"),
		k("kokugo", "国語", "KEY_TAB", "KEY_TAB", 0, 1.5, ""),
		k("eiwa", "英和/和英", "KEY_PAGEUP", "KEY_PAGEUP", 0, 2.5, "「ページアップ」キーではない"),
		k("mydict", "マイ辞書", "KEY_PAGEDOWN", "KEY_PAGEDOWN", 0, 3.5, ""),
		k("history", "履歴/しおり", "KEY_INSERT", "KEY_INSERT", 0, 4.5, ""),
		k("marker", "マーカーテスト", "KEY_DELETE", "KEY_DELETE", 0, 5.5, ""),
		k("tool", "ツール", "", "", 0, 7, "dts に割り当てがない。イベントが出ない"),
		k("home", "ホーム", "", "", 0, 8, "dts に割り当てがない。イベントが出ない"),
	}
	top := "QWERTYUIOP"
	for i, c := range top {
		sym := "KEY_" + string("1234567890"[i])
		keys = append(keys, k(string(c|0x20), string(c), "KEY_"+string(c), sym, 1, float64(i), ""))
	}
	mid := []struct{ c, sym string }{
		{"A", ""}, {"S", ""}, {"D", "KEY_GRAVE"}, {"F", "KEY_EQUAL"}, {"G", "KEY_BACKSLASH"},
		{"H", "KEY_SEMICOLON"}, {"J", "KEY_APOSTROPHE"}, {"K", "KEY_LEFTBRACE"}, {"L", "KEY_RIGHTBRACE"},
	}
	for i, m := range mid {
		note := ""
		if m.sym == "" {
			note = "「記号」を押しているあいだは届かない"
		}
		keys = append(keys, k(string(m.c[0]|0x20), m.c, "KEY_"+m.c, m.sym, 2, 0.5+float64(i), note))
	}
	keys = append(keys, k("shift", "シフト", "KEY_LEFTSHIFT", "KEY_LEFTSHIFT", 3, 0, ""))
	for i, c := range "ZXCVB" {
		keys = append(keys, k(string(c|0x20), string(c), "KEY_"+string(c), "", 3, 1+float64(i), "「記号」を押しているあいだは届かない"))
	}
	keys = append(keys,
		k("n", "N", "KEY_N", "KEY_COMMA", 3, 6, ""),
		k("m", "M", "KEY_M", "KEY_DOT", 3, 7, ""),
		k("minus", "−", "KEY_MINUS", "KEY_SLASH", 3, 8, ""),
		k("bs", "後退", "KEY_BACKSPACE", "KEY_BACKSPACE", 3, 9, ""),
		k("ctrl", "ページアップ", "KEY_LEFTCTRL", "KEY_LEFTCTRL", 4, 0, "《 を横に倒した記号。wiki の Ctrl"),
		k("alt", "文字切替", "KEY_LEFTALT", "KEY_LEFTALT", 4, 1, "wiki の Alt。layer_hold に向く"),
		k("symbol", "記号", "", "", 4, 2, "ドライバの中で処理する。押しているあいだ、ほかのキーのコードが変わる"),
	)
	space := k("space", "スペース", "KEY_SPACE", "", 4, 3, "「記号」を押しているあいだは届かない")
	space.W = 2
	keys = append(keys, space,
		k("back", "戻る", "KEY_ESC", "", 4, 5, "「調べる」と同じコード。「記号」を押しているあいだは届かない"),
		k("enter", "決定", "KEY_ENTER", "", 4, 6, "「記号」を押しているあいだは届かない"),
		k("left", "←", "KEY_LEFT", "", 4, 7, "「記号」を押しているあいだは届かない"),
		k("up", "↑", "KEY_UP", "", 4, 8, "「記号」を押しているあいだは届かない"),
		k("down", "↓", "KEY_DOWN", "", 4, 9, "「記号」を押しているあいだは届かない"),
		k("right", "→", "KEY_RIGHT", "", 4, 10, "「記号」を押しているあいだは届かない"),
	)
	info := KeymapInfo{
		Model:       "PW-SH2",
		Keys:        keys,
		MaxRollover: 3,
		Blocked:     [][]string{{"KEY_LEFTALT", "KEY_Q", "KEY_W"}},
		Constraints: []string{
			"ドライバは 1 回に最大 3 キーまで読む。キーボードの回路にも、同時押しできない組み合わせがある",
			"文字切り替え + Q を押したまま W を押すと、W が届かなかった。layer_hold のレイヤーで 2 つの文字キーを同時に押す使い方は避ける",
			"「記号」はイベントを出さない。押しているあいだ、Q〜P は KEY_1〜KEY_0、D〜L は記号、N・M・− は , . / として届き、A、S、Z〜B、スペース、決定、戻る、矢印は届かない",
			"調べる と 戻る は同じ KEY_ESC で、区別できない",
			"電源、ツール、ホーム、記号 は割り当てられない",
		},
	}
	info.Screen.W, info.Screen.H = 800, 480
	return info
}
