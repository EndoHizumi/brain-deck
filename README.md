# lefthand

Sharp Brain PW-SH2 を、PC 用の左手デバイスにするデーモン。
Brain 上の Brainux で動き、本体のキーボードとタッチパネルの入力を、設定どおりのキー入力に変えて
USB HID キーボードとして PC に送る。タッチパネルの画面には、どこを押すと何が送られるかを表示する。

- **キーボード**：本体のキーごとに、PC に送るキーやショートカットを割り当てる。例：A キーで Ctrl+Z。
- **タッチパネル**：画面を格子に分け、セルごとにキーを割り当てる。セルの枠とラベルが画面に表示され、押しているセルは枠が黄色く光る。
- **ウィジェット**：タッチのセルに、キーの代わりに時計などを表示できる。セルは複数の格子にまたがる大きさにもできる（`span`）。
- **時刻合わせ**：Brain には電池で動く時計（RTC）がないので、設定 GUI が接続したときに PC の時刻に合わせる。
- **レイヤー**：キーとタッチの割り当てを、まとめて切り替えられる。押しているあいだだけ、押すたびに、次の 1 キーだけ、の切り替え方がある。今のレイヤー名は画面の右上に出る。
- **PC から見た Brain**：標準の USB キーボードとして見えるので、PC 側に専用のソフトは要らない。同じ USB ケーブルで、設定や保守のためのネットワーク（SSH）とシリアルも使える。
- **設定 GUI**：PC のブラウザ（Chrome / Edge）から、USB シリアル経由で設定を読み書きできる。保存するとデーモンを止めずにすぐ反映する。

## 目次

