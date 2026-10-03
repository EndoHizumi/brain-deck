# Brain 左手デバイス 作業まとめ

2026-10-03

Sharp Brain PW-SH2 は、起動すると USB HID キーボードとして PC に認識され、本体のキーとタッチパネルの入力を設定どおりのキー入力に変換して送る。デプロイ、実機での確認、自動起動の設定まで完了した。

共有用のドキュメント: https://claude.ai/code/artifact/e37cc86b-6e33-4eea-92b1-5f55695dabd4

## 追記：レイヤー機能（2026-10-04）

キーとタッチの割り当てを、レイヤーで切り替えられるようにした。実機で全項目を確かめ、サービスに反映済み。

- **キー配列**：dts、ドライバ、wiki、実機の計測を突き合わせ、[docs/keymap-pwsh2.md](docs/keymap-pwsh2.md) にまとめた。電源・ツール・ホームと「記号」はイベントが出ない。「調べる」と「戻る」はどちらも KEY_ESC。
- **同時押し**：文字切り替え + 文字キー 1 つは、すべて通った。文字切り替え + Q を押したまま W は、W が届かなかった。
- **画面右の帯**：タッチに反応する。ただし x は画面の右端と同じ範囲（3770〜3860）なので、割り当てた区画だけをセルより優先する。
- **設定**：`layers` を追加し、形式を [docs/config.md](docs/config.md) にまとめた。旧形式もそのまま読める。`-check` で検証、`-dump-json` で JSON に変換できる。
- **性能**：レイヤーを切り替えたときの全体の描き直しは約 33 ms。入力側の SetLayout は待たずに返る。
- **反映**：`/usr/local/bin/lefthand` と `/etc/lefthand/config.yaml` を更新した。前の版は `.prev` に残してある。

## 追記：画面へのセルとラベルの表示（2026-10-04）

タッチセルの枠とラベルを画面に描くようにした。押しているセルは黄色になる。
実機のサービスに反映済みで、画面の内容はフレームバッファを読み出して確認した。
タッチ位置と表示のずれは、ユーザーの目視確認がまだ。

| フレームバッファ | 値 |
| --- | --- |
| デバイス | /dev/fb0（braindrmfb、DRM の fbdev エミュレーション） |
| 解像度 | 800x480、回転なし（fbcon も 0） |
| 色 | 16 bpp、RGB565（R 11/5、G 5/6、B 0/5） |
| stride | 1600 バイト |
| 自動消灯 | consoleblank は 0（無効）。バックライトは 7/7 |

- **コンソールとの共存**：tty2 では ly（ログイン画面）が、tty1 では getty が動いている。fbterm はログイン後に .bash_profile から起動するだけで、常駐していない。デーモンは空いている tty8 に切り替え、KD_GRAPHICS と VT_PROCESS を設定して描く。終了時は tty2 に戻す。
- **起動順**：ly は起動時に tty2 へ切り替えるので、lefthand.service を ly.service のあとに起動する。ほかのプロセスに VT を切り替えられたときは、2 秒後に tty8 を取り戻す。
- **強制終了**：ExecStopPost の `lefthand -restore-console` が元の VT に戻す。
- **性能**：起動時の全体描画は約 25〜50 ms、セル 1 つの描き直しは 3〜8 ms。描画は nice 10 の専用スレッドで行い、入力側は状態を書くだけで待たない。

| 追加・変更したファイル | 内容 |
| --- | --- |
| fb.go（新規） | フレームバッファの ioctl と mmap、裏画面、塗りつぶしと文字の描画 |
| vt.go（新規） | 専用 VT の確保、切り替え、終了時と異常終了後の復帰 |
| display.go（新規） | セルの配置、描画 goroutine、VT の release と acquire の処理 |
| font.go、font/（新規） | 埋め込みフォント k8x12 とライセンス |
| tools/mkfont（新規） | BDF を埋め込み用のバイナリに変換する |
| main.go | セルの `label`、`display` 設定、押下中のハイライト、終了時の後始末、`-restore-console` と `-render-png` |
| config.yaml | 各セルにラベルを付け、display を追加 |
| systemd/lefthand.service | `After=ly.service` と `ExecStopPost` を追加 |
| README.md（新規） | 設定の書き方、画面の仕組み、フォントのライセンス |
| display_test.go、display_hw_test.go（新規） | 単体テストと、実機で動かす描画テスト |

Brain 上の元のバイナリと設定は、`/usr/local/bin/lefthand.prev` と `/etc/lefthand/config.yaml.prev` に残してある。

## 動作確認の結果

キーボード、タッチ、起動時の自動設定のすべてが実機で動いた。送信エラーは一度も出ていない。

| 確認項目 | 操作 | 結果 |
| --- | --- | --- |
| キーボード | Q、W、SPACE を押しながら Q | B、E、Shift+B が PC に届いた。押し続けたときのリピートは無視し、PC 側のリピートに任せる |
| タッチ | 四隅と中央を触る | すべて正しいセルに判定された。割り当てのあるセルでは正しいキーが送られた |
| 終了時 | デーモンを止める | 空のレポートを送るので、キーが押しっぱなしにならない |
| 起動時 | Brain を再起動する | ガジェットの作成、固定 IP、デーモンの起動が自動で行われた。dhclient は実行されない |

