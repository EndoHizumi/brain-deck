# 14. ソースの地図

[目次に戻る](../architecture.md)

リポジトリの直下の `*.go` はすべて 1 つのプログラム（`package main`、デーモン lefthand）。`_test.go` はテストで、`*_hw_test.go` は Brain の上で `LEFTHAND_HW_TEST=1` を付けて動かす実機のテスト（ふだんの `go test` では飛ばす）。

## 「〇〇を変えたいとき」の早見表

| 変えたいこと | 見るファイル |
| --- | --- |
| 設定の項目を足す | config.go（`Config`、`ActionSpec`、`actionFields`、`compileKeymap`）→ 使う係のファイル → main.go の `cstore.apply`（反映）→ docs/config.md → gui/src/types.ts、gui/src/app.ts（GUI の欄）→ gui/src/demo.ts（簡易デーモンの検証） |
| 割り当ての種類（`key`、`layer_*` のような）を足す | config.go（`ActKind`、`actNames`、`ActionSpec.count`、組み立て）、layer.go（`Engine.press`）、main.go（`cellView` の下の文字）、gui/src/model.ts |
| ウィジェットを足す | widget.go（`widgetKinds`、`compileWidget`、`widgetKey`、`widgetNext`、描画の振り分け）、新しい `xxxwidget.go`、todowidget.go（`ownsTouch`、`touchDown`）か gadgetwidget.go（`tapWidget`）、main.go（`WidgetEnv` に要るデータ、`-render-*`）、gui/src/preview.ts と fixture、docs/config.md |
| PC から送るデータを足す | 係のファイル（text.go や graph.go を手本に）、control.go（`request` の項目、`handle` の `case`、`hello` の `commands`）、docs/protocol.md、cmd/brain-deck/ のサブコマンド |
| シリアルのコマンドを足す | control.go（`handle`、`hello` の一覧）、docs/protocol.md、gui/src/protocol.ts か cmd/brain-deck/client.go |
| 分割して受け取るファイルの種類を足す | transfer.go（`Receiver`）、係の `UploadKind`、main.go の `ctl.uploads.Register` |
| 入力の流れ（キー、タッチ） | main.go（`runKeyboard`、`touchProc`）、layer.go（`Engine.PressKey`、`PressTouchAt`、`press`、`Release`） |
| タッチの座標、キャリブレーション | rotate.go（`touchMap`）、touchcal.go、calibui.go、main.go（`cellOf`、`touchCell`） |
| レイヤーの透過のきまり | layer.go（`Keymap.view`、`lookupKey`）、gui/src/model.ts（同じきまり） |
| 画面の描き方、色、配置 | display.go（`drawCell`、`drawAll`、色）、fb.go（`Canvas`）、gui/src/preview.ts（同じ描き方。fixture を作り直す） |
| 描き直しのきっかけ | display.go（`Display.redraw`、`redrawWidgets`）、widget.go（`widgetNext`）、main.go（`SetOnChange(d.Poke)`） |
| 状態の帯 | statusbar.go、gui/src/statusbar.ts |
| ホーム画面 | home.go、icons.go（アイコン。gui/src/icons.json も）、gui/src/home.ts |
| PC に送るキーの名前 | main.go（`hidUsage`、`modBits`、`prettyKey`）、gui/src/keys.ts |
| HID のレポートの形 | hid.go と gadget-setup.sh（同じディスクリプタ。両方を変える）、main.go（`State.send`）、mouse.go、media.go、pen.go |
| USB の構成（ファンクション、名前、電流） | gadget-setup.sh、usbmode.go、update.go（`-check-gadget`） |
| 端末モード | termmode.go（出入り）、term.go（制御シーケンス）、termdraw.go（画面）、termkeys.go（キー）、termpty.go（PTY） |
| 日本語入力 | ime.go、prompt.go、contrib/brain/lefthand-ime.sh |
| 自分で足すアプリ | apps.go（置き場所、app.yaml）、appsrun.go（動かす係、上限）、appwidget.go（出力の決まり、描画）、docs/apps.md、cmd/brain-deck/apps.go |
| 自動減光、写真 | backlight.go（`Dimmer`）、photo.go（`PhotoFrame`） |
| 電池 | battery.go、batteryguard.go、power.go |
| 時刻、RTC | timesync.go、rtc.go |
| PC とのつながり、外したとき | link.go、offline.go |
| ふた | lid.go |
| 上書きの値 | overrides.go（`overrideKeys`）、settingswidget.go、cmd/brain-deck/overrides.go |
| PC ごとの設定 | profile.go、apply.go（`Reprofile`）、cmd/brain-deck/profile.go、sync.go |
| 設定の保存と反映 | apply.go（`configStore`）、main.go（`cstore` の `apply` と `light`） |
| 更新、再起動 | update.go、updatehelper.go、tools/mkupdate、cmd/brain-deck/admin.go |
| バックアップと復元 | backup.go（`backupItems`）、updatehelper.go（`restore`）、cmd/brain-deck/backup.go |
| 起動の順、起動中の画面 | main.go（`main`）、startup.go、splash.go、systemd/ |
| PC 側のポートの探し方、排他 | cmd/brain-deck/ports.go、os_linux.go、os_darwin.go、serial_unix.go、gui/src/ports.ts |
| 同期 | cmd/brain-deck/sync.go、sync.go、contrib/udev/73-brain-sync.rules |
| ヘルプの文 | help/ja.md（デーモンに埋め込む） |

