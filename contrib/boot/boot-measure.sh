#!/bin/sh
# Brain の起動にかかった時間を測って書く（フェーズ K4）。読むだけで、何も変えない。Brain の上で実行する。
#   sudo sh boot-measure.sh            画面に書く
#   sudo sh boot-measure.sh > after.txt
# 比べるもの：Startup finished（kernel + userspace）、graphical.target、lefthand が起動した時刻と、準備ができた時刻
# （lefthand のログの startup: の行。phase K4 から）。
set -u
echo "== $(date '+%F %T') $(uname -r)"
systemd-analyze time 2>&1
echo "== lefthand（起動からの秒。journal の monotonic）"
journalctl -b -o short-monotonic --no-pager 2>/dev/null |
	grep -E "Started (ly|lefthand)\.service|lefthand\[[0-9]+\]: .*(startup:|control: listening)" | head -6
echo "== critical-chain lefthand.service"
systemd-analyze critical-chain lefthand.service --no-pager 2>&1 | tail -n +3
echo "== blame（上位 20）"
systemd-analyze blame --no-pager 2>&1 | head -20
