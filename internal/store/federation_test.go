// Federation store layer: settings validation, peer token lifecycle and the
// per-model sharing switch with allowlist (plan 2026-11-22-relay-federation,
// task 1). Reuses migratedDB/ptr from groups_test.go (same package).
package store_test

import (
	"errors"
	"strings"
	"testing"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
)

func TestFederationSettingsDefaults(t *testing.T) {
	d := migratedDB(t)
	f, err := store.GetFederationSettings(d)
	if err != nil {
		t.Fatal(err)
	}
	if f.Role != "standalone" || f.HubURL != "" || f.HubToken != "" {
		t.Errorf("fresh DB = %+v, want standalone/empty", f)
	}
}

func TestFederationSettingsPut(t *testing.T) {
	d := migratedDB(t)

	// Spoke needs a usable hub_url and a token.
	if err := store.PutFederationSettings(d, store.FederationSettings{Role: "spoke"}); !errors.Is(err, store.ErrFedConfig) {
		t.Errorf("spoke without config = %v, want ErrFedConfig", err)
	}
	if err := store.PutFederationSettings(d, store.FederationSettings{
		Role: "spoke", HubURL: "ftp://hub.example", HubToken: "fed-x",
	}); !errors.Is(err, store.ErrFedConfig) {
		t.Errorf("non-http hub_url = %v, want ErrFedConfig", err)
	}
	if err := store.PutFederationSettings(d, store.FederationSettings{Role: "nonsense"}); !errors.Is(err, store.ErrFedConfig) {
		t.Errorf("bad role = %v, want ErrFedConfig", err)
	}

	// Valid spoke config round-trips; trailing slash is normalized away.
	if err := store.PutFederationSettings(d, store.FederationSettings{
		Role: "spoke", HubURL: "https://hub.example.fi/", HubToken: " fed-secret ",
	}); err != nil {
		t.Fatal(err)
	}
	f, err := store.GetFederationSettings(d)
	if err != nil {
		t.Fatal(err)
	}
	if f.Role != "spoke" || f.HubURL != "https://hub.example.fi" || f.HubToken != "fed-secret" {
		t.Errorf("round-trip = %+v", f)
	}

	// A failed Put is all-or-nothing (one tx): a rejected write must leave
	// the previous values intact, never a half-written block.
	if err := store.PutFederationSettings(d, store.FederationSettings{
		Role: "spoke", HubURL: "ftp://nope", HubToken: "fed-secret",
	}); !errors.Is(err, store.ErrFedConfig) {
		t.Errorf("bad hub_url after valid config = %v, want ErrFedConfig", err)
	}
	if f2, _ := store.GetFederationSettings(d); f2 != f {
		t.Errorf("failed put mutated the settings: %+v, want %+v", f2, f)
	}

	// Leaving spoke mode clears the dial-in config.
	if err := store.PutFederationSettings(d, store.FederationSettings{Role: "hub"}); err != nil {
		t.Fatal(err)
	}
	f, _ = store.GetFederationSettings(d)
	if f.Role != "hub" || f.HubURL != "" || f.HubToken != "" {
		t.Errorf("after role change = %+v, want hub with cleared spoke config", f)
	}
}

