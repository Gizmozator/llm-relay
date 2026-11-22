// Model groups: CRUD, name-collision rules, group-aware resolution and the
// public catalog (plan 2026-11-22, task 1). The migration-rebuild test lives
// in internal/db (it needs the pre-0010 schema).
package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"llm-relay/internal/db"
	"llm-relay/internal/store"
)

func ptr[T any](v T) *T { return &v }

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/g.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestGroupCRUD(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := store.CreateModel(d, b1.ID, "qwen-up", "qwen-a")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(d, b2.ID, "qwen-up", "qwen-b")
	if err != nil {
		t.Fatal(err)
	}

	g, err := store.CreateGroup(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if g.Enabled || g.MemberCount != 0 {
		t.Errorf("new group = %+v, want born disabled with 0 members", g)
	}
	if _, err := store.CreateGroup(d, "qwen3-coder"); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate group name err = %v, want ErrExists", err)
	}

	list, err := store.ListGroups(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "qwen3-coder" {
		t.Fatalf("ListGroups = %+v, want one qwen3-coder", list)
	}

	// Replace members: weights + positions stick, order is by position.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{
		{ModelID: m2.ID, Weight: 2.5, Position: 0},
		{ModelID: m1.ID, Weight: 1, Position: 1},
	}); err != nil {
		t.Fatal(err)
	}
	got, members, err := store.GetGroup(d, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemberCount != 2 {
		t.Fatalf("MemberCount = %d, want 2", got.MemberCount)
	}
	if len(members) != 2 || members[0].ModelID != m2.ID || members[1].ModelID != m1.ID {
		t.Fatalf("members = %+v, want m2 (pos 0) then m1", members)
	}
	if members[0].Weight != 2.5 || members[0].Position != 0 {
		t.Errorf("member weight/position = %v/%d, want 2.5/0", members[0].Weight, members[0].Position)
	}
	if members[0].Model == nil || members[0].Model.Name != "qwen-b" ||
		members[0].Backend == nil || members[0].Backend.Name != "node-b" {
		t.Errorf("member join wrong: %+v", members[0])
	}
	if !members[0].Backend.Enabled || members[0].Backend.Draining {
		t.Errorf("backend flags wrong: enabled=%v draining=%v", members[0].Backend.Enabled, members[0].Backend.Draining)
	}

	// Backend draining is visible through the member join.
	if _, err := store.UpdateBackend(d, b2.ID, nil, nil, nil, nil, ptr(true)); err != nil {
		t.Fatal(err)
	}
	_, members, err = store.GetGroup(d, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !members[0].Backend.Draining {
		t.Error("draining backend not visible on member")
	}

	// A group with members can be enabled (it was born disabled).
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); err != nil {
		t.Fatal(err)
	}

	// Rename + disable keep members.
	upd, err := store.UpdateGroup(d, g.ID, ptr("pool-x"), ptr(false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Name != "pool-x" || upd.Enabled || upd.MemberCount != 2 {
		t.Errorf("after rename/disable: %+v", upd)
	}

	// Empty (non-nil) member list clears the group.
	cleared, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.MemberCount != 0 {
		t.Errorf("after clear: MemberCount = %d, want 0", cleared.MemberCount)
	}

	if err := store.DeleteGroup(d, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetGroup(d, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetGroup after delete = %v, want ErrNotFound", err)
	}
	// Members themselves stay in the catalog.
	if _, err := store.ModelByID(d, m1.ID); err != nil {
		t.Errorf("model deleted with its group: %v", err)
	}
}

func TestGroupNameCollisions(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "eng", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := store.CreateModel(d, b.ID, "pub-up", "shared-name")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(d, "shared-name"); !errors.Is(err, store.ErrExists) {
		t.Errorf("group vs public model = %v, want ErrExists", err)
	}
	// Internal models do not block the name (the group owns the public name).
	if _, err := d.Exec(`UPDATE models SET visibility='internal' WHERE id=?`, pub.ID); err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "shared-name")
	if err != nil {
		t.Fatalf("group vs internal model: %v", err)
	}
	if _, err := store.CreateGroup(d, "shared-name"); !errors.Is(err, store.ErrExists) {
		t.Errorf("group vs group = %v, want ErrExists", err)
	}
	// A disabled public model still holds its public name.
	off, err := store.CreateModel(d, b.ID, "off-up", "off-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, off.ID, nil, ptr(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(d, "off-model"); !errors.Is(err, store.ErrExists) {
		t.Errorf("group vs disabled public model = %v, want ErrExists", err)
	}
	if _, err := store.CreateGroup(d, "  "); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("blank group name = %v, want ErrInvalid", err)
	}

	// Renames re-run the same rules.
	other, err := store.CreateGroup(d, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, ptr("other"), nil, nil); !errors.Is(err, store.ErrExists) {
		t.Errorf("rename onto another group = %v, want ErrExists", err)
	}
	if _, err := store.UpdateGroup(d, g.ID, ptr("off-model"), nil, nil); !errors.Is(err, store.ErrExists) {
		t.Errorf("rename onto public model name = %v, want ErrExists", err)
	}
	if _, err := store.UpdateGroup(d, g.ID, ptr(""), nil, nil); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("rename to blank = %v, want ErrInvalid", err)
	}
	// Renaming to its own name is a no-op, not a collision.
	if _, err := store.UpdateGroup(d, other.ID, ptr("other"), nil, nil); err != nil {
		t.Errorf("self-rename = %v, want ok", err)
	}
	if _, err := store.UpdateGroup(d, 9999, ptr("x"), nil, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update unknown group = %v, want ErrNotFound", err)
	}
}

