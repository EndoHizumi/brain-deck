#!/bin/bash
# USB ガジェット eth を ネットワーク（NCM か RNDIS。NET_MODE）+ HID（キーボードとマウス）+ CDC-ACM 2 つの
# 複合デバイスにして、usb0 に固定 IP を付ける。
#   hid.usb0 → /dev/hidg0：キーボード（ブートキーボード）。HID_MOUSE=1 ならキーボード（レポート ID 1）、マウス（レポート ID 2）、
#                        メディアキー（レポート ID 3）
#   acm.usb0 → /dev/ttyGS0：シリアルコンソール（getty）用
#   acm.usb1 → /dev/ttyGS1：設定 GUI（lefthand の -serial）用
# インターフェイスの番号は ネットワーク 0〜1、HID 2、ACM 3〜4（コンソール）、ACM 5〜6（設定用）。
# ネットワークは、NCM、ECM、RNDIS のどれも 2 つのインターフェイス（制御とデータ）とエンドポイント 3 本
# （IN 2 本、OUT 1 本）なので、方式を変えても、HID と ACM の番号（PC の -if03、-if05）は変わらない。
#
# マウスを別の HID のファンクションにしないのは、USB コントローラ（ci_hdrc）の IN のエンドポイントが
# 7 本しかなく、NCM 2 本、HID 1 本、ACM 4 本ですべて使っているため。足すとガジェット全体がつながらなくなる。
# キーボードとマウスを 1 つの HID に入れると、キーボードはブートキーボードの形ではなくなり、
# BIOS や UEFI の画面では使えない。そのため、既定（起動したとき）はキーボードだけ（ブートキーボード）にし、
# マウスを使うときだけ lefthand が HID_MOUSE=1 でこのスクリプトを実行して切り替える（usb_mode、set_usb_mode）。
# 起動したときからマウスを使うなら、/etc/lefthand/gadget.env に HID_MOUSE=1 と書く。
#
# ethernet_gadget.service の drop-in から、Brainux 標準の enable_ethernet_gadget の
# 代わりに実行される（systemd/ethernet_gadget.service.d/lefthand.conf）。
# 標準スクリプトは最後に dhclient を実行し、PC 側に DHCP サーバーが無いため
# 約 77 秒ブロックするので、それを置き換える。
#
# 何度実行してもよい:
#   - eth が無ければ Brainux と同じ設定で作る
#   - HID/ACM が無ければ追加する（失敗しても NCM だけで接続は維持する）
#   - 設定 GUI 用の ACM（acm.usb1）が無ければ追加する。動作中に追加するときは、
#     一度 UDC から切り離すので usb0 が数秒リンクダウンする（IP はそのまま残る）
#   - HID の形（マウスのあり・なし）が HID_MOUSE と違えば、作り直す。HID の属性はリンク中は書けないので、
#     HID と、そのあとの ACM 2 つのリンクを外し、属性を書いて、同じ順でリンクし直す（番号は変わらない）。
#     ACM のファンクションそのもの（ttyGS0、ttyGS1）は消さない
#   - 構成の名前を GADGET_TERMINAL に合わせる（端末モードでは「NCM+HID+ACM+ACM terminal」）。違えば切り離して書き換える
#   - 構成の MaxPower（PC に求める電流）を GADGET_MAX_POWER（既定 500 mA）に合わせる。違えば切り離して書き換える
#   - UDC が未接続なら接続し、usb0 に固定 IP を付ける
#
# 環境変数（動作確認用）:
#   GADGET_NAME  ガジェット名（既定 eth）
#   SKIP_BIND=1  UDC への接続と IP 設定をしない
#   HID_MOUSE    0（既定）でキーボードだけ（ブートキーボード）、1 でキーボードとマウス。/etc/lefthand/gadget.env にも書ける
#   LEFTHAND_SELF=1  lefthand から実行するとき。lefthand.service を止めない（lefthand が /dev/hidg0 を閉じてから呼ぶ）
#   GADGET_TERMINAL  1 で端末モードの構成の名前にする（lefthand の端末モードだけが渡す。起動したときは 0）
#   GADGET_MAX_POWER  PC に求める電流（mA。既定 500。USB 2.0 の上限）。/etc/lefthand/gadget.env にも書ける
#   USB_WAKEUP   1（既定）で、構成にリモートウェイクアップの印（bmAttributes の 0x20）を立てる。Brain のキーで、
#                スリープ中の PC を起こせるようにする（起こすかは PC が決める。lefthand の wakeup.go）。0 で立てない。
#                /etc/lefthand/gadget.env にも書ける。違っていれば、UDC から切り離して書き換える
#   GADGET_UNBIND_ONLY=1  UDC から切り離すだけで終わる。lefthand の端末モードが、切り離しているあいだに
#                    Brain の getty を止める・ttyGS0 を閉じるために使う（そのあと、もう一度このスクリプトで付ける）
#   NET_MODE     ネットワークの方式。ncm（既定）、ecm、rndis。/etc/lefthand/gadget.env にも書ける。
#                ncm は Linux と macOS と Windows 10 以降、rndis は Android のイーサネットテザリングや古い Windows 向け。
#                ecm はこの Brain のカーネル（CONFIG_USB_CONFIGFS_ECM なし）では作れない（作れなければ今の方式のまま）。
#                環境変数で渡さず、LEFTHAND_SELF=1 で、ガジェットにもうネットワークがあるときは、今の方式のまま
#                （lefthand の USB の形の切り替えや更新で、brain-deck net-mode で選んだ方式が戻らないように）。
#                lefthand から切り替えるときは brain-deck net-mode（環境変数で渡す）。
#                方式を変えるときは、ネットワークのファンクションを作り直す（usb0 は消えて、同じ名前・同じ MAC で
#                でき直す）。HID と ACM はリンクを外して付け直すだけで、ファンクションは消さない
#   NET_MAC      相手（PC やスマホ）から見た Brain の MAC（host_addr）。pc（既定。8a:15:8b:44:3a:01）か phone
#                （88:15:8b:44:3a:01）。/etc/lefthand/gadget.env にも書ける。決め方は NET_MODE と同じ（環境変数 >
#                lefthand からなら今のもの > gadget.env > pc）。
#                pc の値は「ローカルに割り当てた」印（先頭のバイトの 0x02）が立っているので、相手の Linux のカーネルは
#                ネットワークの名前を usb0 にする。PC は udev で enx8a158b443a01 に付け直すが、Android は付け直さず、
#                イーサネットテザリングは eth0 のような名前しか扱わない。phone は印を立てない値で、相手のカーネルは
#                eth0 にする（2026-10-15、Xperia 1 IV の Android 15 で、NCM と phone でだけテザリングの対象になった）。
#                phone のとき、PC のネットワークの名前は enx88158b443a01 に変わる
#   GADGET_CHECK=1   何も変えずに、このスクリプトを実行したら USB を付け直すかを調べる。付け直すなら理由を
#                    「rebind: …」の行に書いて終了コード 10、今のままでよければ 0。brain-deck update が、
#                    入れ替える前に知らせるために使う（lefthand の update.go）
set -e

