#!/usr/bin/env bash
set -Eeuo pipefail

PUB="$HOME/kalshi-termux-publisher"
BOTDIR="$HOME/KalshiArbo/kalshiarbo"
STAMP="$(date -u +%Y%m%d_%H%M%S)"
RUNDIR="$HOME/gate_b_$STAMP"
mkdir -p "$RUNDIR"

cd "$PUB"
git pull --rebase origin main
cp gate_b_recorder.py "$RUNDIR/gate_b_recorder.py"
chmod +x "$RUNDIR/gate_b_recorder.py"

# Safety proof: recorder never imports bot code or sends order requests.
if grep -Eq 'PlaceOrder|/portfolio/orders|POST|DELETE|PATCH' "$RUNDIR/gate_b_recorder.py"; then
  echo 'SAFETY FAIL: recorder contains order-write patterns' >&2
  exit 1
fi

BOTPID="$(pgrep -f '/home/ubuntu/KalshiArbo/kalshiarbo/kalshiarbo -port 8085' | head -1 || true)"
PORTLINE="$(ss -ltnp 2>/dev/null | grep ':8085' || true)"

{
  echo '=================================================='
  echo 'KALSHI PROFESSIONAL PAPER TEST — GATE B START'
  echo "UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "RUN=$RUNDIR"
  echo 'LIVE_ORDERS_ALLOWED=false'
  echo "BOT_PID=${BOTPID:-NONE}"
  echo "PORT_8085=${PORTLINE:-NOT_LISTENING}"
  echo 'RECORDER_INTERVAL_MS=250'
  echo 'RECORDER_DURATION_SEC=21600'
  echo 'PAIR_COMPARISON_BUDGET_USD=10'
  echo '=================================================='
} > docs/termux-latest.txt

git add docs/termux-latest.txt
git commit -m "Gate B start $STAMP" || true
for i in {1..20}; do git pull --rebase origin main && git push origin HEAD:main && break; sleep 2; done

nohup python3 "$RUNDIR/gate_b_recorder.py" \
  --bot-log "$BOTDIR/polyarb.log" \
  --out "$RUNDIR/market.jsonl" \
  --status "$RUNDIR/status.txt" \
  --interval-ms 250 \
  --duration-sec 21600 \
  --pair-budget 10 \
  > "$RUNDIR/recorder.log" 2>&1 &
PID=$!
echo "$PID" > "$RUNDIR/recorder.pid"
ln -sfn "$RUNDIR" "$HOME/gate_b_current"
sleep 4

if ! kill -0 "$PID" 2>/dev/null; then
  { echo 'GATE_B_START_FAILED'; cat "$RUNDIR/recorder.log"; } > "$PUB/docs/termux-latest.txt"
  cd "$PUB"; git add docs/termux-latest.txt; git commit -m "Gate B start failed $STAMP" || true
  for i in {1..20}; do git pull --rebase origin main && git push origin HEAD:main && break; sleep 2; done
  exit 1
fi

{
  echo '=================================================='
  echo 'KALSHI PROFESSIONAL PAPER TEST — GATE B RUNNING'
  echo "UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "RUN=$RUNDIR"
  echo 'LIVE_ORDERS_ALLOWED=false'
  echo "RECORDER_PID=$PID"
  echo "BOT_PID=${BOTPID:-NONE}"
  echo "PORT_8085=${PORTLINE:-NOT_LISTENING}"
  echo
  cat "$RUNDIR/status.txt" 2>/dev/null || true
  echo
  echo 'FILES:'
  ls -lh "$RUNDIR"
} > "$PUB/docs/termux-latest.txt"
cd "$PUB"
git add docs/termux-latest.txt
git commit -m "Gate B running $STAMP" || true
for i in {1..20}; do git pull --rebase origin main && git push origin HEAD:main && break; sleep 2; done

echo "GATE B RUNNING PID=$PID"
echo "RUN=$RUNDIR"
