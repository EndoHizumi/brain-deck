// brain-deck は、PC から Brain（左手デバイス lefthand）のウィジェットを書き換えるコマンド。
// 設定 GUI と同じ USB シリアル（Brain 側 /dev/ttyGS1）と、同じプロトコル（docs/protocol.md）を使う。
// Linux と macOS 用。使い方は README の「brain-deck」。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 終了コード（README に同じ表がある）
const (
	exitOK         = 0
	exitInternal   = 1 // 予期しないエラー
	exitUsage      = 2 // 使い方の誤り
	exitNoBrain    = 3 // Brain が見つからない、返事がない
	exitBusy       = 4 // 設定 GUI（ほかのプログラム）が接続中、ほかの brain-deck が終わらない
	exitBrainError = 5 // Brain がエラーを返した（引数の誤り、古いデーモンなど）
	exitPermission = 6 // ポートを開く権限がない（Linux の dialout）
	// exitFetch = 7（calendar.go）：予定の取得に失敗した（取れたカレンダーは送った）
)

const defaultTimeout = 5 * time.Second

// timeSyncThreshold より Brain の時刻がずれていたら、text の前に時刻を合わせる。
const timeSyncThreshold = 2 * time.Second

var version = "" // -ldflags "-X main.version=..."

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if rev != "" {
			if dirty {
				rev += "+dirty"
			}
			return rev
		}
	}
	return "dev"
}

