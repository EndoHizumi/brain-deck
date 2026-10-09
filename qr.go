package main

import (
	"errors"
	"unicode/utf8"
)

// ---------- QR コードの符号化 ----------
//
// QR コードのウィジェット（widget: qr）で、PC から送った文字列を QR コードにする。
// Project Nayuki の QR Code generator library（MIT ライセンス、https://www.nayuki.io/page/qr-code-generator-library）の
// 手順を、バイトモードだけに絞って Go に書き直したもの。gui/src/qr.ts も同じ手順で、同じ模様になる
// （gui/test/fixtures/qrcodes.json で比べる）。
//
// - 誤り訂正のレベルは M（約 15% の汚れに耐える）。同じバージョンに収まるなら、より強いレベルに上げる。
// - バージョン（大きさ）は、収まるいちばん小さいもの。マスクは、規格の減点がいちばん少ないもの。
// - ASCII 以外の文字を含むときは、先頭に ECI（UTF-8、26）を付ける。

const (
	qrMinVersion = 1
	qrMaxVersion = 40
)

// 誤り訂正のレベル
const (
	qrECLLow = iota
	qrECLMedium
	qrECLQuartile
	qrECLHigh
)

// 形式情報に書く、誤り訂正のレベルの 2 ビット
var qrECLFormatBits = [4]int{1, 0, 3, 2}

var qrECCCodewordsPerBlock = [4][41]int{
	{-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	{-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28},
	{-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	{-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
}

var qrNumErrorCorrectionBlocks = [4][41]int{
	{-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25},
	{-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49},
	{-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68},
	{-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81},
}

// QRCode は QR コードの模様。Dark(x, y) が true の升を黒く塗る。
type QRCode struct {
	Version int
	Size    int // 1 辺の升の数（17 + 4 × Version）
	ECL     int
	Mask    int
	modules [][]bool
	isFunc  [][]bool
}

// Dark は (x, y) の升が黒か。範囲の外は白。
func (q *QRCode) Dark(x, y int) bool {
	return x >= 0 && y >= 0 && x < q.Size && y < q.Size && q.modules[y][x]
}

var errQRTooLong = errors.New("the text is too long for a QR code")

// qrBits はビットの列。
type qrBits []bool

func (b *qrBits) appendBits(val, n int) {
	for i := n - 1; i >= 0; i-- {
		*b = append(*b, (val>>i)&1 != 0)
	}
}

// qrSegment は符号化した区切り（モード、文字数、ビット）。
type qrSegment struct {
	mode     int // 4 ビットのモード指示子
	numChars int
	data     qrBits
}

// charCountBits は、モードとバージョンごとの文字数の欄のビット数。ECI は 0。
func (s qrSegment) charCountBits(ver int) int {
	switch s.mode {
	case 0x4: // バイト
		if ver <= 9 {
			return 8
		}
		return 16
	}
	return 0
}

func qrBytesSegment(data []byte) qrSegment {
	var b qrBits
	for _, c := range data {
		b.appendBits(int(c), 8)
	}
	return qrSegment{mode: 0x4, numChars: len(data), data: b}
}

// qrECISegment は ECI の区切り（assignVal < 128 のときだけ使う）。
func qrECISegment(assignVal int) qrSegment {
	var b qrBits
	b.appendBits(assignVal, 8)
	return qrSegment{mode: 0x7, data: b}
}

// totalBits は、バージョン ver での区切りのビット数。文字数が欄に入らなければ -1。
func qrTotalBits(segs []qrSegment, ver int) int {
	n := 0
	for _, s := range segs {
		cc := s.charCountBits(ver)
		if cc > 0 && s.numChars >= 1<<cc {
			return -1
		}
		n += 4 + cc + len(s.data)
	}
	return n
}

// EncodeQR は文字列を QR コードにする（誤り訂正 M、収まれば上げる）。
func EncodeQR(text string) (*QRCode, error) {
	data := []byte(text)
	segs := []qrSegment{}
	for _, r := range text {
		if r >= utf8.RuneSelf {
			segs = append(segs, qrECISegment(26)) // UTF-8
			break
		}
	}
	segs = append(segs, qrBytesSegment(data))
	return qrEncodeSegments(segs, qrECLMedium, qrMinVersion, qrMaxVersion)
}

func qrEncodeSegments(segs []qrSegment, ecl, minVer, maxVer int) (*QRCode, error) {
	ver, used := minVer, 0
	for ; ; ver++ {
		capBits := qrNumDataCodewords(ver, ecl) * 8
		used = qrTotalBits(segs, ver)
		if used >= 0 && used <= capBits {
			break
		}
		if ver >= maxVer {
			return nil, errQRTooLong
		}
	}
	for _, e := range []int{qrECLMedium, qrECLQuartile, qrECLHigh} { // 同じバージョンのまま、強いレベルに上げる
		if e > ecl && used <= qrNumDataCodewords(ver, e)*8 {
			ecl = e
		}
	}
	var bb qrBits
	for _, s := range segs {
		bb.appendBits(s.mode, 4)
		bb.appendBits(s.numChars, s.charCountBits(ver))
		bb = append(bb, s.data...)
	}
	capBits := qrNumDataCodewords(ver, ecl) * 8
	bb.appendBits(0, min(4, capBits-len(bb)))
	bb.appendBits(0, (8-len(bb)%8)%8)
	for pad := 0xEC; len(bb) < capBits; pad ^= 0xEC ^ 0x11 {
		bb.appendBits(pad, 8)
	}
	codewords := make([]byte, len(bb)/8)
	for i, bit := range bb {
		if bit {
			codewords[i>>3] |= 1 << (7 - i&7)
		}
	}
	return qrNew(ver, ecl, codewords), nil
}

func qrNew(ver, ecl int, dataCodewords []byte) *QRCode {
	size := ver*4 + 17
	q := &QRCode{Version: ver, Size: size, ECL: ecl}
	q.modules = make([][]bool, size)
	q.isFunc = make([][]bool, size)
	for i := range q.modules {
		q.modules[i] = make([]bool, size)
		q.isFunc[i] = make([]bool, size)
	}
	q.drawFunctionPatterns()
	q.drawCodewords(q.addECCAndInterleave(dataCodewords))
	best, minPenalty := 0, int(^uint(0)>>1)
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormatBits(m)
		if p := q.penaltyScore(); p < minPenalty {
			best, minPenalty = m, p
		}
		q.applyMask(m) // XOR なので、もう一度かければ元に戻る
	}
	q.Mask = best
	q.applyMask(best)
	q.drawFormatBits(best)
	q.isFunc = nil
	return q
}

func (q *QRCode) setFunc(x, y int, dark bool) {
	q.modules[y][x] = dark
	q.isFunc[y][x] = true
}

func (q *QRCode) drawFunctionPatterns() {
	for i := 0; i < q.Size; i++ { // タイミングパターン
		q.setFunc(6, i, i%2 == 0)
		q.setFunc(i, 6, i%2 == 0)
	}
	q.drawFinder(3, 3)
	q.drawFinder(q.Size-4, 3)
	q.drawFinder(3, q.Size-4)
	pos := q.alignmentPositions()
	n := len(pos)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if !(i == 0 && j == 0 || i == 0 && j == n-1 || i == n-1 && j == 0) { // 位置検出パターンと重なるものは描かない
				q.drawAlignment(pos[i], pos[j])
			}
		}
	}
	q.drawFormatBits(0) // 場所を取っておく（マスクを決めたあとに書き直す）
	q.drawVersion()
}

