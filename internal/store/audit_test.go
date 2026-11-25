// Audit trail tests (plan 2026-11-25-audit-log-v1): the store insert/query
// surface (filters, paging, newest-first) and the API read gate + a couple
// of instrumented flows.
package store_test

import (
	"testing"

	"llm-relay/internal/store"
)

func TestAuditInsertQuery(t *testing.T) {
	d := migratedDB(t)
	for _, e := range []struct{ actor, action, target, detail string }{
		{"alice", "user.create", "bob", "role=user"},
		{"alice", "backend.create", "halogen", "http://x:1"},
		{"system", "fed.peer.connect", "spoke-a", "version=v0.1.0"},
		{"bob", "model.update", "qwen", "enabled=false"},
	} {
		if err := store.InsertAudit(d, e.actor, e.action, e.target, e.detail); err != nil {
			t.Fatal(err)
		}
	}

	all, total, err := store.QueryAudit(d, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(all) != 4 {
		t.Fatalf("total=%d len=%d, want 4", total, len(all))
	}
	// Newest first.
	if all[0].Action != "model.update" {
		t.Errorf("newest = %s, want model.update", all[0].Action)
	}
	if all[0].Actor != "bob" || all[0].Target != "qwen" {
		t.Errorf("newest row = %+v", all[0])
	}

	if page, total, err := store.QueryAudit(d, store.AuditFilter{Category: "backend"}); err != nil ||
		total != 1 || page[0].Action != "backend.create" {
		t.Errorf("category filter: %d/%d %v", len(page), total, err)
	}
	if page, total, err := store.QueryAudit(d, store.AuditFilter{Actor: "system"}); err != nil ||
		total != 1 || page[0].Action != "fed.peer.connect" {
		t.Errorf("actor filter: %d/%d %v", len(page), total, err)
	}
	if page, total, err := store.QueryAudit(d, store.AuditFilter{Q: "spoke"}); err != nil ||
		total != 1 || page[0].Target != "spoke-a" {
		t.Errorf("q filter: %d/%d %v", len(page), total, err)
	}
	// Paging: total stays the full count, the page is the slice.
	page, total, err := store.QueryAudit(d, store.AuditFilter{Limit: 2, Offset: 2})
	if err != nil || total != 4 || len(page) != 2 || page[0].Action != "backend.create" {
		t.Errorf("paging: %d/%d %v", len(page), total, err)
	}
}

func TestDeleteUser(t *testing.T) {
	d := migratedDB(t)
	hash := "hash"
	admin, _ := store.CreateUser(d, "root", "", hash, "admin")
	victim, _ := store.CreateUser(d, "victim", "", hash, "user")
	other, _ := store.CreateUser(d, "other", "", hash, "user")

	// The victim owns a key and a backend; other owns a peer.
	if _, err := store.CreateKey(d, victim.ID, "k", "hashabc", "hasha"); err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateBackendWithOwner(d, "owned", "http://x:1", "", &victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := store.CreatePeer(d, "spoke-x", other.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteUser(d, victim.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.UserByID(d, victim.ID); err != store.ErrNotFound {
		t.Error("victim still present")
	}
	// Keys die with the account.
	keys, err := store.ListKeys(d, victim.ID)
	if err != nil || len(keys) != 0 {
		t.Errorf("victim keys = %d (%v), want 0", len(keys), err)
	}
	// The backend survives, unowned.
	b2, err := store.BackendByID(d, b.ID)
	if err != nil || b2.OwnerUserID != nil {
		t.Errorf("backend owner = %v (%v), want cleared", b2.OwnerUserID, err)
	}
	// The last enabled admin cannot be deleted.
	if err := store.DeleteUser(d, admin.ID); err != store.ErrLastAdmin {
		t.Errorf("last admin delete = %v, want ErrLastAdmin", err)
	}
	// An owner of federation servers is refused.
	if err := store.DeleteUser(d, other.ID); err != store.ErrUserHasPeers {
		t.Errorf("peer-owner delete = %v, want ErrUserHasPeers", err)
	}
	// After the peer goes, the owner can go too.
	if err := store.DeletePeer(d, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUser(d, other.ID); err != nil {
		t.Errorf("delete after peer removal: %v", err)
	}
}
