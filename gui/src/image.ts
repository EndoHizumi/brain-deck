// 背景画像の変換。画像ファイル（PNG、JPEG、WebP）を、Brain がそのまま画面に写せる形（RGB565）にする。
// 重い処理（切り抜き、縮小、明るさ、ディザリング）は、ここ（PC のブラウザ）で行い、Brain は写すだけにする。
//
// Brain の画像ファイル（<id>.565、image.go と同じ形）：
//   0..3 "LHI1"、4..5 幅、6..7 高さ（リトルエンディアンの uint16）、8.. RGB565（リトルエンディアン）
// id はファイル全体の SHA-256 の先頭 16 文字。設定には `background: <id>` と書く。

export const IMAGE_MAGIC = 'LHI1'
export const IMAGE_HEADER = 8
export const IMAGE_MAX_SIDE = 800
export const IMAGE_MAX_PIXELS = 800 * 480
export const IMAGE_ID = /^[0-9a-f]{16}$/
// 読み込んだ画像の長い辺がこれを超えると、整数の倍率で縮めてから扱う（shrink。brain-deck と同じ）
export const SOURCE_MAX_SIDE = 2048

export type Dither = 'fs' | 'none'
export const DITHER_LABELS: Record<Dither, string> = { fs: 'Floyd–Steinberg（なめらか）', none: 'なし（くっきり）' }

// 画面の画像（RGB565）。プレビューで使う
export interface Image565 {
  w: number
  h: number
  pix: Uint16Array
}

// 読み込んだ元の画像（RGBA）
export interface SourceImage {
  w: number
  h: number
  data: Uint8ClampedArray
}

export interface Crop {
  x: number
  y: number
  w: number
  h: number
}

// 変換の設定。切り抜きの中心（元の画像の座標）、拡大率、明るさ、ディザリング
export interface ConvertParams {
  cx: number
  cy: number
  zoom: number // 1 なら、縦横比を保ってはみ出さない最大の範囲
  brightness: number // -1（真っ黒）〜 0 〜 1（真っ白）。負なら暗く、正なら明るくする
  dither: Dither
}

export const DEFAULT_BRIGHTNESS = -0.35 // 文字が読みやすいよう、少し暗くする
export const MAX_ZOOM = 8

// ---------- 変換（internal/imgconv/imgconv.go と同じ整数の計算） ----------
//
// brain-deck photo add（Go）と画素単位で同じ結果にするため、浮動小数点を使わず、整数だけで計算する。
// 値は 2^53 より小さい整数なので、number のままでも正確に計算できる（割り算は idiv で切り捨てる）。

export const UNIT = 16 // resample の結果の単位（1/16）
export const FULL = 255 * UNIT

// idiv は、0 以上の整数 a を b で割って切り捨てる（浮動小数点の割り算の丸めに頼らない）。
function idiv(a: number, b: number): number {
  return (a - (a % b)) / b
}

// shrink は、長い辺が SOURCE_MAX_SIDE を超えるとき、整数の倍率 k で k×k の画素を平均して縮める。
export function shrinkFactor(w: number, h: number): number {
  return Math.ceil(Math.max(w, h, 1) / SOURCE_MAX_SIDE)
}

export function shrink(src: SourceImage): SourceImage {
  const k = shrinkFactor(src.w, src.h)
  if (k <= 1) return src
  const w = Math.max(Math.floor(src.w / k), 1)
  const h = Math.max(Math.floor(src.h / k), 1)
  const out = new Uint8ClampedArray(w * h * 4)
  const n = k * k
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const s = [0, 0, 0, 0]
      for (let dy = 0; dy < k; dy++) {
        const o = ((y * k + dy) * src.w + x * k) * 4
        for (let dx = 0; dx < k; dx++) for (let c = 0; c < 4; c++) s[c] += src.data[o + dx * 4 + c]
      }
      for (let c = 0; c < 4; c++) out[(y * w + x) * 4 + c] = idiv(s[c] + (n >> 1), n)
    }
  }
  return { w, h, data: out }
}

