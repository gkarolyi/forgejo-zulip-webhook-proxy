package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeProxy creates a proxy configured to use the given test servers.
func makeProxy(giteaWebhookURL, zulipSite string) *proxy {
	return newProxy(Config{
		ZulipGiteaWebhookURL: giteaWebhookURL,
		ZulipSite:            zulipSite,
		ZulipBotEmail:        "bot@example.com",
		ZulipBotAPIKey:       "test-key",
		ZulipStream:          "git",
		ZulipTopic:           "test-repo",
		Port:                 "8080",
	})
}

// postWebhook sends a webhook request to the proxy and returns the response.
func postWebhook(t *testing.T, p *proxy, event string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", event)

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

// captureServer starts a test HTTP server that records the last request body and
// X-Gitea-Event header, and returns 200.
func captureServer(t *testing.T) (serverURL string, getLastReq func() (event string, body []byte)) {
	t.Helper()
	var lastEvent string
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastEvent = r.Header.Get("X-Gitea-Event")
		lastBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() (string, []byte) { return lastEvent, lastBody }
}

// captureZulipAPI starts a Zulip-API-like server that records the last posted message.
func captureZulipAPI(t *testing.T) (siteURL string, getLastMsg func() string) {
	t.Helper()
	var lastContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			lastContent = r.FormValue("content")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"success"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() string { return lastContent }
}

// --- Tests ---

func TestHealthCheck(t *testing.T) {
	p := makeProxy("http://example.com", "http://example.com")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("health check: got %d, want 200", rr.Code)
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "ok" {
		t.Errorf("health check body: got %q, want %q", body, "ok")
	}
}

func TestPullRequestComment_RemappedToIssueComment(t *testing.T) {
	webhookURL, getLastReq := captureServer(t)
	p := makeProxy(webhookURL, "http://example.com")

	pl := map[string]any{
		"action": "created",
		"pull_request": map[string]any{
			"number":   float64(5),
			"title":    "Fix the bug",
			"html_url": "https://git.example.com/repo/pulls/5",
			"state":    "open",
		},
		"comment": map[string]any{
			"body":     "Looks good",
			"html_url": "https://git.example.com/repo/pulls/5#comment-1",
			"user":     map[string]any{"login": "alice"},
		},
		"repository": map[string]any{"full_name": "owner/repo"},
		"sender":     map[string]any{"login": "alice"},
	}

	rr := postWebhook(t, p, "pull_request_comment", pl)

	if rr.Code != http.StatusOK {
		t.Errorf("response: got %d, want 200", rr.Code)
	}

	event, body := getLastReq()
	if event != "issue_comment" {
		t.Errorf("forwarded event: got %q, want %q", event, "issue_comment")
	}

	var forwarded map[string]any
	if err := json.Unmarshal(body, &forwarded); err != nil {
		t.Fatalf("parsing forwarded payload: %v", err)
	}
	if forwarded["is_pull"] != true {
		t.Errorf("is_pull: got %v, want true", forwarded["is_pull"])
	}
	issue, ok := forwarded["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue field missing or wrong type")
	}
	if issue["number"] != float64(5) {
		t.Errorf("issue.number: got %v, want 5", issue["number"])
	}
	if issue["title"] != "Fix the bug" {
		t.Errorf("issue.title: got %v, want %q", issue["title"], "Fix the bug")
	}
}

func TestPullRequestReviewApproved(t *testing.T) {
	// Use a dummy URL for the gitea webhook (should not be called)
	webhookURL, getLastReq := captureServer(t)

	// Use zulip API server
	zulipSrv, zulipMsg2 := captureZulipAPI(t)
	p2 := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"pull_request": map[string]any{
			"number":   float64(3),
			"title":    "Add feature",
			"html_url": "https://git.example.com/repo/pulls/3",
		},
		"review": map[string]any{
			"type":    "approved",
			"content": "LGTM!",
			"user":    map[string]any{"login": "bob"},
		},
		"repository": map[string]any{
			"full_name": "owner/repo",
			"name":      "repo",
		},
	}

	rr := postWebhook(t, p2, "pull_request_review", pl)

	if rr.Code != http.StatusOK {
		t.Errorf("response: got %d, want 200", rr.Code)
	}

	// Gitea webhook should NOT have been called
	_, body := getLastReq()
	if len(body) > 0 {
		t.Errorf("gitea webhook should not be called for review events")
	}

	msg := zulipMsg2()
	if !strings.Contains(msg, "APPROVED") {
		t.Errorf("message should contain APPROVED, got: %q", msg)
	}
	if !strings.Contains(msg, "bob") {
		t.Errorf("message should contain reviewer name, got: %q", msg)
	}
	if !strings.Contains(msg, "LGTM!") {
		t.Errorf("message should contain review body, got: %q", msg)
	}
}