func TestGroupMemberRules(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "eng", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "up", "solo")
	if err != nil {
		t.Fatal(err)
	}
	g1, err := store.CreateGroup(d, "g1")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := store.CreateGroup(d, "g2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g1.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); err != nil {
		t.Fatal(err)
	}
	// One group per model: model_id is UNIQUE across memberships.
	if _, err := store.UpdateGroup(d, g2.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("model in second group = %v, want ErrConflict", err)
	}
	// Unknown model id → ErrNotFound, and the failed update left g2 empty.
	if _, err := store.UpdateGroup(d, g2.ID, nil, nil, []store.GroupMemberInput{{ModelID: 9999}}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown model id = %v, want ErrNotFound", err)
	}
	if _, members, err := store.GetGroup(d, g2.ID); err != nil || len(members) != 0 {
		t.Errorf("failed update mutated members: n=%d err=%v", len(members), err)
	}
	// Same model twice in one input is rejected before insert.
	if _, err := store.UpdateGroup(d, g2.ID, nil, nil,
		[]store.GroupMemberInput{{ModelID: m.ID}, {ModelID: m.ID}}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate input model = %v, want ErrConflict", err)
	}
	if err := store.DeleteGroup(d, g1.ID); err != nil {
		t.Fatal(err)
	}
	// After the first group is gone the model can join another.
	if _, err := store.UpdateGroup(d, g2.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); err != nil {
		t.Errorf("re-member after delete = %v, want ok", err)
	}
}

