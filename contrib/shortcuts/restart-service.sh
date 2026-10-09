#!/bin/sh
# Brain の「再起動」のセル（F16）に割り当てる例。ユーザー単位の systemd のサービスを再起動し、
# 動いているかを確かめて、結果を Brain のテキストのタイル service に出す。
#   SERVICE  再起動するサービス（既定 myapp.service。systemctl --user で動かしているもの）
PATH=$HOME/.local/bin:/usr/local/bin:$PATH
SERVICE=${SERVICE:-myapp.service}
mkdir -p "$HOME/.cache/brain-deck"
exec brain-deck exec --result service --limit 1m -- sh -c "
  systemctl --user restart '$SERVICE' && sleep 2 &&
  systemctl --user is-active '$SERVICE' >/dev/null &&
  echo \"$SERVICE: \$(systemctl --user show -p ActiveState --value '$SERVICE')\"
" >"$HOME/.cache/brain-deck/service.log" 2>&1
