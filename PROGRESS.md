# 進み具合（2026-10-11〜2026-10-13）

フェーズ R2 → G → H → I → P3 → J → R3 → K1〜K7 の順に行った。各フェーズの記録は REPORT.md の「追記：フェーズ …」。

| フェーズ | 状態 | 本番への反映 |
| --- | --- | --- |
| R2 RTC の書き戻し | 終わり（8fdc3c3） | 反映した（config put、01:34） |
| G フォトフレーム | 終わり | デーモンを反映した（9ecd42e28a7c）。設定には入れない |
| H ペンタブレットと手書きメモ | 終わり | デーモンを反映した（4eee939c30d3）。設定には入れない |
| I 読み物 | 終わり | デーモンを反映した（36b6bf49ec22）。設定には入れない |
| P3 同期 | 終わり | デーモンを反映した（64d8bdb36652）。設定には入れない |
| J ファイルと Brain の端末 | 終わり | デーモンを反映した（8dd144e9196d）。設定には入れない |
| R3 充電の調査と、新しい機能を本番の設定に | 終わり | デーモン ae7af4cdef23（MaxPower 500、充電していないの表示）、設定 config/current.yaml（ae72b5a） |
| タッチのキャリブレーション | 終わり | デーモン 6a74e57ee33d、設定 config/current.yaml（ff1c7b4） |
| K1 ふた、設定のアプリ、ヘルプ | 終わり（ふたと再起動のボタンも実機で確かめた） | デーモン a30ea9e82e31、設定 config/current.yaml（d79939f） |
| K2 日本語の入力（uim-fep と Anthy） | 終わり（欄はキーの注入で実機で確かめた。Brain のシェルのログインはユーザーが確かめる） | デーモン e338b6168600、設定 config/current.yaml（e338b61）。Brain の /etc/profile.d/lefthand-ime.sh |
| K3 バックアップと復元、PC ごとの切り替え（と、ふたの描き直し、タブレットの形、日本語入力の調整） | 終わり（ふた、タブレットの形、日本語入力、設定の並びはユーザーが実機で確かめた。バックアップと復元とプロファイルは試験のデーモンで確かめた） | デーモン 8a09379808bb、PC の brain-deck 8a09379808bb。設定は変えていない |
| K4 起動を速くする（2026-10-12） | 終わり（起動中の画面と起動の時間のログ。段階 1〜3 を本番に適用し、84.2 → 59.3 秒、lefthand の準備 86.1 → 69.8 秒） | デーモン d18b7d39e6c6、boot-tune.sh の段階 1〜3、lefthand.service（getty@tty1 を待たない。前は `.prev`） |
| K5 自分でアプリを足す仕組み（2026-10-12） | 終わり（試験のデーモンで確かめた。上限を決めたとおりに直した。実機で見つけた 2 つの不具合を直した） | デーモン d18b7d39e6c6。本番の設定には入れない（例は config/apps-example.yaml） |
| K6 起動をさらに速くする（2026-10-12 夕〜夜） | 終わり（lefthand が USB の構成を待たない。段階 4〜6 を本番に適用し、起動の合計 69.2 → 55.2 秒、PC へのキー 約 71 → 約 28 秒） | デーモン 7a97a93e5130、boot-tune.sh の段階 4〜6、lefthand.service（ethernet_gadget を待たない。前は `.prev2`） |
| K7 実行ファイルを小さくする（2026-10-12 夜〜10-13） | 終わり（13.4 → 7.9 MB。読み物の本文の文字を別のファイルに。起動の時間は変わらなかった。本番のヘルプの明朝体はユーザーが確かめた） | デーモン 5c08f95ede53（2 回に分けて更新）、`/usr/local/share/lefthand/` に読み物の文字 |
| USB のネットワークの方式とスマホのイーサネット テザリング（2026-10-15） | 終わり（NCM と phone の MAC で、Xperia 1 IV の Android 15 からインターネットに出られた。RNDIS と ECM と MAC の切り替えも作った） | デーモン 3a63395abf17（brain-deck run も。gadget-setup.sh は 476947020804 で入れた）、PC の brain-deck。Brain の gadget.env に `NET_MAC=phone`、PC の NetworkManager は両方の名前に当てた（ユーザー）。REPORT.md の「USB のネットワークの方式」 |

## 次にすること

