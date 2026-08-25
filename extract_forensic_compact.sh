#!/usr/bin/env bash
set -euo pipefail

SRC="$HOME/audit_20260825_full.txt"
REPO="$HOME/kalshi-diagnostic"
TMP="$HOME/forensic_compact_$$.txt"
OUTDIR="$REPO/docs"

if [ ! -s "$SRC" ]; then
  echo "ERROR: missing or empty $SRC" >&2
  exit 1
fi

mkdir -p "$OUTDIR"
rm -f "$OUTDIR"/forensic_compact_*.txt

{
  echo "===== FORENSIC COMPACT ====="
  echo "SOURCE_LINES=$(wc -l < "$SRC") SOURCE_BYTES=$(wc -c < "$SRC") GENERATED_UTC=$(date -u --iso-8601=seconds)"
  echo
  echo "===== EXACT PAIR / RESIDUAL / ORPHAN / SETTLEMENT EVENTS ====="
  grep -Ein -B 3 -A 6 'PAIR ARB|RESIDUAL|ORPHAN|LOCKED PROFIT|LOCKED_PROFIT|SETTLED|SETTLEMENT|HEDGE|UNHEDGED|BOTH LEGS|DUAL FILL|LEAD LEG' "$SRC" || true
  echo
  echo "===== EXECUTION / FILL / ORDER EVENTS ====="
  grep -Ein -B 2 -A 4 'paper.*(buy|sell|order|fill)|live.*(buy|sell|order|fill)|order.*(placed|accepted|filled|partial|cancel)|fill(ed)?[^a-z]|actual.*fill|fill_price|filled_price|usd_spent|shares.*filled' "$SRC" || true
  echo
  echo "===== PNL / BALANCE / PROFIT / LOSS ====="
  grep -Ein -B 2 -A 4 'session.*p.?l|session.*pnl|profit|loss|balance|bankroll|payout|roi|win[^a-z]|lose|losing|won|lost' "$SRC" || true
  echo
  echo "===== PAIR CONFIG / THRESHOLDS ====="
  grep -Ein -m 500 -B 1 -A 3 'PAIR_ARB_|pair.*min.*edge|pair.*max.*spend|fee.*buffer|slippage|trade size|cooldown|timeout' "$SRC" || true
  echo
  echo "===== ERRORS / WARNINGS NEAR EXECUTION ====="
  grep -Ein -B 2 -A 4 'ERROR|WARN|FATAL|panic|failed|insufficient|timeout|stale' "$SRC" || true
} | awk 'BEGIN{prev="\034"} {if($0!=prev) print; prev=$0}' > "$TMP"

# Publish in connector-friendly chunks (<400 KiB each).
split -C 380k -d -a 2 "$TMP" "$OUTDIR/forensic_compact_"
for f in "$OUTDIR"/forensic_compact_*; do mv "$f" "$f.txt"; done
rm -f "$TMP"

cd "$REPO"
git pull --rebase >/dev/null 2>&1 || true
git add docs/forensic_compact_*.txt
git commit -m "Publish compact KalshiArbo forensic evidence" >/dev/null 2>&1 || true
git push

echo "FORENSIC COMPACT PUBLISHED"
wc -l -c docs/forensic_compact_*.txt
echo "Files:"
ls -1 docs/forensic_compact_*.txt
