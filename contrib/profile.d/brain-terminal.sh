# PC（Linux）用。Brain の端末モードでログインしたとき、端末の大きさ（行と桁）を Brain に聞いて、stty で合わせる。
# シリアルでは端末の大きさが PC に伝わらず、vi や less が 24 行 80 桁のまま描くため。
#   sudo install -m 0644 brain-terminal.sh /etc/profile.d/
# BRAIN_TERMINAL=1 は brain-terminal-getty@.service が付ける。ほかの端末では何もしない。
# Brain は CSI 18 t に「ESC [ 8 ; 行 ; 桁 t」と答える。答えを待つので、ログインが 0.5 秒ほど遅れる。
if [ "${BRAIN_TERMINAL:-}" = 1 ] && [ -t 0 ] && [ -t 1 ]; then
  brain_terminal_size() {
    _bt_old=$(stty -g) || return 0
    stty raw -echo min 0 time 5
    printf '\033[18t'
    _bt_r=$(dd bs=1 count=24 2>/dev/null)
    stty "$_bt_old"
    _bt_r=${_bt_r#*\[8;}
    _bt_rows=${_bt_r%%;*}
    _bt_cols=${_bt_r#*;}
    _bt_cols=${_bt_cols%%t*}
    case "$_bt_rows$_bt_cols" in
    '' | *[!0-9]*) ;;
    *) stty rows "$_bt_rows" cols "$_bt_cols" ;;
    esac
    unset _bt_old _bt_r _bt_rows _bt_cols
  }
  brain_terminal_size
fi
