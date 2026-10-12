#!/bin/sh
# Brain の起動を速くする（フェーズ K4）。止めるサービスを 3 つの段階に分け、段階ごとに適用・元に戻す・確かめる。Brain の上で root で実行する。
#
#   sudo sh boot-tune.sh plan [段階|項目...|--all]   何を変えるかを書くだけ（省略すると段階 1）
#   sudo sh boot-tune.sh apply [段階|項目...]        止める（省略すると段階 1）。止める前の状態を記録する
#   sudo sh boot-tune.sh revert [段階|項目...]       記録をもとに、止めたものを元に戻す（省略するとすべて）
#   sudo sh boot-tune.sh status                      記録と、今の状態
#
#   段階は 1〜6（stage1 などとも書ける）。段階は 1 つずつ適用し、そのたびに再起動して確かめる（README の「Brain の起動を速くする」）。
#
# Brainux 本体のファイルは書き換えない。段階 1〜3 は systemctl disable / enable だけ
# （/etc/systemd/system/*.wants/ と別名のリンクが変わる）。止めたサービスも、ほかから呼ばれれば（D-Bus など）動く。
# 段階 4〜6（フェーズ K6）は、/etc に brain-deck のファイルを足す（drop-in、設定の .d、udev の規則を /dev/null で隠す）か、
# systemctl mask する。どれも、足したファイルを消す・unmask すれば元に戻る。足すファイルの中身は、このスクリプトの file_content。
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
#   段階 4（lefthand を早く起動する。USB の構成を待たない lefthand.service（b43dd75 から）を入れてから）
#     early      /etc/systemd/system/lefthand.service.d/boot-early.conf   DefaultDependencies=no。basic.target（SD カードの
#                                                 boot 区画と udev の coldplug を待つ）を待たず、ルートの remount と fake-hwclock のあとに起動する
#     gadget     /etc/systemd/system/ethernet_gadget.service.d/boot-early.conf   同じく、USB の構成を configfs のマウントのあとに作る
#                                                 （PC へのキー、brain-deck、usb0。ssh のサーバーと ttyGS0 のログインは basic.target のまま）
#   段階 5（起動の最初の部分を軽くする）
#     quiet      /etc/systemd/system.conf.d/brain-deck-boot.conf（ShowStatus=no）、/etc/sysctl.d/90-brain-deck-console.conf
#                （kernel.printk = 4 4 1 7）  起動中の [ OK ] の行と、カーネルの info のメッセージを、tty1（fbcon）とシリアルに書かない
#     modprobe   modprobe@drm.service、modprobe@efi_pstore.service を mask   モジュールのないカーネルなので空振り。
#                （modprobe@configfs は sys-kernel-config.mount が、modprobe@fuse は sys-fs-fuse-connections.mount が Requires= するので触らない）
#     udev       /etc/udev/rules.d/ の 60-triggerhappy、61-mate-settings-daemon-rfkill、80-udisks2、96-e2scrub を /dev/null へのリンクで隠す
#                （止めたサービスの規則。triggerhappy の規則は、止めた th-cmd を入力のデバイスごとに呼んで失敗していた）
#   段階 6
#     journal    /etc/systemd/journald.conf.d/brain-deck.conf（SystemMaxUse=32M）  ジャーナル（116 MB あった）を小さくする。
#                ディスクに書くのは同じ（再起動しても前の起動のログは残る）。入れたあとの最初の起動で、古いログが消える
#
# 記録は /etc/lefthand/boot-tune.state（1 行に「ユニットかファイル 前の状態 [種類]」。種類がなければ disable、mask、
# file（足したファイル）、null（/dev/null へのリンク）。ファイルの前の状態は absent）。revert はこれだけを見る。
set -eu

STATE=${BOOT_TUNE_STATE:-/etc/lefthand/boot-tune.state}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
STAGE1="accounts udisks e2scrub hotkeys avahi"
STAGE2="nm timesync networking apt"
STAGE3="ly"
STAGE4="early gadget"
STAGE5="quiet modprobe udev"
STAGE6="journal"
ALL_ITEMS="$STAGE1 $STAGE2 $STAGE3 $STAGE4 $STAGE5 $STAGE6"
ROOT=${BOOT_TUNE_ROOT:-} # ファイルを置く場所の前に付ける（テスト用）

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
	early) echo file:/etc/systemd/system/lefthand.service.d/boot-early.conf ;;
	gadget) echo file:/etc/systemd/system/ethernet_gadget.service.d/boot-early.conf ;;
	quiet) echo file:/etc/systemd/system.conf.d/brain-deck-boot.conf file:/etc/sysctl.d/90-brain-deck-console.conf ;;
	modprobe) echo mask:modprobe@drm.service mask:modprobe@efi_pstore.service ;;
	udev) echo null:/etc/udev/rules.d/60-triggerhappy.rules null:/etc/udev/rules.d/61-mate-settings-daemon-rfkill.rules \
		null:/etc/udev/rules.d/80-udisks2.rules null:/etc/udev/rules.d/96-e2scrub.rules ;;
	journal) echo file:/etc/systemd/journald.conf.d/brain-deck.conf ;;
	*) return 1 ;;
	esac
}

