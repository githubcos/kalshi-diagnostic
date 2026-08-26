#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
AUTO="$REPO/docs/agent/autopilot_status.json"
RUNTIME="$REPO/docs/agent/runtime_status.json"
PROGRESS="$REPO/docs/agent/progress.txt"
TRADEMON="$REPO/docs/agent/live_trade_monitor.txt"
SELF="$REPO/control/monitor_compact.sh"
LAST_SYNC=0; SYNC_EVERY=5
jsonget(){ python3 - "$1" "$2" <<'PY' 2>/dev/null
import json,sys
try:
 v=json.load(open(sys.argv[1]));
 for k in sys.argv[2].split('.'): v=v[k]
 print(v)
except: pass
PY
}
sync_one(){ git -C "$REPO" show "origin/main:$1" > "$2.tmp" 2>/dev/null && mv "$2.tmp" "$2"; }
sync_repo(){
 local now remote h1 h2; now=$(date +%s); ((now-LAST_SYNC<SYNC_EVERY)) && return; LAST_SYNC=$now
 git -C "$REPO" fetch -q origin main 2>/dev/null || return
 remote=$(mktemp); git -C "$REPO" show origin/main:control/monitor_compact.sh > "$remote" 2>/dev/null || { rm -f "$remote"; return; }
 h1=$(sha256sum "$SELF"|awk '{print $1}'); h2=$(sha256sum "$remote"|awk '{print $1}')
 if [ "$h1" != "$h2" ]; then chmod +x "$remote"; mv "$remote" "$SELF"; exec bash "$SELF"; fi; rm -f "$remote"
 mkdir -p "$REPO/docs/agent"; sync_one docs/agent/autopilot_status.json "$AUTO"; sync_one docs/agent/runtime_status.json "$RUNTIME"; sync_one docs/agent/progress.txt "$PROGRESS"; sync_one docs/agent/live_trade_monitor.txt "$TRADEMON"
}
render(){
 local lastjob lastresult agent paper active action step tmp
 lastjob=$(jsonget "$RUNTIME" state.last_job_id); lastresult=$(jsonget "$RUNTIME" state.last_result); agent=$(jsonget "$RUNTIME" heartbeat.status)
 if ss -ltnp 2>/dev/null|grep -q ':8085'; then paper='RUNNING :8085'; else paper='DOWN'; fi
 active=$(grep '^JOB_ID=' "$PROGRESS" 2>/dev/null|tail -1|cut -d= -f2-)
 action=$(grep -E 'ACTION [0-9]+/[0-9]+ .* (START|PASS)$' "$PROGRESS" 2>/dev/null|tail -1)
 step=$(grep -E '(START|END rc=|JOB RESULT=)' "$PROGRESS" 2>/dev/null|tail -1)
 tmp=$(mktemp); {
 echo 'KALSHIARBO - LIVE TRADE / MATH AUDIT'; echo '====================================='; printf 'UPDATED      %s\n' "$(date -u '+%H:%M:%S UTC')"
 printf 'AGENT        %s\n' "${agent:-UNKNOWN}"; printf 'PAPER BOT    %s\n' "$paper"; echo 'LIVE MONEY   HARD-BLOCKED'; echo
 if [ -s "$TRADEMON" ]; then
   grep -E '^(VERDICT|PAPER_8085|KALSHI_WS|BLOCKER|EVENTS|LATENCY lead|LATENCY hedge)' "$TRADEMON" | head -10 | fold -s -w 62
   echo
   echo 'LATEST MATH / EXECUTION'
   awk '/RECENT MEANINGFUL EXECUTION \/ MATH LINES/{f=1;next}/^WARNINGS$/{f=0} f&&NF{print}' "$TRADEMON" | tail -8 | fold -s -w 62
 else
   echo 'Trade monitor telemetry is starting...'
 fi
 echo
 echo 'ACTIVE ENGINEERING JOB'; printf '%s\n' "${active:-No new job running}" | fold -s -w 62
 printf '%s\n' "${action:-Agent idle}" | fold -s -w 62; [ -n "${step:-}" ] && printf '%s\n' "$step" | fold -s -w 62
 echo; echo 'LATEST COMPLETED'; printf '%s  %s\n' "${lastjob:-none}" "${lastresult:-UNKNOWN}" | fold -s -w 62
 echo; echo 'Auto-sync 5s | Ctrl+C closes monitor only'; } > "$tmp"
 printf '\033[H\033[2J'; cat "$tmp"; rm -f "$tmp"
}
printf '\033[?25l'; trap 'printf "\033[?25h\033[0m\n"' EXIT INT TERM
while true; do sync_repo; render; sleep 1; done
