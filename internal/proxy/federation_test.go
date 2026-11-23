// Type-scoping proof, /v1 side (plan 2026-11-22-relay-federation task 1):
// a federation server token (fed-) must never authenticate the model API —
// it lives in peers, and /v1 resolves keys against api_keys only. The
// mirror proof (sk- on /federation/*) is in internal/api.
package proxy_test

import (
	"testing"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
)

func TestServerTokenRejectedOnV1(t *testing.T) {
	h := newHarness(t, agentUpstream())
	hash, _ := auth.HashPassword("x")
	owner, err := store.CreateUser(h.db, "peerowner", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := store.CreatePeer(h.db, "spoke", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The fed- token is a perfectly shaped bearer secret — but the relay
	// must not know it: no api_keys row, so authKey rejects it.
	if resp := agentGet(t, h.server.URL+"/v1/models", plain); resp.StatusCode != 401 {
		t.Errorf("server token on /v1/models = %d, want 401", resp.StatusCode)
	}
	// Sanity: the harness's own sk- key still works on the same route.
	if resp := agentGet(t, h.server.URL+"/v1/models", h.key); resp.StatusCode != 200 {
		t.Errorf("sk- key on /v1/models = %d, want 200", resp.StatusCode)
	}
}
