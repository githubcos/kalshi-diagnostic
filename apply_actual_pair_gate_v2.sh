#!/usr/bin/env bash
set -euo pipefail

BOTDIR="$HOME/KalshiArbo/kalshiarbo"
cd "$BOTDIR"

STAMP="$(date -u +%Y%m%d_%H%M%S)"
cp -a strategy/signal.go "strategy/signal.go.pre_actual_pair_gate_$STAMP"

python3 - <<'PY'
from pathlib import Path
p=Path('strategy/signal.go')
s=p.read_text()
old='''\tsigType := SignalPairArbLeadNo\n\ttokenPrice := noPrice\n\tif isYesLead {\n\t\ttokenPrice = yesPrice\n\t\tif reverseMode {\n\t\t\tsigType = SignalPairArbReverseLeadYes\n\t\t} else {\n\t\t\tsigType = SignalPairArbLeadYes\n\t\t}\n\t} else if reverseMode {\n\t\tsigType = SignalPairArbReverseLeadNo\n\t}\n\tif tokenPrice < d.params.PairArbMinTokenPrice || tokenPrice > d.params.PairArbMaxTokenPrice {\n\t\treturn Signal{Type: SignalNone}\n\t}\n\t// Do not require the opposite leg to be within the hedge-distance band at\n\t// signal-generation time. Pair-arb is intentionally a lead-then-hedge\n\t// strategy: the trader owns the hedge timeout and fee-adjusted locked-profit\n\t// limit, and must still refuse a hedge that cannot preserve the configured\n\t// locked profit. Requiring that hedge to already exist here collapses the\n\t// strategy into an instantaneous complete-set scanner and suppresses leads.\n'''
new='''\tsigType := SignalPairArbLeadNo\n\ttokenPrice := noPrice\n\toppPrice := yesPrice\n\tif isYesLead {\n\t\ttokenPrice = yesPrice\n\t\toppPrice = noPrice\n\t\tif reverseMode {\n\t\t\tsigType = SignalPairArbReverseLeadYes\n\t\t} else {\n\t\t\tsigType = SignalPairArbLeadYes\n\t\t}\n\t} else if reverseMode {\n\t\tsigType = SignalPairArbReverseLeadNo\n\t}\n\tif tokenPrice < d.params.PairArbMinTokenPrice || tokenPrice > d.params.PairArbMaxTokenPrice {\n\t\treturn Signal{Type: SignalNone}\n\t}\n\t// ACTUAL pair-lead preflight gate. The prior Kalshi translation intentionally\n\t// removed this check and allowed naked lead legs. Historical PAPER evidence\n\t// showed those exposed legs caused essentially all losses. Require the\n\t// opposite side to already be close enough that the configured locked-profit\n\t// target can be reached within PairArbMaxHedgeDistanceCents.\n\tif d.params.PairArbMaxHedgeDistanceCents > 0 {\n\t\tlockedProfit := d.params.PairArbMinLockedProfitCents / 100.0\n\t\tmaxHedgePrice := 1.0 - lockedProfit - tokenPrice\n\t\tif oppPrice-maxHedgePrice > d.params.PairArbMaxHedgeDistanceCents/100.0 {\n\t\t\treturn Signal{Type: SignalNone}\n\t\t}\n\t}\n'''
if old not in s:
    raise SystemExit('ERROR: exact standard pair-lead block not found; no file changed')
s=s.replace(old,new,1)
p.write_text(s)
print('Patched the standard PAIR_ARB_LEAD YES/NO path.')
PY

gofmt -w strategy/signal.go

echo '=== PATCH MARKER ==='
grep -n -A18 -B8 'ACTUAL pair-lead preflight gate' strategy/signal.go

echo '=== TEST ==='
go test ./...

echo '=== BUILD ==='
go build -o kalshiarbo .

echo '=== PAPER SAFETY ==='
python3 - <<'PY'
import json
p='config.json'
d=json.load(open(p))
d['PAPER_TRADE']=True
# Preserve the current experimental threshold if already present.
d['PAIR_ARB_MIN_LOCKED_PROFIT_CENTS']=float(d.get('PAIR_ARB_MIN_LOCKED_PROFIT_CENTS',3) or 3)
d['PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS']=float(d.get('PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS',1) or 1)
json.dump(d,open(p,'w'),indent=2)
print('PAPER_TRADE=',d['PAPER_TRADE'])
print('PAIR_ARB_MIN_LOCKED_PROFIT_CENTS=',d['PAIR_ARB_MIN_LOCKED_PROFIT_CENTS'])
print('PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS=',d['PAIR_ARB_MAX_HEDGE_DISTANCE_CENTS'])
PY

echo 'PATCH_STATUS=PASS'
echo "BACKUP=strategy/signal.go.pre_actual_pair_gate_$STAMP"