func TestModelByNameVisibility(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	// Same name on two backends inserts fine (0010); same name+backend does not.
	x1, err := store.CreateModel(d, b1.ID, "up-a", "dup")
	if err != nil {
		t.Fatal(err)
	}
	x2, err := store.CreateModel(d, b2.ID, "up-b", "dup")
	if err != nil {
		t.Fatalf("same name on second backend: %v", err)
	}
	if _, err := store.CreateModel(d, b1.ID, "up-c", "dup"); !errors.Is(err, store.ErrExists) {
		t.Errorf("same name+backend = %v, want ErrExists", err)
	}
	if x1.Visibility != "public" {
		t.Errorf("new model visibility = %q, want public", x1.Visibility)
	}

	// Public tie → lowest id.
	m, _, err := store.ModelByName(d, "dup")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != x1.ID {
		t.Errorf("tie resolved to %d, want lowest id %d", m.ID, x1.ID)
	}
	// Public wins over internal even when the internal row has the lower id.
	if _, err := d.Exec(`UPDATE models SET visibility='internal' WHERE id=?`, x1.ID); err != nil {
		t.Fatal(err)
	}
	m, _, err = store.ModelByName(d, "dup")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != x2.ID {
		t.Errorf("public/internal tie resolved to %d, want public %d", m.ID, x2.ID)
	}
	// An internal-only model is still directly resolvable (pin/debug).
	hidden, err := store.CreateModel(d, b1.ID, "hidden-up", "hidden")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE models SET visibility='internal' WHERE id=?`, hidden.ID); err != nil {
		t.Fatal(err)
	}
	m, _, err = store.ModelByName(d, "hidden")
	if err != nil || m.ID != hidden.ID {
		t.Errorf("internal direct lookup = %v, %v, want the model", m, err)
	}
	// Disabled model or disabled backend → not found.
	off, err := store.CreateModel(d, b1.ID, "off-up", "off")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, off.ID, nil, ptr(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ModelByName(d, "off"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("disabled model = %v, want ErrNotFound", err)
	}
	onb2, err := store.CreateModel(d, b2.ID, "onb2-up", "onb2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateBackend(d, b2.ID, nil, nil, nil, ptr(false), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ModelByName(d, onb2.Name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("model on disabled backend = %v, want ErrNotFound", err)
	}
}

func TestResolveTarget(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	// The classic shape: group owns the name, members are internal and share it.
	g, err := store.CreateGroup(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := store.CreateModel(d, b1.ID, "qwen", "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(d, b2.ID, "qwen", "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE models SET visibility='internal'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{
		{ModelID: m1.ID, Weight: 1, Position: 0},
		{ModelID: m2.ID, Weight: 1, Position: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); err != nil {
		t.Fatal(err)
	}

	// Group wins over the same-name models.
	tg, err := store.ResolveTarget(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Kind != store.TargetGroup || tg.Group == nil || tg.Group.ID != g.ID {
		t.Fatalf("resolved = %+v, want group target", tg)
	}
	if tg.Model != nil || tg.Backend != nil {
		t.Error("group target must not carry a model/backend")
	}
	if len(tg.Members) != 2 {
		t.Fatalf("group members = %d, want 2 (exclusion is the caller's job)", len(tg.Members))
	}
	// Disabled members are still returned so the caller can decide.
	if _, err := store.UpdateModel(d, m2.ID, nil, ptr(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	tg, err = store.ResolveTarget(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.Members) != 2 || tg.Members[1].Model.Enabled {
		t.Errorf("disabled member dropped from target: %+v", tg.Members)
	}
	if _, err := store.UpdateModel(d, m2.ID, nil, ptr(true), [4]*int64{}); err != nil {
		t.Fatal(err)
	}

	// Members stay directly resolvable by name.
	dm, _, err := store.ModelByName(d, "qwen3-coder")
	if err != nil {
		t.Fatalf("member direct lookup: %v", err)
	}
	if dm.ID != m1.ID {
		t.Errorf("member lookup = %d, want %d", dm.ID, m1.ID)
	}

	// A disabled group falls through to the model path.
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(false), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE models SET visibility='public' WHERE id=?`, m1.ID); err != nil {
		t.Fatal(err)
	}
	tg, err = store.ResolveTarget(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Kind != store.TargetModel || tg.Model == nil || tg.Model.ID != m1.ID || tg.Backend == nil {
		t.Errorf("disabled group resolved to %+v, want model %d", tg, m1.ID)
	}

	// Plain model name → model target; unknown name → ErrNotFound.
	solo, err := store.CreateModel(d, b1.ID, "solo", "solo-model")
	if err != nil {
		t.Fatal(err)
	}
	tg, err = store.ResolveTarget(d, "solo-model")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Kind != store.TargetModel || tg.Model.ID != solo.ID || tg.Backend.ID != b1.ID {
		t.Errorf("model target = %+v", tg)
	}
	if _, err := store.ResolveTarget(d, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown name = %v, want ErrNotFound", err)
	}
}