1. [仕組み](#仕組み)
2. [必要なもの](#必要なもの)
3. [ビルド](#ビルド)
4. [インストール](#インストール)
5. [PC との接続](#pc-との接続)
6. [設定](#設定)
7. [設定 GUI](#設定-gui)
8. [時刻合わせ](#時刻合わせ)
9. [タッチのキャリブレーション](#タッチのキャリブレーション)
10. [日常の操作](#日常の操作)
11. [画面とコンソール](#画面とコンソール)
12. [困ったとき](#困ったとき)
13. [開発](#開発)
14. [フォントとライセンス](#フォントとライセンス)

## 仕組み

![入力の流れと起動時のユニットの順序](REPORT-architecture.png)

```
本体キーボード (brain-kbd-i2c)  ─┐
                                  ├─ lefthand ─→ /dev/hidg0 ─→ USB ─→ PC（キーボードとして認識）
タッチパネル   (mxs-lradc-ts)   ─┘      │  │
                                         │  └─→ /dev/fb0（セルとラベルを表示）
PC のブラウザ（設定 GUI） ←─ USB シリアル ─→ /dev/ttyGS1（設定の読み書き、学習モード）
```

- **入力の専有**：デーモンは本体のキーボードとタッチパネルを専有する。動いているあいだ、Brain 自身のコンソールには入力が届かない。
- **送信**：押したキーの組み合わせを、標準の 8 バイトのキーボードレポートで送る。同時に押せる通常キーは 6 つまで。オートリピートは PC 側に任せる。
- **終了時**：すべてのキーを離したレポートを送ってから終わる。キーが押しっぱなしにならない。
- **PC が応答しないとき**：PC が未接続やスリープ中でも、デーモンは止まらない。送れなかった状態は 0.2 秒ごとに送り直す。

起動時は、次の順に動く。

1. `ethernet_gadget.service` が USB ガジェットを作る。Brainux 標準の処理を drop-in で置き換え、ネットワーク（NCM）、キーボード（HID）、シリアル 2 つ（ACM。コンソール用と設定 GUI 用）の複合デバイスにする。Brain の usb0 には固定 IP 192.168.7.2 を付ける。
2. `lefthand.service` がデーモンを起動する。ガジェットの作成と、ログイン画面の ly の起動を待ってから動く。

電源を入れてからキー入力を受け付けるまで、約 68 秒かかる。

## 必要なもの

- **Brain 本体**：Sharp Brain PW-SH2 に Brainux（Debian 13 ベース）を入れたもの。カーネルが USB ガジェットの HID と ACM に対応していること。Brainux 標準のカーネルは対応していないので、再ビルドする。手順は [docs/kernel-build.md](docs/kernel-build.md)。
- **PC**：ビルド用に Go 1.27 以降。cgo は使わない。
- **USB ケーブル**：Brain と PC をつなぐもの。動作確認は Linux の PC で行った。

## ビルド

PC で、ARMv5（ARM926EJ-S）向けにクロスコンパイルする。

```sh
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -trimpath -o lefthand
go test ./...
```

できる実行ファイルは約 5.5MB で、フォントも含めてこれ 1 つで動く。

## インストール

PC から Brain にファイルを送り、Brain 上で install.sh を実行する。

```sh
scp -r lefthand config.yaml install.sh gadget-setup.sh systemd brain:lefthand/
ssh brain 'cd ~/lefthand && sudo sh install.sh'
```

install.sh は次のファイルを配置し、`systemctl daemon-reload` を行う。

| 手元のファイル | Brain 上の配置先 |
| --- | --- |
| lefthand | /usr/local/bin/lefthand |
| gadget-setup.sh | /usr/local/sbin/lefthand-gadget-setup |
| config.yaml | /etc/lefthand/config.yaml（まだ無いときだけ） |
| systemd/ethernet_gadget.service.d/lefthand.conf | /etc/systemd/system/ethernet_gadget.service.d/ |
| systemd/lefthand.service | /etc/systemd/system/ |

- **設定ファイル**：install.sh は、既にある `/etc/lefthand/config.yaml` を上書きしない。手元の設定を反映するときは、手動でコピーする。

  ```sh
  ssh brain 'cd ~/lefthand && sudo install -m 0644 config.yaml /etc/lefthand/config.yaml && sudo systemctl restart lefthand.service'
  ```

- **自動起動**：install.sh はサービスを有効にしない。初めて入れたときは、一度だけ有効にする。

  ```sh
  ssh brain sudo systemctl enable --now lefthand.service
  ```

- **ガジェットの設定**：drop-in と gadget-setup.sh は、次に Brain を起動したときから使われる。再起動せずに設定 GUI 用のシリアルを足すときは、`ssh brain sudo /usr/local/sbin/lefthand-gadget-setup` を実行する。USB を一度付け直すので、SSH が数秒止まる。そのあいだ lefthand.service を止め、終わったら再開する（このカーネルでは、/dev/hidg0 を開いたまま付け直すと、HID が使えなくなるため）。SSH が切れても止まらないよう、`sudo systemd-run --collect /usr/local/sbin/lefthand-gadget-setup` で実行するとよい。
- **元に戻す**：drop-in を消して `systemctl daemon-reload` すると、Brainux 標準のガジェット設定に戻る。Brainux 本体のファイルは書き換えていない。

## PC との接続

USB でつなぐと、PC には次の 4 つが見える。

| 機能 | PC 側の見え方 | Brain 側 | 用途 |
| --- | --- | --- | --- |
| HID キーボード | 「SHARP Brain」というキーボード | /dev/hidg0 | 左手デバイスとしての入力 |
| NCM | ネットワークインターフェース（Linux では enx8a158b443a01） | usb0 | SSH |
| CDC-ACM（1 つ目） | シリアルポート（Linux では /dev/ttyACM0） | /dev/ttyGS0 | シリアルコンソール用。getty は立てていない |
| CDC-ACM（2 つ目） | シリアルポート（Linux では /dev/ttyACM1） | /dev/ttyGS1 | 設定 GUI |

キーボードとしては、つなぐだけで使える。SSH を使うには、PC 側のインターフェースに固定 IP を付ける。PC に DHCP サーバーは要らない。

| 機器 | アドレス |
| --- | --- |
| PC | 192.168.7.1/24 |
| Brain の usb0 | 192.168.7.2/24 |

PC の `~/.ssh/config` に次のように書いておくと、`ssh brain` で入れる。この README のコマンドは、この設定を前提にしている。

```
Host brain
    HostName 192.168.7.2
    User user
```

## 設定

設定は `/etc/lefthand/config.yaml` に YAML（または JSON）で書く。[設定 GUI](#設定-gui) を使うか、ファイルを直接編集して `sudo systemctl restart lefthand.service` で反映する。
設定 GUI で保存すると、ファイルは GUI が YAML で書き直すので、**手で書いたコメントは消える**。前の版は `/etc/lefthand/config.yaml.prev` に残る。
形式の詳細は [docs/config.md](docs/config.md)、本体のキーの名前は [docs/keymap-pwsh2.md](docs/keymap-pwsh2.md) にある。
書き方の例は、このリポジトリの config.yaml にある。

変更する前に、デーモンを止めずに検証できる。

```sh
ssh brain lefthand -check /etc/lefthand/config.yaml
```

### 全体

```yaml
hid_device: /dev/hidg0      # 送信先。省略可
keyboard: brain-kbd-i2c     # 本体キーボード。省略可
touch: { ... }              # タッチパネルとソフトキーの範囲。省略するとタッチと画面を使わない
layers: [ ... ]             # レイヤーごとの割り当て。最初のものが base
display: { ... }            # 画面表示。省略可
```

入力デバイスは、`/dev/input/eventN` のパスでも、デバイス名でも指定できる。
event の番号は起動の順で変わることがあるので、デバイス名で書く。

### レイヤー（layers）

```yaml
layers:
  - name: base
    label: "基本"
    keys:
      KEY_Q: B                               # Q で B
      KEY_A: LCTRL+Z                         # A で Ctrl+Z
      KEY_LEFTALT: { layer_hold: edit }      # 文字切り替えを押しているあいだ edit
      KEY_ESC: { layer_to: base }            # 調べる・戻るで base に戻る
    touch:
      cols: 4
      rows: 3
      cells:
        "0,0": { key: B, label: "ブラシ" }
        "3,2": { layer_toggle: edit, label: "編集" }
  - name: edit
    label: "編集"
    keys:
      KEY_Q: LCTRL+C                         # edit では Q で Ctrl+C
      KEY_W: none                            # 何もしない
```

- **キーの名前**：左側は Linux のキー名（`KEY_A`、`KEY_SPACE` など）。どのキーがどの名前かは [docs/keymap-pwsh2.md](docs/keymap-pwsh2.md) にある。
- **送るキー**：右側は送るキーを `+` でつなぐ。大文字小文字は区別しない。
- **切り替え**：`layer_hold`（押しているあいだ）、`layer_toggle`（押すたび）、`layer_oneshot`（次の 1 キーだけ）、`layer_to`（そのレイヤーへ移る）の 4 つ。
- **透過**：書いていないキーとセルは、下のレイヤーの割り当てを使う。`none` と書くと無効になる。
- **押したまま切り替えたとき**：キーは押したときの割り当てで離すので、押しっぱなしにならない。
- **base に戻る手段**：切り替えたままになるレイヤーには、base に戻る手段が要る。ないと、読み込み時にエラーになる。
- **旧形式**：トップレベルに `keys` と `touch.cells` を書く形式も、base レイヤーとしてそのまま読める。

PC に送れるキーの名前は次のとおり。

| 種類 | 名前 |
| --- | --- |
| 修飾キー | LCTRL、LSHIFT、LALT、LGUI、RCTRL、RSHIFT、RALT、RGUI |
| 文字 | A〜Z、0〜9 |
| ファンクション | F1〜F12 |
| 編集 | ENTER、ESC、BACKSPACE、TAB、SPACE、INSERT、DELETE、HOME、END、PAGEUP、PAGEDOWN |
| 矢印 | UP、DOWN、LEFT、RIGHT |
| テンキー | KP0〜KP9、KPPLUS（+）、KPMINUS（-）、KPASTERISK（*）、KPSLASH（/）、KPDOT（.）、KPENTER |
| 記号 | MINUS（-）、EQUAL（=）、LEFTBRACE（[）、RIGHTBRACE（]）、BACKSLASH（\）、SEMICOLON（;）、APOSTROPHE（'）、GRAVE（`）、COMMA（,）、DOT（.）、SLASH（/） |

記号は US 配列での位置を送る。PC が日本語配列のときは、PC 側で別の文字になることがある。
`+` のように配列で位置が違う文字は、テンキーの名前で送ると配列によらない。例：拡大の Ctrl++ は `LCTRL+KPPLUS`。

### タッチパネル（touch とレイヤーの touch）

パネルの設定は `touch` に、格子とセルの割り当ては各レイヤーの `touch` に書く。

```yaml
touch:
  device: mxs-lradc-ts
  swap_xy: false
  min_x: 226
  max_x: 3938
  min_y: 3803
  max_y: 384
  soft_areas:                                 # 画面右の印刷された帯
    home: { x: [3740, 4095], y: [3377, 4095] }
```

- **セルの番号**：`"列,行"` で書き、左上が `"0,0"`。列は右へ、行は下へ増える。
- **min_x などの値**：パネルの生の座標と画面の端との対応。値の求め方は[タッチのキャリブレーション](#タッチのキャリブレーション)を参照。
- **判定**：触れた瞬間のセルで決まり、離すまで変わらない。指を滑らせても、別のセルには移らない。
- **格子の大きさ**：レイヤーごとに変えられる。cols と rows を省略すると base と同じ大きさになる。
- **label**：日本語も使える。`"\n"` で改行できる。その場合はダブルクォートで囲む。
- **label の大きさ**：セルに収まるよう、自動で決まる。等倍でも収まらない部分は切れる。
- **label がないセル**：送るキーを短く書き直して表示する。例：`LCTRL+LSHIFT+Z` は `Ctrl+Shift+Z`。
- **画面の見た目**：割り当てのないセルは、暗い枠だけを描く。押しているあいだ、そのセルの枠を太い黄色の二重の枠にする（`display.press_style`）。レイヤーを切り替えるセルは紫で描く。
- **ソフトキー**：画面右の帯（HOME ▲ ▼ ▶ ◀ 決定 戻る 操作機能）はタッチに反応する。レイヤーの `soft_keys` で割り当てられる。帯の座標は画面の右端と重なるので、割り当てた区画だけがセルより優先される。

### ウィジェットとセルの大きさ

タッチのセルには、キーの代わりにウィジェットを置ける。今あるのは時計（`clock`）。
また、どのセルも `span: [列数, 行数]` で、右と下のセルにまたがる大きさにできる。

```yaml
cells:
  "0,0": { widget: clock, span: [2, 2] }                          # 大きな時計（2 列 × 2 行）
  "2,0": { widget: clock, format: "15:04:05", label: "秒" }       # 秒まで出す（1 秒ごとに描き直す）
  "3,0": { widget: clock, tz: America/Los_Angeles, label: LA, key: LGUI+SPACE }  # タップでキーを送る
  "0,2": { key: ENTER, span: [3, 1], label: "決定" }              # 横に 3 つぶんのキー
```

- **書式**：`format`（時刻、既定 `15:04`）と `date_format`（日付、既定 `1月2日({wday})`、`none` で出さない）は Go の書き方。`{wday}` は日本語の曜日。詳しくは [docs/config.md](docs/config.md) の「ウィジェット」。
- **タップしたとき**：`key` や `layer_*` を書けば、ふつうのセルと同じく働く。書かなければ何もしない（枠も光らない）。
- **描き直し**：時計は 1 分に 1 回（秒を出すときだけ 1 秒に 1 回）、変わったセルだけを描き直す。実機で、秒つきの時計 1 つの描き直しは約 4 ms。
- **時刻を合わせていないとき**：時刻を橙色で描き、日付の代わりに「時刻未設定」と出す（[時刻合わせ](#時刻合わせ)）。
- **例**：[config/widgets-example.yaml](config/widgets-example.yaml) は、今の本番の設定（[config/current.yaml](config/current.yaml)）に、メニューから入る「情報」レイヤーを足したもの。

### データの置き場所（/var/lib/lefthand）

ウィジェットのデータは、設定ファイルとは別に `/var/lib/lefthand/` に置く（デーモンが作る。`-data-dir` で変えられる）。設定 GUI で設定を保存しても消えない。

| ファイル | 内容 |
| --- | --- |
| clock.json | 最後に時刻を合わせた記録（起動ごとの ID、時刻、ずれ、送った側） |

テキスト、Todo、カレンダーのデータも、ここに置く予定。

### 今のレイヤーの表示

画面の右上に、今のレイヤーの label を出す。色で入り方がわかる。

| 色 | 状態 |
| --- | --- |
| 青 | base だけ |
| 緑 | layer_toggle か layer_to で切り替えたまま |
| 橙 | layer_hold か layer_oneshot で、一時的に切り替えている |

セルの枠も同じ色になる。

### 画面表示（display）

```yaml
display:
  enabled: true     # false で画面を使わない
  device: /dev/fb0
  vt: 0             # 0 なら tty8 以降の空いている VT を使う
  rotate: 0         # 画面の回転。0, 90, 180, 270
  press_style: border  # 押したときの見せ方。border（枠を光らせる、既定）か fill（塗りつぶす）
```

- **press_style**：`border` は、押しているセルの内側に、外側が黒、内側が黄色の二重の枠を描く。どんな背景の上でも見えるようにするためで、描き直すのは枠の部分だけ。`fill` は、以前と同じくセル全体を黄色で塗りつぶす。詳しくは [docs/config.md](docs/config.md)。
- **設定 GUI から変えられる項目**：`display` のうち `press_style` だけは、GUI で保存すると再起動なしで反映する。

画面が使えないときは、ログに理由を出して、入力の変換だけを続ける。

## 設定 GUI

PC のブラウザから、USB シリアル（WebSerial）で Brain の設定を読み書きする。ソースは `gui/`、プロトコルは [docs/protocol.md](docs/protocol.md)。

![設定 GUI](docs/gui.png)

### 開き方

WebSerial に対応した **Chrome か Edge** で開く。WebSerial は https のページか localhost でしか使えない。Firefox と Safari は対応していない。

- **localhost で開く**（Node.js 22 以降）：

  ```sh
  cd gui
  npm install
  npm run dev          # http://localhost:5173/ を開く
  ```

  作り直しなしで置くだけにするなら、`npm run build` で `gui/dist/` に静的なファイルができる。`npx vite preview`（http://localhost:4173/）か、任意の静的サーバーで開く。`file://` では開けない。
- **https で開く**：`gui/dist/` をそのまま https のサーバーに置く。パスは相対なので、サブディレクトリでもよい。GitHub Pages なら、`.github/workflows/gui-pages.yml` が main への push のたびに置き直す（リポジトリの Settings → Pages の Source を「GitHub Actions」にしておく）。
- **Brain なしで試す**：URL に `?demo` を付けて開くと、設定の例（config.yaml）を持った模擬のデーモンにつながる。保存しても Brain には何も送らない。

### Linux でシリアルを使う権限

Linux では、/dev/ttyACM* は root と dialout グループしか開けない。ブラウザでポートを選んでも開けないときは、自分を dialout グループに入れる。

```sh
ls -l /dev/ttyACM*            # crw-rw---- 1 root dialout ... なら必要
sudo usermod -aG dialout $USER
```

一度ログアウトしてログインし直すと有効になる（`id` で dialout が出ればよい）。Windows と macOS では要らない。

ログインし直さずに今だけ使うなら、`sudo setfacl -m u:$USER:rw /dev/ttyACM1` でもよい。ただし、ケーブルを抜き差ししたり Brain を再起動したりすると、デバイスが作り直されて権限は消える。

ModemManager が動いている PC では、つないだ直後の数秒、ModemManager がポートを調べるために開くことがある。そのあいだは開けないので、少し待ってからつなぎ直す。気になるなら、udev の規則で Brain（1d6b:0104）に `ENV{ID_MM_DEVICE_IGNORE}="1"` を付けて、調べないようにする。

### 使い方

1. **接続**：Brain と PC を USB ケーブルでつなぎ、「Brain に接続」を押す。ポートの一覧から Brain（USB 1d6b:0104）を選ぶ。Brain のシリアルは 2 つあり、設定用は 2 つ目（Linux では /dev/ttyACM1）。違うほうを選ぶと「lefthand が答えません」と出るので、もう一度押して別のほうを選ぶ。「シリアルポートを開けません」と出るときは、権限がない（下の「Linux でシリアルを使う権限」）か、ほかのアプリが使っている。一度選んだポートは、次からは聞かれずにつながる。上に `lefthand` のバージョンと、Brain の今のレイヤーが出る。
2. **レイヤー**：上のタブで切り替える。「＋ レイヤー」で追加、「このレイヤーを消す」で削除。名前を変えると、そのレイヤーへ切り替える割り当ても書き換わる。表示名は Brain の画面の右上に出る名前。
3. **キーボード**：Brain の本体キーが並ぶ。濃い色がこのレイヤーで割り当てたキー、薄い色は下のレイヤーから透過したキー、斜線は割り当てられないキー（電源、ツール、ホーム、記号）。「『記号』を押しながら」にすると、記号キーを押しているあいだに届くコード（Q なら KEY_1）を編集できる。
4. **タッチ**：Brain の画面と同じ比率で、格子とラベルを、実機と同じ色と字形で描く。セルをクリックして選ぶ。セルをマウスで押さえているあいだは、Brain で押したときの見た目になる。見せ方（枠を光らせる、塗りつぶす）は「押したとき」で選ぶ。右の帯は、画面右に印刷されたソフトキー（HOME、▲ など）。列と行の数はここで変える。base 以外のレイヤーでは、格子を下のレイヤーのままにするか、上書きするかを選ぶ。小さくしてはみ出すセルがあれば、消してよいか聞く。「タッチパネルの調整」で、キャリブレーションの値とソフトキーの区画の座標も変えられる。
5. **割り当て**：選んだキーやセルに、右の欄で割り当てる。種類は、透過（このレイヤーには書かない）、キーを送る、何もしない（none）、レイヤーの 4 つの切り替え方。
6. **送るキー**：修飾キーのチェックと、キーの一覧から選ぶ。「PC のキーで入力」を押してから PC のキーボードで押すと、そのまま取り込む（例：Ctrl+Shift+Z を押すと `LCTRL+LSHIFT+Z`）。取り込むのは押した位置のキーなので、日本語配列の PC でも US 配列の名前になる。Ctrl+W や Ctrl+T など、ブラウザが先に使うキーは取り込めないので、一覧から選ぶ。
7. **学習モード**：「学習モード」を押してから Brain のキーを押すかタッチすると、そのキーやセルが選ばれる。そのあいだ、Brain は PC にキーを送らない。もう一度押すと終わる。GUI を閉じても、30 秒で Brain は元に戻る。
8. **検証**：編集するたびに Brain で検証し、誤りをその場所（キーやセルの赤い枠、タブの数字）と右の一覧に出す。一覧をクリックすると、その場所へ移る。
9. **保存**：「Brain に保存…」で、変更点の一覧が出る。確かめて「保存して反映する」を押すと、Brain が検証してから保存し、すぐに反映する。押しているキーはいったん離れる。誤りがあるあいだは保存できない。
10. **ファイル**：「YAML で書き出す」「JSON で書き出す」で、編集中の設定を PC に保存する。「ファイルを開く」で読み込む。読み込んだだけでは Brain は変わらない。Brain につないでいなくても、ファイルの編集はできる（検証は Brain につないだときに行う）。

- **ウィジェット**：セルを選び、種類を「ウィジェット（時計）」にすると、書式、タイムゾーン、見出し、タップしたときの動きを選べる。プレビューは PC の今の時刻で描き、時刻が変わるたびに描き直す（Brain のタイムゾーンが PC と違うと、`tz` を書いていない時計の表示は Brain と違う）。
- **セルの大きさ**：セルを選び、「大きさ」の列と行を変える。広げた範囲に、このレイヤーのセルがあれば、消してよいか聞く。
- **時刻**：接続するたびに、PC の時刻を Brain に送って合わせる。ずれていたときと、Brain のタイムゾーンが PC と違うときは、そのことを出す。
- **GUI で変えられない項目**：`hid_device`、`keyboard`、`touch.device`、`display`（`press_style` を除く）は、デーモンを再起動しないと変えられないので、GUI からの保存では変えられない（変えると誤りになる）。ファイルを直接編集して、サービスを再起動する。
- **ほかの人が同時に**：ポートは 1 つのタブしか開けない。SSH でファイルを直接編集したときは、GUI で「切断」して接続し直すと読み直す。

## 時刻合わせ

### Brain の時計（実機で調べた結果）

| 項目 | 結果 |
| --- | --- |
| タイムゾーン | `/etc/localtime` が Asia/Tokyo（JST）。`/etc/timezone` は Etc/UTC と書いてあるが、使われていない |
| RTC | なし。カーネルにドライバ（stmp3xxx-rtc）はあるが、デバイスツリーにデバイスがなく、`/dev/rtc0` もない |
| 起動したときの時刻 | systemd-timesyncd が保存した時刻（`/var/lib/systemd/timesync/clock`）と、fake-hwclock（`/etc/fake-hwclock.data`、1 時間ごとと終了時に保存）から戻す |
| 電源を切ったあと | 保たれない。最後に保存した時刻から再開するので、切っていたあいだの分だけ遅れる。調べたときは PC より 37 時間 25 分遅れていた |
| NTP | systemd-timesyncd は動いているが、届くサーバーがなく、一度も同期していない |

### 設定 GUI で合わせる（既定）

設定 GUI は、Brain に接続するたびに PC の時刻を送り（`set_time`）、デーモンがシステムの時刻を合わせる。

- **合わせたかどうか**：Brain を起動してから一度でも合わせれば「合わせ済み」。デーモンを再起動しても覚えている（`/var/lib/lefthand/clock.json`）。Brain を再起動すると「未設定」に戻り、時計に「時刻未設定」と出る。
- **タイムゾーン**：変えない。Brain と PC で違えば、GUI がそのことを出す。時計ごとに `tz` で変えられる。
- **確かめ方**：`ssh brain date` か、GUI の接続の知らせ。`ssh brain cat /var/lib/lefthand/clock.json` で最後に合わせた記録が見られる。

### PC を NTP サーバーにする（提案。PC 側の設定はユーザーが行う）

NCM（USB のネットワーク）で、Brain の timesyncd が PC から時刻を取るようにもできる。GUI を開かなくても、つないでいるあいだ時刻が合い続ける。NTP で合っているあいだは、デーモンも「合わせ済み」とみなす。

1. **PC（Linux、chrony の例）**：`/etc/chrony/conf.d/brain.conf`（ディストリビューションによっては `/etc/chrony.conf` に追記）に次を書き、`sudo systemctl restart chronyd`（または `chrony`）。ファイアウォールがあれば、usb のインターフェースで UDP 123 を通す。

   ```
   allow 192.168.7.0/24
   # PC がインターネットにつながっていないときも、自分の時計を配る
   local stratum 10
   ```

   macOS では、標準の時刻合わせ（timed）は NTP サーバーにならないので、Homebrew の chrony などを使う。

2. **Brain**：timesyncd に PC を教える。

   ```sh
   ssh brain 'sudo mkdir -p /etc/systemd/timesyncd.conf.d && printf "[Time]\nNTP=192.168.7.1\n" | sudo tee /etc/systemd/timesyncd.conf.d/lefthand.conf && sudo systemctl restart systemd-timesyncd'
   ssh brain timedatectl     # System clock synchronized: yes になればよい
   ```

- **どちらがよいか**：まずは設定 GUI の `set_time` で足りる（GUI を開くたびに合う）。GUI を開かない日も時計を使うなら NTP を足す。フェーズ 2 の `brain-deck` コマンドにも時刻合わせを入れれば、PC の cron などから合わせることもできる。
- **注意**：NTP は、PC の usb のインターフェースに 192.168.7.1 が付いているあいだだけ届く。

## タッチのキャリブレーション

パネルの座標と画面の端の対応を測り、`touch` の min_x などに書く。

1. デーモンを止める。

   ```sh
   ssh brain sudo systemctl stop lefthand.service
   ```

2. 測定する。四隅を、左上、右上、右下、左下の順に 1 回ずつ押して離す。

   ```sh
   ssh brain 'cd ~/lefthand && sudo timeout 60 ./lefthand -calibrate config.yaml'
   ```

3. 終わると、推奨値が YAML の形で表示される。設定ファイルの `touch` に書き写す。
4. デーモンを再開する。

   ```sh
   ssh brain sudo systemctl start lefthand.service
   ```

- **押し方**：爪やペン先で、画面の端ぎりぎりを押す。指の腹で押すと、セルの境目が内側に寄る。
- **押す順番**：最後の 4 回を四隅として扱う。四隅以外を最後に押すと、推奨値がおかしくなる。
- **軸の反転**：min が max より大きいと、軸を反転して扱う。Brain の Y 軸は上下が逆なので、min_y が max_y より大きい。
- **画面の端**：端から指一本分ほどは、指の腹では反応しにくい。枠に当たってパネルを押し込めないためで、設定では直せない。

## 日常の操作

コマンドはどれも PC から実行する。

| やりたいこと | コマンド |
| --- | --- |
| 状態とログを見る | `ssh brain 'systemctl status lefthand.service; sudo journalctl -u lefthand -n 30'` |
| 止める | `ssh brain sudo systemctl stop lefthand.service` |
| 再開する | `ssh brain sudo systemctl start lefthand.service` |
| 設定を反映する | `ssh brain sudo systemctl restart lefthand.service` |

### ログを見ながら試す

デーモンを止めてから、`-v` を付けて手動で起動する。
受け取ったイベント、送ったレポート、タッチから送信までの時間、レイヤーの切り替え、画面の描き直しにかかった時間がログに出る。

```sh
ssh brain 'cd ~/lefthand && sudo timeout 60 ./lefthand -v /etc/lefthand/config.yaml'
```

手動で起動するときは、必ず `timeout` で時間を区切る。
デーモンはキーボードを専有するので、Brain 側からは止められない。

### Brain 本体で文字を打つとき

デーモンが動いているあいだは、Brain のキーボードがコンソールに届かない。
デーモンを止めると、画面がログイン画面の ly に戻り、キーボードも使えるようになる。

## 画面とコンソール

Brain の画面は、tty2 のログイン画面（ly）と、tty1 の getty も使っている。
デーモンはこれらと取り合わないよう、専用の仮想端末（VT）を使う。

- **起動時**：空いている VT（通常は tty8）に切り替え、グラフィックモードにして描く。コンソールは自分の VT が表示されていないあいだ画面に描かないので、取り合いは起きない。
- **自動消灯とカーネルのメッセージ**：グラフィックモードの VT は、自動消灯の対象にならない。カーネルのメッセージも画面に出ない。
- **終了時**：元の VT とテキストモードに戻し、専用 VT を解放する。SIGTERM、SIGINT、入力デバイスのエラーのいずれでも同じ。
- **強制終了されたとき**：systemd の `ExecStopPost` で `lefthand -restore-console` を実行し、`/run/lefthand-vt` に残った情報から元の VT に戻す。
- **ほかのプロセスが VT を切り替えたとき**：描画を止めて切り替えを許可し、2 秒後に専用 VT を取り戻す。デーモンの動作中はキーボードを専有していて、コンソールが見えても操作できないため。
- **描画の負荷**：起動時とレイヤーを切り替えたときに画面全体を描き、そのあとは変わったセルだけを描き直す。`press_style: border` では、セルのうち枠の帯だけを描き直して転送する。描画は優先度を下げた別のスレッドで行い、キー入力の処理を待たせない。

| 画面の仕様 | 値 |
| --- | --- |
| デバイス | /dev/fb0（braindrmfb） |
| 解像度 | 800 × 480 |
| 色 | 16 ビット、RGB565 |

## 困ったとき

| 症状 | 確かめること |
| --- | --- |
| PC にキーが届かない | `ls /dev/hidg0` でデバイスがあるか。ログに `hid write failed` が出ていないか。PC がスリープから戻ると、自動で送信を再開する |
| `ssh brain` がつながらない | PC 側のインターフェースに 192.168.7.1/24 が付いているか。USB ケーブルが抜けていないか |
| 押した位置と違うセルが反応する | キャリブレーションをやり直す |
| 画面が戻らず、デーモンの表示が残っている | `ssh brain sudo /usr/local/bin/lefthand -restore-console` で元の VT に戻す |
| 画面に何も表示されない | ログに `display disabled` が出ていないか。設定の `display.enabled` が false になっていないか |
| ラベルの一部が □ になる | フォントにない文字。JIS 第一・第二水準の漢字と、一般的な記号は表示できる。設定 GUI では、入力したときに知らせる |
| 設定 GUI がつながらない | ポートの一覧に Brain が 2 つあるか（`ls /dev/ttyACM*` で 2 つ）。1 つしかないなら、Brain のガジェットが古い（[インストール](#インストール)の「ガジェットの設定」）。Linux で開けないなら dialout グループ（[設定 GUI](#linux-でシリアルを使う権限)）。ログに `control: listening on /dev/ttyGS1` が出ているか |
| 設定 GUI で保存できない | 右の「検証」の一覧に誤りがないか。`hid_device` などを変えていないか |
| 時計に「時刻未設定」と出る | Brain を起動してから、時刻を合わせていない。設定 GUI で接続する（[時刻合わせ](#時刻合わせ)） |
| 時計の時刻が数時間ずれる | Brain のタイムゾーン（`ssh brain timedatectl`）。時計ごとに `tz` でも変えられる |

## 開発

### ファイル構成

| ファイル | 内容 |
| --- | --- |
| main.go | 入力の読み取り、HID レポートの送信、キャリブレーション、コマンドラインの処理 |
| config.go | 設定の読み込み、旧形式の変換、割り当ての組み立てと検証（誤りに場所を付ける） |
| control.go | 設定 GUI とのシリアル通信（/dev/ttyGS1） |
| widget.go | ウィジェット（時計）の書式、描き直しの間隔、描画 |
| timesync.go | 時刻合わせ（set_time）と、合わせたかどうかの判断 |
| store.go | データの置き場所（/var/lib/lefthand）の読み書き |
| apply.go | 設定の保存と、再起動なしの反映、失敗したときの巻き戻し |
| keymap_pwsh2.go | 設定 GUI に渡す、本体キーの配置と制約 |
| version.go | バージョンの文字列 |
| layer.go | レイヤーの重なり、透過の解決、押したときの割り当ての記録 |
| display.go | セルの配置と描画、描画用の goroutine |
| fb.go | フレームバッファの読み書き、裏画面への描画 |
| vt.go | 専用 VT の確保と、元の VT への復帰 |
| font.go、font/ | 埋め込みフォントと、そのライセンス |
| tools/mkfont/ | BDF フォントを埋め込み用の形式に変換するツール |
| gadget-setup.sh | USB ガジェットを作るスクリプト。bash で実行すること（sh では HID の設定が壊れる） |
| systemd/ | サービスと drop-in |
| install.sh | Brain 上での配置 |
| config.yaml | 設定の例。実機と同じ値 |
| config/current.yaml | Brain で動いている本番の設定の写し（2026-10-06 に退避） |
| config/widgets-example.yaml | ウィジェットと span の例（current.yaml に「情報」レイヤーを足したもの） |
| docs/config.md | 設定ファイルの形式（設定 GUI と共有） |
| docs/keymap-pwsh2.md | PW-SH2 のキー配列、同時押しの制約、画面右の帯の座標 |
| docs/kernel-build.md | HID と ACM を有効にしたカーネルのビルドと、SD カードへの差し替え |
| kernel/brain-deck.config | カーネルの設定の差分（brain_defconfig に重ねる） |
| docs/protocol.md | 設定 GUI とのプロトコル |
| gui/ | 設定 GUI（TypeScript、Vite） |
| REPORT.md | 作業の記録 |

### テスト

```sh
go test ./...
cd gui && npm test      # 設定 GUI（シリアルはモック）
```

設定 GUI のテストは、デモ用の模擬デーモン（`gui/src/demo.ts`）につないで、接続、編集、検証、保存、学習モード、切断を確かめる。
プレビューは、`gui/test/fixtures/` の PNG（`lefthand -render-png` で書き出したもの）と画素単位で比べる。画面の描き方を変えたら、PNG を作り直す。

```sh
go run . -render-png gui/test/fixtures/base.png config.yaml
go run . -render-png gui/test/fixtures/view.png -render-layer view config.yaml
go run . -render-png gui/test/fixtures/edit-hold.png -render-layer edit:hold -render-pressed "0,0 3,2" config.yaml
go run . -render-png gui/test/fixtures/base-pressed.png -render-pressed "0,0 3,0 0,2" config.yaml
go run . -render-png gui/test/fixtures/base-pressed-fill.png -render-press-style fill -render-pressed "0,0 3,0 0,2" config.yaml
# ウィジェット。時計の時刻と PC のタイムゾーンを固定する
T=2026-10-06T09:41:27+09:00; C=config/widgets-example.yaml
TZ=Asia/Tokyo go run . -render-png gui/test/fixtures/widgets.png -render-layer info -render-time $T $C
TZ=Asia/Tokyo go run . -render-png gui/test/fixtures/widgets-unsynced-pressed.png -render-layer info -render-time $T -render-unsynced -render-pressed "3,0 0,2" $C
TZ=Asia/Tokyo go run . -render-png gui/test/fixtures/widgets-pressed-fill.png -render-layer info -render-time $T -render-press-style fill -render-pressed "3,0 0,2" $C
```

時計の書式は、GUI（`gui/src/clock.ts`）でも Go と同じ結果になるよう作り直している。Go の結果の表（`gui/test/fixtures/goformat.json`）と比べるので、書式の処理を変えたら `LEFTHAND_UPDATE_GOFORMAT=1 go test -run GoFormatTable` で書き直す。

本体キーの表（keymap_pwsh2.go）を変えたら、GUI に同梱した表も `LEFTHAND_UPDATE_KEYMAP=1 go test -run KeymapJSON` で書き直す。

画面の見た目は、実機がなくても PNG に書き出して確かめられる。

```sh
go run . -render-png out.png -render-pressed "0,0 3,2" config.yaml
go run . -render-png edit.png -render-layer edit:hold config.yaml   # edit を一時的に重ねた画面
```

実機では、描画テストで専用 VT への切り替え、セルの押下と解除、元の VT への復帰を確かめられる。
押下中の画面は `/tmp/lefthand-fb-pressed.raw` に保存される。
Brain のカーネルには uinput がないので、タッチを自動で再現するテストはできない。

```sh
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go test -c -o lefthand.test
scp lefthand.test config.yaml brain:lefthand/
ssh brain 'sudo systemctl stop lefthand.service; cd ~/lefthand && sudo LEFTHAND_HW_TEST=1 timeout 60 ./lefthand.test -test.run HW -test.v; sudo systemctl start lefthand.service'
```

### コマンドラインのオプション

```
lefthand [-v] [-serial /dev/ttyGS1] [-data-dir /var/lib/lefthand] [config.yaml]
                                             通常の動作。設定を省略すると /etc/lefthand/config.yaml。
                                             -serial は設定 GUI と通信するシリアル（空なら使わない）
                                             -data-dir はウィジェットのデータと時刻合わせの記録を置く場所
lefthand -calibrate [config.yaml]            タッチの座標を測る
lefthand -check [config.yaml]                設定を検証して終わる
lefthand -dump-json [config.yaml]            layers の形にそろえた JSON を出力して終わる
lefthand -restore-console                    残った専用 VT を元に戻して終わる
lefthand -render-png out.png [config.yaml]   画面の見た目を PNG に書き出して終わる
         -render-layer name[:hold]           base に重ねるレイヤー（hold なら一時的な色）
         -render-pressed "列,行 ..."         押下中として描くセル
         -render-press-style border|fill     設定の display.press_style の代わりに使う見せ方
         -render-size 800x480                画面の大きさ
         -render-time 2026-10-06T09:41:00+09:00  時計に出す時刻（省略すると今）
         -render-unsynced                    時刻を合わせていないときの時計を描く
```

## フォントとライセンス

ラベルの描画には、Num Kadoma（門真なむ）氏の 8×12 ドット日本語ビットマップフォント
「k8x12」2021-05-05 版を使っている。JIS 第一・第二水準の漢字を含む。
配布元は https://littlelimit.net/k8x12.htm 。

BDF 版の `k8x12.bdf` を `tools/mkfont` で固定長のバイナリ `font/k8x12.bin`（約 112KB）に変換し、
実行ファイルに埋め込んでいる。変換し直すときは次のようにする。

```sh
go run ./tools/mkfont k8x12.bdf font/k8x12.bin
```

ライセンス（`font/k8x12-LICENSE.txt` に同梱のマニュアル全文）:

> These fonts are free software.
> Unlimited permission is granted to use, copy, and distribute them, with or without modification, either commercially or noncommercially.
> THESE FONTS ARE PROVIDED "AS IS" WITHOUT WARRANTY.
>
> これらのフォントはフリー（自由な）ソフトウエアです。
> あらゆる改変の有無に関わらず、また商業的な利用であっても、自由にご利用、複製、再配布することができますが、全て無保証とさせていただきます。

Copyright (C) 2015-2021 Num Kadoma
