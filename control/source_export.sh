#!/usr/bin/env bash
set -euo pipefail

BOT="$HOME/KalshiArbo/kalshiarbo"
REPO="$HOME/kalshi-diagnostic"
DST="$REPO/source_snapshot"

cd "$REPO"

echo "SOURCE_EXPORT_START $(date -u --iso-8601=seconds)"
rm -rf "$DST"
mkdir -p "$DST"

# Whitelist source/document types only. Never export runtime secrets or configs.
while IFS= read -r -d '' f; do
  rel="${f#$BOT/}"
  case "$rel" in
    .git/*|logs/*|node_modules/*|config.json|*.env|*.pem|*.key|*.crt|*.p12|*.pfx|*.log|kalshiarbo|.bot.lock|*.pid)
      continue
      ;;
  esac
  base="$(basename "$f")"
  case "$base" in
    go.mod|go.sum|README.md|package.json|package-lock.json)
      ;;
    *)
      case "$f" in
        *.go|*.js|*.html|*.css|*.md|*.txt)
          ;;
        *) continue ;;
      esac
      ;;
  esac
  mkdir -p "$DST/$(dirname "$rel")"
  cp "$f" "$DST/$rel"
done < <(find "$BOT" -type f -print0)

# Defensive scan: fail closed if common secret material somehow appears.
if grep -RIlE --exclude='*.md' --exclude='*.txt' \
  '(PRIVATE_KEY|API_SECRET|API_PASSPHRASE|BEGIN [A-Z ]*PRIVATE KEY|KALSHI.*SECRET|POLY_API_SECRET)' \
  "$DST" >/tmp/kalshi_source_secret_hits.txt 2>/dev/null; then
  echo "SOURCE_EXPORT_ABORTED: potential secret tokens detected in:"
  cat /tmp/kalshi_source_secret_hits.txt
  rm -rf "$DST"
  exit 2
fi

COUNT=$(find "$DST" -type f | wc -l)
BYTES=$(du -sb "$DST" | awk '{print $1}')

git add source_snapshot
if ! git diff --cached --quiet; then
  git commit -m "Publish sanitized KalshiArbo source snapshot $(date -u +%H:%M:%S)"
  git push
fi

echo "SOURCE_EXPORT_PASS files=$COUNT bytes=$BYTES"
echo "SOURCE_EXPORT_END $(date -u --iso-8601=seconds)"
