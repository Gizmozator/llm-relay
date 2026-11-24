// Regression tests for the final whole-repo scan fix round (backend):
// last-admin guard inside the write tx (1), UpdateModelPatch atomicity (18),
// FedModelCollision error-vs-collision (7), group prev_visibility restore
// (27) and the eligible-member enable guard (28).
package store_test

import (
	"errors"
	"testing"

	"llm-relay/internal/db"
	"llm-relay/internal/store"
)

// Item 1: demoting or disabling the only enabled admin must fail with
// ErrLastAdmin from the store itself (the count runs inside the write tx,
// so two concurrent half-edits cannot both pass a TOCTOU check).
func TestUpdateUserLastAdminGuard(t *testing.T) {
	d := migratedDB(t)
	a1, err := store.CreateUser(d, "admin1", "", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateUser(d, a1.ID, nil, ptr(true), nil, nil, nil); !errors.Is(err, store.ErrLastAdmin) {
		t.Errorf("disable sole admin err = %v, want ErrLastAdmin", err)
	}
	if _, err := store.UpdateUser(d, a1.ID, nil, nil, ptr("user"), nil, nil); !errors.Is(err, store.ErrLastAdmin) {
		t.Errorf("demote sole admin err = %v, want ErrLastAdmin", err)
	}
	// A second enabled admin unblocks both edits.
	a2, err := store.CreateUser(d, "admin2", "", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateUser(d, a1.ID, nil, ptr(true), nil, nil, nil); err != nil {
		t.Errorf("disable one of two admins: %v", err)
	}
	// admin1 is disabled now — a disabled admin is not coverage, so the
	// guard must lock again for the only remaining enabled admin.
	if _, err := store.UpdateUser(d, a2.ID, nil, ptr(true), nil, nil, nil); !errors.Is(err, store.ErrLastAdmin) {
		t.Errorf("disable the last ENABLED admin err = %v, want ErrLastAdmin", err)
	}
}

// Item 18: a PATCH that fails on a late field must apply NOTHING (one tx).
// The old per-field calls committed Enabled before the visibility check
// failed, leaving a half-applied edit the client was told had failed.
func TestUpdateModelPatchAtomic(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "up", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModelPatch(d, m.ID, store.ModelPatch{
		Enabled:    ptr(false),
		Visibility: ptr("bogus"), // fails validation after Enabled was applied
	}); err == nil {
		t.Fatal("invalid visibility must fail the patch")
	}
	got, err := store.ModelByID(d, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Error("failed patch must not have applied Enabled=false")
	}
	if got.Visibility != "public" {
		t.Errorf("visibility = %q, want untouched public", got.Visibility)
	}
	// A valid multi-field patch applies everything at once.
	if _, err := store.UpdateModelPatch(d, m.ID, store.ModelPatch{
		Enabled:    ptr(false),
		Visibility: ptr("internal"),
		Prices:     [4]*int64{ptr(int64(11)), ptr(int64(22)), ptr(int64(33)), ptr(int64(44))},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = store.ModelByID(d, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Visibility != "internal" || got.PriceIn != 11 || got.PriceOut != 22 ||
		got.PriceCacheRead != 33 || got.PriceCacheWrite != 44 {
		t.Errorf("patch not fully applied: %+v", got)
	}
}

// Item 7: a DB failure from FedModelCollision is NOT a collision — the
// caller keeps the previous materialized row instead of dropping it from
// the prune-keep set on a transient error.
func TestFedModelCollisionErrorVsCollision(t *testing.T) {
	d := migratedDB(t)
	if reason, bad, err := store.FedModelCollision(d, "free-name"); bad || err != nil {
		t.Errorf("clean db = %q/%v/%v, want no collision, no error", reason, bad, err)
	}
	// A local group with a member owns the name.
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "up", "member-model")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "taken")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, bad, err := store.FedModelCollision(d, "taken"); !bad || err != nil {
		t.Errorf("group-owned name = bad %v err %v, want collision without error", bad, err)
	}
	// Broken DB: error set, bad=false (a query failure is not a collision).
	broken, err := db.Open(t.TempDir() + "/broken.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	if _, bad, err := store.FedModelCollision(broken, "anything"); err == nil || bad {
		t.Errorf("closed db = bad %v err %v, want err != nil and bad == false", bad, err)
	}
}

// Item 27: leaving a group restores the visibility the model carried when
// it joined — an admin-hidden model must not be un-hidden by membership
// churn (the old code restored a blanket "public"). The recorded value
// also survives a wholesale membership replace that keeps the member.
func TestGroupDepartureRestoresPrevVisibility(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := store.CreateModel(d, b.ID, "up", "hidden-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModelPatch(d, hidden.ID, store.ModelPatch{Visibility: ptr("internal")}); err != nil {
		t.Fatal(err)
	}
	open, err := store.CreateModel(d, b.ID, "up2", "open-model")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "grp")
	if err != nil {
		t.Fatal(err)
	}
	// Join both.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{
		{ModelID: hidden.ID}, {ModelID: open.ID},
	}); err != nil {
		t.Fatal(err)
	}
	// Replace membership keeping `open` (prev must be CARRIED across the
	// replace, not re-recorded from the member's current 'internal'), and
	// dropping `hidden`.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{
		{ModelID: open.ID},
	}); err != nil {
		t.Fatal(err)
	}
	// Drop the last member: each model returns to its pre-join value.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ModelByID(d, hidden.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != "internal" {
		t.Errorf("admin-hidden model came back as %q, want internal", got.Visibility)
	}
	got, err = store.ModelByID(d, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != "public" {
		t.Errorf("public model came back as %q, want public", got.Visibility)
	}
}

// Item 28: enabling a group requires at least one ELIGIBLE member (model
// and backend both enabled) — a group of disabled rows would be advertised
// yet serve nothing.
func TestGroupEnableRequiresEligibleMember(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "up", "only-model")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "grp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); err != nil {
		t.Fatal(err)
	}
	// Disable the only member → enabling the group must be refused.
	if _, err := store.UpdateModelPatch(d, m.ID, store.ModelPatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); !errors.Is(err, store.ErrNoEligibleMember) {
		t.Fatalf("enable with only disabled members err = %v, want ErrNoEligibleMember", err)
	}
	// Re-enabling the member makes the same call succeed.
	if _, err := store.UpdateModelPatch(d, m.ID, store.ModelPatch{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); err != nil {
		t.Fatalf("enable with an eligible member: %v", err)
	}
}
