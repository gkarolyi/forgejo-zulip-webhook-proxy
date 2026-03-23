# Coding Agent Instructions

## Language & toolchain

- **Go 1.23** — no external dependencies, stdlib only
- All logic is in `main.go`; tests are in `main_test.go`

## Before committing any change

Run tests and ensure they pass:

```bash
go test ./...
```

Build the binary and confirm it compiles:

```bash
go build -o /tmp/proxy .
```

Both must succeed with no errors before pushing.

## Testing approach

Tests use `net/http/httptest` — no external services required. Each test:
1. Spins up a mock Zulip endpoint (webhook or bot API) using `httptest.NewServer`
2. Sends a webhook payload directly to `proxy.ServeHTTP`
3. Asserts the correct downstream call was made

When adding a new event handler, add a corresponding test in `main_test.go` that covers:
- The happy path (correct downstream call made, 200 returned to caller)
- Downstream failure (Zulip returns 5xx → proxy returns 500)

## Docker

The Dockerfile runs `go test ./...` before building, so `docker build` will fail if tests are broken. This is intentional — do not remove the test step.

To verify the Docker build locally:

```bash
docker build -t forgejo-zulip-webhook-proxy .
```

After building, smoke-test the health endpoint:

```bash
docker run --rm -e ZULIP_GITEA_WEBHOOK_URL=http://localhost -p 8080:8080 forgejo-zulip-webhook-proxy &
curl http://localhost:8080/health
```

## Environment variables

All config comes from env vars — see `loadConfig()` in `main.go` and the table in `README.md`.
`ZULIP_GITEA_WEBHOOK_URL` is the only required variable; the proxy will exit on startup if it's missing.

## Key behaviours to preserve

- `pull_request_comment` must be remapped to `issue_comment` (with `is_pull: true` and `pull_request` → `issue`) before forwarding
- `pull_request_review` and `pull_request_review_rejected` must post via the Zulip bot API (not the Gitea webhook)
- Any Zulip delivery failure must return HTTP 500 to Forgejo (so it retries)
- `GET /health` must always return 200
