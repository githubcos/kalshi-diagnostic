#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
STATUS="$REPO/control/project_status.json"
F="$REPO/docs/latest.txt"
PROGRESS="$REPO/docs/agent/progress.txt"
RESULT="$REPO/docs/agent/latest.txt"

bar() {
  local pct=${1:-0} width=30 filled empty
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

while true; do
  clear
  NOW=$(date -u '+%H:%M:%S UTC')
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

  echo '============================================================'
  echo '          KALSHIARBO DEVELOPMENT — LIVE CONTROL ROOM'
  echo '============================================================'
  printf '%-11s %s\n' 'TIME' "$NOW"
  printf '%-11s %s\n' 'AGENT' "$AGENT"
  printf '%-11s %s\n' 'BOT' "$BOT"
  printf '%-11s %s\n' 'LIVE MONEY' 'HARD-BLOCKED'
  echo

  echo 'PROJECT COMPLETION (engineering estimate)'
  bar "$OVERALL"; echo
  [ -n "$BASIS" ] && echo "$BASIS"
  echo

  echo 'CURRENT PHASE'
  echo "$PHASE"
  bar "$PHASEP"; echo
  echo

  echo 'CURRENT PATCH / INVESTIGATION'
  printf 'Name:   %s\n' "$PATCH"
  printf 'State:  %s\n' "$PATCHSTATE"
  bar "$PATCHP"; echo
  echo

  echo 'WHAT I AM DOING RIGHT NOW'
  printf '%s\n' "$DOING" | fold -s -w 62
  echo
  echo 'WHY'
  printf '%s\n' "$WHY" | fold -s -w 62
  echo

  echo 'AGENT JOB — LIVE'
  if [ -s "$PROGRESS" ]; then
    tail -n 10 "$PROGRESS" | sed 's/^/  /'
  elif [ -s "$RESULT" ]; then
    grep -aE 'JOB_ID=|RESULT=|GO_TEST|GO_BUILD|BACKUP|FAIL|PASS' "$RESULT" 2>/dev/null | tail -n 6 | sed 's/^/  /'
  else
    echo '  Waiting for first agent event...'
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
    printf 'Bankroll        %s -> %s\n' "${START:-?}" "${CURR:-?}"
    printf 'P&L / Return    %s / %s\n' "${PNL:-?}" "${RET:-?}"
    printf 'Signals seen    %s  (legacy auditor count)\n' "${SIG:-?}"
    printf 'Completed       %s\n' "${CYC:-?}"
    printf 'Hedges          %s\n' "${HED:-?}"
    printf 'Timeouts        %s\n' "${TMO:-?}"
    printf 'Fees            %s\n' "${FEES:-?}"
  else
    echo 'No forensic snapshot yet.'
  fi
  echo

  echo 'NEXT'
  printf '%s\n' "$NEXT" | fold -s -w 62
  echo
  echo 'Refresh: 2s | Ctrl+C closes monitor only'
  sleep 2
done
