#!/usr/bin/env bash
set -Eeuo pipefail

BOTDIR="$HOME/KalshiArbo/kalshiarbo"
BOT="$BOTDIR/kalshiarbo"
REPO="$HOME/kalshi-diagnostic"
PORT=8085
DURATION=2400
STAMP="$(date -u +%Y%m%d_%H%M%S)"
RUN="$HOME/strict_arb_validation_$STAMP"
BOTLOG="$RUN/bot_console.txt"
STATUS="$RUN/audit_status.txt"
EVENTS="$RUN/audit_events.jsonl"
REPORT="$RUN/latest_report.txt"
PUBLISH_PID=""
BOT_PID=""

mkdir -p "$RUN"
cd "$BOTDIR"

# Hard safety: PAPER ONLY and only one KalshiArbo process.
pkill -INT -x kalshiarbo 2>/dev/null || true
sleep 2
pkill -TERM -x kalshiarbo 2>/dev/null || true
sleep 1
rm -f .bot.lock

if ss -ltnp | grep -q ":$PORT "; then
  echo "ERROR: port $PORT is occupied:" >&2
  ss -ltnp | grep ":$PORT " >&2
  exit 1
fi

# Confirm this is the strict build before touching session state.
grep -q 'STRICT COMPLETE-SET ARBITRAGE ONLY' strategy/signal.go || { echo 'ERROR: strict signal patch missing'; exit 1; }
grep -q 'strictPairArbPreflight' strategy/trader.go || { echo 'ERROR: strict preflight patch missing'; exit 1; }
grep -q 'LIVE pair execution blocked' strategy/trader.go || { echo 'ERROR: live safety block missing'; exit 1; }

# Preserve prior paper/session artifacts and start a clean validation session.
mkdir -p "$RUN/pre_run_state"
for f in position_state.json trades.jsonl signals.jsonl; do
  [ -e "$f" ] && cp -a "$f" "$RUN/pre_run_state/$f" || true
  rm -f "$f"
done

cd "$REPO"
git pull --rebase >/dev/null 2>&1 || true

PAIR_SIZE="${PAIR_ARB_TRADE_SIZE_USD:-5}"
START_BAL="${PAPER_START_BALANCE:-20}"
MAX_SPEND="$(python3 - <<PY
print(2.0*float('$PAIR_SIZE'))
PY
)"

publish() {
  local final="${1:-0}"
  {
    echo '=================================================='
    echo 'KALSHIARBO STRICT COMPLETE-SET PAPER VALIDATION'
    echo "RUN=$STAMP"
    echo "UTC=$(date -u --iso-8601=seconds)"
    echo "PORT=$PORT"
    echo "MODE=PAPER"
    echo "DURATION_SEC=$DURATION"
    echo "PAIR_TRADE_SIZE_USD=$PAIR_SIZE"
    echo "PAIR_MAX_SPEND_USD=$MAX_SPEND"
    echo "PAPER_START_BALANCE=$START_BAL"
    echo "BOT_PID=${BOT_PID:-none}"
    if [ -n "${BOT_PID:-}" ] && kill -0 "$BOT_PID" 2>/dev/null; then echo 'BOT_PROCESS=ALIVE'; else echo 'BOT_PROCESS=STOPPED'; fi
    echo '=================================================='
    echo
    echo '===== LIVE ECONOMIC SCOREBOARD ====='
    cat "$STATUS" 2>/dev/null || echo 'Auditor starting...'
    echo
    echo '===== RELEVANT BOT EVIDENCE ====='
    grep -aEi 'STRICT executable arbitrage|strict preflight|PAIR ARB|\[PAIR|PAIR RESIDUAL|SELL PAIR|risk controls|position limit|locked|profit|P&L|timeout|abort|orderbook.*fail|WARN|ERROR|FATAL' "$BOTLOG" 2>/dev/null | tail -n 350 || true
    echo
    echo '===== RECENT INDEPENDENT AUDITOR EVENTS ====='
    tail -n 180 "$EVENTS" 2>/dev/null || true
    if [ "$final" = '1' ]; then
      echo
      echo '===== FINAL RUN FILES ====='
      wc -l -c "$BOTLOG" "$EVENTS" 2>/dev/null || true
    fi
  } > "$REPORT"

  cd "$REPO"
  mkdir -p docs
  cp "$REPORT" docs/latest.txt
  git add docs/latest.txt
  git commit -m "Strict arb validation $STAMP update" >/dev/null 2>&1 || true
  git push >/dev/null 2>&1 || true
  cd "$BOTDIR"
}

