// Drain mode: a draining backend refuses NEW sessions (503 + Retry-After)
// while sessions active within the window keep working — graceful shutdown.
package proxy_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/store"
)

// drainPost sends a chat request with an explicit session id and returns
// the status code plus the Retry-After header value.
func drainPost(t *testing.T, h *harness, session string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.key)
	if session != "" {
		req.Header.Set("X-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

func setDraining(t *testing.T, h *harness, on bool) {
	t.Helper()
	if _, err := store.UpdateBackend(h.db, 1, nil, nil, nil, nil, &on); err != nil {
		t.Fatal(err)
	}
}

func TestDrainGate(t *testing.T) {
	h := newHarness(t, agentUpstream())

	// Warm session: succeeds while the backend is normal and gets marked.
	if st, _ := drainPost(t, h, "s-old"); st != 200 {
		t.Fatalf("warm-up status = %d, want 200", st)
	}

	setDraining(t, h, true)

	// Recent session passes (existing conversation may finish).
	if st, _ := drainPost(t, h, "s-old"); st != 200 {
		t.Fatalf("recent session status = %d, want 200", st)
	}

	// New session is refused with a retry hint.
	st, ra := drainPost(t, h, "s-new")
	if st != http.StatusServiceUnavailable {
		t.Fatalf("new session status = %d, want 503", st)
	}
	if ra != "60" {
		t.Errorf("Retry-After = %q, want 60", ra)
	}

	// Sessionless (derived) traffic counts as new too.
	if st, _ := drainPost(t, h, ""); st != http.StatusServiceUnavailable {
		t.Errorf("derived-session status = %d, want 503", st)
	}

	// usage_log fallback: a session with a recent row but no memory entry
	// (relay restarted while draining) still passes.
	err := store.InsertUsage(h.db, &store.UsageEvent{
		TS: time.Now().UTC().Format(time.RFC3339), UserID: 1, SessionID: "s-db",
		Endpoint: "chat", ModelRequ: "pub-model", LatencyMS: 100, Status: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := drainPost(t, h, "s-db"); st != 200 {
		t.Errorf("usage_log fallback status = %d, want 200", st)
	}

	// Drain off → everything flows again.
	setDraining(t, h, false)
	if st, _ := drainPost(t, h, "s-new2"); st != 200 {
		t.Errorf("after undrain status = %d, want 200", st)
	}
}

func TestDrainUtilizationStatus(t *testing.T) {
	h := newHarness(t, agentUpstream())
	setDraining(t, h, true)

	resp := agentGet(t, h.server.URL+"/v1/utilization", h.key)
	var r struct {
		Models []struct {
			Name     string `json:"name"`
			Online   bool   `json:"online"`
			Draining bool   `json:"draining"`
			Status   string `json:"status"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if len(r.Models) != 1 {
		t.Fatalf("models = %+v, want one", r.Models)
	}
	m := r.Models[0]
	if !m.Online || !m.Draining || m.Status != "draining" {
		t.Errorf("model = %+v, want online+draining status=draining", m)
	}
}
