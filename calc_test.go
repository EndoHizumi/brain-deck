package main

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	evdev "github.com/holoplot/go-evdev"
)

// calcFeed は、ボタンの並び（空白で区切る）を入れ、最後の表示（式か結果）を返す。
func calcFeed(s *CalcState, keys string) string {
	for _, k := range strings.Fields(keys) {
		s.input(k, false)
	}
	if s.Result != "" {
		r, _ := parseRatString(s.Result)
		t, _ := formatRat(r, s.Base, 40, 20)
		return t
	}
	return calcExprText(s.Tokens, s.Base)
}

func TestCalcExact(t *testing.T) {
	for _, c := range []struct{ keys, want string }{
		{"0 . 1 + 0 . 2 =", "0.3"},            // float64 なら 0.30000000000000004
		{"1 / 3 * 3 =", "1"},                  // 10 進の小数なら 0.999…
		{"1 / 3 =", "0.33333333333333333333"}, // 20 桁で丸める
		{"2 / 3 =", "0.66666666666666666667"}, // 四捨五入
		{"1 + 2 * 3 =", "7"},                  // 掛け算が先
		{"( 1 + 2 ) * 3 =", "9"},              // 括弧
		{"( 1 + 2 * ( 3 + 4 =", "15"},         // 閉じていない括弧は閉じる
		{"- 5 * - 2 =", "10"},                 // 単項のマイナス
		{"1 0 - - 3 =", "13"},                 // 続けた演算子は置き換えない：10 − (−3)
		{"1 0 + * 3 =", "30"},                 // + のあとの × は置き換える
		{"7 = + 1 =", "8"},                    // 結果から続ける
		{"7 = 2 =", "2"},                      // 結果のあとの数字は新しい式
		{"1 2 3 4 bs bs =", "12"},             // BS
		{"9 9 ac 4 =", "4"},                   // AC
		{"3 ( 4 + 1 ) =", "15"},               // 3(…) は 3×(…)
		{"0 7 =", "7"},                        // 先頭の 0
		{". 5 + 1 =", "1.5"},                  // 小数点から
		{"1 2 neg + 3 =", "-9"},               // ± は最後の数
		{"1 2 neg neg =", "12"},               // もう一度で戻る
		{"5 = neg", "-5"},                     // 結果の ±
		{"2 * 3 neg =", "-6"},                 // 2×−3
		{"1 0 0 0 0 0 0 0 0 0 0 0 * 1 0 0 0 0 0 0 0 0 0 0 0 =", "10000000000000000000000"},
	} {
		s := newCalcState()
		if got := calcFeed(s, c.keys); got != c.want {
			t.Errorf("%s = %q, want %q (msg %q)", c.keys, got, c.want, s.Msg)
		}
	}
}

func TestCalcErrors(t *testing.T) {
	for _, c := range []struct{ keys, msg string }{
		{"1 / 0 =", "0 で割れません"},
		{"1 + =", "式が終わっていません"},
		{"1 )", "閉じる括弧はいりません"},
		{"+", "演算子の前に数を入れます"},
		{"A", "A〜F は 16 進で使います"},
		{"base base 2", "2 進では 0 と 1 だけを使います"},
		{"hist", "履歴はまだありません"},
		{"type", "入力する結果がありません"},
		{"1 ) ", "閉じる括弧はいりません"},
	} {
		s := newCalcState()
		calcFeed(s, c.keys)
		if s.Msg != c.msg || !s.MsgErr {
			t.Errorf("%s: msg %q (err %v), want %q", c.keys, s.Msg, s.MsgErr, c.msg)
		}
	}
	// 誤りのあとも式は残り、直せる
	s := newCalcState()
	calcFeed(s, "1 / 0 =")
	if got := calcFeed(s, "bs 4 ="); got != "0.25" {
		t.Errorf("fixed: %q", got)
	}
	// とても大きな数は断る
	s = newCalcState()
	keys := "9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9 9"
	calcFeed(s, keys+" =")
	for i := 0; i < 40 && s.Msg == ""; i++ {
		calcFeed(s, "* "+keys+" =")
	}
	if s.Msg != "数が大きすぎます" {
		t.Errorf("huge: msg %q", s.Msg)
	}
}

func TestFormatRat(t *testing.T) {
	r := func(s string) *big.Rat { v, _ := new(big.Rat).SetString(s); return v }
	for _, c := range []struct {
		v          string
		base, n, f int
		want       string
		exact      bool
	}{
		{"1/3", 10, 8, 20, "0.333333", false},
		{"2/3", 10, 8, 20, "0.666667", false},
		{"-2/3", 10, 8, 20, "-0.66667", false},
		{"1/8", 10, 8, 20, "0.125", true},
		{"1/8", 10, 4, 20, "0.13", false}, // ちょうど半分は大きいほうへ
		{"999/1000", 10, 4, 20, "1", false},
		{"-1/1000", 10, 4, 20, "0", false},
		{"255", 16, 10, 20, "FF", true},
		{"-255/16", 16, 10, 20, "-F.F", true},
		{"1/3", 16, 8, 20, "0.555555", false},
		{"5/2", 2, 10, 20, "10.1", true},
		{"1/10", 2, 12, 20, "0.000110011", false},
		{"123456789", 10, 4, 20, "123456789", true}, // 整数の部分は全部
		{"1/3", 10, 80, 12, "0.333333333333", false},
	} {
		got, exact := formatRat(r(c.v), c.base, c.n, c.f)
		if got != c.want || exact != c.exact {
			t.Errorf("formatRat(%s, %d, %d, %d) = %q %v, want %q %v", c.v, c.base, c.n, c.f, got, exact, c.want, c.exact)
		}
	}
}

