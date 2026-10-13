# 7. データ

[目次に戻る](../architecture.md)

Brain の上で lefthand が読み書きするファイル。3 つに分けて考える。

- **設定**：人が決めるもの。PC にも写しがある（リポジトリの config/current.yaml など）。
- **データ**：Brain で作ったもの、Brain にしかないもの。なくすと戻せない。**バックアップに入る**。
- **取り直せるもの**：PC が送ったもの、記録、作業用のもの。消えても PC から送り直せるか、なくても困らない。バックアップに入れない。

「バックアップ」の列は、`brain-deck backup` の tar.gz に入るか（backup.go の `backupItems`）。

## `/etc/lefthand/`（設定）

| ファイル | 中身 | 誰が書くか | 種類 | バックアップ |
| --- | --- | --- | --- | --- |
| `config.yaml` | 設定ファイル（YAML。docs/config.md） | 設定 GUI（`set_config`。コメントは消える）、`brain-deck config put`（`put_config_text`。コメントは残る）、手で編集、復元 | 設定 | 入る（`config/config.yaml`） |
| `config.yaml.prev` | 1 つ前の設定ファイル。最後の保存の前の版だけ | 保存のたびに | 設定（戻すため） | 入らない |
| `gadget.env` | gadget-setup.sh の環境変数（`HID_MOUSE=1`、`USB_WAKEUP`、`GADGET_MAX_POWER`）。なくてもよい | 手で | 設定 | 入らない |
| `boot-tune.state` | contrib/boot/boot-tune.sh が止めたサービス、置いたファイル、mask したものの記録（`revert` で戻すため）。本番では段階 1〜6 を適用している | boot-tune.sh | 記録 | 入らない |

## `/var/lib/lefthand/`（データ。lefthand の `-data-dir`）

### Brain で作るデータ（バックアップに入る）

| ファイル | 中身 | 作るもの |
| --- | --- | --- |
| `todo.json` | Todo の項目（ID、文、完了、更新番号 rev） | todo.go |
| `widgets.json` | ストップウォッチ、ポモドーロ、カウンター、在席ランプ、プレゼン用タイマーの状態 | gadgets.go |
| `calc.json` | 電卓の状態と履歴 | calc.go |
| `display.json` | 画面の向き（動いているあいだに変えたもの）と、そのときの `display.rotate` | rotate.go |
| `overrides.json` | 上書きの値（[4. 割り当ての重なり](04-layers.md)） | overrides.go |
| `touch-cal.json`（と `.prev`） | タッチのキャリブレーション（アフィン変換の係数、押した点）。`.prev` は前のもの | touchcal.go |
| `profile.json` | PC ごとの設定の状態（最後に名乗った PC、手で選んだもの） | profile.go |
| `memo/<id>.json` | 手書きメモ（ページごとの線の点の列） | memo.go |
| `reader/<id>.json.gz`、`reader/index.json`、`reader/reader.json` | 読み物（PC が変換して送ったもの）、題名などの索引、しおりと文字の大きさ | readdoc.go |
| `photos/<id>.565`、`photos/index.json` | フォトフレームの写真（800×480 か 480×800）と索引 | photo.go |
| `images/<id>.565`、`images/index.json` | 背景画像と、元のファイル名などの索引 | image.go |
| `apps/<名前>/` | 自分で足すアプリ（`app.yaml` とスクリプト）。root のもの | apps.go |
| `apps-data/<名前>/` | アプリが書くデータ（`BRAIN_APP_DATA`）。Brain の user のもの | appsrun.go |

日本語入力の学習（Brain の user の `~/.anthy/`。lock-file を除く）もバックアップに入る（`anthy/`）。小さく、取り直せないため（REPORT.md のフェーズ K3）。

読み物、写真、背景画像は PC から送ったものだが、PC に元が残っているとは限らないので、データとして扱う。

### 取り直せるもの（バックアップに入らない）

| ファイル | 中身 | 作るもの | 取り直し方 |
| --- | --- | --- | --- |
| `text.json` | テキストのタイルの中身（有効期限つき） | text.go | PC が `set_text` で送り直す |
| `qr.json` | QR コードの中身 | text.go（同じ仕組み） | `set_qr` |
| `calendar.json` | 予定（展開したもの）と最終更新 | calendar.go | `brain-deck calendar sync` |
| `weather.json` | 天気予報と最終更新 | weather.go | `brain-deck weather sync` |
| `weather-icons/<番号>-<幅>x<高さ>.lha` | 天気のアイコン（セルの大きさごと） | weathericon.go | `weather sync` が、ないものだけを送る |
| `graph.json` | グラフの値（id ごとに 120 個） | graph.go | 送り直す（値の履歴は戻らない） |
| `clock.json` | 時刻を合わせたときの boot_id と時刻、合わせた元、最後に分かっていた時刻（`last_known`）、内蔵 RTC と PC の時刻の差の記録（`rtc_log`） | timesync.go | 時刻を合わせ直す（`rtc_log` の記録は戻らない） |
| `sync.json` | 最後の同期の結果 | sync.go | 次の同期 |
| `battery-log.csv`（と `.1`） | 電池だけで動いているあいだの電圧の記録（512 KiB で `.1` に移す） | batteryguard.go | 記録なので戻らないが、なくても動く |
| `ime/uim.scm`、`ime/inputrc`、`ime/line.sh` | 日本語入力に使う uim の設定と、1 行を読むスクリプト | ime.go | デーモンが起動のたびに書き直す |

### 作業用（一時的なもの）

