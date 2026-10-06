# 設定 GUI とのプロトコル

設定 GUI（`gui/`）と、Brain 上のデーモン `lefthand` が、USB シリアルでやりとりする形式。
実装はデーモン側が `control.go`、GUI 側が `gui/src/protocol.ts`。

## 通信路

USB ガジェットは NCM + HID + ACM + ACM の複合デバイスにしてある（`gadget-setup.sh`）。

| ファンクション | Brain 側 | PC 側（Linux の例） | 用途 |
| --- | --- | --- | --- |
| acm.usb0 | /dev/ttyGS0 | /dev/ttyACM0 | シリアルコンソール（getty を立てる場合） |
| acm.usb1 | /dev/ttyGS1 | /dev/ttyACM1 | 設定 GUI。デーモンが開いて待つ |
| ncm.usb0 | usb0 | enx8a158b443a01 | SSH（192.168.7.2） |
| hid.usb0 | /dev/hidg0 | キーボード | 左手デバイスの入力 |

- **ACM を分けた理由**：コンソールと同じポートを使うと、getty とデーモンが入力を取り合う。GUI 用に別のポートを足せば、どちらも独立して使える。ネットワーク（NCM）経由の HTTP や WebSocket も考えたが、PC 側で固定 IP の設定が要り、https のページ（GitHub Pages）から http の Brain へはつなげない（混在コンテンツ）。WebSerial なら、ケーブルをつなぐだけで、どちらのページからも使える。
- **ポートの見分け方**：2 つの ACM は、USB の ID（1d6b:0104）も説明の文字列も同じで、ブラウザからは区別できない。GUI は `hello` を送って、答えたほうを使う。
- **デバイスの指定**：デーモンの `-serial` オプション（既定 `/dev/ttyGS1`）。空にすると使わない。
- **端末の設定**：デーモンは ttyGS1 を生のモード（エコーなし、行の編集なし、改行の変換なし）にして使う。

## 形式

- 1 行に 1 つの JSON オブジェクト。UTF-8 で、改行（`\n`）で区切る。`\r\n` も受け付ける。空行は無視する。
- **リクエスト**：`{"id": 1, "cmd": "hello", ...}`。`id` は数か文字列で、必須。
- **レスポンス**：同じ `id` を返す。成功は `{"id": 1, "ok": true, "result": {...}}`、失敗は `{"id": 1, "ok": false, "error": {"code": "...", "message": "...", "problems": [...]}}`。
- **通知**：`id` がなく、`event` がある行。`subscribe_input` のあとに届く。
- **順序**：デーモンはリクエストを届いた順に 1 つずつ処理する。レスポンスと通知は、行の途中で混ざらない（書き込みは 1 つの goroutine だけが行う）。通知は、レスポンスの前後どちらにも入りうる。

### 大きさと、壊れた入力

| 状況 | デーモンの動き |
| --- | --- |
| 1 行が 256 KiB（262144 バイト）を超えた | 次の改行まで読み捨て、`too_large` を返す。`id` は読めないので `null` |
| 改行が来ないまま 2 秒止まった | 途中の行を捨て、`incomplete_line` を返す（`id` は `null`）。GUI が閉じた、ケーブルが抜けたなど |
| JSON として読めない、UTF-8 でない | `parse_error`（`id` は `null`） |
| `id` がない、数でも文字列でもない | `bad_request`（`id` は `null`） |
| 知らない `cmd` | `unknown_command` |

- **接続の始め**：GUI はポートを開いたら、まず空行を 1 つ送る。前の接続の切れ端が Brain 側に残っていても、新しいリクエストとつながらない。
- **上限の確認**：`hello` の `max_line` が上限。GUI は、これを超えるリクエストを送らずにエラーにする。設定は、16×16 の格子のレイヤーが 10 個でも 200 KiB 程度に収まる。
- **PC が読まないとき**：書き込みが 5 秒進まなければ、その接続をあきらめ、ポートを開き直す。入力の通知は 64 個まで待ち行列に入れ、あふれたら捨てる。
- **入力の処理への影響**：キーボードとタッチの処理は、通信を一切待たない。ポートがない、開けない、PC が読まない、のどの場合も、キー入力の変換はそのまま続く。

## コマンド

### hello

```json
→ {"id":1,"cmd":"hello"}
← {"id":1,"ok":true,"result":{"protocol":1,"daemon":"lefthand","version":"eb6e67e0a1b2","max_line":262144,
   "config_path":"/etc/lefthand/config.yaml","commands":["hello","get_config",...]}}
```

