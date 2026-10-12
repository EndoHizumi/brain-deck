#!/bin/sh
# Brain の起動を速くする案（フェーズ K4）を、適用する・元に戻す・確かめる。Brain の上で root で実行する。
#
#   sudo sh boot-tune.sh plan                 何を変えるかを書くだけ（既定の項目）
#   sudo sh boot-tune.sh plan --all           選べる項目をすべて並べる
#   sudo sh boot-tune.sh apply [項目...]      既定の項目（または指定した項目）を止める。止める前の状態を記録する
#   sudo sh boot-tune.sh revert               記録をもとに、止めたものを元に戻す
#   sudo sh boot-tune.sh status               記録と、今の状態
#
# Brainux 本体のファイルは書き換えない。使うのは systemctl disable / enable だけ
# （/etc/systemd/system/*.wants/ のリンクが変わる）。止めたサービスも、ほかから呼ばれれば（D-Bus など）動く。
# 効き目は再起動したあとに出る（このスクリプトは再起動しない）。測るのは boot-measure.sh。
#
# 項目（名前: ユニット … 既定で止めるか）
#   accounts   accounts-daemon.service          既定  GUI のログイン画面用のユーザー情報（ly は使わない）
#   udisks     udisks2.service                  既定  デスクトップの自動マウント用
#   e2scrub    e2scrub_reap.service             既定  LVM の検査の後始末（Brain は LVM を使わない）
#   hotkeys    triggerhappy.service triggerhappy.socket   既定  全体のホットキー（キーボードは lefthand が握る）
#   avahi      avahi-daemon.service avahi-daemon.socket   既定  brain.local の名前（ssh brain は 192.168.7.2、brain-deck はシリアルなので使わない）
#   timesync   systemd-timesyncd.service        選ぶ  NTP（Brain はふつうネットにつながらない。時刻は lefthand が RTC と PC で合わせる）
#   nm         NetworkManager.service NetworkManager-wait-online.service NetworkManager-dispatcher.service
#                                               選ぶ  ネットワークの管理。usb0 の 192.168.7.2 はガジェットの係が付けるが、実機で確かめてから
#   ly         ly.service                       選ぶ  ログイン画面。止めると Brain の画面でログインできなくなる（README の「Brain の起動を速くする」の復旧の手段）
#
# 記録は /etc/lefthand/boot-tune.state（1 行に「ユニット 止める前の is-enabled の値」）。revert はこれだけを見る。
set -eu

STATE=${BOOT_TUNE_STATE:-/etc/lefthand/boot-tune.state}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
DEFAULT_ITEMS="accounts udisks e2scrub hotkeys avahi"
ALL_ITEMS="$DEFAULT_ITEMS timesync nm ly"

units_of() {
	case $1 in
	accounts) echo accounts-daemon.service ;;
	udisks) echo udisks2.service ;;
	e2scrub) echo e2scrub_reap.service ;;
	hotkeys) echo triggerhappy.service triggerhappy.socket ;;
	avahi) echo avahi-daemon.service avahi-daemon.socket ;;
	timesync) echo systemd-timesyncd.service ;;
	nm) echo NetworkManager.service NetworkManager-wait-online.service NetworkManager-dispatcher.service ;;
	ly) echo ly.service ;;
	*) return 1 ;;
	esac
}

note_of() {
	case $1 in
	accounts) echo "見込み：起動のとき 18 秒ほど CPU を使っていた（2026-10-12 の blame）。危なさ：小" ;;
	udisks) echo "見込み：18 秒（blame）と、呼び出す polkit の 5 秒。危なさ：小（SD カードの自動マウントはもともと使っていない）" ;;
	e2scrub) echo "見込み：20 秒（blame。終わるまで CPU を取り合う）。危なさ：小" ;;
	hotkeys) echo "見込み：4 秒。危なさ：小（lefthand がキーボードを握るので、もともと届かない）" ;;
	avahi) echo "見込み：12 秒。危なさ：小（brain.local の名前が引けなくなる。ssh brain は IP で書いてある）" ;;
	timesync) echo "見込み：8 秒。危なさ：小（Wi-Fi などでネットにつなぐなら止めない）" ;;
	nm) echo "見込み：起動の道筋（critical-chain）から 10〜25 秒。危なさ：中（SSH の usb0 が付くか、実機で確かめてから）" ;;
	ly) echo "見込み：ly（tty2）を待たずに lefthand が起動。危なさ：中（Brain の画面でログインできなくなる。SSH、USB のシリアル、brain-deck の Brain のシェルは残る）" ;;
	esac
}

die() {
	echo "boot-tune: $*" >&2
	exit 1
}

is_enabled() { $SYSTEMCTL is-enabled "$1" 2>/dev/null || true; }

need_root() {
	[ "${BOOT_TUNE_NOROOT:-}" = 1 ] || [ "$(id -u)" = 0 ] || die "root で実行してください（sudo sh $0 $*）"
}

check_items() {
	for it in "$@"; do
		units_of "$it" >/dev/null || die "知らない項目：$it（$ALL_ITEMS）"
	done
}

cmd=${1:-plan}
[ $# -gt 0 ] && shift

case $cmd in
plan)
	items=$DEFAULT_ITEMS
	if [ "${1:-}" = --all ]; then
		items=$ALL_ITEMS
	elif [ $# -gt 0 ]; then
		check_items "$@"
		items=$*
	fi
	for it in $items; do
		mark="既定"
		case " $DEFAULT_ITEMS " in *" $it "*) ;; *) mark="選ぶ" ;; esac
		echo "[$mark] $it: $(note_of "$it")"
		for u in $(units_of "$it"); do
			echo "    $u（今：$(is_enabled "$u")）"
		done
	done
	;;
apply)
	need_root "$cmd"
	items=$DEFAULT_ITEMS
	if [ $# -gt 0 ]; then
		check_items "$@"
		items=$*
	fi
	mkdir -p "$(dirname "$STATE")"
	touch "$STATE"
	for it in $items; do
		for u in $(units_of "$it"); do
			if grep -q "^$u " "$STATE"; then
				echo "飛ばす：$u（もう記録にある）"
				continue
			fi
			st=$(is_enabled "$u")
			case $st in
			enabled | enabled-runtime)
				echo "$u $st" >>"$STATE"
				$SYSTEMCTL disable "$u"
				echo "止めた：$u（前は $st）"
				;;
			*)
				echo "そのまま：$u（$st。もともと起動のときに動かない、またはない）"
				;;
			esac
		done
	done
	echo "記録：$STATE。効き目は再起動のあと（sh boot-measure.sh で測る）。元に戻すのは sudo sh $0 revert"
	;;
revert)
	need_root "$cmd"
	[ -s "$STATE" ] || {
		echo "記録がありません（$STATE）。戻すものはありません"
		exit 0
	}
	while read -r u st; do
		[ -n "$u" ] || continue
		$SYSTEMCTL enable "$u"
		echo "戻した：$u（$st）"
	done <"$STATE"
	mv "$STATE" "$STATE.reverted"
	echo "記録は $STATE.reverted に移しました。効き目は再起動のあと"
	;;
status)
	if [ -s "$STATE" ]; then
		echo "止めたもの（$STATE）："
		while read -r u st; do
			[ -n "$u" ] && echo "    $u：前は $st、今は $(is_enabled "$u")"
		done <"$STATE"
	else
		echo "止めたものはありません（$STATE がない）"
	fi
	;;
*)
	die "使い方：sh $0 plan [--all|項目...] | apply [項目...] | revert | status"
	;;
esac
