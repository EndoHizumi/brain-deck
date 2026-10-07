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
// 読み込んだ画像の長い辺をここまで縮めてから扱う（大きな写真でもメモリを使いすぎない）
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

// ---------- 切り抜き ----------

// fitCrop は、元の画像 sw×sh から、tw×th と同じ縦横比の範囲を、中心 (cx, cy) と拡大率で決める。
// 範囲は元の画像の中に収める（はみ出すときは中心をずらす）。
export function fitCrop(sw: number, sh: number, tw: number, th: number, zoom: number, cx: number, cy: number): Crop {
  const a = tw / th
  const z = Math.min(Math.max(zoom, 1), MAX_ZOOM)
  let w = Math.min(sw, sh * a) / z
  let h = w / a
  w = Math.min(w, sw)
  h = Math.min(h, sh)
  const x = Math.min(Math.max(cx - w / 2, 0), sw - w)
  const y = Math.min(Math.max(cy - h / 2, 0), sh - h)
  return { x, y, w, h }
}

// ---------- 縮小・拡大 ----------

// axisWeights は、出力の 1 列（1 行）ごとに、元の画素の番号と重みを返す。
// 縮小は、出力の画素が覆う範囲の面積で平均する。拡大は、2 つの画素の間を直線でつなぐ（バイリニア）。
function axisWeights(start: number, len: number, out: number, size: number): { idx: Int32Array; w: Float32Array; off: Int32Array } {
  const s = len / out
  const idx: number[] = []
  const wt: number[] = []
  const off: number[] = [0]
  for (let i = 0; i < out; i++) {
    if (s >= 1) {
      const a = start + i * s
      const b = a + s
      for (let j = Math.floor(a); j < Math.ceil(b); j++) {
        const cover = Math.min(b, j + 1) - Math.max(a, j)
        if (cover <= 0) continue
        idx.push(Math.min(Math.max(j, 0), size - 1))
        wt.push(cover / s)
      }
    } else {
      const c = start + (i + 0.5) * s - 0.5
      const j = Math.floor(c)
      const f = c - j
      idx.push(Math.min(Math.max(j, 0), size - 1), Math.min(Math.max(j + 1, 0), size - 1))
      wt.push(1 - f, f)
    }
    off.push(idx.length)
  }
  return { idx: Int32Array.from(idx), w: Float32Array.from(wt), off: Int32Array.from(off) }
}

// resample は、元の画像の crop の範囲を tw×th にして、RGB（0〜255 の小数）を返す。透明な部分は黒の上に重ねる。
export function resample(src: SourceImage, crop: Crop, tw: number, th: number): Float32Array {
  const xs = axisWeights(crop.x, crop.w, tw, src.w)
  const ys = axisWeights(crop.y, crop.h, th, src.h)
  // 使う行だけ、横方向を先に縮める
  const rows = new Map<number, Float32Array>()
  const row = (sy: number): Float32Array => {
    let r = rows.get(sy)
    if (r) return r
    r = new Float32Array(tw * 3)
    const base = sy * src.w * 4
    for (let i = 0; i < tw; i++) {
      let R = 0
      let G = 0
      let B = 0
      for (let k = xs.off[i]; k < xs.off[i + 1]; k++) {
        const o = base + xs.idx[k] * 4
        const a = (src.data[o + 3] / 255) * xs.w[k]
        R += src.data[o] * a
        G += src.data[o + 1] * a
        B += src.data[o + 2] * a
      }
      r[i * 3] = R
      r[i * 3 + 1] = G
      r[i * 3 + 2] = B
    }
    rows.set(sy, r)
    return r
  }
  const out = new Float32Array(tw * th * 3)
  for (let j = 0; j < th; j++) {
    const o = j * tw * 3
    for (let k = ys.off[j]; k < ys.off[j + 1]; k++) {
      const r = row(ys.idx[k])
      const wy = ys.w[k]
      for (let i = 0; i < tw * 3; i++) out[o + i] += r[i] * wy
    }
  }
  return out
}

