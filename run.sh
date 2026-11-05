#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")"
binary=${VMQ_BINARY:-vmqfox-api-linux-amd64}
case "$binary" in vmqfox-api-linux-amd64|vmqfox-api-linux-arm64) ;; *) exit 1;; esac
pending() { [[ -f .update-state.json ]] && grep -Eq '"phase":"(restarting|installing)"' .update-state.json; }
rollback() {
  [[ -f "$binary.previous" ]] || { echo '更新备份不存在，无法恢复' >&2; return 1; }
  mv -f -- "$binary.previous" "$binary"
  printf '%s\n' '{"phase":"rolled_back","progress":0,"message":"新版本未能正常启动，已自动恢复原程序"}' > .update-state.recovery
  chmod 600 .update-state.recovery
  mv -f -- .update-state.recovery .update-state.json
  rm -f -- .update-started
}
if pending; then
  if [[ -f .update-started || ! -f "$binary" ]]; then rollback; else touch .update-started; fi
else
  rm -f -- .update-started
fi
"./$binary" &
child=$!
trap 'kill -TERM "$child" 2>/dev/null || true' TERM INT
(
  if [[ -f .update-started ]]; then
    for ((i=0;i<30;i++)); do
      if ! pending; then rm -f -- .update-started; exit 0; fi
      kill -0 "$child" 2>/dev/null || exit 0
      sleep 1
    done
    echo '新版本启动超时，终止后由守护服务恢复原程序' >&2
    kill -KILL "$child" 2>/dev/null || true
  fi
) &
watchdog=$!
result=0
wait "$child" || result=$?
if kill -0 "$child" 2>/dev/null; then wait "$child" || true; fi
kill "$watchdog" 2>/dev/null || true
wait "$watchdog" 2>/dev/null || true
exit "$result"
