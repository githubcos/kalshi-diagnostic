#!/usr/bin/env python3
from pathlib import Path

p = Path.home() / "ArboCos" / "realpaper" / "strategy" / "trader.go"
s = p.read_text()
needle = "func (t *Trader) SetMarketTokens(yesTokenID, noTokenID, conditionID string) {\n"
if needle not in s:
    raise SystemExit("SetMarketTokens function signature not found")
marker = 't.logger.Info("market tokens updated"'
if marker in s:
    print("market-token logging already present")
    raise SystemExit(0)
insert = needle + '''\tt.logger.Info("market tokens updated",\n\t\tzap.String("yes_token_id", yesTokenID),\n\t\tzap.String("no_token_id", noTokenID),\n\t\tzap.String("condition_id", conditionID),\n\t)\n'''
s = s.replace(needle, insert, 1)
p.write_text(s)
print(f"patched {p}")
