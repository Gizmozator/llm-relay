// Federated-model write guards on the admin model endpoints (ticket #9): a
// materialized row mirrors a peer's catalog, so the API refuses every edit —
// `enabled` included (a per-model disable silently reverts on the next
// catalog re-push, so the durable kill switches are the peer/backend disable
// in the Servers section, where removal is per-peer too).
package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"llm-relay/internal/store"
)

// newFederatedModel seeds the materialized shape federation writes: a
// synthetic backend (peer://<id> or hub://…) plus a model row on it.
func newFederatedModel(t *testing.T, e *testEnv, backendName, baseURL, modelName string) *store.Model {
	t.Helper()
	be, err := store.CreateBackend(e.db, backendName, baseURL, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(e.db, be.ID, "up-"+modelName, modelName)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// errMessage reads the {"error":{"message":…}} envelope; a body that is not
// that envelope (a 204 with no body, say) is reported as its raw text.
func errMessage(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "body read failed: " + err.Error()
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if derr := json.NewDecoder(bytes.NewReader(raw)).Decode(&out); derr != nil {
		return strings.TrimSpace(string(raw))
	}
	return out.Error.Message
}

// A federated model takes no peer-owned edit: every such field is refused with
// 400 and nothing is written — not the refused field, not a second field that
// rode along in the same PATCH.
func TestFederatedModelEditGuard(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	m := newFederatedModel(t, e, "peer7", store.PeerBackendURL(7), "shared-model")

	// Seed values the rejections below must leave alone.
	if _, err := store.UpdateModelPatch(e.db, m.ID, store.ModelPatch{
		Prices: [4]*int64{ptrI(100), ptrI(200), nil, nil},
	}); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{
		`{"name":"renamed"}`,
		`{"visibility":"internal"}`,
		`{"price_in":1}`,
		`{"price_out":1}`,
		`{"price_cache_read":1}`,
		`{"price_cache_write":1}`,
		`{"context_length":4096}`,
		`{"concurrency_limit":2}`,
		`{"busy_threshold_pct":90}`,
		`{"bench_tok_per_s":10}`,
		`{"bench_pp_tok_per_s":100}`,
		`{"fed_visibility":"all"}`,
		`{"fed_visibility":"allowlist","fed_user_ids":[1]}`,
		// enabled does not license the rest of the patch.
		`{"enabled":false,"name":"renamed"}`,
		// A bogus value on a federated row is still the federated refusal.
		`{"name":""}`,
	} {
		resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID), body, tok)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PATCH %s on a federated model = %d, want 400 (%s)",
				body, resp.StatusCode, errMessage(t, resp))
			continue
		}
		if msg := errMessage(t, resp); msg == "" {
			t.Errorf("PATCH %s: empty error message", body)
		}
	}
	// Nothing committed: neither the single-field patches nor the
	// enabled+name pair left a trace.
	got, err := store.ModelByID(e.db, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "shared-model" || got.Visibility != "public" || !got.Enabled ||
		got.PriceIn != 100 || got.PriceOut != 200 || got.ContextLength != 0 ||
		got.ConcurrencyLimit != 0 || got.BusyThresholdPct != 80 || got.BenchTokPerS != 0 ||
		got.FedVisibility != "off" {
		t.Errorf("federated row edited despite the guard: %+v", got)
	}

	// The hub:// form is federated too.
	hubModel := newFederatedModel(t, e, "hub-fed", "hub://federation", "hub-model")
	if resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", hubModel.ID), `{"name":"renamed"}`, tok); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("hub:// model PATCH = %d, want 400 (%s)", resp.StatusCode, errMessage(t, resp))
	}

	// Unknown id keeps its 404: the guard must not turn it into a 400.
	if resp := e.do(t, "PATCH", "/api/admin/models/9999", `{"name":"x"}`, tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id PATCH = %d, want 404", resp.StatusCode)
		resp.Body.Close()
	}
}

// `enabled` is read-only on a federated row too: the next catalog push
// re-enables it, so the API refuses the PATCH instead of promising a disable
// that silently reverts. Even an empty body is refused — with no writable
// field there is nothing a PATCH could legally say.
func TestFederatedModelEnabledOnlyPATCH(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	m := newFederatedModel(t, e, "peer8", store.PeerBackendURL(8), "shared-off")

	for _, body := range []string{`{"enabled":false}`, `{"enabled":true}`, `{}`} {
		resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID), body, tok)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PATCH %s on a federated model = %d, want 400 (%s)",
				body, resp.StatusCode, errMessage(t, resp))
			continue
		}
		if msg := errMessage(t, resp); msg == "" {
			t.Errorf("PATCH %s: empty error message", body)
		}
	}
	// Nothing committed: the row keeps the seeded enabled=true and name.
	got, err := store.ModelByID(e.db, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Name != "shared-off" {
		t.Errorf("federated row changed despite the guard: %+v", got)
	}
}

// A federated model cannot be deleted (the row is the peer's; removal is
// per-peer), while a local model still edits and deletes normally.
func TestFederatedModelDeleteGuard(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	fed := newFederatedModel(t, e, "peer9", store.PeerBackendURL(9), "fed-keep")
	local := newCatalogModel(t, e, "node-a", "local-keep")

	resp := e.do(t, "DELETE", pathf("/api/admin/models/%d", fed.ID), "", tok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE a federated model = %d, want 400 (%s)", resp.StatusCode, errMessage(t, resp))
	} else if msg := errMessage(t, resp); msg == "" {
		t.Error("empty error message")
	}
	if _, err := store.ModelByID(e.db, fed.ID); err != nil {
		t.Errorf("federated row deleted despite the guard: %v", err)
	}

	// Local rows are untouched by the guard: edit and delete still work.
	if resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", local.ID), `{"name":"renamed-local"}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("local PATCH = %d, want 200 (%s)", resp.StatusCode, errMessage(t, resp))
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "DELETE", pathf("/api/admin/models/%d", local.ID), "", tok); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE a local model = %d, want 204 (%s)", resp.StatusCode, errMessage(t, resp))
	}
	if _, err := store.ModelByID(e.db, local.ID); err != store.ErrNotFound {
		t.Errorf("local model still present after DELETE: %v", err)
	}
	// Unknown id keeps its 404.
	if resp := e.do(t, "DELETE", "/api/admin/models/9999", "", tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id DELETE = %d, want 404", resp.StatusCode)
		resp.Body.Close()
	}
}
