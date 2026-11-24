// Tier 3b wiring test (ticket #18): the per-key bucket hook inside the
// proxy — identity, denial shape, and that unauthenticated traffic never
// reaches it.
package proxy_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The hook runs after auth with the RESOLVED key id (identity reuse: the
// metering path already hashed + looked the key up), denies with 429 +
// Retry-After before any upstream work, and is never consulted for bad
// keys (those are bounded per source by the mux-level tier instead).
func TestKeyLimitHook(t *testing.T) {
	h := newHarness(t, chatUpstream(t))
	var got []int64
	h.ph.KeyLimit = func(keyID int64) (time.Duration, bool) {
		got = append(got, keyID)
		if len(got) >= 2 {
			return 7 * time.Second, false
		}
		return 0, true
	}
	body := `{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`

	resp := h.post(t, "/v1/chat/completions", body, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first request = %d, want 200", resp.StatusCode)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("hook saw %v, want the resolved key id [1]", got)
	}

	resp = h.post(t, "/v1/chat/completions", body, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("denied request = %d, want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "7" {
		t.Errorf("Retry-After = %q, want 7", ra)
	}
	resp.Body.Close()

	// A bad key is refused by auth before the hook: no bucket is spent on
	// traffic that never resolved an identity.
	before := len(got)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-not-a-real-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key = %d, want 401", resp.StatusCode)
	}
	if len(got) != before {
		t.Error("KeyLimit consulted for an unauthenticated request")
	}
}

// nil hook (tests and any build that does not wire the tier) passes
// everything — the tier-off default must not change proxy behaviour.
func TestKeyLimitNilPasses(t *testing.T) {
	h := newHarness(t, chatUpstream(t))
	body := `{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 30; i++ {
		resp := h.post(t, "/v1/chat/completions", body, nil)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d with nil KeyLimit = %d, want 200", i+1, resp.StatusCode)
		}
	}
}