func TestListPublicCatalog(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "eng", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b.ID, "pub", "pub-model"); err != nil {
		t.Fatal(err)
	}
	member, err := store.CreateModel(d, b.ID, "mem", "member-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE models SET visibility='internal' WHERE id=?`, member.ID); err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: member.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); err != nil {
		t.Fatal(err)
	}
	off, err := store.CreateModel(d, b.ID, "off", "off-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, off.ID, nil, ptr(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}

	cat, err := store.ListPublicCatalog(d)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]store.PublicEntry{}
	for _, e := range cat {
		names[e.Name] = e
	}
	if len(cat) != 2 {
		t.Fatalf("catalog = %+v, want pool + pub-model", cat)
	}
	if e, ok := names["pool"]; !ok || e.Kind != store.TargetGroup || e.Model != nil {
		t.Errorf("group entry = %+v", e)
	}
	if e, ok := names["pub-model"]; !ok || e.Kind != store.TargetModel || e.Model == nil {
		t.Errorf("model entry = %+v", e)
	}
	if _, ok := names["member-x"]; ok {
		t.Error("internal member listed")
	}
	if _, ok := names["off-model"]; ok {
		t.Error("disabled model listed")
	}

	// Disabling the group hides it; the internal member stays hidden.
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(false), nil); err != nil {
		t.Fatal(err)
	}
	cat, err = store.ListPublicCatalog(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat) != 1 || cat[0].Name != "pub-model" {
		t.Errorf("after disable group: %+v, want only pub-model", cat)
	}
	// ListPublicModels (models only) also hides internal members.
	models, err := store.ListPublicModels(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Name != "pub-model" {
		t.Errorf("ListPublicModels = %+v, want only pub-model", models)
	}
}

// The import path (AddModels) must accept a name that already exists on
// another backend — the collision fix that motivated groups.
func TestAddModelsAcrossBackends(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b1.ID, "qwen3-coder", "qwen3-coder"); err != nil {
		t.Fatal(err)
	}
	// The second backend imports the same name without collision (0010);
	// only a repeat on the same backend is skipped.
	created, skipped, err := store.AddModels(d, b2.ID, []string{"qwen3-coder", "fresh", "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0].Name != "qwen3-coder" || created[1].Name != "fresh" {
		t.Errorf("AddModels created = %v, want [qwen3-coder fresh]", created)
	}
	if len(skipped) != 1 || skipped[0] != "fresh" {
		t.Errorf("AddModels skipped = %v, want [fresh]", skipped)
	}
	// Both backends now carry a row with the shared name; the tie resolves to
	// the lowest id (backend a's row).
	m, _, err := store.ModelByName(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if m.BackendID != b1.ID {
		t.Errorf("shared name resolved to backend %d, want %d (lowest id)", m.BackendID, b1.ID)
	}
}

func TestDeleteModelUnmembers(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "qwen-up", "qwen-a")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "qwen-pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// Deleting a grouped model must succeed (membership removed first),
	// not abort on the model_group_members FK.
	if err := store.DeleteModel(d, m.ID); err != nil {
		t.Fatalf("DeleteModel on grouped model: %v", err)
	}
	grp, members, err := store.GetGroup(d, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grp.MemberCount != 0 || len(members) != 0 {
		t.Errorf("group still has members after model delete: %d/%d", grp.MemberCount, len(members))
	}
}

func TestAddModelsImportsGroupNames(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "member-up", "member-m")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true),
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// A freshly added replica syncing the same upstream id MUST get its
	// row even though the enabled group owns that name: this row is the
	// future member (same-backend re-adds still skip on the UNIQUE
	// constraint).
	b2, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	created, skipped, err := store.AddModels(d, b2.ID, []string{"qwen3-coder", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || len(skipped) != 0 {
		t.Fatalf("created=%d skipped=%d, want both names imported", len(created), len(skipped))
	}
	// Shadowed while ungrouped: the group still wins the name.
	tgt, err := store.ResolveTarget(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Kind != store.TargetGroup {
		t.Errorf("imported same-name row must not steal the group name, kind=%v", tgt.Kind)
	}
	// Steady-state edit (weight tweak) passes despite the public same-name
	// import row: the group already served and shadowed before the call.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil,
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 2}}); err != nil {
		t.Errorf("steady-state edit blocked by ungrouped import row: %v", err)
	}
	// A no-op rename (UI PUT echoing the group's own name) must pass over
	// the import row it already shadows: echoing the name is not a rename.
	if _, err := store.UpdateGroup(d, g.ID, ptr("qwen3-coder"), nil,
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Errorf("no-op rename blocked by already-shadowed import row: %v", err)
	}
	// Claiming the replica row into the group works (member flips to
	// internal before the ownership checks run).
	var row store.Model
	for _, c := range created {
		if c.Name == "qwen3-coder" {
			row = c
		}
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{
		{ModelID: m.ID, Weight: 1}, {ModelID: row.ID, Weight: 1},
	}); err != nil {
		t.Fatalf("claiming imported replica row: %v", err)
	}
}

