#!/bin/sh
# Brain の起動を速くする（フェーズ K4）。止めるサービスを 3 つの段階に分け、段階ごとに適用・元に戻す・確かめる。Brain の上で root で実行する。
#
#   sudo sh boot-tune.sh plan [段階|項目...|--all]   何を変えるかを書くだけ（省略すると段階 1）
#   sudo sh boot-tune.sh apply [段階|項目...]        止める（省略すると段階 1）。止める前の状態を記録する
#   sudo sh boot-tune.sh revert [段階|項目...]       記録をもとに、止めたものを元に戻す（省略するとすべて）
#   sudo sh boot-tune.sh status                      記録と、今の状態
#
#   段階は 1、2、3（stage1 などとも書ける）。段階は 1 つずつ適用し、そのたびに再起動して確かめる（README の「Brain の起動を速くする」）。
#
# Brainux 本体のファイルは書き換えない。使うのは systemctl disable / enable だけ
# （/etc/systemd/system/*.wants/ と別名のリンクが変わる）。止めたサービスも、ほかから呼ばれれば（D-Bus など）動く。
# 効き目は再起動したあとに出る（このスクリプトは再起動しない）。測るのは boot-measure.sh。
# 必ず残すもの（どの段階でも止めない）：lefthand、ethernet_gadget（USB の構成）、ssh、serial-getty@ttyGS0（USB のシリアルのログイン）、
# getty@tty1、systemd、udev、dbus、journald。
#
# 項目（名前: ユニット）
#   段階 1（brain-deck、SSH、シリアルに関わらない。起動のときに CPU を取り合うだけ）
#     accounts   accounts-daemon.service          GUI のログイン画面用のユーザー情報（ly は使わない）
#     udisks     udisks2.service                  デスクトップの自動マウント用
#     e2scrub    e2scrub_reap.service             LVM の検査の後始末（Brain は LVM を使わない）
#     hotkeys    triggerhappy.service triggerhappy.socket   全体のホットキー（キーボードは lefthand が握る）
#     avahi      avahi-daemon.service avahi-daemon.socket   brain.local の名前（ssh brain は 192.168.7.2、brain-deck はシリアルなので使わない）
#   段階 2（ネットにつなぐためのもの。usb0 は NetworkManager の管理の外で、192.168.7.2 はガジェットの係が付ける）
#     nm         NetworkManager.service NetworkManager-wait-online.service NetworkManager-dispatcher.service
#     timesync   systemd-timesyncd.service        NTP（時刻は lefthand が RTC と PC で合わせる。起動の時刻は fake-hwclock が戻す）
#     networking networking.service               ifupdown（/etc/network/interfaces は lo だけ）
#     apt        apt-daily.timer apt-daily-upgrade.timer   パッケージの自動更新（ネットがないので失敗するだけ。起動のときに逃した回を動かす）
#   段階 3
#     ly         ly.service                       ログイン画面（tty2。display-manager.service の別名ごと）。lefthand は ly を待たずに起動する。
#                                                 Brain のログインは tty1（lefthand を止めたとき）、USB のシリアル、SSH、brain-deck の Brain のシェル
#
# 記録は /etc/lefthand/boot-tune.state（1 行に「ユニット 止める前の is-enabled の値」）。revert はこれだけを見る。
set -eu

STATE=${BOOT_TUNE_STATE:-/etc/lefthand/boot-tune.state}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
STAGE1="accounts udisks e2scrub hotkeys avahi"
STAGE2="nm timesync networking apt"
STAGE3="ly"
ALL_ITEMS="$STAGE1 $STAGE2 $STAGE3"

