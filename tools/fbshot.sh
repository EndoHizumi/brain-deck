#!/bin/bash
# Brain の画面（フレームバッファ /dev/fb0）を、PC に PNG で保存する。PC で実行する。lefthand は止めない。
#   tools/fbshot.sh                    brain-YYYYmmdd-HHMMSS.png に保存
#   tools/fbshot.sh out.png            名前を指定する
#   BRAIN=brain tools/fbshot.sh        ssh の接続先（既定 brain。README の「PC との接続」）
#   tools/fbshot.sh -raw fb.raw out.png  保存してある生のデータ（800×480、RGB565）を変換するだけ
# 取れるのは、そのとき Brain の画面に出ているもの（端末モードなら端末の画面）。
# display.rotate で回転していても、回転する前の向き（フレームバッファのまま）で保存する。
# 変換は python3 だけで行う（追加のライブラリは要らない）。
set -euo pipefail

W=800 H=480
if [ "${1:-}" = -raw ]; then
  raw=$2
  out=${3:-${2%.raw}.png}
else
  out=${1:-brain-$(date +%Y%m%d-%H%M%S).png}
  raw=$(mktemp)
  trap 'rm -f "$raw"' EXIT
  host=${BRAIN:-brain}
  # 大きさと色の形と画面の中身を、1 回の ssh でまとめて読む（ssh の接続に数秒かかるため）。
  # 1 行目に「幅,高さ ビット数」、そのあとに生のデータが続く
  ssh "$host" 'cd /sys/class/graphics/fb0 && s=$(cat virtual_size) && b=$(cat bits_per_pixel) && echo "$s $b" &&
    w=${s%,*} h=${s#*,} && sudo head -c $((w * h * b / 8)) /dev/fb0' > "$raw"
  read -r size bpp < <(head -n 1 "$raw")
  W=${size%,*} H=${size#*,}
  if [ "$bpp" != 16 ]; then
    echo "fbshot: 1 ドット $bpp ビットの画面には対応していません（RGB565 だけ）" >&2
    exit 1
  fi
  skip=$(( $(head -n 1 "$raw" | wc -c) ))
  tail -c +$((skip + 1)) "$raw" > "$raw.data" && mv "$raw.data" "$raw"
fi

python3 - "$raw" "$out" "$W" "$H" <<'PY'
import struct, sys, zlib
src, out, W, H = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
raw = open(src, 'rb').read()
if len(raw) < W * H * 2:
    sys.exit(f"fbshot: {src} は {len(raw)} バイトで、{W}×{H} の画面（{W * H * 2} バイト）より小さい")
rows = bytearray()
for y in range(H):
    rows.append(0)  # PNG の行の先頭（フィルタなし）
    o = y * W * 2
    for x in range(W):
        v = raw[o + x * 2] | raw[o + x * 2 + 1] << 8  # RGB565、リトルエンディアン
        rows += bytes(((v >> 11) << 3, ((v >> 5) & 63) << 2, (v & 31) << 3))
def chunk(t, d):
    return struct.pack('>I', len(d)) + t + d + struct.pack('>I', zlib.crc32(t + d))
png = (b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', W, H, 8, 2, 0, 0, 0))
       + chunk(b'IDAT', zlib.compress(bytes(rows), 6)) + chunk(b'IEND', b''))
open(out, 'wb').write(png)
print(out)
PY