# file_content は、段階 4〜6 で足すファイルの中身。
file_content() {
	case $1 in
	*/lefthand.service.d/boot-early.conf)
		cat <<'EOF'
# brain-deck（contrib/boot/boot-tune.sh の段階 4）：lefthand を起動の早いうちに起動する。消せば元に戻る。
# lefthand が要るのは、フレームバッファ、入力のデバイス、VT（どれもカーネルに組み込み。devtmpfs で最初からある）、
# /etc/lefthand と /var/lib/lefthand（ルートの区画）、時刻だけ。basic.target（SD カードの boot 区画、udev の coldplug）と
# USB の構成（ethernet_gadget）は待たない（USB は、できるまで外れているときと同じに動く。usbboot.go）
[Unit]
DefaultDependencies=no
After=systemd-remount-fs.service fake-hwclock-load.service tmp.mount systemd-journald.socket
Conflicts=shutdown.target
Before=shutdown.target
EOF
		;;
	*/ethernet_gadget.service.d/boot-early.conf)
		cat <<'EOF'
# brain-deck（contrib/boot/boot-tune.sh の段階 4）：USB の構成（gadget-setup.sh）を起動の早いうちに作る。消せば元に戻る。
# gadget-setup.sh が使うのは configfs と、bash、ip だけ。basic.target（SD カードの boot 区画、udev の coldplug）は待たない。
# ssh のサーバーと ttyGS0 のログインは今までどおり basic.target のあと
[Unit]
DefaultDependencies=no
Wants=sys-kernel-config.mount
After=sys-kernel-config.mount systemd-remount-fs.service systemd-journald.socket
Conflicts=shutdown.target
Before=shutdown.target
EOF
		;;
	*/system.conf.d/brain-deck-boot.conf)
		cat <<'EOF'
# brain-deck（contrib/boot/boot-tune.sh の段階 5）：起動中の [ OK ] の行を tty1（fbcon）とシリアルに書かない。消せば元に戻る
[Manager]
ShowStatus=no
EOF
		;;
	*/sysctl.d/90-brain-deck-console.conf)
		cat <<'EOF'
# brain-deck（contrib/boot/boot-tune.sh の段階 5）：コンソール（tty1 の fbcon とシリアル）には警告より重いメッセージだけを書く。
# 消せば元に戻る（Brainux の既定は 7 4 1 7）。dmesg と journal には今までどおり残る
kernel.printk = 4 4 1 7
EOF
		;;
	*/journald.conf.d/brain-deck.conf)
		cat <<'EOF'
# brain-deck（contrib/boot/boot-tune.sh の段階 6）：ジャーナルを小さくする（起動のときに開いて書き出す量を減らす）。消せば元に戻る
[Journal]
SystemMaxUse=32M
EOF
		;;
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
	gadget) echo "見込み：PC へのキーと brain-deck が約 57 秒 → 約 30 秒。危なさ：中（失敗すると ssh も使えない。tty1 か SD カードで revert gadget）" ;;
	early) echo "見込み：lefthand の起動が約 61 秒 → 約 20 秒。危なさ：中（起動の順が変わる。USB の構成を待たない lefthand.service と、b43dd75 からのデーモンが要る）" ;;
	quiet) echo "見込み：1〜4 秒（fbcon のスクロールとシリアルへの書き出しを待たない）。危なさ：小（起動中の進み具合が画面とシリアルに出ない。journal には残る）" ;;
	modprobe) echo "見込み：1〜2 秒ぶんの CPU（モジュールがないので空振り）。危なさ：小（モジュールのあるカーネルにしたら戻す）" ;;
	udev) echo "見込み：1〜3 秒（coldplug で止めたサービスの規則を動かさない）。危なさ：小（udisks2 などを戻すときは一緒に戻す）" ;;
	journal) echo "見込み：journal-flush（9 秒）と journald の起動が軽くなる。危なさ：小（古いログが早く消える。前の起動のログは残る）" ;;
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
		4 | stage4) out="$out $STAGE4" ;;
		5 | stage5) out="$out $STAGE5" ;;
		6 | stage6) out="$out $STAGE6" ;;
		*)
			units_of "$it" >/dev/null || die "知らない項目：$it（段階 1〜6、または $ALL_ITEMS）"
			out="$out $it"
			;;
		esac
	done
	echo $out
}

stage_of() {
	case " $STAGE1 " in *" $1 "*) echo 1; return ;; esac
	case " $STAGE2 " in *" $1 "*) echo 2; return ;; esac
	case " $STAGE3 " in *" $1 "*) echo 3; return ;; esac
	case " $STAGE4 " in *" $1 "*) echo 4; return ;; esac
	case " $STAGE5 " in *" $1 "*) echo 5; return ;; esac
	echo 6
}

