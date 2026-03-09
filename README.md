# TrackAlph

Telegram notifications for Alephium wallet activity, built as small Go services.

## What It Does

- Watches user-provided Alephium addresses
- Sends Telegram alerts for incoming/outgoing activity
- Handles transfers and contract interactions
- Supports per-address filter: `all`, `in`, `out`
- Uses Redis for shared state + Redis Streams between services

## Architecture

- `scraper` (`cmd/scraper`): connects to Alephium websocket and publishes block events
- `matcher` (`cmd/matcher`): fetches tx details, computes flow, formats notifications
- `bot` (`cmd/bot`): handles Telegram commands and delivers Telegram notifications

Streams:
- `trackalph:blocks` (scraper -> matcher)
- `trackalph:notifications` (matcher -> bot)

State:
- Redis keys managed in `internal/store/redis.go`

## Requirements

- Go `1.25+`
- Redis `7+`
- Telegram bot token from [@BotFather](https://t.me/BotFather)

## Configuration

Copy `.env.example` to `.env` and edit values:

```env
TELEGRAM_TOKEN=your-telegram-bot-token
FULLNODE_WS=node.mainnet.alephium.org
FULLNODE_API=https://node.mainnet.alephium.org
EXPLORER_API=https://backend.mainnet.alephium.org
EXPLORER_URL=https://explorer.alephium.org
TOKEN_LIST_URL=https://raw.githubusercontent.com/alephium/token-list/refs/heads/master/tokens/mainnet.json
REDIS_URL=redis://localhost:6379
```

## Run With Docker Compose

```bash
docker compose up --build
```

This starts:
- `redis`
- `scraper`
- `matcher`
- `bot`

## Run Locally (without Docker)

Start Redis first, then in separate terminals:

```bash
go run ./cmd/scraper
go run ./cmd/matcher
go run ./cmd/bot
```

## Telegram Commands

- `/watch <address>`: watch address (bot shows filter buttons)
- `/unwatch <address>`: stop watching address
- `/list`: show watched addresses and filters
- `/help`: help message

Aliases:
- `/add` = `/watch`
- `/remove` = `/unwatch`

## Metrics

Each active service exposes Prometheus metrics:

- `scraper`: `:2112/metrics`
- `matcher`: `:2113/metrics`
- `bot`: `:2114/metrics`

Examples of exposed metrics:
- `trackalph_scraper_blocks_published_total`
- `trackalph_matcher_tx_processed_total`
- `trackalph_matcher_tx_process_duration_seconds`
- `trackalph_bot_messages_sent_total`

## Grafana Dashboard

Dashboard JSON is included at:

- `observability/grafana/dashboards/trackalph-overview.json`

Import it manually in Grafana:
1. Open Grafana -> **Dashboards** -> **Import**
2. Upload `trackalph-overview.json`
3. Select your Prometheus datasource

Note: this repo currently does **not** run Grafana/Prometheus via `docker-compose.yml`.

## Webhook Channel Status

Webhook notification channel code exists (`cmd/webhook`) but is currently disabled in the running flow.
Telegram is the active notification channel.

## Project Layout

- `cmd/scraper/main.go`
- `cmd/matcher/main.go`
- `cmd/bot/main.go`
- `cmd/webhook/main.go` (currently inactive)
- `internal/config`
- `internal/explorer`
- `internal/metrics`
- `internal/models`
- `internal/store`
- `internal/stream`
- `internal/tokens`

