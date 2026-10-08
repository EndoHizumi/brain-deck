#!/bin/bash
# USB ガジェット eth を NCM + HID（キーボードとマウス）+ CDC-ACM 2 つの複合デバイスにして、
# usb0 に固定 IP を付ける。
#   hid.usb0 → /dev/hidg0：キーボード（ブートキーボード）。HID_MOUSE=1 ならキーボード（レポート ID 1）とマウス（レポート ID 2）
#   acm.usb0 → /dev/ttyGS0：シリアルコンソール（getty）用
#   acm.usb1 → /dev/ttyGS1：設定 GUI（lefthand の -serial）用
# インターフェイスの番号は NCM 0〜1、HID 2、ACM 3〜4（コンソール）、ACM 5〜6（設定用）。
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
#   - UDC が未接続なら接続し、usb0 に固定 IP を付ける
#
# 環境変数（動作確認用）:
#   GADGET_NAME  ガジェット名（既定 eth）
#   SKIP_BIND=1  UDC への接続と IP 設定をしない
#   HID_MOUSE    0（既定）でキーボードだけ（ブートキーボード）、1 でキーボードとマウス。/etc/lefthand/gadget.env にも書ける
#   LEFTHAND_SELF=1  lefthand から実行するとき。lefthand.service を止めない（lefthand が /dev/hidg0 を閉じてから呼ぶ）
#   GADGET_TERMINAL  1 で端末モードの構成の名前にする（lefthand の端末モードだけが渡す。起動したときは 0）
#   GADGET_UNBIND_ONLY=1  UDC から切り離すだけで終わる。lefthand の端末モードが、切り離しているあいだに
#                    Brain の getty を止める・ttyGS0 を閉じるために使う（そのあと、もう一度このスクリプトで付ける）
set -e

GADGET_ENV=${GADGET_ENV:-/etc/lefthand/gadget.env}
# shellcheck disable=SC1090
# 環境変数で HID_MOUSE を渡したときは、そちらを使う（lefthand からの切り替え）
want_mouse=${HID_MOUSE:-}
[ -r "$GADGET_ENV" ] && . "$GADGET_ENV"
HID_MOUSE=${want_mouse:-${HID_MOUSE:-0}}

G=/sys/kernel/config/usb_gadget/${GADGET_NAME:-eth}
UDC_NAME=ci_hdrc.0
BRAIN_IP=192.168.7.2/24   # PC側は 192.168.7.1/24（手動設定）

# Brainux の enable_ethernet_gadget と同じ値。
# host_addr を変えると PC 側のインターフェース名（enx8a158b443a01）が変わるので変えないこと
NCM_DEV_ADDR=8a:15:8b:44:3a:02
NCM_HOST_ADDR=8a:15:8b:44:3a:01

# HID のレポートディスクリプタ。lefthand の hid.go の hidDescKeyboard、hidDescCombo と同じ（テストで比べている）。
# bash の printf で \x を解釈させる（dash では効かないので sh で実行しないこと）
# キーボードだけ（標準のブートキーボード、8 バイトのレポート）
KBD_DESC='\x05\x01\x09\x06\xa1\x01\x05\x07\x19\xe0\x29\xe7\x15\x00\x25\x01\x75\x01\x95\x08\x81\x02\x95\x01\x75\x08\x81\x03\x95\x05\x75\x01\x05\x08\x19\x01\x29\x05\x91\x02\x95\x01\x75\x03\x91\x03\x95\x06\x75\x08\x15\x00\x25\x65\x05\x07\x19\x00\x29\x65\x81\x00\xc0'
# キーボード（レポート ID 1、9 バイト）とマウス（レポート ID 2、6 バイト：ボタン 3 つ、X、Y、ホイール、横のホイール）
COMBO_DESC='\x05\x01\x09\x06\xa1\x01\x85\x01\x05\x07\x19\xe0\x29\xe7\x15\x00\x25\x01\x75\x01\x95\x08\x81\x02\x95\x01\x75\x08\x81\x03\x95\x05\x75\x01\x05\x08\x19\x01\x29\x05\x91\x02\x95\x01\x75\x03\x91\x03\x95\x06\x75\x08\x15\x00\x25\x65\x05\x07\x19\x00\x29\x65\x81\x00\xc0'\
'\x05\x01\x09\x02\xa1\x01\x85\x02\x09\x01\xa1\x00\x05\x09\x19\x01\x29\x03\x15\x00\x25\x01\x95\x03\x75\x01\x81\x02\x95\x01\x75\x05\x81\x03'\
'\x05\x01\x09\x30\x09\x31\x09\x38\x15\x81\x25\x7f\x75\x08\x95\x03\x81\x06\x05\x0c\x0a\x38\x02\x15\x81\x25\x7f\x75\x08\x95\x01\x81\x06\xc0\xc0'

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
  echo 250 > configs/c.1/MaxPower
  mkdir functions/ncm.usb0
  echo "$NCM_DEV_ADDR" > functions/ncm.usb0/dev_addr
  echo "$NCM_HOST_ADDR" > functions/ncm.usb0/host_addr
  ln -s functions/ncm.usb0 configs/c.1/
  echo "created: $G (NCM)"
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
  echo "NCM+HID+ACM" > configs/c.1/strings/0x409/configuration
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

# LINK_ORDER は、NCM のあとにリンクするファンクションの順（インターフェイスの番号を決める）
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
  echo "NCM+HID+ACM+ACM" > configs/c.1/strings/0x409/configuration
  echo "added: ACM for the settings GUI (port $(cat functions/acm.usb1/port_num))"
}

[ -d "$G" ] || create_eth

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

# 構成の名前。端末モード（lefthand の termmode.go が GADGET_TERMINAL=1 で実行する）では、最後に " terminal" を付ける。
# PC の udev の規則（contrib/udev/71-brain-terminal.rules）は、この名前のときだけ 1 つ目のシリアル（-if03）で
# getty を起動する。ふだんの名前では何も起動しないので、Brain 側の getty（ttyGS0）とぶつからない。
# 名前は付け直したときに PC に伝わるので、違っていれば UDC から切り離して書き換える
set_conf_name() {
  [ -e "$G/configs/c.1/acm.usb1" ] || return 0
  local want="NCM+HID+ACM+ACM" f="$G/configs/c.1/strings/0x409/configuration"
  [ "${GADGET_TERMINAL:-0}" = 1 ] && want="$want terminal"
  [ "$(cat "$f")" = "$want" ] && return 0
  unbind_udc
  echo "$want" > "$f"
  echo "configuration: $want"
}
if [ "$hid_ok" = 1 ]; then
  set_conf_name || echo "構成の名前を変えられませんでした" >&2
fi

if [ "${SKIP_BIND:-0}" = 1 ]; then
  echo "SKIP_BIND=1: UDC 接続と IP 設定を省略"
  if [ -e "$RESTART_MARK" ]; then
    rm -f "$RESTART_MARK"
    systemctl start --no-block lefthand.service
  fi
  [ "$hid_ok" = 1 ]
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
echo "done: usb0 = $BRAIN_IP, HID = $HID_KIND"
[ "$hid_ok" = 1 ]