// adjust は明るさを変える（画像に焼き込む）。k が負なら黒に、正なら白に近づける。
export function adjust(rgb: Float32Array, k: number): Float32Array {
  const out = new Float32Array(rgb.length)
  const t = Math.min(Math.max(k, -1), 1)
  for (let i = 0; i < rgb.length; i++) {
    const v = rgb[i]
    out[i] = t < 0 ? v * (1 + t) : v + (255 - v) * t
  }
  return out
}

// ---------- RGB565 ----------

// unpack565 は、RGB565 の値を、Brain の画面と同じ 8 ビットの色に戻す（fb.go の unpack）。
export function unpack565(v: number): [number, number, number] {
  return [Math.floor((((v >> 11) & 31) * 255) / 31), Math.floor((((v >> 5) & 63) * 255) / 63), Math.floor(((v & 31) * 255) / 31)]
}

// to565 は、RGB を RGB565 にする。fs なら Floyd–Steinberg のディザリングで、丸めた誤差を右と下の画素に分ける。
// 誤差は、Brain が表示する色（unpack565）との差で計る。
export function to565(rgb: Float32Array, w: number, h: number, dither: Dither): Uint16Array {
  const buf = Float32Array.from(rgb)
  const out = new Uint16Array(w * h)
  const max = [31, 63, 31]
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const q = [0, 0, 0]
      for (let c = 0; c < 3; c++) {
        const i = (y * w + x) * 3 + c
        const m = max[c]
        const v = Math.min(Math.max(buf[i], 0), 255)
        q[c] = Math.round((v * m) / 255)
        if (dither !== 'fs') continue
        const e = v - Math.floor((q[c] * 255) / m)
        if (x + 1 < w) buf[i + 3] += (e * 7) / 16
        if (y + 1 < h) {
          const d = i + w * 3
          if (x > 0) buf[d - 3] += (e * 3) / 16
          buf[d] += (e * 5) / 16
          if (x + 1 < w) buf[d + 3] += e / 16
        }
      }
      out[y * w + x] = (q[0] << 11) | (q[1] << 5) | q[2]
    }
  }
  return out
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
export function convert(src: SourceImage, tw: number, th: number, p: ConvertParams): { img: Image565; file: Uint8Array; crop: Crop } {
  const crop = fitCrop(src.w, src.h, tw, th, p.zoom, p.cx, p.cy)
  const rgb = adjust(resample(src, crop, tw, th), p.brightness)
  const img = { w: tw, h: th, pix: to565(rgb, tw, th, p.dither) }
  return { img, file: encodeImageFile(img), crop }
}

// checkTarget は、Brain に置ける大きさかを確かめる。
export function checkTarget(w: number, h: number): string | null {
  if (w < 1 || h < 1 || w > IMAGE_MAX_SIDE || h > IMAGE_MAX_SIDE || w * h > IMAGE_MAX_PIXELS)
    return `${w}×${h} は置けません（各辺 ${IMAGE_MAX_SIDE} まで、${IMAGE_MAX_PIXELS} 画素まで）`
  return null
}

// decodeFile は、画像ファイル（PNG、JPEG、WebP など、ブラウザが読めるもの）を RGBA にする（ブラウザだけで使う）。
// 長い辺が SOURCE_MAX_SIDE を超えるときは、読み込むときに縮める。
export async function decodeFile(file: Blob): Promise<SourceImage> {
  const probe = await createImageBitmap(file)
  const s = Math.min(1, SOURCE_MAX_SIDE / Math.max(probe.width, probe.height))
  let bmp = probe
  if (s < 1) {
    bmp = await createImageBitmap(file, { resizeWidth: Math.round(probe.width * s), resizeHeight: Math.round(probe.height * s), resizeQuality: 'high' })
    probe.close()
  }
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