GADGET_ENV=${GADGET_ENV:-/etc/lefthand/gadget.env}
# shellcheck disable=SC1090
# 環境変数で HID_MOUSE を渡したときは、そちらを使う（lefthand からの切り替え）
want_mouse=${HID_MOUSE:-}
want_wakeup=${USB_WAKEUP:-}
want_net=${NET_MODE:-}
want_mac=${NET_MAC:-}
[ -r "$GADGET_ENV" ] && . "$GADGET_ENV"
HID_MOUSE=${want_mouse:-${HID_MOUSE:-0}}
USB_WAKEUP=${want_wakeup:-${USB_WAKEUP:-1}}
want_power=${GADGET_MAX_POWER:-}
MAX_POWER=${want_power:-500}
case "$MAX_POWER" in
  [0-9]|[0-9][0-9]|[0-9][0-9][0-9]) [ "$MAX_POWER" -le 500 ] || MAX_POWER=500 ;;
  *) echo "GADGET_MAX_POWER=$MAX_POWER は mA の数ではありません。500 にします" >&2; MAX_POWER=500 ;;
esac

G=/sys/kernel/config/usb_gadget/${GADGET_NAME:-eth}
UDC_NAME=ci_hdrc.0
BRAIN_IP=192.168.7.2/24   # PC側は 192.168.7.1/24（手動設定）

# net_linked は、構成にリンクしているネットワークのファンクションの名前（ncm.usb0 など）。なければ空
net_linked() {
  local f
  for f in ncm.usb0 ecm.usb0 rndis.usb0; do
    [ -e "$G/configs/c.1/$f" ] && { echo "$f"; return 0; }
  done
  return 0
}