// The model side of the name-collision rules: a non-empty group owns its
// name against CreateModel and UpdateModel renames; an empty group reserves
// nothing.
func TestModelNameRespectsGroupNames(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "member-up", "member-m")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "pool")
	if err != nil {
		t.Fatal(err)
	}
	// Empty group: the name is free for models.
	if _, err := store.CreateModel(d, b.ID, "free-up", "pool"); err != nil {
		t.Errorf("empty group must not block model names: %v", err)
	}
	// Non-empty group: creation and renames are rejected.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b.ID, "x-up", "pool"); !errors.Is(err, store.ErrExists) {
		t.Errorf("CreateModel onto group name = %v, want ErrExists", err)
	}
	other, err := store.CreateModel(d, b2.ID, "other-up", "other-m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, other.ID, ptr("pool"), nil, [4]*int64{}); !errors.Is(err, store.ErrExists) {
		t.Errorf("UpdateModel rename onto group name = %v, want ErrExists", err)
	}
	// Deleting the group frees the name again (other lives on b2: on b the
	// free-up model already holds UNIQUE(backend_id,name)).
	if err := store.DeleteGroup(d, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, other.ID, ptr("pool"), nil, [4]*int64{}); err != nil {
		t.Errorf("rename after group delete = %v, want ok", err)
	}
}

func TestGroupEnableRulesAndTrim(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "qwen-up", "qwen-a")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "  padded-name  ")
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "padded-name" {
		t.Errorf("stored name = %q, want trimmed padded-name", g.Name)
	}
	// Empty group cannot be enabled.
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(false), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); !errors.Is(err, store.ErrConflict) {
		t.Errorf("enable empty group err = %v, want ErrConflict", err)
	}
	// With a member it enables, and an empty group is not advertised.
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), []store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(false), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); err != nil {
		t.Errorf("enable with existing members: %v", err)
	}
	// Disabled group: ResolveTarget must fall through to a same-name model.
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(false), nil); err != nil {
		t.Fatal(err)
	}
	cat, err := store.ListPublicCatalog(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cat {
		if e.Name == "padded-name" {
			t.Errorf("disabled group advertised in public catalog")
		}
	}
}

func TestMembershipVisibilityFlip(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "qwen-up", "qwen-a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Visibility != "public" {
		t.Fatalf("fresh model visibility = %q, want public", m.Visibility)
	}
	g, err := store.CreateGroup(d, "qwen-pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// Joined: hidden from the public list, still directly resolvable.
	pub, err := store.ListPublicModels(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, pm := range pub {
		if pm.Name == "qwen-a" {
			t.Errorf("group member still listed in public models")
		}
	}
	if _, _, err := store.ModelByName(d, "qwen-a"); err != nil {
		t.Errorf("internal member not directly resolvable: %v", err)
	}
	tgt, err := store.ResolveTarget(d, "qwen-a")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Kind != store.TargetModel {
		t.Errorf("ResolveTarget(member) kind = %v, want TargetModel", tgt.Kind)
	}
	// Left: public again and re-listed.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{}); err != nil {
		t.Fatal(err)
	}
	pub, err = store.ListPublicModels(d)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pm := range pub {
		if pm.Name == "qwen-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("removed member not restored to public list")
	}
}

func TestDeleteGroupRestoresVisibility(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "qwen-up", "qwen-a")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "qwen-pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(d, g.ID); err != nil {
		t.Fatal(err)
	}
	pub, err := store.ListPublicModels(d)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pm := range pub {
		if pm.Name == "qwen-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("model of deleted group not restored to public list")
	}
}