units_of() {
	case $1 in
	accounts) echo accounts-daemon.service ;;
	udisks) echo udisks2.service ;;
	e2scrub) echo e2scrub_reap.service ;;
	hotkeys) echo triggerhappy.service triggerhappy.socket ;;
	avahi) echo avahi-daemon.service avahi-daemon.socket ;;
	timesync) echo systemd-timesyncd.service ;;
	nm) echo NetworkManager.service NetworkManager-wait-online.service NetworkManager-dispatcher.service ;;
	networking) echo networking.service ;;
	apt) echo apt-daily.timer apt-daily-upgrade.timer ;;
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
	nm) echo "見込み：起動の道筋（critical-chain）から 10〜25 秒（2026-10-12 は +17 秒）。危なさ：小〜中（usb0 は unmanaged。Wi-Fi などを使うなら止めない）" ;;
	networking) echo "見込み：11 秒（blame）。危なさ：小（/etc/network/interfaces は lo だけ）" ;;
	apt) echo "見込み：逃した回の apt-daily-upgrade が起動のあとに 51 秒 CPU を使っていた（2026-10-12）。危なさ：小（ネットにつないで更新するなら手で apt）" ;;
	ly) echo "見込み：ly（tty2）を待たずに lefthand が起動。危なさ：中（ly のログイン画面がなくなる。tty1、SSH、USB のシリアル、brain-deck の Brain のシェルは残る）" ;;
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

# expand は、段階（1、2、3、stage1…）を項目に広げ、知らない項目なら止める。
expand() {
	out=""
	for it in "$@"; do
		case $it in
		1 | stage1) out="$out $STAGE1" ;;
		2 | stage2) out="$out $STAGE2" ;;
		3 | stage3) out="$out $STAGE3" ;;
		*)
			units_of "$it" >/dev/null || die "知らない項目：$it（段階 1 2 3、または $ALL_ITEMS）"
			out="$out $it"
			;;
		esac
	done
	echo $out
}

stage_of() {
	case " $STAGE1 " in *" $1 "*) echo 1; return ;; esac
	case " $STAGE2 " in *" $1 "*) echo 2; return ;; esac
	echo 3
}

cmd=${1:-plan}
[ $# -gt 0 ] && shift

case $cmd in
plan)
	items=$STAGE1
	if [ "${1:-}" = --all ]; then
		items=$ALL_ITEMS
	elif [ $# -gt 0 ]; then
		items=$(expand "$@")
	fi
	for it in $items; do
		echo "[段階 $(stage_of "$it")] $it: $(note_of "$it")"
		for u in $(units_of "$it"); do
			echo "    $u（今：$(is_enabled "$u")）"
		done
	done
	;;
apply)
	need_root "$cmd"
	items=$STAGE1
	if [ $# -gt 0 ]; then
		items=$(expand "$@")
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
				# disable は、止まっていても 0 でない値を返すことがある（e2scrub_reap で起きた）。結果は is-enabled で確かめる
				$SYSTEMCTL disable "$u" || true
				now=$(is_enabled "$u")
				case $now in
				enabled | enabled-runtime) echo "止められませんでした：$u（今も $now）" >&2 ;;
				*) echo "止めた：$u（前は $st、今は $now）" ;;
				esac
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
	if [ $# -eq 0 ]; then
		while read -r u st; do
			[ -n "$u" ] || continue
			$SYSTEMCTL enable "$u" || true
			echo "戻した：$u（$st、今は $(is_enabled "$u")）"
		done <"$STATE"
		mv "$STATE" "$STATE.reverted"
		echo "記録は $STATE.reverted に移しました。効き目は再起動のあと"
		exit 0
	fi
	# 段階や項目を選んだとき：そのユニットだけを戻し、記録から消す（ほかは残す）
	want=""
	for it in $(expand "$@"); do
		want="$want $(units_of "$it")"
	done
	keep="$STATE.keep"
	: >"$keep"
	while read -r u st; do
		[ -n "$u" ] || continue
		case " $want " in
		*" $u "*)
			$SYSTEMCTL enable "$u" || true
			echo "戻した：$u（$st、今は $(is_enabled "$u")）"
			echo "$u $st" >>"$STATE.reverted"
			;;
		*) echo "$u $st" >>"$keep" ;;
		esac
	done <"$STATE"
	if [ -s "$keep" ]; then
		mv "$keep" "$STATE"
		echo "まだ止めているもの（$STATE）：$(cut -d' ' -f1 "$STATE" | tr '\n' ' ')"
	else
		rm -f "$keep" "$STATE"
		echo "止めているものはなくなりました"
	fi
	echo "効き目は再起動のあと"
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
	die "使い方：sh $0 plan [段階|項目...|--all] | apply [段階|項目...] | revert [段階|項目...] | status（段階は 1 2 3）"
	;;
esac