func TestCalcBases(t *testing.T) {
	s := newCalcState()
	calcFeed(s, "2 5 5 + 1")
	s.input(calcBase, false)
	if s.Base != 16 || calcExprText(s.Tokens, 16) != "FF+1" {
		t.Fatalf("to hex: %d %q", s.Base, calcExprText(s.Tokens, s.Base))
	}
	if got := calcFeed(s, "+ A ="); got != "10A" {
		t.Fatalf("hex = %q", got)
	}
	s.input(calcBase, false) // 結果は 2 進で出す
	if r, _ := parseRatString(s.Result); s.Base != 2 || r.Cmp(big.NewRat(266, 1)) != 0 {
		t.Fatalf("bin %d %v", s.Base, s.Result)
	}
	// 2 進で終わらない小数は、切り替えない
	s = newCalcState()
	calcFeed(s, "0 . 1")
	s.input(calcBase, false)
	if s.Base != 10 || !strings.Contains(s.Msg, "表せない") {
		t.Fatalf("0.1 to hex: base %d msg %q", s.Base, s.Msg)
	}
	s = newCalcState()
	calcFeed(s, "0 . 5")
	s.input(calcBase, false)
	if s.Base != 16 || calcExprText(s.Tokens, 16) != "0.8" {
		t.Fatalf("0.5 to hex: %q", calcExprText(s.Tokens, s.Base))
	}
}

func TestCalcHistory(t *testing.T) {
	s := newCalcState()
	calcFeed(s, "1 + 1 = 2 * 5 =")
	if len(s.History) != 2 || s.History[0].Expr != "2×5" || s.History[1].Value != "2/1" {
		t.Fatalf("history %+v", s.History)
	}
	if got := calcFeed(s, "hist + hist hist"); got != "10+2" {
		t.Fatalf("recall: %q (%q)", got, s.Msg)
	}
	if got := calcFeed(s, "hist+ ="); got != "20" {
		t.Fatalf("recall newer: %q", got)
	}
	for i := 0; i < calcHistoryMax+3; i++ {
		calcFeed(s, "1 =")
	}
	if len(s.History) != calcHistoryMax {
		t.Fatalf("history %d", len(s.History))
	}
}

func TestCalcType(t *testing.T) {
	s := newCalcState()
	calcFeed(s, "1 / 4")
	text, exact := s.input(calcType, false) // = の前でも、計算してから打つ
	if text != "0.25" || !exact || s.Result == "" {
		t.Fatalf("type %q %v", text, exact)
	}
	calcFeed(s, "1 / 3 =")
	if text, exact = s.input(calcType, false); text != "0.333333333333" || exact {
		t.Fatalf("type 1/3 %q %v", text, exact)
	}
	calcFeed(s, "base 2 5 5 neg =")
	if text, _ = s.input(calcType, true); text != "-0x255" {
		t.Fatalf("hex prefix %q", text)
	}
	for _, ok := range []string{"0.25", "-0x1f", "0b101", "ABCDEF"} {
		if !calcTypeable(ok) {
			t.Errorf("%q should be typeable", ok)
		}
	}
	for _, ng := range []string{"", "1+2", "約1", "1,000", "1e5"} {
		if calcTypeable(ng) && ng != "1e5" {
			t.Errorf("%q should not be typeable", ng)
		}
	}
	// US と JIS で同じ位置のキーだけを使う（数字の段、-、.、文字）
	for r, want := range map[rune]byte{'1': 0x1e, '0': 0x27, '-': 0x2d, '.': 0x37, 'a': 0x04, 'F': 0x09, 'x': 0x1b} {
		if c, ok := calcTypeKey(r); !ok || c.Keys[0] != want || c.Mods != 0 {
			t.Errorf("%q -> %+v", r, c)
		}
	}
}

func TestCalcServiceTypesAndSaves(t *testing.T) {
	dir := t.TempDir()
	c := NewCalcService(OpenStore(dir))
	c.saved = make(chan struct{}, 4)
	var typed []string
	c.SetTyper(func(s string) error { typed = append(typed, s); return nil })
	for _, k := range []string{"1", "2", "+", "3", calcType} {
		c.Input("calculator", k, false)
	}
	if len(typed) != 1 || typed[0] != "15" {
		t.Fatalf("typed %v", typed)
	}
	st := c.Snapshot()["calculator"]
	if st.Msg != "入力しました：15" || st.MsgErr {
		t.Fatalf("msg %q", st.Msg)
	}
	<-c.saved
	c.Input("calculator", calcBase, false)
	<-c.saved
	c2 := NewCalcService(OpenStore(dir))
	if s := c2.Snapshot()["calculator"]; s.Base != 16 || len(s.History) != 1 || s.History[0].Expr != "12+3" {
		t.Fatalf("reloaded %+v", s)
	}
}

