#!/usr/bin/env bash
set -u
REPO="$HOME/kalshi-diagnostic"
STATUS="$REPO/control/project_status.json"
F="$REPO/docs/latest.txt"
PROGRESS="$REPO/docs/agent/progress.txt"
RESULT="$REPO/docs/agent/latest.txt"
TELEMETRY="$REPO/docs/agent/telemetry_status.txt"
AUTO="$REPO/docs/agent/autopilot_status.json"
TMP="/tmp/kalshi_monitor_body.$$"
LAST_SYNC=0
SYNC_EVERY=5
SELF="$REPO/control/monitor_compact.sh"

bar(){ local pct=${1:-0} width=28 filled empty; ((pct<0))&&pct=0; ((pct>100))&&pct=100; filled=$((pct*width/100)); empty=$((width-filled)); printf '['; printf '%*s' "$filled" ''|tr ' ' '#'; printf '%*s' "$empty" ''|tr ' ' '-'; printf '] %3d%%' "$pct"; }
jget(){ python3 - "$STATUS" "$1" <<'PY' 2>/dev/null
import json,sys
try:
 d=json.load(open(sys.argv[1])); v=d
 for k in sys.argv[2].split('.'): v=v[k]
 print('\n'.join(map(str,v)) if isinstance(v,list) else v)
except: pass
PY
}
ajget(){ python3 - "$AUTO" "$1" <<'PY' 2>/dev/null
import json,sys
try:
 d=json.load(open(sys.argv[1])); v=d
 for k in sys.argv[2].split('.'): v=v[k]
 print(v)
except: pass
PY
}
tget(){ [ -s "$TELEMETRY" ] && grep -a "^$1=" "$TELEMETRY"|tail -1|cut -d= -f2-; }
iso_age(){ python3 - "$1" <<'PY' 2>/dev/null
from datetime import datetime,timezone
import sys
try:
 d=datetime.fromisoformat(sys.argv[1].strip().replace('Z','+00:00')); print(max(0,int((datetime.now(timezone.utc)-d).total_seconds())))
except: print(-1)
PY
}
sync_one(){ git -C "$REPO" show "origin/main:$1" > "$2.tmp" 2>/dev/null && mv "$2.tmp" "$2"; }
self_update(){ local remote hash1 hash2; remote=$(mktemp); git -C "$REPO" show origin/main:control/monitor_compact.sh > "$remote" 2>/dev/null || { rm -f "$remote"; return; }; hash1=$(sha256sum "$SELF" 2>/dev/null|awk '{print $1}'); hash2=$(sha256sum "$remote"|awk '{print $1}'); if [ "$hash1" != "$hash2" ]; then chmod +x "$remote"; mv "$remote" "$SELF"; printf '\033[?25h'; exec bash "$SELF"; fi; rm -f "$remote"; }
sync_repo(){ local now; now=$(date +%s); ((now-LAST_SYNC<SYNC_EVERY))&&return; LAST_SYNC=$now; git -C "$REPO" fetch -q origin main 2>/dev/null||return; self_update; mkdir -p "$REPO/docs/agent"; sync_one control/project_status.json "$STATUS"; sync_one docs/latest.txt "$F"; sync_one docs/agent/progress.txt "$PROGRESS"; sync_one docs/agent/latest.txt "$RESULT"; sync_one docs/agent/telemetry_status.txt "$TELEMETRY"; sync_one docs/agent/autopilot_status.json "$AUTO"; }
render(){
  local hb age health overall phase phasep patch patchp patchstate doing why next ac ap ar ad an p
  hb=$(tget AGENT_HEARTBEAT_UTC); age=$(iso_age "${hb:-}"); health=MISSING; [ "$age" -ge 0 ]&&health="LIVE ${age}s"; [ "$age" -gt 20 ]&&health="STALE ${age}s"
  overall=$(jget overall_percent); overall=${overall:-0}; phase=$(jget phase); phasep=$(jget phase_percent); phasep=${phasep:-0}; patch=$(jget current_patch); patchp=$(jget patch_percent); patchp=${patchp:-0}; patchstate=$(jget patch_state); doing=$(jget doing); why=$(jget why); next=$(jget next)
  ac=$(ajget cycle); ap=$(ajget phase); ar=$(ajget result); ad=$(ajget detail); an=$(ajget next); ac=${ac:-0}; ap=${ap:-STARTING}; ar=${ar:-UNKNOWN}
  case "$ap" in SOURCE_AUDIT) p=10;; GO_TEST) p=35;; GO_BUILD) p=60;; PAPER_HEALTH) p=80;; CYCLE_COMPLETE) p=100;; *) p=0;; esac
  {
    echo '==========================================================='
    echo '            KALSHIARBO AUTONOMOUS DEVELOPMENT'
    echo '==========================================================='
    printf 'SCREEN UTC   %s\n' "$(date -u '+%H:%M:%S UTC')"
    printf 'REMOTE UTC   %s\n' "$(tget UPDATED_UTC)"
    printf 'TELEMETRY    %s\n' "$health"
    printf 'AGENT        %s  job=%s\n' "$(tget AGENT_STATUS)" "$(tget AGENT_JOB)"
    printf 'LAST JOB     %s  %s\n' "$(tget LAST_JOB_ID)" "$(tget LAST_RESULT)"
    echo
    echo 'AUTOPILOT — CONTINUOUS'
    printf 'CYCLE        %s\n' "$ac"
    printf 'PHASE        %s\n' "$ap"
    printf 'RESULT       %s\n' "$ar"
    bar "$p"; echo
    printf '%s\n' "$ad"|fold -s -w 58
    printf 'NEXT: %s\n' "$an"|fold -s -w 58
    echo
    echo 'PROJECT'
    bar "$overall"; echo
    printf '%s\n' "$phase"|fold -s -w 58; bar "$phasep"; echo
    echo
    echo 'CURRENT ENGINEERING TARGET'
    printf '%s\n' "$patch"|fold -s -w 58
    printf 'STATE: %s\n' "$patchstate"|fold -s -w 58
    bar "$patchp"; echo
    echo
    echo 'DOING'; printf '%s\n' "$doing"|fold -s -w 58
    echo
    echo 'WHY'; printf '%s\n' "$why"|fold -s -w 58
    echo
    echo 'NEXT ENGINEERING'; printf '%s\n' "$next"|fold -s -w 58
    echo
    if ss -ltnp 2>/dev/null|grep -q ':8085'; then echo 'PAPER BOT     RUNNING :8085'; else echo 'PAPER BOT     DOWN — AUTOPILOT WILL RESTART'; fi
    echo 'LIVE MONEY    HARD-BLOCKED'
    echo 'Auto-sync 5s | screen clock 1s | Ctrl+C monitor only'
  } > "$TMP"
}
printf '\033[?25l\033[2J'
trap 'printf "\033[?25h"; rm -f "$TMP" "$STATUS.tmp" "$F.tmp" "$PROGRESS.tmp" "$RESULT.tmp" "$TELEMETRY.tmp" "$AUTO.tmp"' EXIT INT TERM
while true; do sync_repo; render; printf '\033[H'; cat "$TMP"; printf '\033[J'; sleep 1; done
