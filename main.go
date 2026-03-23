package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds all runtime configuration.
// ZulipSite and ZulipBotAPIKey are derived from ZulipGiteaWebhookURL at startup
// and are not set from environment variables directly.
type Config struct {
	ZulipGiteaWebhookURL string
	ZulipSite            string // derived: scheme+host of ZulipGiteaWebhookURL
	ZulipBotEmail        string
	ZulipBotAPIKey       string // derived: api_key param of ZulipGiteaWebhookURL
	WebhookSecret        string
	UIPassword           string
	Port                 string
	UIPort               string
}

func loadConfig() Config {
	webhookURL := os.Getenv("ZULIP_GITEA_WEBHOOK_URL")

	// Derive Zulip site and bot API key from the webhook URL so the operator
	// only needs to configure one Zulip-related URL.
	var zulipSite, zulipBotAPIKey string
	if parsed, err := url.Parse(webhookURL); err == nil && parsed.Host != "" {
		zulipSite = parsed.Scheme + "://" + parsed.Host
		zulipBotAPIKey = parsed.Query().Get("api_key")
	}

	c := Config{
		ZulipGiteaWebhookURL: webhookURL,
		ZulipSite:            zulipSite,
		ZulipBotEmail:        os.Getenv("ZULIP_BOT_EMAIL"),
		ZulipBotAPIKey:       zulipBotAPIKey,
		WebhookSecret:        os.Getenv("WEBHOOK_SECRET"),
		UIPassword:           os.Getenv("UI_PASSWORD"),
		Port:                 os.Getenv("PORT"),
		UIPort:               os.Getenv("UI_PORT"),
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	if c.UIPort == "" {
		c.UIPort = "3000"
	}
	return c
}

type proxy struct {
	cfg    Config
	client *http.Client
	rb     *ringBuf
	bc     *broadcaster
}

func newProxy(cfg Config) *proxy {
	return &proxy{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
		rb:     newRingBuf(ringBufSize),
		bc:     newBroadcaster(),
	}
}

// payload is a generic JSON object.
type payload map[string]any

// validateSignature checks the HMAC-SHA256 signature from Forgejo.
// Returns true if no secret is configured (validation disabled).
func (p *proxy) validateSignature(body []byte, sigHeader string) bool {
	if p.cfg.WebhookSecret == "" {
		return true
	}
	if sigHeader == "" {
		log.Println("warning: missing X-Gitea-Signature header")
		return false
	}
	mac := hmac.New(sha256.New, []byte(p.cfg.WebhookSecret))
	mac.Write(body)
	expected := fmt.Sprintf("%x", mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sigHeader))
}

// resolveStreamAndTopic extracts stream and topic from the incoming request's
// query parameters. Topic falls back to the repository name in the payload.
// Stream falls back to "git". Both can be overridden per-repo by setting
// ?stream=X&topic=Y on the webhook URL registered in Forgejo.
func resolveStreamAndTopic(pl payload, r *http.Request) (stream, topic string) {
	stream = r.URL.Query().Get("stream")
	if stream == "" {
		stream = "git"
	}

	topic = r.URL.Query().Get("topic")
	if topic == "" {
		if repo := getMap(pl, "repository"); repo != nil {
			topic = getString(repo, "name")
		}
	}
	if topic == "" {
		topic = "webhooks"
	}
	return
}

// postToZulipAPI sends a formatted message via the Zulip bot REST API.
func (p *proxy) postToZulipAPI(stream, topic, content string) error {
	if p.cfg.ZulipSite == "" || p.cfg.ZulipBotEmail == "" || p.cfg.ZulipBotAPIKey == "" {
		return fmt.Errorf("ZULIP_GITEA_WEBHOOK_URL / ZULIP_BOT_EMAIL not configured")
	}

	data := url.Values{
		"type":    {"stream"},
		"to":      {stream},
		"topic":   {topic},
		"content": {content},
	}

	req, err := http.NewRequest(http.MethodPost, p.cfg.ZulipSite+"/api/v1/messages", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(p.cfg.ZulipBotEmail+":"+p.cfg.ZulipBotAPIKey),
	))

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to Zulip API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Zulip API returned %d: %s", resp.StatusCode, body)
	}
	log.Printf("posted to Zulip API: %d", resp.StatusCode)
	return nil
}

// forwardToGiteaWebhook forwards a payload to the Zulip Gitea integration endpoint,
// injecting stream and topic derived from the incoming webhook URL's query params.
// A 4xx response from Zulip means the event type is not supported by the integration;
// we log a warning and return nil so Forgejo does not retry (retrying will never succeed).
// A 5xx response is treated as a transient error and returned so Forgejo will retry.
func (p *proxy) forwardToGiteaWebhook(pl payload, eventType, stream, topic string) error {
	if p.cfg.ZulipGiteaWebhookURL == "" {
		return fmt.Errorf("ZULIP_GITEA_WEBHOOK_URL not configured")
	}

	targetURL, err := url.Parse(p.cfg.ZulipGiteaWebhookURL)
	if err != nil {
		return fmt.Errorf("parsing ZULIP_GITEA_WEBHOOK_URL: %w", err)
	}
	q := targetURL.Query()
	q.Set("stream", stream)
	q.Set("topic", topic)
	targetURL.RawQuery = q.Encode()

	body, err := json.Marshal(pl)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, targetURL.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", eventType)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("forwarding to Zulip webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// Client error: Zulip doesn't support this event type. Log and drop —
		// retrying will never succeed.
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("warning: Zulip webhook returned %d for %s (unsupported event, dropping): %s",
			resp.StatusCode, eventType, respBody)
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Zulip webhook returned %d: %s", resp.StatusCode, respBody)
	}
	log.Printf("forwarded %s to Zulip webhook: %d", eventType, resp.StatusCode)
	return nil
}

