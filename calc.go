package main

import (
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------- 電卓（widget: calculator）の計算 ----------
//
// 数は有理数（math/big の Rat。分子と分母の整数）で持ち、四則演算は誤差なしで行う。
// 0.1 + 0.2 は 3/10 のまま計算し、1 ÷ 3 × 3 も 1 になる。浮動小数点数（float64）は 2 進の小数なので、
// 10 進の 0.1 を正確に持てず、0.30000000000000004 のような誤差が出る。10 進の小数（decimal）は 1 ÷ 3 を
// 途中で丸めるので、× 3 で 0.999… になる。有理数は割り算でも丸めないので、どちらも起きない。
// 丸めるのは画面に出すときと、PC に打ち込むときだけ（そのときは「約」と出す）。
//
// 式は、入力した順の「トークン」（数、演算子、括弧、前の結果の値）で持つ。数は打った文字のまま持つので、
// 16 進や 2 進で打ったものも、そのまま表示できる。基数を切り替えると、数を新しい基数の文字に書き直す
// （小数が新しい基数で終わらないときは、切り替えない）。
// 計算の履歴（式と結果）は 20 個まで /var/lib/lefthand/calc.json に残す（= を押したとき。返事は書き込みを待たない）。

const (
	calcStoreName  = "calc"
	calcMaxTokens  = 64
	calcMaxDigits  = 32   // 1 つの数の桁数
	calcMaxBits    = 4096 // 分子と分母の大きさの上限（ビット）
	calcHistoryMax = 20
	calcTypeFrac   = 12 // PC に打ち込むときの、小数点以下の桁数の上限
	calcMaxIDs     = 16
	calcDigits     = "0123456789ABCDEF"
)

// 電卓への入力（ボタンとキーの働き）
const (
	calcBS      = "bs"
	calcAC      = "ac"
	calcBase    = "base"
	calcNeg     = "neg"
	calcHist    = "hist"  // 履歴の、ひとつ前の結果を入れる
	calcHistNew = "hist+" // 履歴の、ひとつ新しい結果に戻す
	calcType    = "type"  // 結果を PC に打ち込む
	calcEq      = "="
)

// calcTok は式のトークン 1 つ。
type calcTok struct {
	Kind   string `json:"t"`           // num、op、lp、rp、val
	S      string `json:"s,omitempty"` // num：打った文字（基数の数字と .）。op：+ - * /
	V      string `json:"v,omitempty"` // val：値（"分子/分母"）
	recall int    // val：履歴から入れたなら、その番号（1 が最新）
}

// CalcEntry は履歴 1 つ。Expr は計算したときの式の表示、Value は結果（"分子/分母"）。
type CalcEntry struct {
	Expr  string `json:"expr"`
	Value string `json:"value"`
	Base  int    `json:"base"`
}

// CalcState は、電卓 1 つ（id ごと）の状態。get_widgets と -render-calc の形でもある。
type CalcState struct {
	Base    int         `json:"base"`
	Tokens  []calcTok   `json:"tokens"`
	Result  string      `json:"result,omitempty"` // = のあと：結果（"分子/分母"）。次の入力で式に戻る
	Done    string      `json:"done,omitempty"`   // = のあと：計算した式の表示
	Msg     string      `json:"msg,omitempty"`    // 知らせ（次の入力で消える）
	MsgErr  bool        `json:"msg_err,omitempty"`
	History []CalcEntry `json:"history,omitempty"` // 新しい順
	Rev     uint64      `json:"rev"`
}

func (s *CalcState) clone() CalcState {
	c := *s
	c.Tokens = append([]calcTok(nil), s.Tokens...)
	c.History = append([]CalcEntry(nil), s.History...)
	return c
}

func newCalcState() *CalcState { return &CalcState{Base: 10} }

// ratString は、有理数を "分子/分母" にする（JSON に残す形。GUI は BigInt で読む）。
func ratString(r *big.Rat) string { return r.Num().String() + "/" + r.Denom().String() }

func parseRatString(s string) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func baseName(b int) string {
	switch b {
	case 16:
		return "HEX"
	case 2:
		return "BIN"
	}
	return "DEC"
}

// validDigit は、文字 d がその基数の数字か。
func validDigit(d byte, base int) bool {
	i := strings.IndexByte(calcDigits, d)
	return i >= 0 && i < base
}

// parseLiteral は、打った文字（基数の数字と、. を 1 つまで）を有理数にする。
func parseLiteral(s string, base int) (*big.Rat, error) {
	ip, fp, _ := strings.Cut(s, ".")
	if ip == "" && fp == "" {
		return nil, errCalc("数が正しくありません")
	}
	n := new(big.Int)
	b := big.NewInt(int64(base))
	for i := 0; i < len(ip)+len(fp); i++ {
		var c byte
		if i < len(ip) {
			c = ip[i]
		} else {
			c = fp[i-len(ip)]
		}
		if !validDigit(c, base) {
			return nil, errCalc(fmt.Sprintf("%s 進の数ではありません", map[int]string{10: "10", 16: "16", 2: "2"}[base]))
		}
		n.Mul(n, b)
		n.Add(n, big.NewInt(int64(strings.IndexByte(calcDigits, c))))
	}
	den := new(big.Int).Exp(b, big.NewInt(int64(len(fp))), nil)
	return new(big.Rat).SetFrac(n, den), nil
}

// formatRat は、有理数を基数 base の文字にする。全体（符号と小数点を含む）を maxChars 文字までにし、
// 小数点以下は maxFrac 桁まで。それで終わらなければ、最後の桁で四捨五入し（ちょうど半分は、絶対値の大きいほうへ）、
// exact を false にする。整数の部分だけで maxChars を超えるときも、整数の部分は全部書く。
// gui/src/calc.ts の formatRat と同じ結果にする。
func formatRat(r *big.Rat, base, maxChars, maxFrac int) (string, bool) {
	neg := r.Sign() < 0
	num := new(big.Int).Abs(r.Num())
	den := r.Denom()
	ip, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	is := strings.ToUpper(ip.Text(base))
	signLen := 0
	if neg {
		signLen = 1
	}
	f := min(maxChars-signLen-len(is)-1, maxFrac)
	b := big.NewInt(int64(base))
	var digits []byte
	q := new(big.Int)
	for i := 0; i < f && rem.Sign() != 0; i++ {
		rem.Mul(rem, b)
		q.QuoRem(rem, den, rem)
		digits = append(digits, calcDigits[q.Int64()])
	}
	exact := rem.Sign() == 0
	if !exact {
		// 四捨五入：残り × 2 ≥ 分母 なら、最後の桁に 1 を足す（繰り上がりは整数の部分まで）
		if new(big.Int).Lsh(rem, 1).Cmp(den) >= 0 {
			i := len(digits) - 1
			for ; i >= 0; i-- {
				k := strings.IndexByte(calcDigits, digits[i]) + 1
				if k < base {
					digits[i] = calcDigits[k]
					break
				}
				digits[i] = '0'
			}
			if i < 0 {
				ip.Add(ip, big.NewInt(1))
				is = strings.ToUpper(ip.Text(base))
			}
		}
		for len(digits) > 0 && digits[len(digits)-1] == '0' {
			digits = digits[:len(digits)-1]
		}
	}
	s := is
	if len(digits) > 0 {
		s += "." + string(digits)
	}
	if neg && s != "0" {
		s = "-" + s
	}
	return s, exact
}

type errCalc string

func (e errCalc) Error() string { return string(e) }

// calcParser は、トークンの並びを計算する（再帰下降。掛け算と割り算が先、単項のマイナス、括弧は閉じなくてよい）。
type calcParser struct {
	toks []calcTok
	pos  int
	base int
}

func (p *calcParser) peek() *calcTok {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func checkSize(r *big.Rat) error {
	if r.Num().BitLen() > calcMaxBits || r.Denom().BitLen() > calcMaxBits {
		return errCalc("数が大きすぎます")
	}
	return nil
}

func (p *calcParser) expr() (*big.Rat, error) {
	a, err := p.term()
	if err != nil {
		return nil, err
	}
	for t := p.peek(); t != nil && t.Kind == "op" && (t.S == "+" || t.S == "-"); t = p.peek() {
		p.pos++
		b, err := p.term()
		if err != nil {
			return nil, err
		}
		if t.S == "+" {
			a = new(big.Rat).Add(a, b)
		} else {
			a = new(big.Rat).Sub(a, b)
		}
		if err := checkSize(a); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (p *calcParser) term() (*big.Rat, error) {
	a, err := p.unary()
	if err != nil {
		return nil, err
	}
	for t := p.peek(); t != nil && t.Kind == "op" && (t.S == "*" || t.S == "/"); t = p.peek() {
		p.pos++
		b, err := p.unary()
		if err != nil {
			return nil, err
		}
		if t.S == "*" {
			a = new(big.Rat).Mul(a, b)
		} else {
			if b.Sign() == 0 {
				return nil, errCalc("0 で割れません")
			}
			a = new(big.Rat).Quo(a, b)
		}
		if err := checkSize(a); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (p *calcParser) unary() (*big.Rat, error) {
	if t := p.peek(); t != nil && t.Kind == "op" && t.S == "-" {
		p.pos++
		a, err := p.unary()
		if err != nil {
			return nil, err
		}
		return new(big.Rat).Neg(a), nil
	}
	return p.primary()
}

func (p *calcParser) primary() (*big.Rat, error) {
	t := p.peek()
	if t == nil {
		return nil, errCalc("式が終わっていません")
	}
	p.pos++
	switch t.Kind {
	case "num":
		return parseLiteral(t.S, p.base)
	case "val":
		r, ok := parseRatString(t.V)
		if !ok {
			return nil, errCalc("値が正しくありません")
		}
		return r, nil
	case "lp":
		a, err := p.expr()
		if err != nil {
			return nil, err
		}
		if t := p.peek(); t != nil && t.Kind == "rp" {
			p.pos++
		}
		return a, nil
	}
	return nil, errCalc("式が正しくありません")
}

// evalTokens は式を計算する。
func evalTokens(toks []calcTok, base int) (*big.Rat, error) {
	if len(toks) == 0 {
		return nil, errCalc("式がありません")
	}
	p := &calcParser{toks: toks, base: base}
	r, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(toks) {
		return nil, errCalc("式が正しくありません")
	}
	return r, nil
}

var calcOpText = map[string]string{"+": "+", "-": "−", "*": "×", "/": "÷"}

// calcValText は、式の中の値（前の結果）の表示。終わらない小数は 10 桁で切り、「…」を付ける。
func calcValText(v string, base int) string {
	r, ok := parseRatString(v)
	if !ok {
		return "?"
	}
	s, exact := formatRat(r, base, 24, 10)
	if !exact {
		s += "…"
	}
	return s
}

// calcExprText は、式の表示。gui/src/calc.ts の exprText と同じ。
func calcExprText(toks []calcTok, base int) string {
	var b strings.Builder
	for _, t := range toks {
		switch t.Kind {
		case "num":
			if strings.HasPrefix(t.S, ".") {
				b.WriteString("0")
			}
			b.WriteString(t.S)
		case "op":
			b.WriteString(calcOpText[t.S])
		case "lp":
			b.WriteString("(")
		case "rp":
			b.WriteString(")")
		case "val":
			b.WriteString(calcValText(t.V, base))
		}
	}
	return b.String()
}

func (s *CalcState) last() *calcTok {
	if len(s.Tokens) == 0 {
		return nil
	}
	return &s.Tokens[len(s.Tokens)-1]
}

// before は、最後から n 番目（0 が最後）のトークン。なければ nil。
func (s *CalcState) before(n int) *calcTok {
	if i := len(s.Tokens) - 1 - n; i >= 0 {
		return &s.Tokens[i]
	}
	return nil
}

func (s *CalcState) info(msg string) { s.Msg, s.MsgErr = msg, false }
func (s *CalcState) fail(msg string) { s.Msg, s.MsgErr = msg, true }
func (s *CalcState) fresh() bool     { return s.Result != "" }
func isOperand(t *calcTok) bool {
	return t != nil && (t.Kind == "num" || t.Kind == "val" || t.Kind == "rp")
}
func (s *CalcState) full() bool { return len(s.Tokens) >= calcMaxTokens }
func (s *CalcState) push(t calcTok) bool {
	if s.full() {
		s.fail("式が長すぎます")
		return false
	}
	s.Tokens = append(s.Tokens, t)
	return true
}

// leaveResult は、= のあとの結果から式に戻る。keep なら、結果を式の最初の値にする。
func (s *CalcState) leaveResult(keep bool) {
	if !s.fresh() {
		return
	}
	s.Tokens = nil
	if keep {
		s.Tokens = []calcTok{{Kind: "val", V: s.Result}}
	}
	s.Result, s.Done = "", ""
}

// input は、ボタンかキー 1 つの働き。打ち込む文字があれば返す（calcType）。
func (s *CalcState) input(in string, prefix bool) (typeText string, typeExact bool) {
	s.Msg, s.MsgErr = "", false
	switch {
	case len(in) == 1 && validDigit(in[0], 16):
		s.digit(in[0])
	case in == ".":
		s.dot()
	case in == "+" || in == "-" || in == "*" || in == "/":
		s.op(in)
	case in == "(":
		s.leaveResult(false)
		if isOperand(s.last()) {
			s.push(calcTok{Kind: "op", S: "*"}) // 3(4+5) は 3×(4+5)
		}
		s.push(calcTok{Kind: "lp"})
	case in == ")":
		open := 0
		for _, t := range s.Tokens {
			if t.Kind == "lp" {
				open++
			} else if t.Kind == "rp" {
				open--
			}
		}
		if open <= 0 || !isOperand(s.last()) || s.fresh() {
			s.fail("閉じる括弧はいりません")
			return
		}
		s.push(calcTok{Kind: "rp"})
	case in == calcEq:
		s.equal()
	case in == calcBS:
		if s.fresh() {
			s.leaveResult(false)
			return
		}
		if t := s.last(); t != nil {
			if t.Kind == "num" && len(t.S) > 1 {
				t.S = t.S[:len(t.S)-1]
			} else {
				s.Tokens = s.Tokens[:len(s.Tokens)-1]
			}
		}
	case in == calcAC:
		s.Tokens, s.Result, s.Done = nil, "", ""
	case in == calcBase:
		s.switchBase()
	case in == calcNeg:
		s.negate()
	case in == calcHist || in == calcHistNew:
		s.recall(in == calcHist)
	case in == calcType:
		if !s.fresh() {
			if len(s.Tokens) == 0 {
				s.fail("入力する結果がありません")
				return
			}
			if !s.equal() {
				return
			}
		}
		r, _ := parseRatString(s.Result)
		t, exact := formatRat(r, s.Base, 80, calcTypeFrac)
		if prefix && s.Base != 10 {
			p := map[int]string{16: "0x", 2: "0b"}[s.Base]
			if strings.HasPrefix(t, "-") {
				t = "-" + p + t[1:]
			} else {
				t = p + t
			}
		}
		return t, exact
	}
	return "", false
}

func (s *CalcState) digit(d byte) {
	if !validDigit(d, s.Base) {
		if s.Base == 2 {
			s.fail("2 進では 0 と 1 だけを使います")
		} else {
			s.fail("A〜F は 16 進で使います")
		}
		return
	}
	s.leaveResult(false)
	t := s.last()
	switch {
	case t != nil && t.Kind == "num":
		if len(t.S) >= calcMaxDigits {
			s.fail("桁が多すぎます")
			return
		}
		if t.S == "0" { // 先頭の 0 は書き換える
			t.S = string(d)
			return
		}
		t.S += string(d)
	case isOperand(t):
		s.fail("数のあとには、演算子を入れます")
	default:
		s.push(calcTok{Kind: "num", S: string(d)})
	}
}

func (s *CalcState) dot() {
	s.leaveResult(false)
	t := s.last()
	switch {
	case t != nil && t.Kind == "num":
		if !strings.Contains(t.S, ".") && len(t.S) < calcMaxDigits {
			t.S += "."
		}
	case isOperand(t):
		s.fail("数のあとには、演算子を入れます")
	default:
		s.push(calcTok{Kind: "num", S: "0."})
	}
}

func (s *CalcState) op(o string) {
	s.leaveResult(true)
	t := s.last()
	switch {
	case isOperand(t):
		s.push(calcTok{Kind: "op", S: o})
	case t == nil || t.Kind == "lp":
		if o == "-" {
			s.push(calcTok{Kind: "op", S: "-"}) // 単項のマイナス
		} else {
			s.fail("演算子の前に数を入れます")
		}
	default: // 演算子のあと
		if o == "-" && isOperand(s.before(1)) {
			s.push(calcTok{Kind: "op", S: "-"}) // 二項の演算子のあとの − は単項（3×−2、10−−3）
			return
		}
		// 続けて押した演算子は、前のものを置き換える（3×− のあとの + は、× と − をまとめて置き換える）
		for t := s.last(); t != nil && t.Kind == "op"; t = s.last() {
			s.Tokens = s.Tokens[:len(s.Tokens)-1]
		}
		if t := s.last(); isOperand(t) {
			s.push(calcTok{Kind: "op", S: o})
		} else if o == "-" {
			s.push(calcTok{Kind: "op", S: "-"})
		}
	}
}

// equal は、式を計算して結果にする。できなければ知らせて false。
func (s *CalcState) equal() bool {
	if s.fresh() || len(s.Tokens) == 0 {
		return s.fresh()
	}
	r, err := evalTokens(s.Tokens, s.Base)
	if err != nil {
		s.fail(err.Error())
		return false
	}
	expr := calcExprText(s.Tokens, s.Base)
	s.Done = expr + " ="
	s.Result = ratString(r)
	s.Tokens = nil
	s.History = append([]CalcEntry{{Expr: expr, Value: s.Result, Base: s.Base}}, s.History...)
	if len(s.History) > calcHistoryMax {
		s.History = s.History[:calcHistoryMax]
	}
	return true
}

// switchBase は 10 → 16 → 2 → 10 進と切り替え、式の数を書き直す。
func (s *CalcState) switchBase() {
	nb := map[int]int{10: 16, 16: 2, 2: 10}[s.Base]
	conv := make([]calcTok, len(s.Tokens))
	for i, t := range s.Tokens {
		conv[i] = t
		if t.Kind != "num" {
			continue
		}
		r, err := parseLiteral(t.S, s.Base)
		if err != nil {
			s.fail(err.Error())
			return
		}
		txt, exact := formatRat(r, nb, 1<<20, calcMaxDigits)
		if !exact || len(txt) > calcMaxDigits+1 {
			s.fail(fmt.Sprintf("%s は %s で表せないので、切り替えません", t.S, baseName(nb)))
			return
		}
		if strings.HasSuffix(t.S, ".") && !strings.Contains(txt, ".") {
			txt += "." // 打ちかけの小数点は残す
		}
		conv[i].S = txt
	}
	s.Tokens, s.Base = conv, nb
	s.Done = ""
}

// negate は、最後の数の符号を変える（= のあとなら結果の符号）。
func (s *CalcState) negate() {
	if s.fresh() {
		r, _ := parseRatString(s.Result)
		s.Result = ratString(r.Neg(r))
		s.Done = ""
		return
	}
	i := len(s.Tokens) - 1
	if i < 0 || !(s.Tokens[i].Kind == "num" || s.Tokens[i].Kind == "val") {
		s.op("-")
		return
	}
	if i > 0 && s.Tokens[i-1].Kind == "op" && s.Tokens[i-1].S == "-" && (i == 1 || s.Tokens[i-2].Kind == "op" || s.Tokens[i-2].Kind == "lp") {
		s.Tokens = append(s.Tokens[:i-1], s.Tokens[i:]...) // 単項のマイナスを外す
		return
	}
	if s.full() {
		s.fail("式が長すぎます")
		return
	}
	s.Tokens = append(s.Tokens[:i], append([]calcTok{{Kind: "op", S: "-"}}, s.Tokens[i:]...)...)
}

// recall は、履歴の結果を式に入れる。続けて押すと、older なら古いほうへ、そうでなければ新しいほうへ替える。
func (s *CalcState) recall(older bool) {
	if len(s.History) == 0 {
		s.fail("履歴はまだありません")
		return
	}
	s.leaveResult(false)
	t := s.last()
	k := 1
	if t != nil && t.Kind == "val" && t.recall > 0 {
		k = t.recall + 1
		if !older {
			k = t.recall - 1
		}
		k = min(max(k, 1), len(s.History))
		s.Tokens = s.Tokens[:len(s.Tokens)-1]
	} else if isOperand(t) {
		s.fail("数のあとには、演算子を入れます")
		return
	}
	h := s.History[k-1]
	s.push(calcTok{Kind: "val", V: h.Value, recall: k})
	s.info(fmt.Sprintf("履歴 %d：%s", k, h.Expr))
}

// ---------- CalcService ----------

// calcSaved は calc.json の形（id ごとの基数と履歴）。
type calcSaved struct {
	Calc map[string]calcSavedOne `json:"calc"`
}

type calcSavedOne struct {
	Base    int         `json:"base"`
	History []CalcEntry `json:"history"`
}

// CalcService は、電卓の状態を持つ。入力の処理（タッチとキーの goroutine）から呼ばれ、待たずに返る。
type CalcService struct {
	mu       sync.Mutex
	m        map[string]*CalcState
	store    *Store
	onChange func()
	typer    func(text string) error // PC に打ち込む（HID）。nil なら打ち込めない
	dirty    chan struct{}
	saved    chan struct{} // テスト用
	queue    chan func()   // Submit の入力（入力の goroutine を待たせない）
}

// NewCalcService は calc.json を読む。
func NewCalcService(store *Store) *CalcService {
	c := &CalcService{m: map[string]*CalcState{}, store: store, dirty: make(chan struct{}, 1), queue: make(chan func(), 64)}
	go func() {
		for f := range c.queue {
			f()
		}
	}()
	var f calcSaved
	if err := store.Load(calcStoreName, &f); err == nil {
		for id, v := range f.Calc {
			if !validTextID(id) {
				continue
			}
			s := newCalcState()
			if v.Base == 16 || v.Base == 2 {
				s.Base = v.Base
			}
			for _, h := range v.History {
				if _, ok := parseRatString(h.Value); ok && len(s.History) < calcHistoryMax {
					s.History = append(s.History, h)
				}
			}
			c.m[id] = s
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("calc: %v", err)
	}
	go c.saver()
	return c
}

// SetOnChange は、状態が変わったときに呼ぶ関数を設定する（画面の描き直し）。
func (c *CalcService) SetOnChange(f func()) {
	c.mu.Lock()
	c.onChange = f
	c.mu.Unlock()
}

// SetTyper は、結果を PC に打ち込む関数を設定する。
func (c *CalcService) SetTyper(f func(string) error) {
	c.mu.Lock()
	c.typer = f
	c.mu.Unlock()
}

// Snapshot は、すべての電卓の状態の写し。
func (c *CalcService) Snapshot() map[string]CalcState {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]CalcState, len(c.m))
	for id, s := range c.m {
		out[id] = s.clone()
	}
	return out
}

// Submit は、Input を別の goroutine で順に行う（入力の goroutine から呼ぶ。計算と式の表示を待たせない）。
// 実機（ARMv5、CPU 1 つ）では、= の計算と描き直しの始まりが重なると、入力の処理が 10 ms を超えることがあった。
func (c *CalcService) Submit(id, in string, prefix bool) {
	if c == nil {
		return
	}
	select {
	case c.queue <- func() { c.Input(id, in, prefix) }:
	default:
		log.Printf("calc: too many inputs at once; dropped %q", in)
	}
}

// Sync は、Submit した入力がすべて終わるまで待つ（テスト用）。
func (c *CalcService) Sync() {
	done := make(chan struct{})
	c.queue <- func() { close(done) }
	<-done
}

// Input は、電卓 id にボタンかキー 1 つを入れる。prefix は、16 進と 2 進を 0x、0b を付けて打ち込むか。
func (c *CalcService) Input(id, in string, prefix bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	s, ok := c.m[id]
	if !ok {
		if len(c.m) >= calcMaxIDs {
			for k := range c.m { // 多すぎる id は、どれか 1 つを捨てる（ふつうは起きない）
				delete(c.m, k)
				break
			}
		}
		s = newCalcState()
		c.m[id] = s
	}
	hist, base := len(s.History), s.Base
	var head CalcEntry
	if hist > 0 {
		head = s.History[0]
	}
	text, exact := s.input(in, prefix)
	if text != "" {
		switch {
		case c.typer == nil:
			s.fail("PC に入力できません")
		case !calcTypeable(text):
			s.fail("この結果は、配列によらず同じキーで打てない文字を含むので、入力しません")
		default:
			if err := c.typer(text); err != nil {
				s.fail("入力できません：" + err.Error())
			} else if exact {
				s.info("入力しました：" + text)
			} else {
				s.info(fmt.Sprintf("入力しました（小数 %d 桁に丸めた値）：%s", calcTypeFrac, text))
			}
		}
	}
	s.Rev++
	save := len(s.History) != hist || s.Base != base || (hist > 0 && s.History[0] != head)
	f := c.onChange
	c.mu.Unlock()
	vlogf("calc: %s %q", id, in)
	if save {
		select {
		case c.dirty <- struct{}{}:
		default:
		}
	}
	if f != nil {
		f()
	}
}

func (c *CalcService) saver() {
	for range c.dirty {
		c.mu.Lock()
		f := calcSaved{Calc: map[string]calcSavedOne{}}
		for id, s := range c.m {
			f.Calc[id] = calcSavedOne{Base: s.Base, History: append([]CalcEntry(nil), s.History...)}
		}
		c.mu.Unlock()
		if err := c.store.Save(calcStoreName, f); err != nil {
			log.Printf("calc: save: %v", err)
		}
		if c.saved != nil {
			c.saved <- struct{}{}
		}
	}
}

// ---------- PC に打ち込む ----------

// calcTypeKey は、結果の文字を、US 配列でも JIS 配列でも同じ文字になるキーにする。
// 数字（キーボードの上の段。テンキーは NumLock で変わるので使わない）、-（US と JIS のどちらも 0 の右）、
// .（どちらも M の 2 つ右）、A〜F、x、b（文字のキー。Shift を使わないので、CapsLock でなければ小文字になる。
// 16 進の数字としては、大文字でも小文字でも同じ）。これ以外の文字は、配列で位置が違うか、Shift の有無で変わるので打たない。
func calcTypeKey(r rune) (Combo, bool) {
	var name string
	switch {
	case r >= '0' && r <= '9':
		name = string(r)
	case r >= 'A' && r <= 'F':
		name = string(r)
	case r >= 'a' && r <= 'f':
		name = strings.ToUpper(string(r))
	case r == 'x' || r == 'X':
		name = "X"
	case r == '-':
		name = "MINUS"
	case r == '.':
		name = "DOT"
	default:
		return Combo{}, false
	}
	u, ok := hidUsage[name]
	return Combo{Keys: []byte{u}}, ok
}

// calcTypeable は、text をすべて打てるか。
func calcTypeable(text string) bool {
	for _, r := range text {
		if _, ok := calcTypeKey(r); !ok {
			return false
		}
	}
	return text != ""
}

// calcTypeGap は、1 つのキーを押してから離すまで、離してから次を押すまでの時間。
// PC は 1 ms ごとにレポートを読むが、同じキーが続くとき（11 など）に、離したことが確実に届くようにあける。
const calcTypeGap = 12 * time.Millisecond

// typeOnHID は、text を HID のキーとして打ち込む。別の goroutine で打ち、待たずに返る。
func typeOnHID(st *State, text string) error {
	var combos []Combo
	for _, r := range text {
		c, ok := calcTypeKey(r)
		if !ok {
			return fmt.Errorf("%q は打てません", r)
		}
		combos = append(combos, c)
	}
	go func() {
		for _, c := range combos {
			st.press("calc:type", c)
			time.Sleep(calcTypeGap)
			st.release("calc:type")
			time.Sleep(calcTypeGap)
		}
		log.Printf("calc: typed %d characters", len(combos))
	}()
	return nil
}
