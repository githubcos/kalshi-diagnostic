#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
STATUS="$REPO/control/project_status.json"
F="$REPO/docs/latest.txt"
PROGRESS="$REPO/docs/agent/progress.txt"
RESULT="$REPO/docs/agent/latest.txt"
TMP="/tmp/kalshi_monitor_body.$$"
PREV_HASH=""
trap 'rm -f "$TMP"' EXIT

bar() {
  local pct=${1:-0} width=28 filled empty
  (( pct < 0 )) && pct=0
  (( pct > 100 )) && pct=100
  filled=$((pct*width/100)); empty=$((width-filled))
  printf '['
  printf '%*s' "$filled" '' | tr ' ' '#'
  printf '%*s' "$empty" '' | tr ' ' '-'
  printf '] %3d%%' "$pct"
}

jget() {
  python3 - "$STATUS" "$1" <<'PY' 2>/dev/null
import json,sys
try:
 d=json.load(open(sys.argv[1])); v=d
 for k in sys.argv[2].split('.'):
  v=v[k]
 if isinstance(v,list): print('\n'.join(str(x) for x in v))
 else: print(v)
except Exception: pass
PY
}

render_body() {
  AGENT=$(systemctl is-active kalshi-agent.service 2>/dev/null || true)
  if ss -ltnp 2>/dev/null | grep -q ':8085'; then BOT='PAPER RUNNING'; else BOT='NOT LISTENING'; fi

  OVERALL=$(jget overall_percent); OVERALL=${OVERALL:-0}
  PHASE=$(jget phase); PHASE=${PHASE:-'Kalshi parity engineering'}
  PHASEP=$(jget phase_percent); PHASEP=${PHASEP:-0}
  PATCH=$(jget current_patch); PATCH=${PATCH:-'No active patch'}
  PATCHP=$(jget patch_percent); PATCHP=${PATCHP:-0}
  PATCHSTATE=$(jget patch_state); PATCHSTATE=${PATCHSTATE:-'IDLE'}
  DOING=$(jget doing)
  WHY=$(jget why)
  NEXT=$(jget next)
  BASIS=$(jget overall_basis)

  {
    printf 'AGENT       %s\n' "$AGENT"
    printf 'BOT         %s\n' "$BOT"
    printf 'LIVE MONEY  HARD-BLOCKED\n\n'

    echo 'PROJECT COMPLETION'
    bar "$OVERALL"; echo
    [ -n "$BASIS" ] && printf '%s\n' "$BASIS" | fold -s -w 58
    echo

    echo 'CURRENT PHASE'
    printf '%s\n' "$PHASE" | fold -s -w 58
    bar "$PHASEP"; echo
    echo

    echo 'CURRENT PATCH / INVESTIGATION'
    printf 'PATCH: %s\n' "$PATCH" | fold -s -w 58
    printf 'STATE: %s\n' "$PATCHSTATE"
    bar "$PATCHP"; echo
    echo

    echo 'DOING NOW'
    printf '%s\n' "$DOING" | fold -s -w 58
    echo
    echo 'WHY'
    printf '%s\n' "$WHY" | fold -s -w 58
    echo

    echo 'AGENT — LIVE'
    if [ -s "$PROGRESS" ]; then
      tail -n 8 "$PROGRESS" | sed 's/^/  /' | fold -s -w 58
    elif [ -s "$RESULT" ]; then
      grep -aE 'JOB_ID=|RESULT=|GO_TEST|GO_BUILD|BACKUP|FAIL|PASS' "$RESULT" 2>/dev/null | tail -n 5 | sed 's/^/  /'
    else
      echo '  Waiting for agent event...'
    fi
    echo

    echo 'PAPER ECONOMICS'
    if [ -s "$F" ]; then
      START=$(grep -a 'Starting bankroll' "$F" | tail -1 | awk '{print $NF}')
      CURR=$(grep -a 'Current bankroll' "$F" | tail -1 | awk '{print $NF}')
      PNL=$(grep -a 'Net realized P&L' "$F" | tail -1 | awk '{print $NF}')
      RET=$(grep -a 'Return ' "$F" | tail -1 | awk '{print $NF}')
      SIG=$(grep -a 'Lead attempts' "$F" | tail -1 | awk '{print $NF}')
      CYC=$(grep -a 'Completed cycles' "$F" | tail -1 | awk '{print $NF}')
      HED=$(grep -a 'Hedge events' "$F" | tail -1 | awk '{print $NF}')
      TMO=$(grep -a 'Timeout exits' "$F" | tail -1 | awk '{print $NF}')
      FEES=$(grep -a 'Recorded fees' "$F" | tail -1 | awk '{print $NF}')
      printf 'BANKROLL   %s -> %s\n' "${START:-?}" "${CURR:-?}"
      printf 'P&L        %s   RETURN %s\n' "${PNL:-?}" "${RET:-?}"
      printf 'SIGNALS    %s   COMPLETED %s\n' "${SIG:-?}" "${CYC:-?}"
      printf 'HEDGES     %s   TIMEOUTS  %s\n' "${HED:-?}" "${TMO:-?}"
      printf 'FEES       %s\n' "${FEES:-?}"
    else
      echo 'No forensic snapshot yet.'
    fi
    echo

    echo 'NEXT'
    printf '%s\n' "$NEXT" | fold -s -w 58
  } > "$TMP"
}

# Hide cursor while monitor is open; restore it on exit.
printf '\033[?25l'
trap 'printf "\033[?25h"; rm -f "$TMP"' EXIT INT TERM

while true; do
  render_body
  HASH=$(sha256sum "$TMP" | awk '{print $1}')

  # Redraw ONLY when actual status/economics/progress changes.
  # This removes the phone-terminal flicker caused by clear every 2 seconds.
  if [ "$HASH" != "$PREV_HASH" ]; then
    PREV_HASH="$HASH"
    printf '\033[2J\033[H'
    echo '==========================================================='
    echo '            KALSHIARBO DEVELOPMENT — LIVE'
    echo '==========================================================='
    printf 'UPDATED     %s\n\n' "$(date -u '+%H:%M:%S UTC')"
    cat "$TMP"
    echo
    echo 'Updates instantly when status changes | Ctrl+C monitor only'
  fi

  sleep 1
done
