#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
AUTO="$REPO/docs/agent/autopilot_status.json"
RUNTIME="$REPO/docs/agent/runtime_status.json"
TELEMETRY="$REPO/docs/agent/telemetry_status.txt"
SELF="$REPO/control/monitor_compact.sh"
LAST_SYNC=0
SYNC_EVERY=5

jsonget(){ python3 - "$1" "$2" <<'PY' 2>/dev/null
import json,sys
try:
 v=json.load(open(sys.argv[1]))
 for k in sys.argv[2].split('.'): v=v[k]
 print(v)
except: pass
PY
}
tget(){ [ -s "$TELEMETRY" ] && grep -a "^$1=" "$TELEMETRY" | tail -1 | cut -d= -f2-; }
sync_one(){ git -C "$REPO" show "origin/main:$1" > "$2.tmp" 2>/dev/null && mv "$2.tmp" "$2"; }
sync_repo(){
 local now remote h1 h2
 now=$(date +%s); ((now-LAST_SYNC<SYNC_EVERY)) && return; LAST_SYNC=$now
 git -C "$REPO" fetch -q origin main 2>/dev/null || return
 remote=$(mktemp); git -C "$REPO" show origin/main:control/monitor_compact.sh > "$remote" 2>/dev/null || { rm -f "$remote"; return; }
 h1=$(sha256sum "$SELF" 2>/dev/null|awk '{print $1}'); h2=$(sha256sum "$remote"|awk '{print $1}')
 if [ "$h1" != "$h2" ]; then chmod +x "$remote"; mv "$remote" "$SELF"; exec bash "$SELF"; fi
 rm -f "$remote"
 mkdir -p "$REPO/docs/agent"
 sync_one docs/agent/autopilot_status.json "$AUTO"
 sync_one docs/agent/runtime_status.json "$RUNTIME"
 sync_one docs/agent/telemetry_status.txt "$TELEMETRY"
}
render(){
 local phase pct result detail next lastjob lastresult agent paper tmp
 phase=$(jsonget "$AUTO" phase); pct=$(jsonget "$AUTO" percent); result=$(jsonget "$AUTO" result)
 detail=$(jsonget "$AUTO" detail); next=$(jsonget "$AUTO" next)
 lastjob=$(jsonget "$RUNTIME" state.last_job_id); lastresult=$(jsonget "$RUNTIME" state.last_result)
 agent=$(jsonget "$RUNTIME" heartbeat.status)
 if ss -ltnp 2>/dev/null | grep -q ':8085'; then paper='RUNNING :8085'; else paper='DOWN'; fi
 tmp=$(mktemp)
 {
   echo 'KALSHIARBO DEVELOPMENT'
   echo '======================='
   printf 'UPDATED       %s\n' "$(date -u '+%H:%M:%S UTC')"
   printf 'PROJECT       %s%%\n' "${pct:-0}"
   printf 'PHASE         %s\n' "${phase:-UNKNOWN}"
   printf 'RESULT        %s\n' "${result:-UNKNOWN}"
   echo
   printf 'LAST JOB      %s\n' "${lastjob:-none}"
   printf 'JOB RESULT    %s\n' "${lastresult:-UNKNOWN}"
   printf 'AGENT         %s\n' "${agent:-UNKNOWN}"
   printf 'PAPER BOT     %s\n' "$paper"
   echo 'LIVE MONEY    HARD-BLOCKED'
   echo
   echo 'CURRENT'
   printf '%s\n' "${detail:-No current detail}" | fold -s -w 54
   echo
   echo 'NEXT'
   printf '%s\n' "${next:-UNKNOWN}" | fold -s -w 54
   echo
   echo 'Auto-sync 5s | Ctrl+C closes monitor only'
 } > "$tmp"
 # Atomic full-screen redraw: home + clear, then one write.
 printf '\033[H\033[2J'
 cat "$tmp"
 rm -f "$tmp"
}
printf '\033[?25l'
trap 'printf "\033[?25h\033[0m\n"' EXIT INT TERM
while true; do sync_repo; render; sleep 1; done