| 起動時間 | 値 |
| --- | --- |
| ガジェットの作成 | 約 15 秒。以前は dhclient の待ちで 77 秒以上 |
| 電源投入からキー入力を受け付けるまで | 約 68 秒 |

## 構成

![入力の流れと起動時のユニットの順序](REPORT-architecture.png)

起動時は、まず ethernet_gadget.service が USB ガジェットと /dev/hidg0 を作り、その完了後に lefthand.service がデーモンを起動する。PC からは、同じ USB 接続で SSH（NCM）も使える。

## 変更したファイル

Brainux 本体のファイルは変更していない。標準の起動処理は、systemd の drop-in で置き換えている。元に戻すときは、drop-in を消して daemon-reload する。

| ファイル | Brain 上の配置先 | 変更点 |
| --- | --- | --- |
| gadget-setup.sh | /usr/local/sbin/lefthand-gadget-setup | eth ガジェットを Brainux と同じ値で一から作り、HID と ACM を足して、usb0 に固定 IP を付ける。何度実行してもよい。HID を追加できなくても NCM を残すので、SSH は常に使える |
| systemd/ethernet_gadget.service.d/lefthand.conf（新規） | /etc/systemd/system/ethernet_gadget.service.d/ | 標準の enable_ethernet_gadget の代わりに上のスクリプトを実行し、不要な dhclient を省く |
| systemd/lefthand.service | /etc/systemd/system/ | 依存先を ethernet_gadget.service に変更し、enable した |
| systemd/lefthand-gadget.service | 削除 | 役割を drop-in に移したので不要になった |
| install.sh | ― | drop-in の配置と、古いユニットの削除を追加した。lefthand.service の enable はしない |
| main.go | /usr/local/bin/lefthand | キャリブレーションの推奨値を、ssh 経由でも見出しの下に表示するようにした |
| config.yaml | /etc/lefthand/config.yaml | タッチのキャリブレーション値を反映した |

## タッチのキャリブレーション

反映したのは、爪で画面の端ぎりぎりの四隅を押して測った値。中央の測定値も計算上の中心とほぼ一致したので、座標は線形で一定のずれもない。

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
```

| 触った位置 | 生の座標 (x, y) | 判定されたセル |
| --- | --- | --- |
| 左上 | 229, 3834 | 0,0 |
| 右上 | 3990, 3773 | 3,0 |
| 右下 | 3886, 352 | 3,2 |
| 左下 | 224, 416 | 0,2 |
| 中央 | 2191, 2222 | 2,1 |

- **Y 軸の向き**：パネルの Y 軸は上下が逆なので、min_y を max_y より大きくしている。デーモンはこの反転に対応している。
- **測り方**：角を指の腹で押すと、指の中心が端より内側になる。その値を使うと、セルの境目が画面の内側に寄る。爪やペン先で端ぎりぎりを押して測る。
- **ツールの仕様**：`-calibrate` は最後の 4 回のタッチを、左上、右上、右下、左下として扱う。四隅以外を最後に押すと、推奨値がおかしくなる。

## 運用

デーモンが動いているあいだは、Brain のキーボードをデーモンが専有する。Brain 本体で文字を打つときは、デーモンを止める。コマンドはどれも PC から実行する。

1. デーモンを止める：`ssh brain sudo systemctl stop lefthand.service`
2. キーの割り当てを変える：Brain の `/etc/lefthand/config.yaml` を編集し、`ssh brain sudo systemctl restart lefthand.service` で反映する
3. ログを見ながら試す：デーモンを止めてから、`ssh brain 'cd ~/lefthand && sudo timeout 60 ./lefthand -v /etc/lefthand/config.yaml'` を実行する。キーボードを専有するので、必ず時間制限をつける
4. タッチを測り直す：デーモンを止めてから、`ssh brain 'cd ~/lefthand && sudo timeout 60 ./lefthand -calibrate config.yaml'` を実行し、四隅だけを押す
5. 終わったら、`ssh brain sudo systemctl start lefthand.service` でデーモンを再開する

接続は PC 側が 192.168.7.1/24、Brain 側の usb0 が 192.168.7.2/24 の固定 IP。PC に DHCP サーバーは不要。

## 残っている課題と注意点

- **画面の端**：端から指一本分ほどの帯は、指の腹では反応しない。枠に当たってパネルを押し込めず、ドライバにタッチが届かないためで、ソフトでは直せない。端の列は幅が画面の 4 分の 1 あるので、少し内側を押せば使える。
- **左下のセル**：セル 0,2 にはキーを割り当てていない。必要なら config.yaml の cells に追加する。
- **起動の遅さ**：電源投入からキー入力を受け付けるまで約 68 秒かかる。大半は SD カードのマウントなど別の処理で、ガジェットの作成は原因ではない。
- **install.sh と設定**：install.sh は既存の /etc/lefthand/config.yaml を上書きしない。手元の config.yaml を更新したら、手動でコピーする。
- **gadget-setup.sh の実行**：bash で実行すること。Brain の sh は dash で、HID のディスクリプタを書く printf が正しく動かない。
