#!/bin/sh
# PC（Linux）の CPU の使用率、メモリの使用率、温度を、Brain のグラフのセル（widget: graph）に送る例。
# config/graph-example.yaml の「グラフ」のレイヤー（id は cpu、mem、temp、load）と組で使う。
#
#   linux-stats.sh            1 回測って送る（systemd のタイマーから起動する。contrib/systemd/brain-deck-graph.*）
#   linux-stats.sh --loop 10  10 秒ごとに測って送り続ける（端末で試すとき。Ctrl-C で終わる）
#
# CPU：/proc/stat を 1 秒あけて 2 回読み、そのあいだに忙しかった割合（%）。
# メモリ：/proc/meminfo の (MemTotal - MemAvailable) / MemTotal（%）。
# 温度：/sys/class/thermal/thermal_zone*/temp のうち、いちばん高いもの（℃）。読めなければ送らない。
# 負荷：/proc/loadavg の 1 分の平均。
# brain-deck が Brain を見つけられない（3）、設定 GUI が接続中（4）のときは、何もせずに終わる（終了コード 0）。
PATH=$HOME/.local/bin:/usr/local/bin:$PATH

cpu_ticks() {
	# cpu の行の合計と、idle + iowait
	awk '/^cpu /{ t = 0; for (i = 2; i <= NF; i++) t += $i; print t, $5 + $6; exit }' /proc/stat
}

measure() {
	set -- $(cpu_ticks)
	t1=$1 i1=$2
	sleep 1
	set -- $(cpu_ticks)
	t2=$1 i2=$2
	cpu=$(awk -v t="$((t2 - t1))" -v i="$((i2 - i1))" 'BEGIN { if (t > 0) printf "%.1f", (t - i) * 100 / t; else print 0 }')
	mem=$(awk '/^MemTotal:/{ t = $2 } /^MemAvailable:/{ a = $2 } END { if (t > 0) printf "%.1f", (t - a) * 100 / t }' /proc/meminfo)
	temp=""
	for z in /sys/class/thermal/thermal_zone*/temp; do
		[ -r "$z" ] || continue
		v=$(cat "$z" 2>/dev/null) || continue
		case $v in '' | *[!0-9-]*) continue ;; esac
		if [ -z "$temp" ] || [ "$v" -gt "$temp" ]; then temp=$v; fi
	done
	load=$(cut -d' ' -f1 /proc/loadavg)
	set -- cpu "$cpu" mem "$mem" load "$load"
	if [ -n "$temp" ]; then
		set -- "$@" temp "$(awk -v v="$temp" 'BEGIN { printf "%.1f", v / 1000 }')"
	fi
	# Brain の時刻がずれていれば、brain-deck が先に合わせる（値の時刻と「古い」の判断に使う）
	brain-deck -q graph "$@"
	case $? in 0 | 3 | 4) return 0 ;; *) return 1 ;; esac
}

if [ "$1" = "--loop" ]; then
	every=${2:-10}
	while :; do
		measure
		sleep $((every > 1 ? every - 1 : 1))
	done
fi
measure
