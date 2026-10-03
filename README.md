# lefthand

Sharp Brain PW-SH2 を、PC 用の左手デバイスにするデーモン。
Brain 上の Brainux で動き、本体のキーボードとタッチパネルの入力を、設定どおりのキー入力に変えて
USB HID キーボードとして PC に送る。タッチパネルの画面には、どこを押すと何が送られるかを表示する。

- **キーボード**：本体のキーごとに、PC に送るキーやショートカットを割り当てる。例：A キーで Ctrl+Z。
- **タッチパネル**：画面を格子に分け、セルごとにキーを割り当てる。セルの枠とラベルが画面に表示され、押しているセルは黄色になる。
- **PC から見た Brain**：標準の USB キーボードとして見えるので、PC 側に専用のソフトは要らない。同じ USB ケーブルで、設定や保守のためのネットワーク（SSH）とシリアルも使える。

## 目次

1. [仕組み](#仕組み)
2. [必要なもの](#必要なもの)
3. [ビルド](#ビルド)
4. [インストール](#インストール)
5. [PC との接続](#pc-との接続)
6. [設定](#設定)
7. [タッチのキャリブレーション](#タッチのキャリブレーション)
8. [日常の操作](#日常の操作)
9. [画面とコンソール](#画面とコンソール)
10. [困ったとき](#困ったとき)
11. [開発](#開発)
12. [フォントとライセンス](#フォントとライセンス)

## 仕組み

![入力の流れと起動時のユニットの順序](REPORT-architecture.png)

```
本体キーボード (brain-kbd-i2c)  ─┐
                                  ├─ lefthand ─→ /dev/hidg0 ─→ USB ─→ PC（キーボードとして認識）
タッチパネル   (mxs-lradc-ts)   ─┘      │
                                         └─→ /dev/fb0（セルとラベルを表示）
```

- **入力の専有**：デーモンは本体のキーボードとタッチパネルを専有する。動いているあいだ、Brain 自身のコンソールには入力が届かない。
- **送信**：押したキーの組み合わせを、標準の 8 バイトのキーボードレポートで送る。同時に押せる通常キーは 6 つまで。オートリピートは PC 側に任せる。
- **終了時**：すべてのキーを離したレポートを送ってから終わる。キーが押しっぱなしにならない。
- **PC が応答しないとき**：PC が未接続やスリープ中でも、デーモンは止まらない。送れなかった状態は 0.2 秒ごとに送り直す。

起動時は、次の順に動く。

1. `ethernet_gadget.service` が USB ガジェットを作る。Brainux 標準の処理を drop-in で置き換え、ネットワーク（NCM）、キーボード（HID）、シリアル（ACM）の複合デバイスにする。Brain の usb0 には固定 IP 192.168.7.2 を付ける。
2. `lefthand.service` がデーモンを起動する。ガジェットの作成と、ログイン画面の ly の起動を待ってから動く。

電源を入れてからキー入力を受け付けるまで、約 68 秒かかる。

## 必要なもの

- **Brain 本体**：Sharp Brain PW-SH2 に Brainux（Debian 13 ベース）を入れたもの。カーネルが USB ガジェットの HID と ACM に対応していること。
- **PC**：ビルド用に Go 1.27 以降。cgo は使わない。
- **USB ケーブル**：Brain と PC をつなぐもの。動作確認は Linux の PC で行った。

## ビルド

PC で、ARMv5（ARM926EJ-S）向けにクロスコンパイルする。

```sh
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -trimpath -o lefthand
go test ./...
```

できる実行ファイルは約 4.4MB で、フォントも含めてこれ 1 つで動く。

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

- **ガジェットの設定**：drop-in は、次に Brain を起動したときから使われる。
- **元に戻す**：drop-in を消して `systemctl daemon-reload` すると、Brainux 標準のガジェット設定に戻る。Brainux 本体のファイルは書き換えていない。

## PC との接続

USB でつなぐと、PC には次の 3 つが見える。

| 機能 | PC 側の見え方 | 用途 |
| --- | --- | --- |
| HID キーボード | 「SHARP Brain」というキーボード | 左手デバイスとしての入力 |
| NCM | ネットワークインターフェース（Linux では enx8a158b443a01） | SSH |
| CDC-ACM | シリアルポート（Linux では /dev/ttyACM0） | シリアルログイン |

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

設定は `/etc/lefthand/config.yaml` に YAML で書く。変更したら `sudo systemctl restart lefthand.service` で反映する。
書き方の例は、このリポジトリの config.yaml にある。

### 全体

```yaml
hid_device: /dev/hidg0      # 送信先。省略可
keyboard: brain-kbd-i2c     # 本体キーボード。省略可
keys: { ... }               # 本体キーの割り当て
touch: { ... }              # タッチパネル。省略するとタッチと画面を使わない
display: { ... }            # 画面表示。省略可
```

入力デバイスは、`/dev/input/eventN` のパスでも、デバイス名でも指定できる。
event の番号は起動の順で変わることがあるので、デバイス名で書く。

### 本体キーの割り当て（keys）

左に Brain のキー、右に PC に送るキーを書く。

```yaml
keys:
  KEY_A: LCTRL+Z           # A で Ctrl+Z
  KEY_S: LCTRL+LSHIFT+Z    # S で Ctrl+Shift+Z
  KEY_SPACE: LSHIFT        # スペースを Shift として使う
  KEY_Q: B
```

- **左側**：Linux のキー名（`KEY_A`、`KEY_SPACE` など）。どのキーがどの名前かは、`-v` を付けて起動し、キーを押したときのログで確かめる。
- **右側**：送るキーを `+` でつなぐ。大文字小文字は区別しない。
- **割り当てのないキー**：何も送らない。

PC に送れるキーの名前は次のとおり。

| 種類 | 名前 |
| --- | --- |
| 修飾キー | LCTRL、LSHIFT、LALT、LGUI、RCTRL、RSHIFT、RALT、RGUI |
| 文字 | A〜Z、0〜9 |
| ファンクション | F1〜F12 |
| 編集 | ENTER、ESC、BACKSPACE、TAB、SPACE、INSERT、DELETE、HOME、END、PAGEUP、PAGEDOWN |
| 矢印 | UP、DOWN、LEFT、RIGHT |
| 記号 | MINUS（-）、EQUAL（=）、LEFTBRACE（[）、RIGHTBRACE（]）、BACKSLASH（\）、SEMICOLON（;）、APOSTROPHE（'）、GRAVE（`）、COMMA（,）、DOT（.）、SLASH（/） |

記号は US 配列での位置を送る。PC が日本語配列のときは、PC 側で別の文字になることがある。

### タッチパネル（touch）

画面を `cols` 列 × `rows` 行の格子に分け、セルごとにキーを割り当てる。

```yaml
touch:
  device: mxs-lradc-ts
  cols: 4
  rows: 3
  swap_xy: false
  min_x: 226
  max_x: 3938
  min_y: 3803
  max_y: 384
  cells:
    "0,0": B                                   # 画面にはキー名「B」を表示
    "1,0": { key: LCTRL+Z, label: "取り消し" }   # 「取り消し」を大きく、「Ctrl+Z」を下に小さく表示
    "3,2": { key: SPACE, label: "手のひら" }
```

- **セルの番号**：`"列,行"` で書き、左上が `"0,0"`。列は右へ、行は下へ増える。
- **min_x などの値**：パネルの生の座標と画面の端との対応。値の求め方は[タッチのキャリブレーション](#タッチのキャリブレーション)を参照。
- **判定**：触れた瞬間のセルで決まり、離すまで変わらない。指を滑らせても、別のセルには移らない。
- **label**：日本語も使える。`"\n"` で改行できる。その場合はダブルクォートで囲む。
- **label の大きさ**：セルに収まるよう、自動で決まる。等倍でも収まらない部分は切れる。
- **label がないセル**：送るキーを短く書き直して表示する。例：`LCTRL+LSHIFT+Z` は `Ctrl+Shift+Z`。
- **画面の見た目**：割り当てのないセルは、暗い枠だけを描く。押しているあいだ、そのセルは黄色になる。

### 画面表示（display）

```yaml
display:
  enabled: true     # false で画面を使わない
  device: /dev/fb0
  vt: 0             # 0 なら tty8 以降の空いている VT を使う
  rotate: 0         # 画面の回転。0, 90, 180, 270
```

画面が使えないときは、ログに理由を出して、入力の変換だけを続ける。

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
受け取ったイベント、送ったレポート、タッチから送信までの時間、画面の描き直しにかかった時間がログに出る。

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
- **描画の負荷**：起動時に画面全体を一度描き、そのあとは変わったセルだけを描き直す。描画は優先度を下げた別のスレッドで行い、キー入力の処理を待たせない。

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
| ラベルの一部が □ になる | フォントにない文字。JIS 第一・第二水準の漢字と、一般的な記号は表示できる |

## 開発

### ファイル構成

| ファイル | 内容 |
| --- | --- |
| main.go | 設定の読み込み、入力の読み取り、HID レポートの送信、キャリブレーション |
| display.go | セルの配置と描画、描画用の goroutine |
| fb.go | フレームバッファの読み書き、裏画面への描画 |
| vt.go | 専用 VT の確保と、元の VT への復帰 |
| font.go、font/ | 埋め込みフォントと、そのライセンス |
| tools/mkfont/ | BDF フォントを埋め込み用の形式に変換するツール |
| gadget-setup.sh | USB ガジェットを作るスクリプト。bash で実行すること（sh では HID の設定が壊れる） |
| systemd/ | サービスと drop-in |
| install.sh | Brain 上での配置 |
| config.yaml | 設定の例。実機と同じ値 |
| REPORT.md | 作業の記録 |

### テスト

```sh
go test ./...
```

画面の見た目は、実機がなくても PNG に書き出して確かめられる。

```sh
go run . -render-png out.png -render-pressed "0,0 3,2" config.yaml
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
lefthand [-v] [config.yaml]                  通常の動作。設定を省略すると /etc/lefthand/config.yaml
lefthand -calibrate [config.yaml]            タッチの座標を測る
lefthand -restore-console                    残った専用 VT を元に戻して終わる
lefthand -render-png out.png [config.yaml]   画面の見た目を PNG に書き出して終わる
         -render-pressed "列,行 ..."         押下中として描くセル
         -render-size 800x480                画面の大きさ
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
