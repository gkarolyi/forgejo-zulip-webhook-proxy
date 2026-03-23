package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

const ringBufSize = 200

// --- ring buffer ---

type ringBuf struct {
	mu    sync.Mutex
	lines []string
	pos   int
	full  bool
}

func newRingBuf(size int) *ringBuf {
	return &ringBuf{lines: make([]string, size)}
}

func (rb *ringBuf) add(line string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.lines[rb.pos] = line
	rb.pos = (rb.pos + 1) % len(rb.lines)
	if rb.pos == 0 {
		rb.full = true
	}
}

func (rb *ringBuf) snapshot() []string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := rb.pos
	if rb.full {
		n = len(rb.lines)
	}
	out := make([]string, n)
	if rb.full {
		copy(out, rb.lines[rb.pos:])
		copy(out[len(rb.lines)-rb.pos:], rb.lines[:rb.pos])
	} else {
		copy(out, rb.lines[:rb.pos])
	}
	return out
}

// --- broadcaster (fan-out to SSE clients) ---

type broadcaster struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{clients: make(map[chan string]struct{})}
}

func (b *broadcaster) subscribe() chan string {
	ch := make(chan string, 64)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *broadcaster) send(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- line:
		default: // slow client, drop
		}
	}
}

// --- custom log writer ---

type multiLogWriter struct {
	rb   *ringBuf
	bc   *broadcaster
	dest io.Writer
}

func (w *multiLogWriter) Write(p []byte) (n int, err error) {
	line := strings.TrimRight(string(p), "\n")
	if line != "" {
		w.rb.add(line)
		w.bc.send(line)
	}
	return w.dest.Write(p)
}

// newLogWriter returns a writer that tees to stderr, the ring buffer, and the broadcaster.
func newLogWriter(rb *ringBuf, bc *broadcaster) io.Writer {
	return &multiLogWriter{rb: rb, bc: bc, dest: os.Stderr}
}

// uiHTML is embedded from ui.html at compile time.
//
//go:embed ui.html
var uiHTML string

// --- UI handlers ---

// checkUIAuth validates Basic auth for /ui routes.
// Returns true if auth is satisfied (or not required).
func (p *proxy) checkUIAuth(w http.ResponseWriter, r *http.Request) bool {
	if p.cfg.UIPassword == "" {
		return true
	}
	_, pass, ok := r.BasicAuth()
	if !ok || pass != p.cfg.UIPassword {
		w.Header().Set("WWW-Authenticate", `Basic realm="proxy"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (p *proxy) handleUI(w http.ResponseWriter, r *http.Request) {
	if !p.checkUIAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, uiHTML)
}

func (p *proxy) handleUILogs(w http.ResponseWriter, r *http.Request) {
	if !p.checkUIAuth(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send existing ring buffer contents first.
	for _, line := range p.rb.snapshot() {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	flusher.Flush()

	// Stream new lines as they arrive.
	ch := p.bc.subscribe()
	defer p.bc.unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

func (p *proxy) handleUITest(w http.ResponseWriter, r *http.Request) {
	if !p.checkUIAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	stream := r.FormValue("stream")
	if stream == "" {
		stream = "git"
	}
	topic := r.FormValue("topic")
	if topic == "" {
		topic = "test"
	}

	// Simulate a pull_request_comment webhook — one of the event types the proxy
	// handles specially (remaps to issue_comment for Zulip's Gitea integration).
	// Using a comment makes it obvious in Zulip that this is a self-test, unlike
	// a fake APPROVED message which could cause confusion.
	fakePL := payload{
		"action": "created",
		"pull_request": map[string]any{
			"number":   0,
			"title":    "Self-test",
			"html_url": "",
			"body":     "",
			"state":    "open",
		},
		"comment": map[string]any{
			"body": "[self-test] Webhook proxy connection OK",
		},
		"repository": map[string]any{
			"full_name": "proxy/self-test",
		},
		"sender": map[string]any{
			"login": "webhook-proxy",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	err := p.handlePullRequestComment(fakePL, stream, topic)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// uiHandler returns an http.Handler for the dedicated UI port.
// Routes are at the root level (/, /logs, /test, /health).
func (p *proxy) uiHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/logs", p.handleUILogs)
	mux.HandleFunc("/test", p.handleUITest)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve the UI page for / and any unknown path.
		p.handleUI(w, r)
	})

	return mux
}
