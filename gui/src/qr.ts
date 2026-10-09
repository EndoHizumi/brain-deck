// QR コードの符号化。qr.go と同じ手順（Project Nayuki の QR Code generator library を、バイトモードだけに絞ったもの。MIT ライセンス）。
// 誤り訂正は M（同じバージョンに収まるなら上げる）、バージョンは収まるいちばん小さいもの、マスクは減点のいちばん少ないもの。
// ASCII 以外の文字を含むときは、先頭に ECI（UTF-8、26）を付ける。gui/test/fixtures/qrcodes.json で Go と同じ模様になることを確かめる。

const ECL_MEDIUM = 1
const ECL_FORMAT_BITS = [1, 0, 3, 2]

const ECC_CODEWORDS_PER_BLOCK = [
  [-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  [-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28],
  [-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  [-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
]

const NUM_ERROR_CORRECTION_BLOCKS = [
  [-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25],
  [-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49],
  [-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68],
  [-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81],
]

export interface QRCode {
  version: number
  size: number
  ecl: number
  mask: number
  dark(x: number, y: number): boolean
}

interface Segment {
  mode: number
  numChars: number
  bits: number[]
}

function appendBits(bb: number[], val: number, n: number): void {
  for (let i = n - 1; i >= 0; i--) bb.push((val >>> i) & 1)
}

function charCountBits(s: Segment, ver: number): number {
  if (s.mode === 0x4) return ver <= 9 ? 8 : 16
  return 0
}

function totalBits(segs: Segment[], ver: number): number {
  let n = 0
  for (const s of segs) {
    const cc = charCountBits(s, ver)
    if (cc > 0 && s.numChars >= 1 << cc) return -1
    n += 4 + cc + s.bits.length
  }
  return n
}

function numRawDataModules(ver: number): number {
  let r = (16 * ver + 128) * ver + 64
  if (ver >= 2) {
    const n = Math.floor(ver / 7) + 2
    r -= (25 * n - 10) * n - 55
    if (ver >= 7) r -= 36
  }
  return r
}

function numDataCodewords(ver: number, ecl: number): number {
  return Math.floor(numRawDataModules(ver) / 8) - ECC_CODEWORDS_PER_BLOCK[ecl][ver] * NUM_ERROR_CORRECTION_BLOCKS[ecl][ver]
}

function mul(x: number, y: number): number {
  let z = 0
  for (let i = 7; i >= 0; i--) {
    z = (z << 1) ^ ((z >>> 7) * 0x11d)
    z ^= ((y >>> i) & 1) * x
  }
  return z & 0xff
}

function rsDivisor(degree: number): number[] {
  const r = new Array<number>(degree).fill(0)
  r[degree - 1] = 1
  let root = 1
  for (let i = 0; i < degree; i++) {
    for (let j = 0; j < r.length; j++) {
      r[j] = mul(r[j], root)
      if (j + 1 < r.length) r[j] ^= r[j + 1]
    }
    root = mul(root, 0x02)
  }
  return r
}

function rsRemainder(data: number[], div: number[]): number[] {
  const r = new Array<number>(div.length).fill(0)
  for (const b of data) {
    const factor = b ^ r.shift()!
    r.push(0)
    for (let i = 0; i < r.length; i++) r[i] ^= mul(div[i], factor)
  }
  return r
}

// encodeQR は文字列を QR コードにする。収まらなければ null。
export function encodeQR(text: string): QRCode | null {
  const data = new TextEncoder().encode(text)
  const segs: Segment[] = []
  if ([...text].some((c) => c.codePointAt(0)! >= 0x80)) {
    const bits: number[] = []
    appendBits(bits, 26, 8)
    segs.push({ mode: 0x7, numChars: 0, bits })
  }
  const bits: number[] = []
  for (const b of data) appendBits(bits, b, 8)
  segs.push({ mode: 0x4, numChars: data.length, bits })

  let ver = 1
  let used = 0
  let ecl = ECL_MEDIUM
  for (;; ver++) {
    used = totalBits(segs, ver)
    if (used >= 0 && used <= numDataCodewords(ver, ecl) * 8) break
    if (ver >= 40) return null
  }
  for (const e of [1, 2, 3]) if (e > ecl && used <= numDataCodewords(ver, e) * 8) ecl = e
  const bb: number[] = []
  for (const s of segs) {
    appendBits(bb, s.mode, 4)
    appendBits(bb, s.numChars, charCountBits(s, ver))
    bb.push(...s.bits)
  }
  const capBits = numDataCodewords(ver, ecl) * 8
  appendBits(bb, 0, Math.min(4, capBits - bb.length))
  appendBits(bb, 0, (8 - (bb.length % 8)) % 8)
  for (let pad = 0xec; bb.length < capBits; pad ^= 0xec ^ 0x11) appendBits(bb, pad, 8)
  const codewords = new Array<number>(bb.length / 8).fill(0)
  bb.forEach((bit, i) => {
    if (bit) codewords[i >>> 3] |= 1 << (7 - (i & 7))
  })
  return build(ver, ecl, codewords)
}

function build(ver: number, ecl: number, dataCodewords: number[]): QRCode {
  const size = ver * 4 + 17
  const modules: boolean[][] = Array.from({ length: size }, () => new Array<boolean>(size).fill(false))
  const isFunc: boolean[][] = Array.from({ length: size }, () => new Array<boolean>(size).fill(false))
  const setFunc = (x: number, y: number, dark: boolean) => {
    modules[y][x] = dark
    isFunc[y][x] = true
  }

  const drawFormatBits = (mask: number) => {
    const data = (ECL_FORMAT_BITS[ecl] << 3) | mask
    let rem = data
    for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537)
    const bits = ((data << 10) | rem) ^ 0x5412
    const bit = (i: number) => ((bits >>> i) & 1) !== 0
    for (let i = 0; i <= 5; i++) setFunc(8, i, bit(i))
    setFunc(8, 7, bit(6))
    setFunc(8, 8, bit(7))
    setFunc(7, 8, bit(8))
    for (let i = 9; i < 15; i++) setFunc(14 - i, 8, bit(i))
    for (let i = 0; i < 8; i++) setFunc(size - 1 - i, 8, bit(i))
    for (let i = 8; i < 15; i++) setFunc(8, size - 15 + i, bit(i))
    setFunc(8, size - 8, true)
  }

  // 機能パターン
  for (let i = 0; i < size; i++) {
    setFunc(6, i, i % 2 === 0)
    setFunc(i, 6, i % 2 === 0)
  }
  const finder = (x: number, y: number) => {
    for (let dy = -4; dy <= 4; dy++) {
      for (let dx = -4; dx <= 4; dx++) {
        const d = Math.max(Math.abs(dx), Math.abs(dy))
        const xx = x + dx
        const yy = y + dy
        if (xx >= 0 && xx < size && yy >= 0 && yy < size) setFunc(xx, yy, d !== 2 && d !== 4)
      }
    }
  }
  finder(3, 3)
  finder(size - 4, 3)
  finder(3, size - 4)
  const pos: number[] = []
  if (ver > 1) {
    const n = Math.floor(ver / 7) + 2
    const step = Math.floor((ver * 8 + n * 3 + 5) / (n * 4 - 4)) * 2
    pos.push(6)
    for (let i = n - 1, p = size - 7; i >= 1; i--, p -= step) pos.splice(1, 0, p)
  }
  const n = pos.length
  for (let i = 0; i < n; i++) {
    for (let j = 0; j < n; j++) {
      if ((i === 0 && j === 0) || (i === 0 && j === n - 1) || (i === n - 1 && j === 0)) continue
      for (let dy = -2; dy <= 2; dy++) {
        for (let dx = -2; dx <= 2; dx++) setFunc(pos[i] + dx, pos[j] + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1)
      }
    }
  }
  drawFormatBits(0)
  if (ver >= 7) {
    let rem = ver
    for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25)
    const bits = (ver << 12) | rem
    for (let i = 0; i < 18; i++) {
      const dark = ((bits >>> i) & 1) !== 0
      const a = size - 11 + (i % 3)
      const b = Math.floor(i / 3)
      setFunc(a, b, dark)
      setFunc(b, a, dark)
    }
  }

  // 誤り訂正の符号を足し、ブロックを交互に並べる
  const numBlocks = NUM_ERROR_CORRECTION_BLOCKS[ecl][ver]
  const blockECC = ECC_CODEWORDS_PER_BLOCK[ecl][ver]
  const raw = Math.floor(numRawDataModules(ver) / 8)
  const numShort = numBlocks - (raw % numBlocks)
  const shortLen = Math.floor(raw / numBlocks)
  const div = rsDivisor(blockECC)
  const blocks: number[][] = []
  for (let i = 0, k = 0; i < numBlocks; i++) {
    const len = shortLen - blockECC + (i < numShort ? 0 : 1)
    const dat = dataCodewords.slice(k, k + len)
    k += len
    const ecc = rsRemainder(dat, div)
    if (i < numShort) dat.push(0)
    blocks.push(dat.concat(ecc))
  }
  const all: number[] = []
  for (let i = 0; i < blocks[0].length; i++) {
    blocks.forEach((b, j) => {
      if (i !== shortLen - blockECC || j >= numShort) all.push(b[i])
    })
  }

  // 符号を置く
  let bi = 0
  for (let right = size - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5
    for (let vert = 0; vert < size; vert++) {
      for (let j = 0; j < 2; j++) {
        const x = right - j
        const upward = ((right + 1) & 2) === 0
        const y = upward ? size - 1 - vert : vert
        if (!isFunc[y][x] && bi < all.length * 8) {
          modules[y][x] = ((all[bi >>> 3] >>> (7 - (bi & 7))) & 1) !== 0
          bi++
        }
      }
    }
  }

  const applyMask = (mask: number) => {
    for (let y = 0; y < size; y++) {
      for (let x = 0; x < size; x++) {
        let inv: boolean
        switch (mask) {
          case 0: inv = (x + y) % 2 === 0; break
          case 1: inv = y % 2 === 0; break
          case 2: inv = x % 3 === 0; break
          case 3: inv = (x + y) % 3 === 0; break
          case 4: inv = (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0; break
          case 5: inv = ((x * y) % 2) + ((x * y) % 3) === 0; break
          case 6: inv = (((x * y) % 2) + ((x * y) % 3)) % 2 === 0; break
          default: inv = (((x + y) % 2) + ((x * y) % 3)) % 2 === 0
        }
        if (inv && !isFunc[y][x]) modules[y][x] = !modules[y][x]
      }
    }
  }

  const addHistory = (run: number, h: number[]) => {
    if (h[0] === 0) run += size
    h.pop()
    h.unshift(run)
  }
  const countPatterns = (h: number[]) => {
    const m = h[1]
    const core = m > 0 && h[2] === m && h[3] === m * 3 && h[4] === m && h[5] === m
    return (core && h[0] >= m * 4 && h[6] >= m ? 1 : 0) + (core && h[6] >= m * 4 && h[0] >= m ? 1 : 0)
  }
  const terminate = (color: boolean, run: number, h: number[]) => {
    if (color) {
      addHistory(run, h)
      run = 0
    }
    run += size
    addHistory(run, h)
    return countPatterns(h)
  }
  const penalty = () => {
    let result = 0
    for (const swap of [false, true]) {
      for (let a = 0; a < size; a++) {
        let color = false
        let run = 0
        const h = [0, 0, 0, 0, 0, 0, 0]
        for (let b = 0; b < size; b++) {
          const c = swap ? modules[b][a] : modules[a][b]
          if (c === color) {
            run++
            if (run === 5) result += 3
            else if (run > 5) result++
          } else {
            addHistory(run, h)
            if (!color) result += countPatterns(h) * 40
            color = c
            run = 1
          }
        }
        result += terminate(color, run, h) * 40
      }
    }
    for (let y = 0; y < size - 1; y++) {
      for (let x = 0; x < size - 1; x++) {
        const c = modules[y][x]
        if (c === modules[y][x + 1] && c === modules[y + 1][x] && c === modules[y + 1][x + 1]) result += 3
      }
    }
    let dark = 0
    for (const row of modules) for (const c of row) if (c) dark++
    const total = size * size
    const k = Math.ceil(Math.abs(dark * 20 - total * 10) / total) - 1
    return result + k * 10
  }

  let best = 0
  let minPenalty = Infinity
  for (let m = 0; m < 8; m++) {
    applyMask(m)
    drawFormatBits(m)
    const p = penalty()
    if (p < minPenalty) {
      best = m
      minPenalty = p
    }
    applyMask(m)
  }
  applyMask(best)
  drawFormatBits(best)
  return {
    version: ver,
    size,
    ecl,
    mask: best,
    dark: (x, y) => x >= 0 && y >= 0 && x < size && y < size && modules[y][x],
  }
}
