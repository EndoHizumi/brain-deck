#!/bin/sh
# Brain の「ビルド」のセル（F14）に割り当てる例。PC の OS のショートカットから実行する。
# プロジェクトをビルドし、結果を Brain のテキストのタイル build に出す（実行中 → 成功 / 失敗(終了コード) と最後の行）。
#   PROJECT  ビルドするディレクトリ（既定 ~/src/myapp）
# ショートカットから動かすと PATH が短いことがあるので、brain-deck は ~/.local/bin を前に足して探す。
PATH=$HOME/.local/bin:/usr/local/bin:$PATH
cd "${PROJECT:-$HOME/src/myapp}" || exit 1
# ログは ~/.cache/brain-deck/build.log に残す（タイルには最後の 1 行だけ出る）
mkdir -p "$HOME/.cache/brain-deck"
exec brain-deck exec --result build --limit 15m -- make -j"$(nproc 2>/dev/null || echo 2)" \
  >"$HOME/.cache/brain-deck/build.log" 2>&1
