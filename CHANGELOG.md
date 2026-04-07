# Changelog

All notable changes to this project will be documented in this file.

## [v1.0.1] - 2026-04-07

### Fixed
- `pull_request_comment` events with `action=reviewed` (PR review submissions without approve/reject) now post a `REVIEWED` message via the Zulip bot API instead of crashing with a Zulip 500. Forgejo sends these with no `comment` field; the review body is in `review.content`.

## [v1.0.0] - 2026-03-25

First stable release.

### Features
- Proxies Forgejo webhook events to Zulip's Gitea integration and bot API
- `pull_request_comment` → remapped to `issue_comment` for Zulip's Gitea integration
- `pull_request_approved` / `pull_request_rejected` → `APPROVED` / `REQUESTED CHANGES` messages via Zulip bot API
- `pull_request` action=`review_requested` / `review_request_removed` → reviewer assignment notifications via bot API
- Stream and topic routing per repository via webhook URL query params (`?stream=git&topic=my-repo`)
- Web UI on a separate port (default 3000) with live SSE log streaming and connection test button
- Optional HMAC-SHA256 webhook signature validation (`WEBHOOK_SECRET`)
- Optional Basic Auth for the web UI (`UI_PASSWORD`)
- Docker image with health check on both ports

[v1.0.1]: https://github.com/gkarolyi/forgejo-zulip-webhook-proxy/releases/tag/v1.0.1
[v1.0.0]: https://github.com/gkarolyi/forgejo-zulip-webhook-proxy/releases/tag/v1.0.0
