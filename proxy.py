#!/usr/bin/env python3
"""
Forgejo → Zulip webhook proxy.

Handles two event types that Zulip's built-in Gitea integration can't:
  - pull_request_comment  → remapped to issue_comment and forwarded
  - pull_request_review_rejected → posted as a formatted message via Zulip bot API

All other events are forwarded as-is to the Zulip Gitea webhook URL.

Environment variables:
  ZULIP_GITEA_WEBHOOK_URL  Full URL of your Zulip Gitea integration endpoint
                           e.g. https://chat.example.org/api/v1/external/gitea?api_key=XXX&stream=git&topic=myrepo
  ZULIP_SITE               Zulip instance base URL (for bot API calls)
                           e.g. https://chat.example.org
  ZULIP_BOT_EMAIL          Bot email address
  ZULIP_BOT_API_KEY        Bot API key
  ZULIP_STREAM             Stream to post review notifications to (default: same as webhook stream)
  ZULIP_TOPIC              Topic for review notifications (default: repo name from payload)
  PORT                     Port to listen on (default: 8080)
  FORGEJO_SECRET           Shared secret for validating webhook signatures (optional but recommended)
"""

import hashlib
import hmac
import json
import logging
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs, urlencode, urlunparse
from urllib.request import Request, urlopen
from urllib.error import HTTPError

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
)
log = logging.getLogger(__name__)

ZULIP_GITEA_WEBHOOK_URL = os.environ.get("ZULIP_GITEA_WEBHOOK_URL", "")
ZULIP_SITE = os.environ.get("ZULIP_SITE", "").rstrip("/")
ZULIP_BOT_EMAIL = os.environ.get("ZULIP_BOT_EMAIL", "")
ZULIP_BOT_API_KEY = os.environ.get("ZULIP_BOT_API_KEY", "")
ZULIP_STREAM = os.environ.get("ZULIP_STREAM", "")
ZULIP_TOPIC = os.environ.get("ZULIP_TOPIC", "")
PORT = int(os.environ.get("PORT", "8080"))
FORGEJO_SECRET = os.environ.get("FORGEJO_SECRET", "")


def validate_signature(body: bytes, signature_header: str) -> bool:
    if not FORGEJO_SECRET:
        return True
    if not signature_header:
        log.warning("Missing X-Gitea-Signature header")
        return False
    expected = "sha256=" + hmac.new(
        FORGEJO_SECRET.encode(), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature_header)


def post_to_zulip_api(stream: str, topic: str, content: str) -> None:
    """Post a message directly via the Zulip bot REST API."""
    if not all([ZULIP_SITE, ZULIP_BOT_EMAIL, ZULIP_BOT_API_KEY]):
        log.error("ZULIP_SITE / ZULIP_BOT_EMAIL / ZULIP_BOT_API_KEY not set — cannot post review notification")
        return

    import base64
    url = f"{ZULIP_SITE}/api/v1/messages"
    data = urlencode({
        "type": "stream",
        "to": stream,
        "topic": topic,
        "content": content,
    }).encode()

    credentials = base64.b64encode(f"{ZULIP_BOT_EMAIL}:{ZULIP_BOT_API_KEY}".encode()).decode()
    req = Request(url, data=data, method="POST")
    req.add_header("Authorization", f"Basic {credentials}")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")

    try:
        with urlopen(req) as resp:
            log.info("Zulip API response: %s", resp.status)
    except HTTPError as e:
        log.error("Zulip API error %s: %s", e.code, e.read())


