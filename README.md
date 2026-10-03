# lefthand

Sharp Brain PW-SH2（Brainux）を、PC 用の左手デバイスにするデーモン。
本体のキーボードとタッチパネルの入力を、USB HID キーボードとして PC に送る。
タッチパネルの画面には、セルの枠とラベルを描く。

## ビルド

```sh
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -trimpath -o lefthand
go test ./...
```

## 設定（/etc/lefthand/config.yaml）

タッチのセルは、2 通りの書き方ができる。

```yaml
touch:
  cells:
    "0,0": B                                  # 画面にはキー名「B」を表示
    "1,0": { key: LCTRL+Z, label: "取り消し" }  # 「取り消し」を大きく、「Ctrl+Z」を下に小さく表示
```

- **label**：日本語も使える。`"\n"` で改行できる。文字の大きさは、セルに収まるよう自動で決まる。
- **割り当てのないセル**：暗い枠だけを描く。
- **押しているあいだ**：セルを黄色で表示する。

画面の設定は `display` に書く。省略すると、touch があれば `/dev/fb0` に描く。

```yaml
display:
  enabled: true     # false で画面を使わない
  device: /dev/fb0
  vt: 0             # 0 なら tty8 以降の空いている VT を使う
  rotate: 0         # 0, 90, 180, 270
```

## 画面とコンソール

デーモンは起動すると、空いている VT（通常は tty8）に切り替え、その VT を KD_GRAPHICS にして描く。
コンソール（fbcon、ly、getty）は自分の VT が表示されていないあいだ画面に描かないので、取り合いは起きない。
グラフィックモードの VT は自動消灯の対象にならない。カーネルのメッセージも画面に出ない。

- **終了時**：元の VT（通常は ly の tty2）に戻し、専用 VT をテキストモードに戻して解放する。SIGTERM、SIGINT、入力デバイスのエラーのいずれでも同じ。
- **強制終了されたとき**：systemd の `ExecStopPost` で `lefthand -restore-console` を実行し、`/run/lefthand-vt` に残った情報から元の VT に戻す。手動で試していて画面が戻らないときも、このコマンドで戻せる。
- **ほかのプロセスが VT を切り替えたとき**：描画を止めて切り替えを許可し、2 秒後に専用 VT を取り戻す。デーモンの動作中はキーボードを専有しているので、コンソールが見えても操作できないため。

## 確認用のコマンド

```sh
# 実機なしで、画面の見た目を PNG に書き出す
go run . -render-png out.png -render-pressed "0,0" config.yaml

# 実機で描画ループを試す（専用 VT に切り替え、セルを押下・解除して元に戻る）
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go test -c -o lefthand.test
sudo LEFTHAND_HW_TEST=1 timeout 60 ./lefthand.test -test.run HW -test.v
```

## フォント

ラベルの描画には、門真なむ（Num Kadoma）氏の 8×12 ドット日本語ビットマップフォント
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
