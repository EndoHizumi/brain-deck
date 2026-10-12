#!/bin/sh
# サイコロ。タップ、「振る」、スペース、決定で 1 個、「2 個」と 2 キーで 2 個振る。「面の数」で 2〜100 面にする。
# 面の数と、最近の 5 回は $BRAIN_APP_DATA（アプリが書ける場所）に置く。
data=${BRAIN_APP_DATA:-.}
sides=$(cat "$data/sides" 2>/dev/null || echo 6)

roll() { # 1〜$sides を $1 個
	od -An -N$(($1 * 2)) -tu2 /dev/urandom | awk -v s="$sides" '{ for (i = 1; i <= NF; i++) printf "%s%d", (n++ ? " " : ""), $i % s + 1 } END { print "" }'
}

result=""
case "$1 $2" in
"tap "* | "button roll" | "key space" | "key enter") result=$(roll 1) ;;
"button two" | "key 2") result=$(roll 2) ;;
"button clear") rm -f "$data/history" ;;
"input sides")
	read -r n
	case $n in
	'' | *[!0-9]*) echo "@color red"; echo "数字で入れてください（$n）"; echo "@color" ;;
	*) if [ "$n" -ge 2 ] && [ "$n" -le 100 ]; then echo "$n" > "$data/sides"; sides=$n; else echo "@color red"; echo "2〜100 で入れてください"; echo "@color"; fi ;;
	esac
	;;
esac
if [ -n "$result" ]; then
	{ echo "$result"; head -n 4 "$data/history" 2>/dev/null; } > "$data/history.new" && mv "$data/history.new" "$data/history"
fi

echo "@title サイコロ（${sides} 面）"
if [ -n "$result" ]; then
	echo "@big $result"
else
	echo "@color gray"
	echo "タップかスペースで振る"
	echo "@color"
fi
if [ -s "$data/history" ]; then
	echo "@sep"
	echo "@color gray"
	echo "最近："
	tr '\n' '/' < "$data/history" | sed 's#/$##; s#/# / #g'
	echo
	echo "@color"
fi
echo "@button roll 振る"
echo "@button two 2 個"
echo "@input sides 面の数 | 面の数（2〜100）"
echo "@button clear 履歴を消す"