func (q *QRCode) drawFormatBits(mask int) {
	data := qrECLFormatBits[q.ECL]<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>i)&1 != 0 }
	for i := 0; i <= 5; i++ {
		q.setFunc(8, i, bit(i))
	}
	q.setFunc(8, 7, bit(6))
	q.setFunc(8, 8, bit(7))
	q.setFunc(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.setFunc(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.setFunc(q.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.setFunc(8, q.Size-15+i, bit(i))
	}
	q.setFunc(8, q.Size-8, true) // いつも黒の升
}

func (q *QRCode) drawVersion() {
	if q.Version < 7 {
		return
	}
	rem := q.Version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := q.Version<<12 | rem
	for i := 0; i < 18; i++ {
		dark := (bits>>i)&1 != 0
		a, b := q.Size-11+i%3, i/3
		q.setFunc(a, b, dark)
		q.setFunc(b, a, dark)
	}
}

func (q *QRCode) drawFinder(x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			d := max(abs(dx), abs(dy))
			xx, yy := x+dx, y+dy
			if xx >= 0 && xx < q.Size && yy >= 0 && yy < q.Size {
				q.setFunc(xx, yy, d != 2 && d != 4)
			}
		}
	}
}

func (q *QRCode) drawAlignment(x, y int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			q.setFunc(x+dx, y+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

func (q *QRCode) alignmentPositions() []int {
	if q.Version == 1 {
		return nil
	}
	n := q.Version/7 + 2
	step := (q.Version*8 + n*3 + 5) / (n*4 - 4) * 2
	out := make([]int, n)
	out[0] = 6
	for i, pos := n-1, q.Size-7; i >= 1; i, pos = i-1, pos-step {
		out[i] = pos
	}
	return out
}

func (q *QRCode) addECCAndInterleave(data []byte) []byte {
	numBlocks := qrNumErrorCorrectionBlocks[q.ECL][q.Version]
	blockECC := qrECCCodewordsPerBlock[q.ECL][q.Version]
	raw := qrNumRawDataModules(q.Version) / 8
	numShort := numBlocks - raw%numBlocks
	shortLen := raw / numBlocks
	div := qrRSDivisor(blockECC)
	blocks := make([][]byte, numBlocks)
	k := 0
	for i := range blocks {
		n := shortLen - blockECC
		if i >= numShort {
			n++
		}
		dat := append([]byte{}, data[k:k+n]...)
		k += n
		ecc := qrRSRemainder(dat, div)
		if i < numShort {
			dat = append(dat, 0) // 長いブロックとそろえる（あとで飛ばす）
		}
		blocks[i] = append(dat, ecc...)
	}
	out := make([]byte, 0, raw)
	for i := range blocks[0] {
		for j, b := range blocks {
			if i != shortLen-blockECC || j >= numShort {
				out = append(out, b[i])
			}
		}
	}
	return out
}

func (q *QRCode) drawCodewords(data []byte) {
	i := 0
	for right := q.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.Size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = q.Size - 1 - vert
				}
				if !q.isFunc[y][x] && i < len(data)*8 {
					q.modules[y][x] = (data[i>>3]>>(7-i&7))&1 != 0
					i++
				}
			}
		}
	}
}

