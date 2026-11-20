// End-to-end hardening tests: full relay against hostile upstreams.
package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A non-stream upstream that sends 200 + Content-Length then dies mid-body
// must surface as a 500 to the client (not a truncated 200) and be recorded
// with status 500 — pins the buffered-response fix: the status line may only
// be committed after the whole body is in hand.
func TestProxyNonStreamMidBodyFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream has no hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		// Claims 500 bytes, delivers ~45, hangs up.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n"))
		conn.Write([]byte(`{"choices":[{"message":{"content":"trunc`))
		conn.Close()
	}))
	h := newHarnessWithURL(t, up)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"pub-model","messages":[{"role":"user","content":"hello world this is a test prompt"}]}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("client status = %d, want 500 (truncated body must not pass as 200)", resp.StatusCode)
	}
	var v struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("error body is not valid JSON: %v", err)
	}
	if v.Error.Type != "api_error" {
		t.Errorf("error type = %q, want api_error", v.Error.Type)
	}

	row := h.usageRow(t)
	if row.Status != 500 {
		t.Errorf("usage row status = %d, want 500", row.Status)
	}
}
