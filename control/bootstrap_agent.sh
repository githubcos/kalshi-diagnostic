#!/usr/bin/env bash
set -euo pipefail

REPO="$HOME/kalshi-diagnostic"
AGENT_SRC="$REPO/control/kalshi_agent.py"
AGENT_DST="$HOME/kalshi_agent.py"
WATCH_SRC="$REPO/control/kalshi_agent_watchdog.sh"
WATCH_DST="$HOME/kalshi_agent_watchdog.sh"
TEL_SRC="$REPO/control/kalshi_telemetry_sync.py"
TEL_DST="$HOME/kalshi_telemetry_sync.py"
SERVICE="/etc/systemd/system/kalshi-agent.service"
WATCH_SERVICE="/etc/systemd/system/kalshi-agent-watchdog.service"
WATCH_TIMER="/etc/systemd/system/kalshi-agent-watchdog.timer"
TEL_SERVICE="/etc/systemd/system/kalshi-agent-telemetry.service"

echo "=== KALSHI AGENT BOOTSTRAP ==="

# Deliberately do NOT git pull here. Caller installs known-good control files
# from origin/main before invoking this script.
cp "$AGENT_SRC" "$AGENT_DST"
cp "$WATCH_SRC" "$WATCH_DST"
cp "$TEL_SRC" "$TEL_DST"
chmod 700 "$AGENT_DST" "$WATCH_DST" "$TEL_DST"
python3 -m py_compile "$AGENT_DST" "$TEL_DST"

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
After=network-online.target kalshi-agent.service
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
sudo systemctl enable kalshi-agent.service kalshi-agent-watchdog.timer kalshi-agent-telemetry.service >/dev/null
sudo systemctl restart kalshi-agent.service
sudo systemctl restart kalshi-agent-watchdog.timer
sudo systemctl restart kalshi-agent-telemetry.service
sleep 4

echo
sudo systemctl --no-pager --full status kalshi-agent.service || true
echo
sudo systemctl --no-pager --full status kalshi-agent-telemetry.service || true
echo
sudo systemctl --no-pager --full status kalshi-agent-watchdog.timer || true

echo
echo "=== SELF-HEALING SAFETY ==="
echo "Agent polls origin/main directly every 5s."
echo "Agent self-updates after syntax validation."
echo "Dedicated clean telemetry clone publishes progress/results every 5s."
echo "Telemetry publishing is independent of the diagnostic working tree."
echo "systemd restarts agent and telemetry publisher after any process exit."
echo "Watchdog restarts agent if heartbeat is missing/stale (>300s)."
echo "LIVE trading remains hard-blocked by the agent."
echo
echo "BOOTSTRAP_COMPLETE"
