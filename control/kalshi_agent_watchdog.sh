#!/usr/bin/env bash
set -euo pipefail
HB="$HOME/.kalshi_agent_heartbeat.json"
SERVICE="kalshi-agent.service"
MAX_AGE=300
now=$(date +%s)
restart=0
if ! systemctl is-active --quiet "$SERVICE"; then
  restart=1
elif [ ! -s "$HB" ]; then
  restart=1
else
  epoch=$(python3 - <<'PY' "$HB" 2>/dev/null || echo 0
import json,sys
try: print(int(float(json.load(open(sys.argv[1])).get('epoch',0))))
except Exception: print(0)
PY
)
  if [ "$epoch" -le 0 ] || [ $((now-epoch)) -gt "$MAX_AGE" ]; then restart=1; fi
fi
if [ "$restart" -eq 1 ]; then
  logger -t kalshi-agent-watchdog "agent missing/stale; restarting service"
  systemctl restart "$SERVICE"
fi