# ネットワークの方式。環境変数 > （lefthand から呼ばれ、もうあれば）今の方式 > gadget.env > ncm
gadget_env_net=${NET_MODE:-}
if [ -n "$want_net" ]; then
  NET_MODE=$want_net
elif [ "${LEFTHAND_SELF:-0}" = 1 ] && [ -n "$(net_linked)" ]; then
  NET_MODE=$(net_linked)
  NET_MODE=${NET_MODE%.usb0}
else
  NET_MODE=${gadget_env_net:-ncm}
fi
case "$NET_MODE" in
  ncm|ecm|rndis) ;;
  *) echo "NET_MODE=$NET_MODE は ncm、ecm、rndis のどれでもありません。ncm にします" >&2; NET_MODE=ncm ;;
esac
net_ok=1

# MAC の決め方も NET_MODE と同じ。今のものは、リンクしているネットワークの host_addr から読む
NET_MAC_PC=8a:15:8b:44:3a:01
NET_MAC_PHONE=88:15:8b:44:3a:01
# cur_host_addr は、リンクしているネットワークの host_addr（configfs は最後に NUL を付けて返すので消す）
cur_host_addr() {
  local f
  f=$(net_linked)
  [ -n "$f" ] && tr -d '\0\n' < "$G/functions/$f/host_addr"
  return 0
}
gadget_env_mac=${NET_MAC:-}
if [ -n "$want_mac" ]; then
  NET_MAC=$want_mac
elif [ "${LEFTHAND_SELF:-0}" = 1 ] && [ -n "$(net_linked)" ]; then
  case "$(cur_host_addr)" in
    "$NET_MAC_PHONE") NET_MAC=phone ;;
    *) NET_MAC=pc ;;
  esac
else
  NET_MAC=${gadget_env_mac:-pc}
fi
case "$NET_MAC" in
  pc) NET_HOST=$NET_MAC_PC ;;
  phone) NET_HOST=$NET_MAC_PHONE ;;
  *) echo "NET_MAC=$NET_MAC は pc か phone ではありません。pc にします" >&2; NET_MAC=pc; NET_HOST=$NET_MAC_PC ;;
esac

# Brainux の enable_ethernet_gadget と同じ値。
# host_addr を変えると PC 側のインターフェース名（enx8a158b443a01）が変わるので変えないこと
# NCM のほか、ECM と RNDIS でも同じ値にする（PC は MAC から名前を付けるので、方式を変えても同じ名前になる）。
# 相手から見た MAC（host_addr）は NET_MAC で選ぶ（NET_HOST。既定は pc の 8a:15:8b:44:3a:01）
NCM_DEV_ADDR=8a:15:8b:44:3a:02

# HID のレポートディスクリプタ。lefthand の hid.go の hidDescKeyboard、hidDescCombo と同じ（テストで比べている）。
# bash の printf で \x を解釈させる（dash では効かないので sh で実行しないこと）
# キーボードだけ（標準のブートキーボード、8 バイトのレポート）。キーの番号は F24（0x73）まで（hid.go の hidKeyMax）
KBD_DESC='\x05\x01\x09\x06\xa1\x01\x05\x07\x19\xe0\x29\xe7\x15\x00\x25\x01\x75\x01\x95\x08\x81\x02\x95\x01\x75\x08\x81\x03\x95\x05\x75\x01\x05\x08\x19\x01\x29\x05\x91\x02\x95\x01\x75\x03\x91\x03\x95\x06\x75\x08\x15\x00\x25\x73\x05\x07\x19\x00\x29\x73\x81\x00\xc0'
# キーボード（レポート ID 1、9 バイト）、マウス（レポート ID 2、6 バイト：ボタン 3 つ、X、Y、ホイール、横のホイール）、
# メディアキー（レポート ID 3、5 バイト：Consumer Control の番号 2 つ）、ペン（レポート ID 4、8 バイト：Tip Switch と In Range、X、Y、Tip Pressure。pen.go）
COMBO_DESC='\x05\x01\x09\x06\xa1\x01\x85\x01\x05\x07\x19\xe0\x29\xe7\x15\x00\x25\x01\x75\x01\x95\x08\x81\x02\x95\x01\x75\x08\x81\x03\x95\x05\x75\x01\x05\x08\x19\x01\x29\x05\x91\x02\x95\x01\x75\x03\x91\x03\x95\x06\x75\x08\x15\x00\x25\x73\x05\x07\x19\x00\x29\x73\x81\x00\xc0'\
'\x05\x01\x09\x02\xa1\x01\x85\x02\x09\x01\xa1\x00\x05\x09\x19\x01\x29\x03\x15\x00\x25\x01\x95\x03\x75\x01\x81\x02\x95\x01\x75\x05\x81\x03'\
'\x05\x01\x09\x30\x09\x31\x09\x38\x15\x81\x25\x7f\x75\x08\x95\x03\x81\x06\x05\x0c\x0a\x38\x02\x15\x81\x25\x7f\x75\x08\x95\x01\x81\x06\xc0\xc0'\
'\x05\x0c\x09\x01\xa1\x01\x85\x03\x15\x00\x26\xff\x03\x19\x00\x2a\xff\x03\x75\x10\x95\x02\x81\x00\xc0'\
'\x05\x0d\x09\x01\xa1\x01\x85\x04\x09\x20\xa1\x00\x09\x42\x09\x32\x15\x00\x25\x01\x75\x01\x95\x02\x81\x02\x95\x06\x81\x03\x05\x01\x26\xff\x7f\x75\x10\x95\x01\x55\x0e\x65\x11\x35\x00\x46\x38\x04\x09\x30\x81\x02\x46\x8a\x02\x09\x31\x81\x02\x45\x00\x65\x00\x55\x00\x05\x0d\x09\x30\x26\xff\x03\x81\x02\xc0\xc0'