- 充電：Brainux では充電されない。充電は純正のファームウェアで行う。内蔵の充電器を動かす案は保留（作るならカーネルのドライバとして、Brain Hackers で見てもらう）。電池は電圧で守る（3.5 V で知らせ、3.4 V で電源を切る。本番の設定）。
- 決めてほしいこと：tty1 の画面が Enter まで描き直されないままでよいか（lefthand が終わるときに描き直させる案がある。REPORT.md の「K4 と K5 を本番に入れ」）。起動中の lefthand の約 12 秒は K6 で調べた（今は exec から準備まで約 9 秒。udev の coldplug と journald との取り合い）。
- 分かっていないこと：段階 6 の再起動で一度、起動がルートのマウントより前で止まった（REPORT.md のフェーズ K6）。もう一度起きたら記録する。
- 次：Brain の中で同期を動かすか（REPORT.md の見積もり、3〜4 日）。スマホ（Xperia）で、ずっと phone の形のままテザリングできるかと、つないだまま Brain のキーボードで打てるかを、ユーザーに確かめてもらう。

## 本番の状態（最後に確かめたとき）

- 2026-10-16 21:45：デーモン 99b412788322（`.prev` は同じコードの 99b412788322+dirty。その前は 3a63395abf17）。PC の brain-deck も 99b412788322。設定のアプリの「タブレットの形の本体キー」の行と上書き `lid.tablet_keys` を消した（PW-SH2 はタブレットの形のあいだキーを送らない。REPORT.md の「タブレットの形のキー」）。本番の上書きはなし。
- 2026-10-13 01:12（フェーズ K7 の終わり）：デーモン 5c08f95ede53（`.prev` 7a97a93e5130）。読み物の本文の文字は `/usr/local/share/lefthand/reader-24.lrf.gz`、`reader-32.lrf.gz`（`-check-fonts` で ok）。設定は config/current.yaml（profiles なし、例のアプリなし）。PC の `~/.local/bin/brain-deck` は 4a1cd8cc04ba（「.prev はそのまま」の表示を直した版。デーモンの側は 5c08f95 と同じ）。
- Brain：boot-tune.sh の段階 1〜6 を適用（`/etc/lefthand/boot-tune.state`）。ly は止めている（Brain の画面のログインは、lefthand を止めて tty1 で Enter）。`/etc/systemd/system/lefthand.service` は getty@tty1 も ethernet_gadget も待たない版（段階 3 の前は `.prev`、K6 の前は `.prev2`）。Brain の ~/boot に contrib/boot の写し。
- 戻すとき：`brain-deck update --rollback` は 7a97a93e5130（文字を埋め込んだ版。今のユニットと組み合わせてよい）。d18b7d39e6c6 以前に戻すなら、先にユニットを `.prev2` に戻し、段階 4 も戻す。8a09379808bb には月の表示のタップで固まる不具合がある（docs/architecture/16-limitations.md）。
- 2026-10-13 にこのファイルを直したとき、Brain は PC につながっていなかった（`brain-deck status` で見つからない、ssh も届かない）ので、上はフェーズ K7 の記録のまま。
- 試験のユニット、タイマー、/tmp/lhu は残していない。
- 本番に反映しない、と決めたフェーズ：なし。

## 再開するときの注意

- **新しいセッションは、最初に [docs/architecture.md](docs/architecture.md)（仕組みの資料。2026-10-12 に作った）を読む**。全体像、USB、入力の流れ、割り当ての重なり、データ、通信、保守、起動、安全をまとめ、ソースの地図（「〇〇を変えたいときはどのファイルを見るか」）と用語集もある。コードを変えたら、関係する章も直す。
- go は `../toolchain/go/bin`（PATH にない）。
- 試験のユニットは時間制限付き、`-data-dir` は /tmp の下。試験の前後で本番の lefthand.service を確かめる。
- 試験の手順：/tmp/prodcmp.sh（本番の画面と -render-png の比較。背景画像は DATADIR=/tmp/prodimg に本番の images を写して使う。K3 の終わりに /tmp/prodimg は消した）、/tmp/brainreq.py（1 つのリクエストを送る）は /tmp にあるだけ（セッションが切れたら作り直す）。
- `/dev/input/event1` にキーやスイッチのイベントを書く試験は、K3 では自動の許可で止められた。ふたやキーの試験はユーザーに操作を頼む。
- 試験では、`config put` と上書きの反映も行う（K1 で、反映のときだけ止まる不具合を本番で見つけた）。試験の前後で `uptime` も見る（電池の見張りが電源を切っていても気づけるように）。
