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
| `pull_request_comment` | Remaps to `issue_comment` format, forwards to Zulip Gitea webhook |
| `pull_request_approved` | Posts `APPROVED: reviewer on #N title` via Zulip bot API |
| `pull_request_rejected` | Posts `REJECTED: reviewer on #N title` via Zulip bot API |
| `pull_request` action=`review_requested` | Posts `user requested review from X on #N title` via Zulip bot API |
| `pull_request` (all other actions) | Forwarded as-is to Zulip Gitea webhook |
| `push`, `create`, `issues`, `issue_comment`, `release` | Forwarded as-is to Zulip Gitea webhook |
| Unknown events | Forwarded; silently dropped if Zulip returns 4xx (unsupported) |

## Setup

### 1. Create a Zulip bot

In Zulip: Settings → Bots → Add a new bot (Generic bot). Note the email and API key.

### 2. Configure and run

```bash
cp docker-compose.yml docker-compose.override.yml  # or edit directly
# Fill in the env vars (see docker-compose.yml comments)
docker compose up -d
```

### 3. Point Forgejo webhooks at the proxy

In each Forgejo repository, add a webhook with the URL:

```
http://your-server:8080/?stream=git&topic=my-repo-name
```

- `stream` — the Zulip stream to post to (default: `git` if omitted)
- `topic` — the Zulip topic to post to (default: the repository name from the payload if omitted)

By encoding stream and topic in the webhook URL, each repo can route to its own Zulip topic without any proxy configuration changes. Select all events you want forwarded.

### 4. Optional: signature validation

Set `WEBHOOK_SECRET` to the same value as the Forgejo webhook secret. The proxy will reject requests with invalid HMAC-SHA256 signatures.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `ZULIP_GITEA_WEBHOOK_URL` | Yes | Zulip Gitea integration URL with `api_key` param (e.g. `https://chat.example.org/api/v1/external/gitea?api_key=XXX`). The site URL and bot API key are derived from this. |
| `ZULIP_BOT_EMAIL` | Yes | Bot email for posting review/reviewer notifications via Zulip API |
| `WEBHOOK_SECRET` | No | Shared secret for HMAC signature validation of incoming Forgejo webhooks |
| `UI_PASSWORD` | No | Password for the `/ui` web interface (Basic auth, any username). No auth if unset. |
| `PORT` | No | Port to listen on (default: 8080) |

## Development

### Prerequisites

- Go 1.23+

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

`GET /ui` serves a single-page interface with:

- A **test connection** button — sends a dummy message via the Zulip bot API to confirm credentials are working
- A **live log view** — streams proxy log lines via Server-Sent Events (`GET /ui/logs`), showing the last 200 lines on connect plus new events in real time

Set `UI_PASSWORD` to require Basic auth (any username, the env var value as password). If unset, `/ui` is open to anyone who can reach the proxy.

## Health check

`GET /health` returns `200 ok`. Used by Docker's `HEALTHCHECK` directive.

## Caveats

Forgejo currently has a bug ([issue #7935](https://codeberg.org/forgejo/forgejo/issues/7935)) where `review.content` is always empty for inline review comments. Review notifications will still post with the PR link; the body text will appear once Forgejo fixes the payload.