if [ "$HID_MOUSE" = 1 ]; then
  HID_DESC=$COMBO_DESC HID_PROTOCOL=0 HID_SUBCLASS=0 HID_LEN=9 HID_KIND="keyboard+mouse"
else
  HID_DESC=$KBD_DESC HID_PROTOCOL=1 HID_SUBCLASS=1 HID_LEN=8 HID_KIND="keyboard"
fi

hid_ok=1
# 付け直しのために止めた lefthand.service を、最後に再開する印（サブシェルからも分かるようにファイルにする）
RESTART_MARK=/run/lefthand-gadget-restart

# unbind_udc は UDC から切り離す。動作中に付け直すときだけ切り離す（起動時は未接続）。
# このカーネル（6.1）の f_hid は、/dev/hidg0 を開いたまま付け直すと、そのあと ENXIO で開けなくなる。
# そのため、先に lefthand.service を止めて hidg0 を閉じさせる
unbind_udc() {
  [ -n "$(cat "$G/UDC")" ] || return 0
  if [ "${LEFTHAND_SELF:-0}" != 1 ] && systemctl is-active --quiet lefthand.service; then
    echo "lefthand.service を止めてから付け直します"
    systemctl stop lefthand.service
    touch "$RESTART_MARK"
  fi
  echo "" > "$G/UDC"
}

# 切り離すだけ（端末モードの切り替えの前半）。USB がつながっていないあいだに ttyGS0 を閉じると、
# u_serial は送っていない出力を待たずに捨てる（つながっていれば、PC が読むまで最大 15 秒待ち、
# 開いたまま付け直すと、残った出力を次の接続で PC に送ってしまう）
if [ "${GADGET_UNBIND_ONLY:-0}" = 1 ]; then
  [ -d "$G" ] && unbind_udc
  echo "unbound"
  exit 0
fi

# conf_label は、構成の名前の先頭（今リンクしているネットワークの方式。NCM、ECM、RNDIS）
conf_label() {
  case "$(net_linked)" in
    ecm.usb0) echo ECM ;;
    rndis.usb0) echo RNDIS ;;
    *) echo NCM ;;
  esac
}

# make_net は、ネットワークのファンクション（$1 は ncm、ecm、rndis）を作り、MAC を書く。リンクはしない。
# 作れなければ（カーネルにない）1 を返す。RNDIS のときは、Windows が自分の RNDIS のドライバーを使うように、
# Microsoft OS ディスクリプタ（os_desc。互換 ID RNDIS、5162001）も書く。IAD は f_rndis が自分で付ける
make_net() {
  local f="$G/functions/$1.usb0"
  if [ ! -d "$f" ] && ! mkdir "$f" 2>/dev/null; then
    echo "functions/$1.usb0 を作成できません。カーネルが対応していない可能性があります" >&2
    return 1
  fi
  echo "$NCM_DEV_ADDR" > "$f/dev_addr"
  echo "$NET_HOST" > "$f/host_addr"
  if [ "$1" = rndis ]; then
    echo RNDIS > "$f/os_desc/interface.rndis/compatible_id"
    echo 5162001 > "$f/os_desc/interface.rndis/sub_compatible_id"
  fi
}