const usage = `使い方：
  brain-deck text <id> <テキスト> [--style normal|ok|error|warn] [--ttl 10m]
  brain-deck text <id> -              テキストを標準入力から読む
  brain-deck text <id> --clear        テキストを消す（「未設定」に戻す）
  brain-deck text --list              Brain にあるテキストの一覧
  brain-deck qr <id> <文字列> [--ttl 1h]   QR コードのセル（widget: qr）に出す文字列を送る（UTF-8 で 400 バイトまで）
  brain-deck qr <id> -                文字列を標準入力から読む。qr <id> --clear で消す。qr --list で一覧
  brain-deck presence                 在席ランプ（widget: presence）の今の状態と、選べる状態
  brain-deck presence <状態>|next|prev [--id <id>]   在席ランプの状態を変える（id を省略すると presence）
  brain-deck graph <id> <値> [<id> <値>...]   グラフのセル（widget: graph）に値を足す（Brain は id ごとに新しい 120 個を覚える）
  brain-deck graph <id> --clear       グラフの値を消す。graph --list [--json] で一覧
  brain-deck todo [list] [--json]     Todo の一覧（番号は Brain の画面と同じ順）
  brain-deck todo add <項目> [--top]  Todo を足す（- なら標準入力の 1 行ごとに足す。--top で先頭に）
  brain-deck todo done <番号|ID>...   完了にする（undo で未完了に戻す）
  brain-deck todo edit <番号|ID> <文> 項目の文を書き換える
  brain-deck todo rm <番号|ID>...     項目を消す
  brain-deck todo clear-done          完了した項目をまとめて消す
  brain-deck calendar sync            予定を ICS の URL から取ってきて Brain に送る（~/.config/brain-deck/calendars.yaml）。
                                      送るのは、先月の初めから来月の終わりまで（past_months、future_months で変える）
  brain-deck calendar sync --ics <URL> [--days 7]   設定ファイルを使わず、URL を直接指定する（何度でも書ける。
                                      --days は、少なくとも今日から何日先までを送るか）
  brain-deck calendar sync --dry-run  取ってきた予定を表示するだけで、Brain には送らない
  brain-deck calendar [list] [--json] Brain にある予定
  brain-deck calendar clear           Brain の予定を消す
  brain-deck weather sync             天気予報を取ってきて Brain に送る（天気予報 API。地点は ~/.config/brain-deck/weather.yaml の
                                      city）。天気アイコン（気象庁）は、Brain にないものだけを送る
  brain-deck weather sync --city <ID> 設定ファイルを使わず、地点（一次細分区域の ID）を直接指定する
  brain-deck weather sync --dry-run   取ってきた予報を表示するだけで、Brain には送らない
  brain-deck weather [list] [--json]  Brain にある予報とアイコン
  brain-deck images [list] [--json]   Brain にある背景画像の一覧（使っている場所の数、名前）
  brain-deck images put <ファイル.565|ディレクトリ>...  変換済みの画像を送る（PNG などは設定 GUI で変換する）
  brain-deck images prune [--dry-run] 今の設定で使っていない背景画像を消す
  brain-deck photo [list] [--json]    フォトフレームの写真の一覧（番号、id、横・縦、足した日時、名前）
  brain-deck photo add <ファイル>... [--portrait] [--zoom 1.5] [--center x,y] [--brightness -20] [--no-dither]
                                      写真（PNG、JPEG、GIF、WebP）を 800x480 に（--portrait なら 480x800 に）切り抜いて送る。
                                      変換は設定 GUI と同じ計算。--center は元の画像の画素の位置、--brightness は百分率
  brain-deck photo add <ファイル> --of <番号|id>   横向きの写真を、縦向きの画面用に切り抜き直して足す
  brain-deck photo rm <番号|id>...    写真を消す。photo get <番号|id> -o <ファイル.png> で取り出す
  brain-deck memo [list] [--json]     手書きメモ（widget: memo）の一覧（番号、id、ページと線の数、更新した日時）
  brain-deck memo get <番号|id> -o <ファイル.png> [--page N]   メモを PNG に描き出す（ページは縦に並べる）
  brain-deck memo rm <番号|id>...     メモを消す
  brain-deck app [list] [--json]      自分で足したアプリ（/var/lib/lefthand/apps。docs/apps.md）の一覧と、動きの様子
  brain-deck app install <フォルダ>...   アプリ（app.yaml とスクリプト）を Brain に置く（同じ名前なら入れ替える）
  brain-deck app rm <名前>...         アプリと、アプリが書いたデータを消す
  brain-deck read add <ファイル | URL>...  読み物（青空文庫のテキストか zip、DRM のない EPUB、Web の記事）を取り込む
                                      （-o <ファイル> で送らずにファイルに書く）。read [list]、read rm <番号>
  brain-deck read feed add <URL> [--limit-items 10]、feed list、feed rm <番号>、feed sync   RSS の記事を取り込む
  brain-deck files ls [<Brain のフォルダ>] [--sort size]   Brain のファイル（設定の files.roots の場所だけ）の一覧。
                                      フォルダを書かなければ場所の一覧。--sort size は大きい順（フォルダは中の合計）
  brain-deck files get <Brain のファイル> [-o <ファイル>] [--force]   取り出す。files put <ファイル>... <Brain のフォルダ>
                                      で足す、files rm <Brain のファイル>... で消す（delete: true の場所だけ）
  brain-deck calibrate [--points 9] [--no-wait]   タッチのキャリブレーションを始める（Brain の画面の十字を順に押す。
                                      終わるまで待ち、前と後のずれを出す）。--status で今の変換、--reset で設定の min と max に戻す
  brain-deck sync [--wait 90s] [--if-older 10m]   時刻、予定、天気、読み物の RSS、手書きメモの取り出しを、まとめて行う
                                      （~/.config/brain-deck/sync.yaml で選ぶ。--wait は Brain の準備を待つ。--if-older は、
                                      Brain がその時間のうちに同期していれば何もしない）。終了コードは、失敗があれば 9
  brain-deck time sync                PC の時刻を Brain に送る
  brain-deck status                   Brain の状態（版、時刻と内蔵 RTC、PC とのつながり、レイヤー、USB の形、電池、
                                      ふた、Brain の設定のアプリで変えた上書きの値）
  brain-deck overrides [list]         Brain の設定のアプリで変えた上書きの値（設定ファイルの値と並べる）
  brain-deck overrides clear <名前>... | all   上書きを消して、設定ファイルの値に戻す
  brain-deck overrides set <名前> <値>   上書きを置く（backlight.level 1〜7、backlight.dim_after 3m か off、
                                      backlight.offline.dim_after、backlight.photo true か false、lid.close off か none、
                                      lid.tablet_keys off か on、ime.method）
  brain-deck battery log [-n 行数]    電池だけで動かしたときの電圧の記録（CSV。Brain の battery-log.csv）
  brain-deck battery log --fit        記録から、残りの見積もりの表（設定の battery.curve）を作る
  brain-deck usb-mode [keyboard|mouse|toggle]  USB の形を見る・切り替える。keyboard はキーボードだけ（ブートキーボード。
                                      起動したときの形）、mouse はキーボードとマウス。USB を付け直すので、2〜3 秒切れる
  brain-deck net-mode [ncm|ecm|rndis]  USB のネットワークの方式を見る・切り替える。ncm は既定（PC 向け）、rndis は
                                      Android のイーサネットテザリングなど NCM を持たない相手向け。ecm はカーネルにあるときだけ。
                                      USB を付け直すので、数秒、キー入力、SSH、シリアルが切れる。起動し直すと
                                      /etc/lefthand/gadget.env の NET_MODE（既定 ncm）に戻る
  brain-deck terminal [on|off|toggle|pc|brain]  端末モード（Brain の画面とキーボードで PC か Brain 自身にログインする）を
                                      見る・切り替える。on は Brain の画面で PC か Brain かを選ぶ。pc、brain で直接選ぶ。
                                      シリアルの端末モードでは、入るとき・抜けるときに USB を付け直す
  brain-deck update <ファイル.update.tar.gz>  lefthand と gadget-setup.sh を組で入れ替える（go run ./tools/mkupdate で作る）。
                                      Brain が確かめてから入れ替え、つなぎ直して確かめる。失敗したら前の版に戻す。
                                      要れば USB を付け直す。--dry-run で確かめるだけ
  brain-deck update --rollback        前の版（.prev）に戻す。もう一度実行すると、戻す前の版になる
  brain-deck update --status          今の版と、最後の更新の結果
  brain-deck restart [--usb]          lefthand を再起動する（--usb なら USB も付け直す）
  brain-deck reboot [--no-wait]       Brain を再起動し、起動するまで待つ（約 1 分。--timeout 4m が既定）
  brain-deck poweroff [--no-wait]     Brain の電源を切る
  brain-deck logs [-n 100] [-f]       lefthand と更新の係のログ（journal）。-f で追いかける（Ctrl-C で終わる）
  brain-deck config get [--prev]      Brain の設定ファイル（/etc/lefthand/config.yaml）を、コメントもそのまま書き出す
  brain-deck config put <ファイル|-> [--restart] [--dry-run]   設定を検証してから、そのまま書き、すぐ反映する
                                      （前の版は .prev）。再起動が要る項目を変えるときは --restart
  brain-deck config check <ファイル>  設定を Brain で検証するだけ
  brain-deck backup [-o <ファイル>]   Brain の設定ファイルとデータ（Todo、メモ、読み物、写真、背景画像、上書きの値、
                                      日本語入力の学習など）を 1 つのファイルにして PC に保存する（既定は
                                      ~/.local/share/brain-deck/backups/brain-<日時>.tar.gz。個人のデータが入る）
  brain-deck backup list | show <ファイル>   保存したバックアップの一覧、中身
  brain-deck restore <ファイル> [--no-config] [--no-data] [--no-anthy] [--dry-run]
                                      バックアップを Brain に戻す。Brain が確かめ、今の状態を Brain に残してから
                                      入れ替え、つなぎ直して確かめる（失敗したら元に戻す）
  brain-deck restore --undo           最後の restore の前の状態に戻す
  brain-deck profile [show|list]      PC ごとの設定（Brain の設定の profiles）の今の状態、一覧
  brain-deck profile use <名前>|none  手で選ぶ（別の PC が名乗るまで続く）。profile auto でやめる
  brain-deck profile announce [<名前>]   この PC の名前を Brain に知らせる（sync が最初に行う。名前は sync.yaml の
                                      pc_name、なければホスト名）
  brain-deck exec --result <id> [--limit 10m] -- <コマンド> [引数...]
                                      コマンドを実行し、結果をテキストのタイル <id> に出す（実行中、成功、失敗と、
                                      出力の最後の行）。終了コードはコマンドのもの
  brain-deck version

共通のオプション：
  --port <パス>      ポートを指定する（例：/dev/serial/by-id/usb-SHARP_Brain_0123456789-if05、/dev/cu.usbmodem01234567895）。
                     環境変数 BRAIN_DECK_PORT でも指定できる。省略すると探す
  --timeout <時間>   全体の時間の上限（既定 5s）
  --no-time-sync     text、qr、graph、calendar sync、weather sync の前に、Brain の時刻を合わせない
  --id <id>          presence で、在席ランプの id（省略すると presence）
  --config <パス>    calendar sync、weather sync の設定ファイル（既定 ~/.config/brain-deck/calendars.yaml、weather.yaml）
  --city <ID>        weather sync で、地点（地点定義表の一次細分区域の id）
  --fetch-timeout <時間>  calendar sync、weather sync で取ってくる時間の上限（既定 30s。--timeout とは別）
  --usb              restart で、USB も付け直す
  --no-wait          reboot、poweroff で、頼んだら待たずに終わる
  -n, --lines <数>   logs で、読む行数（既定 100）。battery log で、最後の何行を読むか（既定はすべて）
  --fit              battery log で、記録から残りの見積もりの表を作る
  -f, --follow       logs で、新しいログを待って出し続ける
  --prev             config get で、前の版（.prev）を読む
  --restart          config put で、再起動が要る項目を変えてもよい（書いて lefthand を再起動する）
  --result <id>      exec で、結果を出すテキストのタイルの id
  --limit <時間>     exec で、コマンドの時間の上限（既定 10m。過ぎたら止めて「失敗」にする）
  -q, --quiet        成功したときに何も出さない
  -v, --verbose      通信の内容を標準エラーに出す

終了コード：0 成功、1 予期しないエラー、2 使い方の誤り、3 Brain が見つからない・返事がない、
           4 設定 GUI が接続中（ほかのプログラムがポートを使用中）、5 Brain がエラーを返した、6 ポートを開く権限がない、
           7 予定や予報の取得に失敗した（calendar sync は取れたカレンダーを送る。weather sync は失敗を Brain に知らせ、
             Brain は前の予報を残す）、
           8 更新・再起動に失敗した（update は前の版に戻した）。exec はコマンドの終了コード
           （コマンドが時間切れなら 124、同じ id で実行中なら 75）
`

