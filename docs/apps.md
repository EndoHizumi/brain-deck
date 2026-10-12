# 自分でアプリを足す（widget: app）

Brain の画面に、自分で書いたスクリプトの出力を出す仕組み（フェーズ K5）。sh だけで書ける。

- アプリは、**説明のファイル `app.yaml` とスクリプト**を入れたフォルダ。PC から `brain-deck app install <フォルダ>` で Brain の `/var/lib/lefthand/apps/<名前>/` に置く。
- 設定のセルに `{widget: app, name: <名前>}` と書くと、そのセルにスクリプトの出力を描く。ホーム画面から開くには、そのセルを置いたレイヤーを作り、`home.apps` に `{name: …, icon: …, layer: <そのレイヤー>}` を足す（ほかのアプリと同じ）。
- 例：[contrib/apps/](../contrib/apps/)（`sdcard`：SD カードの空きの内訳、`dice`：サイコロ、`load`：Brain の負荷）と、それを使う設定 [config/apps-example.yaml](../config/apps-example.yaml)。

```sh
brain-deck app install contrib/apps/dice     # 置く（同じ名前なら入れ替える。アプリのデータは残す）
brain-deck app list                          # 置いてあるアプリと、動きの様子
brain-deck app rm dice                       # 消す（アプリが書いたデータも消す）
```

設定 GUI の「アプリ」のタブでも、一覧を見て消せる。セルの種類「ウィジェット」→「アプリ（自分で足す）」で名前を選ぶ（プレビューでは説明だけを描く）。

## いちばん小さいアプリ

```
hello/
  app.yaml
  main.sh
```

`app.yaml`：

```yaml
title: こんにちは
every: 1m
```

`main.sh`：

```sh
echo "@title こんにちは"
echo "@big $(date +%H:%M)"
echo "Brain は $(cut -d' ' -f1 /proc/uptime | cut -d. -f1) 秒動いています"
echo "@button again もう一度"
```

```sh
brain-deck app install hello
```

設定のレイヤーのセルに `"0,0": {widget: app, name: hello}` と書いて `brain-deck config put` すると、そのレイヤーを開いているあいだ、1 分ごと（とタップやボタンのとき）に `main.sh` を動かして描き直す。

## app.yaml

| 項目 | 既定 | 意味 |
| --- | --- | --- |
| `name` | フォルダの名前 | アプリの名前。英小文字、数字、`_`、`-` の 32 字まで。書くならフォルダの名前と同じ（`brain-deck app install` では、Brain のフォルダの名前になる） |
| `title` | `name` | 画面に出す名前（セルの見出し。設定の `label`、出力の `@title` があればそちら） |
| `description` | | 一覧に出す説明 |
| `icon` | | ホーム画面に置くときの組み込みのアイコン（`brain-deck app install` が勧める） |
| `run` | `main.sh` | 動かすスクリプト（フォルダの中のファイル） |
| `mode` | `interval` | `interval`：決まった間隔で動かし、出力を描く。`stream`：動き続け、1 行ごとに描き直す |
| `every` | `1m` | interval の間隔（`2s`〜`24h`）。`off` なら、タップやボタンのときだけ動かす |
| `timeout` | `10s` | interval の 1 回の時間の上限（`1s`〜設定の `apps.max_timeout`。既定の上限 `60s`） |
| `memory_mb` | `32` | メモリの上限（MB。仮想メモリ。`8`〜設定の `apps.max_memory_mb`。既定の上限 `64`） |
| `output_kb` | `32` | 出力の上限（KiB。interval は 1 回、stream は 10 秒にこの 2 倍。`1`〜設定の `apps.max_output_kb`。既定の上限 `256`） |
| `keys` | | 受け取る本体キー（`left`、`right`、`up`、`down`、`enter`、`space`、`backspace`、`tab`、`pageup`、`pagedown`、`home`、`end`、`delete`、`minus`、`equal`、`comma`、`dot`、`a`〜`z`、`0`〜`9`）。アプリのセルが画面にあるあいだ、このキーは PC に送らず、スクリプトに渡す |