# set_os_desc は、ガジェットの os_desc を、RNDIS のときだけ使う（UDC から切り離しているときだけ書ける）
set_os_desc() {
  [ -d "$G/os_desc" ] || return 0
  if [ "$1" = rndis ]; then
    echo 0xcd > "$G/os_desc/b_vendor_code"
    echo MSFT100 > "$G/os_desc/qw_sign"
    echo 1 > "$G/os_desc/use"
    [ -e "$G/os_desc/c.1" ] || ln -s "$G/configs/c.1" "$G/os_desc/c.1"
  else
    [ -e "$G/os_desc/c.1" ] && rm "$G/os_desc/c.1"
    echo 0 > "$G/os_desc/use"
  fi
  return 0
}

create_eth() {
  modprobe libcomposite 2>/dev/null || true
  mkdir "$G"
  cd "$G"
  echo 0x0200 > bcdUSB
  echo 0x0200 > bcdDevice
  mkdir -p strings/0x409
  echo "0123456789" > strings/0x409/serialnumber
  echo "SHARP" > strings/0x409/manufacturer
  echo "Brain" > strings/0x409/product
  mkdir -p configs/c.1/strings/0x409
  echo "$MAX_POWER" > configs/c.1/MaxPower
  # 方式のファンクションを作れなければ、NCM にする（SSH できる状態を必ず残す）
  if ! make_net "$NET_MODE"; then
    echo "$NET_MODE を作れないので、NCM にします" >&2
    net_ok=0
    NET_MODE=ncm
    make_net ncm
  fi
  set_os_desc "$NET_MODE"
  ln -s "functions/$NET_MODE.usb0" configs/c.1/
  echo "created: $G ($NET_MODE)"
}

add_hid_acm() {
  cd "$G"

  # 必要なモジュールを読み込む（ビルトインなら何もしない）
  modprobe usb_f_hid 2>/dev/null || true
  modprobe usb_f_acm 2>/dev/null || true

  # 接続を切る前に、ファンクションが作れるか確認する
  # （作れない＝カーネルに CONFIG_USB_CONFIGFS_F_HID / F_ACM が無い）
  for f in hid.usb0 acm.usb0; do
    [ -d "functions/$f" ] && continue
    if ! mkdir "functions/$f" 2>/dev/null; then
      echo "functions/$f を作成できません。カーネルが対応していない可能性があります" >&2
      rmdir functions/hid.usb0 functions/acm.usb0 2>/dev/null || true
      return 1
    fi
  done

  # 接続済みなら一旦切る（usb0 が一瞬リンクダウンする）。起動時は未接続なので切れない
  unbind_udc

  # 元スクリプトは VID/PID 未設定(0000:0000)なので設定する
  echo 0x1d6b > idVendor    # Linux Foundation（個人開発用）
  echo 0x0104 > idProduct   # Multifunction Composite Gadget
  # 複合デバイス(IAD)にする。NCM と ACM を Windows で正しく分けるため
  echo 0xEF > bDeviceClass
  echo 0x02 > bDeviceSubClass
  echo 0x01 > bDeviceProtocol

  # --- HID（キーボードとマウス。HID_MOUSE=0 ならキーボードだけ） ---
  write_hid_attrs

  # --- CDC-ACM（シリアルログイン用 → /dev/ttyGS0） ---

  for f in hid.usb0 acm.usb0; do
    [ -e "configs/c.1/$f" ] || ln -s "functions/$f" configs/c.1/
  done
  echo "$(conf_label)+HID+ACM" > configs/c.1/strings/0x409/configuration
  echo "added: HID + ACM"
}

# write_hid_attrs は、HID の属性を HID_MOUSE の形にする。HID がリンクされていないときだけ書ける。
# report_desc は、書くたびに中身を置き換える（足さない）。bash の printf は長いと何回かに分けて書くので、
# 一時ファイルに作ってから、dd で 1 回の write で書く
write_hid_attrs() {
  local tmp=/run/lefthand-hid-desc.$$
  echo "$HID_PROTOCOL" > "$G/functions/hid.usb0/protocol"
  echo "$HID_SUBCLASS" > "$G/functions/hid.usb0/subclass"
  echo "$HID_LEN" > "$G/functions/hid.usb0/report_length"
  # shellcheck disable=SC2059
  printf "$HID_DESC" > "$tmp"
  dd if="$tmp" of="$G/functions/hid.usb0/report_desc" bs=4096 count=1 status=none
  local rc=0
  # configfs のファイルは大きさが 4096 に見え、cmp は中身を読まずに違うと判断するので、cat を通す
  cat "$G/functions/hid.usb0/report_desc" | cmp -s "$tmp" - || rc=1
  rm -f "$tmp"
  if [ "$rc" != 0 ]; then
    echo "report_desc を書けませんでした" >&2
    return 1
  fi
}

