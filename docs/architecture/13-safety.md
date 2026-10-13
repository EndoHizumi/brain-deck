# 13. 安全のための仕組み

[目次に戻る](../architecture.md)

## 信頼の境界：USB でつないだ PC を信頼する

まず前提をはっきりさせる。**シリアルのプロトコルには認証がない**。Brain の設定用のシリアルを開けるもの（USB でつないだ PC の上で、そのポートを開く権限のあるユーザー）は、次のことがすべてできる。

- 設定を書き換える、Brain のデータを読む・消す、見られる場所のファイルを取り出す
- `brain-deck update` で、root で動くデーモンを好きな実行ファイルに入れ替える（更新のファイルの確かめは「壊れていないか」「この設定で動くか」で、だれが作ったかの署名はない）
- 自分で足すアプリ（Brain の user で動くスクリプト）を置く

つまり「USB ケーブルでつないだ PC と、その PC の dialout グループのユーザー」が、Brain の root と同じ力を持つ。brain-deck は「自分の机の上の、自分の PC につなぐ道具」として作られている。
知らない PC につなぐときは、その PC が Brain を書き換えられる、と考えること。ほかに、SSH（NCM）と、1 つ目のシリアルの getty（パスワードが要る）がある。

この前提の中で、「誤操作や壊れた入力で Brain を壊さない」「Brain の画面とキーボードだけで、root の力を出さない」ための仕組みを、以下にまとめる。

## 何を root で、何を user で動かすか

| 動くもの | 権限 | なぜ |
| --- | --- | --- |
| lefthand（デーモン） | root | 入力デバイスの専有（grab）、`/dev/hidg0`、フレームバッファと VT、`/dev/mem`（RTC、電源のレジスタ）、システムの時刻、バックライト、configfs、`systemctl` を使うため |
| lefthand -update-apply（更新の係） | root（`systemd-run` の一時的なユニット） | サービスを止める、実行ファイルを入れ替える |
| gadget-setup.sh | root | configfs と UDC、`ip` |
| `/bin/login`（端末モードの Brain） | root で起動し、パスワードが合えば `terminal.brain_user` のシェル | login がそうなっている。`login -f`（パスワードなし）は使わない |
| `terminal.command`（ssh など） | `terminal.user`（root は設定の検証で断る） | |
| uim-fep と line.sh（日本語入力） | `ime.user`（既定 `terminal.brain_user`、なければ `user`。root は断る） | |
| 自分で足すアプリ | `terminal.brain_user`（既定 `user`。root なら動かさない）。補助グループはなし | |
| 1 つ目のシリアルの getty | root（systemd の serial-getty のまま）。ログインにはパスワード | |

- user で動かすものは、`setuid` してから exec する（termpty.go の `startUserPTY`、appsrun.go の `AppService.command`）。
- 画面とキーボードだけで root のシェルを開く道はない（端末モードの Brain は `login` のパスワード、アプリは user）。
- 設定のアプリの「再起動」「電源を切る」は、2 秒押し続けたときだけ働く。

## 見られる場所の制限（ファイル）

デーモンは root で動くので、ファイルエクスプローラ（`widget: files`）と `brain-deck files` で触れる場所を決めてある（files.go）。

| 決まり | 中身 |
| --- | --- |
| 見られる場所 | 設定の `files.roots` だけ（既定は `/home/user`、写真、手書きメモ、読み物） |
| 場所に書けない | `/`、`/proc`、`/sys`、`/dev`、`/run`、`/boot`、`/root`、`/etc/shadow`、`/etc/gshadow`、`/etc/sudoers`（と `.d`）、`/etc/ssh` |
| 中にあっても出さない | 上の隠すパスと、名前が `.ssh`、`.gnupg` のフォルダ |
| 消せる、足せる場所（`delete: true`） | `/home/`、`/media/`、`/mnt/`、`/tmp/` の下と、データの写真、メモ、読み物だけ。`/etc` などを消せる設定は受け付けない |
| パス | 絶対パスで、`..` やシンボリックリンクを通るものは断る（リンクはたどらない） |
| データのフォルダ | 写真、メモ、読み物は、それぞれの係を通して消す（索引のファイルは消さない）。PC から足せるのは、ふつうのフォルダだけ |
| 大きさ | 一覧は 2000 個、送るのは 96 KiB ずつ、足すのは 64 MiB まで |

## 上限

壊れた入力や大きすぎる入力で、メモリや SD カードをあふれさせないための上限（主なもの）。

