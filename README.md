# forgejo-zulip-webhook-proxy

A small Go proxy that fills the gap between Forgejo's webhook events and Zulip's built-in Gitea integration.

## Why

Zulip's Gitea integration handles a subset of events natively (`push`, `create`, `pull_request`, `issues`, `issue_comment`, `release`) but doesn't support:

- `pull_request_comment` — Forgejo uses a different header than `issue_comment`, causing 400s from Zulip
- `pull_request_approved` / `pull_request_rejected` — review approvals and rejections
- `pull_request` action=`review_requested` — reviewer assignment notifications

This proxy sits between Forgejo and Zulip and handles all of them.

## What it does

| Forgejo event | Action |
|---|---|
| `pull_request_comment` (action=`created`/`edited`/`deleted`) | Remaps to `issue_comment` format, forwards to Zulip Gitea webhook |
| `pull_request_comment` action=`reviewed` | Posts `REVIEWED: reviewer on #N title` via Zulip bot API (review submitted without approve/reject) |
| `pull_request_approved` | Posts `APPROVED: reviewer on #N title` via Zulip bot API |
| `pull_request_rejected` | Posts `REQUESTED CHANGES: reviewer on #N title` via Zulip bot API |
| `pull_request` action=`review_requested` | Posts `user requested review from X on #N title` via Zulip bot API |
| `pull_request` (all other actions) | Forwarded as-is to Zulip Gitea webhook |
| `push`, `create`, `issues`, `issue_comment`, `release` | Forwarded as-is to Zulip Gitea webhook |
| Unknown events | Forwarded; logged and dropped if Zulip returns 4xx (unsupported event type) |

## Setup

### 1. Create a Zulip bot

In Zulip: **Settings → Bots → Add a new bot** (Generic bot). Note the email and API key.

### 2. Get the Gitea webhook URL

In Zulip: **Settings → Integrations → Gitea** — follow the steps shown. Copy the webhook URL (it ends with `?api_key=...`). This is your `ZULIP_GITEA_WEBHOOK_URL`.

### 3. Run with Docker Compose

Create a `docker-compose.yml`:

```yaml
services:
  proxy:
    image: ghcr.io/gkarolyi/forgejo-zulip-webhook-proxy:latest
    restart: unless-stopped
    ports:
      - "8080:8080"   # webhook endpoint
      - "3000:3000"   # web UI
    environment:
      ZULIP_GITEA_WEBHOOK_URL: "https://chat.example.org/api/v1/external/gitea?api_key=XXX"
      ZULIP_BOT_EMAIL: "bot@example.org"
      # WEBHOOK_SECRET: "..."   # optional: shared secret for HMAC signature validation
      # UI_PASSWORD: "..."      # optional: password for web UI Basic auth
```

Then start it:

```bash
docker compose up -d
docker compose logs -f   # check it's running
```

To build from source instead, replace `image:` with `build: .`.

### 4. Point Forgejo webhooks at the proxy

In each Forgejo repository, add a webhook with the URL:

```
http://your-server:8080/?stream=git&topic=my-repo-name
```

- `stream` — the Zulip stream to post to (default: `git` if omitted)
- `topic` — the Zulip topic to post to (default: the repository name from the payload if omitted)

By encoding stream and topic in the webhook URL, each repo can route to its own Zulip topic without any proxy configuration changes. Select all events you want forwarded.

### 5. Optional: signature validation

Set `WEBHOOK_SECRET` to the same value as the Forgejo webhook secret. The proxy will reject requests with invalid HMAC-SHA256 signatures.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `ZULIP_GITEA_WEBHOOK_URL` | Yes | Zulip Gitea integration URL. Find it in Zulip: **Settings → Integrations → Gitea** — copy the webhook URL shown there (it includes `api_key`). Example: `https://chat.example.org/api/v1/external/gitea?api_key=XXX`. The site URL and bot API key are derived from this. Do not add `stream` or `topic` params here; those come from each Forgejo webhook URL. |
| `ZULIP_BOT_EMAIL` | Yes | Bot email for posting review/reviewer notifications via Zulip API |
| `WEBHOOK_SECRET` | No | Shared secret for HMAC signature validation of incoming Forgejo webhooks |
| `UI_PASSWORD` | No | Password for web UI Basic auth (any username). No auth if unset. |
| `PORT` | No | Webhook listener port (default: 8080) |
| `UI_PORT` | No | Web UI listener port (default: 3000) |

## Development

### Prerequisites

- Go 1.23+

### Setup (first time)

```bash
git config core.hooksPath .githooks
```

This installs the pre-commit hook that runs `go test ./...` before every commit.

### Run tests

```bash
go test ./...
```

### Build

```bash
go build -o proxy .
```

### Run locally

```bash
ZULIP_GITEA_WEBHOOK_URL="https://chat.example.org/api/v1/external/gitea?api_key=XXX" \
ZULIP_BOT_EMAIL="bot@example.org" \
./proxy
```

### Docker

```bash
# Build (also runs tests)
docker build -t forgejo-zulip-webhook-proxy .

# Run
docker compose up -d
```

## Web UI

The proxy runs a dedicated web UI server on `UI_PORT` (default: 3000). Accessing `http://your-server:3000/` opens a single-page interface with:

- A **test connection** button — sends a `pull_request_comment` self-test event through the proxy's own handler (`POST /test`), forwarding it to Zulip's Gitea integration so you can confirm end-to-end delivery
- A **live log view** — streams proxy log lines via Server-Sent Events (`GET /logs`), showing the last 200 lines on connect plus new events in real time

Point Traefik or another reverse proxy at port 3000 for easy access.

Set `UI_PASSWORD` to require Basic auth (any username, the env var value as password). If unset, the UI is open to anyone who can reach the port.

## Health check

`GET /health` returns `200 ok` on both the webhook port (8080) and the UI port (3000). Used by Docker's `HEALTHCHECK` directive.

## Caveats

Forgejo currently has a bug ([issue #7935](https://codeberg.org/forgejo/forgejo/issues/7935)) where `review.content` is always empty for inline review comments. Review notifications will still post with the PR link; the body text will appear once Forgejo fixes the payload.