# hid_matches は、HID の属性が HID_MOUSE の形になっているか
hid_matches() {
  local want=/run/lefthand-hid-desc.$$ rc=0
  # shellcheck disable=SC2059
  printf "$HID_DESC" > "$want"
  cat "$G/functions/hid.usb0/report_desc" | cmp -s "$want" - || rc=1
  rm -f "$want"
  [ "$rc" = 0 ] && [ "$(cat "$G/functions/hid.usb0/report_length")" = "$HID_LEN" ] &&
    [ "$(cat "$G/functions/hid.usb0/protocol")" = "$HID_PROTOCOL" ]
}

# LINK_ORDER は、ネットワークのあとにリンクするファンクションの順（インターフェイスの番号を決める）
LINK_ORDER="hid.usb0 acm.usb0 acm.usb1"

# update_hid は、リンク済みの HID の形を変える。HID の属性はリンク中は書けない（f_hid が EBUSY を返す）ので、
# HID と、そのあとにリンクしたファンクションのリンクを外し、属性を書いて、同じ順でリンクし直す。
# ファンクションそのものは消さない（ttyGS0、ttyGS1 はそのまま残る）。
update_hid() {
  cd "$G"
  unbind_udc
  local linked="" f rc=0
  for f in $LINK_ORDER; do
    if [ -e "configs/c.1/$f" ]; then
      linked="$linked $f"
      rm "configs/c.1/$f"
    fi
  done
  write_hid_attrs || rc=$?
  # 属性を書けなくても、リンクは必ず元に戻す
  for f in $linked; do
    ln -s "functions/$f" configs/c.1/ || rc=$?
  done
  [ "$rc" = 0 ] && echo "updated: HID is now $HID_KIND"
  return "$rc"
}

# 設定 GUI 用の 2 つ目の ACM（/dev/ttyGS1）。acm.usb0 より後にリンクするので、
# PC 側では ttyGS0 が先の番号（Linux なら /dev/ttyACM0）、こちらが次の番号（/dev/ttyACM1）になる
add_acm_gui() {
  cd "$G"
  [ -e configs/c.1/acm.usb1 ] && return 0
  if ! mkdir -p functions/acm.usb1 2>/dev/null; then
    echo "functions/acm.usb1 を作成できません（ACM のポート数の上限?）" >&2
    return 1
  fi
  unbind_udc
  ln -s functions/acm.usb1 configs/c.1/
  echo "$(conf_label)+HID+ACM+ACM" > configs/c.1/strings/0x409/configuration
  echo "added: ACM for the settings GUI (port $(cat functions/acm.usb1/port_num))"
}

# switch_net は、ネットワークの方式を NET_MODE に、相手から見た MAC を NET_HOST に変える。方式が変わるときは、
# 新しいファンクションを先に作って（作れなければ何も変えない）、UDC から切り離し、全部のリンクを外し、
# 古いネットワークのファンクションを消し、新しいものを先頭に、ほかを同じ順でリンクし直す（インターフェイスの
# 番号は変わらない）。古いものを消すのは、残すと usb0 の名前を持ったままで、新しいものが usb1 になるため。
# 方式が同じで MAC だけが変わるときは、リンクを外しているあいだに host_addr を書き直す（リンク中は書けない）。
# ACM のファンクションは消さない（ttyGS を開いている getty で rmdir が止まる）
switch_net() {
  cd "$G"
  local old f linked="" rc=0
  old=$(net_linked)
  if [ "$old" != "$NET_MODE.usb0" ]; then
    make_net "$NET_MODE" || return 1
  fi
  unbind_udc
  [ -n "$old" ] && rm "configs/c.1/$old"
  for f in $LINK_ORDER; do
    if [ -e "configs/c.1/$f" ]; then
      linked="$linked $f"
      rm "configs/c.1/$f"
    fi
  done
  set_os_desc "$NET_MODE" || rc=$?
  if [ "$old" = "$NET_MODE.usb0" ]; then
    echo "$NET_HOST" > "functions/$old/host_addr" || rc=$?
  elif [ -n "$old" ]; then
    rmdir "functions/$old" || rc=$?
  fi
  ln -s "functions/$NET_MODE.usb0" configs/c.1/ || rc=$?
  # 失敗しても、リンクは必ず元に戻す
  for f in $linked; do
    ln -s "functions/$f" configs/c.1/ || rc=$?
  done
  [ "$rc" = 0 ] && echo "updated: network is now $NET_MODE, MAC $NET_MAC ($NET_HOST) (was ${old%.usb0})"
  return "$rc"
}

