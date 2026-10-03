#!/bin/bash
# Brainux標準のNCMガジェット(eth)に HIDキーボード + CDC-ACM を追加して
# NCM + HID + ACM の複合デバイスにする。
# enable_ethernet_gadget が実行済みであることが前提。
set -e

G=/sys/kernel/config/usb_gadget/eth
UDC_NAME=ci_hdrc.0
BRAIN_IP=192.168.7.2/24   # PC側は 192.168.7.1/24 を設定

if [ ! -d "$G" ]; then
  echo "$G がありません。enable_ethernet_gadget が動いていないかも" >&2
  exit 1
fi
if [ -d "$G/functions/hid.usb0" ]; then
  echo "HID/ACM は追加済みです"
  exit 0
fi

cd "$G"

# 必要なモジュールを読み込む（ビルトインなら何もしない）
modprobe usb_f_hid 2>/dev/null || true
modprobe usb_f_acm 2>/dev/null || true

# 接続を切る前に、ファンクションが作れるか確認する
# （作れない＝カーネルに CONFIG_USB_CONFIGFS_F_HID / F_ACM が無い）
for f in hid.usb0 acm.usb0; do
  if ! mkdir "functions/$f" 2>/dev/null; then
    echo "functions/$f を作成できません。カーネルが対応していない可能性があります" >&2
    rmdir functions/hid.usb0 functions/acm.usb0 2>/dev/null || true
    exit 1
  fi
done

# ここから接続を一旦切る（usb0 が一瞬リンクダウンする）
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
echo 1 > functions/hid.usb0/protocol
echo 1 > functions/hid.usb0/subclass
echo 8 > functions/hid.usb0/report_length
printf '\x05\x01\x09\x06\xa1\x01\x05\x07\x19\xe0\x29\xe7\x15\x00\x25\x01\x75\x01\x95\x08\x81\x02\x95\x01\x75\x08\x81\x03\x95\x05\x75\x01\x05\x08\x19\x01\x29\x05\x91\x02\x95\x01\x75\x03\x91\x03\x95\x06\x75\x08\x15\x00\x25\x65\x05\x07\x19\x00\x29\x65\x81\x00\xc0' \
  > functions/hid.usb0/report_desc

# --- CDC-ACM（設定GUI / シリアルログイン用 → /dev/ttyGS0） ---

ln -s functions/hid.usb0 configs/c.1/
ln -s functions/acm.usb0 configs/c.1/
echo "NCM+HID+ACM" > configs/c.1/strings/0x409/configuration

echo "$UDC_NAME" > UDC
sleep 1

# usb0 に固定IP（DHCPが取れない問題の回避）
ip link set usb0 up
ip addr add "$BRAIN_IP" dev usb0 2>/dev/null || true

ls -l /dev/hidg0 /dev/ttyGS0
echo "done: NCM + HID + ACM"
