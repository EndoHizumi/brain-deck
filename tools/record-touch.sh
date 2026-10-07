#!/bin/bash
# トラックパッドの調整のために、タッチの生のイベントを動きごとに記録する。Brain 上で ~/lefthand から:
#   sudo bash tools/record-touch.sh               すべての動きを順に記録する
#   sudo bash tools/record-touch.sh tap scroll    指定した動きだけ記録する
# PC からは、端末を割り当てて実行する（Enter を押して次に進むため）:
#   ssh -t brain 'cd ~/lefthand && sudo bash tools/record-touch.sh'
#
# 記録のあいだは lefthand.service を止める（タッチパネルを専有するため。PC へのキー入力も止まる）。
# 終わったとき（Ctrl-C で止めたときも）、動いていたなら起動し直す。
# 画面には config/trackpad-example.yaml の「マウス」のレイヤーを出す（トラックパッドと、右端のスクロールの帯）。
# 記録は touch-rec/<動き>.touch に置く。PC に持ってきて、testdata/touch/ に入れる（README の「トラックパッドの調整」）。
set -e
cd "$(dirname "$0")/.."
BIN=${LEFTHAND_BIN:-./lefthand}
CFG=${CFG:-config/trackpad-example.yaml}
LAYER=${LAYER:-mouse}
OUT=${OUT:-touch-rec}

# 名前|秒|やること
GESTURES=(
  "slow|30|トラックパッドの中を、左から右へ、ゆっくり（2〜3 秒かけて）5 回なぞる。続けて、上から下へ、ゆっくり 5 回なぞる"
  "fast|20|トラックパッドの中を、左から右へ、速く（さっと払うように）5 回なぞる。続けて、右から左へ、速く 5 回なぞる"
  "tap|25|トラックパッドの真ん中あたりを、ふつうにタップする。1 回ずつ 1 秒以上あけて、10 回"
  "doubletap|25|ダブルクリックのつもりで、2 回続けてタップする。2 秒ずつあけて、6 回"
  "tapdrag|35|1 回タップして、すぐにもう一度触れ、離さずに 2〜3 cm 動かしてから離す。2 秒ずつあけて、6 回"
  "scroll|30|右端の細い帯（▲ と ▼ のある帯）を、上から下へ 4 回なぞる。続けて、下から上へ 4 回なぞる"
  "hold|30|トラックパッドに指を置き、動かさずに 3 秒止めてから離す。2 秒ずつあけて、4 回"
  "light|25|ごく軽く触れて（反応するかどうかくらいの強さで）、なぞったりタップしたりする。10 回ほど"
)

if [ "$(id -u)" != 0 ]; then
  echo "root で実行してください（sudo bash $0）" >&2
  exit 2
fi
[ -x "$BIN" ] || { echo "$BIN がありません" >&2; exit 2; }
[ -t 0 ] || { echo "端末から実行してください（ssh -t brain ...）" >&2; exit 2; }

want=" $* "
mkdir -p "$OUT"
was_active=0
if systemctl is-active --quiet lefthand.service; then
  was_active=1
  echo "lefthand.service を止めます（記録が終わったら起動し直します）"
  systemctl stop lefthand.service
fi
restart() {
  if [ "$was_active" = 1 ]; then
    systemctl start lefthand.service && echo "lefthand.service を起動し直しました"
  fi
}
trap restart EXIT

for g in "${GESTURES[@]}"; do
  IFS='|' read -r name secs what <<<"$g"
  if [ -n "$*" ] && [[ "$want" != *" $name "* ]]; then
    continue
  fi
  echo
  echo "=== $name（${secs} 秒）==="
  echo "やること：$what"
  read -r -p "準備ができたら Enter（飛ばすなら s と Enter）: " ans
  [ "$ans" = s ] && continue
  "$BIN" -record-touch "$OUT/$name.touch" -record-gesture "$name" -record-note "$what" \
    -record-layer "$LAYER" -record-for "${secs}s" "$CFG" || echo "記録に失敗しました：$name" >&2
done
chown -R "${SUDO_UID:-0}:${SUDO_GID:-0}" "$OUT" 2>/dev/null || true
echo
echo "記録したファイル："
ls -l "$OUT"
