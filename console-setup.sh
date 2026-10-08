#!/bin/sh
# Brain のシリアルコンソール（/dev/ttyGS0 の getty）を、起動時に自動で動かす / 止める。
# Brain 上で ~/lefthand から実行する:
#   sudo sh console-setup.sh on      有効にして、今すぐ起動する
#   sudo sh console-setup.sh off     元に戻す（止めて、自動起動をやめ、drop-in を消す）
#   sh console-setup.sh status       今の状態を見る
#
# Brainux 本体のファイルは書き換えない。使うのは systemd の serial-getty@.service そのままで、
#   - /etc/systemd/system/getty.target.wants/serial-getty@ttyGS0.service（systemctl enable。起動時に動かす）
#   - /etc/systemd/system/dev-ttyGS0.device.wants/serial-getty@ttyGS0.service
#     （ttyGS0 ができたとき、作り直されたときにも起動する）
#   - /etc/systemd/system/serial-getty@ttyGS0.service.d/lefthand.conf（端末の種類を xterm-256color にし、
#     起動したときに何も書かず、Enter が届いてからログイン画面を出す。中身の説明はそのファイルに書いてある）
# を置くだけ。gadget-setup.sh が USB を付け直すと getty は一度切れるが、serial-getty@.service の
# Restart=always で起動し直す。
# ログインにはパスワードが要る（Brainux の設定のまま。このスクリプトは変えない）。
set -e
cd "$(dirname "$0")"
UNIT=serial-getty@ttyGS0.service
DROPIN=/etc/systemd/system/$UNIT.d/lefthand.conf
WANTS=/etc/systemd/system/dev-ttyGS0.device.wants

case "${1:-}" in
on)
  install -D -m 0644 "systemd/$UNIT.d/lefthand.conf" "$DROPIN"
  systemctl daemon-reload
  systemctl enable "$UNIT"
  # デバイスのユニットにはファイルがないので、systemctl add-wants は使えない。.wants のリンクを直接作る
  mkdir -p "$WANTS"
  ln -sf /usr/lib/systemd/system/serial-getty@.service "$WANTS/$UNIT"
  systemctl daemon-reload
  if [ -e /dev/ttyGS0 ]; then
    systemctl restart "$UNIT"
  else
    echo "/dev/ttyGS0 がまだありません。ガジェットができたときに起動します"
  fi
  ;;
off)
  systemctl disable --now "$UNIT" 2>/dev/null || true
  rm -f "$WANTS/$UNIT"
  rmdir "$WANTS" 2>/dev/null || true
  rm -f "$DROPIN"
  rmdir "$(dirname "$DROPIN")" 2>/dev/null || true
  systemctl daemon-reload
  echo "元に戻しました（$UNIT は止めて、自動では起動しません）"
  ;;
status) ;;
*)
  echo "使い方: sudo sh $0 on | off | status" >&2
  exit 2
  ;;
esac

echo "enabled: $(systemctl is-enabled "$UNIT" 2>/dev/null || true)  active: $(systemctl is-active "$UNIT" 2>/dev/null || true)"
echo "dev-ttyGS0.device の Wants: $(systemctl show -p Wants --value dev-ttyGS0.device 2>/dev/null)"
pid=$(systemctl show -p MainPID --value "$UNIT" 2>/dev/null); [ "${pid:-0}" != 0 ] && ps -o pid,args -p "$pid" || true
