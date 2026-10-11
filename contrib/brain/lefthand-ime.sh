# lefthand（brain-deck）：端末モードで Brain 自身にログインしたとき、日本語入力（uim-fep）を通してシェルを開く。
# /etc/profile.d/lefthand-ime.sh に置く（install.sh が置く）。
#
# lefthand は /bin/login -p に BRAIN_TERMINAL=1、BRAIN_IME=<uim の入力方式>、LIBUIM_USER_SCM_FILE=<lefthand の uim.scm> を
# 渡す。この 3 つがそろっているときだけ、uim-fep の中で $SHELL を開き直す。ssh やコンソールからのログイン、
# すでに uim-fep の中（UIM_FEP_PID）、対話でないシェルでは何もしない。設定の ime.terminal: false なら lefthand が渡さない。
if [ "${BRAIN_TERMINAL-}" = 1 ] && [ -n "${BRAIN_IME-}" ] && [ -z "${UIM_FEP_PID-}" ]; then
  lh_ime=$BRAIN_IME
  unset BRAIN_IME
  case $- in
    *i*)
      if command -v uim-fep >/dev/null 2>&1; then
        exec uim-fep -u "$lh_ime" -s lastline
      fi
      ;;
  esac
  unset lh_ime
fi