// countDual counts catalog entries named "dual" by kind.
func countDual(t *testing.T, d *sql.DB) (int, int) {
	t.Helper()
	cat, err := store.ListPublicCatalog(d)
	if err != nil {
		t.Fatal(err)
	}
	groups, models := 0, 0
	for _, e := range cat {
		if e.Name == "dual" {
			if e.Kind == store.TargetGroup {
				groups++
			} else {
				models++
			}
		}
	}
	return groups, models
}

func TestEmptyGroupFallThrough(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "dual")
	if err != nil {
		t.Fatal(err)
	}
	// Both members share the group name (allowed while the group is empty).
	m1, err := store.CreateModel(d, b1.ID, "u1", "dual")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(d, b2.ID, "u2", "dual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), []store.GroupMemberInput{
		{ModelID: m1.ID, Weight: 1}, {ModelID: m2.ID, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	tgt, err := store.ResolveTarget(d, "dual")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Kind != store.TargetGroup {
		t.Fatalf("populated group should win, got kind %v", tgt.Kind)
	}
	// Empty the group (stays enabled): both members turn public again, so
	// resolution and the catalog must fall through to the same-name model
	// instead of dead-ending (dedupe keeps the lowest id, m1).
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{}); err != nil {
		t.Fatal(err)
	}
	tgt, err = store.ResolveTarget(d, "dual")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Kind != store.TargetModel || tgt.Model == nil || tgt.Model.ID != m1.ID {
		t.Errorf("empty group must fall through to same-name model, got %+v", tgt)
	}
	if groups, models := countDual(t, d); groups != 0 || models != 1 {
		t.Errorf("catalog for 'dual': %d group entries, %d model entries, want 0/1", groups, models)
	}
	// Re-populate both and enable: the group wins again...
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), []store.GroupMemberInput{
		{ModelID: m1.ID, Weight: 1}, {ModelID: m2.ID, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	tgt, err = store.ResolveTarget(d, "dual")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Kind != store.TargetGroup {
		t.Fatalf("re-populated group should win, got kind %v", tgt.Kind)
	}
	// m2 leaves the group (public again) while the group stays enabled with
	// m1. Disabling m1's model leaves no ELIGIBLE member: same fall-through
	// as an empty group, to the same-name public model m2, and the catalog
	// drops the group entry.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m1.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, m1.ID, nil, ptr(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	tgt, err = store.ResolveTarget(d, "dual")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Kind != store.TargetModel || tgt.Model == nil || tgt.Model.ID != m2.ID {
		t.Errorf("group with only ineligible members must fall through to m2, got %+v", tgt)
	}
	if groups, models := countDual(t, d); groups != 0 || models != 1 {
		t.Errorf("catalog for 'dual' with ineligible member: %d group entries, %d model entries, want 0/1", groups, models)
	}
}

// Two enabled public models sharing a name on different backends must not
// both appear in /v1/models: the catalog keeps one entry per name — the
// lowest id, matching ModelByName's tie-break.
func TestListPublicCatalogDedupesNames(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	d1, err := store.CreateModel(d, b1.ID, "dup-up", "dup-name")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b2.ID, "dup-up", "dup-name"); err != nil {
		t.Fatal(err)
	}
	cat, err := store.ListPublicCatalog(d)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var kept *store.Model
	for _, e := range cat {
		if e.Name == "dup-name" {
			n++
			kept = e.Model
		}
	}
	if n != 1 || kept == nil || kept.ID != d1.ID {
		t.Errorf("dup-name entries = %d (kept %+v), want exactly 1 with lowest id %d", n, kept, d1.ID)
	}
}

func TestMemberWeightNormalization(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "u", "wm")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "wpool")
	if err != nil {
		t.Fatal(err)
	}
	// Omitted (zero) and negative weights normalize to the default 1:
	// a 0 weight would silently remove the member from the HRW rotation.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{
		{ModelID: m.ID, Weight: 0},
	}); err != nil {
		t.Fatal(err)
	}
	_, members, err := store.GetGroup(d, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Weight != 1 {
		t.Fatalf("members = %+v, want single member with weight 1", members)
	}
}