def forward_to_gitea_webhook(payload: dict, event_type: str) -> None:
    """Forward a (possibly transformed) payload to the Zulip Gitea webhook."""
    if not ZULIP_GITEA_WEBHOOK_URL:
        log.error("ZULIP_GITEA_WEBHOOK_URL not set")
        return

    body = json.dumps(payload).encode()
    req = Request(ZULIP_GITEA_WEBHOOK_URL, data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("X-Gitea-Event", event_type)

    try:
        with urlopen(req) as resp:
            log.info("Forwarded %s → Zulip: %s", event_type, resp.status)
    except HTTPError as e:
        log.error("Zulip webhook error %s: %s", e.code, e.read())


def resolve_stream_and_topic(payload: dict) -> tuple[str, str]:
    """Determine the stream and topic to use for direct bot posts."""
    repo_name = payload.get("repository", {}).get("name", "unknown")

    stream = ZULIP_STREAM
    if not stream and ZULIP_GITEA_WEBHOOK_URL:
        parsed = urlparse(ZULIP_GITEA_WEBHOOK_URL)
        qs = parse_qs(parsed.query)
        stream = qs.get("stream", ["git"])[0]

    topic = ZULIP_TOPIC or repo_name
    return stream, topic


def handle_pull_request_comment(payload: dict) -> None:
    """
    Remap pull_request_comment → issue_comment so Zulip's Gitea integration handles it.

    Zulip's issue_comment handler checks is_pull to know whether it's on a PR.
    It reads: action, comment.body, comment.user.login, issue.number, issue.title,
              repository.full_name, repository.html_url, comment.html_url
    """
    pr = payload.get("pull_request", {})
    transformed = {
        "action": payload.get("action", "created"),
        "is_pull": True,
        "issue": {
            "number": pr.get("number", payload.get("number")),
            "title": pr.get("title", ""),
            "body": pr.get("body", ""),
            "state": pr.get("state", ""),
            "user": pr.get("user", {}),
            "html_url": pr.get("html_url", ""),
        },
        "comment": payload.get("comment", {}),
        "repository": payload.get("repository", {}),
        "sender": payload.get("sender", {}),
    }
    forward_to_gitea_webhook(transformed, "issue_comment")


def handle_pull_request_review_rejected(payload: dict) -> None:
    """
    Format a "request changes" review and post it via the Zulip bot API.
    Zulip's Gitea integration has no handler for this event type.
    """
    pr = payload.get("pull_request", {})
    review = payload.get("review", {})
    repo = payload.get("repository", {})
    sender = payload.get("sender", payload.get("review", {}).get("user", {}))

    pr_number = pr.get("number", "?")
    pr_title = pr.get("title", "PR #" + str(pr_number))
    pr_url = pr.get("html_url", "")
    reviewer = review.get("user", {}).get("login") or sender.get("login", "someone")
    repo_name = repo.get("full_name", repo.get("name", "unknown"))
    comment = review.get("content", "").strip()

    lines = [f"**[{repo_name}]** {reviewer} requested changes on [#{pr_number} {pr_title}]({pr_url})"]
    if comment:
        lines.append(f"\n> {comment}")

    stream, topic = resolve_stream_and_topic(payload)
    post_to_zulip_api(stream, topic, "\n".join(lines))


class ProxyHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # suppress default access log
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)

        sig = self.headers.get("X-Gitea-Signature", "")
        if not validate_signature(body, sig):
            log.warning("Invalid signature — rejecting request")
            self.send_response(403)
            self.end_headers()
            return

        event = self.headers.get("X-Gitea-Event", "")
        log.info("Received event: %s", event)

        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            log.error("Invalid JSON body")
            self.send_response(400)
            self.end_headers()
            return

        try:
            if event == "pull_request_comment":
                handle_pull_request_comment(payload)
            elif event == "pull_request_review_rejected":
                handle_pull_request_review_rejected(payload)
            else:
                forward_to_gitea_webhook(payload, event)
        except Exception as e:
            log.exception("Error handling event %s: %s", event, e)
            self.send_response(500)
            self.end_headers()
            return

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")


def main():
    missing = []
    if not ZULIP_GITEA_WEBHOOK_URL:
        missing.append("ZULIP_GITEA_WEBHOOK_URL")
    if missing:
        log.error("Missing required env vars: %s", ", ".join(missing))
        sys.exit(1)

    server = HTTPServer(("0.0.0.0", PORT), ProxyHandler)
    log.info("Proxy listening on port %d", PORT)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log.info("Shutting down")


if __name__ == "__main__":
    main()
