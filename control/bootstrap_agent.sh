#!/usr/bin/env bash
set -euo pipefail

REPO="$HOME/kalshi-diagnostic"
AGENT_SRC="$REPO/control/kalshi_agent.py"; AGENT_DST="$HOME/kalshi_agent.py"
WATCH_SRC="$REPO/control/kalshi_agent_watchdog.sh"; WATCH_DST="$HOME/kalshi_agent_watchdog.sh"
TEL_SRC="$REPO/control/kalshi_telemetry_sync.py"; TEL_DST="$HOME/kalshi_telemetry_sync.py"
AUTO_SRC="$REPO/control/kalshi_autopilot.py"; AUTO_DST="$HOME/kalshi_autopilot.py"
SERVICE="/etc/systemd/system/kalshi-agent.service"
WATCH_SERVICE="/etc/systemd/system/kalshi-agent-watchdog.service"
WATCH_TIMER="/etc/systemd/system/kalshi-agent-watchdog.timer"
TEL_SERVICE="/etc/systemd/system/kalshi-agent-telemetry.service"
AUTO_SERVICE="/etc/systemd/system/kalshi-autopilot.service"

echo "=== KALSHI AGENT + AUTOPILOT BOOTSTRAP ==="
cp "$AGENT_SRC" "$AGENT_DST"
cp "$WATCH_SRC" "$WATCH_DST"
cp "$TEL_SRC" "$TEL_DST"
cp "$AUTO_SRC" "$AUTO_DST"
chmod 700 "$AGENT_DST" "$WATCH_DST" "$TEL_DST" "$AUTO_DST"
python3 -m py_compile "$AGENT_DST" "$TEL_DST" "$AUTO_DST"

sudo tee "$SERVICE" >/dev/null <<EOF
[Unit]
Description=Restricted KalshiArbo GitHub Control Agent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0
[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu
ExecStart=/usr/bin/python3 /home/ubuntu/kalshi_agent.py
Restart=always
RestartSec=2
Environment=HOME=/home/ubuntu
KillSignal=SIGTERM
TimeoutStopSec=20
[Install]
WantedBy=multi-user.target
EOF

sudo tee "$AUTO_SERVICE" >/dev/null <<EOF
[Unit]
Description=KalshiArbo Autonomous Audit and Regression Autopilot
After=network-online.target kalshi-agent.service
Wants=network-online.target kalshi-agent.service
StartLimitIntervalSec=0
[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu
ExecStart=/usr/bin/python3 /home/ubuntu/kalshi_autopilot.py
Restart=always
RestartSec=2
Environment=HOME=/home/ubuntu
[Install]
WantedBy=multi-user.target
EOF

sudo tee "$WATCH_SERVICE" >/dev/null <<EOF
[Unit]
Description=Kalshi Agent Heartbeat Watchdog
After=kalshi-agent.service
[Service]
Type=oneshot
User=root
Environment=HOME=/home/ubuntu
ExecStart=/home/ubuntu/kalshi_agent_watchdog.sh
EOF

sudo tee "$WATCH_TIMER" >/dev/null <<EOF
[Unit]
Description=Run Kalshi Agent Watchdog Every Minute
[Timer]
OnBootSec=60
OnUnitActiveSec=60
AccuracySec=10
Persistent=true
[Install]
WantedBy=timers.target
EOF

sudo tee "$TEL_SERVICE" >/dev/null <<EOF
[Unit]
Description=Kalshi Agent Dedicated GitHub Telemetry Publisher
After=network-online.target kalshi-agent.service kalshi-autopilot.service
Wants=network-online.target
StartLimitIntervalSec=0
[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu
ExecStart=/usr/bin/python3 /home/ubuntu/kalshi_telemetry_sync.py
Restart=always
RestartSec=2
Environment=HOME=/home/ubuntu
[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable kalshi-agent.service kalshi-autopilot.service kalshi-agent-watchdog.timer kalshi-agent-telemetry.service >/dev/null
sudo systemctl restart kalshi-agent.service
sudo systemctl restart kalshi-autopilot.service
sudo systemctl restart kalshi-agent-watchdog.timer
sudo systemctl restart kalshi-agent-telemetry.service

echo "Waiting for agent + autopilot + telemetry health..."
for i in $(seq 1 15); do
  AG=$(systemctl is-active kalshi-agent.service 2>/dev/null || true)
  AP=$(systemctl is-active kalshi-autopilot.service 2>/dev/null || true)
  TEL=$(systemctl is-active kalshi-agent-telemetry.service 2>/dev/null || true)
  if [ "$AG" = active ] && [ "$AP" = active ] && [ "$TEL" = active ] && [ -d "$HOME/kalshi-agent-telemetry/.git" ] && [ ! -s "$HOME/.kalshi_telemetry_error.txt" ]; then break; fi
  sleep 2
done
AG=$(systemctl is-active kalshi-agent.service 2>/dev/null || true)
AP=$(systemctl is-active kalshi-autopilot.service 2>/dev/null || true)
TEL=$(systemctl is-active kalshi-agent-telemetry.service 2>/dev/null || true)
echo "AGENT_SERVICE=$AG"
echo "AUTOPILOT_SERVICE=$AP"
echo "TELEMETRY_SERVICE=$TEL"
[ -f "$HOME/.kalshi_agent_heartbeat.json" ] && cat "$HOME/.kalshi_agent_heartbeat.json"
[ -f "$HOME/.kalshi_autopilot_state.json" ] && cat "$HOME/.kalshi_autopilot_state.json"
if [ -s "$HOME/.kalshi_telemetry_error.txt" ]; then cat "$HOME/.kalshi_telemetry_error.txt"; fi
if [ "$AG" != active ] || [ "$AP" != active ] || [ "$TEL" != active ] || [ ! -d "$HOME/kalshi-agent-telemetry/.git" ] || [ -s "$HOME/.kalshi_telemetry_error.txt" ]; then
  echo "BOOTSTRAP_FAILED"
  sudo journalctl -u kalshi-agent.service -n 30 --no-pager || true
  sudo journalctl -u kalshi-autopilot.service -n 30 --no-pager || true
  sudo journalctl -u kalshi-agent-telemetry.service -n 30 --no-pager || true
  exit 1
fi

echo "AUTOPILOT: continuous source audit -> go test -> go build -> paper health -> report -> repeat"
echo "TELEMETRY: publishes every 5s"
echo "RECOVERY: systemd Restart=always + watchdog"
echo "LIVE trading remains hard-blocked"
echo "BOOTSTRAP_COMPLETE"