// fitCrop は、元の画像 sw×sh から、tw×th と同じ縦横比の範囲を、中心 (cx, cy) と拡大率で決める（整数の画素）。
// 範囲は元の画像の中に収める（はみ出すときは中心をずらす）。拡大率は 1000 分率に丸める。
export function fitCrop(sw: number, sh: number, tw: number, th: number, zoom: number, cx: number, cy: number): Crop {
  const zm = Math.min(Math.max(Math.round(zoom * 1000), 1000), MAX_ZOOM * 1000)
  let bw: number
  let bh: number
  if (sw * th <= sh * tw) {
    bw = sw
    bh = idiv(sw * th, tw)
  } else {
    bh = sh
    bw = idiv(sh * tw, th)
  }
  const w = Math.min(Math.max(idiv(bw * 1000, zm), 1), sw)
  const h = Math.min(Math.max(idiv(bh * 1000, zm), 1), sh)
  // 負の中心は、真ん中（brain-deck の既定）
  const px = cx < 0 ? sw >> 1 : Math.round(cx)
  const py = cy < 0 ? sh >> 1 : Math.round(cy)
  const x = Math.min(Math.max(px - (w >> 1), 0), sw - w)
  const y = Math.min(Math.max(py - (h >> 1), 0), sh - h)
  return { x, y, w, h }
}

// spans は、長さ n の範囲を out 個に分けたときの、出力ごとの最初の画素と重み（合計は n）。
function spans(n: number, out: number): { j0: number; w: number[] }[] {
  const sp = []
  for (let i = 0; i < out; i++) {
    const a = i * n
    const b = (i + 1) * n
    const j0 = Math.floor(a / out)
    const j1 = Math.ceil(b / out)
    const w: number[] = []
    for (let j = j0; j < j1; j++) w.push(Math.max(Math.min(b, (j + 1) * out) - Math.max(a, j * out), 0))
    sp.push({ j0, w })
  }
  return sp
}

// resample は、元の画像の crop の範囲を tw×th にして、RGB を 1/16 の単位（0〜4080）で返す。
// 出力の画素が覆う元の画素の面積で重みを付けて平均する（縮小も拡大も同じ式）。透明な部分は黒の上に重ねる。
export function resample(src: SourceImage, crop: Crop, tw: number, th: number): Int32Array {
  const xs = spans(crop.w, tw)
  const ys = spans(crop.h, th)
  const out = new Int32Array(tw * th * 3)
  const row = new Float64Array(tw * 3)
  let rowY = -1
  const acc = new Float64Array(tw * 3)
  const d = crop.w * crop.h * 255
  for (let j = 0; j < th; j++) {
    acc.fill(0)
    const sj = ys[j]
    for (let k = 0; k < sj.w.length; k++) {
      const wy = sj.w[k]
      if (!wy) continue
      const sy = crop.y + sj.j0 + k
      if (sy !== rowY) {
        const base = sy * src.w * 4
        for (let i = 0; i < tw; i++) {
          let R = 0
          let G = 0
          let B = 0
          const si = xs[i]
          for (let q = 0; q < si.w.length; q++) {
            const wx = si.w[q]
            if (!wx) continue
            const o = base + (crop.x + si.j0 + q) * 4
            const a = src.data[o + 3] * wx
            R += src.data[o] * a
            G += src.data[o + 1] * a
            B += src.data[o + 2] * a
          }
          row[i * 3] = R
          row[i * 3 + 1] = G
          row[i * 3 + 2] = B
        }
        rowY = sy
      }
      for (let i = 0; i < acc.length; i++) acc[i] += row[i] * wy
    }
    const o = j * tw * 3
    for (let i = 0; i < acc.length; i++) out[o + i] = idiv(acc[i] * UNIT + Math.floor(d / 2), d)
  }
  return out
}

// adjust は明るさを変える（画像に焼き込む）。k は -1〜1（百分率に丸める）。負なら黒に、正なら白に近づける。
export function adjust(v: Int32Array, k: number): Int32Array {
  const t = Math.min(Math.max(Math.round(k * 100), -100), 100)
  const out = Int32Array.from(v)
  if (t === 0) return out
  for (let i = 0; i < out.length; i++) {
    const x = out[i]
    out[i] = t < 0 ? Math.trunc((x * (100 + t)) / 100) : x + Math.trunc(((FULL - x) * t) / 100)
  }
  return out
}

