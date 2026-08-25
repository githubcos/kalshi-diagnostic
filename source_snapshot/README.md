# PolyArbPro — Setup Guide

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Installation](#installation)
   - [Windows](#windows)
   - [macOS](#macos)
   - [Linux](#linux)
3. [Running the Bot](#running-the-bot)
4. [First-Run Setup Wizard](#first-run-setup-wizard)
5. [Configuration Reference](#configuration-reference)
6. [Paper Trading vs Live Trading](#paper-trading-vs-live-trading)
7. [Dashboard](#dashboard)
8. [Risk Controls](#risk-controls)
9. [Running as a Background Service](#running-as-a-background-service)
10. [Troubleshooting](#troubleshooting)

---

## Prerequisites

- A **Polymarket account** with a funded proxy wallet (USDC on Polygon)
- Your Ethereum **private key** (hex, no `0x` prefix) for the signing wallet
- Your Polymarket **CLOB API credentials** (key, secret, passphrase) — the setup wizard can generate these for you
- **Go 1.24 or later** to build from source (not needed if using a pre-built binary)
- NodeJS if you are using Poly Proxy and Builder credentials, you need to get these from https://polymarket.com/settings?tab=builder

---

## Installation

### Windows
0. **Install Node dependencies (Optional, only for poly proxy wallets, email)**
   ```bash
   npm i
   ```

1. **Install Go** (if building from source)
   - Download from [go.dev/dl](https://go.dev/dl/) and run the installer
   - Open a new PowerShell window and confirm: `go version`

2. **Get the source**
   Unzip the folder in one folder and open a powershell terminal in that folder.

3. **Build**
   ```powershell
   go build -o PolyArbPro.exe .
   ```

4. **Run**
   ```powershell
   .\PolyArbPro.exe
   ```
   The dashboard opens at `http://localhost:8080`. On first run the setup wizard launches automatically.

---

### macOS
0. **Install Node dependencies (Optional, only for poly proxy wallets, email)**
   ```bash
   npm i
   ```

1. **Install Go**
   ```bash
   brew install go
   ```
   Or download the `.pkg` installer from [go.dev/dl](https://go.dev/dl/).

2. **Get the source**
    Unzip the downloaded source to your desktop.
   ```bash
   cd Desktop/polyarbpro_2.1
   ```


3. **Build**
   ```bash
   go build -o PolyArbPro .
   ```

4. **Run**
   ```bash
   ./PolyArbPro
   ```
   Open `http://localhost:8080` in your browser. The setup wizard will appear on first run.

---

### Linux
0. **Install Node dependencies (Optional, only for poly proxy wallets, email)**
   ```bash
   npm i
   ```
   
1. **Install Go**
   ```bash
   # Debian / Ubuntu
   sudo apt update && sudo apt install -y golang-go

   # Or install the latest version manually:
   wget https://go.dev/dl/go1.24.0.linux-amd64.tar.gz
   sudo tar -C /usr/local -xzf go1.24.0.linux-amd64.tar.gz
   export PATH=$PATH:/usr/local/go/bin   # add to ~/.bashrc or ~/.profile
   ```

2. **Get the source**
    Upload the source code through FTP
   ```bash
   cd polyarbpro_2.1
   ```

3. **Build**
   ```bash
   go build -o PolyArbPro .
   ```

4. **Run**
   ```bash
   ./PolyArbPro
   ```
   The web dashboard is available at `http://localhost:8080` (or on the server's IP if accessed remotely).

---

## Running the Bot

```
Usage: PolyArbPro [flags]

Flags:
  -live    Submit real orders (default: paper trade mode)
  -port    HTTP port for the web dashboard (default: 8080)
```

**Examples:**

```bash
# Paper trade on the default port (safe, no real money)
./PolyArbPro

# Live trading on port 9090
./PolyArbPro -live -port 9090

# Paper trade, dashboard on a different port
./PolyArbPro -port 3000
```

> **Always start in paper trade mode first** to verify your credentials and confirm the bot is detecting signals correctly before using real funds.

---

## First-Run Setup Wizard

If `config.json` is missing or incomplete, the bot starts in setup mode and displays a wizard at:

```
http://localhost:8080/wizard
```

Default login password: **`changeme`** — change it immediately in **Settings → Change Password**.

The wizard walks you through:

1. **Private Key** — paste your Ethereum signing wallet private key (hex, no `0x`)
2. **Derive EOA** — the wizard derives your public wallet address automatically
3. **Derive Proxy Wallet** — looks up your Polymarket proxy wallet address from the Polymarket API
4. **Generate API Credentials** — creates the CLOB API key/secret/passphrase using your private key; no Polymarket website action needed
5. **Enter Builder Credentials** — get the builder credentials from polymarket.com and enter them here for claiming.
6. **Trade Size** — dollar amount per position (e.g. `5` for $5 per trade)
7. **Paper Trade** — leave checked until you are confident the bot is working correctly
8. **Save** — writes `config.json` and starts the bot immediately; click **▶ Start** in the dashboard

---

## Paper Trading vs Live Trading

| Mode | Flag | Orders | Risk |
|---|---|---|---|
| Paper | _(default, no flag)_ | Simulated only | None |
| Live | `-live` | Real CLOB orders | Real USDC |

In paper mode the bot simulates fills at the current mid-price and tracks a virtual balance starting from `PAPER_START_BALANCE`. All logic (signal detection, entry gates, exit timing) runs identically to live mode.

To switch to live trading:
1. Verify paper results show the bot is entering and exiting correctly
2. Ensure your proxy wallet has USDC on Polygon
3. Restart with the `-live` flag: `./PolyArbPro -live`

---

## Dashboard

The web dashboard runs at `http://localhost:8080` (or the port you configured with `-port`).

| Page | Path | Description |
|---|---|---|
| Dashboard | `/dashboard` | Live feed status, current position, P&L, activity log |
| Strategies | `/strategies` | Activate strategies, adjust knobs and backtest with your signals |
| Settings | `/settings` | Change password, configure risk controls, claim settings, and Discord trade notifications |
| Wizard | `/wizard` | Initial credential setup |

### Dashboard controls

- **▶ Start** — begin the trading session (bot waits for this after launch)
- **■ Stop** — pause trading after the current window completes; click Start again to resume
- **Markets tab** — per-window history: entry side, P&L, resolution
- **Trades tab** — full trade log for the session

---

## Risk Controls

### Session Stop-Loss

The simplest protection: go to **Settings → Session Stop-Loss** and enter a dollar amount. If cumulative session losses exceed that amount, the bot stops automatically and returns to the "waiting for Start" state. You must click **▶ Start** to resume.

### Trade Size

Keep `TRADE_SIZE_USD` small ($5–$20) while tuning parameters. Each snipe is a single binary bet — the token either settles at $0.99 or $0.01.

---

## Troubleshooting

### "another PolyArbPro instance is already running"
A previous run crashed without deleting the lock file. Safe to remove:
```bash
rm .bot.lock          # Linux / macOS
Remove-Item .bot.lock # Windows PowerShell
```

### Bot launches but never enters a position
- Check the **Active Strategy Gates** inside the Dashboard as it contains active strategies and their thresholds
- Market conditions may genuinely not meet the entry criteria

### "config still incomplete after wizard save"
All five required fields must be non-empty: `PRIVATE_KEY_HEX`, `PROXY_WALLET_ADDRESS`, `POLY_API_KEY`, `POLY_API_SECRET`, `POLY_API_PASSPHRASE`. Re-run the wizard and confirm each step completes successfully.

### Dashboard not accessible
- Confirm the bot is still running (it exits if config load fails)
- Check the port: default is `8080`, or whatever you passed with `-port`
- On Linux/macOS, check firewall rules if accessing from a remote machine: `sudo ufw allow 8080`

### Orders rejected / auth errors
- Verify `POLY_SIG_TYPE` matches how you created the API key (`1` for PolyProxy is most common)
- CLOB credentials expire — use **Settings → Wizard → Generate Credentials** to rotate them
- Ensure the proxy wallet has sufficient USDC on the Polygon network

### High latency / missed entries
For best results run the bot on a server, best recommended an AWS EC2 instance located in Ireland.

This bot is a tool - not a guarantee. Prediction markets carry inherent risk. Only deploy capital you're comfortable risking. Past signal accuracy does not guarantee future results.