# 何も変えずに、付け直しが要るかを調べる（GADGET_CHECK=1）
if [ "${GADGET_CHECK:-0}" = 1 ]; then
  need=0
  reason() { echo "rebind: $*"; need=1; }
  if [ ! -d "$G" ]; then
    reason "the gadget $G does not exist"
  else
    if [ ! -e "$G/configs/c.1/hid.usb0" ]; then
      reason "HID and ACM are not linked"
    elif ! hid_matches; then
      reason "the HID report descriptor or attributes differ (want $HID_KIND)"
    fi
    [ -e "$G/configs/c.1/acm.usb1" ] || reason "the ACM for the settings GUI is not linked"
    [ "$(net_linked)" = "$NET_MODE.usb0" ] || reason "the network function differs (want $NET_MODE, now $(net_linked))"
    [ "$(cur_host_addr)" = "$NET_HOST" ] || reason "the host MAC differs (want $NET_MAC $NET_HOST, now $(cur_host_addr))"
    want_conf="$(echo "$NET_MODE" | tr a-z A-Z)+HID+ACM+ACM"
    [ "${GADGET_TERMINAL:-0}" = 1 ] && want_conf="$want_conf terminal"
    [ "$(cat "$G/configs/c.1/strings/0x409/configuration" 2>/dev/null)" = "$want_conf" ] || reason "the configuration name differs (want $want_conf)"
    want_attr=0x80
    [ "$USB_WAKEUP" = 1 ] && want_attr=0xa0
    [ "$(cat "$G/configs/c.1/bmAttributes" 2>/dev/null)" = "$want_attr" ] || reason "bmAttributes differs (want $want_attr)"
    [ "$(cat "$G/configs/c.1/MaxPower" 2>/dev/null)" = "$MAX_POWER" ] || reason "MaxPower differs (want $MAX_POWER)"
    [ -n "$(cat "$G/UDC" 2>/dev/null)" ] || reason "the gadget is not bound to the UDC"
  fi
  if [ "$need" = 1 ]; then
    exit 10
  fi
  echo "up to date: HID = $HID_KIND, network = $NET_MODE, MAC = $NET_MAC"
  exit 0
fi

[ -d "$G" ] || create_eth

# ネットワークの方式か MAC が違えば作り直す。作れない方式（ecm など）なら、今の方式のまま続ける
if [ -n "$(net_linked)" ] && { [ "$(net_linked)" != "$NET_MODE.usb0" ] || [ "$(cur_host_addr)" != "$NET_HOST" ]; }; then
  echo "ネットワークを $NET_MODE（MAC $NET_MAC）にします"
  set +e
  (set -e; switch_net)
  rc=$?
  set -e
  if [ "$rc" != 0 ]; then
    echo "ネットワークを $NET_MODE にできませんでした（$(net_linked) のまま続けます）" >&2
    net_ok=0
  fi
fi

# 判定はリンクの有無で行う（ファンクションだけ作られて途中で失敗した場合も追加し直す）
if [ -e "$G/configs/c.1/hid.usb0" ]; then
  echo "HID/ACM は追加済みです"
  if ! hid_matches; then
    echo "HID を $HID_KIND の形にします"
    set +e
    (set -e; update_hid)
    rc=$?
    set -e
    if [ "$rc" != 0 ]; then
      echo "HID の形を変えられませんでした（前の形のまま続けます）" >&2
    fi
  fi
else
  # if や ! の中で呼ぶと関数内の set -e が無効になるので、サブシェルの終了コードで判定する
  set +e
  (set -e; add_hid_acm)
  rc=$?
  set -e
  if [ "$rc" != 0 ]; then
    # HID が使えなくても、NCM で SSH できる状態は必ず残す
    echo "HID/ACM を追加できませんでした。NCM のみで続けます" >&2
    hid_ok=0
  fi
fi