- `protocol` はこの文書の版。互換性のない変更をしたら上げる。GUI は違えば使わない。
- `version` はデーモンをビルドした git のリビジョン（12 桁）。作業中の変更を含むと `+dirty` が付く。

### get_config

```json
→ {"id":2,"cmd":"get_config"}
← {"id":2,"ok":true,"result":{"config":{...},"path":"/etc/lefthand/config.yaml"}}
```

`config` は、今動いている設定。旧形式は layers の形にそろえ、割り当てはすべてオブジェクトの形（`{"key":"B"}`）、省略した項目には既定値が入る（`lefthand -dump-json` と同じ）。

### validate

```json
→ {"id":3,"cmd":"validate","config":{...}}
→ {"id":3,"cmd":"validate","text":"layers:\n  - name: base\n ..."}
← {"id":3,"ok":true,"result":{"valid":false,
   "errors":[{"path":"/layers/0/keys/KEY_Q","message":"layer \"base\" key KEY_Q: unknown key \"NOPE\" in \"LCTRL+NOPE\""}],
   "warnings":[]}}
```

- 適用せずに検証する。`-check` と同じ検証に加え、動作中には変えられない項目（下を参照）が変わっていないことを確かめる。
- `config`（JSON のオブジェクト）か `text`（YAML か JSON の文字列）のどちらかを渡す。
- 正しければ `valid: true` と、layers の形にそろえた `config` を返す。
- **誤りの場所**：`path` は JSON Pointer（RFC 6901）。`~` は `~0`、`/` は `~1` と書く。場所が分からない誤り（YAML の文法、知らない項目など）は `path` が空。

| path の形 | 場所 |
| --- | --- |
| `/layers/N/keys/KEY_X` | レイヤー N の本体キー |
| `/layers/N/touch/cells/C,R` | レイヤー N のセル |
| `/layers/N/soft_keys/NAME` | レイヤー N のソフトキー |
| `/layers/N/touch` | レイヤー N の格子の大きさ |
| `/layers/N/name` | レイヤーの名前（空、重複） |
| `/layers/N` | レイヤー全体（base に戻る手段がない、など） |
| `/touch`、`/display/rotate`、`/hid_device` など | そのほかの項目 |

### set_config

```json
→ {"id":4,"cmd":"set_config","config":{...}}
← {"id":4,"ok":true,"result":{"saved":"/etc/lefthand/config.yaml","previous":"/etc/lefthand/config.yaml.prev",
   "warnings":[],"status":{...}}}
```

次の順に行う。

1. `validate` と同じ検証をする。誤りがあれば `invalid_config` を返し、何も変えない（`problems` に場所付きの誤り）。
2. YAML にして、同じディレクトリの一時ファイルに書き、fsync してから `config.yaml` と置き換える（rename）。前の版は `config.yaml.prev` に残す。書き出した YAML を読み直し、同じ設定になることも確かめる。
3. デーモンを再起動せずに反映する。押しているキーをすべて離し（空のレポートを送る）、レイヤーの重なりを base だけに戻してから、新しい割り当てにする。画面も新しい設定で描き直す。
4. 反映に失敗したら、ファイルを元の内容に戻し、前の割り当てで動き続ける。`apply_failed` を返す。

- `config` だけを受け付ける（`text` は不可）。
- **保存の形式**：YAML。手で編集できるよう、割り当ては 1 行（キーだけなら `KEY_A: LCTRL+Z`、ほかは `{key: B, label: ブラシ}`）、セルの番号は `"0,0"` と書く。**元のファイルのコメントは消える**。先頭に、GUI が書いたことを示すコメントを付ける。
- **押したまま保存したとき**：保存の前に押していたキーやタッチは、離しても何も送らない。
- **動作中には変えられない項目**：`hid_device`、`keyboard`、`touch.device`、`touch` の有無、`display`（`press_style` を除く）。開いているデバイスにかかわるため。変えると `/hid_device` などの場所付きで `invalid_config` になる。変えるときは、ファイルを直接編集してサービスを再起動する。
- **キャリブレーション**：`touch` の min_x などと `soft_areas` は、動作中に変えられる。
- **押したときの見せ方**：`display.press_style` は、動作中に変えられる。画面全体を描き直す。

### get_keymap

```json
→ {"id":5,"cmd":"get_keymap"}
← {"id":5,"ok":true,"result":{"model":"PW-SH2","keys":[
   {"id":"q","label":"Q","code":"KEY_Q","symbol":"KEY_1","row":1,"x":0,"w":1}, ...],
   "max_rollover":3,"blocked":[["KEY_LEFTALT","KEY_Q","KEY_W"]],"constraints":["..."],"screen":{"w":800,"h":480}}}
```