| もの | 上限 | ファイル |
| --- | --- | --- |
| シリアルの 1 行 | 256 KiB。改行なしで 2 秒止まったら捨てる | control.go |
| 通知の待ち行列 | 64 個（あふれたら捨てる） | control.go |
| 背景画像 | 1 枚 768,008 バイト（800×480）、合計 16 MiB | image.go |
| 写真 | 既定 10 枚（`photo.max`、100 まで） | photo.go |
| 読み物 | 1 つ 8 MiB（gzip）、戻して 32 MiB、合計 32 MiB | readdoc.go |
| 天気のアイコン | 合計 2 MiB（超えたら、今の設定で使わない大きさから消す） | weathericon.go |
| SD カードの空き | 画像、写真、読み物、ファイルを置いたあとも 64 MiB 残す（ファイルシステムの 10% のほうが小さければそちら）。更新は 16 MiB | image.go、update.go |
| テキスト | 200 文字、8 行、64 個、有効期限 30 日まで | text.go |
| Todo | 1 項目 200 文字 | todo.go |
| 予定 | カレンダー 16 個、予定 1000 個 | calendar.go |
| グラフ | id 32 個、id ごとに 120 個 | graph.go |
| 手書きメモ | 200 個、50 ページ、3000 本、1 本 2000 点 | memo.go |
| 更新のファイル | 24 MiB、展開した lefthand 32 MiB | update.go |
| バックアップ | 受け取り 192 MiB、戻した中身 512 MiB、20,000 ファイル | backup.go |
| 自分で足すアプリ | 32 個、1 つ 32 ファイル、1 ファイル 256 KiB、合計 1 MiB。動かすときは仮想メモリ、CPU の時間、ファイルの大きさ 16 MiB、ファイル 64 個、プロセスの数、時間、出力、同時に 3 つ | apps.go、appsrun.go |
| ログ（`get_logs`） | 2000 行、200 KiB | update.go |

## 失敗したときに戻す仕組み

| 場面 | 仕組み | ファイル |
| --- | --- | --- |
| 設定の保存 | 反映に失敗したら、ファイルと動作を前の設定に戻す。前の版は `.prev` | apply.go |
| 再起動が要る設定（`put_config_text` の restart） | 新しいデーモンが起動しなければ `.prev` に戻して起動し直す | updatehelper.go の `restoreConfig` |
| 更新 | 起動しない、版が違う、PC がつなぎ直せない、すぐ落ちる、のどれでも前の版（lefthand、gadget-setup.sh、読み物の文字の組）に戻す。`--rollback` でも戻せる。中身が同じファイルは入れ替えず、`.prev` を上書きしない | updatehelper.go |
| 復元 | 前の状態を `before-restore.tar.gz` に残す。起動しなければ元に戻す。`--undo` | backup.go、updatehelper.go |
| タッチのキャリブレーション | 保存の前に確かめの画面。前のものは `.prev`。`--reset` で設定の値に戻る | calibui.go、touchcal.go |
| USB の構成 | HID や ACM を足せなくても、NCM だけは残して SSH を使えるようにする。HID の形を変えられなくても、リンクは必ず元に戻す | gadget-setup.sh |
| 異常終了 | `ExecStopPost` の `-restore-console` が、VT、明るさ、端末モードを戻す。`Restart=on-failure` | systemd/lefthand.service、vt.go、backlight.go、termmode.go |
| ファイルの書き込み | 一時ファイルと rename。途中で電源が切れても、前か後のどちらか | store.go、apply.go |
| 電池切れ | 電圧で知らせ、データを保存してから電源を切る | batteryguard.go |

## 入力を守る仕組み

| 仕組み | 何を防ぐか |
| --- | --- |
| 入力は通信、描画、書き込み、スクリプトを待たない | 何かが詰まってもキーボードが止まらない |
| 描画の panic を拾う（`Display.loop`）、設定の反映の panic を拾う（`reloader`） | 画面や反映の不具合で入力が止まらない |
| 押したときの割り当てで離す。終了、反映、PC から外れた、USB の付け直しの前に全部離す。0.2 秒ごとの送り直し | キーの押しっぱなし |
| `/dev/hidg0` を非ブロッキングで開く | PC が読まないときに止まらない |
| 暗いときのタッチは明るくするだけ。タブレットの形では本体キーを捨てる。ふたを閉じたらタッチを捨てる | 誤操作 |
| 学習モードの止める時間は 30 秒で、GUI が切れたら戻る | 入力が PC に届かないまま残る |
| `checkWayBack`（戻れないレイヤーの設定を断る） | 画面とキーだけで抜けられなくなる |
| コンソール用のシリアルには何も書かない | ログイン画面にユーザー名が入る（ログインの失敗） |

## 気をつけること（作りの上で）

- **ロックの順**：エンジンのロックを持ったまま、ウィジェットのタップの処理、通知、`onView`、`onStatus`、`onRotate` を呼ぶ。そこからエンジンや設定の置き場所のロックを取ると止まる。K1 と K5 で実機で起きた（REPORT.md、コミット d18b7d3）。新しい処理を足すときは、ロックを持ったまま呼ばれるかを確かめる。
- **`/dev/mem`**：RTC と電源のレジスタを読み、`clock.rtc: write` のときは RTC の秒を書く。書く範囲は RTC の `SECONDS` だけ（PERSISTENT のレジスタには触らない）。電源のレジスタは読むだけ。
