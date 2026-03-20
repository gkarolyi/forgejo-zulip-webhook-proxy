# forgejo-zulip-webhook-proxy

A tiny proxy that fills the gap between Forgejo's webhook events and Zulip's built-in Gitea integration.

## Why

Zulip's Gitea integration handles `push` and basic `pull_request` events (open/close/merge) but doesn't know about:
- `pull_request_comment` — Forgejo uses this header (not `issue_comment`) for PR comments, causing a 400 from Zulip
- `pull_request_review_rejected` — "request changes" reviews; Zulip has no handler at all

This proxy sits between Forgejo and Zulip and fixes both.

## What it does

| Forgejo event | Proxy action |
|---|---|
| `pull_request_comment` | Remaps payload to `issue_comment` format, forwards to Zulip Gitea webhook |
| `pull_request_review_rejected` | Formats a message, posts via Zulip bot API |
| Everything else (`push`, `pull_request`, etc.) | Forwarded as-is to Zulip Gitea webhook |

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

The proxy URL replaces the Zulip Gitea integration URL in Forgejo — leave the integration URL in `ZULIP_GITEA_WEBHOOK_URL` in the proxy config.

### 4. Optional: signature validation

Set `FORGEJO_SECRET` to the same value as the Forgejo webhook secret. The proxy will reject requests with invalid HMAC-SHA256 signatures.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `ZULIP_GITEA_WEBHOOK_URL` | Yes | Full Zulip Gitea integration URL (with `api_key`, `stream`, `topic` params) |
| `ZULIP_SITE` | For reviews | Zulip instance base URL, e.g. `https://chat.example.org` |
| `ZULIP_BOT_EMAIL` | For reviews | Bot email |
| `ZULIP_BOT_API_KEY` | For reviews | Bot API key |
| `ZULIP_STREAM` | No | Override stream for review notifications (defaults to stream from webhook URL) |
| `ZULIP_TOPIC` | No | Override topic for review notifications (defaults to repo name) |
| `FORGEJO_SECRET` | No | Shared secret for HMAC signature validation |
| `PORT` | No | Port to listen on (default: 8080) |
