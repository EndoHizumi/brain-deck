#!/bin/sh
# lefthand を入れる。
#
#   Brain 上で ~/lefthand から:        sudo sh install.sh
#     配置と daemon-reload だけ行う。lefthand.service の enable/start はしない。
#     ethernet_gadget.service の drop-in は配置した時点で次回起動から有効になる。
#
#   PC で、Brainux を書いた SD カードに:  sudo sh install.sh --root /media/$USER/<ルートのパーティション>
#     SD カードのルートのファイルシステムに直接置き、lefthand.service を有効にする（systemctl は使わない）。
#     Brain は起動したときから USB キーボードとして動き、brain-deck で操作できる（ssh は要らない）。
#     /boot（カーネルの入ったパーティション）には触らない。
set -e
cd "$(dirname "$0")"
ROOT=""
if [ "$1" = "--root" ]; then
  ROOT=${2%/}
  if [ -z "$ROOT" ] || [ ! -d "$ROOT/etc/systemd/system" ] || [ ! -d "$ROOT/usr/local" ]; then
    echo "--root には、Brainux のルートのファイルシステム（etc/systemd/system と usr/local があるところ）を書きます: $ROOT" >&2
    exit 2
  fi
fi
[ -f lefthand ] || { echo "lefthand がありません。先にビルドします（README の「ビルド」）" >&2; exit 2; }
[ -f font/reader-24.lrf.gz ] && [ -f font/reader-32.lrf.gz ] || { echo "font/reader-24.lrf.gz と reader-32.lrf.gz がありません（リポジトリの font/ も一緒に写します）" >&2; exit 2; }

install -D -m 0755 lefthand "$ROOT/usr/local/bin/lefthand"
install -D -m 0755 gadget-setup.sh "$ROOT/usr/local/sbin/lefthand-gadget-setup"
# 読み物の本文の文字（フェーズ K7 から lefthand に埋め込まない。lefthand の -font-dir の既定の場所）
for f in reader-24.lrf.gz reader-32.lrf.gz; do
  install -D -m 0644 "font/$f" "$ROOT/usr/local/share/lefthand/$f"
done
if [ -e "$ROOT/etc/lefthand/config.yaml" ]; then
  echo "$ROOT/etc/lefthand/config.yaml は既にあるので上書きしません（新しい版: config.yaml）"
else
  install -D -m 0644 config.yaml "$ROOT/etc/lefthand/config.yaml"
fi
install -D -m 0644 systemd/ethernet_gadget.service.d/lefthand.conf \
  "$ROOT/etc/systemd/system/ethernet_gadget.service.d/lefthand.conf"
install -m 0644 systemd/lefthand.service "$ROOT/etc/systemd/system/"
# 端末モードの Brain のシェルで日本語入力（uim-fep）を通す（lefthand が渡す BRAIN_IME があるときだけ働く）
install -D -m 0644 contrib/brain/lefthand-ime.sh "$ROOT/etc/profile.d/lefthand-ime.sh"

# 旧版の lefthand-gadget.service（drop-in に統合済み）を片付ける
if [ -e "$ROOT/etc/systemd/system/lefthand-gadget.service" ]; then
  [ -z "$ROOT" ] && { systemctl disable lefthand-gadget.service 2>/dev/null || true; }
  rm -f "$ROOT/etc/systemd/system/lefthand-gadget.service" "$ROOT/etc/systemd/system/multi-user.target.wants/lefthand-gadget.service"
  echo "removed: lefthand-gadget.service"
fi

if [ -n "$ROOT" ]; then
  # systemctl enable と同じリンクを作る（[Install] WantedBy=multi-user.target）
  mkdir -p "$ROOT/etc/systemd/system/multi-user.target.wants"
  ln -sf /etc/systemd/system/lefthand.service "$ROOT/etc/systemd/system/multi-user.target.wants/lefthand.service"
  echo "installed into $ROOT（lefthand.service は有効。SD カードを Brain に戻して起動してください）"
  exit 0
fi

systemctl daemon-reload
systemctl --no-pager cat ethernet_gadget.service | grep -E '^(# |ExecStart)' || true
systemctl --no-pager status lefthand.service || true
