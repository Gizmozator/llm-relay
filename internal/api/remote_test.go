// Remote-backend API tests (plan 2026-11-24-remote-backend-v1): admin
// create/rotate surface, connect-token auth on the relay route, and the
// probe branch for offline remote backends. The channel itself is covered
// end-to-end in internal/remote.
package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

func createRemoteBackend(t *testing.T, e *testEnv, adminTok, name string) (store.Backend, string) {
	t.Helper()
	resp := e.do(t, "POST", "/api/admin/backends/remote", `{"name":"`+name+`"}`, adminTok)
	if resp.StatusCode != 201 {
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create remote backend = %d (%s)", resp.StatusCode, msg)
	}
	var out struct {
		Backend store.Backend `json:"backend"`
		Token   string        `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(out.Token, "rb-") {
		t.Fatalf("token = %q, want rb- prefix", out.Token)
	}
	if want := "remote://" + strconv.FormatInt(out.Backend.ID, 10); out.Backend.BaseURL != want {
		t.Fatalf("base_url = %q, want %q", out.Backend.BaseURL, want)
	}
	return out.Backend, out.Token
}

func TestAdminRemoteBackendLifecycle(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	b, token := createRemoteBackend(t, e, admin, "gpu-box")

	// The row appears in the list like any backend — and reads OFFLINE:
	// health is the registry hook, not a phantom HTTP probe that would
	// answer online for a URL that does not exist.
	resp := e.do(t, "GET", "/api/admin/backends", "", admin)
	var list struct {
		Backends []struct {
			store.Backend
			Online    bool   `json:"online"`
			HealthErr string `json:"health_error"`
		} `json:"backends"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Backends) != 1 || list.Backends[0].ID != b.ID {
		t.Fatalf("backends list = %+v", list.Backends)
	}
	if list.Backends[0].Online || list.Backends[0].HealthErr != "remote backend not connected" {
		t.Errorf("never-connected remote backend lists online=%v err=%q, want offline",
			list.Backends[0].Online, list.Backends[0].HealthErr)
	}

	// Connect auth: missing → 401, wrong → 401, valid → past auth (the WS
	// upgrade itself fails on a plain GET, so anything but 401 proves the
	// token resolved).
	if resp := e.do(t, "GET", "/remote/v1/connect", "", ""); resp.StatusCode != 401 {
		t.Errorf("no token = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}
	if resp := e.do(t, "GET", "/remote/v1/connect", "", "rb-wrong"); resp.StatusCode != 401 {
		t.Errorf("wrong token = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}
	if resp := e.do(t, "GET", "/remote/v1/connect", "", token); resp.StatusCode == 401 {
		t.Error("valid token rejected at auth")
		resp.Body.Close()
	}

	// Rotate: new token lives, old one dies.
	resp = e.do(t, "POST", "/api/admin/backends/"+strconv.FormatInt(b.ID, 10)+"/remote/rotate", "", admin)
	if resp.StatusCode != 200 {
		t.Fatalf("rotate = %d", resp.StatusCode)
	}
	var rot struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&rot)
	resp.Body.Close()
	if rot.Token == token || !strings.HasPrefix(rot.Token, "rb-") {
		t.Fatalf("rotated token = %q", rot.Token)
	}
	if resp := e.do(t, "GET", "/remote/v1/connect", "", token); resp.StatusCode != 401 {
		t.Errorf("old token after rotate = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}

	// Disabling answers 403 (distinct from an unknown token).
	off := false
	patch, _ := json.Marshal(map[string]any{"enabled": off})
	if resp := e.do(t, "PATCH", "/api/admin/backends/"+strconv.FormatInt(b.ID, 10), string(patch), admin); resp.StatusCode != 200 {
		t.Fatalf("disable = %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/remote/v1/connect", "", rot.Token); resp.StatusCode != 403 {
		t.Errorf("disabled connect = %d, want 403", resp.StatusCode)
		resp.Body.Close()
	}
}

func TestRemoteBackendGuards(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	b, _ := createRemoteBackend(t, e, admin, "gpu-box")
	id := strconv.FormatInt(b.ID, 10)

	// base_url is the registry key: editing it on a remote row is refused.
	patch, _ := json.Marshal(map[string]any{"base_url": "http://elsewhere:9"})
	if resp := e.do(t, "PATCH", "/api/admin/backends/"+id, string(patch), admin); resp.StatusCode != 400 {
		t.Errorf("base_url edit on remote = %d, want 400", resp.StatusCode)
		resp.Body.Close()
	}
	// Rename is fine.
	patch, _ = json.Marshal(map[string]any{"name": "renamed-box"})
	if resp := e.do(t, "PATCH", "/api/admin/backends/"+id, string(patch), admin); resp.StatusCode != 200 {
		t.Errorf("rename on remote = %d, want 200", resp.StatusCode)
		resp.Body.Close()
	}
	// Round-tripping the whole backend object (base_url echoed unchanged
	// while editing something else) is a common admin-client pattern: an
	// identical echo must be accepted, only a real change is refused.
	patch, _ = json.Marshal(map[string]any{"name": "round-trip", "base_url": b.BaseURL})
	if resp := e.do(t, "PATCH", "/api/admin/backends/"+id, string(patch), admin); resp.StatusCode != 200 {
		t.Errorf("unchanged base_url echo = %d, want 200", resp.StatusCode)
		resp.Body.Close()
	}
	// Rotate on a plain backend is refused.
	resp := e.do(t, "POST", "/api/admin/backends", `{"name":"plain","base_url":"http://127.0.0.1:9"}`, admin)
	var plain store.Backend
	json.NewDecoder(resp.Body).Decode(&plain)
	resp.Body.Close()
	if resp := e.do(t, "POST", "/api/admin/backends/"+strconv.FormatInt(plain.ID, 10)+"/remote/rotate", "", admin); resp.StatusCode != 400 {
		t.Errorf("rotate on plain backend = %d, want 400", resp.StatusCode)
		resp.Body.Close()
	}
	// Duplicate names collide across kinds (one shared namespace).
	if resp := e.do(t, "POST", "/api/admin/backends/remote", `{"name":"plain"}`, admin); resp.StatusCode != 409 {
		t.Errorf("duplicate name = %d, want 409", resp.StatusCode)
		resp.Body.Close()
	}
}

// An offline remote backend must fail the probe cleanly (dial-class), not
// hang and not pretend to be a healthy HTTP backend.
func TestProbeOfflineRemoteBackend(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	b, _ := createRemoteBackend(t, e, admin, "offline-box")

	resp := e.do(t, "POST", "/api/admin/backends/"+strconv.FormatInt(b.ID, 10)+"/probe", "", admin)
	if resp.StatusCode != 200 {
		t.Fatalf("probe = %d", resp.StatusCode)
	}
	var res proxy.ProbeResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if res.OK || res.Error == "" {
		t.Fatalf("offline probe = %+v, want ok=false with an error", res)
	}
}

// Token guessing must not be free: every attempt costs a DB lookup, so the
// connect endpoint carries the login limiter's mechanism (own bucket
// space). After the burst, bad tokens get 429 + Retry-After; a valid token
// resets the bucket so legitimate connectors never drain it.
func TestRemoteConnectThrottled(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())

	code := 0
	for i := 0; i < 12; i++ {
		resp := e.do(t, "GET", "/remote/v1/connect", "", "rb-guess-guess-guess")
		code = resp.StatusCode
		resp.Body.Close()
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("after 12 bad-token attempts status = %d, want 429", code)
	}
}

// The throttle runs before auth (that is the point — the DB lookup is what
// it protects), so a drained bucket locks out the address until refill,
// valid token included: the same fail-closed trade-off login makes. What a
// valid token DOES do is reset the bucket on every success — a connector
// flapping next to an attacker on the same address never drains its own
// credits.
func TestRemoteConnectResetOnAuth(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	_, tok := createRemoteBackend(t, e, admin, "fresh-box")

	for i := 0; i < 12; i++ {
		resp := e.do(t, "GET", "/remote/v1/connect", "", "rb-guess-guess-guess")
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("interleaved attempt %d: valid-token resets must keep the bucket alive, got 429", i)
		}
		resp = e.do(t, "GET", "/remote/v1/connect", "", tok)
		resp.Body.Close()
	}
}