cleanup() {
  set +e
  [ -n "${PUBLISH_PID:-}" ] && kill "$PUBLISH_PID" 2>/dev/null || true
  if [ -n "${BOT_PID:-}" ] && kill -0 "$BOT_PID" 2>/dev/null; then
    kill -INT "$BOT_PID" 2>/dev/null || true
    sleep 3
    kill -TERM "$BOT_PID" 2>/dev/null || true
  fi
  publish 1
}
trap cleanup EXIT INT TERM

cd "$BOTDIR"
stdbuf -oL -eL "$BOT" -port "$PORT" > >(tee -a "$BOTLOG" >/dev/null) 2>&1 &
BOT_PID=$!
sleep 7

if ! kill -0 "$BOT_PID" 2>/dev/null; then
  echo 'ERROR: bot exited during startup' | tee "$STATUS"
  exit 1
fi
if grep -aEqi 'LIVE TRADE|LIVE MODE|mode[^A-Za-z]+LIVE' "$BOTLOG"; then
  echo 'ERROR: LIVE MODE DETECTED — STOPPED' | tee "$STATUS"
  exit 1
fi
if ! grep -aEqi 'PAPER|PAPER TRADE|PAPER MODE' "$BOTLOG"; then
  echo 'ERROR: PAPER MODE NOT CONFIRMED — STOPPED' | tee "$STATUS"
  exit 1
fi
if ! ss -ltnp | grep -q ":$PORT "; then
  echo "ERROR: dashboard is not listening on $PORT" | tee "$STATUS"
  exit 1
fi

publish 0
(
  while kill -0 "$BOT_PID" 2>/dev/null; do
    sleep 60
    publish 0 || true
  done
) &
PUBLISH_PID=$!

clear || true
echo 'Starting independent strict-arbitrage auditor...'
echo "Dashboard: http://localhost:$PORT/dashboard"
echo 'Published audit: https://githubcos.github.io/kalshi-diagnostic/latest.txt'
echo

python3 "$REPO/strict_arb_live_auditor.py" \
  --bot-log "$BOTLOG" \
  --status "$STATUS" \
  --events "$EVENTS" \
  --duration "$DURATION" \
  --interval 2 \
  --max-spend "$MAX_SPEND" \
  --start-bankroll "$START_BAL"

# Auditor completed normally. Stop bot before final publication.
kill "$PUBLISH_PID" 2>/dev/null || true
PUBLISH_PID=""
kill -INT "$BOT_PID" 2>/dev/null || true
sleep 3
kill -TERM "$BOT_PID" 2>/dev/null || true
BOT_PID=""

# Save new paper state and permanent evidence.
for f in position_state.json trades.jsonl signals.jsonl; do
  [ -e "$BOTDIR/$f" ] && cp -a "$BOTDIR/$f" "$RUN/$f" || true
done

publish 1
cd "$REPO"
cp "$REPORT" "docs/strict_arb_audit_${STAMP}.txt"
cp "$EVENTS" "docs/strict_arb_events_${STAMP}.jsonl"
grep -aEi 'STRICT executable arbitrage|strict preflight|PAIR ARB|\[PAIR|PAIR RESIDUAL|SELL PAIR|risk controls|position limit|locked|profit|P&L|timeout|abort|orderbook.*fail|WARN|ERROR|FATAL' "$BOTLOG" > "docs/strict_arb_bot_relevant_${STAMP}.txt" || true
git add "docs/strict_arb_audit_${STAMP}.txt" "docs/strict_arb_events_${STAMP}.jsonl" "docs/strict_arb_bot_relevant_${STAMP}.txt" docs/latest.txt
git commit -m "Publish completed strict arb validation $STAMP" >/dev/null 2>&1 || true
git push >/dev/null 2>&1 || true

trap - EXIT INT TERM
echo
echo 'STRICT ARBITRAGE PAPER VALIDATION COMPLETE'
echo 'https://githubcos.github.io/kalshi-diagnostic/latest.txt'
