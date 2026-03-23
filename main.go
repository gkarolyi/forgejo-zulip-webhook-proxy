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

// Config holds all runtime configuration from environment variables.
type Config struct {
	ZulipGiteaWebhookURL string
	ZulipSite            string
	ZulipBotEmail        string
	ZulipBotAPIKey       string
	ForgejoSecret        string
	Port                 string
}

func loadConfig() Config {
	c := Config{
		ZulipGiteaWebhookURL: os.Getenv("ZULIP_GITEA_WEBHOOK_URL"),
		ZulipSite:            strings.TrimRight(os.Getenv("ZULIP_SITE"), "/"),
		ZulipBotEmail:        os.Getenv("ZULIP_BOT_EMAIL"),
		ZulipBotAPIKey:       os.Getenv("ZULIP_BOT_API_KEY"),
		ForgejoSecret:        os.Getenv("FORGEJO_SECRET"),
		Port:                 os.Getenv("PORT"),
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	return c
}

type proxy struct {
	cfg    Config
	client *http.Client
}

func newProxy(cfg Config) *proxy {
	return &proxy{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// payload is a generic JSON object.
type payload map[string]any

// validateSignature checks the HMAC-SHA256 signature from Forgejo.
// Returns true if no secret is configured (validation disabled).
func (p *proxy) validateSignature(body []byte, sigHeader string) bool {
	if p.cfg.ForgejoSecret == "" {
		return true
	}
	if sigHeader == "" {
		log.Println("warning: missing X-Gitea-Signature header")
		return false
	}
	mac := hmac.New(sha256.New, []byte(p.cfg.ForgejoSecret))
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
		return fmt.Errorf("ZULIP_SITE / ZULIP_BOT_EMAIL / ZULIP_BOT_API_KEY not configured")
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

// --- PR metadata event handlers ---

// handlePullRequestSync handles pull_request_sync events (new commits pushed to a PR branch).
func (p *proxy) handlePullRequestSync(pl payload, stream, topic string) error {
	number, title, htmlURL, repoName, sender := extractEntityFields(pl, "pull_request")
	msg := fmt.Sprintf("**[%s]** %s synchronized %s", repoName, sender, buildRef(number, title, htmlURL))
	return p.postToZulipAPI(stream, topic, msg)
}

// handlePullRequestReviewRequest handles pull_request_review_request events
// (reviewer added or removed on a PR).
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

// handleAssign handles pull_request_assign and issue_assign events.
// entityKey is "pull_request" or "issue".
func (p *proxy) handleAssign(pl payload, entityKey, stream, topic string) error {
	number, title, htmlURL, repoName, sender := extractEntityFields(pl, entityKey)

	assignee := ""
	if a := getMap(pl, "assignee"); a != nil {
		assignee = getString(a, "login")
	}

	ref := buildRef(number, title, htmlURL)
	var msg string
	if getString(pl, "action") == "unassigned" {
		if assignee != "" {
			msg = fmt.Sprintf("**[%s]** %s unassigned %s from %s", repoName, sender, assignee, ref)
		} else {
			msg = fmt.Sprintf("**[%s]** %s unassigned someone from %s", repoName, sender, ref)
		}
	} else {
		if assignee != "" {
			msg = fmt.Sprintf("**[%s]** %s assigned %s to %s", repoName, sender, assignee, ref)
		} else {
			msg = fmt.Sprintf("**[%s]** %s assigned someone to %s", repoName, sender, ref)
		}
	}

	return p.postToZulipAPI(stream, topic, msg)
}

// handleLabel handles pull_request_label and issue_label events.
// entityKey is "pull_request" or "issue".
func (p *proxy) handleLabel(pl payload, entityKey, stream, topic string) error {
	number, title, htmlURL, repoName, sender := extractEntityFields(pl, entityKey)

	label := ""
	if l := getMap(pl, "label"); l != nil {
		label = getString(l, "name")
	}

	ref := buildRef(number, title, htmlURL)
	var msg string
	if getString(pl, "action") == "label_cleared" {
		if label != "" {
			msg = fmt.Sprintf("**[%s]** %s removed label \"%s\" from %s", repoName, sender, label, ref)
		} else {
			msg = fmt.Sprintf("**[%s]** %s cleared labels on %s", repoName, sender, ref)
		}
	} else {
		if label != "" {
			msg = fmt.Sprintf("**[%s]** %s added label \"%s\" to %s", repoName, sender, label, ref)
		} else {
			msg = fmt.Sprintf("**[%s]** %s updated labels on %s", repoName, sender, ref)
		}
	}

	return p.postToZulipAPI(stream, topic, msg)
}

// handleMilestone handles pull_request_milestone and issue_milestone events.
// entityKey is "pull_request" or "issue".
func (p *proxy) handleMilestone(pl payload, entityKey, stream, topic string) error {
	number, title, htmlURL, repoName, sender := extractEntityFields(pl, entityKey)

	milestone := ""
	if m := getMap(pl, "milestone"); m != nil {
		milestone = getString(m, "title")
	}

	ref := buildRef(number, title, htmlURL)
	var msg string
	if getString(pl, "action") == "demilestoned" {
		msg = fmt.Sprintf("**[%s]** %s removed milestone from %s", repoName, sender, ref)
	} else if milestone != "" {
		msg = fmt.Sprintf("**[%s]** %s set milestone \"%s\" on %s", repoName, sender, milestone, ref)
	} else {
		msg = fmt.Sprintf("**[%s]** %s updated milestone on %s", repoName, sender, ref)
	}

	return p.postToZulipAPI(stream, topic, msg)
}

// handlePullRequestReviewComment handles inline code review comments
// (pull_request_review_comment events). These are line-level comments left during
// a formal review on the "Files changed" tab. Zulip has no handler for this event
// type, so we post a formatted message via the bot API.
func (p *proxy) handlePullRequestReviewComment(pl payload, stream, topic string) error {
	number, title, prURL, repoName, _ := extractEntityFields(pl, "pull_request")

	comment := getMap(pl, "comment")
	if comment == nil {
		comment = payload{}
	}

	commenter := ""
	if user := getMap(comment, "user"); user != nil {
		commenter = getString(user, "login")
	}
	if commenter == "" {
		if sender := getMap(pl, "sender"); sender != nil {
			commenter = getString(sender, "login")
		}
	}
	if commenter == "" {
		commenter = "someone"
	}

	// Include file path and line number if present.
	location := ""
	if path := getString(comment, "path"); path != "" {
		location = fmt.Sprintf(" on `%s`", path)
		if line, ok := comment["line"]; ok && line != nil {
			location = fmt.Sprintf(" on `%s:%v`", path, line)
		}
	}

	commentURL := getString(comment, "html_url")
	commentRef := "commented"
	if commentURL != "" {
		commentRef = fmt.Sprintf("[commented](%s)", commentURL)
	}

	msg := fmt.Sprintf("**[%s]** %s %s%s in %s", repoName, commenter, commentRef, location, buildRef(number, title, prURL))

	if body := strings.TrimSpace(getString(comment, "body")); body != "" {
		msg += "\n\n> " + strings.ReplaceAll(body, "\n", "\n> ")
	}

	return p.postToZulipAPI(stream, topic, msg)
}

// handlePullRequestComment remaps pull_request_comment → issue_comment so
// Zulip's Gitea integration can handle it. Zulip checks is_pull=true to know
// the comment is on a PR, and reads issue.{number,title} for context.
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
	case reviewType == "approved":
		prefix = "APPROVED"
	}

	msg := fmt.Sprintf("**[%s]** %s: %s on %s", repoName, prefix, reviewer, buildRef(number, title, prURL))

	if body := strings.TrimSpace(getString(review, "content")); body != "" {
		msg += "\n\n> " + body
	}

	return p.postToZulipAPI(stream, topic, msg)
}

// ServeHTTP handles all incoming webhook requests.
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
	// PR comment: remap to issue_comment for Zulip's Gitea integration
	case "pull_request_comment":
		handleErr = p.handlePullRequestComment(pl, stream, topic)
	// PR review (approve/reject): post via Zulip bot API
	case "pull_request_approved", "pull_request_rejected":
		handleErr = p.handlePullRequestReview(pl, event, stream, topic)
	// PR inline review comment: post via Zulip bot API
	case "pull_request_review_comment":
		handleErr = p.handlePullRequestReviewComment(pl, stream, topic)
	// PR metadata events: post compact messages via Zulip bot API
	case "pull_request_sync":
		handleErr = p.handlePullRequestSync(pl, stream, topic)
	case "pull_request_review_request":
		handleErr = p.handlePullRequestReviewRequest(pl, stream, topic)
	case "pull_request_assign":
		handleErr = p.handleAssign(pl, "pull_request", stream, topic)
	case "pull_request_label":
		handleErr = p.handleLabel(pl, "pull_request", stream, topic)
	case "pull_request_milestone":
		handleErr = p.handleMilestone(pl, "pull_request", stream, topic)
	// Issue metadata events: post compact messages via Zulip bot API
	case "issue_assign":
		handleErr = p.handleAssign(pl, "issue", stream, topic)
	case "issue_label":
		handleErr = p.handleLabel(pl, "issue", stream, topic)
	case "issue_milestone":
		handleErr = p.handleMilestone(pl, "issue", stream, topic)
	// All other events: forward to Zulip Gitea webhook (natively supported: push,
	// create, pull_request, issues, issue_comment, release). Unknown events are
	// forwarded and dropped if Zulip returns 4xx.
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
	addr := "0.0.0.0:" + cfg.Port
	log.Printf("proxy listening on %s", addr)
	if err := http.ListenAndServe(addr, p); err != nil {
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
