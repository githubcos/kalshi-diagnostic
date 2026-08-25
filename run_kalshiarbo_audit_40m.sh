#!/usr/bin/env bash
set -Eeuo pipefail

BOTDIR="$HOME/KalshiArbo/kalshiarbo"
BOT="$BOTDIR/kalshiarbo"
PORT=8085
REPO="$HOME/kalshi-diagnostic"
DURATION_MIN=40
PUBLISH_EVERY_MIN=2
STAMP="$(date -u +%Y%m%d_%H%M%S)"
RUN="$HOME/kalshiarbo_audit_$STAMP"
CONSOLE="$RUN/console.txt"
STATUS="$RUN/status.txt"
mkdir -p "$RUN"

cd "$BOTDIR"
pkill -INT -x kalshiarbo 2>/dev/null || true
sleep 2
pkill -TERM -x kalshiarbo 2>/dev/null || true
sleep 1
rm -f .bot.lock

if ss -ltnp | grep -q ":$PORT "; then
  echo "ERROR: port $PORT is occupied" | tee "$CONSOLE"
  ss -ltnp | grep ":$PORT " | tee -a "$CONSOLE"
  exit 1
fi

if [ ! -d "$REPO/.git" ]; then
  git clone https://github.com/githubcos/kalshi-diagnostic.git "$REPO"
fi

publish() {
  local elapsed="$1"
  {
    echo "KALSHIARBO 40-MIN PAPER AUDIT"
    echo "RUN=$STAMP"
    echo "UTC=$(date -u --iso-8601=seconds)"
    echo "PORT=$PORT"
    echo "TARGET_DURATION_MIN=$DURATION_MIN"
    echo "ELAPSED_MIN=$elapsed"
    echo "BOT_PID=${PID:-not_started}"
    if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then echo "PROCESS=ALIVE"; else echo "PROCESS=STOPPED"; fi
    echo
    echo "===== STATUS ====="
    cat "$STATUS" 2>/dev/null || true
    echo
    echo "===== CONSOLE ====="
    cat "$CONSOLE" 2>/dev/null || true
    echo
    echo "===== RECENT BOT LOG EVENTS ====="
    find "$BOTDIR/logs" -type f -mmin -50 -name '*.log' -print0 2>/dev/null | while IFS= read -r -d '' f; do
      echo "--- $f ---"
      grep -Ei 'arbitrage|arb|signal|candidate|reject|skip|yes|no|hedge|fill|filled|order|locked|profit|loss|balance|position|orphan|stale|timeout|fee|slip|cooldown|abort|error|warn|paper' "$f" 2>/dev/null | tail -n 1200 || true
    done
  } > "$RUN/latest.txt"

  cd "$REPO"
  git pull --rebase >/dev/null 2>&1 || true
  mkdir -p docs
  cp "$RUN/latest.txt" docs/latest.txt
  git add docs/latest.txt
  git commit -m "KalshiArbo audit $STAMP minute $elapsed" >/dev/null 2>&1 || true
  git push >/dev/null 2>&1 || true
  cd "$BOTDIR"
}

echo "START_UTC=$(date -u --iso-8601=seconds)" > "$STATUS"
echo "MODE_REQUESTED=PAPER" >> "$STATUS"
echo "PORT=$PORT" >> "$STATUS"

stdbuf -oL -eL "$BOT" -port "$PORT" > >(tee -a "$CONSOLE") 2>&1 &
PID=$!
echo "PID=$PID" >> "$STATUS"
sleep 6

if ! kill -0 "$PID" 2>/dev/null; then
  echo "STARTUP=FAILED" >> "$STATUS"
  publish 0
  exit 1
fi

if grep -Eqi 'LIVE TRADE|LIVE MODE|mode[^A-Za-z]+LIVE' "$CONSOLE"; then
  echo "SAFETY=LIVE_DETECTED_STOPPED" >> "$STATUS"
  kill -INT "$PID" 2>/dev/null || true
  publish 0
  exit 1
fi

if grep -Eqi 'PAPER|PAPER TRADE|PAPER MODE' "$CONSOLE"; then
  echo "SAFETY=PAPER_CONFIRMED" >> "$STATUS"
else
  echo "SAFETY=PAPER_NOT_CONFIRMED_STOPPED" >> "$STATUS"
  kill -INT "$PID" 2>/dev/null || true
  publish 0
  exit 1
fi

if ss -ltnp | grep -q ":$PORT "; then
  echo "PORT_STATUS=LISTENING" >> "$STATUS"
else
  echo "PORT_STATUS=NOT_LISTENING" >> "$STATUS"
fi

publish 0

echo
echo "=================================================="
echo "KALSHIARBO PAPER AUDIT RUNNING"
echo "Dashboard: http://localhost:$PORT/dashboard"
echo "Live published log: https://githubcos.github.io/kalshi-diagnostic/latest.txt"
echo "=================================================="

for minute in $(seq 1 "$DURATION_MIN"); do
  sleep 60
  if kill -0 "$PID" 2>/dev/null; then
    echo "MINUTE_$minute=ALIVE" >> "$STATUS"
  else
    echo "MINUTE_$minute=PROCESS_DIED" >> "$STATUS"
    publish "$minute"
    break
  fi
  if (( minute % PUBLISH_EVERY_MIN == 0 )); then publish "$minute"; fi
  echo "Audit minute $minute/$DURATION_MIN"
done

if kill -0 "$PID" 2>/dev/null; then
  kill -INT "$PID" 2>/dev/null || true
  sleep 3
  kill -TERM "$PID" 2>/dev/null || true
fi

echo "END_UTC=$(date -u --iso-8601=seconds)" >> "$STATUS"
echo "AUDIT=COMPLETE" >> "$STATUS"
publish "$DURATION_MIN"

echo
echo "AUDIT COMPLETE"
echo "Published: https://githubcos.github.io/kalshi-diagnostic/latest.txt"