## デーモン（リポジトリの直下）

### 骨組み

| ファイル | 役目 |
| --- | --- |
| main.go | 起動の順（`main`）、コマンドラインのオプション（`-render-*` など）、キーとタッチの読み取り（`runKeyboard`、`touchProc`）、HID の出力（`State`、`HIDWriter`）、キー名の表、セルの見た目の組み立て（`cellView`、`buildLayout`）、四隅のキャリブレーション（`-calibrate`） |
| config.go | 設定の構造（`Config`、`ActionSpec` など）、読み込み、旧形式の変換、割り当ての組み立てと検証（`compileKeymap`、`checkWayBack`）、誤りの場所（`Problems`） |
| layer.go | レイヤーの重なりと透過（`Keymap.view`、`lookupKey`）、入力の処理の中心（`Engine`） |
| apply.go | 設定の検証、保存、再起動なしの反映、失敗したときの巻き戻し（`configStore`）、原子的な書き込み（`writeFileAtomic`） |
| control.go | シリアルのプロトコル（`Controller`、`Serve`、`handle`）、入力の通知と学習モード（`Monitor`） |
| transfer.go | 分割して送られるファイルの受け取り（`Receiver`、`Uploads`） |
| store.go | `/var/lib/lefthand` の JSON の読み書き（`Store`） |
| version.go | 版の文字列（`-ldflags "-X main.version=…"`、なければ git のリビジョン） |
| startup.go | 起動の段階ごとの時間（`bootTrace`） |

### USB と PC への出力

| ファイル | 役目 |
| --- | --- |
| hid.go | HID のディスクリプタ（キーボードだけ、キーボード＋マウス＋メディアキー＋ペン）と、configfs から形を読む（`detectHID`） |
| mouse.go | マウスのレポートと、`mouse:` の割り当て |
| media.go | メディアキー（Consumer Control）と、`media:` の割り当て |
| pen.go | ペンタブレット（`widget: pentablet`）のレポート、PC の画面への当てはめ、筆圧 |
| usbmode.go | USB の形の切り替え（`USBMode`）、端末モードの構成の名前、gadget-setup.sh の実行 |
| wakeup.go | スリープ中の PC を起こす（`Waker`） |
| link.go | PC とのつながり（UDC の state と 5V。`LinkMonitor`） |
| offline.go | PC から外れたとき（休みにする、ホーム画面に切り替える、「PCなし」） |
| keymap_pwsh2.go | 設定 GUI に渡す、本体キーの配置と制約（`get_keymap`） |

