# 設定 GUI とのプロトコル

設定 GUI（`gui/`）と PC のコマンド `brain-deck`（`cmd/brain-deck/`）が、Brain 上のデーモン `lefthand` と USB シリアルでやりとりする形式。
実装はデーモン側が `control.go`、GUI 側が `gui/src/protocol.ts`、brain-deck 側が `cmd/brain-deck/client.go`。

## 通信路

USB ガジェットは NCM + HID + ACM + ACM の複合デバイスにしてある（`gadget-setup.sh`）。

| ファンクション | Brain 側 | PC 側（Linux の例） | 用途 |
| --- | --- | --- | --- |
| acm.usb0 | /dev/ttyGS0 | /dev/ttyACM0 | シリアルコンソール（getty）。設定 GUI のコンソールのタブ |
| acm.usb1 | /dev/ttyGS1 | /dev/ttyACM1 | 設定 GUI。デーモンが開いて待つ |
| ncm.usb0 | usb0 | enx8a158b443a01 | SSH（192.168.7.2） |
| hid.usb0 | /dev/hidg0 | キーボード（usb_mode: mouse ではマウスとメディアキーも） | 左手デバイスの入力 |

- **ACM を分けた理由**：コンソールと同じポートを使うと、getty とデーモンが入力を取り合う。GUI 用に別のポートを足せば、どちらも独立して使える。ネットワーク（NCM）経由の HTTP や WebSocket も考えたが、PC 側で固定 IP の設定が要り、https のページ（GitHub Pages）から http の Brain へはつなげない（混在コンテンツ）。WebSerial なら、ケーブルをつなぐだけで、どちらのページからも使える。
- **ポートの見分け方**：コンソール用のポートでは getty がログイン画面を出しているので、`hello` を書くとユーザー名として入力されてしまう。そのため、コンソール用には何も書かずに設定用を見分ける。
  - **brain-deck**：ポートの名前で見分ける。Linux は `/dev/serial/by-id/…-if05`（コンソール用は `-if03`）、macOS は `/dev/cu.usbmodem0123456789` に続く番号の大きいほう。設定用の 1 つだけを開く（`cmd/brain-deck/ports.go`）。
  - **設定 GUI**：WebSerial の `getInfo()` で分かるのは USB の ID（1d6b:0104）だけで、2 つの ACM は同じに見える（インターフェイス番号もデバイス名も、ページからは見えない。Chromium 147 で確かめた）。そこで、役割が分からないポートには自動で書かず、ブラウザのポートの一覧（Linux なら ttyACM0 / ttyACM1 の名前が出る）でユーザーに選んでもらう。選んだポートは、`hello` を送る前に 0.3 秒、何も書かずに待ち、ログイン画面やシェルの文字が届いたら送らない。`hello` に答えたポートは設定用、コンソールのタブで lefthand ではない文字が届いたポートはコンソール用として、ページを開いているあいだ覚える。2 つのうち一方が分かれば、もう一方はその逆として、一覧を出さずに使う（`gui/src/ports.ts`）。
- **デバイスの指定**：デーモンの `-serial` オプション（既定 `/dev/ttyGS1`）。空にすると使わない。
- **端末の設定**：デーモンは ttyGS1 を生のモード（エコーなし、行の編集なし、改行の変換なし）にして使う。
- **1 つの接続だけ**：Brain 側からは、PC で何個のプロセスがポートを開いているか分からない。PC で 2 つのプロセスが同時に開くと、返事がどちらかに分かれて届き、両方の通信が壊れる。そこで PC 側で、同時には開かないようにする。設定 GUI（Chrome の WebSerial）は開くと TIOCEXCL（排他モード）にし、brain-deck も同じようにするので、片方が開いているあいだ、もう片方の open は EBUSY になる。brain-deck は、EBUSY を「設定 GUI が接続中」として、書かずに終わる（README の「設定 GUI と同時に使えない仕組み」）。
- **デーモンが開く前に送った行**：デーモンが ttyGS1 を開いていないあいだ（再起動の途中など）に PC が送った行は、Brain 側で捨てられ、返事は来ない（実機で確かめた）。返事がなければ、開き直してもう一度送る。

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
→ {"id":1,"cmd":"hello","client":"brain-deck/0123456789ab"}
← {"id":1,"ok":true,"result":{"protocol":1,"daemon":"lefthand","version":"eb6e67e0a1b2","max_line":262144,
   "config_path":"/etc/lefthand/config.yaml","commands":["hello","get_config",...]}}
```

- `protocol` はこの文書の版。互換性のない変更をしたら上げる。GUI は違えば使わない。
- `version` はデーモンをビルドした git のリビジョン（12 桁）。作業中の変更を含むと `+dirty` が付く。
- `commands` は、このデーモンが受け付けるコマンド。コマンドを足しただけ（前の版の GUI もそのまま使える）のときは、`protocol` を上げない。GUI は、`set_time` があるときだけ時刻を合わせ、`get_text` があるときだけテキストを読み、`get_todo` があるときだけ Todo を読み、`get_calendar` があるときだけ予定を読み、`image_begin` があるときだけ背景画像を扱い、`get_qr`・`get_widgets` があるときだけ QR コードとストップウォッチなどの状態を読み、`get_graph` があるときだけグラフの値を読み、`set_rotate` があるときだけ「Brain もこの向きにする」を出す。brain-deck は、使うコマンドがなければ「lefthand を新しくしてください」で終わる。
- `client`（省略可）は、つないだ側の名前。デーモンは `-v` のときログに出すだけ。
- `upload_kinds` は、`upload_begin` で受け取れる種類（`update`、`image`、`weather_icon`）。この項目がなければ、`upload_*` がない版。

### get_config

```json
→ {"id":2,"cmd":"get_config"}
← {"id":2,"ok":true,"result":{"config":{...},"path":"/etc/lefthand/config.yaml","overrides":[…]}}
```

`config` は、設定ファイルの値（Brain の設定のアプリで変えた上書きを重ねる前）。設定 GUI はこれを編集して `set_config` で保存するので、上書きは設定ファイルに入らない。旧形式は layers の形にそろえ、割り当てはすべてオブジェクトの形（`{"key":"B"}`）、省略した項目には既定値が入る（`lefthand -dump-json` と同じ）。

`overrides` は、上書きの値（下の「上書きの値」）。この項目がなければ、上書きのない版（フェーズ K1 より前）。そのときの `config` は、今動いている設定。

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
   "stack":[{"layer":"edit","kind":"layer_hold"}],"cols":4,"rows":3,"rotate":0,"portrait":false,"status_bar":false,"home":false},
   "uptime_sec":120,"subscribed":true,"suppressing":true,
   "time":{"now":"2026-10-06T19:04:27.13+09:00","timezone":"Asia/Tokyo","utc_offset_sec":32400,"synced":true,"ntp_synced":false,
   "last_set":"2026-10-06T10:00:46.61Z","last_source":"gui"},"hid":{"mouse":true,"media":true,"switching":false,"usb_state":"configured"},
   "terminal":{"active":true,"state":"on","transport":"serial","status":"PC のログイン画面","cols":100,"rows":31},
   "battery":{"percent":100,"voltage_mv":4172,"plugged":true,"charging":false,"source":"lradc","estimated":true},
   "backlight":{"enabled":true,"state":"dim","level":1,"max":7,"device":"/sys/class/backlight/backlight@0"},
   "rtc":{"mode":"read","readable":true,"seconds":844482277,"time":"2026-10-05T03:04:37+09:00","offset_ms":-497629310,
     "stat":"0xe0000000","persistent0":"0x820121","boot":"not used: the RTC says 2026-10-05 03:01:40, before the clock restored at boot or the last known time (2026-10-10 21:15:28)"},
   "link":{"state":"configured","online":true,"offline_mode":false}}}
```

`time` は Brain の時刻の状態（下の `set_time` の結果と同じ形）。

`rtc` は i.MX28 の内蔵 RTC（`rtc.go`。設定の `clock.rtc`）。この項目がなければ、RTC を読まない版（フェーズ P2 より前）。

| 項目 | 内容 |
| --- | --- |
| `mode` | `read`（起動したときに読んで合わせるだけ）、`write`（時刻を合わせたら書き戻す）、`off`（使わない。ほかの項目は出さない） |
| `readable` | 読めたか。false なら `error` に理由（`/dev/mem` が開けない、RTC_PRESENT が 0 など） |
| `seconds` | HW_RTC_SECONDS（0x80056030）の値 |
| `time` | `seconds` を「2000-01-01 00:00:00 起点の、Brain のタイムゾーンの時刻」とみなしたもの（Brain 純正のファームウェアの形） |
| `offset_ms` | `time` − Brain のシステムの時刻（読んだとき） |
| `stat`、`persistent0` | HW_RTC_STAT と HW_RTC_PERSISTENT0（調べるため。lefthand は PERSISTENT に書かない） |
| `boot` | デーモンが起動したときにどうしたか（`set the clock from the RTC (…)`、`not used: …`、`not needed (…)`） |
| `last_write`、`written_at` | `write` のとき、最後に書いた結果（`ok` か `failed: …`）と時刻 |

`lid` はふた（`lid.go`）：`{"closed":false,"tablet":false,"close":"off","since":"…"}`。`closed` はふたが閉じている、`tablet` は画面を裏返したタブレットの形、`close` は閉じたときの動き（設定の `lid.close`、上書き）、`screen` は閉じて画面を消しているとき `off`。`overrides` は上書きの値（`get_overrides` と同じ）。`backlight.lid` は、ふたが閉じていて画面を消している。どれもなければ、フェーズ K1 より前の版。

