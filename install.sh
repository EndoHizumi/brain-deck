#!/bin/sh
# Brain 上で ~/lefthand から実行する: sudo sh install.sh
# 配置と daemon-reload だけ行う。lefthand.service の enable/start はしない。
# ethernet_gadget.service の drop-in は配置した時点で次回起動から有効になる。
set -e
cd "$(dirname "$0")"
install -D -m 0755 lefthand /usr/local/bin/lefthand
install -D -m 0755 gadget-setup.sh /usr/local/sbin/lefthand-gadget-setup
if [ -e /etc/lefthand/config.yaml ]; then
  echo "/etc/lefthand/config.yaml は既にあるので上書きしません（新しい版: config.yaml）"
else
  install -D -m 0644 config.yaml /etc/lefthand/config.yaml
fi
install -D -m 0644 systemd/ethernet_gadget.service.d/lefthand.conf \
  /etc/systemd/system/ethernet_gadget.service.d/lefthand.conf
install -m 0644 systemd/lefthand.service /etc/systemd/system/

# 旧版の lefthand-gadget.service（drop-in に統合済み）を片付ける
if [ -e /etc/systemd/system/lefthand-gadget.service ]; then
  systemctl disable lefthand-gadget.service 2>/dev/null || true
  rm -f /etc/systemd/system/lefthand-gadget.service
  echo "removed: lefthand-gadget.service"
fi

systemctl daemon-reload
systemctl --no-pager cat ethernet_gadget.service | grep -E '^(# |ExecStart)' || true
systemctl --no-pager status lefthand.service || true