# GUI 用の ACM は HID が使えるときだけ足す。失敗しても、ほかの機能はそのまま使う
if [ "$hid_ok" = 1 ]; then
  set +e
  (set -e; add_acm_gui)
  rc=$?
  set -e
  if [ "$rc" != 0 ]; then
    echo "設定 GUI 用の ACM を追加できませんでした。GUI は使えませんが、ほかは動きます" >&2
  fi
fi

# 構成の名前（先頭はネットワークの方式。NCM、ECM、RNDIS）。端末モード（lefthand の termmode.go が GADGET_TERMINAL=1 で実行する）では、最後に " terminal" を付ける。
# PC の udev の規則（contrib/udev/71-brain-terminal.rules）は、この名前のときだけ 1 つ目のシリアル（-if03）で
# getty を起動する。ふだんの名前では何も起動しないので、Brain 側の getty（ttyGS0）とぶつからない。
# 名前は付け直したときに PC に伝わるので、違っていれば UDC から切り離して書き換える
set_conf_name() {
  [ -e "$G/configs/c.1/acm.usb1" ] || return 0
  local want f="$G/configs/c.1/strings/0x409/configuration"
  want="$(conf_label)+HID+ACM+ACM"
  [ "${GADGET_TERMINAL:-0}" = 1 ] && want="$want terminal"
  [ "$(cat "$f")" = "$want" ] && return 0
  unbind_udc
  echo "$want" > "$f"
  echo "configuration: $want"
}
if [ "$hid_ok" = 1 ]; then
  set_conf_name || echo "構成の名前を変えられませんでした" >&2
fi

# 構成の bmAttributes。0x80 は「バスから電源を取る」（USB 2.0 で必ず立てる）、0x20 は「リモートウェイクアップできる」。
# PC は、この印があるときだけ、スリープの前にリモートウェイクアップを許す（SET_FEATURE）。
# 印は PC がつないだときに読むので、違っていれば UDC から切り離して書き換える
set_wakeup() {
  local want=0x80 f="$G/configs/c.1/bmAttributes"
  [ "$USB_WAKEUP" = 1 ] && want=0xa0
  [ "$(cat "$f")" = "$want" ] && return 0
  unbind_udc
  echo "$want" > "$f"
  echo "bmAttributes: $want (remote wakeup $([ "$want" = 0xa0 ] && echo on || echo off))"
}
set_wakeup || echo "bmAttributes を書けませんでした（スリープ中の PC を起こせません）" >&2

# 構成の MaxPower（mA）。PC はつないだときにこの値を読み、それより多く流さないことがある（ハブなど）。
# 250 だった（Brainux の enable_ethernet_gadget と同じ）。電池が減り続けていたので、USB 2.0 の上限の 500 にした
set_max_power() {
  local f="$G/configs/c.1/MaxPower"
  [ "$(cat "$f")" = "$MAX_POWER" ] && return 0
  unbind_udc
  echo "$MAX_POWER" > "$f"
  echo "MaxPower: $MAX_POWER mA"
}
set_max_power || echo "MaxPower を書けませんでした" >&2

if [ "${SKIP_BIND:-0}" = 1 ]; then
  echo "SKIP_BIND=1: UDC 接続と IP 設定を省略"
  if [ -e "$RESTART_MARK" ]; then
    rm -f "$RESTART_MARK"
    systemctl start --no-block lefthand.service
  fi
  [ "$hid_ok" = 1 ] && [ "$net_ok" = 1 ]
  exit
fi

if [ -z "$(cat "$G/UDC")" ]; then
  echo "$UDC_NAME" > "$G/UDC"
  sleep 1
fi

# usb0 に固定IP（PC 側に DHCP サーバーは無い）。毎回行う
ip link set usb0 up
ip addr add "$BRAIN_IP" dev usb0 2>/dev/null || true   # 付与済みなら何もしない

if [ -e "$RESTART_MARK" ]; then
  rm -f "$RESTART_MARK"
  systemctl start --no-block lefthand.service
  echo "lefthand.service を再開しました"
fi

ls -l /dev/hidg0 /dev/ttyGS0 /dev/ttyGS1 || echo "warning: /dev/hidg0、/dev/ttyGS0、/dev/ttyGS1 のどれかが見つかりません" >&2
echo "done: usb0 = $BRAIN_IP, network = $(conf_label), MAC = $(cur_host_addr), HID = $HID_KIND, remote wakeup = $([ "$USB_WAKEUP" = 1 ] && echo on || echo off)"
[ "$hid_ok" = 1 ] && [ "$net_ok" = 1 ]
