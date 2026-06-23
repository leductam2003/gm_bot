# GM Suite — EVM Automation Desktop App

**Languages:** **English** · [Tiếng Việt](README.vi.md) · [中文](README.zh.md)

A self-contained desktop app for high-volume EVM automation: multi-wallet NFT minting (SeaDrop + OpenSea presale vouchers), Flashbots private bundles, whitelist eligibility checking, RPC/proxy management, and Telegram remote control — all driven from a single native window.

Written in Go (single binary, pure-Go SQLite, no CGO) with a WebView2 desktop UI. **EVM-only.**

> ⚠️ **Disclaimer.** This tool handles private keys and automates on-chain transactions. Use it only with wallets and funds you control. There is no warranty — you are responsible for what you sign and broadcast. Not financial advice.

---

## Features

- **Wallets** — Generate or import in bulk, organize into groups. Keys are **encrypted at rest** with a locally-stored random secret (`vault.key`) and auto-unlocked on launch; reveal is disabled by default.
- **Tasks (minting engine)**
  - SeaDrop **public mint** and OpenSea **signed/voucher presale** (allowlist) mints.
  - **Flashbots** private bundles (anti-frontrun) with tunable bundle window + priority/max fee.
  - **Simulate → execute**: dry-runs each tx (`eth_call`) and only broadcasts if it would succeed.
  - **Post-mint chained action**: auto Transfer / List / Accept-offer / Drain after a successful mint.
  - Scheduled **start time**, per-task multi-RPC selection, proxy group assignment.
- **Whitelist Checker** — Signs each wallet in to OpenSea (SIWE) and reads **per-phase eligibility** (WL / presale / public) with cumulative caps. Results **stream in row-by-row**; configurable **threads** + **proxy group** to avoid per-IP throttling.
- **NFT Manager** — Holdings, list/cancel listings on OpenSea (Seaport).
- **RPC** — Multi-chain endpoint manager with latency testing; per-chain fallbacks.
- **Proxies** — Groups, rotation, and a live reachability test.
- **Contract tools** — Paste/fetch ABI + function picker for arbitrary contracts; replay a tx by hash or explorer link.
- **Settings** — OpenSea API keys (**multiple keys, auto-rotated** to dodge rate limits), Etherscan key, Flashbots tuning, **custom chains** (add a brand-new chain with no code change), Discord webhook notifications, UI scale, and check-for-update.
- **Telegram remote control** — Start/stop tasks and get notifications from your phone.

## Requirements

- **Go 1.21+** to build.
- **Windows** for the native desktop window (uses the WebView2 runtime, preinstalled on Windows 11). On other OSes — or with `ZYPER_HEADLESS=1` — it serves the same dashboard in your browser.

## Build & Run

```bash
# Windows desktop build — no console window, just the UI (pure Go, no CGO).
# -H windowsgui sets the GUI subsystem; logs still go to the logs/ folder.
CGO_ENABLED=0 go build -ldflags="-H windowsgui" -o zyper-bot.exe ./cmd/server
# (or just run ./build.ps1)

# run — opens a native desktop window
./zyper-bot.exe
```

> Building without `-ldflags="-H windowsgui"` produces a console-attached exe that
> prints logs to a terminal — handy for debugging, but it shows a console window.

The app stores its data next to the executable: `zyperbot.db` (SQLite), `vault.key`, `logs/`, and reads `.env` from the same folder. Keep the `web/` directory beside the binary.

## Configuration

All secrets live in a local **`.env`** (gitignored) and/or the in-app **Settings** tab — never in source. Copy the template:

```bash
cp .env.example .env
```

| Variable | Purpose |
| --- | --- |
| `ZYPER_ETH_RPC` | Default Ethereum mainnet RPC URL. |
| `OPENSEA_API_KEY` | One or more OpenSea API keys (newline/comma separated) — rotated automatically. |
| `ETHERSCAN_API_KEY` | Etherscan V2 (multichain) key for ABI fetch + tx replay. |
| `ZYPER_AUTH_TOKEN` | Shared secret required for **remote** API access (sent as `X-Auth-Token`). |
| `ZYPER_HEADLESS` | `1` = run server-only and open in a browser (VPS mode). |
| `ZYPER_ADDR` | Bind address (default `127.0.0.1:0`, a random loopback port). |

Keys set in the Settings UI are saved to the DB and override `.env` on the next start.

## Security model

- Private keys are sealed with the vault before they touch disk; the plaintext only exists in memory while a task runs.
- The dashboard binds to a **random loopback port** by default — it is an internal window transport, not a public site.
- **Remote access is refused** unless `ZYPER_AUTH_TOKEN` is set (and you should front it with a TLS reverse proxy).
- Private-key reveal endpoints are off by default (`ZYPER_ALLOW_REVEAL`) and loopback-gated.
- **Never commit `.env`, `vault.key`, or `*.db`.** They are gitignored.

## Tech stack

Go · [go-ethereum](https://github.com/ethereum/go-ethereum) · [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (pure-Go) · [chi](https://github.com/go-chi/chi) router · [go-webview2](https://github.com/jchv/go-webview2) · vanilla HTML/CSS/JS frontend.

## License

For personal and educational use. Provided as-is, without warranty of any kind.