func TestCalcKeyInput(t *testing.T) {
	for _, c := range []struct {
		code  evdev.EvCode
		shift bool
		want  string
	}{
		{evdev.KEY_Q, false, "1"}, {evdev.KEY_P, false, "0"}, {evdev.KEY_5, false, "5"},
		{evdev.KEY_E, false, "3"}, {evdev.KEY_E, true, "E"}, {evdev.KEY_A, false, "A"}, {evdev.KEY_F, true, "F"},
		{evdev.KEY_L, false, "+"}, {evdev.KEY_MINUS, false, "-"}, {evdev.KEY_X, false, "*"}, {evdev.KEY_SLASH, false, "/"},
		{evdev.KEY_ENTER, false, "="}, {evdev.KEY_ENTER, true, calcType}, {evdev.KEY_BACKSPACE, true, calcAC},
		{evdev.KEY_UP, false, calcHist},
	} {
		if got, ok := calcKeyInput(c.code, c.shift); !ok || got != c.want {
			t.Errorf("%v shift=%v -> %q %v, want %q", c.code, c.shift, got, ok, c.want)
		}
	}
	for _, code := range []evdev.EvCode{evdev.KEY_ESC, evdev.KEY_TAB, evdev.KEY_LEFTALT, evdev.KEY_LEFT, evdev.KEY_PAGEDOWN, evdev.KEY_M} {
		if _, ok := calcKeyInput(code, false); ok {
			t.Errorf("%v should not go to the calculator", code)
		}
	}
}

// Brain のキーが電卓に行くのは、電卓のセルが今の画面にあるあいだだけ。Ctrl を押しているあいだは行かない
func TestCalcKeysOnlyInItsLayer(t *testing.T) {
	cfg, err := parseConfig([]byte(`
touch: { min_x: 0, max_x: 4095, min_y: 0, max_y: 4095 }
layers:
  - name: base
    keys: { KEY_Q: Q, KEY_TAB: { layer_toggle: calc }, KEY_LEFTCTRL: LCTRL }
    touch: { cols: 4, rows: 3, cells: { "0,0": A } }
  - name: calc
    touch: { cells: { "0,0": { widget: calculator, span: [2, 2] } } }
`))
	if err != nil {
		t.Fatal(err)
	}
	km, warns, err := compileKeymap(cfg)
	if err != nil || len(warns) != 0 {
		t.Fatal(err, warns)
	}
	hid := t.TempDir() + "/hidg"
	writeSys(t, "/", map[string]string{strings.TrimPrefix(hid, "/"): ""})
	e := NewEngine(km, &State{hid: NewHIDWriter(hid), active: map[string]Combo{}})
	calc := NewCalcService(OpenStore(t.TempDir()))
	rt := NewWidgetRT(nil)
	rt.calc = calc
	e.SetWidgets(rt)
	tap := func(code evdev.EvCode) { e.PressKey(code); e.ReleaseKey(code) }
	tap(evdev.KEY_Q) // base：PC に Q
	calc.Sync()
	if _, ok := calc.Snapshot()["calculator"]; ok {
		t.Fatal("the key went to the calculator in the base layer")
	}
	tap(evdev.KEY_TAB) // 電卓のレイヤーへ
	tap(evdev.KEY_Q)
	tap(evdev.KEY_L)
	tap(evdev.KEY_W)
	e.PressKey(evdev.KEY_LEFTCTRL)
	tap(evdev.KEY_Q) // Ctrl+Q は PC へ
	e.ReleaseKey(evdev.KEY_LEFTCTRL)
	tap(evdev.KEY_ENTER)
	calc.Sync()
	if s := calc.Snapshot()["calculator"]; s.Result != "3/1" {
		t.Fatalf("calc state %+v", s)
	}
	if len(e.down) != 0 {
		t.Fatalf("keys left down: %v", e.down)
	}
	_ = time.Second
}

func TestControlCalcState(t *testing.T) {
	d := newTestDaemon(t, `
touch: { min_x: 0, max_x: 4095, min_y: 0, max_y: 4095 }
layers:
  - name: base
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { widget: calculator, span: [2, 2] }
`)
	d.calc.Input("calculator", "7", false)
	g := result(t, d.call("get_widgets", nil))
	c := g["calculator"].(map[string]any)["calculator"].(map[string]any)
	if c["base"] != 10.0 || len(c["tokens"].([]any)) != 1 || fmt.Sprint(g["ids"].(map[string]any)["calculator"]) != "[calculator]" {
		t.Fatalf("get_widgets = %v", g)
	}
}
