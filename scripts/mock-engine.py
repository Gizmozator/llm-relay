#!/usr/bin/env python3
"""Mock OpenAI-compatible engine for the federation e2e run.

Serves just what llm-relay needs from an upstream: GET /v1/models (probe +
discovery), GET /health (the relay probe reads its `version` field) and
POST /v1/chat/completions in both the plain JSON and the SSE stream form,
each carrying a usage report (the stream ends with the usual usage-only
chunk before [DONE]). Every reply names the requested model back, so a test
can tell which member of a pool answered.

Usage: mock-engine.py <port> <model-id> [model-id ...]
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODELS = []
# Fixed, distinctive counts: the e2e asserts on exactly these numbers.
PROMPT_TOKENS = 10
COMPLETION_TOKENS = 5
VERSION = "mock-engine-1"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # keep the transcript clean
        pass

    def _json(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _content_length(self):
        """Body size from the header; None when malformed (caller 400s)."""
        raw = self.headers.get("Content-Length")
        if raw is None:
            return 0
        try:
            n = int(raw)
        except ValueError:
            return None
        if n < 0:
            # A negative length parses as an int but poisons the read:
            # read(-1) blocks until EOF and any other negative misframes the
            # stream. Treat it exactly like garbage — 400 and close.
            return None
        return n

    def _drain(self, n):
        # HTTP/1.1 keep-alive hygiene: a request body left unread is parsed
        # as the next request on the reused connection. Read it all out
        # before answering on every early-return path.
        while n > 0:
            chunk = self.rfile.read(min(n, 65536))
            if not chunk:
                break
            n -= len(chunk)

    def do_GET(self):
        if self.path == "/v1/models":
            self._json(200, {
                "object": "list",
                "data": [{"id": m, "object": "model"} for m in MODELS],
            })
        elif self.path in ("/health", "/healthz"):
            # /health is what the relay's backend probe reads (version only).
            self._json(200, {"status": "ok", "version": VERSION})
        else:
            self._json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        n = self._content_length()
        if n is None:
            # Unreadable body size: answer 400 and close, so no garbage can
            # leak into a reused connection.
            self.close_connection = True
            self._json(400, {"error": {"message": "bad Content-Length"}})
            return
        if self.path != "/v1/chat/completions":
            self._drain(n)
            self._json(404, {"error": {"message": "not found"}})
            return
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except (json.JSONDecodeError, UnicodeDecodeError):
            self._json(400, {"error": {"message": "bad json"}})
            return
        if not isinstance(req, dict):
            # [] / "x" / 3 parse fine but are not requests; answer 400 rather
            # than raising mid-handler and dropping the connection.
            self._json(400, {"error": {"message": "body must be a JSON object"}})
            return
        model = req.get("model", "")
        if model not in MODELS:
            self._json(404, {"error": {"message": f"unknown model {model!r}"}})
            return
        usage = {
            "prompt_tokens": PROMPT_TOKENS,
            "completion_tokens": COMPLETION_TOKENS,
            "total_tokens": PROMPT_TOKENS + COMPLETION_TOKENS,
        }
        if not req.get("stream"):
            self._json(200, {
                "id": "chatcmpl-mock",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": f"mock reply from {model}"},
                    "finish_reason": "stop",
                }],
                "usage": usage,
            })
            return
        # SSE: two content chunks, a usage-only final chunk, then [DONE].
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def chunk(obj):
            data = f"data: {json.dumps(obj)}\n\n".encode()
            self.wfile.write(b"%X\r\n%s\r\n" % (len(data), data))
            self.wfile.flush()

        base = {"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model}
        chunk({**base, "choices": [{"index": 0, "delta": {"role": "assistant", "content": "mock "}}]})
        chunk({**base, "choices": [{"index": 0, "delta": {"content": f"stream from {model}"},
                                    "finish_reason": "stop"}]})
        chunk({**base, "choices": [], "usage": usage})
        done = b"data: [DONE]\n\n"
        self.wfile.write(b"%X\r\n%s\r\n" % (len(done), done))
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


def main():
    global MODELS
    if len(sys.argv) < 3:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    try:
        port = int(sys.argv[1])
    except ValueError:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    MODELS = sys.argv[2:]
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
