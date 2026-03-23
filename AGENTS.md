# Coding Agent Instructions

## Language & toolchain

- **Go 1.23** — no external dependencies, stdlib only
- `main.go` — all proxy logic (handlers, routing, helpers)
- `main_test.go` — all tests

## Before committing any change

```bash
go test ./...
go build -o /tmp/proxy .
```

Both must succeed with no errors before pushing.

## Testing approach

Tests use `net/http/httptest` — no external services required. Each test:
1. Spins up mock Zulip endpoints using `httptest.NewServer`
2. Sends a webhook payload directly to `proxy.ServeHTTP`
3. Asserts the correct downstream call was made

When adding a new event handler, add a corresponding test in `main_test.go` that covers the happy path (correct message sent, 200 returned to caller).

## Docker

The Dockerfile runs `go test ./...` before building, so `docker build` will fail if tests are broken. Do not remove the test step.

```bash
docker build -t forgejo-zulip-webhook-proxy .
# Smoke-test the health endpoint
docker run --rm -e ZULIP_GITEA_WEBHOOK_URL=http://localhost -p 8080:8080 forgejo-zulip-webhook-proxy &
curl http://localhost:8080/health
```

## Code structure

`main.go` is organised in sections:

1. **Config & proxy struct** — `Config`, `loadConfig`, `proxy`, `newProxy`
2. **Core delivery** — `postToZulipAPI` (bot API), `forwardToGiteaWebhook` (Gitea webhook)
3. **Shared helpers** — `extractEntityFields`, `buildRef` (used by all event handlers)
4. **PR event handlers** — `handlePullRequestSync`, `handlePullRequestReviewRequest`, `handleAssign`, `handleLabel`, `handleMilestone`, `handlePullRequestReviewComment`, `handlePullRequestComment`, `handlePullRequestReview`
5. **ServeHTTP / main** — routing switch and entry point
6. **Low-level helpers** — `getString`, `getStringOr`, `getMap`, `firstNonNil`

## Key behaviours to preserve

- `pull_request_comment` must be remapped to `issue_comment` (with `is_pull: true`) before forwarding to the Gitea webhook — Zulip won't accept the original event name
- `pull_request_review` / `pull_request_review_rejected` must post via the Zulip bot API with APPROVED/REJECTED prefix
- `pull_request_review_comment` must post via the bot API with `file:line` context
- PR and issue metadata events (`*_assign`, `*_label`, `*_milestone`) share generic handlers — pass `"pull_request"` or `"issue"` as `entityKey`
- Any Zulip **5xx** response must return HTTP 500 to Forgejo (triggers retry)
- Any Zulip **4xx** response must return HTTP 200 to Forgejo (event is unsupported; retrying will never succeed)
- `GET /health` must always return 200