本体の物理キーの一覧。内容は [keymap-pwsh2.md](keymap-pwsh2.md) と同じ。

| 項目 | 内容 |
| --- | --- |
| `id` | 表の中で一意な名前。「調べる」と「戻る」は同じ KEY_ESC なので、コードとは別に持つ |
| `label` | 刻印 |
| `code` | 届く KEY_* 名。ない（電源、ツール、ホーム、記号）なら割り当てられない |
| `symbol` | 「記号」を押しながらのときに届く KEY_* 名。ないなら、そのあいだは届かない |
| `row`、`x`、`w` | 描く位置。段、左端、幅（キー 1 つの幅を 1 とする）。wiki の keymap.png の並びをもとにした概略 |
| `note` | 注意 |
| `max_rollover` | ドライバが 1 回に読めるキーの数 |
| `blocked` | 実機で届かなかった同時押し（最後のキーが届かない） |
| `screen` | 画面の大きさ（ピクセル） |

GUI は、接続する前にも使えるよう、同じ内容を `gui/src/keymap-pwsh2.json` に持っている。Go のテスト（`keymap_pwsh2_test.go`）が、2 つが同じことを確かめる。

### get_status

```json
→ {"id":6,"cmd":"get_status"}
← {"id":6,"ok":true,"result":{"status":{"layer":"edit","label":"編集","mode":"temp",
   "stack":[{"layer":"edit","kind":"layer_hold"}],"cols":4,"rows":3},
   "uptime_sec":120,"subscribed":true,"suppressing":true}}
```

`mode` は `base`（base だけ）、`latched`（layer_toggle か layer_to で切り替えたまま）、`temp`（layer_hold か layer_oneshot で一時的）。画面の右上の札の色と同じ。

### subscribe_input

```json
→ {"id":7,"cmd":"subscribe_input","enable":true,"suppress":true}
← {"id":7,"ok":true,"result":{"subscribed":true,"suppress":true,"lease_sec":30}}
→ {"id":8,"cmd":"subscribe_input","enable":false}
```

Brain のキーとタッチを、通知で知らせる。GUI の学習モードで使う。

- **suppress**：true なら、押したキーとタッチを PC に送らず、レイヤーも変えない（知らせるだけ）。離したときはいつもエンジンに渡すので、押しっぱなしにはならない。
- **期限**：suppress は、最後のリクエストから 30 秒（`lease_sec`）で自動で解ける。GUI が落ちても、Brain が使えなくならないようにするため。GUI は学習モードのあいだ、10 秒ごとに `get_status` を送る。
- **終わり**：`enable: false`、ポートの開き直し（ケーブルを抜いた、PC が読まなかった）で購読も suppress も終わる。購読できるのは 1 つの接続だけ。

通知の形：

```json
{"event":"input","type":"key","code":"KEY_Q","layer":"base","suppressed":true}
{"event":"input","type":"touch","x":2191,"y":2222,"col":2,"row":1,"layer":"base","suppressed":true}
{"event":"input","type":"touch","x":3800,"y":3700,"col":3,"row":0,"soft":"home","layer":"base"}
{"event":"layer","layer":"edit","label":"編集","mode":"temp","stack":[{"layer":"edit","kind":"layer_hold"}],"cols":4,"rows":3}
```

- **key**：押したときだけ（離したときとリピートは知らせない）。「記号」を押しながらのときは、そのコード（KEY_1 など）で届く。
- **touch**：触れた瞬間の生の座標。`col`、`row` は今の重なりの格子でのセル。`soft` は、ソフトキーの範囲に入っていれば、割り当ての有無によらず、その名前。GUI は、編集中のレイヤーの格子と、割り当ての有無で、セルかソフトキーかを決め直す。
- **layer**：レイヤーの重なりが変わったとき。購読していれば、学習モードでなくても届く。

## エラーの種類

| code | 意味 |
| --- | --- |
| `parse_error` | JSON として読めない |
| `bad_request` | `id` や `cmd` がない、引数がおかしい |
| `unknown_command` | 知らないコマンド |
| `too_large` | 行が上限を超えた |
| `incomplete_line` | 改行が来ないまま時間が過ぎた |
| `invalid_config` | 設定の誤り。`problems` に場所付きで入る。何も変えていない |
| `apply_failed` | 保存したが反映に失敗し、ファイルも動作も前の設定に戻した |
| `internal_error` | そのほか（ファイルを書けないなど）。何も変えていないか、前の設定に戻せなかったことを message に書く |

GUI 側だけのエラー：`timeout`（返事が来ない。既定 5 秒、保存は 15 秒）、`closed`（切れた）、`write_failed`。