func (q *QRCode) applyMask(mask int) {
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			var inv bool
			switch mask {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv && !q.isFunc[y][x] {
				q.modules[y][x] = !q.modules[y][x]
			}
		}
	}
}

const (
	qrPenaltyN1 = 3
	qrPenaltyN2 = 3
	qrPenaltyN3 = 40
	qrPenaltyN4 = 10
)

func (q *QRCode) penaltyScore() int {
	result := 0
	size := q.Size
	at := func(swap bool, a, b int) bool {
		if swap {
			return q.modules[b][a]
		}
		return q.modules[a][b]
	}
	// 行（swap=false：a が y、b が x）と列（swap=true）の、同じ色の続きと、位置検出パターンに似た並び
	for _, swap := range []bool{false, true} {
		for a := 0; a < size; a++ {
			runColor, run := false, 0
			hist := make([]int, 7)
			for b := 0; b < size; b++ {
				if at(swap, a, b) == runColor {
					run++
					if run == 5 {
						result += qrPenaltyN1
					} else if run > 5 {
						result++
					}
				} else {
					q.finderAddHistory(run, hist)
					if !runColor {
						result += q.finderCount(hist) * qrPenaltyN3
					}
					runColor, run = at(swap, a, b), 1
				}
			}
			result += q.finderTerminate(runColor, run, hist) * qrPenaltyN3
		}
	}
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			c := q.modules[y][x]
			if c == q.modules[y][x+1] && c == q.modules[y+1][x] && c == q.modules[y+1][x+1] {
				result += qrPenaltyN2
			}
		}
	}
	dark := 0
	for _, row := range q.modules {
		for _, c := range row {
			if c {
				dark++
			}
		}
	}
	total := size * size
	k := (abs(dark*20-total*10)+total-1)/total - 1 // ceil(|dark*20 - total*10| / total) - 1
	result += k * qrPenaltyN4
	return result
}

func (q *QRCode) finderCount(h []int) int {
	n := h[1]
	core := n > 0 && h[2] == n && h[3] == n*3 && h[4] == n && h[5] == n
	c := 0
	if core && h[0] >= n*4 && h[6] >= n {
		c++
	}
	if core && h[6] >= n*4 && h[0] >= n {
		c++
	}
	return c
}

func (q *QRCode) finderTerminate(runColor bool, run int, h []int) int {
	if runColor { // 黒で終わったら、白の続きを足す
		q.finderAddHistory(run, h)
		run = 0
	}
	run += q.Size // 外側の白
	q.finderAddHistory(run, h)
	return q.finderCount(h)
}

func (q *QRCode) finderAddHistory(run int, h []int) {
	if h[0] == 0 {
		run += q.Size // 外側の白を最初の続きに足す
	}
	copy(h[1:], h[:len(h)-1])
	h[0] = run
}

func qrNumRawDataModules(ver int) int {
	r := (16*ver+128)*ver + 64
	if ver >= 2 {
		n := ver/7 + 2
		r -= (25*n-10)*n - 55
		if ver >= 7 {
			r -= 36
		}
	}
	return r
}

func qrNumDataCodewords(ver, ecl int) int {
	return qrNumRawDataModules(ver)/8 - qrECCCodewordsPerBlock[ecl][ver]*qrNumErrorCorrectionBlocks[ecl][ver]
}

func qrRSDivisor(degree int) []byte {
	r := make([]byte, degree)
	r[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range r {
			r[j] = qrMul(r[j], root)
			if j+1 < len(r) {
				r[j] ^= r[j+1]
			}
		}
		root = qrMul(root, 0x02)
	}
	return r
}

func qrRSRemainder(data, div []byte) []byte {
	r := make([]byte, len(div))
	for _, b := range data {
		factor := b ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i := range r {
			r[i] ^= qrMul(div[i], factor)
		}
	}
	return r
}

func qrMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= int((y>>i)&1) * int(x)
	}
	return byte(z)
}
