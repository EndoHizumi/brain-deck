#!/bin/bash
# Brain の負荷（mode: stream）。画面を「@clear」で始めて描き、2 秒（ボタンで変える）待つあいだに、
# 標準入力に来たもの（button <id>）を読む。標準入力が閉じたら（デーモンが止めたら）終わる。
every=2
prev=""

cpu_line() { awk '/^cpu / { print $2 + $3 + $4 + $6 + $7 + $8, $5 }' /proc/stat; }

while :; do
	cur=$(cpu_line)
	pct=0
	if [ -n "$prev" ]; then
		pct=$(awk -v a="$prev" -v b="$cur" 'BEGIN { split(a, x, " "); split(b, y, " "); bu = y[1] - x[1]; bi = y[2] - x[2]; if (bu + bi > 0) printf "%d", 100 * bu / (bu + bi); else print 0 }')
	fi
	prev=$cur
	read -r total avail < <(awk '/^MemTotal:/ { t = $2 } /^MemAvailable:/ { a = $2 } END { print t, a }' /proc/meminfo)
	used=$((total - avail))
	read -r l1 l5 l15 _ < /proc/loadavg

	echo "@clear"
	echo "@title Brain の負荷（${every} 秒ごと）"
	if [ "$pct" -ge 80 ]; then echo "@color red"; elif [ "$pct" -ge 50 ]; then echo "@color yellow"; fi
	echo "@bar $pct 100 CPU ${pct}%"
	echo "@color"
	echo "@bar $used $total メモリ $((used / 1024)) / $((total / 1024)) MB"
	echo "@row 負荷の平均 | $l1  $l5  $l15"
	v=/sys/bus/iio/devices/iio:device0
	if [ -r "$v/in_voltage7_raw" ] && [ -r "$v/in_voltage7_scale" ]; then
		mv=$(awk -v r="$(cat "$v/in_voltage7_raw")" -v s="$(cat "$v/in_voltage7_scale")" 'BEGIN { printf "%.2f", r * s / 1000 }')
		echo "@row 電池 | $mv V"
	fi
	echo "@row プロセス | $(ls -d /proc/[0-9]* 2>/dev/null | wc -l)"
	echo "@button fast 1 秒"
	echo "@button normal 2 秒"
	echo "@button slow 5 秒"

	if read -r -t "$every" ev arg; then
		case "$ev $arg" in
		"button fast") every=1 ;;
		"button normal") every=2 ;;
		"button slow") every=5 ;;
		esac
	elif [ $? -le 128 ]; then
		exit 0 # 標準入力が閉じた
	fi
done