### 画面

| ファイル | 役目 |
| --- | --- |
| display.go | セルの配置と描画（`drawCell`、`drawAll`）、描画の goroutine（`Display`）、部分的な描き直し |
| fb.go | フレームバッファ（`Framebuffer`）、裏画面（`Canvas`。回転、塗り、文字、画像） |
| vt.go | 専用 VT の確保と、元の VT への復帰（`-restore-console`） |
| splash.go | 起動中の画面（フェーズ K4） |
| font.go | k8x12 フォントの埋め込みと字形 |
| readfont.go | 読み物の明朝体（gzip で埋め込み、使うときに戻す）、縦書きの文字の向き |
| statusbar.go | 状態の帯 |
| prompt.go | 画面の下に重ねる知らせと、文字を入れる欄（`Prompt`、`Overlay`） |
| rotate.go | 画面の回転、向きの保存（`RotateStore`）、タッチの座標の変換（`touchMap`）、縦向きの格子 |
| image.go | 背景画像の置き場所、受け取り、キャッシュ、先読み（`ImageStore`） |
| icons.go | ホーム画面の組み込みのアイコン（16×16 のドット絵） |
| home.go | ホーム画面（`compileHome`、配置、ページ、タップ、描画） |

### 入力のまわり

| ファイル | 役目 |
| --- | --- |
| touchcal.go | タッチのキャリブレーションの保存と、生の座標から画面のドットへの変換 |
| calibui.go | キャリブレーションの画面（十字を押す、確かめ、保存） |
| trackpad.go | トラックパッドの判定（タップ、ドラッグ、スクロール、ブレ対策）と描画 |
| stroke.go | スタイラスの線の読み取り（跳ね、平均、つなぎ）。ペンとメモで共通 |
| record.go | タッチの記録（`-record-touch`）と再生（`-replay-touch`） |
| penreplay.go | ペンの記録の再生（`-replay-pen`） |
| lid.go | ふたとタブレットの形 |

### ウィジェット

| ファイル | 役目 |
| --- | --- |
| widget.go | ウィジェットの共通の部分（種類の一覧、`WidgetDef`、`WidgetEnv`、`compileWidget`、`widgetKey`、`widgetNext`）、時計とテキストの描画 |
| text.go | テキストのタイルと QR コードの中身（`TextService`。text.json、qr.json） |
| qr.go | QR コードの符号化（Project Nayuki の手順を絞ったもの） |
| todo.go、todowidget.go | Todo の項目と保存、セルの配置と描画とページ送り。todowidget.go にはタップの共通の部分（`WidgetRT`、`touchDown`、`ownsTouch`）もある |
| calendar.go、calwidget.go | 予定（set_calendar）と保存、カレンダーのセル |
| monthwidget.go | 月の表示 |
| weather.go、weatherwidget.go、weathericon.go | 天気予報の保存、天気のセル、天気のアイコン（.lha） |
| graph.go、graphwidget.go | グラフの値と保存（間引き）、グラフのセル |
| gadgets.go、gadgetwidget.go | ストップウォッチ、ポモドーロ、カウンター、在席ランプ、プレゼン用タイマーの状態（widgets.json）、電池などのセルの描画とタップ |
| presenter.go | プレゼン用タイマー |
| calc.go、calcwidget.go | 電卓の計算（有理数）と、セル、ボタン、本体キー |
| battery.go | 電池の状態（LRADC）と残りの見積もり、`widget: battery` |
| memo.go | 手書きメモ |
| readdoc.go、readlayout.go、readerwidget.go | 読み物の形と置き場所としおり、ページの組み方（禁則、ルビ、縦書き）、読み物のアプリ |
| files.go、filewidget.go | 見られる場所とファイルの操作、ファイルエクスプローラ |
| settingswidget.go | 設定のアプリ（設定、情報、電源のタブ） |
| helpwidget.go | ヘルプ（help/ja.md を読み物の描き方で） |
| apps.go、appsrun.go、appwidget.go | 自分で足すアプリ（置き場所、動かす係、出力の決まり） |