type options struct {
	port       string
	timeout    time.Duration
	style      string
	ttl        time.Duration
	clear      bool
	list       bool
	noTimeSync bool
	id         string // presence：在席ランプの id
	top        bool   // todo add：先頭に足す
	json       bool   // todo list、calendar list：JSON で出す
	// calendar sync
	ics          []string // --ics（何度でも）
	days         int
	config       string
	dryRun       bool
	fetchTimeout time.Duration
	city         string // weather sync：地点
	quiet        bool
	verbose      bool
	// update、restart、reboot、logs、config
	status    bool // update --status
	rollback  bool // update --rollback
	usb       bool // restart --usb
	noWait    bool // reboot、poweroff
	follow    bool // logs -f
	lines     int  // logs -n
	fit       bool // battery log --fit
	prev      bool // config get --prev
	restartOK bool // config put --restart
	// exec
	result string
	limit  time.Duration
	args   []string
	dashAt int // `--` のあとの最初の引数の位置（なければ -1）
	// photo、memo、read、files、sync のオプション（コマンドごとに、使えるものを確かめる）
	vals  map[string]string // 値を取るもの（-o、--of など）
	flags map[string]bool   // 値を取らないもの（--portrait など）
}

// subValueOpts、subFlagOpts は、photo、memo、read、files、sync のオプション。
var (
	subValueOpts = map[string]bool{"o": true, "page": true, "wait": true, "if-older": true, "out": true, "of": true, "zoom": true, "center": true, "brightness": true, "limit-items": true, "sort": true, "points": true}
	subFlagOpts  = map[string]bool{"portrait": true, "no-dither": true, "all": true, "force": true, "du": true, "recursive": true, "reset": true,
		"no-config": true, "no-data": true, "no-anthy": true, "undo": true}
)

// subCommands は、vals と flags を使うコマンド。
var subCommands = " photo memo read files sync calibrate backup restore "

// checkSub は、コマンドが使えないオプションを書いていないかを確かめる。allowed は使える名前（空白区切り）。
func (o *options) checkSub(allowed string) error {
	for k := range o.vals {
		if !strings.Contains(" "+allowed+" ", " "+k+" ") {
			return usageError("--" + k + " は " + o.args[0] + " では使えません")
		}
	}
	for k := range o.flags {
		if !strings.Contains(" "+allowed+" ", " "+k+" ") {
			return usageError("--" + k + " は " + o.args[0] + " では使えません")
		}
	}
	return nil
}

type usageError string

func (e usageError) Error() string { return string(e) }