# now_of は、ユニット（種類:名前）の今の状態。ファイルは present か absent
now_of() {
	case $1 in
	file:* | null:*)
		if [ -e "$ROOT${1#*:}" ] || [ -L "$ROOT${1#*:}" ]; then echo present; else echo absent; fi
		;;
	mask:*) is_enabled "${1#mask:}" ;;
	*) is_enabled "$1" ;;
	esac
}

# apply_one は、1 つ（種類:名前）を変え、記録に足す。
apply_one() {
	t=$1
	case $t in
	file:* | null:*)
		kind=${t%%:*}
		f=${t#*:}
		if grep -q "^$f " "$STATE"; then
			echo "飛ばす：$f（もう記録にある）"
			return
		fi
		if [ -e "$ROOT$f" ] || [ -L "$ROOT$f" ]; then
			echo "そのまま：$f（もうある。brain-deck が置いたものではないので触らない）"
			return
		fi
		mkdir -p "$(dirname "$ROOT$f")"
		echo "$f absent $kind" >>"$STATE"
		if [ "$kind" = null ]; then
			ln -s /dev/null "$ROOT$f"
			echo "隠した：$f（/dev/null へのリンク）"
		else
			file_content "$f" >"$ROOT$f"
			chmod 0644 "$ROOT$f"
			echo "置いた：$f"
		fi
		changed_files=1
		;;
	mask:*)
		u=${t#mask:}
		if grep -q "^$u " "$STATE"; then
			echo "飛ばす：$u（もう記録にある）"
			return
		fi
		st=$(is_enabled "$u")
		case $st in
		masked | masked-runtime) echo "そのまま：$u（もう $st）" ;;
		not-found | "") echo "そのまま：$u（ない）" ;;
		*)
			echo "$u $st mask" >>"$STATE"
			$SYSTEMCTL mask "$u" || true
			now=$(is_enabled "$u")
			case $now in
			masked*) echo "mask した：$u（前は $st）" ;;
			*) echo "mask できませんでした：$u（今は $now）" >&2 ;;
			esac
			;;
		esac
		;;
	*)
		u=$t
		if grep -q "^$u " "$STATE"; then
			echo "飛ばす：$u（もう記録にある）"
			return
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
		;;
	esac
}

# revert_one は、記録の 1 行（名前 前の状態 種類）を元に戻す。
revert_one() {
	case $3 in
	file | null)
		rm -f "$ROOT$1"
		case $1 in
		*/lefthand.service.d/* | */system.conf.d/* | */journald.conf.d/*)
			rmdir "$(dirname "$ROOT$1")" 2>/dev/null || true # 置いたときに作った .d が空になったら消す
			;;
		esac
		echo "戻した：$1（消した）"
		changed_files=1
		;;
	mask)
		$SYSTEMCTL unmask "$1" || true
		echo "戻した：$1（unmask。$2、今は $(is_enabled "$1")）"
		;;
	*)
		$SYSTEMCTL enable "$1" || true
		echo "戻した：$1（$2、今は $(is_enabled "$1")）"
		;;
	esac
}

# reload は、ファイルを変えたら systemd に読み直させる（効き目は再起動のあと）。
reload() {
	[ "${changed_files:-0}" = 1 ] && $SYSTEMCTL daemon-reload || true
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
			echo "    $u（今：$(now_of "$u")）"
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
			apply_one "$u"
		done
	done
	reload
	echo "記録：$STATE。効き目は再起動のあと（sh boot-measure.sh で測る）。元に戻すのは sudo sh $0 revert"
	;;
revert)
	need_root "$cmd"
	[ -s "$STATE" ] || {
		echo "記録がありません（$STATE）。戻すものはありません"
		exit 0
	}
	if [ $# -eq 0 ]; then
		while read -r u st kind; do
			[ -n "$u" ] || continue
			revert_one "$u" "$st" "$kind"
		done <"$STATE"
		reload
		mv "$STATE" "$STATE.reverted"
		echo "記録は $STATE.reverted に移しました。効き目は再起動のあと"
		exit 0
	fi
	# 段階や項目を選んだとき：そのユニットだけを戻し、記録から消す（ほかは残す）
	want=""
	for it in $(expand "$@"); do
		for t in $(units_of "$it"); do
			want="$want ${t#*:}"
		done
	done
	keep="$STATE.keep"
	: >"$keep"
	while read -r u st kind; do
		[ -n "$u" ] || continue
		case " $want " in
		*" $u "*)
			revert_one "$u" "$st" "$kind"
			echo "$u $st $kind" >>"$STATE.reverted"
			;;
		*) echo "$u $st $kind" >>"$keep" ;;
		esac
	done <"$STATE"
	reload
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
		while read -r u st kind; do
			case $kind in
			file | null) t="$kind:$u" ;;
			mask) t="mask:$u" ;;
			*) t=$u ;;
			esac
			[ -n "$u" ] && echo "    $u：前は $st、今は $(now_of "$t")${kind:+（$kind）}"
		done <"$STATE"
	else
		echo "止めたものはありません（$STATE がない）"
	fi
	;;
*)
	die "使い方：sh $0 plan [段階|項目...|--all] | apply [段階|項目...] | revert [段階|項目...] | status（段階は 1〜6）"
	;;
esac
