#!/usr/bin/env bash
set -euo pipefail

SRC="$HOME/audit_20260825_full.txt"
REPO="$HOME/kalshi-diagnostic"
OUT="$REPO/docs/forensic_core.txt"
TMP="$HOME/forensic_core_$$.txt"

if [ ! -s "$SRC" ]; then
  echo "ERROR: missing or empty $SRC" >&2
  exit 1
fi

{
  echo "===== KALSHIARBO FORENSIC CORE ====="
  echo "SOURCE=$SRC"
  echo "GENERATED_UTC=$(date -u --iso-8601=seconds)"
  echo "SOURCE_LINES=$(wc -l < "$SRC")"
  echo "SOURCE_BYTES=$(wc -c < "$SRC")"
  echo
  echo "===== SESSION / BANKROLL / CONFIG ====="
  grep -Ein -m 300 -B 3 -A 6 'PAPER TRADE|paper balance|Trade Size|Balance|session P&L|session pnl|Win / Loss|WIN / LOSS|ACTIVE FLAGS|CONFIG SOURCE|PAIR_ARB_|pair_arb' "$SRC" || true
  echo
  echo "===== ALL PAIR ENTRY / FILL / HEDGE / RESIDUAL / SETTLEMENT EVENTS WITH CONTEXT ====="
  grep -Ein -B 12 -A 20 'PAIR ARB|pair arb|pair_arb|RESIDUAL|residual|locked pair|locked_profit|locked profit|lead.*fill|hedge.*fill|both legs|dual.*fill|entry|entered|entering|opened|settlement|settled|orphan|rebalance|unbalanced|balanced|actual.*fill|fill_price|filled_price|usd_spent|yes_shares|no_shares|yes.*spent|no.*spent' "$SRC" || true
  echo
  echo "===== NO-ENTRY / REJECTION SNAPSHOTS ====="
  grep -Ein -B 5 -A 10 'no entry snapshot|reject|blocked|outside.*window|token.*price|btc.*gap|stale|signal.*age|insufficient|timeout|cooldown|abort' "$SRC" || true
  echo
  echo "===== ECONOMIC VALUES / ORDERBOOK / FEES ====="
  grep -Ein -B 5 -A 10 'yes[^[:alnum:]]*[=: ]|no[^[:alnum:]]*[=: ]|total[^[:alnum:]]*[=: ]|ask|bid|orderbook|book|VWAP|fee|slippage|payout|profit|ROI|cost|notional|locked' "$SRC" || true
  echo
  echo "===== ERROR / WARN ====="
  grep -Ein -B 5 -A 10 'ERROR|WARN|FATAL|panic|failed|failure' "$SRC" || true
} > "$TMP"

# Remove exact duplicate adjacent blocks/lines where possible, keep chronology and evidence.
awk 'BEGIN{prev="\034"} {if($0!=prev) print; prev=$0}' "$TMP" > "$OUT"
rm -f "$TMP"

cd "$REPO"
git pull --rebase >/dev/null 2>&1 || true
git add docs/forensic_core.txt
git commit -m "Publish KalshiArbo forensic core evidence" >/dev/null 2>&1 || true
git push

echo "FORENSIC CORE PUBLISHED"
wc -l -c "$OUT"
echo "https://githubcos.github.io/kalshi-diagnostic/forensic_core.txt"