// parseArgs は、オプションをどこに書いても受け付ける（`text build "x" --style ok` も `--style ok text build "x"` も）。
// `--` のあとは、すべてふつうの引数にする（`-` で始まるテキストを渡すとき）。
func parseArgs(argv []string) (*options, error) {
	o := &options{dashAt: -1, timeout: defaultTimeout, port: os.Getenv("BRAIN_DECK_PORT"), fetchTimeout: defaultFetchTimeout}
	withValue := map[string]bool{"port": true, "timeout": true, "style": true, "ttl": true, "ics": true, "days": true, "config": true, "fetch-timeout": true, "city": true, "id": true, "n": true, "lines": true, "result": true, "limit": true}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			o.dashAt = len(o.args)
			o.args = append(o.args, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" || isNumber(a) { // -5 などの数はふつうの引数（graph の値）
			o.args = append(o.args, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if subValueOpts[name] || subFlagOpts[name] {
			if subValueOpts[name] && !hasVal {
				if i+1 >= len(argv) {
					return nil, usageError("--" + name + " に値がありません")
				}
				i++
				val = argv[i]
			} else if subFlagOpts[name] && hasVal {
				return nil, usageError("--" + name + " は値を取りません")
			}
			if name == "out" {
				name = "o"
			}
			if subValueOpts[name] {
				if o.vals == nil {
					o.vals = map[string]string{}
				}
				o.vals[name] = val
			} else {
				if o.flags == nil {
					o.flags = map[string]bool{}
				}
				o.flags[name] = true
			}
			continue
		}
		if withValue[name] && !hasVal {
			if i+1 >= len(argv) {
				return nil, usageError("--" + name + " に値がありません")
			}
			i++
			val = argv[i]
		} else if !withValue[name] && hasVal {
			return nil, usageError("--" + name + " は値を取りません")
		}
		var err error
		switch name {
		case "port":
			o.port = val
		case "timeout":
			o.timeout, err = time.ParseDuration(val)
			if err == nil && o.timeout <= 0 {
				err = errors.New("must be positive")
			}
		case "style":
			o.style = val
		case "ttl":
			o.ttl, err = parseTTL(val)
		case "clear":
			o.clear = true
		case "list":
			o.list = true
		case "no-time-sync":
			o.noTimeSync = true
		case "id":
			o.id = val
		case "ics":
			o.ics = append(o.ics, val)
		case "days":
			o.days, err = strconv.Atoi(val)
			if err == nil && (o.days < 1 || o.days > maxCalDays) {
				err = fmt.Errorf("must be 1..%d", maxCalDays)
			}
		case "config":
			o.config = val
		case "city":
			o.city = val
		case "fetch-timeout":
			o.fetchTimeout, err = time.ParseDuration(val)
			if err == nil && o.fetchTimeout <= 0 {
				err = errors.New("must be positive")
			}
		case "dry-run":
			o.dryRun = true
		case "top":
			o.top = true
		case "status":
			o.status = true
		case "rollback":
			o.rollback = true
		case "usb":
			o.usb = true
		case "no-wait":
			o.noWait = true
		case "f", "follow":
			o.follow = true
		case "n", "lines":
			o.lines, err = strconv.Atoi(val)
			if err == nil && (o.lines < 1 || o.lines > 2000) {
				err = errors.New("must be 1..2000")
			}
		case "fit":
			o.fit = true
		case "prev":
			o.prev = true
		case "restart":
			o.restartOK = true
		case "result":
			o.result = val
		case "limit":
			o.limit, err = time.ParseDuration(val)
			if err == nil && o.limit <= 0 {
				err = errors.New("must be positive")
			}
		case "json":
			o.json = true
		case "q", "quiet":
			o.quiet = true
		case "v", "verbose":
			o.verbose = true
		case "h", "help":
			return nil, usageError("")
		default:
			return nil, usageError("知らないオプション " + a)
		}
		if err != nil {
			return nil, usageError(fmt.Sprintf("--%s %q: %v", name, val, err))
		}
	}
	return o, nil
}

// isNumber は、a が数（-5、-1.5e3 など）か。
func isNumber(a string) bool {
	_, err := strconv.ParseFloat(a, 64)
	return err == nil
}

// parseTTL は Go の時間の書き方（90s、10m、1h30m）に加えて、日数（2d）を受け付ける。
func parseTTL(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil || n <= 0 {
			return 0, errors.New("use a duration such as 90s, 10m, 2h or 1d")
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	t, err := time.ParseDuration(s)
	if err != nil || t <= 0 {
		return 0, errors.New("use a duration such as 90s, 10m, 2h or 1d")
	}
	return t, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseArgs(argv)
	if err != nil {
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(stderr, "brain-deck: "+msg+"（使い方は brain-deck --help）")
			return exitUsage
		}
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	if len(o.args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	hint := "（使い方は brain-deck --help）"
	if (o.top || o.json) && o.args[0] != "todo" && !(o.json && strings.Contains(" calendar images weather graph photo memo read files sync ", " "+o.args[0]+" ")) {
		fmt.Fprintln(stderr, "brain-deck: --top は todo で、--json は todo、calendar、images、weather、graph で使います"+hint)
		return exitUsage
	}
	if o.id != "" && o.args[0] != "presence" {
		fmt.Fprintln(stderr, "brain-deck: --id は presence で使います"+hint)
		return exitUsage
	}
	if o.args[0] != "calendar" && (len(o.ics) > 0 || o.days != 0 || (o.config != "" && o.args[0] != "weather") ||
		(o.dryRun && o.args[0] != "images" && o.args[0] != "update" && o.args[0] != "config" && o.args[0] != "weather")) {
		fmt.Fprintln(stderr, "brain-deck: --ics、--days は calendar sync で、--config は calendar sync、weather sync で、--dry-run は calendar sync、weather sync、images prune、update、config put で使います"+hint)
		return exitUsage
	}
	if o.city != "" && o.args[0] != "weather" {
		fmt.Fprintln(stderr, "brain-deck: --city は weather sync で使います"+hint)
		return exitUsage
	}
	for _, f := range []struct {
		on   bool
		name string
		cmds string
	}{{o.rollback, "--rollback", "update"}, {o.status, "--status", "update calibrate"}, {o.usb, "--usb", "restart"}, {o.noWait, "--no-wait", "reboot poweroff calibrate"},
		{o.follow, "-f", "logs"}, {o.lines != 0, "-n", "logs battery"}, {o.fit, "--fit", "battery"}, {o.prev || o.restartOK, "--prev、--restart", "config"}, {o.result != "" || o.limit != 0, "--result、--limit", "exec"}} {
		if f.on && !strings.Contains(" "+f.cmds+" ", " "+o.args[0]+" ") {
			fmt.Fprintf(stderr, "brain-deck: %s は %s で使います%s\n", f.name, f.cmds, hint)
			return exitUsage
		}
	}
	if (len(o.vals) > 0 || len(o.flags) > 0) && !strings.Contains(subCommands, " "+o.args[0]+" ") {
		fmt.Fprintln(stderr, "brain-deck: -o、--of、--portrait などは photo、memo、read、files、sync で使います"+hint)
		return exitUsage
	}
	var cmd func(*Client) (string, error)
	runningCmd, warnOut = o.args[0], stderr
	switch o.args[0] {
	case "version":
		fmt.Fprintln(stdout, "brain-deck", versionString())
		return exitOK
	case "text":
		cmd, err = textCommand(o, stdin, kindText)
	case "qr":
		cmd, err = textCommand(o, stdin, kindQR)
	case "presence":
		cmd, err = presenceCommand(o)
	case "graph":
		cmd, err = graphCommand(o)
	case "todo":
		cmd, err = todoCommand(o, stdin)
	case "calendar":
		return runCalendar(o, stdout, stderr)
	case "weather":
		return runWeather(o, stdout, stderr)
	case "update":
		return runUpdate(o, stdout, stderr)
	case "restart":
		return runRestart(o, stdout, stderr)
	case "reboot", "poweroff":
		return runPower(o, stdout, stderr)
	case "logs":
		return runLogs(o, stdout, stderr)
	case "config":
		return runConfig(o, stdin, stdout, stderr)
	case "exec":
		return runExec(o, stdout, stderr)
	case "images":
		cmd, err = imagesCommand(o)
	case "photo":
		cmd, err = photoCommand(o)
	case "memo":
		cmd, err = memoCommand(o)
	case "read":
		cmd, err = readCommand(o)
	case "files":
		cmd, err = filesCommand(o)
	case "app", "apps":
		cmd, err = appCommand(o)
	case "calibrate":
		return runCalibrate(o, stdout, stderr)
	case "sync":
		return runSync(o, stdout, stderr)
	case "backup":
		return runBackup(o, stdout, stderr)
	case "restore":
		return runRestore(o, stdout, stderr)
	case "profile":
		cmd, err = profileCommand(o)
	case "time":
		if len(o.args) != 2 || o.args[1] != "sync" {
			err = usageError("time のあとには sync を書きます（brain-deck time sync）")
		}
		cmd = timeSync
	case "status":
		if len(o.args) != 1 {
			err = usageError("status は引数を取りません")
		}
		cmd = status
	case "battery":
		if len(o.args) != 2 || o.args[1] != "log" {
			err = usageError("battery のあとには log を書きます（brain-deck battery log）")
		}
		cmd = func(c *Client) (string, error) { return batteryLog(c, o) }
	case "usb-mode":
		cmd, err = usbModeCommand(o)
	case "net-mode":
		cmd, err = netModeCommand(o)
	case "terminal":
		cmd, err = terminalCommand(o)
	case "overrides":
		cmd, err = overridesCommand(o)
	default:
		err = usageError("知らないコマンド " + strconv.Quote(o.args[0]))
	}
	var lo localOutput
	if errors.As(err, &lo) {
		if !o.quiet {
			fmt.Fprintln(stdout, string(lo))
		}
		return exitOK
	}
	var ue usageError
	if err != nil && !errors.As(err, &ue) && o.args[0] == "read" {
		fmt.Fprintln(stderr, "brain-deck: "+err.Error()) // 取り込めなかった（ファイル、ネットワーク）
		return exitFetch
	}
	if err != nil {
		fmt.Fprintln(stderr, "brain-deck: "+err.Error()+hint)
		return exitUsage
	}

	deadline := time.Now().Add(o.timeout)
	c, unlock, err := connect(connectOptions{port: o.port, timeout: o.timeout, verbose: o.verbose, deadline: deadline})
	if err != nil {
		return report(stderr, err)
	}
	defer unlock()
	defer c.Close()
	callTimeout = func() time.Duration { return max(time.Until(deadline), 100*time.Millisecond) }
	out, err := cmd(c)
	if err != nil {
		return report(stderr, err)
	}
	if out != "" && !o.quiet {
		fmt.Fprintln(stdout, out)
	}
	return exitOK
}

// runningCmd は、実行しているコマンドの名前（エラーの説明を変える）。
var runningCmd string

// warnOut は、成功したときの注意を書く先（run の stderr）。
var warnOut io.Writer = os.Stderr

// callTimeout は、残りの時間（全体の --timeout まで）。
var callTimeout = func() time.Duration { return defaultTimeout }

// report は、エラーを分かりやすく書き、終了コードを返す。
func report(w io.Writer, err error) int {
	var be *busyError
	var br *brainError
	var ue usageError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintln(w, "brain-deck: "+ue.Error())
		return exitUsage
	case errors.As(err, &be):
		fmt.Fprintln(w, "brain-deck: "+describeBusy(be))
		return exitBusy
	case errors.Is(err, errPermission):
		fmt.Fprintf(w, "brain-deck: %v\n%s\n", err, permissionHint)
		return exitPermission
	case errors.Is(err, errNotFound):
		fmt.Fprintf(w, "brain-deck: Brain が見つかりません（%v）\n%s。--port で指定することもできます\n", err, portHint)
		return exitNoBrain
	case errors.Is(err, errNoReply):
		fmt.Fprintf(w, "brain-deck: Brain から返事がありません（%v）\nlefthand.service が動いているかを確かめてください（ssh brain systemctl status lefthand）\n", err)
		return exitNoBrain
	case errors.As(err, &br):
		fmt.Fprintf(w, "brain-deck: Brain がエラーを返しました：%s（%s）\n", br.Message, br.Code)
		for _, p := range br.Problems {
			if p.Path != "" {
				fmt.Fprintf(w, "  %s：%s\n", p.Path, p.Message)
			} else {
				fmt.Fprintf(w, "  %s\n", p.Message)
			}
		}
		if (br.Code == "conflict" || br.Code == "not_found") && runningCmd == "todo" {
			fmt.Fprintln(w, "読んだあとに Brain で項目が変わりました。brain-deck todo list で確かめてから、もう一度実行してください")
		}
		return exitBrainError
	}
	fmt.Fprintln(w, "brain-deck: "+err.Error())
	return exitInternal
}

// need は、デーモンが cmd に対応しているかを確かめる。
func need(c *Client, cmd string) error {
	if !c.Hello.has(cmd) {
		return &brainError{Code: "unknown_command", Message: fmt.Sprintf(
			"Brain の lefthand（%s）は %s に対応していません。lefthand を新しくしてください", c.Hello.Version, cmd)}
	}
	return nil
}

// ---------- text ----------

// textKind は、テキストのタイル（text）と QR コード（qr）の違い。どちらも同じ形のコマンドで中身を書く。
type textKind struct {
	name     string // コマンドの名前
	set, get string // プロトコルのコマンド
	widget   string // セルの widget
	example  string
	styles   bool // --style を使える
}

var (
	kindText = textKind{name: "text", set: "set_text", get: "get_text", widget: "text", example: `brain-deck text build "ビルド成功"`, styles: true}
	kindQR   = textKind{name: "qr", set: "set_qr", get: "get_qr", widget: "qr", example: `brain-deck qr share "https://example.com/"`}
)

func textCommand(o *options, stdin io.Reader, k textKind) (func(*Client) (string, error), error) {
	a := o.args[1:]
	if o.style != "" && !k.styles {
		return nil, usageError("--style は text で使います")
	}
	if o.list {
		if len(a) != 0 || o.clear {
			return nil, usageError(k.name + " --list は、ほかの引数を取りません")
		}
		return func(c *Client) (string, error) { return listTexts(c, k) }, nil
	}
	if len(a) == 0 {
		return nil, usageError(k.name + " のあとに id を書きます（" + k.example + "）")
	}
	params := map[string]any{"name": a[0], "source": "brain-deck"}
	switch {
	case o.clear:
		if len(a) != 1 || o.style != "" || o.ttl != 0 {
			return nil, usageError(k.name + " <id> --clear は、テキストや --style、--ttl を取りません")
		}
		params["clear"] = true
	case len(a) == 2:
		text := a[1]
		if text == "-" {
			b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
			if err != nil {
				return nil, err
			}
			text = strings.TrimRight(string(b), "\r\n")
		}
		params["text"] = text
		if o.style != "" {
			params["style"] = o.style
		}
		if o.ttl > 0 {
			params["ttl_sec"] = o.ttl.Seconds()
		}
	case len(a) == 1:
		return nil, usageError("テキストがありません（消すときは --clear）")
	default:
		return nil, usageError("テキストは 1 つの引数にします（空白を含むときは \"\" で囲む）")
	}
	return func(c *Client) (string, error) {
		if err := need(c, k.set); err != nil {
			return "", err
		}
		if !o.noTimeSync {
			if err := syncIfNeeded(c); err != nil {
				return "", err
			}
		}
		raw, err := c.Call(k.set, params, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Cleared bool            `json:"cleared"`
			Shown   bool            `json:"shown"`
			Entry   json.RawMessage `json:"entry"`
		}
		json.Unmarshal(raw, &r)
		id := a[0]
		if !r.Shown {
			fmt.Fprintf(warnOut, "brain-deck: 注意：今の設定には、id が %s の %s のセル（widget: %s）がありません。中身は Brain に保存しました\n", id, k.name, k.widget)
		}
		if r.Cleared {
			return id + ": 消しました", nil
		}
		msg := fmt.Sprintf("%s: %s", id, params["text"])
		var extra []string
		if s, _ := params["style"].(string); s != "" {
			extra = append(extra, s)
		}
		if o.ttl > 0 {
			extra = append(extra, o.ttl.String()+"で期限切れ")
		}
		if len(extra) > 0 {
			msg += "（" + strings.Join(extra, "、") + "）"
		}
		return msg, nil
	}, nil
}

type textInfo struct {
	Text      string     `json:"text"`
	Style     string     `json:"style"`
	SetAt     time.Time  `json:"set_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	Source    string     `json:"source"`
	Expired   bool       `json:"expired"`
}

func listTexts(c *Client, k textKind) (string, error) {
	if err := need(c, k.get); err != nil {
		return "", err
	}
	raw, err := c.Call(k.get, nil, callTimeout())
	if err != nil {
		return "", err
	}
	var r struct {
		Texts map[string]textInfo `json:"texts"`
		IDs   []string            `json:"ids"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	inCfg := map[string]bool{}
	for _, id := range r.IDs {
		inCfg[id] = true
		if _, ok := r.Texts[id]; !ok {
			r.Texts[id] = textInfo{Style: "-"}
		}
	}
	ids := make([]string, 0, len(r.Texts))
	for id := range r.Texts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	if k.styles {
		b.WriteString("id\t色\t状態\t書いた時刻\tテキスト\n")
	} else {
		b.WriteString("id\t状態\t書いた時刻\t文字列\n")
	}
	for _, id := range ids {
		t := r.Texts[id]
		state := "表示中"
		switch {
		case t.SetAt.IsZero():
			state = "未設定"
		case t.Expired:
			state = "期限切れ（薄く表示）"
		case t.ExpiresAt != nil:
			state = "期限 " + t.ExpiresAt.Local().Format("01/02 15:04")
		}
		if !inCfg[id] {
			state += "、セルなし"
		}
		at := ""
		if !t.SetAt.IsZero() {
			at = t.SetAt.Local().Format("01/02 15:04")
		}
		if k.styles {
			fmt.Fprintf(&b, "%s\t%s\t", id, t.Style)
		} else {
			fmt.Fprintf(&b, "%s\t", id)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", state, at, strings.ReplaceAll(t.Text, "\n", "⏎"))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------- time ----------

type timeInfo struct {
	Now        time.Time  `json:"now"`
	Synced     bool       `json:"synced"`
	TZ         string     `json:"timezone"`
	BootSource string     `json:"boot_source"`
	LastKnown  *time.Time `json:"last_known"`
}

// sourceNames は、この起動で時刻を合わせた側の名前。
var sourceNames = map[string]string{"gui": "設定 GUI", "brain-deck": "brain-deck", "brain": "Brain のキーボード", "rtc": "内蔵 RTC"}

// rtcInfo は get_status の rtc（lefthand の timesync.go の RTCInfo）。
type rtcInfo struct {
	Mode      string     `json:"mode"`
	Readable  bool       `json:"readable"`
	Error     string     `json:"error"`
	Time      *time.Time `json:"time"`
	OffsetMS  int64      `json:"offset_ms"`
	Boot      string     `json:"boot"`
	LastWrite string     `json:"last_write"`
	Log       []struct {
		At       time.Time `json:"at"`
		OffsetMS int64     `json:"offset_ms"`
		SinceSec int64     `json:"since_write_sec"`
	} `json:"log"`
}

var rtcModeNames = map[string]string{"read": "読むだけ", "write": "時刻を合わせたら書き戻す", "off": "使わない"}

// describe は、RTC の時刻と、PC との差（RTC − Brain + Brain − PC）を書く。
func (r rtcInfo) describe(brainNow time.Time, brainMinusPC time.Duration) string {
	mode := rtcModeNames[r.Mode]
	if mode == "" {
		mode = r.Mode
	}
	if !r.Readable {
		if r.Error == "" {
			return mode
		}
		return "読めません（" + r.Error + "）"
	}
	d := (time.Duration(r.OffsetMS)*time.Millisecond + brainMinusPC).Round(time.Second)
	s := fmt.Sprintf("%s（PC との差 %s、%s", r.Time.Format("2006-01-02 15:04:05"), longDuration(d), mode)
	if r.LastWrite != "" {
		s += "。最後に書いた結果：" + r.LastWrite
	}
	s += "）"
	if r.Boot != "" {
		s += "\n\t起動したとき：" + r.Boot
	}
	// 書き戻す前の RTC と PC の差の記録（新しい 5 つ）
	for i := max(len(r.Log)-5, 0); i < len(r.Log); i++ {
		l := r.Log[i]
		s += "\n\t" + l.At.Local().Format("2006-01-02 15:04") + " に合わせたとき：" + rtcDiffText(l.OffsetMS, l.SinceSec, false)
	}
	return s
}

// longDuration は、日をまたぐ差を「−5日18時間13分48秒」の形で書く。
func longDuration(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign, d = "−", -d
	}
	day := d / (24 * time.Hour)
	d -= day * 24 * time.Hour
	if day == 0 {
		return sign + d.String()
	}
	return fmt.Sprintf("%s%d日%v", sign, day, d)
}

// linkInfo は get_status の link（lefthand の link.go）。
type linkInfo struct {
	State   string `json:"state"`
	Online  bool   `json:"online"`
	Offline bool   `json:"offline_mode"`
}

func (l linkInfo) describe() string {
	switch l.State {
	case "configured":
		return "PC とつながっている"
	case "suspended":
		return "PC がスリープ中"
	case "power":
		return "電源だけ（PC と通信していない）"
	case "":
		return "分からない"
	}
	return "外れている（" + l.State + "）"
}

// syncIfNeeded は、Brain の時刻が合っていなければ合わせる（テキストの有効期限を正しくするため）。
func syncIfNeeded(c *Client) error {
	if !c.Hello.has("set_time") {
		return nil
	}
	t0 := time.Now()
	raw, err := c.Call("get_status", nil, callTimeout())
	if err != nil {
		return err
	}
	rtt := time.Since(t0)
	var st struct {
		Time timeInfo `json:"time"`
	}
	json.Unmarshal(raw, &st)
	off := t0.Add(rtt / 2).Sub(st.Time.Now)
	if st.Time.Synced && off.Abs() < timeSyncThreshold {
		return nil
	}
	_, err = setTime(c)
	return err
}

func setTime(c *Client) (string, error) {
	if err := need(c, "set_time"); err != nil {
		return "", err
	}
	raw, err := c.Call("set_time", map[string]any{"unix_ms": time.Now().UnixMilli(), "source": "brain-deck"}, callTimeout())
	if err != nil {
		return "", err
	}
	var r struct {
		Stepped  bool  `json:"stepped"`
		OffsetMS int64 `json:"offset_ms"`
		RTC      *struct {
			OffsetMS int64 `json:"offset_ms"`
			SinceSec int64 `json:"since_write_sec"`
		} `json:"rtc"`
		RTCWrite bool `json:"rtc_write"`
		timeInfo
	}
	json.Unmarshal(raw, &r)
	off := (time.Duration(r.OffsetMS) * time.Millisecond).Round(time.Millisecond)
	var out string
	if !r.Stepped {
		out = fmt.Sprintf("Brain の時刻は合っています（ずれ %v）", off)
	} else {
		dir := "遅れていた"
		if off < 0 {
			dir, off = "進んでいた", -off
		}
		out = fmt.Sprintf("Brain の時刻を合わせました（%v %s。今 %s）", off, dir, r.Now.Local().Format("2006-01-02 15:04:05"))
	}
	if r.RTC != nil {
		out += "\n" + rtcDiffText(r.RTC.OffsetMS, r.RTC.SinceSec, r.RTCWrite)
	}
	return out, nil
}

// rtcDiffText は、書き戻す前の内蔵 RTC と PC の差（RTC の遅れ方の記録）を書く。
func rtcDiffText(offMS, sinceSec int64, written bool) string {
	d := (time.Duration(offMS) * time.Millisecond).Round(100 * time.Millisecond)
	dir := "進んでいた"
	if d < 0 {
		dir, d = "遅れていた", -d
	}
	s := fmt.Sprintf("内蔵 RTC は PC より %v %s（RTC は秒の単位なので ±1 秒）", d, dir)
	if sinceSec > 0 {
		s += fmt.Sprintf("。前に書いてから %s", longDuration(time.Duration(sinceSec)*time.Second))
		if sinceSec >= 6*3600 {
			s += fmt.Sprintf("、1 日に %+.1f 秒", float64(offMS)/1000/(float64(sinceSec)/86400))
		}
	}
	if written {
		s += "。PC の時刻を書き戻しました"
	}
	return s
}

func timeSync(c *Client) (string, error) { return setTime(c) }

// ---------- status ----------

func status(c *Client) (string, error) {
	raw, err := c.Call("get_status", nil, callTimeout())
	if err != nil {
		return "", err
	}
	var st struct {
		Status struct {
			Layer string `json:"layer"`
			Label string `json:"label"`
		} `json:"status"`
		Uptime int       `json:"uptime_sec"`
		Time   timeInfo  `json:"time"`
		HID    *usbHID   `json:"hid"`
		Term   *termInfo `json:"terminal"`
		Bat    *battery  `json:"battery"`
		Light  *dimInfo  `json:"backlight"`
		Photo  *struct {
			Available bool   `json:"available"`
			Count     int    `json:"count"`
			Showing   string `json:"showing"`
		} `json:"photo"`
		RTC       *rtcInfo       `json:"rtc"`
		Link      *linkInfo      `json:"link"`
		Lid       *lidInfo       `json:"lid"`
		IME       *imeInfo       `json:"ime"`
		Overrides []overrideInfo `json:"overrides"`
		Profile   *profileInfo   `json:"profile"`
		Backup    *struct {
			BeforeRestore *struct {
				At time.Time `json:"at"`
			} `json:"before_restore"`
		} `json:"backup"`
	}
	json.Unmarshal(raw, &st)
	pcNow := time.Now()
	sync := "合わせてある"
	if !st.Time.Synced {
		sync = "未設定（brain-deck time sync で合わせる）"
	} else if s := sourceNames[st.Time.BootSource]; s != "" {
		sync = "合わせてある：" + s
	}
	off := st.Time.Now.Sub(pcNow).Round(time.Second)
	out := fmt.Sprintf("ポート\t%s\nlefthand\t%s（起動から %v）\nレイヤー\t%s\nBrain の時刻\t%s（%s、PC との差 %v）",
		c.Port, c.Hello.Version, time.Duration(st.Uptime)*time.Second, st.Status.Label,
		st.Time.Now.Format("2006-01-02 15:04:05 MST"), sync, off)
	if !st.Time.Synced && st.Time.LastKnown != nil {
		out += "\n最後に分かっていた時刻\t" + st.Time.LastKnown.Local().Format("2006-01-02 15:04:05")
	}
	if st.RTC != nil {
		out += "\n内蔵 RTC\t" + st.RTC.describe(st.Time.Now, off)
	}
	if st.Link != nil {
		out += "\nPC とのつながり\t" + st.Link.describe()
	}
	if st.HID != nil {
		out += "\nUSB\t" + st.HID.describe()
		if st.HID.Net != nil && *st.HID.Net != "" {
			out += "\nUSB のネットワーク\t" + netModeName(*st.HID.Net)
		}
	}
	if st.Term != nil {
		out += "\n端末モード\t" + st.Term.describe()
	}
	if st.Bat != nil {
		out += "\n電池\t" + st.Bat.describe()
	}
	if st.Light != nil {
		out += "\n画面の明るさ\t" + st.Light.describe()
	}
	if st.Photo != nil && st.Photo.Available {
		p := fmt.Sprintf("%d 枚", st.Photo.Count)
		if st.Photo.Showing != "" {
			p += "（今 " + st.Photo.Showing + " を出している）"
		}
		out += "\n写真\t" + p
	}
	if st.Lid != nil {
		out += "\nふた\t" + st.Lid.describe()
	}
	if st.IME != nil {
		out += "\n日本語入力\t" + st.IME.describe()
	}
	if c.Hello.has("get_overrides") {
		out += "\n上書き\t" + describeOverrides(st.Overrides)
	}
	if st.Profile != nil {
		out += "\nPC ごとの設定\t" + st.Profile.describe()
	}
	if st.Backup != nil && st.Backup.BeforeRestore != nil {
		out += "\n復元の前の状態\t" + st.Backup.BeforeRestore.At.Local().Format("2006-01-02 15:04") + " に残した（brain-deck restore --undo で戻せる）"
	}
	return out, nil
}

// lidInfo は get_status の lid（lefthand の lid.go の LidInfo）。
// imeInfo は get_status の ime（日本語入力。lefthand の ime.go）。
type imeInfo struct {
	Method    string   `json:"method"`
	Setting   string   `json:"setting"`
	Installed []string `json:"installed"`
	UimFep    bool     `json:"uim_fep"`
	Running   bool     `json:"running"`
	Terminal  bool     `json:"terminal"`
	Error     string   `json:"error"`
}

func (i imeInfo) describe() string {
	switch {
	case !i.UimFep:
		return "使えない（Brain に uim-fep がない）"
	case i.Method == "":
		return "使わない（設定 " + i.Setting + "）"
	}
	s := i.Method
	if i.Setting == "auto" {
		s += "（自動）"
	}
	if i.Running {
		s += "、uim-fep は動いている"
	} else {
		s += "、uim-fep は止まっている"
	}
	if i.Terminal {
		s += "、Brain のシェルでも使う"
	}
	if len(i.Installed) > 0 {
		s += "、入っている方式：" + strings.Join(i.Installed, " ")
	}
	if i.Error != "" {
		s += "、" + i.Error
	}
	return s
}

type lidInfo struct {
	Closed     bool   `json:"closed"`
	Tablet     bool   `json:"tablet"`
	TabletKeys string `json:"tablet_keys"`
	KeysOff    bool   `json:"keys_off"`
	Close      string `json:"close"`
}

func (l lidInfo) describe() string {
	s := "開いている"
	if l.Closed {
		s = "閉じている"
	}
	if l.Tablet {
		s += "（タブレットの形"
		if l.KeysOff {
			s += "。本体のキーは使っていない"
		}
		s += "）"
	}
	if l.Close == "none" {
		return s + "。閉じても何もしない（lid.close: none）"
	}
	if l.Closed {
		return s + "。画面を消している"
	}
	return s + "。閉じたら画面を消す"
}

// battery は get_status の battery（lefthand の battery.go の BatteryInfo）。
type battery struct {
	Percent    int    `json:"percent"`
	VoltageMV  int    `json:"voltage_mv"`
	Plugged    bool   `json:"plugged"`
	Charging   bool   `json:"charging"`
	Estimated  bool   `json:"estimated"`
	Source     string `json:"source"`
	ChargerOff bool   `json:"charger_off"`
}

func (b battery) describe() string {
	if b.Percent < 0 {
		return "読めません"
	}
	s := fmt.Sprintf("%d%%", b.Percent)
	switch {
	case b.Charging:
		s += "、充電中"
	case b.ChargerOff:
		s += "、USB 給電、充電していない（i.MX28 の充電器が止まっている。README の「充電」）"
	case b.Plugged:
		s += "、USB 給電"
	}
	if b.VoltageMV > 0 {
		s += fmt.Sprintf("（%.2f V", float64(b.VoltageMV)/1000)
		if b.Estimated {
			s += "。残りと充電中は電圧からの見積もり"
		}
		s += "）"
	}
	return s
}

// dimInfo は get_status の backlight（lefthand の backlight.go の DimInfo）。
type dimInfo struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Level   int    `json:"level"`
	Max     int    `json:"max"`
}

func (d dimInfo) describe() string {
	if !d.Enabled {
		return "自動減光なし（設定の backlight）"
	}
	return fmt.Sprintf("%s（%d/%d）", map[string]string{"bright": "明るい", "dim": "暗くしている", "off": "消している", "photo": "写真を出している"}[d.State], d.Level, d.Max)
}

// termInfo は get_status の terminal（lefthand の termmode.go の TermInfo）。
type termInfo struct {
	Active    bool   `json:"active"`
	State     string `json:"state"`
	Transport string `json:"transport"`
	Status    string `json:"status"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

func (t termInfo) describe() string {
	switch t.State {
	case "off":
		return "オフ"
	case "entering":
		return "入っている途中"
	case "leaving":
		return "抜けている途中"
	}
	s := "オン"
	if t.Transport == "command" {
		s += "（コマンド）"
	} else {
		s += "（シリアル）"
	}
	if t.Cols > 0 {
		s += fmt.Sprintf(" %d×%d", t.Cols, t.Rows)
	}
	if t.Status != "" {
		s += "：" + t.Status
	}
	return s
}

// terminalCommand は terminal。引数がなければ今の状態を出し、あれば切り替える。
func terminalCommand(o *options) (func(*Client) (string, error), error) {
	if len(o.args) > 2 {
		return nil, usageError("terminal の引数は on、off、toggle、pc、brain のどれか 1 つです")
	}
	old := errors.New("Brain の lefthand が古く、端末モードがありません")
	if len(o.args) == 1 {
		return func(c *Client) (string, error) {
			raw, err := c.Call("get_status", nil, callTimeout())
			if err != nil {
				return "", err
			}
			var st struct {
				Term *termInfo `json:"terminal"`
			}
			json.Unmarshal(raw, &st)
			if st.Term == nil {
				return "", old
			}
			return "端末モードは " + st.Term.describe(), nil
		}, nil
	}
	mode := o.args[1]
	switch mode {
	case "on", "off", "toggle", "pc", "brain":
	default:
		return nil, usageError("terminal の引数は on、off、toggle、pc、brain のどれかです（" + strconv.Quote(mode) + "）")
	}
	return func(c *Client) (string, error) {
		if !c.Hello.has("set_terminal") {
			return "", old
		}
		raw, err := c.Call("set_terminal", map[string]any{"mode": mode}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Terminal bool     `json:"terminal"`
			Info     termInfo `json:"info"`
		}
		json.Unmarshal(raw, &r)
		if r.Terminal {
			if r.Info.State == "on" {
				return "端末モードに入っています", nil
			}
			if r.Info.State == "choosing" {
				return "Brain の画面で、つなぐ先（PC か Brain）を選んでください（brain-deck terminal pc か brain で直接選べます）", nil
			}
			if mode == "brain" {
				return "Brain 自身にログインします（Brain の画面でパスワードを聞きます。USB は付け直しません）", nil
			}
			return "端末モードに入ります。シリアルの端末モードでは USB を付け直すので、数秒、キー入力、SSH、シリアルが切れます", nil
		}
		if r.Info.State == "off" {
			return "端末モードではありません", nil
		}
		return "端末モードを抜けます。シリアルの端末モードでは USB を付け直すので、数秒、SSH とシリアルが切れます", nil
	}, nil
}

// usbHID は get_status の hid。
type usbHID struct {
	Mouse     bool    `json:"mouse"`
	Media     *bool   `json:"media"` // 古い lefthand にはない
	Switching bool    `json:"switching"`
	Preparing bool    `json:"preparing"` // 起動のときの USB の準備中（フェーズ K6。古い lefthand にはない）
	USBState  *string `json:"usb_state"` // 古い lefthand にはない
	Net       *string `json:"net"`       // ネットワークの方式（ncm、ecm、rndis。古い lefthand にはない）
}

func (h usbHID) describe() string {
	var s string
	switch {
	case h.Mouse && h.Media != nil && *h.Media:
		s = "キーボードとマウスとメディアキー"
	case h.Mouse:
		s = "キーボードとマウス"
	case h.Media != nil:
		s = "キーボードだけ（ブートキーボード。メディアキーは送れない）"
	default:
		s = "キーボードだけ（ブートキーボード）"
	}
	if h.Preparing {
		s += "（起動のときの USB の準備中）"
	} else if h.Switching {
		s += "（切り替え中）"
	}
	if h.USBState != nil && *h.USBState == "suspended" {
		s += "。PC はスリープ中（USB がサスペンド）"
	}
	return s
}

// usbModeCommand は usb-mode。引数がなければ今の形を出し、あれば切り替える。
func usbModeCommand(o *options) (func(*Client) (string, error), error) {
	if len(o.args) > 2 {
		return nil, usageError("usb-mode の引数は keyboard、mouse、toggle のどれか 1 つです")
	}
	if len(o.args) == 1 {
		return func(c *Client) (string, error) {
			raw, err := c.Call("get_status", nil, callTimeout())
			if err != nil {
				return "", err
			}
			var st struct {
				HID *usbHID `json:"hid"`
			}
			json.Unmarshal(raw, &st)
			if st.HID == nil {
				return "", errors.New("Brain の lefthand が古く、USB の形を切り替えられません")
			}
			return "USB は " + st.HID.describe(), nil
		}, nil
	}
	mode := o.args[1]
	switch mode {
	case "keyboard", "mouse", "toggle":
	default:
		return nil, usageError("usb-mode の引数は keyboard、mouse、toggle のどれかです（" + strconv.Quote(mode) + "）")
	}
	return func(c *Client) (string, error) {
		if !c.Hello.has("set_usb_mode") {
			return "", errors.New("Brain の lefthand が古く、USB の形を切り替えられません")
		}
		raw, err := c.Call("set_usb_mode", map[string]any{"mode": mode}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Mouse     bool `json:"mouse"`
			Switching bool `json:"switching"`
		}
		json.Unmarshal(raw, &r)
		h := usbHID{Mouse: r.Mouse}
		if !r.Switching {
			return "USB はすでに " + h.describe() + " の形です", nil
		}
		return "USB を " + h.describe() + " の形に切り替えます。2〜3 秒、キー入力、SSH、シリアルが切れます", nil
	}, nil
}

// netModeName は、ネットワークの方式の表示。
func netModeName(m string) string {
	switch m {
	case "ncm":
		return "NCM（既定。Linux、macOS、Windows 10 以降の PC）"
	case "ecm":
		return "ECM"
	case "rndis":
		return "RNDIS（Android のイーサネットテザリング、Windows）"
	}
	return m
}

// netModeCommand は net-mode。引数がなければ今の方式を出し、あれば切り替える。
func netModeCommand(o *options) (func(*Client) (string, error), error) {
	if len(o.args) > 2 {
		return nil, usageError("net-mode の引数は ncm、ecm、rndis のどれか 1 つです")
	}
	old := errors.New("Brain の lefthand が古く、USB のネットワークの方式を切り替えられません")
	if len(o.args) == 1 {
		return func(c *Client) (string, error) {
			raw, err := c.Call("get_status", nil, callTimeout())
			if err != nil {
				return "", err
			}
			var st struct {
				HID *usbHID `json:"hid"`
			}
			json.Unmarshal(raw, &st)
			if st.HID == nil || st.HID.Net == nil {
				return "", old
			}
			if *st.HID.Net == "" {
				return "USB のネットワークの方式が分かりません（ネットワークのファンクションがありません）", nil
			}
			s := "USB のネットワークは " + netModeName(*st.HID.Net)
			if st.HID.Switching {
				s += "（切り替え中）"
			}
			return s, nil
		}, nil
	}
	mode := o.args[1]
	switch mode {
	case "ncm", "ecm", "rndis":
	default:
		return nil, usageError("net-mode の引数は ncm、ecm、rndis のどれかです（" + strconv.Quote(mode) + "）")
	}
	return func(c *Client) (string, error) {
		if !c.Hello.has("set_net_mode") {
			return "", old
		}
		raw, err := c.Call("set_net_mode", map[string]any{"mode": mode}, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Switching bool `json:"switching"`
		}
		json.Unmarshal(raw, &r)
		if !r.Switching {
			return "USB のネットワークはすでに " + netModeName(mode) + " です", nil
		}
		return "USB のネットワークを " + netModeName(mode) + " に切り替えます。数秒、キー入力、SSH、シリアルが切れます。" +
			"終わったら brain-deck net-mode で確かめてください", nil
	}, nil
}

// ---------- presence ----------

// presenceCommand は presence。引数がなければ今の状態を出し、あれば変える。
func presenceCommand(o *options) (func(*Client) (string, error), error) {
	if len(o.args) > 2 {
		return nil, usageError("presence の引数は、状態の名前か next、prev のどれか 1 つです（空白を含む名前は \"\" で囲む）")
	}
	old := func(c *Client, cmd string) error { return need(c, cmd) }
	if len(o.args) == 1 {
		if o.id != "" {
			return nil, usageError("--id は、状態を変えるときに使います")
		}
		return func(c *Client) (string, error) {
			if err := old(c, "get_widgets"); err != nil {
				return "", err
			}
			raw, err := c.Call("get_widgets", nil, callTimeout())
			if err != nil {
				return "", err
			}
			var r struct {
				Widgets struct {
					Presence map[string]struct {
						State string `json:"state"`
					} `json:"presence"`
				} `json:"widgets"`
				IDs    map[string][]string `json:"ids"`
				States map[string][]struct {
					Name  string `json:"name"`
					Color string `json:"color"`
				} `json:"presence_states"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return "", err
			}
			if len(r.IDs["presence"]) == 0 {
				return "", &brainError{Code: "not_found", Message: "今の設定に、在席ランプのセル（widget: presence）がありません"}
			}
			var b strings.Builder
			b.WriteString("id\t今の状態\t選べる状態\n")
			for _, id := range r.IDs["presence"] {
				var names []string
				for _, s := range r.States[id] {
					names = append(names, s.Name)
				}
				cur := r.Widgets.Presence[id].State
				if cur == "" && len(names) > 0 {
					cur = names[0] + "（まだ変えていない）"
				}
				fmt.Fprintf(&b, "%s\t%s\t%s\n", id, cur, strings.Join(names, "、"))
			}
			return strings.TrimRight(b.String(), "\n"), nil
		}, nil
	}
	params := map[string]any{"source": "brain-deck"}
	if o.id != "" {
		params["name"] = o.id
	}
	switch arg := o.args[1]; arg {
	case "next":
		params["step"] = 1
	case "prev":
		params["step"] = -1
	default:
		params["state"] = arg
	}
	return func(c *Client) (string, error) {
		if err := old(c, "set_presence"); err != nil {
			return "", err
		}
		raw, err := c.Call("set_presence", params, callTimeout())
		if err != nil {
			return "", err
		}
		var r struct {
			Name  string `json:"name"`
			State string `json:"state"`
		}
		json.Unmarshal(raw, &r)
		return fmt.Sprintf("%s: %s", r.Name, r.State), nil
	}, nil
}