// ---------- RGB565 ----------

// unpack565 は、RGB565 の値を、Brain の画面と同じ 8 ビットの色に戻す（fb.go の unpack）。
export function unpack565(v: number): [number, number, number] {
  return [Math.floor((((v >> 11) & 31) * 255) / 31), Math.floor((((v >> 5) & 63) * 255) / 63), Math.floor(((v & 31) * 255) / 31)]
}

// to565 は、RGB（1/16 の単位）を RGB565 にする。fs なら Floyd–Steinberg のディザリングで、丸めた誤差を右と下の画素に分ける。
// 誤差は、Brain が表示する色（unpack565）との差で計り、1/16 の単位の整数で分ける。
export function to565(v: Int32Array, w: number, h: number, dither: Dither): Uint16Array {
  const buf = Int32Array.from(v)
  const out = new Uint16Array(w * h)
  const max = [31, 63, 31]
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const q = [0, 0, 0]
      for (let c = 0; c < 3; c++) {
        const i = (y * w + x) * 3 + c
        const m = max[c]
        const p = Math.min(Math.max(buf[i], 0), FULL)
        q[c] = idiv(p * m * 2 + FULL, FULL * 2)
        if (dither !== 'fs') continue
        const e = p - idiv(q[c] * 255, m) * UNIT
        const e7 = (e * 7) >> 4
        const e3 = (e * 3) >> 4
        const e5 = (e * 5) >> 4
        const e1 = e - e7 - e3 - e5
        if (x + 1 < w) buf[i + 3] += e7
        if (y + 1 < h) {
          const d = i + w * 3
          if (x > 0) buf[d - 3] += e3
          buf[d] += e5
          if (x + 1 < w) buf[d + 3] += e1
        }
      }
      out[y * w + x] = (q[0] << 11) | (q[1] << 5) | q[2]
    }
  }
  return out
}

// fit565 は、RGB565 の画像を、縦横比を保って dw×dh の中央に縮めて置き、残りを黒にする（Brain の photo.go と同じ）。
// 縦向きの画面に、縦向きに切り抜いていない横向きの写真を出すときの見た目。
export function fit565(src: Image565, dw: number, dh: number): Image565 {
  const { w: sw, h: sh } = src
  const out = new Uint16Array(dw * dh)
  let w: number
  let h: number
  if (sw * dh <= sh * dw) {
    h = dh
    w = Math.max(idiv(sw * dh, sh), 1)
  } else {
    w = dw
    h = Math.max(idiv(sh * dw, sw), 1)
  }
  const ox = (dw - w) >> 1
  const oy = (dh - h) >> 1
  const xs = spans(sw, w)
  const ys = spans(sh, h)
  const d = sw * sh
  const hd = Math.floor(d / 2)
  for (let j = 0; j < h; j++) {
    for (let i = 0; i < w; i++) {
      let r = 0
      let g = 0
      let b = 0
      for (let k = 0; k < ys[j].w.length; k++) {
        const wy = ys[j].w[k]
        if (!wy) continue
        const base = (ys[j].j0 + k) * sw
        for (let q = 0; q < xs[i].w.length; q++) {
          const wx = xs[i].w[q]
          if (!wx) continue
          const p = src.pix[base + xs[i].j0 + q]
          const wt = wx * wy
          r += ((p >> 11) & 31) * wt
          g += ((p >> 5) & 63) * wt
          b += (p & 31) * wt
        }
      }
      out[(oy + j) * dw + ox + i] = (idiv(r + hd, d) << 11) | (idiv(g + hd, d) << 5) | idiv(b + hd, d)
    }
  }
  return { w: dw, h: dh, pix: out }
}

// ---------- ファイル ----------

export function encodeImageFile(img: Image565): Uint8Array {
  const b = new Uint8Array(IMAGE_HEADER + img.w * img.h * 2)
  const dv = new DataView(b.buffer)
  for (let i = 0; i < 4; i++) b[i] = IMAGE_MAGIC.charCodeAt(i)
  dv.setUint16(4, img.w, true)
  dv.setUint16(6, img.h, true)
  for (let i = 0; i < img.pix.length; i++) dv.setUint16(IMAGE_HEADER + i * 2, img.pix[i], true)
  return b
}