`link` は PC とのつながり（`link.go`）。この項目がなければ、フェーズ P2 より前の版。

| 項目 | 内容 |
| --- | --- |
| `state` | `configured`（PC とつながっている）、`suspended`（PC がスリープ中）、`power`（USB の 5V はあるが PC と通信していない。充電器、電源の切れた PC）、`not attached`（外れている）、そのほかの UDC の state。分からなければ空 |
| `online` | `configured`、`suspended`、空なら true |
| `offline_mode` | デーモンが「外れている」として、PC に送るものを休みにしているか（5V があるまま `configured` でなくなったときは、8 秒続いてから） |

`hid.mouse` は、Brain の USB にマウスがあるか（`mouse:` とトラックパッドが PC に届くか）。起動したときはキーボードだけ（ブートキーボード）の形なので false。`hid.media` は、メディアキー（`media:`）が PC に届くか。キーボードとマウスの形で true（ガジェットを作り直す前の古い形では、マウスがあっても false）。この項目がなければ、メディアキーに対応する前の版。`hid.switching` は、`set_usb_mode` で切り替えている途中か。`hid.usb_state` は、Brain の USB コントローラの状態（`/sys/class/udc/ci_hdrc.0/state`。`configured` でつながっている、`suspended` は PC がスリープしてバスを止めている）。`hid` がなければ、lefthand がマウスに対応する前の版。

`battery` は電池の状態（`battery.go` の `BatteryInfo`）。読めなければ `null`。この項目がなければ、電池を読まない版。

| 項目 | 内容 |
| --- | --- |
| `percent` | 残り（0〜100） |
| `voltage_mv` | 電池の電圧（mV。lradc では 5 回の移動平均） |
| `plugged` | USB などから給電されている |
| `charging` | 充電中。`estimated` なら、給電されていて満充電の電圧（4.15 V）より低いことからの見積もり |
| `source` | `power_supply` か `lradc`（PW-SH2） |
| `estimated` | 残りと充電中を電圧から見積もった |

`backlight` は自動減光（設定の `backlight`）の状態（`backlight.go` の `DimInfo`）。`enabled` が false なら減光していない（設定に `backlight` がない、バックライトがない）。`state` は `bright`、`dim`、`off`、`photo`（暗くする代わりに写真を出している。フォトフレーム）、`level` は今の明るさ、`max` は max_brightness。

`photo` はフォトフレーム（`photo.go`）：`{"available":true,"count":3,"showing":"8e507de0d4f1d833"}`。`showing` は今出している写真の id（出していなければない）。この項目がなければ、フォトフレームのない版。

`terminal` は端末モードの状態（`termmode.go` の `TermInfo`）。この項目がなければ、端末モードがない版。

| 項目 | 内容 |
| --- | --- |
| `active` | 端末モード（入る途中を含む）。true のあいだ、Brain のコンソール（ttyGS0 の getty）は止めてある |
| `state` | `off`、`entering`（入る途中）、`on`、`leaving`（抜ける途中） |
| `transport` | `serial`（ttyGS0 で PC の getty にログインする）か `command`（`terminal.command` を PTY で動かす）。`off` のときはない |
| `status` | Brain の画面の帯に出している状態（「PC のログイン画面」「PC から応答がありません…」など） |
| `cols`、`rows` | 端末の桁数と行数 |

### set_usb_mode

USB の形を切り替える（キーボードだけ ⇔ キーボードとマウス）。`mode` は `keyboard`、`mouse`、`toggle`。

```json
→ {"id":7,"cmd":"set_usb_mode","mode":"mouse"}
← {"id":7,"ok":true,"result":{"mouse":true,"switching":true}}
```

- **返事のあと**：`switching` が true なら、返事を送ってから 0.3 秒ほどあとに USB を付け直す。このシリアルも切れるので、GUI は接続し直す。2〜3 秒かかる。
- **何もしないとき**：すでにその形なら `switching` は false。
- **誤り**：切り替えの途中なら `conflict`、mode が違えば `bad_request`。

### set_terminal