// --- shared helpers for event handlers ---

// extractEntityFields pulls common fields from a PR or issue payload.
// entityKey is "pull_request" or "issue".
func extractEntityFields(pl payload, entityKey string) (number any, title, htmlURL, repoName, sender string) {
	entity := getMap(pl, entityKey)
	if entity == nil {
		entity = payload{}
	}
	repo := getMap(pl, "repository")
	if repo == nil {
		repo = payload{}
	}

	number = firstNonNil(entity["number"], pl["number"], "?")
	title = getString(entity, "title")
	htmlURL = getString(entity, "html_url")

	repoName = getString(repo, "full_name")
	if repoName == "" {
		repoName = getString(repo, "name")
	}

	if s := getMap(pl, "sender"); s != nil {
		sender = getString(s, "login")
	}
	if sender == "" {
		sender = "someone"
	}
	return
}

// buildRef builds a Markdown reference like "[#5 title](url)" or "#5".
func buildRef(number any, title, htmlURL string) string {
	ref := fmt.Sprintf("#%v", number)
	if title != "" {
		ref = fmt.Sprintf("#%v %s", number, title)
	}
	if htmlURL != "" {
		ref = fmt.Sprintf("[%s](%s)", ref, htmlURL)
	}
	return ref
}

// --- event handlers ---

// handlePullRequestComment remaps pull_request_comment → issue_comment so
// Zulip's Gitea integration can handle it. Zulip checks is_pull=true to know
// the comment is on a PR, and reads issue.{number,title} for context.
//
// Forgejo fires pull_request_comment (X-Gitea-Event) for inline review comments
// (HookEventPullRequestReviewComment). Regular PR thread comments arrive as
// issue_comment and are forwarded directly by the default case.
func (p *proxy) handlePullRequestComment(pl payload, stream, topic string) error {
	pr := getMap(pl, "pull_request")
	if pr == nil {
		pr = payload{}
	}
	transformed := payload{
		"action":  getStringOr(pl, "action", "created"),
		"is_pull": true,
		"issue": payload{
			"number":   firstNonNil(pr["number"], pl["number"], "?"),
			"title":    getString(pr, "title"),
			"body":     getString(pr, "body"),
			"state":    getString(pr, "state"),
			"user":     getMap(pr, "user"),
			"html_url": getString(pr, "html_url"),
		},
		"comment":    getMap(pl, "comment"),
		"repository": getMap(pl, "repository"),
		"sender":     getMap(pl, "sender"),
	}
	return p.forwardToGiteaWebhook(transformed, "issue_comment", stream, topic)
}

// handlePullRequestReviewRequest handles pull_request events where
// action == "review_requested" or "review_request_removed".
//
// Forgejo sends these as X-Gitea-Event: pull_request with the action field set —
// there is no separate pull_request_review_request event type.
// The Gitea integration does not produce a notification for these actions,
// so we post a compact message via the Zulip bot API.
func (p *proxy) handlePullRequestReviewRequest(pl payload, stream, topic string) error {
	number, title, htmlURL, repoName, sender := extractEntityFields(pl, "pull_request")

	reviewer := ""
	if rr := getMap(pl, "requested_reviewer"); rr != nil {
		reviewer = getString(rr, "login")
	}

	var msg string
	action := getString(pl, "action")
	ref := buildRef(number, title, htmlURL)
	switch action {
	case "review_request_removed":
		if reviewer != "" {
			msg = fmt.Sprintf("**[%s]** %s removed review request for %s on %s", repoName, sender, reviewer, ref)
		} else {
			msg = fmt.Sprintf("**[%s]** %s removed a review request on %s", repoName, sender, ref)
		}
	default:
		if reviewer != "" {
			msg = fmt.Sprintf("**[%s]** %s requested review from %s on %s", repoName, sender, reviewer, ref)
		} else {
			msg = fmt.Sprintf("**[%s]** %s requested a review on %s", repoName, sender, ref)
		}
	}

	return p.postToZulipAPI(stream, topic, msg)
}

