#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
BOTDIR="$HOME/KalshiArbo/kalshiarbo"
while true; do
  clear
  NOW=$(date -u '+%Y-%m-%d %H:%M:%S UTC')
  AGENT=$(systemctl is-active kalshi-agent.service 2>/dev/null || true)
  if ss -ltnp 2>/dev/null | grep -q ':8085'; then BOT='PAPER RUNNING'; else BOT='NOT LISTENING'; fi
  echo '============================================================'
  echo '              KALSHIARBO DEVELOPMENT — LIVE'
  echo '============================================================'
  printf '%s\n\n' "$NOW"
  printf 'AGENT       %s\n' "$AGENT"
  printf 'BOT         %s\n' "$BOT"
  printf 'LIVE        HARD-BLOCKED BY AGENT\n\n'

  echo 'CURRENT WORK'
  echo 'PolyArbPro -> Kalshi parity validation'
  echo

  echo 'CURRENT JOB / LIVE PROGRESS'
  if [ -s "$REPO/docs/agent/progress.txt" ]; then
    tail -n 14 "$REPO/docs/agent/progress.txt"
  else
    JOB=$(python3 - <<'PY' 2>/dev/null
import json, pathlib
p=pathlib.Path.home()/"kalshi-diagnostic/control/job.json"
try:
 d=json.loads(p.read_text()); print(d.get("id","no job")); print(" -> ".join(x.get("name","?") for x in d.get("actions",[])))
except Exception: print("Waiting for job")
PY
)
    echo "$JOB"
    echo 'Waiting for next agent progress event...'
  fi
  echo

  echo 'LAST RESULT'
  if [ -s "$REPO/docs/agent/latest.txt" ]; then
    grep -aE 'JOB_ID=|RESULT=|ACTION|PASS|FAIL|GO_TEST|GO_BUILD|BACKUP|STATUS' "$REPO/docs/agent/latest.txt" 2>/dev/null | tail -n 8
  else
    echo 'No completed agent result available yet.'
  fi
  echo

  echo 'WHAT I FOUND / WHAT I AM DOING'
  if [ -s "$REPO/control/dev_status.txt" ]; then
    cat "$REPO/control/dev_status.txt"
  else
    echo 'FOUND: forensic lead counter currently conflates signals with executed entries.'
    echo 'DOING: separating signal -> accepted -> executed -> hedged -> timeout.'
  fi
  echo

  echo 'REAL TRADING STATE'
  F="$REPO/docs/latest.txt"
  if [ -s "$F" ]; then
    grep -aE 'Starting bankroll|Current bankroll|Net realized P&L|Return |Maximum drawdown|Lead attempts|Hedge events|Locked events|Completed cycles|Profitable cycles|Losing cycles|Timeout exits|Recorded fees' "$F" 2>/dev/null | tail -n 14
    echo 'NOTE: Lead attempts above may still be signal-count until auditor fix lands.'
  else
    echo 'Waiting for forensic snapshot...'
  fi
  echo
  echo 'NEXT'
  if [ -s "$REPO/control/next.txt" ]; then cat "$REPO/control/next.txt"; else echo 'Trace rejected lead signals against original PolyArbPro gates.'; fi
  echo
  echo 'Refresh 2s | Ctrl+C closes monitor only'
  sleep 2
done
