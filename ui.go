package main

import (
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

// --- embedded HTML ---

const uiHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Webhook Proxy</title>
<style>
body { font-family: monospace; max-width: 900px; margin: 2rem auto; padding: 0 1rem; background: #1a1a1a; color: #ddd; }
h1 { font-size: 1.2rem; color: #fff; }
h2 { font-size: 1rem; margin-top: 2rem; color: #aaa; }
label { display: inline-block; margin-right: 1rem; }
input[type=text] { background: #2a2a2a; border: 1px solid #444; color: #ddd; padding: 4px 6px; font-family: monospace; border-radius: 3px; }
button { background: #334; border: 1px solid #556; color: #ddd; padding: 4px 12px; cursor: pointer; border-radius: 3px; }
button:hover { background: #445; }
#result { margin-top: 0.6rem; font-size: 0.9rem; min-height: 1.2em; }
.ok { color: #4f4; }
.err { color: #f66; }
#logs { background: #111; color: #ccc; padding: 1rem; height: 420px; overflow-y: auto; white-space: pre-wrap; font-size: 0.82rem; border-radius: 4px; border: 1px solid #333; margin-top: 0.5rem; }
</style>
</head>
<body>
<h1>Webhook Proxy</h1>

<h2>Test connection</h2>
<form id="testForm">
  <label>Stream <input type="text" name="stream" value="git" style="width:100px"></label>
  <label>Topic <input type="text" name="topic" value="test" style="width:120px"></label>
  <button type="submit">Send test message</button>
</form>
<div id="result"></div>

<h2>Live logs</h2>
<div id="logs"></div>

<script>
document.getElementById('testForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const res = document.getElementById('result');
  res.className = '';
  res.textContent = 'Sending\u2026';
  try {
    const r = await fetch('/test', {
      method: 'POST',
      headers: {'Content-Type': 'application/x-www-form-urlencoded'},
      body: new URLSearchParams(new FormData(e.target)),
    });
    const j = await r.json();
    res.className = j.ok ? 'ok' : 'err';
    res.textContent = j.ok ? 'OK \u2014 test message sent to Zulip' : ('Error: ' + j.error);
  } catch (err) {
    res.className = 'err';
    res.textContent = 'Request failed: ' + err;
  }
});

const logDiv = document.getElementById('logs');
const es = new EventSource('/logs');
es.onmessage = (e) => {
  logDiv.textContent += e.data + '\n';
  logDiv.scrollTop = logDiv.scrollHeight;
};
es.onerror = () => {
  logDiv.textContent += '[connection lost, retrying\u2026]\n';
};
</script>
</body>
</html>`

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

	// Simulate a real pull_request_approved webhook so the full handler path
	// (including logging) is exercised, not just a raw bot API call.
	fakePL := payload{
		"pull_request": payload{
			"number":   0,
			"title":    "Webhook proxy self-test",
			"html_url": "",
		},
		"repository": payload{
			"full_name": "proxy/self-test",
		},
		"sender": payload{
			"login": "webhook-proxy",
		},
		"review": payload{
			"type":    "approved",
			"content": "Self-test triggered from /ui",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	err := p.handlePullRequestReview(fakePL, "pull_request_approved", stream, topic)
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