- **スクリプトの言語**：1 行目の `#!`（絶対パスか `#!/usr/bin/env <名前>`）で選ぶ。なければ `/bin/sh`（dash）。実行の許可（chmod +x）は要らない。
- **Brain に入っている言語**（2026-10-12 に調べた）：

  | 言語 | 版 | 起動の時間（Brain） | 使えるメモリの目安 |
  | --- | --- | --- | --- |
  | sh（dash） | 0.5.12 | 0.03 秒 | 8 MB でも動く |
  | bash | 5.2.37 | 0.05 秒 | |
  | busybox sh | 1.37.0 | 0.03 秒 | |
  | awk（mawk） | 1.3.4 | 0.03 秒 | |
  | perl | 5.40.1 | 0.06 秒 | 16 MB で動く |
  | lua | 5.1.5 | 0.04 秒（初回 3 秒） | |
  | python3 | 3.13.5 | **0.85 秒**（`-S` で 0.53 秒、`import json` で 1.5 秒） | 16 MB で動く |

  Brain の CPU は遅い（ARM926、454 MHz、浮動小数点の計算はソフトウェア）。よく動かすもの（`every` が短い、`stream`）は sh と awk がおすすめ。Python は 1 回ごとに 1 秒ほどかかる。

## 出力の決まり

1 行ずつ。`@` で始まらない行は、そのまま文字の行（セルの幅で折り返す）。`@` で始まる行は指示：

| 行 | 描くもの |
| --- | --- |
| `ふつうの文字` | 文字の行 |
| `@title 見出し` | セルの上の見出し（設定の `label` があれば `label`） |
| `@big 42%` | 大きな文字の行（幅に入る大きさで、中央に） |
| `@color red` | これより後の行の色。`red`、`orange`、`yellow`、`green`、`blue`、`gray`、`white` か `#rrggbb`。`@color` だけで元に戻す |
| `@bar 75 [100] [文字]` | 進み具合の帯（値、最大（既定 100）、帯に重ねる文字）。色は `@color`（なければ緑） |
| `@row 左 \| 右` | 左と右に分けた行（右は右に寄せる。一覧の表に） |
| `@item <id> 文字` | タップできる行。タップすると `item <id>` |
| `@sep` | 区切りの線 |
| `@button <id> 文字` | 下に並ぶボタン（6 つまで）。押すと `button <id>` |
| `@input <id> 文字 [\| 欄の見出し]` | 文字を入れるボタン。押すと画面の下に入力欄が開き（日本語入力も使える）、決定すると `input <id>` |
| `@clear` | ここから新しい画面（stream で、1 画面ずつ描くとき） |
| `@@…` | `@` で始まる文字の行（`@` を 1 つ取る） |

- 知らない `@…` は、そのまま黄色の文字で出す（書き間違いに気づけるように）。
- 入りきらない行は描かず、右下に `…` を出す（stream は最後の行を残す）。1 行は 1024 バイト、1 画面は 200 行まで。
- 制御文字（エスケープシーケンスなど）は捨てる。

## スクリプトに渡すもの

| とき | interval（引数） | stream（標準入力の 1 行） |
| --- | --- | --- |
| 時間で動かす、画面に出たとき | なし | （動かし始める） |
| ボタン以外のところをタップ | `tap` | `tap` |
| ボタン | `button <id>` | `button <id>` |
| タップできる行 | `item <id>` | `item <id>` |
| 本体キー（`keys`） | `key <名前>` | `key <名前>` |
| 文字（`@input`） | `input <id>`、入れた文字は標準入力（1 行） | `input <id> <入れた文字>`（改行は空白にする） |

環境変数：`BRAIN_APP`（名前）、`BRAIN_APP_DIR`（アプリのフォルダ。読むだけ）、`BRAIN_APP_DATA`（アプリが書ける場所。今いるフォルダもここ）、`BRAIN_EVENT`（引数をつないだもの）、`HOME`、`USER`、`LANG=C.UTF-8`、`PATH=/usr/local/bin:/usr/bin:/bin`。

### interval の例（sh）

```sh
#!/bin/sh
n=$(cat "$BRAIN_APP_DATA/count" 2>/dev/null || echo 0)
case "$1" in
button) [ "$2" = up ] && n=$((n + 1)); [ "$2" = reset ] && n=0 ;;
input) read -r v; n=$v ;;
esac
echo "$n" > "$BRAIN_APP_DATA/count"
echo "@big $n"
echo "@button up ＋1"
echo "@button reset 0 に"
echo "@input set 数を入れる"
```

