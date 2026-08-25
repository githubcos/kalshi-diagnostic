#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
BOT="$HOME/KalshiArbo/kalshiarbo"
while true; do
  clear
  echo "════════════════════════════════════════════════════════════════════════════════"
  echo "                   KALSHI AUTONOMOUS DEVELOPMENT CONTROL ROOM"
  echo "════════════════════════════════════════════════════════════════════════════════"
  echo "UTC: $(date -u --iso-8601=seconds)"
  echo
  echo "=== SAFETY / SERVICES ==="
  printf "Agent service:   "; systemctl is-active kalshi-agent.service 2>/dev/null || true
  printf "Bot port 8085:   "; if ss -ltnp 2>/dev/null | grep -q ':8085'; then echo "LISTENING"; else echo "DOWN"; fi
  echo "LIVE trading:    HARD-BLOCKED BY AGENT"
  echo
  echo "=== CURRENT JOB FROM CHATGPT/GITHUB ==="
  cat "$REPO/control/job.json" 2>/dev/null || echo "No job.json"
  echo
  echo "=== LIVE AGENT PROGRESS ==="
  if [ -s "$REPO/docs/agent/progress.txt" ]; then
    tail -n 140 "$REPO/docs/agent/progress.txt"
  else
    echo "Waiting for agent progress..."
  fi
  echo
  echo "=== LAST COMPLETED AGENT RESULT SUMMARY ==="
  if [ -s "$REPO/docs/agent/latest.txt" ]; then
    grep -aE 'JOB_ID=|START_UTC=|END_UTC=|RESULT=|ERROR=|BACKUP=|START_PAPER|STOP_PAPER|patch|checking file|FAIL|PASS|ok[[:space:]]+github.com/polyarb' "$REPO/docs/agent/latest.txt" 2>/dev/null | tail -n 80
  else
    echo "No completed result yet."
  fi
  echo
  echo "=== LIVE PAPER BOT — IMPORTANT ECONOMIC EVENTS ==="
  if [ -f "$BOT/polyarb.log" ]; then
    grep -aEi 'SIGNAL|PAIR ARB|PAIR_LEAD|PAIR HEDGE|PAIR RESIDUAL|BUY YES|BUY NO|SELL YES|SELL NO|locked|timeout|P&L|bankroll|paper balance|fee' "$BOT/polyarb.log" 2>/dev/null | tail -n 45
  else
    echo "No bot log."
  fi
  echo
  echo "=== CURRENT FORENSIC ECONOMICS ==="
  if [ -s "$REPO/docs/latest.txt" ]; then
    grep -aE 'Bot +|Starting bankroll|Current bankroll|Net realized P&L|Return +|Maximum drawdown|Lead attempts|Hedge events|Locked events|Completed cycles|Profitable cycles|Losing cycles|Win rate|Timeout exits|Recorded fees|DIAGNOSIS|WARNING|CURRENT SAMPLE' "$REPO/docs/latest.txt" 2>/dev/null | tail -n 35
  else
    echo "No forensic snapshot."
  fi
  echo
  echo "=== RECENT CONTROL REPO ACTIVITY ==="
  git -C "$REPO" log -8 --pretty=format:'%h  %ad  %s' --date=format:'%H:%M:%S' 2>/dev/null || true
  echo
  echo
  echo "Refresh: 1 second | Ctrl+C closes ONLY this monitor"
  sleep 1
done
