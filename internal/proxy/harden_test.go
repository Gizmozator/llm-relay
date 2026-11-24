package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestCopyStreamKeepaliveDuringSilence(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 40 * time.Millisecond
	defer func() { keepaliveInterval = old }()

	rp, wp := io.Pipe()
	go func() {
		// silent long enough for several keepalives, then real data
		time.Sleep(150 * time.Millisecond)
		wp.Write([]byte("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n"))
		wp.Close()
	}()
	rec := httptest.NewRecorder()
	acc := newSSEAccum(EpChat)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := (&Handler{}).copyStream(rec, rp, acc, r); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	out := rec.Body.String()
	ka := strings.Index(out, ": keepalive")
	data := strings.Index(out, "data:")
	if ka < 0 {
		t.Fatalf("no keepalive injected during upstream silence: %q", out)
	}
	if data >= 0 && ka > data {
		t.Errorf("keepalive must precede the first data line: %q", out)
	}
	if tok, ok := acc.result(); !ok || tok.Prompt != 5 || tok.Completion != 2 {
		t.Errorf("accumulator missed usage after keepalives: %+v ok=%v", tok, ok)
	}
}

// A client hang-up mid-stream must log the diagnostic line the operator
// needs to tell "who quit" apart: upstream TTFB, forwarded volume, ctx error.
func TestCopyStreamHangupLogsDiagnostic(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	rp, wp := io.Pipe()
	defer wp.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	go func() {
		time.Sleep(30 * time.Millisecond) // upstream stays silent, then client quits
		cancel()
	}()
	acc := newSSEAccum(EpChat)
	if got := (&Handler{}).copyStream(httptest.NewRecorder(), rp, acc, r); got != statusClientClosed {
		t.Fatalf("status = %d, want %d", got, statusClientClosed)
	}
	out := buf.String()
	if !strings.Contains(out, "stream 499") {
		t.Fatalf("no 499 diagnostic logged: %q", out)
	}
	if !strings.Contains(out, "upstream first line never") {
		t.Fatalf("diagnostic must record that the upstream never answered: %q", out)
	}
	if !strings.Contains(out, "context canceled") {
		t.Fatalf("diagnostic must carry the context error: %q", out)
	}
}

func TestCopyStreamScannerBufferNotAliased(t *testing.T) {
	// Many lines arriving in large misaligned chunks: the scanner refills its
	// internal buffer while the main loop is still consuming an earlier line.
	// Delivered lines must be private copies — run under -race, which trips on
	// the aliasing variant (scanner Read vs consumer Write of the same array).
	const lines = 2000
	rp, wp := io.Pipe()
	go func() {
		var payload []byte
		for i := 0; i < lines; i++ {
			pad := strings.Repeat("x", 200)
			payload = append(payload, []byte(fmt.Sprintf(
				"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-%04d-%s\"}}]}\n", i, pad))...)
			if len(payload) >= 40<<10 {
				if _, err := wp.Write(payload); err != nil {
					return
				}
				payload = payload[:0]
			}
		}
		if len(payload) > 0 {
			wp.Write(payload)
		}
		wp.Close()
	}()
	rec := httptest.NewRecorder()
	acc := newSSEAccum(EpChat)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := (&Handler{}).copyStream(rec, rp, acc, r); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	out := rec.Body.String()
	for i := 0; i < lines; i++ {
		if want := fmt.Sprintf("chunk-%04d", i); !strings.Contains(out, want) {
			t.Fatalf("forwarded stream lost or corrupted %s", want)
		}
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
