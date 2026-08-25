#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
STATUS="$REPO/control/project_status.json"
F="$REPO/docs/latest.txt"
PROGRESS="$REPO/docs/agent/progress.txt"
RESULT="$REPO/docs/agent/latest.txt"
TELEMETRY="$REPO/docs/agent/telemetry_status.txt"
TMP="/tmp/kalshi_monitor_body.$$"
PREV_HASH=""
LAST_SYNC=0
SYNC_EVERY=5
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

tget() {
  local key="$1"
  [ -s "$TELEMETRY" ] || return 0
  grep -a "^${key}=" "$TELEMETRY" 2>/dev/null | tail -1 | cut -d= -f2-
}

iso_age() {
  python3 - "$1" <<'PY' 2>/dev/null
from datetime import datetime, timezone
import sys
s=sys.argv[1].strip()
try:
 d=datetime.fromisoformat(s.replace('Z','+00:00'))
 age=max(0,int((datetime.now(timezone.utc)-d).total_seconds()))
 print(age)
except Exception:
 print(-1)
PY
}

sync_one() {
  local remote="$1" localfile="$2"
  git -C "$REPO" show "origin/main:$remote" > "$localfile.tmp" 2>/dev/null && mv "$localfile.tmp" "$localfile"
}

sync_repo() {
  local now
  now=$(date +%s)
  (( now - LAST_SYNC < SYNC_EVERY )) && return 0
  LAST_SYNC=$now
  git -C "$REPO" fetch -q origin main 2>/dev/null || return 0
  mkdir -p "$REPO/docs/agent"
  sync_one control/project_status.json "$STATUS"
  sync_one docs/latest.txt "$F"
  sync_one docs/agent/progress.txt "$PROGRESS"
  sync_one docs/agent/latest.txt "$RESULT"
  sync_one docs/agent/telemetry_status.txt "$TELEMETRY"
}