func TestPullRequestReviewRejected(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"pull_request": map[string]any{
			"number":   float64(7),
			"title":    "Refactor auth",
			"html_url": "https://git.example.com/repo/pulls/7",
		},
		"review": map[string]any{
			"type":    "request_changes",
			"content": "Please add error handling",
			"user":    map[string]any{"login": "carol"},
		},
		"repository": map[string]any{
			"full_name": "owner/repo",
			"name":      "repo",
		},
	}

	rr := postWebhook(t, p, "pull_request_review_rejected", pl)

	if rr.Code != http.StatusOK {
		t.Errorf("response: got %d, want 200", rr.Code)
	}

	msg := getMsg()
	if !strings.Contains(msg, "REJECTED") {
		t.Errorf("message should contain REJECTED, got: %q", msg)
	}
	if !strings.Contains(msg, "carol") {
		t.Errorf("message should contain reviewer name, got: %q", msg)
	}
	if !strings.Contains(msg, "Please add error handling") {
		t.Errorf("message should contain review body, got: %q", msg)
	}
}

func TestPushEventForwarded(t *testing.T) {
	webhookURL, getLastReq := captureServer(t)
	p := makeProxy(webhookURL, "http://example.com")

	pl := map[string]any{
		"ref":        "refs/heads/main",
		"commits":    []any{},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "push", pl)

	if rr.Code != http.StatusOK {
		t.Errorf("response: got %d, want 200", rr.Code)
	}

	event, _ := getLastReq()
	if event != "push" {
		t.Errorf("forwarded event: got %q, want %q", event, "push")
	}
}

func TestInvalidSignatureRejected(t *testing.T) {
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, "http://example.com")
	p.cfg.ForgejoSecret = "mysecret"

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", "push")
	req.Header.Set("X-Gitea-Signature", "sha256=wrongsignature")

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", rr.Code)
	}
}

func TestZulipFailureReturns500(t *testing.T) {
	// Webhook server returns 500
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	p := makeProxy(failSrv.URL, "http://example.com")

	pl := map[string]any{"ref": "refs/heads/main", "repository": map[string]any{"full_name": "owner/repo"}}
	rr := postWebhook(t, p, "push", pl)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500 when Zulip fails", rr.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	p := makeProxy("http://example.com", "http://example.com")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d, want 405", rr.Code)
	}
}

func TestPullRequestReviewComment(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"pull_request": map[string]any{
			"number":   float64(9),
			"title":    "Add logging",
			"html_url": "https://git.example.com/repo/pulls/9",
		},
		"comment": map[string]any{
			"body":     "This function needs a timeout",
			"path":     "main.go",
			"line":     float64(42),
			"html_url": "https://git.example.com/repo/pulls/9#discussion_r1",
			"user":     map[string]any{"login": "gergely"},
		},
		"repository": map[string]any{
			"full_name": "owner/repo",
			"name":      "repo",
		},
	}

	rr := postWebhook(t, p, "pull_request_review_comment", pl)

	if rr.Code != http.StatusOK {
		t.Errorf("response: got %d, want 200", rr.Code)
	}

	msg := getMsg()
	if !strings.Contains(msg, "gergely") {
		t.Errorf("message should contain commenter name, got: %q", msg)
	}
	if !strings.Contains(msg, "main.go") {
		t.Errorf("message should contain file path, got: %q", msg)
	}
	if !strings.Contains(msg, "This function needs a timeout") {
		t.Errorf("message should contain comment body, got: %q", msg)
	}
	if !strings.Contains(msg, "#9") {
		t.Errorf("message should reference PR number, got: %q", msg)
	}
}

func TestPullRequestSync(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "synchronized",
		"pull_request": map[string]any{
			"number":   float64(12),
			"title":    "My feature",
			"html_url": "https://git.example.com/repo/pulls/12",
		},
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "pull_request_sync", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "alice") {
		t.Errorf("message should contain sender, got: %q", msg)
	}
	if !strings.Contains(msg, "synchronized") {
		t.Errorf("message should contain 'synchronized', got: %q", msg)
	}
	if !strings.Contains(msg, "#12") {
		t.Errorf("message should reference PR, got: %q", msg)
	}
}

