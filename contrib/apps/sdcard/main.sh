#!/bin/sh
# SD カード（/）の空きと、おもな場所の大きさを出す。Brain の user として動くので、読めない場所は数えない。
# du は SD カード全体には使わない（Brain では数分かかる）。数えた場所のほかは「その他（システムなど）」にまとめる。

kib() { # KiB を読みやすく
	awk -v k="$1" 'BEGIN { if (k >= 1048576) printf "%.1f GB", k / 1048576; else if (k >= 1024) printf "%.0f MB", k / 1024; else printf "%d KB", k }'
}

set -- $(df -Pk / | awk 'NR == 2 { print $2, $3, $4 }')
total=$1 used=$2 free=$3

echo "@title SD カード"
echo "@big 空き $(kib "$free")"
echo "@bar $used $total 使用 $(kib "$used") / $(kib "$total")"
echo "@sep"
rest=$used
for p in /home /var/lib/lefthand /var/log /var/cache /tmp; do
	[ -d "$p" ] || continue
	k=$(du -sk "$p" 2>/dev/null | awk '{ print $1 }')
	[ -n "$k" ] || continue
	rest=$((rest - k))
	echo "@row $p | $(kib "$k")"
done
[ "$rest" -gt 0 ] && echo "@row その他（システムなど） | $(kib "$rest")"
echo "@color gray"
echo "$(date '+%H:%M') に調べました"
echo "@button refresh 今すぐ調べる"