// handlePullRequestReview handles pull_request_approved and pull_request_rejected
// events by posting a formatted APPROVED/REJECTED message to Zulip via the bot API.
//
// Note: Forgejo currently has a bug where review.content is always empty for
// inline review comments (issue #7935). Messages will still post with the PR link;
// the body will appear once Forgejo fixes the payload.
func (p *proxy) handlePullRequestReview(pl payload, eventType, stream, topic string) error {
	number, title, prURL, repoName, _ := extractEntityFields(pl, "pull_request")

	review := getMap(pl, "review")
	if review == nil {
		review = payload{}
	}

	reviewer := ""
	if user := getMap(review, "user"); user != nil {
		reviewer = getString(user, "login")
	}
	if reviewer == "" {
		if sender := getMap(pl, "sender"); sender != nil {
			reviewer = getString(sender, "login")
		}
	}
	if reviewer == "" {
		reviewer = "someone"
	}

	// Determine prefix from event type and review.type field.
	prefix := "REVIEWED"
	reviewType := getString(review, "type")
	switch {
	case eventType == "pull_request_rejected" || reviewType == "request_changes":
		prefix = "REJECTED"
	case eventType == "pull_request_approved" || reviewType == "approved":
		prefix = "APPROVED"
	}

	msg := fmt.Sprintf("**[%s]** %s: %s on %s", repoName, prefix, reviewer, buildRef(number, title, prURL))

	if body := strings.TrimSpace(getString(review, "content")); body != "" {
		msg += "\n\n> " + body
	}

	return p.postToZulipAPI(stream, topic, msg)
}

// ServeHTTP handles all incoming webhook requests.
//
// Forgejo X-Gitea-Event values and how we handle them:
//
//	pull_request_comment  → remap to issue_comment for Gitea integration
//	                        (fired for inline review comments)
//	pull_request_approved → bot API: APPROVED message
//	pull_request_rejected → bot API: REJECTED message
//	pull_request          → bot API for review_requested/review_request_removed;
//	                        Gitea integration for all other actions (opened, closed,
//	                        synchronized, assigned, label_updated, milestoned, etc.)
//	issues, issue_comment → Gitea integration (opened, closed, assigned, label_updated,
//	                        milestoned, and all comments)
//	push, create, release → Gitea integration
//	everything else       → Gitea integration (dropped with warning if unsupported)
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health check endpoint.
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		fmt.Fprintln(w, "ok")
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusInternalServerError)
		return
	}

	if !p.validateSignature(body, r.Header.Get("X-Gitea-Signature")) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	event := r.Header.Get("X-Gitea-Event")
	log.Printf("received event: %s", event)

	var pl payload
	if err := json.Unmarshal(body, &pl); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	stream, topic := resolveStreamAndTopic(pl, r)

	var handleErr error
	switch event {
	// Inline PR review comment: remap to issue_comment for Zulip's Gitea integration.
	case "pull_request_comment":
		handleErr = p.handlePullRequestComment(pl, stream, topic)
	// PR review (approve/reject): post via Zulip bot API.
	case "pull_request_approved", "pull_request_rejected":
		handleErr = p.handlePullRequestReview(pl, event, stream, topic)
	// pull_request: review_requested/review_request_removed use the bot API;
	// all other actions (opened, closed, synchronized, assigned, label_updated,
	// milestoned, etc.) are handled natively by the Gitea integration.
	case "pull_request":
		action := getString(pl, "action")
		if action == "review_requested" || action == "review_request_removed" {
			handleErr = p.handlePullRequestReviewRequest(pl, stream, topic)
		} else {
			handleErr = p.forwardToGiteaWebhook(pl, event, stream, topic)
		}
	// All other events (push, create, issues, issue_comment, release, etc.):
	// forward to Zulip Gitea webhook. Unknown events are dropped if Zulip returns 4xx.
	default:
		handleErr = p.forwardToGiteaWebhook(pl, event, stream, topic)
	}

	if handleErr != nil {
		log.Printf("error handling %s: %v", event, handleErr)
		http.Error(w, "delivery failed", http.StatusInternalServerError)
		return
	}

	fmt.Fprintln(w, "ok")
}

func main() {
	cfg := loadConfig()
	if cfg.ZulipGiteaWebhookURL == "" {
		log.Fatal("ZULIP_GITEA_WEBHOOK_URL is required")
	}

	p := newProxy(cfg)
	log.SetOutput(newLogWriter(p.rb, p.bc))

	// Start the UI server on its own port.
	uiAddr := "0.0.0.0:" + cfg.UIPort
	go func() {
		log.Printf("UI listening on %s", uiAddr)
		if err := http.ListenAndServe(uiAddr, p.uiHandler()); err != nil {
			log.Fatalf("UI server: %v", err)
		}
	}()

	// Start the webhook proxy server.
	webhookAddr := "0.0.0.0:" + cfg.Port
	log.Printf("webhook proxy listening on %s", webhookAddr)
	if err := http.ListenAndServe(webhookAddr, p); err != nil {
		log.Fatal(err)
	}
}

// --- helpers ---

func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getStringOr(m map[string]any, key, fallback string) string {
	if s := getString(m, key); s != "" {
		return s
	}
	return fallback
}

func getMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if mm, ok := v.(map[string]any); ok {
			return mm
		}
	}
	return nil
}

// firstNonNil returns the first non-nil value from the list.
func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}