func TestPullRequestReviewRequest(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "review_requested",
		"pull_request": map[string]any{
			"number":   float64(3),
			"title":    "Add feature",
			"html_url": "https://git.example.com/repo/pulls/3",
		},
		"requested_reviewer": map[string]any{"login": "bob"},
		"sender":             map[string]any{"login": "alice"},
		"repository":         map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "pull_request_review_request", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "alice") {
		t.Errorf("message should contain sender, got: %q", msg)
	}
	if !strings.Contains(msg, "bob") {
		t.Errorf("message should contain requested reviewer, got: %q", msg)
	}
	if !strings.Contains(msg, "#3") {
		t.Errorf("message should reference PR, got: %q", msg)
	}
}

func TestPullRequestAssign(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "assigned",
		"pull_request": map[string]any{
			"number":   float64(7),
			"title":    "Refactor",
			"html_url": "https://git.example.com/repo/pulls/7",
		},
		"assignee":   map[string]any{"login": "carol"},
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "pull_request_assign", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "carol") {
		t.Errorf("message should contain assignee, got: %q", msg)
	}
	if !strings.Contains(msg, "assigned") {
		t.Errorf("message should contain 'assigned', got: %q", msg)
	}
}

func TestPullRequestLabel(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "label_updated",
		"pull_request": map[string]any{
			"number":   float64(2),
			"title":    "Fix bug",
			"html_url": "https://git.example.com/repo/pulls/2",
		},
		"label":      map[string]any{"name": "bug"},
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "pull_request_label", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "bug") {
		t.Errorf("message should contain label name, got: %q", msg)
	}
}

func TestPullRequestMilestone(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "milestoned",
		"pull_request": map[string]any{
			"number":   float64(4),
			"title":    "Ship it",
			"html_url": "https://git.example.com/repo/pulls/4",
		},
		"milestone":  map[string]any{"title": "v1.0"},
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "pull_request_milestone", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "v1.0") {
		t.Errorf("message should contain milestone name, got: %q", msg)
	}
}

func TestIssueAssign(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "assigned",
		"issue": map[string]any{
			"number":   float64(10),
			"title":    "Investigate crash",
			"html_url": "https://git.example.com/repo/issues/10",
		},
		"assignee":   map[string]any{"login": "dave"},
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "issue_assign", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "dave") {
		t.Errorf("message should contain assignee, got: %q", msg)
	}
	if !strings.Contains(msg, "#10") {
		t.Errorf("message should reference issue, got: %q", msg)
	}
}

func TestIssueLabel(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "label_updated",
		"issue": map[string]any{
			"number":   float64(11),
			"title":    "Perf issue",
			"html_url": "https://git.example.com/repo/issues/11",
		},
		"label":      map[string]any{"name": "enhancement"},
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "issue_label", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "enhancement") {
		t.Errorf("message should contain label name, got: %q", msg)
	}
}

func TestIssueMilestone(t *testing.T) {
	zulipSrv, getMsg := captureZulipAPI(t)
	webhookURL, _ := captureServer(t)
	p := makeProxy(webhookURL, zulipSrv)

	pl := map[string]any{
		"action": "milestoned",
		"issue": map[string]any{
			"number":   float64(15),
			"title":    "Track progress",
			"html_url": "https://git.example.com/repo/issues/15",
		},
		"milestone":  map[string]any{"title": "v2.0"},
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "owner/repo"},
	}

	rr := postWebhook(t, p, "issue_milestone", pl)
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	msg := getMsg()
	if !strings.Contains(msg, "v2.0") {
		t.Errorf("message should contain milestone name, got: %q", msg)
	}
}

func TestZulip4xxDropped(t *testing.T) {
	// Zulip returns 400 (unsupported event) — proxy should return 200 (don't retry)
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer badSrv.Close()

	p := makeProxy(badSrv.URL, "http://example.com")

	pl := map[string]any{"repository": map[string]any{"full_name": "owner/repo"}}
	rr := postWebhook(t, p, "some_unknown_event", pl)

	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200 — 4xx from Zulip should be dropped not retried", rr.Code)
	}
}
