# brain-deck 用カーネルのビルド

Brainux 標準のカーネルは、USB ガジェットの HID と ACM に対応していない。
brain-deck（lefthand）を動かすには、この 2 つを有効にしたカーネルをビルドし、SD カードの `zImage` を差し替える。

ここでは、作者が実際に動かしているカーネルと同じものを作る手順を書く。

| 項目 | 値 |
| --- | --- |
| 対象機種 | Sharp Brain PW-SH2（他の機種では未確認） |
| カーネル | `6.1.70-ga9f534c7f53e` |
| ソース | [brain-hackers/linux-brain](https://github.com/brain-hackers/linux-brain) のコミット `a9f534c7f53e` |
| ビルド環境 | [brain-hackers/buildbrain](https://github.com/brain-hackers/buildbrain) |
| 標準からの変更 | `CONFIG_USB_CONFIGFS_F_HID=y`、`CONFIG_USB_CONFIGFS_ACM=y` の 2 項目だけ |

## 目次

1. [必要なもの](#必要なもの)
2. [ソースの取得](#ソースの取得)
3. [設定](#設定)
4. [ビルド](#ビルド)
5. [SD カードの zImage の差し替え](#sd-カードの-zimage-の差し替え)
6. [動作確認](#動作確認)
7. [注意点](#注意点)

## 必要なもの

- **ホスト**：x86_64 の Linux。作者は Ubuntu 22.04 でビルドした。
  macOS では、buildbrain の Docker イメージを使う（[Docker でビルドする](#docker-でビルドする)）。macOS での手順は未確認。
- **パッケージ**（Linux で直接ビルドするとき）：

  ```sh
  sudo apt install build-essential bison flex bc lzop libssl-dev libncurses-dev gcc-arm-linux-gnueabi git
  ```

  作者のクロスコンパイラは `arm-linux-gnueabi-gcc` 11.4.0（Ubuntu 22.04 のもの）。
- **ディスク**：約 3 GB。カーネルのソースとビルド結果で約 2 GB、git の履歴で約 0.3 GB。
  Docker を使うときは、さらにイメージの分が要る。
- **時間**：4 コアの PC で、ビルドに約 7 分半。
- **SD カードリーダー**：SD カードの起動用パーティションを、PC から書き換えるため。

## ソースの取得

buildbrain を取得し、サブモジュールは linux-brain だけを取る。
buildbrain のほかのサブモジュール（buildroot など）は、カーネルのビルドには要らない。

```sh
git clone https://github.com/brain-hackers/buildbrain.git
cd buildbrain
git submodule update --init --depth 1 linux-brain
```

linux-brain を、コミット `a9f534c7f53e` に合わせる。
buildbrain のコミット `2e954a9`（2026 年 10 月時点の master）は、linux-brain をこのコミットに固定しているので、上のコマンドでこのコミットになる。
buildbrain の master が先に進んでいるときは、次のように合わせる。

```sh
cd linux-brain
git fetch --depth 1 origin a9f534c7f53e7ee6bb2cb3581fd471cc964f1601
git checkout --detach a9f534c7f53e7ee6bb2cb3581fd471cc964f1601
cd ..
```

確認：

```sh
git -C linux-brain log --oneline -1
# a9f534c7f arm: brain: enable NCM
```

> このコミットは、linux-brain の `brain` ブランチの履歴には含まれていない（2026 年 10 月時点）。
> そのため `git clone` だけでは取得されないが、上のようにコミットのハッシュを指定すれば取得できる。

## 設定

まず、Brain の標準の設定を作る。buildbrain のディレクトリで実行する。

```sh
export ARCH=arm CROSS_COMPILE=arm-linux-gnueabi-
make ldefconfig      # linux-brain/.config ができる（brain_defconfig の内容）
```

次に、HID と ACM を有効にする。方法は 2 つある。どちらでも同じ `.config` になる。

### 方法 1：設定の差分ファイルを合成する（おすすめ）

brain-deck のリポジトリにある [`kernel/brain-deck.config`](../kernel/brain-deck.config) を、`merge_config.sh` で `.config` に重ねる。
`/path/to/brain-deck` は、brain-deck のリポジトリの場所に置き換える。

```sh
cd linux-brain
scripts/kconfig/merge_config.sh .config /path/to/brain-deck/kernel/brain-deck.config
cd ..
```

`Value of CONFIG_USB_CONFIGFS_F_HID is redefined by fragment ...` と表示されるのは、正常。
`not in final .config` と表示されたら、その項目は有効になっていない。ARCH や linux-brain のコミットを確かめる。

### 方法 2：menuconfig で手で変える

```sh
make lmenuconfig
```

次の場所にある 2 つの項目を、`[*]` にする。

```
Device Drivers  --->
  [*] USB support  --->
    <*> USB Gadget Support  --->
      <*> USB Gadget functions configurable through configfs
        [*] Abstract Control Model (CDC ACM)      ← CONFIG_USB_CONFIGFS_ACM
        [*] HID function                          ← CONFIG_USB_CONFIGFS_F_HID
```

保存して終了する。保存先のファイル名は `.config` のままにする。

### 設定の確認

```sh
grep -E 'CONFIG_USB_CONFIGFS_(F_HID|ACM)=|CONFIG_USB_F_(HID|ACM)=|CONFIG_USB_U_SERIAL=' linux-brain/.config
```

次の 5 行が出ればよい。

```
CONFIG_USB_F_ACM=y
CONFIG_USB_U_SERIAL=y
CONFIG_USB_F_HID=y
CONFIG_USB_CONFIGFS_ACM=y
CONFIG_USB_CONFIGFS_F_HID=y
```

標準の設定との差分は、この 5 行と、`# CONFIG_U_SERIAL_CONSOLE is not set` の 1 行だけになる（`U_SERIAL_CONSOLE` は、ACM を有効にすると選べるようになる項目で、無効のまま）。

## ビルド

```sh
make lbuild
```

`Kernel: arch/arm/boot/zImage is ready` と出れば完了。できあがるファイル：

| ファイル | 内容 | 使うか |
| --- | --- | --- |
| `linux-brain/arch/arm/boot/zImage` | カーネル本体 | SD カードに置く |
| `linux-brain/arch/arm/boot/dts/imx28-pwsh2.dtb` | PW-SH2 のデバイスツリー | 使わない（SD カードのものをそのまま使う） |
| `linux-brain/System.map` など | デバッグ用 | 使わない |

バージョン文字列を確かめる。

```sh
cat linux-brain/include/config/kernel.release
# 6.1.70-ga9f534c7f53e
```

`-dirty` が付いたり、ハッシュが違ったりするときは、[注意点](#バージョン文字列とモジュール) を読む。

### Docker でビルドする

buildbrain の Docker イメージ（Debian 13、`gcc-arm-linux-gnueabi` 入り）を使う。
`make docker-kernel` は、設定を標準に戻してからビルドするので使わない。代わりに、次のようにコンテナの中で上の手順を実行する。

```sh
make docker-build    # イメージ buildbrain-builder:local を作る（初回だけ）

docker run --rm -it --platform linux/amd64 \
  -v "$PWD":/work -v /path/to/brain-deck:/brain-deck:ro -w /work \
  buildbrain-builder:local bash -lc '
    export ARCH=arm CROSS_COMPILE=arm-linux-gnueabi-
    make ldefconfig &&
    (cd linux-brain && scripts/kconfig/merge_config.sh .config /brain-deck/kernel/brain-deck.config) &&
    make lbuild'
```

コンパイラが作者のもの（GCC 11）と違うので、できる `zImage` は同じバイナリにはならない。設定の項目は同じになる。
macOS では、ソースを大文字と小文字を区別しないファイルシステムに置くと、linux-brain のチェックアウトが壊れ、バージョンに `-dirty` が付くおそれがある（未確認）。
メモリ不足でビルドが落ちるときは、Docker Desktop のメモリの割り当てを増やす。

## SD カードの zImage の差し替え

Brainux の SD カードの 1 つ目のパーティション（FAT）に、`zImage` と各機種の `.dtb` が入っている。
Brain の上では、これが `/boot` に読み取り専用でマウントされている。

ここでは、Brain の電源を切って SD カードを抜き、PC のカードリーダーで書き換える。
差し替えるのは `zImage` だけ。`imx28-pwsh2.dtb` はそのまま使う。

```sh
# PC に挿した SD カードの 1 つ目のパーティションをマウントする（/dev/sdX1 は環境に合わせる）
sudo mount /dev/sdX1 /mnt
cd /mnt

# 元の zImage を残す（すでに zImage.orig があるときは、上書きしないよう別の名前にする）
sudo cp -p zImage zImage.orig

# 新しい zImage を置く
sudo cp /path/to/buildbrain/linux-brain/arch/arm/boot/zImage zImage
sync
cd / && sudo umount /mnt
```

PC 側にも、元の `zImage` のコピーを取っておくと安心。

### 元に戻す

SD カードを PC に挿し、`zImage.orig` を `zImage` に戻す。

```sh
sudo mount /dev/sdX1 /mnt
sudo cp -p /mnt/zImage.orig /mnt/zImage
sync && sudo umount /mnt
```

### Brain の上で差し替える（未確認）

作者はこの方法を試していない。`/boot` を書き込めるようにマウントし直せば、同じ操作ができるはず。

```sh
sudo mount -o remount,rw /boot
sudo cp -p /boot/zImage /boot/zImage.orig
sudo cp ~/zImage /boot/zImage     # 先に scp などで zImage を送っておく
sync
sudo mount -o remount,ro /boot
```

新しいカーネルで起動できないと、Brain の上からは戻せない。そのときは PC のカードリーダーで戻す。

## 動作確認

SD カードを Brain に戻して起動し、Brain の上で確かめる。

```sh
uname -a
# Linux brain 6.1.70-ga9f534c7f53e #1 PREEMPT <ビルドした日時> armv5tejl GNU/Linux
```

ビルドした日時になっていれば、新しいカーネルで起動している。

```sh
zcat /proc/config.gz | grep -E 'CONFIG_USB_CONFIGFS_(F_HID|ACM)='
# CONFIG_USB_CONFIGFS_ACM=y
# CONFIG_USB_CONFIGFS_F_HID=y

ls /sys/kernel/config/usb_gadget/
# Brainux 標準のガジェット（eth）などが表示される
```

`/dev/hidg0` は、ガジェットに HID の関数を足したときにできる。
brain-deck をインストールすると（[README のインストール](../README.md#インストール)）、起動時に `gadget-setup.sh` が HID と ACM を足すので、次のファイルができる。

```sh
ls -l /dev/hidg0 /dev/ttyGS0 /dev/ttyGS1
```

カーネルが HID に対応していないと、`gadget-setup.sh` は `functions/hid.usb0 を作成できません` と表示する。

## 注意点

### バージョン文字列とモジュール

カーネルのバージョン文字列（`uname -r`）は、linux-brain のコミットから作られる（`CONFIG_LOCALVERSION_AUTO=y`）。
コミット `a9f534c7f53e` を、追跡しているファイルを変えずにビルドすれば、`6.1.70-ga9f534c7f53e` になる。
作者の Brainux（`brainux_version` が `2026-03-25-024518`）では、Brain の `/lib/modules/6.1.70-ga9f534c7f53e` がそのまま使われている。

次のときは、バージョン文字列が変わる。

- linux-brain を別のコミットにした
- `arch/arm/configs/brain_defconfig` など、追跡しているファイルを書き換えた（`-dirty` が付く）

バージョン文字列が変わると、Brain の `/lib/modules/<新しいバージョン>` が無いので、モジュールを読み込めなくなる。
PW-SH2 の標準の設定でモジュールになっているのは、`tsc2007`、`spi-gpio`、`spi-bitbang`、`rtc-ds1307`、`netfs`、`fscache`、`cachefiles` の 7 つだけ。
作者の環境では、これらは使っていない。

対処：ビルドしたモジュールを Brain に入れる。

```sh
# PC で（buildbrain のディレクトリ）
export ARCH=arm CROSS_COMPILE=arm-linux-gnueabi-
make -C linux-brain INSTALL_MOD_PATH="$PWD/modstage" INSTALL_MOD_STRIP=1 modules_install
rm -f modstage/lib/modules/*/build modstage/lib/modules/*/source
tar -C modstage --owner=0 --group=0 -czf modules.tar.gz lib
scp modules.tar.gz brain:

# Brain で
sudo tar -C / -xzf ~/modules.tar.gz
```

設定の変更（方法 1 または方法 2）は `.config` だけを変え、追跡しているファイルを変えないので、バージョン文字列は変わらない。

### 対象機種

PW-SH2 でだけ確かめた。ほかの機種（PW-SH1、PW-SH3〜7、PW-A7200 など）は、同じ linux-brain から作るので動く可能性はあるが、未確認。

### 対応するブランチとの関係

上の手順は、作者が実際に動かしているカーネルを再現するためのもの。
linux-brain の新しいコミットでビルドするときも、`kernel/brain-deck.config` はそのまま使えるはず（未確認）。そのときはバージョン文字列が変わるので、モジュールも入れ直す。
