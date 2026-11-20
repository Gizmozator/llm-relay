package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- rewriteBody / forceIncludeUsage hardening -----------------------------

func TestForceIncludeUsageNull(t *testing.T) {
	// stream_options:null unmarshals to a nil map — must not panic.
	got := forceIncludeUsage(json.RawMessage("null"))
	if !strings.Contains(string(got), `"include_usage":true`) {
		t.Fatalf("null stream_options => %s", got)
	}
}

func TestRewriteBodyNullStreamOptions(t *testing.T) {
	body := []byte(`{"model":"pub","stream":true,"stream_options":null}`)
	out := rewriteBody(body, "up", EpChat, true)
	if !strings.Contains(string(out), `"include_usage":true`) {
		t.Fatalf("rewrite dropped stream_options: %s", out)
	}
	if !strings.Contains(string(out), `"model":"up"`) {
		t.Fatalf("model not rewritten: %s", out)
	}
}

func TestRewriteBodyNullBody(t *testing.T) {
	// A JSON null body unmarshals into a nil map — must return input, not panic.
	out := rewriteBody([]byte("null"), "up", EpChat, true)
	if string(out) != "null" {
		t.Fatalf("null body => %s", out)
	}
}

// --- clientIP ---------------------------------------------------------------

func TestClientIPRightmostXFF(t *testing.T) {
	h := &Handler{TrustProxyHeaders: true}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	// Leftmost is client-spoofable; the rightmost entry is the one our own
	// reverse proxy appended.
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	if got := h.clientIP(r); got != "5.6.7.8" {
		t.Errorf("clientIP = %q, want 5.6.7.8 (rightmost)", got)
	}
}

// --- error type mapping ------------------------------------------------------

func TestWriteErrorTypeByStatus(t *testing.T) {
	cases := map[int]string{401: "authentication_error", 404: "not_found_error", 413: "request_too_large", 502: "api_error", 400: "invalid_request_error"}
	for status, want := range cases {
		rec := httptest.NewRecorder()
		writeError(rec, status, "x")
		var v struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &v)
		if v.Error.Type != want {
			t.Errorf("status %d: type = %q, want %q", status, v.Error.Type, want)
		}
	}
}

// --- SSE accumulator ---------------------------------------------------------

func TestSSEChatCountsReasoningContent(t *testing.T) {
	acc := newSSEAccum(EpChat)
	acc.feed([]byte(`{"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}`))
	if acc.textChars == 0 {
		t.Error("reasoning_content deltas must count toward estimated completion")
	}
}

func TestSSEMessagesPartialNotFinal(t *testing.T) {
	// Only message_start (no message_delta): completion is unknown — the
	// caller must fall back to estimation instead of billing 0 completion.
	acc := newSSEAccum(EpMessages)
	acc.feed([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`))
	if _, ok := acc.result(); ok {
		t.Error("message_start alone must not count as final usage")
	}
}

func TestSSEResponsesIncompleteUsage(t *testing.T) {
	acc := newSSEAccum(EpResponses)
	acc.feed([]byte(`{"type":"response.incomplete","response":{"usage":{"input_tokens":7,"output_tokens":3}}}`))
	tok, ok := acc.result()
	if !ok || tok.Prompt != 7 || tok.Completion != 3 {
		t.Errorf("response.incomplete usage: %+v ok=%v", tok, ok)
	}
}

// --- copyStream scanner semantics -------------------------------------------

func TestCopyStreamCRLFAndFinalLineNoNewline(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n" +
		"data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\r\n" +
		"data: [DONE]" // no trailing newline
	rec := httptest.NewRecorder()
	acc := newSSEAccum(EpChat)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := (&Handler{}).copyStream(rec, strings.NewReader(body), acc, r); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	out := rec.Body.String()
	for _, want := range []string{`{"choices":[{"delta":{"content":"hi"}}]}`, `"prompt_tokens":5`, "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("forwarded stream missing %q: %q", want, out)
		}
	}
	// Scanner normalizes CRLF to LF; SSE clients treat both as terminators.
	if strings.Contains(out, "\r") {
		t.Errorf("CRLF should be normalized to LF: %q", out)
	}
	if tok, ok := acc.result(); !ok || tok.Prompt != 5 || tok.Completion != 2 {
		t.Errorf("accumulator missed usage: %+v ok=%v", tok, ok)
	}
}

// --- discovery ---------------------------------------------------------------

func TestProbeHealthNonOKVersionIgnored(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"version":"bogus"}`))
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
		}
	}))
	defer up.Close()
	res := Probe(context.Background(), up.URL, "")
	if res.Version != "" {
		t.Errorf("version = %q from a 404 /health, want empty", res.Version)
	}
	if !res.OK || len(res.Models) != 1 {
		t.Errorf("probe should still succeed via /v1/models: %+v", res)
	}
}
