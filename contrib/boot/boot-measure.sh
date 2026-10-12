#!/bin/sh
# Brain の起動にかかった時間を測って書く（フェーズ K4）。読むだけで、何も変えない。Brain の上で実行する。
#   sudo sh boot-measure.sh            画面に書く
#   sudo sh boot-measure.sh > after.txt
# 比べるもの：Startup finished（kernel + userspace）、graphical.target、lefthand が起動した時刻と、準備ができた時刻、
# 「起動中」の画面が出た時刻、USB の準備ができた時刻（フェーズ K6）
# （lefthand のログの startup: の行。phase K4 から）。
set -u
echo "== $(date '+%F %T') $(uname -r)"
systemd-analyze time 2>&1
echo "== lefthand（起動からの秒。journal の monotonic）"
journalctl -b -o short-monotonic --no-pager 2>/dev/null |
	grep -E "Started (ly|lefthand)\.service|lefthand\[[0-9]+\]: .*(startup:|control: listening|usb: ready|usb: .* not there yet)" | head -8
# 「起動中」の画面が出た時刻（フェーズ K6）：startup の行の時刻 - main から準備までの時間 + config + splash
journalctl -b -o short-monotonic --no-pager 2>/dev/null | grep -m1 "lefthand\[[0-9]*\]: .*startup:" |
	sed -E 's/^\[ *([0-9.]+)\].* ready ([0-9]+)ms after main: config ([0-9]+)ms, splash ([0-9]+)ms.*/\1 \2 \3 \4/' |
	awk 'NF == 4 { printf "起動中の画面：%.1f 秒（準備：%.1f 秒）\n", $1 - $2/1000 + ($3+$4)/1000, $1 }'
echo "== critical-chain lefthand.service"
systemd-analyze critical-chain lefthand.service --no-pager 2>&1 | tail -n +3
echo "== blame（上位 20）"
systemd-analyze blame --no-pager 2>&1 | head -20