### Brain の状態と保守

| ファイル | 役目 |
| --- | --- |
| backlight.go | 自動減光（`Dimmer`） |
| photo.go | フォトフレーム（`PhotoStore`、`PhotoFrame`） |
| batteryguard.go | 電池が少ないときの知らせと電源断、電池の記録 |
| power.go | i.MX28 の電源のレジスタを読む（充電しているか） |
| timesync.go | 時刻合わせ（`TimeService`） |
| rtc.go | 内蔵 RTC のレジスタ（`/dev/mem`） |
| sync.go | 同期の状態（set_sync、sync.json） |
| overrides.go | 上書きの値 |
| profile.go | PC ごとの設定 |
| ime.go | 日本語の入力（uim-fep） |
| term.go、termdraw.go、termkeys.go、termmode.go、termpty.go | 端末モード（[9.](09-terminal-ime-apps.md)） |
| update.go、updatehelper.go | 更新、再起動、電源、ログ、入れ替えの係 |
| backup.go | バックアップと復元 |

### 内部のパッケージ

| ディレクトリ | 役目 | 使うもの |
| --- | --- | --- |
| internal/imgconv | 画像を RGB565 にする変換（整数だけ。gui/src/image.ts と画素単位で同じ） | lefthand（写真の縮小、ファイルの画像）、brain-deck |
| internal/ink | 手書きの線の描き方（整数だけ） | lefthand（メモ）、brain-deck（`memo get`） |

## brain-deck（cmd/brain-deck/）

| ファイル | 役目 |
| --- | --- |
| main.go | サブコマンドの振り分け、オプション、終了コード、`usage`、text・qr・presence・status・usb-mode・terminal など小さなもの |
| client.go | lefthand との通信（`Client`、`connect`、`Call`） |
| serial_unix.go | ポートを開く（TIOCEXCL、生のモード）、brain-deck どうしのロック |
| ports.go、os_linux.go、os_darwin.go | 設定用のポートの探し方、ほかに開いているプロセス |
| admin.go | update、restart、reboot、poweroff、logs、config（つなぎ直しながら結果を待つ `session`） |
| backup.go | backup、restore、同期のときの自動のバックアップ |
| sync.go | sync |
| profile.go | profile、PC の名前 |
| overrides.go | overrides |
| todo.go | todo |
| calendar.go、ics.go | calendar（ICS の取得、繰り返しとタイムゾーンの展開） |
| weather.go、svg.go、svgfilter.go | weather（API、気象庁のアイコンの SVG を描く） |
| graph.go | graph |
| images.go | images |
| photo.go、decode.go | photo add（画像の読み込みと変換） |
| memo.go | memo（PNG に描く） |
| read.go、aozora.go、readweb.go | read（青空文庫、EPUB、Web の記事、RSS） |
| files.go | files |
| apps.go | app |
| calibrate.go | calibrate |
| batterylog.go | battery log |
| exec.go | exec（ショートカットからコマンドを動かし、結果をタイルに出す） |

## 設定 GUI（gui/src/）