render_body() {
  AGENT_SERVICE=$(systemctl is-active kalshi-agent.service 2>/dev/null || true)
  if ss -ltnp 2>/dev/null | grep -q ':8085'; then BOT='PAPER RUNNING'; else BOT='NOT LISTENING'; fi

  TEL_UPDATED=$(tget UPDATED_UTC)
  TEL_HEARTBEAT=$(tget AGENT_HEARTBEAT_UTC)
  TEL_STATUS=$(tget AGENT_STATUS)
  TEL_JOB=$(tget AGENT_JOB)
  LAST_JOB=$(tget LAST_JOB_ID)
  LAST_RESULT=$(tget LAST_RESULT)
  LAST_RUN=$(tget LAST_RUN_UTC)
  HB_AGE=$(iso_age "${TEL_HEARTBEAT:-}")

  if [ -n "${TEL_HEARTBEAT:-}" ] && [ "$HB_AGE" -ge 0 ] && [ "$HB_AGE" -le 20 ]; then
    TELEMETRY_HEALTH='LIVE'
  elif [ -n "${TEL_HEARTBEAT:-}" ]; then
    TELEMETRY_HEALTH="STALE (${HB_AGE}s)"
  else
    TELEMETRY_HEALTH='MISSING'
  fi

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

  # Runtime truth overrides stale descriptive wording.
  if [ "${TEL_STATUS:-}" = "working" ] && [ -n "${TEL_JOB:-}" ]; then
    RUNTIME_STATE="RUNNING"
    RUNTIME_JOB="$TEL_JOB"
  elif [ -n "${LAST_JOB:-}" ]; then
    RUNTIME_STATE="${LAST_RESULT:-IDLE}"
    RUNTIME_JOB="$LAST_JOB"
  else
    RUNTIME_STATE="${TEL_STATUS:-IDLE}"
    RUNTIME_JOB="none"
  fi

  {
    echo 'RUNTIME TRUTH'
    printf 'TELEMETRY   %s\n' "$TELEMETRY_HEALTH"
    printf 'HEARTBEAT   %s' "${TEL_HEARTBEAT:-unknown}"
    [ "$HB_AGE" -ge 0 ] 2>/dev/null && printf '  (%ss ago)' "$HB_AGE"
    echo
    printf 'AGENT       %s / %s\n' "$AGENT_SERVICE" "${TEL_STATUS:-unknown}"
    printf 'JOB         %s\n' "$RUNTIME_JOB" | fold -s -w 58
    printf 'JOB STATE   %s\n' "$RUNTIME_STATE"
    [ -n "${LAST_RUN:-}" ] && printf 'LAST RUN    %s\n' "$LAST_RUN"
    printf 'BOT         %s\n' "$BOT"
    printf 'LIVE MONEY  HARD-BLOCKED\n\n'

    echo 'PROJECT COMPLETION'; bar "$OVERALL"; echo
    [ -n "$BASIS" ] && printf '%s\n' "$BASIS" | fold -s -w 58
    echo
    echo 'CURRENT PHASE'; printf '%s\n' "$PHASE" | fold -s -w 58; bar "$PHASEP"; echo; echo
    echo 'ENGINEERING DESCRIPTION'
    printf 'PATCH: %s\n' "$PATCH" | fold -s -w 58
    printf 'DESCRIBED STATE: %s\n' "$PATCHSTATE" | fold -s -w 58
    bar "$PATCHP"; echo; echo
    echo 'DOING / FINDING'; printf '%s\n' "$DOING" | fold -s -w 58; echo
    echo 'WHY'; printf '%s\n' "$WHY" | fold -s -w 58; echo

    echo 'AGENT EVENT'
    if [ "${TEL_STATUS:-}" = "working" ] && [ -s "$PROGRESS" ]; then
      tail -n 10 "$PROGRESS" | sed 's/^/  /' | fold -s -w 58
    elif [ -n "${LAST_JOB:-}" ]; then
      printf '  LAST JOB: %s\n' "$LAST_JOB" | fold -s -w 58
      printf '  RESULT:   %s\n' "${LAST_RESULT:-unknown}"
      [ -n "${LAST_RUN:-}" ] && printf '  FINISHED: %s\n' "$LAST_RUN"
    else
      echo '  No completed job yet.'
    fi

    echo
    echo 'PAPER ECONOMICS'
    if [ -s "$F" ]; then
      START=$(grep -a 'Starting bankroll' "$F" | tail -1 | awk '{print $NF}'); CURR=$(grep -a 'Current bankroll' "$F" | tail -1 | awk '{print $NF}')
      PNL=$(grep -a 'Net realized P&L' "$F" | tail -1 | awk '{print $NF}'); RET=$(grep -a 'Return ' "$F" | tail -1 | awk '{print $NF}')
      SIG=$(grep -a 'Lead attempts' "$F" | tail -1 | awk '{print $NF}'); CYC=$(grep -a 'Completed cycles' "$F" | tail -1 | awk '{print $NF}')
      HED=$(grep -a 'Hedge events' "$F" | tail -1 | awk '{print $NF}'); TMO=$(grep -a 'Timeout exits' "$F" | tail -1 | awk '{print $NF}')
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
    echo 'NEXT ENGINEERING TARGET'; printf '%s\n' "$NEXT" | fold -s -w 58
  } > "$TMP"
}

printf '\033[?25l'
trap 'printf "\033[?25h"; rm -f "$TMP" "$STATUS.tmp" "$F.tmp" "$PROGRESS.tmp" "$RESULT.tmp" "$TELEMETRY.tmp"' EXIT INT TERM
while true; do
  sync_repo
  render_body
  HASH=$(sha256sum "$TMP" | awk '{print $1}')
  if [ "$HASH" != "$PREV_HASH" ]; then
    PREV_HASH="$HASH"
    printf '\033[2J\033[H'
    echo '==========================================================='
    echo '            KALSHIARBO DEVELOPMENT — LIVE'
    echo '==========================================================='
    printf 'SCREEN UTC  %s\n' "$(date -u '+%H:%M:%S UTC')"
    printf 'REMOTE UTC  %s\n' "${TEL_UPDATED:-unknown}"
    printf 'AUTO-SYNC   GitHub every %ss\n\n' "$SYNC_EVERY"
    cat "$TMP"
    echo
    echo 'Runtime truth comes from telemetry | Ctrl+C monitor only'
  fi
  sleep 1
done