func TestPeerLifecycle(t *testing.T) {
	d := migratedDB(t)
	hash, _ := auth.HashPassword("x")
	alice, err := store.CreateUser(d, "alice", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser(d, "bob", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}

	p, plain, err := store.CreatePeer(d, "home-server", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "fed-") {
		t.Errorf("token %q, want fed- prefix", plain)
	}
	if p.Name != "home-server" || p.OwnerUserID != alice.ID || p.OwnerName != "alice" || !p.Enabled {
		t.Errorf("peer = %+v", p)
	}

	// Auth lookup by hash finds it; a wrong hash does not.
	got, err := store.GetPeerByKeyHash(d, auth.HashKey(plain))
	if err != nil || got.ID != p.ID {
		t.Fatalf("GetPeerByKeyHash = %v, %v", got, err)
	}
	if _, err := store.GetPeerByKeyHash(d, auth.HashKey("fed-nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown hash = %v, want ErrNotFound", err)
	}

	// Rotate: the new token resolves, the OLD hash is dead.
	fresh, plain2, err := store.CreatePeer(d, "other", bob.ID)
	_ = fresh
	newPlain, err := store.RotatePeer(d, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPeerByKeyHash(d, auth.HashKey(plain)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old token still resolves after rotate: %v", err)
	}
	if g, err := store.GetPeerByKeyHash(d, auth.HashKey(newPlain)); err != nil || g.ID != p.ID {
		t.Errorf("new token does not resolve: %v %v", g, err)
	}
	if plain2 == "" {
		t.Fatal("second peer token empty")
	}

	// Disable / enable.
	if err := store.SetPeerEnabled(d, p.ID, false); err != nil {
		t.Fatal(err)
	}
	g, err := store.GetPeerByKeyHash(d, auth.HashKey(newPlain))
	if err != nil || g.Enabled {
		t.Errorf("disabled peer = %+v, %v", g, err)
	}
	if err := store.SetPeerEnabled(d, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPeerEnabled(d, 9999, true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("enable unknown peer = %v, want ErrNotFound", err)
	}

	// TouchPeer records liveness bookkeeping (task 2 writes it).
	if err := store.TouchPeer(d, p.ID, "2026-11-23T00:00:00Z", "0.4.2"); err != nil {
		t.Fatal(err)
	}
	g, _ = store.PeerByID(d, p.ID)
	if g.LastSeen != "2026-11-23T00:00:00Z" || g.Version != "0.4.2" {
		t.Errorf("after touch = %+v", g)
	}

	// Owner-scoped listing: alice sees only her peer, with the owner name.
	mine, err := store.ListPeersByOwner(d, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].ID != p.ID || mine[0].OwnerName != "alice" {
		t.Errorf("alice's peers = %+v", mine)
	}
	all, err := store.ListPeers(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("admin list = %d peers, want 2", len(all))
	}

	// Delete drops it (and cascades the allowlist — see sharing test).
	if err := store.DeletePeer(d, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePeer(d, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("double delete = %v, want ErrNotFound", err)
	}
}

func TestModelFedVisibility(t *testing.T) {
	d := migratedDB(t)
	hash, _ := auth.HashPassword("x")
	owner, err := store.CreateUser(d, "alice", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "up", "qwen")
	if err != nil {
		t.Fatal(err)
	}
	p1, _, err := store.CreatePeer(d, "one", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := store.CreatePeer(d, "two", owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Fresh model rows: off, empty allowlist.
	if m.FedVisibility != "off" || len(m.FedPeerIDs) != 0 {
		t.Errorf("fresh model = %q/%v, want off/[]", m.FedVisibility, m.FedPeerIDs)
	}

	// Invalid visibility rejected.
	if err := store.SetModelFedVisibility(d, m.ID, "sometimes", nil); !errors.Is(err, store.ErrFedConfig) {
		t.Errorf("bad visibility = %v, want ErrFedConfig", err)
	}
	// Unknown model.
	if err := store.SetModelFedVisibility(d, 9999, "all", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown model = %v, want ErrNotFound", err)
	}

	// Allowlist round-trip (dupes collapse), surfaced on the model row.
	if err := store.SetModelFedVisibility(d, m.ID, "allowlist", []int64{p2.ID, p1.ID, p1.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ModelByID(d, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FedVisibility != "allowlist" || len(got.FedPeerIDs) != 2 {
		t.Errorf("model = %q/%v, want allowlist/2 peers", got.FedVisibility, got.FedPeerIDs)
	}
	ids, err := store.ModelFedAllowlist(d, m.ID)
	if err != nil || len(ids) != 2 {
		t.Errorf("allowlist = %v, %v", ids, err)
	}

	// Allowlist must reference existing peers.
	if err := store.SetModelFedVisibility(d, m.ID, "allowlist", []int64{9999}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown peer = %v, want ErrNotFound", err)
	}
	// The failed call left the previous list intact (one tx).
	if ids, _ := store.ModelFedAllowlist(d, m.ID); len(ids) != 2 {
		t.Errorf("failed call mutated the list: %v", ids)
	}

	// SharedWithPeer matrix.
	for _, tc := range []struct {
		vis  string
		peer int64
		want bool
	}{
		{"all", p1.ID, true}, {"all", p2.ID, true},
		{"off", p1.ID, false},
		{"allowlist", p1.ID, true}, {"allowlist", p2.ID, true},
	} {
		if err := store.SetModelFedVisibility(d, m.ID, tc.vis, nil); err != nil {
			t.Fatal(err)
		}
		if tc.vis == "allowlist" {
			if err := store.SetModelFedVisibility(d, m.ID, "allowlist", []int64{p1.ID, p2.ID}); err != nil {
				t.Fatal(err)
			}
		}
		ok, err := store.SharedWithPeer(d, m.ID, tc.peer)
		if err != nil || ok != tc.want {
			t.Errorf("vis=%s peer=%d: %v %v, want %v", tc.vis, tc.peer, ok, err, tc.want)
		}
	}
	// allowlist with an empty list shares with nobody.
	if err := store.SetModelFedVisibility(d, m.ID, "allowlist", nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.SharedWithPeer(d, m.ID, p1.ID); ok {
		t.Error("empty allowlist shares with someone")
	}

	// SharedModels count on the peer list: 'all' models + own allowlist rows.
	if err := store.SetModelFedVisibility(d, m.ID, "all", nil); err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(d, b.ID, "up2", "llama")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelFedVisibility(d, m2.ID, "allowlist", []int64{p1.ID}); err != nil {
		t.Fatal(err)
	}
	peers, err := store.ListPeers(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		want := 2 // both models: one 'all', one allowlisted to p1
		if p.ID == p2.ID {
			want = 1 // only the 'all' model
		}
		if p.SharedModels != want {
			t.Errorf("peer %d shared_models = %d, want %d", p.ID, p.SharedModels, want)
		}
	}

	// Deleting the model cascades its allowlist rows.
	if err := store.DeleteModel(d, m2.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.ModelFedAllowlist(d, m2.ID); err != nil || len(ids) != 0 {
		t.Errorf("after model delete = %v, %v", ids, err)
	}
	// Deleting a peer cascades its allowlist rows.
	if err := store.DeletePeer(d, p1.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.ModelFedAllowlist(d, m.ID); err != nil {
		t.Fatal(err)
	} else {
		for _, id := range ids {
			if id == p1.ID {
				t.Errorf("peer delete left allowlist row: %v", ids)
			}
		}
	}
}

// shared_models must only count servable models (model AND backend
// enabled, the ListPublicModels filter): a disabled model is not shareable.
func TestSharedModelsEnabledFilter(t *testing.T) {
	d := migratedDB(t)
	hash, _ := auth.HashPassword("x")
	owner, err := store.CreateUser(d, "alice", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "up", "qwen")
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := store.CreatePeer(d, "spoke", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		peers, err := store.ListPeers(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(peers) != 1 {
			t.Fatalf("peers = %d, want 1", len(peers))
		}
		return peers[0].SharedModels
	}

	// Allowlist term: disable drops, enable restores.
	if err := store.SetModelFedVisibility(d, m.ID, "allowlist", []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("allowlisted model = %d, want 1", n)
	}
	if _, err := store.UpdateModel(d, m.ID, nil, ptr(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Errorf("disabled model still counted: %d, want 0", n)
	}
	if _, err := store.UpdateModel(d, m.ID, nil, ptr(true), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("re-enabled model = %d, want 1", n)
	}

	// 'all' term: same filter.
	if err := store.SetModelFedVisibility(d, m.ID, "all", nil); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("'all' model = %d, want 1", n)
	}
	if _, err := store.UpdateModel(d, m.ID, nil, ptr(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Errorf("disabled 'all' model still counted: %d, want 0", n)
	}
}