| ファイル | 中身 | 作るもの |
| --- | --- | --- |
| `update/<stage>/`、`update/plan.json`、`update/state.json`、`update/confirmed` | 更新のファイルを展開したもの、入れ替えの係への指示、係の進み具合（`state.json` は終わっても残る） | update.go、updatehelper.go |
| `backup/` | 作ったバックアップ（読まれなくなって 10 分で消す）、受け取った復元のファイル、復元の前の状態（`before-restore.tar.gz`。`brain-deck restore --undo` で戻す） | backup.go |
| `files-tmp/` | PC から足すファイルの受け取りの途中 | files.go、transfer.go |
| `.upload-<token>.part`（各フォルダ） | 分割して受け取っている途中のファイル | transfer.go |

## `/run/`（電源を切ると消えるもの）

| ファイル | 中身 | 使うもの |
| --- | --- | --- |
| `/run/lefthand-ready` | デーモンが起動を終えたしるし | 更新の係が、新しい版が動いたことを確かめる（update.go の `defaultReadyFile`） |
| `/run/lefthand-vt` | 元の VT（異常終了したときに戻すため） | `lefthand -restore-console`（vt.go） |
| `/run/lefthand-backlight` | 戻す明るさ（暗くしているあいだだけある） | `-restore-console`（backlight.go） |
| `/run/lefthand-terminal` | 端末モードに入ったしるし（Brain の getty を止めたかなど） | `-restore-console` と次の起動（termmode.go） |
| `/run/lefthand-gadget-restart` | gadget-setup.sh が lefthand.service を止めたしるし | gadget-setup.sh |

## Brain のほかの場所

| 場所 | 中身 | 誰が書くか |
| --- | --- | --- |
| `/usr/local/bin/lefthand`（と `.prev`） | デーモン。`.prev` は 1 つ前の版 | install.sh、更新の係 |
| `/usr/local/sbin/lefthand-gadget-setup`（と `.prev`） | gadget-setup.sh | install.sh、更新の係 |
| `/usr/local/share/lefthand/reader-24.lrf.gz`、`reader-32.lrf.gz`（と `.prev`） | 読み物の本文の文字（フェーズ K7 から lefthand に埋め込まない。`-font-dir`）。プログラムの一部なので、バックアップには入れない | install.sh、更新の係 |
| `/etc/systemd/system/lefthand.service`、`ethernet_gadget.service.d/lefthand.conf`、`serial-getty@ttyGS0.service.d/lefthand.conf` | systemd のユニットと drop-in | install.sh、console-setup.sh |
| `/etc/profile.d/lefthand-ime.sh` | Brain のシェルで uim-fep を通す | install.sh |
| `/etc/systemd/system/lefthand.service.d/boot-early.conf`、`ethernet_gadget.service.d/boot-early.conf` | 段階 4：lefthand と USB の構成を basic.target より前に起動する（`DefaultDependencies=no`） | boot-tune.sh |
| `/etc/systemd/system.conf.d/brain-deck-boot.conf`、`/etc/sysctl.d/90-brain-deck-console.conf`、`/etc/systemd/journald.conf.d/brain-deck.conf` | 段階 5 と 6：起動中の表示を減らす、コンソールに出すカーネルのログを減らす、ジャーナルを 32 MB まで | boot-tune.sh |
| `/etc/udev/rules.d/` の 4 つ（triggerhappy、udisks2、e2scrub など） | 段階 5：止めたサービスの udev の規則を `/dev/null` へのリンクで隠す | boot-tune.sh |
| `/etc/systemd/system/lefthand.service.prev`、`.prev2` | 本番で手で入れ替えたユニットの前の版（段階 3 の前と、K6 の前）。リポジトリにはない | 手で（REPORT.md のフェーズ K6） |
| `/home/user/` | ファイルエクスプローラの既定の場所（見る、消す、PC から足す） | ユーザー |

## PC の側

| 場所 | 中身 |
| --- | --- |
| `~/.config/brain-deck/`（`$XDG_CONFIG_HOME`） | `calendars.yaml`（ICS の URL。秘密）、`weather.yaml`、`feeds.yaml`、`sync.yaml`。リポジトリには入れない |
| `~/.local/share/brain-deck/backups/` | 自動のバックアップ（フォルダ 0700、ファイル 0600。既定は 1 日 1 回、7 個） |
| `~/.cache/brain-deck/weather-icons/` | 天気のアイコンの SVG（macOS は `~/Library/Caches/brain-deck/`） |
| `$XDG_RUNTIME_DIR/brain-deck.lock` | brain-deck どうしの順番待ちのロック |

## 書き方の決まり

- **原子的な書き込み**：一時ファイルに書いて `rename` する（store.go、apply.go の `writeFileAtomic`）。途中で電源が切れても壊れない。
- **待たせない保存**：返事や入力の処理は書き込みを待たない。1 つの goroutine がまとめて書く。グラフのように頻繁に変わるものは、5 分に 1 回にまとめる（SD カードの書き換えの回数に限りがあるため）。
- **データが作れなくても動く**：`/var/lib/lefthand` を作れなくても、読み書きがエラーになるだけで、デーモン（キーボード）は動き続ける（store.go）。
- **上限**：背景画像は合計 16 MiB、写真は既定 10 枚、読み物は合計 32 MiB、どれも SD カードの空きを 64 MiB 残す（[13. 安全](13-safety.md)）。

### なぜこうなっているか

- **なぜ予定や天気をバックアップに入れないか**：PC にいつでも元があり、次の同期で送り直せる。バックアップを小さく、速く（実機で 3.8 秒）保つため（backup.go の冒頭、REPORT.md のフェーズ K3）。
- **なぜ `config.yaml.prev` は 1 つだけか**：保存のたびに上書きされる。何度か保存したあとで「確かめる前の版」に戻したいときは、`.prev` ではなく、リポジトリの config/current.yaml から戻す（REPORT.md の設定 GUI の「確認後の後片付け」で、`.prev` が 3 回目の保存の版になっていた）。
