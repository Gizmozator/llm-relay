// Build-version awareness endpoints (plan 2026-11-25-version-awareness):
// the /api/version chip payload, the version_warn setting round-trip, and
// the version_state stamp on peer lists.
package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"llm-relay/internal/store"
)

func TestVersionEndpoint(t *testing.T) {
	e := newTestEnv(t, nil)
	tok := loginAs(t, e, "admin", "adminpass1")
	resp := e.do(t, "GET", "/api/version", "", tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Version     string `json:"version"`
		Outdated    bool   `json:"outdated"`
		WarnEnabled bool   `json:"warn_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// The test env stamps v9.9.9; nothing is connected, so not outdated;
	// the warning defaults to on.
	if out.Version != "v9.9.9" || out.Outdated || !out.WarnEnabled {
		t.Errorf("got %+v, want v9.9.9 / not outdated / warn on", out)
	}
}

func TestVersionWarnSettingRoundTrip(t *testing.T) {
	e := newTestEnv(t, nil)
	tok := loginAs(t, e, "admin", "adminpass1")

	// Default on; PUT it off and both the settings pair and /api/version
	// (which drives the banner) must agree.
	resp := e.do(t, "PUT", "/api/admin/settings", `{"currency":"eur","version_warn":false}`, tok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}
	resp = e.do(t, "GET", "/api/admin/settings", "", tok)
	var s struct {
		VersionWarn bool `json:"version_warn"`
	}
	json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if s.VersionWarn {
		t.Error("settings still report version_warn on after muting")
	}
	resp = e.do(t, "GET", "/api/version", "", tok)
	var v struct {
		WarnEnabled bool `json:"warn_enabled"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if v.WarnEnabled {
		t.Error("/api/version still reports warn_enabled after muting")
	}
	// Absent in the payload: leaves the muted value alone.
	resp = e.do(t, "PUT", "/api/admin/settings", `{"currency":"usd"}`, tok)
	resp.Body.Close()
	resp = e.do(t, "GET", "/api/admin/settings", "", tok)
	json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if s.VersionWarn {
		t.Error("version_warn reset to default by a PUT that omitted it")
	}
}

// The peer list stamps version_state against this build (v9.9.9 in the
// test env): an older reported version reads "older" — the Federation
// page's small note; an unreported one reads "unknown".
func TestPeerListVersionState(t *testing.T) {
	e := newTestEnv(t, nil)
	tok := loginAs(t, e, "admin", "adminpass1")
	p, _, err := store.CreatePeer(e.db, "old-spoke", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchPeer(e.db, p.ID, time.Now().UTC().Format(time.RFC3339), "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreatePeer(e.db, "never-seen", 1); err != nil {
		t.Fatal(err)
	}
	resp := e.do(t, "GET", "/api/admin/peers", "", tok)
	defer resp.Body.Close()
	var out struct {
		Peers []struct {
			Name         string `json:"name"`
			VersionState string `json:"version_state"`
		} `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"old-spoke": "older", "never-seen": "unknown"}
	for _, p := range out.Peers {
		if w, ok := want[p.Name]; !ok || p.VersionState != w {
			t.Errorf("peer %s version_state = %q, want %q", p.Name, p.VersionState, want[p.Name])
		}
	}
}