func TestEnabledGroupNameOwnershipPostCondition(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "dual")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := store.CreateModel(d, b1.ID, "u1", "dual")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(d, b2.ID, "u2", "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), []store.GroupMemberInput{{ModelID: m1.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// Replacing the member that carries the group's own name must pass:
	// the departing member turns public under that name in the same call
	// and is exempt from the ownership check.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m2.ID, Weight: 1}}); err != nil {
		t.Fatalf("replace member carrying the group name: %v", err)
	}
	// Empty the group: m1 ("dual") is public again while the enabled group
	// sits empty (empty groups reserve nothing).
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{}); err != nil {
		t.Fatal(err)
	}
	// Re-populating would silently shadow the public "dual" model: the
	// post-condition must reject it.
	_, err = store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m2.ID, Weight: 1}})
	if !errors.Is(err, store.ErrExists) {
		t.Errorf("repopulate over public same-name model: err = %v, want ErrExists", err)
	}
}

func TestListPublicModelsDedupesNames(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := store.CreateModel(d, b1.ID, "dup-up", "dup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b2.ID, "dup-up", "dup"); err != nil {
		t.Fatal(err)
	}
	pub, err := store.ListPublicModels(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub) != 1 {
		t.Fatalf("public list = %d entries, want 1 (same-name replicas collapse)", len(pub))
	}
	if pub[0].ID != m1.ID {
		t.Errorf("kept id = %d, want lowest %d", pub[0].ID, m1.ID)
	}
}

func TestRenameClaimInOneCall(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "oldname")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "u", "newname")
	if err != nil {
		t.Fatal(err)
	}
	// Renaming onto a public model's name while claiming that very model as
	// the member must succeed: the member flips internal in the same call,
	// so nothing is shadowed at the end state.
	if _, err := store.UpdateGroup(d, g.ID, ptr("newname"), nil,
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatalf("rename+claim in one call: %v", err)
	}
	// Renaming onto a still-public name without claiming it stays rejected.
	g2, err := store.CreateGroup(d, "other")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(d, b.ID, "u2", "held")
	if err != nil {
		t.Fatal(err)
	}
	_ = m2
	if _, err := store.UpdateGroup(d, g2.ID, ptr("held"), nil, nil); !errors.Is(err, store.ErrExists) {
		t.Errorf("rename onto public model name: err = %v, want ErrExists", err)
	}
}

func TestRenameWithDepartingNameCarrier(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "pool")
	if err != nil {
		t.Fatal(err)
	}
	// The typical shape: members carry the (future) group name.
	m1, err := store.CreateModel(d, b1.ID, "u1", "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(d, b2.ID, "u2", "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), []store.GroupMemberInput{
		{ModelID: m1.ID, Weight: 1}, {ModelID: m2.ID, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Rename onto the members' name while one of them departs in the same
	// call: the departing member's public flip is the call's own doing and
	// must not block the rename.
	if _, err := store.UpdateGroup(d, g.ID, ptr("qwen3-coder"), nil,
		[]store.GroupMemberInput{{ModelID: m2.ID, Weight: 1}}); err != nil {
		t.Fatalf("rename onto departing member's name: %v", err)
	}
	// An external public same-name model (not a member) still blocks.
	g2, err := store.CreateGroup(d, "pool2")
	if err != nil {
		t.Fatal(err)
	}
	m3, err := store.CreateModel(d, b1.ID, "u3", "held2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g2.ID, ptr("held2"), nil,
		[]store.GroupMemberInput{{ModelID: m3.ID, Weight: 1}}); err != nil {
		// m3 is claimed as a member in the same call: must pass.
		t.Errorf("rename+claim external model: %v", err)
	}
}

func TestUpdateModelNoopRenamePastGroup(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	// Created while the group is still empty (empty groups reserve nothing).
	m, err := store.CreateModel(d, b.ID, "u", "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil,
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// Echoing the model's own name (a UI PUT round-trip) must not trip the
	// group guard: the member legitimately carries the group's name.
	if _, err := store.UpdateModel(d, m.ID, ptr("qwen3-coder"), nil, [4]*int64{}); err != nil {
		t.Errorf("no-op rename on member model: %v", err)
	}
	// A real rename away and back onto the group-owned name still rejects.
	other, err := store.CreateModel(d, b.ID, "u2", "spare")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, other.ID, ptr("qwen3-coder"), nil, [4]*int64{}); !errors.Is(err, store.ErrExists) {
		t.Errorf("rename onto group-owned name: err = %v, want ErrExists", err)
	}
}

func TestIneligibleGroupEditsPassOverPublicName(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "gname")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b1.ID, "u", "mm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true),
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// Public same-name row via the import path.
	if _, _, err := store.AddModels(d, b2.ID, []string{"gname"}); err != nil {
		t.Fatal(err)
	}
	// Disable the member's model: the group is enabled but cannot serve,
	// so it shadows nothing (fall-through keeps the public row resolvable).
	off := false
	if _, err := store.UpdateModel(d, m.ID, nil, &off, [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	// A pure weight tweak must pass: no transition into serving.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil,
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 5}}); err != nil {
		t.Errorf("edit on unable-to-serve group: %v", err)
	}
	// Gaining an ELIGIBLE member over the public same-name row is a
	// transition into serving and must reject.
	m2, err := store.CreateModel(d, b2.ID, "u2", "mm2")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{
		{ModelID: m.ID, Weight: 1}, {ModelID: m2.ID, Weight: 1},
	})
	if !errors.Is(err, store.ErrExists) {
		t.Errorf("gaining first eligible member over public name: err = %v, want ErrExists", err)
	}
}