// decodeImageFile は Brain の画像ファイルを読む。形が違えば null。
export function decodeImageFile(b: Uint8Array): Image565 | null {
  if (b.length < IMAGE_HEADER || String.fromCharCode(b[0], b[1], b[2], b[3]) !== IMAGE_MAGIC) return null
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
  const w = dv.getUint16(4, true)
  const h = dv.getUint16(6, true)
  if (!w || !h || b.length !== IMAGE_HEADER + w * h * 2) return null
  const pix = new Uint16Array(w * h)
  for (let i = 0; i < pix.length; i++) pix[i] = dv.getUint16(IMAGE_HEADER + i * 2, true)
  return { w, h, pix }
}

// sha256Hex は中身の SHA-256（16 進）。id はその先頭 16 文字。
export async function sha256Hex(b: Uint8Array): Promise<string> {
  const d = await crypto.subtle.digest('SHA-256', b as BufferSource)
  return [...new Uint8Array(d)].map((x) => x.toString(16).padStart(2, '0')).join('')
}

// convert は、元の画像を tw×th の Brain の画像ファイルにする。
// 長い辺が SOURCE_MAX_SIDE を超える画像は、先に shrink で縮める（cx、cy は縮める前の座標で受け取る）。
export function convert(src: SourceImage, tw: number, th: number, p: ConvertParams): { img: Image565; file: Uint8Array; crop: Crop } {
  const k = shrinkFactor(src.w, src.h)
  const s = shrink(src)
  const c = fitCrop(s.w, s.h, tw, th, p.zoom, p.cx < 0 ? -1 : Math.floor(Math.round(p.cx) / k), p.cy < 0 ? -1 : Math.floor(Math.round(p.cy) / k))
  const rgb = adjust(resample(s, c, tw, th), p.brightness)
  const img = { w: tw, h: th, pix: to565(rgb, tw, th, p.dither) }
  // crop は、元の画像の座標で返す（切り抜きの枠を描くため）
  return { img, file: encodeImageFile(img), crop: { x: c.x * k, y: c.y * k, w: c.w * k, h: c.h * k } }
}

// checkTarget は、Brain に置ける大きさかを確かめる。
export function checkTarget(w: number, h: number): string | null {
  if (w < 1 || h < 1 || w > IMAGE_MAX_SIDE || h > IMAGE_MAX_SIDE || w * h > IMAGE_MAX_PIXELS)
    return `${w}×${h} は置けません（各辺 ${IMAGE_MAX_SIDE} まで、${IMAGE_MAX_PIXELS} 画素まで）`
  return null
}

// decodeFile は、画像ファイル（PNG、JPEG、WebP など、ブラウザが読めるもの）を RGBA にする（ブラウザだけで使う）。
export async function decodeFile(file: Blob): Promise<SourceImage> {
  // EXIF の向きはブラウザが直す（imageOrientation: from-image。brain-deck も同じく直す）。
  // 縮めるのは convert の shrink（整数の計算）で行い、ブラウザの縮小は使わない（brain-deck と同じ結果にするため）
  const bmp = await createImageBitmap(file, { imageOrientation: 'from-image' })
  const c = document.createElement('canvas')
  c.width = bmp.width
  c.height = bmp.height
  const ctx = c.getContext('2d', { willReadFrequently: true })
  if (!ctx) throw new Error('canvas is not available')
  ctx.drawImage(bmp, 0, 0)
  const d = ctx.getImageData(0, 0, c.width, c.height)
  bmp.close()
  return { w: d.width, h: d.height, data: d.data }
}

// toBase64 は、送るためにバイト列を base64 にする。
export function toBase64(b: Uint8Array): string {
  let s = ''
  for (let i = 0; i < b.length; i += 0x8000) s += String.fromCharCode(...b.subarray(i, i + 0x8000))
  return btoa(s)
}

export function fromBase64(s: string): Uint8Array {
  const bin = atob(s)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}
