#!/bin/bash
# USB ガジェット eth を NCM + HID キーボード + CDC-ACM の複合デバイスにして、
# usb0 に固定 IP を付ける。
#
# ethernet_gadget.service の drop-in から、Brainux 標準の enable_ethernet_gadget の
# 代わりに実行される（systemd/ethernet_gadget.service.d/lefthand.conf）。
# 標準スクリプトは最後に dhclient を実行し、PC 側に DHCP サーバーが無いため
# 約 77 秒ブロックするので、それを置き換える。
#
# 何度実行してもよい:
#   - eth が無ければ Brainux と同じ設定で作る
#   - HID/ACM が無ければ追加する（失敗しても NCM だけで接続は維持する）
#   - UDC が未接続なら接続し、usb0 に固定 IP を付ける
#
# 環境変数（動作確認用）:
#   GADGET_NAME  ガジェット名（既定 eth）
#   SKIP_BIND=1  UDC への接続と IP 設定をしない
set -e

G=/sys/kernel/config/usb_gadget/${GADGET_NAME:-eth}
UDC_NAME=ci_hdrc.0
BRAIN_IP=192.168.7.2/24   # PC側は 192.168.7.1/24（手動設定）

# Brainux の enable_ethernet_gadget と同じ値。
# host_addr を変えると PC 側のインターフェース名（enx8a158b443a01）が変わるので変えないこと
NCM_DEV_ADDR=8a:15:8b:44:3a:02
NCM_HOST_ADDR=8a:15:8b:44:3a:01

hid_ok=1

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
  if [ -n "$(cat UDC)" ]; then
    echo "" > UDC
  fi

  # 元スクリプトは VID/PID 未設定(0000:0000)なので設定する
  echo 0x1d6b > idVendor    # Linux Foundation（個人開発用）
  echo 0x0104 > idProduct   # Multifunction Composite Gadget
  # 複合デバイス(IAD)にする。NCM と ACM を Windows で正しく分けるため
  echo 0xEF > bDeviceClass
  echo 0x02 > bDeviceSubClass
  echo 0x01 > bDeviceProtocol

  # --- HID キーボード（標準ブートキーボード, 8byteレポート） ---
  # bash の printf で \x を解釈させる（dash では効かないので sh で実行しないこと）
  echo 1 > functions/hid.usb0/protocol
  echo 1 > functions/hid.usb0/subclass
  echo 8 > functions/hid.usb0/report_length
  printf '\x05\x01\x09\x06\xa1\x01\x05\x07\x19\xe0\x29\xe7\x15\x00\x25\x01\x75\x01\x95\x08\x81\x02\x95\x01\x75\x08\x81\x03\x95\x05\x75\x01\x05\x08\x19\x01\x29\x05\x91\x02\x95\x01\x75\x03\x91\x03\x95\x06\x75\x08\x15\x00\x25\x65\x05\x07\x19\x00\x29\x65\x81\x00\xc0' \
    > functions/hid.usb0/report_desc

  # --- CDC-ACM（設定GUI / シリアルログイン用 → /dev/ttyGS0） ---

  for f in hid.usb0 acm.usb0; do
    [ -e "configs/c.1/$f" ] || ln -s "functions/$f" configs/c.1/
  done
  echo "NCM+HID+ACM" > configs/c.1/strings/0x409/configuration
  echo "added: HID + ACM"
}

[ -d "$G" ] || create_eth

# 判定はリンクの有無で行う（ファンクションだけ作られて途中で失敗した場合も追加し直す）
if [ -e "$G/configs/c.1/hid.usb0" ]; then
  echo "HID/ACM は追加済みです"
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

if [ "${SKIP_BIND:-0}" = 1 ]; then
  echo "SKIP_BIND=1: UDC 接続と IP 設定を省略"
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

ls -l /dev/hidg0 /dev/ttyGS0 || echo "warning: /dev/hidg0 または /dev/ttyGS0 が見つかりません" >&2
echo "done: usb0 = $BRAIN_IP"
[ "$hid_ok" = 1 ]
