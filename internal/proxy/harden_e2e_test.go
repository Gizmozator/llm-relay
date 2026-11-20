// End-to-end hardening tests: full relay against hostile upstreams.
package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm-relay/internal/store"
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

// The user's price multiplier must be applied to the recorded cost snapshot.
func TestProxyUserMultiplierApplied(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"x"}}],
			"usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}`))
	})
	h := newHarness(t, up)

	// Harness prices: in 100/M, out 200/M → base cost 100+200 = 300 µ€;
	// multiplier 2.5 → 750.
	mult := 2.5
	users, err := store.ListUsers(h.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Username == "bob" {
			if _, err := store.UpdateUser(h.db, u.ID, nil, nil, nil, nil, &mult); err != nil {
				t.Fatal(err)
			}
		}
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	resp.Body.Close()
	row := h.usageRow(t)
	if row.CostMicro != 750 {
		t.Errorf("cost = %d µ, want 750 (300 × 2.5)", row.CostMicro)
	}
}