| ファイル | 役目 |
| --- | --- |
| main.ts | 入り口。`?demo` なら簡易デーモンにつなぐ |
| app.ts | 画面全体（`App`）。設定、Todo、写真、読み物、ファイル、アプリ、コンソールのタブ。接続、保存、背景画像の切り抜き |
| types.ts | 設定の型（docs/config.md と同じ構造） |
| model.ts | 設定の操作。透過の解決（layer.go と同じきまり）、割り当ての名前 |
| protocol.ts | プロトコル（行の組み立て、id と返事の対応、通知） |
| serial.ts | WebSerial のトランスポート |
| ports.ts | 2 つのシリアルのどちらを使うか（役割を覚える） |
| console.ts、terminal.ts | コンソールのタブ（xterm.js） |
| preview.ts | Brain の画面のプレビュー（display.go と同じ描き方） |
| font.ts | k8x12 の字形 |
| clock.ts、textwidget.ts、todowidget.ts、calwidget.ts、monthwidget.ts、weatherwidget.ts、graphwidget.ts、gadgetwidget.ts、presenter.ts、calc.ts、penwidget.ts、readerwidget.ts、home.ts、statusbar.ts、qr.ts | ウィジェットごとの配置と中身（Go の同じ名前のファイルと同じ結果） |
| image.ts | 画像の変換（internal/imgconv と同じ） |
| keys.ts | PC に送るキーの名前 |
| diff.ts | 保存の前に見せる差分 |
| yamlio.ts | 設定ファイルの読み込みと書き出し |
| dom.ts | DOM を組み立てる補助 |
| demo.ts | 簡易デーモンと WebSerial のモック（`?demo` とテスト） |
| icons.json、keymap-pwsh2.json | 組み込みのアイコンと本体キーの配置（Go と同じ表） |

テストは gui/test/（vitest）。gui/test/fixtures/ の PNG は `lefthand -render-png` で作ったもので、プレビューと画素単位で比べる。

## スクリプト、ユニット、そのほか

| ファイル | 役目 |
| --- | --- |
| gadget-setup.sh | USB ガジェットを作る（[2.](02-usb.md)）。Brain の `/usr/local/sbin/lefthand-gadget-setup` |
| install.sh | Brain の上か、SD カードに直接（`--root`）入れる |
| console-setup.sh | 1 つ目のシリアルの getty を有効にする・戻す |
| systemd/lefthand.service | デーモンのユニット |
| systemd/ethernet_gadget.service.d/lefthand.conf | Brainux のガジェットの処理を gadget-setup.sh に置き換える drop-in |
| systemd/serial-getty@ttyGS0.service.d/lefthand.conf | 1 つ目のシリアルの getty の drop-in |
| contrib/udev/70〜73 | PC 用の udev の規則（ModemManager、端末モード、スリープ解除、同期） |
| contrib/systemd/ | PC 用のユニット（端末モードの getty、同期、予定・天気・グラフの定期実行） |
| contrib/profile.d/brain-terminal.sh | PC 用。端末モードでログインしたときに端末の大きさを合わせる |
| contrib/brain/lefthand-ime.sh | Brain 用。Brain のシェルで uim-fep を通す（`/etc/profile.d/`） |
| contrib/apps/ | 自分で足すアプリの例（sdcard、dice、load） |
| contrib/boot/ | 起動を速くする案（boot-tune.sh、boot-measure.sh。適用していない） |
| contrib/graph/linux-stats.sh | PC の CPU などをグラフに送る例 |
| contrib/shortcuts/ | PC の OS のショートカットに割り当てるスクリプトの例 |
| tools/mkupdate | 更新のファイルを作る |
| tools/mkfont、tools/mkreadfont | フォントを埋め込みの形にする |
| tools/mkbg | 例の背景画像を作る |
| tools/fbshot.sh | Brain の画面を PC に PNG で取る |
| tools/record-touch.sh | タッチを動きごとに記録する |
| kernel/brain-deck.config | カーネルの設定の差分（HID と ACM のガジェット） |
| config/current.yaml | 本番の設定の写し |
| config/*-example.yaml | 機能ごとの例（current.yaml などに足したもの） |
| config.yaml | install.sh が最初に置く設定の例（`/etc/lefthand/config.yaml` が既にあれば置かない） |
| config/production.yaml | Brainux Wiki のキーアサインに合わせた古い設定（フェーズ R より前） |
| help/ja.md | ヘルプの文（デーモンに埋め込む） |
| docs/ | config.md（設定の形式）、protocol.md（プロトコル）、apps.md（自分で足すアプリ）、keymap-pwsh2.md（本体キー）、kernel-build.md（カーネル）、この資料 |
| testdata/ | タッチの記録、読み物の例、画像の変換の結果、uim-fep の記録 |
| _orig/ | 最初の版の写し（git には入っていない。作業の名残） |
