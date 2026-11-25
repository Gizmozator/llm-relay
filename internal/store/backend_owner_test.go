// Backend owner user (ticket #12): the fairness-credit owner stored on a
// backend — set/clear round-trips, the invalid-user rejection, and that the
// list/read path surfaces it. The ledger effect of an owner is proven in
// fedstats_test.go.
package store_test

import (
	"errors"
	"testing"

	"llm-relay/internal/store"
)

func TestBackendOwnerSetClear(t *testing.T) {
	d := migratedDB(t)
	alice, err := store.CreateUser(d, "alice", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser(d, "bob", "", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Create with no owner: NULL, behaves exactly as a pre-#12 backend.
	b, err := store.CreateBackend(d, "gpu", "http://gpu:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.OwnerUserID != nil {
		t.Fatalf("fresh backend owner = %v, want nil", *b.OwnerUserID)
	}

	// Set an owner at create time.
	b2, err := store.CreateBackendWithOwner(d, "gpu2", "http://gpu2:8080", "", &alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b2.OwnerUserID == nil || *b2.OwnerUserID != alice.ID {
		t.Fatalf("create-with-owner = %v, want alice(%d)", b2.OwnerUserID, alice.ID)
	}

	// Re-point it at another user.
	got, err := store.SetBackendOwner(d, b.ID, &bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerUserID == nil || *got.OwnerUserID != bob.ID {
		t.Fatalf("after set = %v, want bob(%d)", got.OwnerUserID, bob.ID)
	}

	// Clear it back to uncredited hub hardware.
	got, err = store.SetBackendOwner(d, b.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerUserID != nil {
		t.Fatalf("after clear = %v, want nil", *got.OwnerUserID)
	}

	// The list path reads owner_user_id back for every row.
	list, err := store.ListBackends(d)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*store.Backend{}
	for i := range list {
		byName[list[i].Name] = &list[i]
	}
	if o := byName["gpu"].OwnerUserID; o != nil {
		t.Errorf("list gpu owner = %v, want nil", *o)
	}
	if o := byName["gpu2"].OwnerUserID; o == nil || *o != alice.ID {
		t.Errorf("list gpu2 owner = %v, want alice(%d)", o, alice.ID)
	}
}

func TestBackendOwnerInvalidUser(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "gpu", "http://gpu:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	missing := int64(9999)

	// Setting a non-existent owner is rejected with ErrInvalid (a clear client
	// error), not a raw FK violation.
	if _, err := store.SetBackendOwner(d, b.ID, &missing); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("SetBackendOwner(missing) = %v, want ErrInvalid", err)
	}
	// And the backend keeps no owner after the refused write.
	cur, err := store.BackendByID(d, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.OwnerUserID != nil {
		t.Errorf("owner after refused set = %v, want nil", *cur.OwnerUserID)
	}
	// Create-with-owner validates the same way.
	if _, err := store.CreateBackendWithOwner(d, "gpu9", "http://gpu9:1", "", &missing); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("CreateBackendWithOwner(missing) = %v, want ErrInvalid", err)
	}
	// An unknown backend is ErrNotFound, not ErrInvalid.
	if _, err := store.SetBackendOwner(d, 4242, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetBackendOwner(unknown backend) = %v, want ErrNotFound", err)
	}
}

// A synthetic peer://<id> backend carries no owner_user_id of its own —
// the ownership of federated hardware lives on the peers row. ListBackends
// resolves the effective owner through it so the admin Backends Owner
// column names the peer's owner instead of "—". A hub:// row (a spoke's
// view of the hub) has no peers row and stays unowned.
func TestListBackendsSyntheticOwner(t *testing.T) {
	d := migratedDB(t)
	alice, err := store.CreateUser(d, "alice", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := store.CreatePeer(d, "spoke-a", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBackend(d, "federation: spoke-a", store.PeerBackendURL(p.ID), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBackend(d, "hub-side", store.FedHubBaseURL, ""); err != nil {
		t.Fatal(err)
	}
	// An explicit owner still wins over the peer resolution (COALESCE order).
	p2, _, err := store.CreatePeer(d, "spoke-b", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser(d, "bob", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBackendWithOwner(d, "federation: spoke-b",
		store.PeerBackendURL(p2.ID), "", &bob.ID); err != nil {
		t.Fatal(err)
	}

	list, err := store.ListBackends(d)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*store.Backend{}
	for i := range list {
		byName[list[i].Name] = &list[i]
	}
	if o := byName["federation: spoke-a"].OwnerUserID; o == nil || *o != alice.ID {
		t.Errorf("synthetic backend owner = %v, want alice(%d) via the peer", o, alice.ID)
	}
	if o := byName["federation: spoke-b"].OwnerUserID; o == nil || *o != bob.ID {
		t.Errorf("explicit owner overridden: %v, want bob(%d)", o, bob.ID)
	}
	if o := byName["hub-side"].OwnerUserID; o != nil {
		t.Errorf("hub:// backend owner = %v, want nil (no peers row)", *o)
	}
}
