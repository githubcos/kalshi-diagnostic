#!/usr/bin/env bash
set -euo pipefail

REPO="$HOME/kalshi-diagnostic"
AGENT_SRC="$REPO/control/kalshi_agent.py"
AGENT_DST="$HOME/kalshi_agent.py"
SERVICE="/etc/systemd/system/kalshi-agent.service"

echo "=== KALSHI AGENT BOOTSTRAP ==="

cd "$REPO"
git pull --rebase

cp "$AGENT_SRC" "$AGENT_DST"
chmod 700 "$AGENT_DST"

sudo tee "$SERVICE" >/dev/null <<EOF
[Unit]
Description=Restricted KalshiArbo GitHub Control Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu
ExecStart=/usr/bin/python3 /home/ubuntu/kalshi_agent.py
Restart=always
RestartSec=5
Environment=HOME=/home/ubuntu

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now kalshi-agent.service
sleep 3

echo
sudo systemctl --no-pager --full status kalshi-agent.service || true

echo
echo "=== SAFETY ==="
echo "LIVE trading is hard-blocked by the agent."
echo "Allowed actions are restricted to status/backup/patch/gofmt/test/build/start-paper/stop-paper/audit/rollback."
echo
echo "BOOTSTRAP_COMPLETE"
