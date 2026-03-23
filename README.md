# forgejo-zulip-webhook-proxy

A small Go proxy that fills the gap between Forgejo's webhook events and Zulip's built-in Gitea integration.

## Why

Zulip's Gitea integration handles a subset of events natively (`push`, `create`, `pull_request`, `issues`, `issue_comment`, `release`) but doesn't support:

- `pull_request_comment` — Forgejo uses a different header than `issue_comment`, causing 400s from Zulip
- `pull_request_review` / `pull_request_review_rejected` — review approvals and rejections
- `pull_request_review_comment` — inline code review comments
- `pull_request_sync`, `pull_request_review_request` — PR workflow notifications
- `pull_request_assign/label/milestone` and `issue_assign/label/milestone` — metadata changes

This proxy sits between Forgejo and Zulip and handles all of them.

## What it does

| Forgejo event | Action |
|---|---|
| `pull_request_comment` | Remaps to `issue_comment` format, forwards to Zulip Gitea webhook |
| `pull_request_review` | Posts `APPROVED: reviewer on #N title` via Zulip bot API |
| `pull_request_review_rejected` | Posts `REJECTED: reviewer on #N title` via Zulip bot API |
| `pull_request_review_comment` | Posts `user commented on file:line in #N title` via Zulip bot API |
| `pull_request_sync` | Posts `user synchronized #N title` via Zulip bot API |
| `pull_request_review_request` | Posts `user requested review from X on #N title` via Zulip bot API |
| `pull_request_assign` | Posts `user assigned X to #N title` via Zulip bot API |
| `pull_request_label` | Posts `user added label "X" to #N title` via Zulip bot API |
| `pull_request_milestone` | Posts `user set milestone "X" on #N title` via Zulip bot API |
| `issue_assign` | Posts `user assigned X to #N title` via Zulip bot API |
| `issue_label` | Posts `user added label "X" to #N title` via Zulip bot API |
| `issue_milestone` | Posts `user set milestone "X" on #N title` via Zulip bot API |
| `push`, `create`, `pull_request`, `issues`, `issue_comment`, `release` | Forwarded as-is to Zulip Gitea webhook |
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

In Forgejo, set the webhook URL to `http://your-server:8080/` and select all the events you want.

The proxy URL replaces the Zulip Gitea integration URL in Forgejo — keep the integration URL in `ZULIP_GITEA_WEBHOOK_URL` in the proxy config.

### 4. Optional: signature validation

Set `FORGEJO_SECRET` to the same value as the Forgejo webhook secret. The proxy will reject requests with invalid HMAC-SHA256 signatures.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `ZULIP_GITEA_WEBHOOK_URL` | Yes | Full Zulip Gitea integration URL (with `api_key`, `stream`, `topic` params) |
| `ZULIP_SITE` | Yes | Zulip instance base URL, e.g. `https://chat.example.org` |
| `ZULIP_BOT_EMAIL` | Yes | Bot email for posting via Zulip API |
| `ZULIP_BOT_API_KEY` | Yes | Bot API key |
| `ZULIP_STREAM` | No | Override stream for bot API messages (defaults to stream from webhook URL) |
| `ZULIP_TOPIC` | No | Override topic for bot API messages (defaults to repo name) |
| `FORGEJO_SECRET` | No | Shared secret for HMAC signature validation |
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
ZULIP_GITEA_WEBHOOK_URL="https://..." \
ZULIP_SITE="https://chat.example.org" \
ZULIP_BOT_EMAIL="bot@example.org" \
ZULIP_BOT_API_KEY="your-key" \
./proxy
```

### Docker

```bash
# Build (also runs tests)
docker build -t forgejo-zulip-webhook-proxy .

# Run
docker compose up -d
```

## Health check

`GET /health` returns `200 ok`. Used by Docker's `HEALTHCHECK` directive.

## Caveats

Forgejo currently has a bug ([issue #7935](https://codeberg.org/forgejo/forgejo/issues/7935)) where `review.content` is always empty for inline review comments. Review notifications will still post with the PR link; the body text will appear once Forgejo fixes the payload.