端末モードに入る・抜ける（[README の「端末モード」](../README.md#端末モード)）。`mode` は `on`、`off`、`toggle`、`pc`（PC に直接）、`brain`（Brain 自身にログイン）。PC につないでいるときの `on` と `toggle` は、Brain の画面に「PC」と「Brain」を選ぶ画面を出す（`info.state` が `choosing`）。PC から外しているときの `on` は Brain。Brain 自身のときは `info.transport` が `brain`。

```json
→ {"id":8,"cmd":"set_terminal","mode":"on"}
← {"id":8,"ok":true,"result":{"terminal":true,"info":{"active":true,"state":"entering"}}}
```

- **返事のあと**：`terminal` は切り替えたあとに入っているか。手順は返事のあとに進む。シリアルの端末モードでは、0.3 秒ほどあとに USB を付け直す（入るときも抜けるときも）。このシリアルも切れるので、GUI は接続し直す。入るのに約 4 秒、抜けるのに約 4 秒（実機）。
- **途中で頼んだとき**：入る途中で `off` を頼むと、入り終えてから抜ける。
- **誤り**：mode が違えば `bad_request`。

`mode` は `base`（base だけ）、`latched`（layer_toggle か layer_to で切り替えたまま）、`temp`（layer_hold か layer_oneshot で一時的）。画面の右上の札の色と同じ。

`status` の `rotate` は画面の向き（0、90、180、270）、`portrait` は縦向きか（`cols` と `rows` は縦向きの格子のもの）、`status_bar` は状態の帯を出しているか、`home` はホーム画面を出しているか。ホーム画面のときは、`layer` が `home`、`mode` が `base`、`cols` と `rows` が 1。これらがなければ、フェーズ P1 より前の版。レイヤーの通知（`event: layer`）も同じ項目を持つ。

### set_rotate

画面の向きを変える（設定の `rotate` の割り当てと同じ）。`mode` は `next`（90 度回す）、`0`、`90`、`180`、`270`。変えた向きは Brain に残る（`display.json`）。

```json
→ {"id":9,"cmd":"set_rotate","mode":"90"}
← {"id":9,"ok":true,"result":{"status":{"layer":"home","label":"ホーム","mode":"base","stack":[{"layer":"home","kind":"layer_to"}],
   "cols":1,"rows":1,"rotate":90,"portrait":true,"status_bar":true,"home":true}}}
```

- **誤り**：mode が違えば `bad_request`。

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
{"event":"terminal","active":true,"state":"on","transport":"serial","status":"PC のログイン画面","cols":100,"rows":31}
```

- **key**：押したときだけ（離したときとリピートは知らせない）。「記号」を押しながらのときは、そのコード（KEY_1 など）で届く。
- **touch**：触れた瞬間の生の座標。`col`、`row` は今の重なりの格子でのセル。`soft` は、ソフトキーの範囲に入っていれば、割り当ての有無によらず、その名前。GUI は、編集中のレイヤーの格子と、割り当ての有無で、セルかソフトキーかを決め直す。
- **layer**：レイヤーの重なりが変わったとき。購読していれば、学習モードでなくても届く。
- **terminal**：端末モードに入った・抜けた・帯の状態が変わったとき（`get_status` の `terminal` と同じ形）。シリアルの端末モードでは、切り替えの途中で USB を付け直すので、つなぎ直したあとは `get_status` で読み直す。

### set_time

```json
→ {"id":9,"cmd":"set_time","unix_ms":1791280847384,"source":"gui"}
← {"id":9,"ok":true,"result":{"stepped":true,"offset_ms":134715876,
   "now":"2026-10-06T19:00:47.38+09:00","timezone":"Asia/Tokyo","utc_offset_sec":32400,"synced":true,"ntp_synced":false,
   "last_set":"2026-10-06T10:00:46.61Z","last_source":"gui"}}
```

Brain のシステムの時刻を合わせる。Brainux は内蔵 RTC を使わない（ドライバが無効）ので、lefthand が起動したときに RTC から合わせられなければ、電源を切っていたあいだの分だけ時刻が遅れる。設定 GUI は、接続するたびに PC の時刻を送る。設定の `clock.rtc` が `write` なら、合わせた時刻を内蔵 RTC にも書く（返事のあと）。

| 引数 | 内容 |
| --- | --- |
| `unix_ms` | 合わせる時刻。1970-01-01 UTC からのミリ秒。2024 年から 2100 年まで。範囲の外は `bad_request` |
| `source` | 送った側の名前（記録とログ用）。省略すると `unknown` |

| 結果 | 内容 |
| --- | --- |
| `stepped` | システムの時刻を動かしたか。ずれが 0.5 秒より小さいときは動かさない（合わせたことにはなる） |
| `offset_ms` | 合わせる前のずれ（送られた時刻 − Brain の時刻）。正なら Brain が遅れていた |
| `now`、`timezone`、`utc_offset_sec` | 合わせたあとの Brain の時刻と、タイムゾーン（`/etc/localtime`）。タイムゾーンは変えない。GUI は PC と違えば知らせる |
| `synced` | Brain を起動してから、時刻を合わせたか（`set_time` か NTP）。false のあいだ、時計のウィジェットは「時刻未設定」と出す |
| `ntp_synced` | NTP で合っているか（カーネルの STA_UNSYNC が消えている） |
| `last_set`、`last_source` | 最後に合わせた時刻（UTC）と、合わせた側（`set_time` の `source`、Brain のキーボードなら `brain`、起動したときに内蔵 RTC からなら `rtc`） |
| `last_pc_set` | 最後に PC から（`set_time` で）合わせた時刻。状態の帯の「同期」に出す |
| `boot_source` | この起動のあいだに合わせた側（合わせていなければない） |
| `last_known` | 合っていると分かっていた最後の時刻（合っているあいだ 30 分ごとと、デーモンが終わるときに残す）。RTC が使えないときの目安 |

- **記録**：合わせたことは `/var/lib/lefthand/clock.json` に、起動ごとの ID（boot_id）と一緒に残す。デーモンを再起動しても「合わせ済み」のまま。Brain を再起動すると、ID が変わるので「未設定」に戻る。記録は返事のあとに書く（SD カードの書き込みを待たせない）。
- **権限**：デーモンは root で動くので、時刻を変えられる。

### set_text

```json
→ {"id":10,"cmd":"set_text","name":"build","text":"ビルド成功","style":"ok","ttl_sec":600,"source":"brain-deck"}
← {"id":10,"ok":true,"result":{"name":"build","cleared":false,"shown":true,
   "entry":{"text":"ビルド成功","style":"ok","set_at":"2026-10-06T13:03:34.5Z","expires_at":"2026-10-06T13:13:34.5Z",
   "source":"brain-deck","expired":false}}}
→ {"id":11,"cmd":"set_text","name":"build","clear":true}
← {"id":11,"ok":true,"result":{"name":"build","cleared":true,"shown":true}}
```

テキストのタイル（`{ widget: text, id: build }`）の中身を書き換える。

| 引数 | 内容 |
| --- | --- |
| `name` | 必須。セルの `id`。英数字と `_ . -` の 32 文字まで（リクエストの `id` と区別するため、`name` にした） |
| `text` | 中身。200 文字、8 行（改行 `\n`）まで。`\r\n` と `\r` は改行に、タブは空白にする。そのほかの制御文字は誤り。空の文字列も書ける（何も描かない） |
| `style` | `normal`（既定）、`ok`、`error`、`warn` |
| `ttl_sec` | 有効期限（秒、正の数）。省略すると期限なし。30 日まで |
| `clear` | `true` なら消す（`text` は要らない）。セルは「未設定」に戻る |
| `source` | 送った側の名前（記録とログ用） |

| 結果 | 内容 |
| --- | --- |
| `shown` | 今の設定に、この `name` のテキストのセルがあるか。なくても保存はする（あとで設定にセルを足せば出る） |
| `cleared` | 消したか |
| `entry` | 保存した中身。`expires_at` は Brain の時刻での有効期限 |

- **保存**：`/var/lib/lefthand/text.json` に書く。返事は書き込みを待たない（SD カードの書き込みは数秒かかることがある）。続けて書き換えたときは、まとめて書く。
- **数**：64 個まで。超えたら、いちばん前に書いたものを捨てる。
- **時刻を合わせる前に書いたとき**：Brain の時刻が合っていない（`synced: false`）ときに書いたものは `clock_unset: true` を付けておき、あとで `set_time` が時刻を動かしたら、`set_at` と `expires_at` も同じだけ動かす。期限は「書いてから `ttl_sec` 秒」のまま保たれる。brain-deck は、Brain の時刻がずれていれば、`set_text` の前に `set_time` を送る。
- **誤り**：`name`、`style`、`ttl_sec`、`text` がおかしいと `bad_request`。何も変えない。

### get_text

```json
→ {"id":12,"cmd":"get_text"}
← {"id":12,"ok":true,"result":{"texts":{"build":{"text":"ビルド成功","style":"ok","set_at":"...","expires_at":"...","expired":true}},
   "ids":["build","deploy"]}}
```

| 結果 | 内容 |
| --- | --- |
| `texts` | Brain にあるすべてのテキスト。形は `set_text` の `entry` と同じ。`expired` は、今の時刻で期限が切れているか |
| `ids` | 今の設定で、テキストのセルに使われている id（名前の順） |

### Todo（get_todo、todo_add、todo_update、todo_delete、todo_move、todo_clear_done）

Todo のセル（`{ widget: todo }`）の項目を読み書きする。項目は `/var/lib/lefthand/todo.json` に置き、Brain のデータを正とする。

```json
→ {"id":20,"cmd":"get_todo"}
← {"id":20,"ok":true,"result":{"rev":12,"shown":true,"items":[
   {"id":"t9","text":"急ぎ：Brain の電池を充電","done":false,"rev":9,"created_at":"2026-10-07T00:32:13Z","updated_at":"2026-10-07T00:32:13Z","source":"brain-deck"},
   {"id":"t2","text":"PR #42 のレビュー","done":true,"rev":10,"created_at":"...","updated_at":"...","done_at":"2026-10-07T00:32:14Z","source":"brain"}]}}
→ {"id":21,"cmd":"todo_add","text":"牛乳を買う","index":0,"source":"gui"}
← {"id":21,"ok":true,"result":{"item":{"id":"t10",...},"rev":13,"items":[...],"shown":true}}
→ {"id":22,"cmd":"todo_update","item":"t10","rev":13,"done":true,"source":"gui"}
→ {"id":23,"cmd":"todo_update","item":"t10","rev":14,"text":"牛乳と卵を買う"}
→ {"id":24,"cmd":"todo_move","item":"t10","rev":15,"index":2}
→ {"id":25,"cmd":"todo_delete","item":"t10","rev":15}
→ {"id":26,"cmd":"todo_clear_done"}
← {"id":26,"ok":true,"result":{"removed":1,"rev":17,"items":[...]}}
```

| 項目 | 内容 |
| --- | --- |
| `items` | 並べた順（GUI で並べ替えた順）。完了したものも混ざっている。Brain の画面と brain-deck の番号は、未完了を並べた順のあとに完了を並べた順 |
| `id` | 項目の ID（`t` と数）。消した ID は使い回さない |
| `rev`（一覧） | 一覧が変わるたびに 1 増える |
| `rev`（項目） | その項目を最後に変えたときの、一覧の `rev` |
| `source` | 最後に変えた側（`gui`、`brain-deck`、Brain で長押ししたときは `brain`） |
| `shown` | 今の設定に Todo のセルがあるか（get_todo、todo_add） |

| コマンド | 引数 | 内容 |
| --- | --- | --- |
| `get_todo` | なし | 一覧を返す |
| `todo_add` | `text`（必須）、`index`（省略すると最後） | 足す。`index` は `items` の中の位置 |
| `todo_update` | `item`（必須）、`rev`、`text`、`done` | 文か完了を変える（どちらか 1 つは必須） |
| `todo_delete` | `item`（必須）、`rev` | 消す |
| `todo_move` | `item`、`index`（必須）、`rev` | `items` の中で、ほかの項目をよけたあとの `index` の位置に移す |
| `todo_clear_done` | なし | 完了した項目をすべて消す。`removed` は消した数 |

- **返事**：どれも、変えたあとの一覧（`rev`、`items`）を返す。`todo_add` と `todo_update` は、変えた項目（`item`）も返す。
- **同時の編集**：`rev` を付けると、その項目の今の `rev` と違うとき（送った側が読んだあとに、Brain での長押しなどで変わったとき）は `conflict` を返し、何も変えない。ない ID は `not_found`。GUI と brain-deck は、読んだときの `rev` を付けて送る。
- **文**：前後の空白を除き、改行とタブは空白にする。空と、200 文字を超えるものは `bad_request`。
- **保存**：返事は、`todo.json` の書き込み（SD カード）を待たない。続けて変えたときは、まとめて書く。
- **数**：上限はない。

### set_calendar

カレンダーのセル（`{ widget: calendar }`）と月の表示（`{ widget: month }`）の予定を、まとめて差し替える。PC の `brain-deck calendar sync` が、ICS の URL から取ってきて送る。

```json
→ {"id":30,"cmd":"set_calendar","from":"2026-10-06","days":7,"source":"brain-deck","calendars":[
   {"name":"仕事","color":"#4f9dff","fetched_at":"2026-10-06T00:35:00Z","events":[
     {"title":"設計レビュー","start":"2026-10-06T00:30:00Z","end":"2026-10-06T01:30:00Z","location":"会議室A"},
     {"title":"社内イベント週間","day":"2026-10-05","end_day":"2026-10-10"}]},
   {"name":"家","color":"#50d880","error":"取得できません（HTTP 404）","events":[]}]}
← {"id":30,"ok":true,"result":{"rev":4,"shown":true,"calendars":[
   {"name":"仕事","events":2,"fetched_at":"2026-10-06T00:35:00Z"},
   {"name":"家","events":3,"fetched_at":"2026-10-05T13:10:00Z","error":"取得できません（HTTP 404）","kept":true}]}}
```

| 引数 | 内容 |
| --- | --- |
| `calendars` | 必須。カレンダーの配列（16 個まで）。送らなかったカレンダーは Brain から消える |
| `name` | 1〜32 文字。ほかと重ならない。セルの `calendars` で選ぶときの名前 |
| `color` | `#rrggbb`。省略すると、順に決まった色 |
| `holiday` | `true` なら祝日のカレンダー。月の表示では、その予定のある日を赤くし（名前は日付を選んだときに出す）、予定の印（点）は付けない。カレンダーのセルには、ほかの予定と同じく出る。前の版の lefthand は読み飛ばす |
| `fetched_at` | 予定を取ってきた時刻（PC の時刻）。画面の「更新」に出る |
| `error` | 取得に失敗したときの説明（200 文字まで）。`events` が空なら、Brain は前に受け取った予定と `fetched_at` を残す（結果の `kept`） |
| `events` | 予定。時刻の決まった予定は `start` と `end`（RFC 3339）、終日の予定は `day` と `end_day`（YYYY-MM-DD。`end_day` の日は含まない。省略すると 1 日）。`title` と `location` は 200 文字まで（超えたら切る）。改行は空白にする |
| `from`、`days` | 送った範囲。`from` の日から `days` 日先まで（`days`+1 日分）。月の表示は、この外の日を「範囲外」と出す。`days` は 0〜366。brain-deck は、先月の 1 日から来月の末日までを送る |

- **展開済み**：繰り返し（RRULE）、例外、タイムゾーンは PC で解いてから送る。Brain は受け取った予定をそのまま出す。
- **数**：すべてのカレンダーの予定を合わせて 1000 件まで。1 行の上限（256 KiB）にも収まるように、brain-deck は送る JSON を 200 KiB までにする（超えたら、今日から遠い予定から外す。祝日のカレンダーの予定は先に残す）。実機で、名前の長めな 1000 件の要求は 189 KB だった。
- **保存**：`/var/lib/lefthand/calendar.json` に書く。返事は書き込みを待たない。実機の SD カードで、予定 60 件ほどは 0.1〜0.2 秒、1000 件（246 KB）は 1.0〜7.1 秒かかった（ばらつきが大きい）。1 秒以上かかったときは、`-v` でなくてもログに出す。
- **誤り**：`name` の重なり、`color` の形、`start` と `day` の両方、`end` が `start` より前、制御文字などは `bad_request`。何も変えない。
- **消すとき**：`calendars` を空の配列にして送る（`brain-deck calendar clear`）。

### get_calendar

```json
→ {"id":31,"cmd":"get_calendar"}
← {"id":31,"ok":true,"result":{"rev":4,"received_at":"2026-10-06T00:35:02Z","from":"2026-10-06","days":7,"source":"brain-deck",
   "shown":true,"calendars":[{"name":"仕事","color":"#4f9dff","fetched_at":"...","events":[...]}]}}
```

Brain にある予定を返す。形は `set_calendar` の引数と同じ（`error` を含む）。一度も受け取っていなければ `rev` は 0、`calendars` は空。`shown` は、今の設定にカレンダーのセルがあるか。設定 GUI は、接続したときに読んでプレビューに描く。

### set_weather

天気のセル（`{ widget: weather }`）の予報を差し替える。PC の `brain-deck weather sync` が、天気予報 API（livedoor 天気互換。データの元は気象庁）から取ってきて送る。

```json
→ {"id":32,"cmd":"set_weather","source":"brain-deck","weather":{"area":"東京都 東京","office":"気象庁",
   "public_time":"2026-10-05T20:00:00Z","fetched_at":"2026-10-06T00:05:00Z","days":[
     {"date":"2026-10-06","telop":"晴時々曇","icon":"101","max":26,"min":null,"rain":[null,10,30,60]},
     {"date":"2026-10-07","telop":"曇のち一時雨","icon":"212","max":22,"min":17,"rain":[20,40,70,50]}]}}
← {"id":32,"ok":true,"result":{"rev":3,"kept":false,"shown":true,"icon_boxes":[[183,122],[87,58]],
   "icons_missing":[{"name":"212-183x122","icon":"212","w":183,"h":122}]}}
→ {"id":33,"cmd":"set_weather","weather":{"error":"取得できません（HTTP 500）"}}
← {"id":33,"ok":true,"result":{"rev":4,"kept":true,"shown":true,"icon_boxes":[...],"icons_missing":[]}}
```

| 引数（`weather` の中） | 内容 |
| --- | --- |
| `days` | 日ごとの予報（7 日まで）。`date`（YYYY-MM-DD。重ならない）、`telop`（天気の文。40 文字まで）、`icon`（気象庁の天気アイコンの番号。3 桁）、`max`・`min`（℃。-99〜99。取れなければ `null`）、`rain`（0〜6、6〜12、12〜18、18〜24 時の降水確率。0〜100。取れなければ `null`） |
| `area`、`office` | 地点（「東京都 東京」）と、発表した気象台（40 文字まで） |
| `public_time` | 予報の発表の時刻（RFC 3339）。画面の「5時発表」に出る |
| `fetched_at` | 予報を取ってきた時刻（PC の時刻）。画面の「更新」に出る。省略すると Brain が受け取った時刻 |
| `error` | 取得に失敗したときの説明（200 文字まで）。`days` が空なら、Brain は前の予報（地点、発表、`fetched_at` も）を残し、失敗と時刻だけを覚える（結果の `kept`）。画面には「失敗」と出る |

| 結果 | 内容 |
| --- | --- |
| `kept` | 失敗だけを知らせて、前の予報を残した |
| `shown` | 今の設定に天気のセルがあるか |
| `icon_boxes` | 今の設定の天気のセルで使うアイコンの箱の大きさ（[幅, 高さ]、大きい順）。箱の大きさはセルの大きさだけで決まる |
| `icons_missing` | 予報の天気（`days` の `icon`）と `icon_boxes` の組のうち、まだ Brain にないアイコン。PC は、それぞれを箱に収めた大きさで作り、`upload_*`（kind: `weather_icon`、name: `<番号>-<幅>x<高さ>`）で送る |

- **今日の決め方**：Brain は予報を日付で持ち、Brain の日付と同じ日付のものを今日として出す（API が 0 時〜5 時に返す、前の日の発表の「明日」も、日付で今日になる）。
- **保存**：`/var/lib/lefthand/weather.json` に書く。返事は書き込みを待たない。
- **誤り**：`days` も `error` もない、日付の形や重なり、`icon` の形、範囲の外の数、制御文字などは `bad_request`。何も変えない。

### get_weather

```json
→ {"id":34,"cmd":"get_weather"}
← {"id":34,"ok":true,"result":{"rev":3,"received_at":"...","area":"東京都 東京","public_time":"...","fetched_at":"...","days":[...],
   "shown":true,"icon_boxes":[[183,122],[87,58]],"icons":[{"name":"101-183x122","w":163,"h":122,"bytes":59686,"added":"..."}],
   "icons_bytes":59686,"icons_limit":2097152,"icons_missing":[...]}}
```

Brain にある予報（形は `set_weather` の `weather` と同じ。`error_at` を含む）と、アイコンの一覧。一度も受け取っていなければ `rev` は 0、`days` は空。設定 GUI は、接続したときに読んでプレビューに描く。

### get_weather_icon

```json
→ {"id":35,"cmd":"get_weather_icon","name":"101-87x58","offset":0}
← {"id":35,"ok":true,"result":{"name":"101-87x58","offset":0,"bytes":13406,"data":"TEhBMU0AOgA…"}}
```

アイコンのファイル（下の `.lha`）の `offset` から `length`（既定と上限は 98,304 バイト）を base64 で返す。`bytes` はファイル全体の大きさ。設定 GUI のプレビューが使う。ないアイコンは `not_found`。

**アイコンのファイル**（`/var/lib/lefthand/weather-icons/<番号>-<幅>x<高さ>.lha`）：

```
0..3   "LHA1"
4..5   幅（リトルエンディアンの uint16）
6..7   高さ（同）
8..    画素。左上から行の順に、1 画素 2 バイトの RGB565（リトルエンディアン）
..     そのあとに、1 画素 1 バイトの不透明度（0 透明 〜 255 不透明）
```

- 画像は名前の箱以下の大きさ（縦横比を保って箱に収めたもの）。描くときは箱の中央に置き、不透明度で下の色と混ぜる（下の色を 8 ビットに戻して `(s·a + d·(255−a) + 127) / 255`）。
- 1 つ 460,808 バイト（480×320）まで。合計 2 MiB まで。超えたら、今の設定で使わない大きさのものから古い順に消し、それでも足りなければ `quota_exceeded`。SD カードの空きは背景画像と同じく確かめる（`no_space`）。

### set_qr、get_qr

QR コードのセル（`{ widget: qr, id: share }`）の中身を書き換える、読む。引数と結果は `set_text`、`get_text` と同じ（`name`、`text`、`ttl_sec`、`clear`、`source`。`style` は使わない）。

```json
→ {"id":60,"cmd":"set_qr","name":"share","text":"https://github.com/endohizumi/brain-deck","ttl_sec":3600,"source":"brain-deck"}
← {"id":60,"ok":true,"result":{"name":"share","cleared":false,"shown":true,"entry":{"text":"https://...","style":"normal","set_at":"...","expires_at":"...","expired":false}}}
→ {"id":61,"cmd":"get_qr"}
← {"id":61,"ok":true,"result":{"texts":{"share":{...}},"ids":["share"]}}
```

- **違い**：長さは UTF-8 で 400 バイトまで（行数の上限はない）。タブはそのまま残す。そのほかの制御文字は `bad_request`。
- **保存**：`/var/lib/lefthand/qr.json`。返事は書き込みを待たない。64 個まで。時刻を合わせる前に書いたときの扱いもテキストと同じ。
- `shown` と `ids` は、QR コードのセルについて。

### get_widgets

ストップウォッチ、ポモドーロ、カウンター、在席ランプ、プレゼン用タイマーの状態と、今の設定で使っている id を返す。設定 GUI はプレビューに描く。

```json
→ {"id":62,"cmd":"get_widgets"}
← {"id":62,"ok":true,"result":{
   "widgets":{"stopwatch":{"stopwatch":{"elapsed_ms":5000,"started_at":"2026-10-09T05:32:19.58Z","updated_at":"..."}},
     "pomodoro":{"pomodoro":{"running":true,"anchor":"2026-10-09T05:32:34.58Z","work_ms":1500000,"break_ms":300000,"done":2,"updated_at":"..."}},
     "counter":{"coffee":{"value":3,"updated_at":"..."}},
     "presence":{"presence":{"state":"会議中","updated_at":"...","source":"brain-deck"}}},
   "ids":{"stopwatch":["stopwatch"],"pomodoro":["pomodoro"],"counter":["coffee","counter"],"presence":["door","presence"],"qr":["share"]},
   "presence_states":{"presence":[{"name":"在席","color":"#40c070"},...],"door":[{"name":"在室","color":"#40c070"},{"name":"不在","color":"#606870"}]},
   "battery":{...},"now":"2026-10-09T14:33:30.1+09:00"}}
```

| 項目 | 内容 |
| --- | --- |
| `stopwatch` | `elapsed_ms`（止めているあいだの経過時間。動いているなら、始める前までの分）、`started_at`（動いているなら、始めた時刻）。今の経過時間は `elapsed_ms` + (今 − `started_at`) |
| `pomodoro` | 動いているとき（`running`）：`anchor`（最初の作業を始めたことにした時刻）、`work_ms`、`break_ms`、`done`（`anchor` より前に終えた作業の数）。今の段階は、`anchor` からの経過を `work_ms + break_ms` で割って求める。止めているとき：`phase`（`work` か `break`。なければ、まだ始めていない）、`remaining_ms`、`done`。`ack_at` は点滅をタップで止めた時刻 |
| `counter` | `value` |
| `presenter` | プレゼン用タイマー。`stopwatch` と同じ形（経過時間）。持ち時間と色はセルの設定 |
| `calculator` | 結果の一番上の項目（`widgets` の外）。電卓の id ごとの状態：`base`（10、16、2）、`tokens`（式。`t` が `num`（`s` は打った文字）、`op`（`s` は `+ - * /`）、`lp`、`rp`、`val`（`v` は前の結果の値 `分子/分母`））、`result`（= のあとの結果 `分子/分母`）、`done`（計算した式の表示）、`msg`、`msg_err`、`history`（`expr`、`value`、`base`。新しい順）、`rev`。電卓は通知しない（接続したときに読む） |
| `presence` | `state`（状態の名前）、`source`（変えた側。Brain でタップしたときは `brain`） |
| `ids` | 今の設定で、種類ごとに使われている id（id を書かないセルは種類の名前） |
| `presence_states` | id ごとの、在席ランプの状態の並び（同じ id のセルの状態を、出てきた順に重なりを除いて並べたもの） |

- **保存**：`/var/lib/lefthand/widgets.json`（`boot_id`、`last_seen` も入る）。状態は Brain でのタップか `set_presence` で変わる。

### set_presence

在席ランプの状態を変える。`name` は在席ランプの id（省略すると `presence`）。`state`（状態の名前）か `step`（1 で次、-1 で前）のどちらかを書く。

```json
→ {"id":63,"cmd":"set_presence","state":"会議中","source":"brain-deck"}
← {"id":63,"ok":true,"result":{"name":"presence","state":"会議中","color":"#ff8020","states":4}}
→ {"id":64,"cmd":"set_presence","name":"door","step":1}
```

- **誤り**：設定にその id の在席ランプがなければ `not_found`。`state` がその id の状態にない、`state` と `step` の両方か、どちらもないときは `bad_request`（message に選べる状態を書く）。
- **画面**：すぐ描き直す。画面を明るくはしない。

### add_graph、clear_graph、get_graph

グラフのセル（`{ widget: graph, id: cpu }`）の値を足す、消す、読む。

```json
→ {"id":70,"cmd":"add_graph","points":[{"name":"cpu","value":12.5},{"name":"temp","value":51.3}],"source":"brain-deck"}
← {"id":70,"ok":true,"result":{"added":2,"unknown":[]}}
→ {"id":71,"cmd":"clear_graph","name":"cpu"}
← {"id":71,"ok":true,"result":{"name":"cpu","cleared":true}}
→ {"id":72,"cmd":"get_graph"}
← {"id":72,"ok":true,"result":{"series":{"temp":{"points":[{"t":"2026-10-10T03:04:05Z","v":51.3}],"count":1,
     "last":{"t":"2026-10-10T03:04:05Z","v":51.3},"source":"brain-deck"}},"ids":["cpu","temp"],"now":"..."}}
```

| 項目 | 内容 |
| --- | --- |
| `points` | `add_graph`：足す値。`name`（id。英数字と `_ . -` の 32 文字まで）と `value`（数。-1e12〜1e12）。1 回に 64 個まで。1 つでも誤りがあれば、どれも足さずに `bad_request` |
| `unknown` | 今の設定にセルがない id（値は覚える） |
| `series` | id ごとの値（古い順。`t` は受け取った時刻で、Brain の時計による）。`ids` は、今の設定のグラフのセルの id |

- **覚える数**：id ごとに新しい 120 個。id は 32 個まで（超えたら、いちばん前に値を足した id を捨てる）。
- **保存**：`/var/lib/lefthand/graph.json`。返事は書き込みを待たない。書き込みは間引く（前に書いてから 5 分たつまで書かない。しばらく来ていなかったときは 10 秒後。デーモンが終わるときにも書く）。
- **時刻**：Brain の時刻を合わせる前に受け取った値は、あとで `set_time` で時刻が動いたら、同じだけ動かす（テキストと同じ）。
- **画面**：すぐ描き直す。画面を明るくはしない。

### subscribe_data

```json
→ {"id":27,"cmd":"subscribe_data","enable":true}
← {"id":27,"ok":true,"result":{"subscribed":true,"events":["todo","widgets"]}}
```

Brain のデータが変わったことを、通知で知らせる。今は Todo と、ストップウォッチなどの状態。設定 GUI は接続したときに購読し、Brain で長押しして切り替えたことを、すぐ一覧やプレビューに反映する。

```json
{"event":"todo","rev":14,"items":[...]}
{"event":"widgets","stopwatch":{...},"pomodoro":{...},"counter":{...},"presence":{...}}
```

- **widgets**：ストップウォッチ、ポモドーロ、カウンター、在席ランプの状態が変わるたび（Brain でのタップ、`set_presence`、時刻合わせで動いているタイマーをずらしたとき）。形は `get_widgets` の `widgets` と同じで、すべての状態が入る。時間がたつだけでは届かない（受け取る側が計算する）。

- **いつ届くか**：一覧が変わるたび（どこから変えても。自分のリクエストで変えたときも届く）。続けて変わったときは、最後の一覧だけが届くことがある。返事の前後どちらにも届きうるので、受け取る側は `rev` が今持っているものより小さければ捨てる。
- **終わり**：`enable: false`、またはポートの開き直し。`subscribe_input` とは別で、学習モードを終えても購読は続く。

### 背景画像（list_images、image_begin、image_chunk、image_end、image_abort、get_image、prune_images）

背景画像（設定の `background`。[config.md](config.md) の「背景画像」）のファイルを、Brain の `/var/lib/lefthand/images/` に置く、読む、消す。
画像の変換（切り抜き、縮小、RGB565、ディザリング）は送る側（設定 GUI）で行い、デーモンは中身を確かめて保存するだけ。

**送る**：1 行の上限（256 KiB）を変えずに済むよう、ファイルを 96 KiB ずつ base64 にして送る（1 行 128 KiB ほど）。

```json
→ {"id":40,"cmd":"image_begin","sha256":"7f8f231d4a49bfee…（64 文字）","bytes":768008,"w":800,"h":480,"name":"夕焼け.jpg","source":"gui"}
← {"id":40,"ok":true,"result":{"id":"7f8f231d4a49bfee","exists":false,"upload":"2fd28426216d","chunk_bytes":98304}}
→ {"id":41,"cmd":"image_chunk","upload":"2fd28426216d","offset":0,"data":"TEhJMSADwAH…"}
← {"id":41,"ok":true,"result":{"received":98304}}
   …（offset を増やして続ける）
→ {"id":49,"cmd":"image_end","upload":"2fd28426216d"}
← {"id":49,"ok":true,"result":{"id":"7f8f231d4a49bfee"}}
```

| コマンド | 引数 | 内容 |
| --- | --- | --- |
| `image_begin` | `sha256`（ファイル全体の SHA-256）、`bytes`、`w`、`h`（必須）、`name`（表示用の名前。100 文字まで）、`source` | 受け取りを始める。同じ中身がもうあれば `exists: true` で、送らなくてよい。`id` は `sha256` の先頭 16 文字 |
| `image_chunk` | `upload`、`offset`、`data`（base64。1〜`chunk_bytes` バイト） | 書く。`offset` は、それまでに受け取った大きさと同じでなければ `bad_request`（受け取りは続く）。結果の `received` はそこまでの大きさ |
| `image_end` | `upload` | 大きさ、SHA-256、ヘッダ（`LHI1` と幅、高さ）を確かめ、fsync してから `<id>.565` に rename する。合わなければ一時ファイルを消して `hash_mismatch` か `bad_request` |
| `image_abort` | `upload`（省略するとどれでも） | やめて、一時ファイルを消す。結果の `aborted` は、やめるものがあったか |

- **途中で切れたとき**：受け取っている途中のファイルは `images/.upload-<upload>.part`。`image_end` で確かめるまで、`<id>.565` はできない。`image_abort`、別の `image_begin`、ポートの開き直し（接続の終わり）、60 秒 `image_chunk` が来ない、デーモンの再起動、のどれでも一時ファイルを消す。受け取りは同時に 1 つだけ。
- **上限**：1 枚 800×480（768,008 バイト）まで（各辺 800 まで、384,000 画素まで）。`bytes` は `8 + w*h*2` でなければ `bad_request`。置いてある画像との合計が 16 MiB を超えるなら `quota_exceeded`。保存したあとに SD カードの空きが 64 MiB（ファイルシステムの 10% のほうが小さければ、そちら）を下回るなら `no_space`。どちらも `image_begin` で断り、何も書かない。message に今の量と理由を書く。
- **時間**：実機で、画面いっぱいの画像は 1.8〜2.0 秒（375 KB/秒ほど）。`image_end` は fsync を待つ。GUI は `image_chunk` を 15 秒、`image_end` を 30 秒まで待つ。
- **保存したあと**：画面を描き直す（その画像を使っていれば、すぐに出る）。名前などは `images/index.json` に残す。

**一覧**：

```json
→ {"id":50,"cmd":"list_images"}
← {"id":50,"ok":true,"result":{"images":[{"id":"7f8f231d4a49bfee","name":"夕焼け.jpg","w":800,"h":480,"bytes":768008,
   "sha256":"7f8f…","added":"2026-10-07T07:25:35Z","source":"gui","refs":["/layers/8/touch/background"]}],
   "total_bytes":768008,"limit_bytes":16777216,"max_image_bytes":768008,"max_side":800,"max_pixels":384000,
   "free_bytes":134869401,"reserve_bytes":67108864,"chunk_bytes":98304,"missing":[]}}
```

`refs` は今の設定でその画像を使っている場所（JSON Pointer）、`missing` は今の設定で使っているのに Brain にない id。

**読む**（GUI のプレビュー用）：`{"cmd":"get_image","image":"<id>","offset":0,"length":98304}` → `{"image","offset","bytes"（ファイル全体の大きさ）,"sha256","data"（base64）}`。`length` は省略すると 96 KiB。ない id は `not_found`。

**消す**：`{"cmd":"prune_images","keep":["<id>",...],"dry_run":false}` → `{"removed":[id...],"freed_bytes":n,"dry_run":false}`。今の設定と `keep` のどちらでも使っていない画像を消す。GUI は、編集中の設定で使っている id を `keep` に入れる。`dry_run` なら消さずに、消すものを返す。

## 分割して送る（upload_begin、upload_chunk、upload_end、upload_abort）

1 行の上限（256 KiB）より大きなファイルを、種類（`kind`）を選んで送る共通の形。受け取りの手順は背景画像の `image_*` と同じ（実装も共通。`transfer.go`）。

```json
→ {"id":70,"cmd":"upload_begin","kind":"update","sha256":"…（64 文字）","bytes":4555264,"name":"lefthand-hwtest-b.update.tar.gz","source":"brain-deck"}
← {"id":70,"ok":true,"result":{"upload":"5d1c0e2a9b33","chunk_bytes":98304}}
→ {"id":71,"cmd":"upload_chunk","upload":"5d1c0e2a9b33","offset":0,"data":"H4sIAAAA…"}
← {"id":71,"ok":true,"result":{"received":98304}}
   …
→ {"id":118,"cmd":"upload_end","upload":"5d1c0e2a9b33"}
← {"id":118,"ok":true,"result":{…種類ごとの結果…}}
```

| kind | 上限 | upload_end の結果 |
| --- | --- | --- |
| `update` | 24 MiB | 確かめ終えた更新（下の `update_*`） |
| `image` | 768,008 バイト | `{"id":"<画像の id>"}`。`image_begin` と違い、幅と高さは受け取ったファイルのヘッダから読む。合計の上限と SD カードの空きは同じ |
| `photo` | 768,008 バイト | `{"id":"<写真の id>","count":3,"max":10}`。800×480 か 480×800 の LHI1 だけ。`upload_begin` に `"of":"<横向きの写真の id>"` を付けると、その写真を縦向きに切り抜き直したもの（480×800 だけ）。枚数が `photo.max` に達していれば `quota_exceeded`、SD カードの空きが足りなければ `no_space`（同じ写真は数えない） |
| `read` | 8 MiB | `{"id":"<読み物の id>","title":"…","chars":3127}`。gzip した読み物の JSON（下の「読み物」）。確かめて断るものは `bad_request`、合計が 32 MiB を超えるときは `quota_exceeded`、SD カードの空きが足りなければ `no_space` |
| `weather_icon` | 460,808 バイト | `{"name":"101-87x58","w":77,"h":58,"bytes":13406}`（消したものがあれば `removed`）。`name` は `<番号>-<幅>x<高さ>`（箱の大きさ）。形は上の `get_weather_icon` の `.lha` |

- **順番**：`offset` は、それまでに受け取った大きさと同じでなければ `bad_request`（受け取りは続く）。言った大きさを超えたら捨てる。
- **確かめ**：`upload_end` で大きさと SHA-256 を確かめる。違えば `hash_mismatch` で、一時ファイルを消す。
- **やめる**：`upload_abort`、別の `upload_begin`、接続の終わり、60 秒 `upload_chunk` が来ない、デーモンの再起動。受け取りは同時に 1 つだけ（`image_*` とは別に数える）。
- **断る**：知らない `kind`、上限を超える `bytes` は `bad_request`。空きが足りなければ `no_space`。

## 写真（list_photos、get_photo、delete_photo）

フォトフレームの写真（`/var/lib/lefthand/photos/`。設定の `photo` と `backlight.photo`）。足すのは `upload_*` の `kind: photo`。

```json
→ {"id":1,"cmd":"list_photos"}
← {"id":1,"ok":true,"result":{"photos":[{"id":"8e507de0d4f1d833","name":"sunset","w":800,"h":480,"bytes":768008,"sha256":"…","added":"2026-10-10T17:05:02Z","source":"brain-deck"},
     {"id":"20bdb0d8a5173ba6","name":"sunset（縦）","w":480,"h":800,"bytes":768008,"added":"…","of":"8e507de0d4f1d833"}],
     "max":10,"total_bytes":1536016,"free_bytes":47603712,"reserve_bytes":5912576,"showing":"8e507de0d4f1d833"}}
→ {"id":2,"cmd":"get_photo","photo":"8e507de0d4f1d833","offset":0}
← {"id":2,"ok":true,"result":{"photo":"8e507de0d4f1d833","offset":0,"total":768008,"data":"TEhJMSAD4AE…"}}
→ {"id":3,"cmd":"delete_photo","photo":"20bdb0d8a5173ba6"}
← {"id":3,"ok":true,"result":{"deleted":"20bdb0d8a5173ba6","count":1}}
```

- 並びは足した順。`get_photo` は 1 回に 96 KiB まで（`length` で小さくできる）。`total` までくり返して読む。
- 出している写真を消すと、写真をやめて元の画面に戻す。もとの写真を消しても、縦向きに切り抜き直した写真は残る（縦向きだけの写真として出す）。
- ない id は `not_found`、id の形が違えば `bad_request`。

## 手書きメモ（list_memos、get_memo、delete_memo）

手書きメモ（widget: memo）の線（`/var/lib/lefthand/memo/<id>.json`）。id は `m1`、`m2` …（作った順）。Brain のデータが正。

```json
→ {"id":1,"cmd":"list_memos"}
← {"id":1,"ok":true,"result":{"memos":[{"id":"m1","created":"…","updated":"…","pages":2,"strokes":37,"bytes":5120}]}}
→ {"id":2,"cmd":"get_memo","memo":"m1","offset":0}
← {"id":2,"ok":true,"result":{"memo":"m1","offset":0,"total":5120,"data":"eyJpZCI6Im0xIiw…"}}
→ {"id":3,"cmd":"delete_memo","memo":"m1"}
← {"id":3,"ok":true,"result":{"deleted":"m1"}}
```

- `get_memo` は、メモの JSON（下の形）を 96 KiB ずつ base64 で返す。`total` までくり返して読む。書いていない線も含む。
- メモの JSON：`{"id":"m1","created":…,"updated":…,"pages":[{"w":776,"h":410,"strokes":[{"w":4,"p":[x0,y0,x1,y1,…]},{"w":20,"e":true,"p":[…]}]}]}`。`w`、`h` はページの大きさ（ドット）、線の `w` は太さ、`e` は消しゴム（紙の色で塗る）、`p` は点の列。描き方は `internal/ink`（丸いペン先を点と点のあいだに動かした跡）。
- ない id は `not_found`。

## 読み物（list_reads、delete_read）

読み物のアプリ（widget: reader）の読み物（`/var/lib/lefthand/reader/<id>.json.gz`）。足すのは `upload_*` の `kind: read`（brain-deck read add）。

```json
→ {"id":1,"cmd":"list_reads"}
← {"id":1,"ok":true,"result":{"reads":[{"id":"9e1f…","title":"蜘蛛の糸","author":"芥川龍之介","kind":"aozora","source":"kumono_ito_aozora.zip",
     "bytes":5163,"chars":3127,"added":"…","pos":{"b":5,"c":120},"progress":0.31}],"total_bytes":5163,"limit_bytes":33554432,"free_bytes":…,"reserve_bytes":…}}
→ {"id":2,"cmd":"delete_read","read":"9e1f…"}
← {"id":2,"ok":true,"result":{"deleted":"9e1f…"}}
```

読み物の JSON（gzip する。`readdoc.go` の `ReadDoc`）：

```json
{"format":1,"title":"蜘蛛の糸","author":"芥川龍之介","kind":"aozora","source":"…","flow":"book","blocks":[
  {"t":"h","l":2,"r":[{"s":"一"}]},
  {"t":"p","i":0,"r":[{"s":"　ある日の事でございます。"},{"s":"御釈迦様","ruby":"おしゃかさま"},{"s":"は"},{"s":"強調","em":true},{"s":"12","tcy":true}]},
  {"t":"pb"},
  {"t":"info","r":[{"s":"底本：「芥川龍之介全集2」ちくま文庫、筑摩書房"}]}]}
```

- ブロック：`p`（段落）、`h`（見出し。`l` は 1〜3）、`pb`（改ページ）、`info`（底本などの情報。少し薄く描く）。`i` は字下げ（文字の数、すべての行）。
- 文字の続き：`s`（文字）、`ruby`、`em`（傍点）、`tcy`（縦中横）。`flow` が `article` なら、段落のあいだを 1 行空ける。
- 上限：1 つ 8 MiB（gzip）、戻して 32 MiB、ブロック 20 万。題名は 1〜200 文字。
- 消すと、しおりも消す。開いていれば一覧に戻る。

## タッチのキャリブレーション（calibrate）

```json
→ {"id":1,"cmd":"calibrate","mode":"start","kind":"touch9"}
← {"id":1,"ok":true,"result":{"status":{"state":"target","point":1,"points":9,"gen":3},"calibration":{"source":"config","config":{"min_x":226,"max_x":3938,"min_y":3803,"max_y":384,"swap_xy":false}}}}
→ {"id":2,"cmd":"calibrate"}
← {"id":2,"ok":true,"result":{"status":{"state":"off","result":"saved","points":9,"before":{"mean":12.5,"max":30.1},"after":{"mean":1.6,"max":3.2},"gen":3},
     "calibration":{"source":"calibration","config":{…},"file":{"version":1,"affine":[…],"w":800,"h":480,"points":[{"screen":[80,48],"raw":[612,3460]},…],"before":{…},"after":{…},…}}}}
→ {"id":3,"cmd":"calibrate","mode":"reset"}
← {"id":3,"ok":true,"result":{"reset":true,"calibration":{"source":"config",…}}}
```

- `mode`：`start`（`kind` は `touch` で 5 点、`touch9` で 9 点）、`cancel`（保存せずにやめる）、`reset`（保存したものをやめ、設定の min と max に戻す。ファイルは `.prev` に）、`status`（省略したとき）。
- `status.state`：`target`（十字を押している。`point` は今の十字、1 から）、`verify`（確かめの画面。`before` と `after` は十字と押した位置のずれ、ドット）、`off`（`result` は最後に終わったときの `saved`、`cancelled`、`timeout`）。`gen` は始めるたびに増える。
- `calibration.source`：`calibration`（保存したもの。`file` は touch-cal.json の中身）か `config`。
- `affine`：X = a0·x + a1·y + a2、Y = a3·x + a4·y + a5（x、y はパネルの生の座標、X、Y は向き 0 の 800×480 の画面のドット）。
- 端末モードのあいだは `start` を断る（`conflict`）。

## ファイル（list_files、get_file、delete_file）

ファイルエクスプローラ（widget: files。`files.go`）と同じ、見られる場所（設定の `files.roots`）のファイル。`path` は絶対パス（`..` やシンボリックリンクを通るものは `not_allowed`）。

```json
→ {"id":1,"cmd":"list_files","path":""}
← {"id":1,"ok":true,"result":{"path":"","entries":[{"name":"ホーム","path":"/home/user","dir":true,"size":0,"mtime":"…"}],"free_bytes":…,"total_bytes":…}}
→ {"id":2,"cmd":"list_files","path":"/home/user","by_size":true}
← {"id":2,"ok":true,"result":{"path":"/home/user","root":"ホーム","delete":true,"put":true,"parent":"","entries":[
     {"name":"src","path":"/home/user/src","dir":true,"size":1048576,"mtime":"…"},{"name":"a.txt","path":"/home/user/a.txt","size":12,"mtime":"…"}],
     "free_bytes":…,"total_bytes":…}}
→ {"id":3,"cmd":"get_file","path":"/home/user/a.txt","offset":0}
← {"id":3,"ok":true,"result":{"path":"/home/user/a.txt","offset":0,"total":12,"data":"…"}}
→ {"id":4,"cmd":"delete_file","path":"/home/user/a.txt"}
← {"id":4,"ok":true,"result":{"deleted":"/home/user/a.txt"}}
```

- `list_files`：`path` が空なら場所の一覧。`by_size` で大きい順（フォルダは中の合計。8 秒までで、終わらなければ `partial`）。2000 個まで（超えたら `truncated`）。フォルダが先、名前順。`link` はシンボリックリンク（たどらない）。`parent` が空なら、上は場所の一覧。`delete` は消せる場所、`put` は足せる場所。
- `get_file`：96 KiB ずつ base64。`total` までくり返す。
- `delete_file`：`delete: true` の場所だけ。フォルダは空のときだけ。写真、メモ、読み物のデータは、その係を通して消す（一覧と索引も直す）。データの中の索引などのファイルとフォルダは消せない。
- 足すのは `upload_*` の `kind: file`。`upload_begin` に `path`（置くフォルダ）と `name`（ファイルの名前）を書く。`put` の場所だけ（データの場所には足せない。写真と読み物はそれぞれの kind で）。同じ名前があれば断る。64 MiB まで。SD カードの空きを 64 MiB 残す。
- 見られないもの：`/proc` `/sys` `/dev` `/run`、`/boot`、`/root`、`/etc/shadow` `/etc/gshadow` `/etc/sudoers` `/etc/sudoers.d` `/etc/ssh`、名前が `.ssh` と `.gnupg` のフォルダ。
- 誤り：場所の外、見られないもの、消せない場所は `not_allowed`、ないものは `not_found`。

## 同期（set_sync）

`brain-deck sync` が、始めるときと終わったときに送る（`sync.go`）。Brain は、同期しているあいだ状態の帯に「同期中」と出し、終わったら最後に同期した時刻を出す（失敗があれば橙）。

```json
→ {"id":1,"cmd":"set_sync","state":"start","source":"brain-deck"}
← {"id":1,"ok":true,"result":{"running":true,"started":"…"}}
→ {"id":2,"cmd":"set_sync","state":"done","ok":false,"summary":"4 件を行った、1 件が失敗","steps":[{"name":"時刻","ok":true},{"name":"天気","ok":false,"note":"…"}],"source":"brain-deck"}
← {"id":2,"ok":true,"result":{"running":false,"last":{"at":"…","ok":false,"summary":"…","steps":[…]}}}
```

- `get_status` の `sync`：`{"running":false,"last":{…}}`。最後の結果は `sync.json` に残る。`done` が 10 分来なければ「同期中」をやめる。
- `set_time` の結果の `rtc`：書き戻す前の内蔵 RTC と送られた時刻の差 `{"at":…,"offset_ms":-1500,"since_write_sec":172800,"source":"brain-deck"}`（RTC を読めるときだけ）と、`rtc_write`（書き戻すか）。`get_status` の `rtc.log` に新しい 30 回。

## 上書きの値（get_overrides、set_override、clear_overrides）

Brain の設定のアプリで変えた値は、設定ファイルを書き換えず、`/var/lib/lefthand/overrides.json` に置く（`overrides.go`。docs/config.md の「上書きの値」）。デーモンは設定ファイルの値に上書きを重ねて使う。`brain-deck overrides` と設定 GUI が使う。

```json
→ {"id":1,"cmd":"get_overrides"}
← {"id":1,"ok":true,"result":{"overrides":[{"key":"backlight.level","label":"明るさ","value":3,"shown":"3","file":"起動したときの明るさ","used":true}],
   "keys":["backlight.level","backlight.dim_after","backlight.offline.dim_after","backlight.photo","lid.close"]}}
→ {"id":2,"cmd":"set_override","key":"lid.close","value":"none","source":"brain-deck"}
← {"id":2,"ok":true,"result":{"overrides":[…],"keys":[…]}}
→ {"id":3,"cmd":"clear_overrides","keys":[],"source":"gui"}
← {"id":3,"ok":true,"result":{"cleared":["backlight.level","lid.close"],"overrides":[],"keys":[…]}}
```

| 項目 | 内容 |
| --- | --- |
| `key` | 上書きの名前（`keys` のどれか） |
| `label`、`shown`、`file` | 画面に出す名前、上書きの値、設定ファイルの値（どれも日本語の文） |
| `value` | 上書きの値（ファイルの形。数、文字列、真偽） |
| `used`、`note` | 使っているか。false なら、ほかの項目と合わないので使っていない（`note` に理由） |

- `set_override`：`value` を確かめて置き、すぐ反映する（自動減光とふただけを反映する。割り当ては変わらないので、押しているキーは離さない）。知らない名前や範囲の外の値は `bad_request`。
- `clear_overrides`：`keys` の上書きを消す。`keys` が空ならすべて。`cleared` は消したもの。
- 設定ファイルを変えても（`set_config`、`put_config_text`）、上書きは残る。

## 更新、再起動、電源、ログ（ssh を使わない保守）

README の「Brain の保守」。lefthand が systemd のサービスとして動いていないとき（`-unit` も `/proc/self/cgroup` も分からない）は、`update_apply`、`update_rollback`、`restart` が `internal_error` になる。

### 更新のファイルと、確かめた結果

`upload_*`（kind: `update`）で、`tools/mkupdate` が作る tar.gz を送る。中身は `manifest.json`、`lefthand`、`gadget-setup.sh` の 3 つだけ（ほかのものがあれば断る）。

```json
{"format":1,"version":"hwtest-b","built":"2026-10-09T09:03:00Z",
 "files":{"lefthand":{"sha256":"…","bytes":8206241},"gadget-setup.sh":{"sha256":"…","bytes":18832}}}
```

`upload_end` は、`<data-dir>/update/<stage>/` に展開して確かめ、次を返す（まだ何も入れ替えていない）。

```json
← {"id":118,"ok":true,"result":{"stage":"9f2a4c1d7e60","action":"update","version":"hwtest-b","current_version":"hwtest-a",
   "bin_changed":true,"has_gadget":true,"gadget_changed":true,"rebind":"yes",
   "rebind_reasons":["the HID report descriptor or attributes differ (want keyboard)"],"warnings":[]}}
```

| 確かめること | 合わなければ |
| --- | --- |
| manifest の format、ファイルごとの SHA-256 と大きさ、lefthand と gadget-setup.sh の両方があるか | `update_rejected` |
| 新しい lefthand を `-version` で実行し、manifest の版と同じか（`update` が 1 以上か） | `update_rejected` |
| 新しい lefthand の `-check-gadget`（ディスクリプタが gadget-setup.sh と同じか）、`bash -n` | `update_rejected` |
| 新しい lefthand の `-check`（今の設定を読めるか）。警告は `warnings` に入れる | `update_rejected` |
| gadget-setup.sh が変わるなら `GADGET_CHECK=1` で実行し、付け直しが要るか（`rebind`：`no`、`yes`、`maybe`） | — |

`update_rejected` のときは、展開したものを消す。何も変えていない。

### update_apply、update_discard、update_rollback

```json
→ {"id":119,"cmd":"update_apply","stage":"9f2a4c1d7e60"}
← {"id":119,"ok":true,"result":{"stage":"9f2a4c1d7e60","action":"update","version":"hwtest-b","from":"hwtest-a","helper":"lefthand-update","confirm":true}}
```

- **update_apply**：`systemd-run --unit=lefthand-update` で係（`lefthand -update-apply <stage>/plan.json`）を起動して返事をする。係は 1 秒待ってからサービスを止めるので、このシリアルはそのあと切れる。PC はつなぎ直し、`update_status` の `phase` が `waiting_confirm` になったら、新しいデーモンに `update_confirm` を送る。
- **update_discard**：確かめ終えた更新を入れ替えずに消す（`{"discarded":true}`）。
- **update_rollback**：`.prev`（前の版）を、受け取ったものと同じように確かめて、同じ形の結果を返す（`action` は `rollback`）。続けて `update_apply` を送る。前の版が `-version` に答えない古い版なら `legacy: true` で、係は起動の知らせと `update_confirm` を待たず、10 秒動き続けることだけを確かめる。前の版がなければ `not_found`。
- **断る**：端末モードのあいだ、USB の形の切り替えの途中、係が動いているあいだは `conflict`。

### update_confirm、update_status

```json
→ {"id":5,"cmd":"update_confirm","stage":"9f2a4c1d7e60","source":"brain-deck"}
← {"id":5,"ok":true,"result":{"confirmed":true,"version":"hwtest-b"}}
→ {"id":6,"cmd":"update_status"}
← {"id":6,"ok":true,"result":{"version":"hwtest-b","helper":"inactive","unit":"lefthand.service","bin":"/usr/local/bin/lefthand",
   "gadget":"/usr/local/sbin/lefthand-gadget-setup","has_prev":true,
   "state":{"stage":"9f2a4c1d7e60","action":"update","phase":"done","done":true,"ok":true,"from":"hwtest-a","to":"hwtest-b","rebound":false,…}}}
```

- **update_confirm**：受けたデーモンの PID を `<stage>/confirmed` に書く。係は、それが新しいデーモンの PID と同じなら確認できたとする（古いデーモンが受けたものは数えない）。
- **state**（`<data-dir>/update/state.json`）の `phase`：`starting_helper`、`stopping`、`installing`、`rebinding`、`starting`、`waiting_ready`、`waiting_confirm`、`done`、`rolling_back`、`rolled_back`（失敗して前の版に戻した）、`failed`（restart で起動しなかった）。`done` が true で終わり、`ok` が結果、`message` が理由、`rebound` は USB を付け直したか（gadget-setup.sh がガジェットを変えたとき）。
- **時間**：新しいデーモンが `-ready-file` に起動を書くまで 45 秒、そのあと `update_confirm` まで 90 秒、そのあと 2 秒動き続けること。どれかが満たされなければ、係は前の版（と前の `.prev`）に戻し、gadget-setup.sh を入れ替えていれば前のものを実行し直して、サービスを起動する。

### restart、power

```json
→ {"id":7,"cmd":"restart","usb":true}
← {"id":7,"ok":true,"result":{"stage":"3b0f9e2d1c44","action":"restart","usb":true,"helper":"lefthand-update"}}
→ {"id":8,"cmd":"power","action":"reboot"}
← {"id":8,"ok":true,"result":{"action":"reboot"}}
```

- **restart**：係が、サービスを止めて起動し、起動の知らせを待つ。`usb` なら、止めているあいだに `GADGET_UNBIND_ONLY=1` で切り離して gadget-setup.sh で付け直す（USB の形は保つ）。結果は `update_status` の `state`（同じ `stage`）。
- **power**：`action` は `reboot` か `poweroff`。返事のあと 0.5 秒で `systemctl --no-block <action>`。係が動いているあいだは `conflict`。

### get_logs

```json
→ {"id":9,"cmd":"get_logs","lines":100}
← {"id":9,"ok":true,"result":{"text":"2026-10-09T18:06:31+09:00 brain lefthand[3999]: …","cursor":"s=…"}}
→ {"id":10,"cmd":"get_logs","cursor":"s=…"}
```

`journalctl -u <デーモンのユニット> -u lefthand-update.service -o short-iso`。`cursor` を渡すと、そのあとのものだけ（追いかけるとき）。`lines` は 2000 まで、`text` は 200 KiB まで（長ければ新しいほうを残す）。

### get_battery_log

```json
→ {"id":11,"cmd":"get_battery_log","lines":3}
← {"id":11,"ok":true,"result":{"csv":"time,uptime_s,vbatt_mv,vbatt_avg_mv,vbus_mv,percent,plugged,backlight,event\n2026-10-11T09:00:00+09:00,…\n"}}
```

電池だけで動かしたときの記録（`<data-dir>/battery-log.csv` と、大きくなって移した `.1` を古い順につないだもの。`batteryguard.go`）。`lines` を書けば最後のその行数だけ（見出しは付ける）。設定の `battery.log` が false なら記録しない。

| 列 | 内容 |
| --- | --- |
| `time`、`uptime_s` | 読んだ時刻と、起動してからの秒 |
| `vbatt_mv`、`vbatt_avg_mv` | 電池の電圧（3 回読んだ中央値）と、5 回の移動平均（残りの見積もりに使う） |
| `vbus_mv` | USB の 5V |
| `percent` | 見積もった残り |
| `plugged` | 5V があるか |
| `backlight` | 画面の明るさ（PW-SH2 は 0〜7。読めなければ -1） |
| `event` | `start`（起動）、`unplugged`、`plugged`、`warn`、`critical`、`poweroff-countdown`、`poweroff-cancelled`、`poweroff` |

行を書くのは、5V がないあいだは 30 秒ごと、5V があるあいだは起動したときとつないだときだけ。

## 設定のテキスト（get_config_text、put_config_text）

`get_config`、`set_config` と違い、設定ファイルを**テキストのまま**（コメントも）読み書きする。brain-deck の `config get`、`config put`。

```json
→ {"id":11,"cmd":"get_config_text","prev":false}
← {"id":11,"ok":true,"result":{"text":"# 手で書いたコメント\nhid_device: /dev/hidg0\n…","sha256":"…","path":"/etc/lefthand/config.yaml","prev":false}}
→ {"id":12,"cmd":"put_config_text","text":"…","restart":false,"dry_run":false,"source":"brain-deck"}
← {"id":12,"ok":true,"result":{"saved":"/etc/lefthand/config.yaml","previous":"/etc/lefthand/config.yaml.prev","warnings":[],"restarting":false,"status":{…}}}
```

- **検証**：`validate` と同じ。誤りは `invalid_config`（`problems` に場所付き。YAML の文法の誤りも）。何も書かない。
- **保存**：`text` をそのままファイルにする（一時ファイル、fsync、rename）。前の版は `.prev`。そのあと再起動せずに反映し、反映に失敗したらファイルも戻す（`apply_failed`）。
- **再起動が要る項目**（`hid_device`、`keyboard`、`touch.device`、`touch` の有無、`display`）が変わるとき：`restart` が false なら `needs_restart`（`problems` にその項目）で何も書かない。true なら書いてから、係でデーモンを再起動する（結果に `restarting: true` と `stage`）。新しいデーモンが起動しなければ、係が `.prev` に戻して起動し直す（`state.phase` は `rolled_back`）。
- **dry_run**：検証だけ。`{"valid":true,"warnings":[…],"needs_restart":false}`。
- `prev` が true なら `.prev` を読む。なければ `not_found`。

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
| `not_found` | Todo：その ID の項目がない。get_image：その id の画像がない。set_presence：その id の在席ランプが設定にない。update_apply：確かめ終えた更新がない。update_rollback：前の版がない |
| `conflict` | Todo：項目が、送った `rev` のあとに変わった。何も変えていない。更新、再起動：端末モード、USB の切り替え、ほかの更新の途中 |
| `quota_exceeded` | 画像：置いてある画像との合計が上限（16 MiB）を超える。何も書いていない |
| `no_space` | 画像：保存すると SD カードの空きが足りなくなる。何も書いていない |
| `hash_mismatch` | 画像、upload_*：受け取った中身の SHA-256 が、始めるときに言ったものと違う。一時ファイルを消した |
| `update_rejected` | 更新：受け取ったものを確かめて断った（版、組、設定が読めない、など）。何も変えていない |
| `needs_restart` | put_config_text：再起動が要る項目が変わった（`restart` を付けていない）。何も書いていない |

brain-deck は、エラーの種類によって終了コードを変える（README の「終了コード」）。

GUI 側だけのエラー：`timeout`（返事が来ない。既定 5 秒、保存は 15 秒）、`closed`（切れた）、`write_failed`。