// The manual visibility editor (admin API, task 3): valid values apply, bad
// input is ErrInvalid, unknown ids ErrNotFound. A hand-set public on a member
// is allowed but the next membership write re-asserts the membership rule.
func TestUpdateModelVisibility(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "eng", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "vis", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.UpdateModelVisibility(d, m.ID, "internal"); err != nil || got.Visibility != "internal" {
		t.Fatalf("set internal: %v %+v", err, got)
	}
	if got, err := store.UpdateModelVisibility(d, m.ID, "public"); err != nil || got.Visibility != "public" {
		t.Fatalf("set public: %v %+v", err, got)
	}
	if _, err := store.UpdateModelVisibility(d, m.ID, "secret"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("bad visibility = %v, want ErrInvalid", err)
	}
	if _, err := store.UpdateModelVisibility(d, 9999, "public"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown model = %v, want ErrNotFound", err)
	}

	g, err := store.CreateGroup(d, "vis-pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModelVisibility(d, m.ID, "public"); err != nil {
		t.Fatalf("re-expose member: %v", err)
	}
	// The next membership write flips it back to internal.
	if _, err := store.UpdateGroup(d, g.ID, nil, nil, []store.GroupMemberInput{{ModelID: m.ID}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ModelByID(d, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != "internal" {
		t.Errorf("after membership rewrite visibility = %q, want internal", got.Visibility)
	}
}

func TestEnablePastReexposedMemberName(t *testing.T) {
	d := migratedDB(t)
	b1, err := store.CreateBackend(d, "node-a", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := store.CreateBackend(d, "node-b", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "gname")
	if err != nil {
		t.Fatal(err)
	}
	// Empty group reserves nothing, so the member can carry the name.
	m, err := store.CreateModel(d, b1.ID, "u", "gname")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, nil,
		[]store.GroupMemberInput{{ModelID: m.ID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// The admin visibility editor re-exposes the member (plan: "admin may
	// re-expose"). The row is the group's own, not an external claim.
	if _, err := store.UpdateModelVisibility(d, m.ID, "public"); err != nil {
		t.Fatal(err)
	}
	// Enabling must succeed: the member is exempt from the ownership check.
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); err != nil {
		t.Fatalf("enable over re-exposed own member: %v", err)
	}
	// The group wins the name while serving (shadow state).
	tgt, err := store.ResolveTarget(d, "gname")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Kind != store.TargetGroup {
		t.Errorf("kind = %v, want the group to win its own name", tgt.Kind)
	}
	// An EXTERNAL public same-name row still blocks the enable transition.
	if _, _, err := store.AddModels(d, b2.ID, []string{"gname"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(false), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptr(true), nil); !errors.Is(err, store.ErrExists) {
		t.Errorf("re-enable over external public row: err = %v, want ErrExists", err)
	}
}
