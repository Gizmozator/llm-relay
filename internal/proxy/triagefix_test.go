// Regression tests for the final whole-repo scan fix round (proxy side):
// empty usage blocks are not authoritative (9), the outbound client
// refuses redirects (22), and a key-lookup DB failure answers 500 (24).
package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"llm-relay/internal/db"
	"llm-relay/internal/proxy"
)

// Item 9: "usage":{} (some gateways emit it on error paths) must read as
// "no usage reported" so the estimate fallback runs, in every dialect.
func TestExtractUsageEmptyBlockIsMissing(t *testing.T) {
	for _, ep := range []string{"chat", "completions", "embeddings", "responses", "messages"} {
		for _, body := range []string{
			`{"usage":{}}`,
			`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			`{"usage":{"input_tokens":0,"output_tokens":0}}`,
		} {
			if _, ok := proxy.ExtractUsage(ep, []byte(body)); ok {
				t.Errorf("%s %s: all-zero usage accepted, want ok=false", ep, body)
			}
		}
	}
	// Positive counts still count (embeddings legitimately has no output).
	if tok, ok := proxy.ExtractUsage("embeddings", []byte(`{"usage":{"prompt_tokens":7,"completion_tokens":0}}`)); !ok || tok.Prompt != 7 {
		t.Errorf("embeddings prompt=7 rejected: %+v ok=%v", tok, ok)
	}
}

// Item 22: the relay's outbound client must not follow redirects — the
// backend's API key would ride to a host the operator never configured.
func TestClientRefusesRedirects(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		_, _ = w.Write([]byte("stolen"))
	}))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer src.Close()

	h := proxy.New(nil, nil, false)
	resp, err := h.Client.Get(src.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want the 302 passed through unfollowed", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&targetHits); n != 0 {
		t.Errorf("redirect target was fetched %d times — client followed the hop", n)
	}
}

// Item 24: a key lookup that fails for server reasons (closed/busy DB)
// must answer 500 (and log), not 401 — an "invalid API key" for a broken
// database sends the operator chasing the wrong problem.
func TestAuthKeyDBErrorIs500(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/closed.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	h := proxy.New(d, nil, false)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-0123456789abcdef")
	// Control: unknown key on a healthy DB is the caller's problem — 401.
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("healthy db + unknown key: status = %d, want 401", w.Code)
	}
	// Same request with a closed DB is ours — 500, not 401.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.Mux().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("closed db: status = %d, want 500", w.Code)
	}
}
