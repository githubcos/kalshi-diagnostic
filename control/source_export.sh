#!/usr/bin/env bash
set -euo pipefail

BOT="$HOME/KalshiArbo/kalshiarbo"
REPO="$HOME/kalshi-diagnostic"
DST="$REPO/source_snapshot"

cd "$REPO"

echo "SOURCE_EXPORT_START $(date -u --iso-8601=seconds)"
rm -rf "$DST"
mkdir -p "$DST"

copy_tree() {
  local rel="$1"
  [ -d "$BOT/$rel" ] || return 0
  while IFS= read -r -d '' f; do
    local sub="${f#$BOT/}"
    mkdir -p "$DST/$(dirname "$sub")"
    cp "$f" "$DST/$sub"
  done < <(find "$BOT/$rel" -type f \( -name '*.go' -o -name '*.md' \) -print0)
}

# Export only trading/runtime implementation and tests needed for parity work.
# Explicitly exclude web/, config/, credential setup, logs, binaries and secrets.
copy_tree strategy
copy_tree kalshi
copy_tree market
copy_tree exchangefeed

for f in main.go go.mod go.sum README.md; do
  [ -f "$BOT/$f" ] && cp "$BOT/$f" "$DST/$f"
done

# Extra fail-closed filename checks.
if find "$DST" -type f \( -name 'config.json' -o -name '*.env' -o -name '*.pem' -o -name '*.key' -o -name '*.log' \) | grep -q .; then
  echo "SOURCE_EXPORT_ABORTED: forbidden filename detected"
  rm -rf "$DST"
  exit 2
fi

# Defensive content scan for actual credential assignments/material.
# Generic identifier names such as API_SECRET inside source code are allowed;
# concrete-looking assigned values and private-key blocks are not.
if grep -RIlE \
  '(BEGIN [A-Z ]*PRIVATE KEY|[A-Za-z0-9_]*(SECRET|PRIVATE_KEY|PASSPHRASE)[A-Za-z0-9_]*[[:space:]]*[:=][[:space:]]*["'\''`][A-Za-z0-9+/=_-]{16,}["'\''`])' \
  "$DST" >/tmp/kalshi_source_secret_hits.txt 2>/dev/null; then
  echo "SOURCE_EXPORT_ABORTED: possible concrete secret material detected in:"
  cat /tmp/kalshi_source_secret_hits.txt
  rm -rf "$DST"
  exit 2
fi

COUNT=$(find "$DST" -type f | wc -l)
BYTES=$(du -sb "$DST" | awk '{print $1}')

git add source_snapshot
if ! git diff --cached --quiet; then
  git commit -m "Publish sanitized KalshiArbo trading source $(date -u +%H:%M:%S)"
  git push
fi

echo "SOURCE_EXPORT_PASS files=$COUNT bytes=$BYTES"
echo "SOURCE_EXPORT_END $(date -u --iso-8601=seconds)"