### stream の例（bash）

```bash
#!/bin/bash
# 2 秒ごとに描き直し、そのあいだに来たボタンを読む。標準入力が閉じたら終わる
while :; do
  echo "@clear"
  echo "@big $(date +%H:%M:%S)"
  echo "@button x 押す"
  if read -r -t 2 ev arg; then
    echo "押した：$ev $arg" > "$BRAIN_APP_DATA/last"
  elif [ $? -le 128 ]; then
    exit 0
  fi
done
```

`read -t` は bash にある（dash にはない）。標準入力が閉じたとき（デーモンが止めたとき）に終わるようにする。

## いつ動かすか

- **画面にあるあいだだけ**動かす。アプリのセルを描いてから 5 秒のあいだ「画面にある」とみなす（ほかのアプリを開く、ふたを閉じる、写真を出すなどで止まる）。
- interval：画面に出たとき、`every` ごと、タップやボタンのとき。動いているあいだに来たタップやボタンは、終わってから順に（8 つまで）。
- stream：画面に出たら動かし、動き続ける。画面から消えて 30 秒たったら止める。終わったり落ちたりしたら、5 秒、10 秒…（5 分まで）待ってから動かし直す（1 分以上動いていれば 5 秒に戻す）。
- 同時に動かすのは、すべてのアプリで **3 つまで**（設定の `apps.max_running` で変える。stream は動いているあいだ 1 つ使う）。空くのを待つあいだは「待っています」と出す。

## 安全と上限

- スクリプトは **root ではなく、Brain の user**（`terminal.brain_user`、既定 `user`）として動かす。書けるのは `BRAIN_APP_DATA`（`/var/lib/lefthand/apps-data/<名前>/`）と user が書ける場所だけ。アプリのフォルダ（root のもの）は書き換えられない。
- **既定と上限**：アプリごとの既定は、メモリ 32 MB、時間 10 秒、出力 32 KiB。`app.yaml` の `memory_mb`、`timeout`、`output_kb` で変えられるが、Brain の設定の `apps`（[docs/config.md](config.md) の「自分で足すアプリの上限（apps）」。既定はメモリ 64 MB、時間 60 秒、出力 256 KiB）を超える値は断る（`brain-deck app install` がエラーになる。置いたあとで上限を下げたら、そのアプリは「使えません」になる）。
- 上限（`/bin/sh` の `ulimit` で掛ける）：仮想メモリ（`memory_mb`）、CPU の時間（interval は `timeout` + 1 秒）、書けるファイルの大きさ（16 MiB）、開けるファイル（64）、user のプロセスの数（今の数 + 32）。プロセスのグループごと優先度を下げ（nice 15）、入力と描画を先にする。
- interval の時間（`timeout`）を超えたら、プロセスのグループごと止める。出力は 1 回 `output_kb`（既定 32 KiB）まで、stream は 10 秒にその 2 倍まで。超えたら止めて、セルの下に赤で「止めました：…」と出す。終了コードが 0 でないときは、標準エラーの最後の行を黄色で出す。
- スクリプトが止まったり遅かったりしても、**キー入力と画面の描画は待たない**（スクリプトは別に動かし、描くのは最後の出力の写し）。
- 名前とファイルは英数字と `_ . -` だけ。フォルダの中は**ふつうのファイルだけ**（サブフォルダ、リンク、ほかの場所を指す名前は、送るときも Brain で読むときも断る）。1 つのアプリは 32 ファイル、1 ファイル 256 KiB、合計 1 MiB まで。アプリは 32 個まで。
- アプリは自分で書いたものだけを入れる。スクリプトは user の権限で、その user が読めるもの（`~/` など）は読める。

## バックアップ

`brain-deck backup` に、アプリ（`apps/`）とアプリのデータ（`apps-data/`）も入る。復元すると、アプリもバックアップのとおりになる（データの持ち主は、次に動かすときに user に直す）。

## 画面の絵を作る（開発）

```sh
sh contrib/apps/dice/main.sh button two > /tmp/dice.out     # PC で出力を作る
lefthand -render-png dice.png -render-layer dice -render-app dice=/tmp/dice.out config/apps-example.yaml
```
