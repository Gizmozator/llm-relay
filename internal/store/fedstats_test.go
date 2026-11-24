// Store-level tests for the federation metering aggregation (plan
// 2026-11-22-relay-federation, task 4): the double-entry columns on
// usage_log and the four rollups the stats endpoints serve.
package store_test

import (
	"database/sql"
	"testing"
	"time"

	"llm-relay/internal/store"
)

// fedUser creates a user and returns its id.
func fedUser(t *testing.T, d *sql.DB, name string) int64 {
	t.Helper()
	u, err := store.CreateUser(d, name, "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// insertFed writes one usage row with explicit attribution and counts.
func insertFed(t *testing.T, d *sql.DB, e *store.UsageEvent) {
	t.Helper()
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if e.Endpoint == "" {
		e.Endpoint = "chat"
	}
	if err := store.InsertUsage(d, e); err != nil {
		t.Fatal(err)
	}
}

func ago(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format(time.RFC3339)
}

func ptrInt64(v int64) *int64 { return &v }

// Plain rows keep the empty defaults (the migration's DEFAULT ” applies
// to every pre-federation row too); fed rows round-trip all three columns.
func TestInsertUsageFedColumns(t *testing.T) {
	d := migratedDB(t)
	uid := fedUser(t, d, "alice")
	insertFed(t, d, &store.UsageEvent{UserID: uid, ModelID: 7, Prompt: 1})
	peerID := int64(4)
	insertFed(t, d, &store.UsageEvent{UserID: uid, ModelID: 7, Prompt: 2,
		FedDirection: "served", FedPeerID: &peerID, FedUser: "bob"})

	var dir, user string
	var pid sql.NullInt64
	if err := d.QueryRow(`SELECT fed_direction, fed_peer_id, fed_user FROM usage_log
		ORDER BY id LIMIT 1`).Scan(&dir, &pid, &user); err != nil {
		t.Fatal(err)
	}
	if dir != "" || pid.Valid || user != "" {
		t.Errorf("plain row = %q/%v/%q, want empty defaults", dir, pid, user)
	}
	if err := d.QueryRow(`SELECT fed_direction, fed_peer_id, fed_user FROM usage_log
		ORDER BY id DESC LIMIT 1`).Scan(&dir, &pid, &user); err != nil {
		t.Fatal(err)
	}
	if dir != "served" || !pid.Valid || pid.Int64 != 4 || user != "bob" {
		t.Errorf("fed row = %q/%v/%q, want served/4/bob", dir, pid, user)
	}
}

func TestFedUserTotals(t *testing.T) {
	d := migratedDB(t)
	a := fedUser(t, d, "alice")
	b := fedUser(t, d, "bob")
	peerID := int64(1)
	// alice: served 2 requests (12p/6c) + consumed 1 (5p/2c); bob: served 1.
	// One plain row for alice must never appear. One old row (before the
	// window) must be excluded by `since`.
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "served", FedPeerID: &peerID,
		Prompt: 10, Completion: 5, CostTotal: 100})
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "served", FedPeerID: &peerID,
		Prompt: 2, Completion: 1, CostTotal: 20})
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "consumed", FedPeerID: &peerID,
		Prompt: 5, Completion: 2, CostTotal: 50})
	insertFed(t, d, &store.UsageEvent{UserID: b, FedDirection: "served", FedPeerID: &peerID,
		Prompt: 1, Completion: 1, CostTotal: 5})
	insertFed(t, d, &store.UsageEvent{UserID: a, Prompt: 99})
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "served",
		TS: ago(40 * 24 * time.Hour), Prompt: 1000})
	// Two deleted users (no rows in users): they share the '(deleted user)'
	// label but must stay DISTINCT entries keyed by user_id — one consumed,
	// one served, each with its own accumulated totals.
	insertFed(t, d, &store.UsageEvent{UserID: 998, FedDirection: "consumed", Prompt: 3})
	insertFed(t, d, &store.UsageEvent{UserID: 998, FedDirection: "consumed", Prompt: 2})
	insertFed(t, d, &store.UsageEvent{UserID: 997, FedDirection: "served", Prompt: 5})

	since := ago(30 * 24 * time.Hour)
	all, err := store.FedUserTotals(d, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("users = %d, want 4 (alice, bob, two distinct deleted): %+v", len(all), all)
	}
	byName := map[string]store.FedUserTotal{}
	byID := map[int64]store.FedUserTotal{}
	for _, e := range all {
		byName[e.User] = e
		byID[e.UserID] = e
	}
	alice := byName["alice"]
	if alice.UserID != a || alice.Served.Requests != 2 || alice.Served.Prompt != 12 ||
		alice.Served.Completion != 6 || alice.Served.CostMicro != 120 ||
		alice.Consumed.Requests != 1 || alice.Consumed.Prompt != 5 {
		t.Errorf("alice = %+v", alice)
	}
	// The two orphan ids are separate entries with the same label, and
	// each id's rows accumulated (998: two consumed rows = 5p).
	d1, d2 := byID[997], byID[998]
	if d1.User != "(deleted user)" || d1.Served.Prompt != 5 || d1.Consumed.Prompt != 0 {
		t.Errorf("deleted 997 = %+v, want served 5p only", d1)
	}
	if d2.User != "(deleted user)" || d2.Consumed.Prompt != 5 || d2.Consumed.Requests != 2 ||
		d2.Served.Prompt != 0 {
		t.Errorf("deleted 998 = %+v, want consumed 5p/2req only", d2)
	}
	one, err := store.FedUserTotals(d, since, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].User != "bob" || one[0].UserID != b || one[0].Served.Requests != 1 {
		t.Errorf("scoped = %+v, want only bob", one)
	}
}

func TestFedPeerTotals(t *testing.T) {
	d := migratedDB(t)
	a := fedUser(t, d, "alice")
	b := fedUser(t, d, "bob")
	p1, _, err := store.CreatePeer(d, "spoke-a", a)
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := store.CreatePeer(d, "spoke-b", a) // same owner: caveat case
	if err != nil {
		t.Fatal(err)
	}
	p3, _, err := store.CreatePeer(d, "spoke-c", b)
	if err != nil {
		t.Fatal(err)
	}
	p1id, p2id := p1.ID, p2.ID
	// served side: fed_peer_id = the HOST credited.
	insertFed(t, d, &store.UsageEvent{UserID: b, FedDirection: "served", FedPeerID: &p1id, Prompt: 7})
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "served", FedPeerID: &p2id, Prompt: 3})
	// A served row for a since-deleted peer aggregates under the
	// placeholder (999: no peers row, usage_log has no FK).
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "served",
		FedPeerID: ptrInt64(999), Prompt: 2})
	// consumed side is OWNER-level: every consumed row of alice lands in
	// BOTH her peers' buckets (the multi-peer caveat), whether the row
	// names a host peer or NULL (hub hardware served).
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "consumed", FedPeerID: &p1id, Prompt: 10})
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "consumed", Prompt: 5})
	insertFed(t, d, &store.UsageEvent{UserID: b, FedDirection: "consumed", Prompt: 4})
	// A served row with NULL peer (a spoke's own shape) must not appear.
	insertFed(t, d, &store.UsageEvent{UserID: a, FedDirection: "served", Prompt: 99})

	out, err := store.FedPeerTotals(d, ago(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("peers = %d, want 4: %+v", len(out), out)
	}
	byID := map[int64]store.FedPeerTotal{}
	for _, e := range out {
		byID[e.PeerID] = e
	}
	spokeA := byID[p1.ID]
	if spokeA.Peer != "spoke-a" || spokeA.Served.Prompt != 7 || spokeA.Consumed.Prompt != 15 {
		t.Errorf("spoke-a = %+v, want served 7 / consumed 15 (owner total)", spokeA)
	}
	spokeB := byID[p2.ID]
	if spokeB.Peer != "spoke-b" || spokeB.Served.Prompt != 3 || spokeB.Consumed.Prompt != 15 {
		t.Errorf("spoke-b = %+v, want served 3 / consumed 15 (same owner caveat)", spokeB)
	}
	spokeC := byID[p3.ID]
	if spokeC.Peer != "spoke-c" || spokeC.Served.Prompt != 0 || spokeC.Consumed.Prompt != 4 {
		t.Errorf("spoke-c = %+v, want served 0 / consumed 4", spokeC)
	}
	if del := byID[999]; del.Peer != "(deleted peer)" || del.Served.Prompt != 2 {
		t.Errorf("deleted peer = %+v", del)
	}
}

func TestFedServedBySource(t *testing.T) {
	d := migratedDB(t)
	insertFed(t, d, &store.UsageEvent{FedDirection: "served", FedUser: "hub-alice", Prompt: 10, CostTotal: 3})
	insertFed(t, d, &store.UsageEvent{FedDirection: "served", FedUser: "hub-alice", Prompt: 1})
	insertFed(t, d, &store.UsageEvent{FedDirection: "served", FedUser: "hub-bob", Prompt: 4})
	insertFed(t, d, &store.UsageEvent{FedDirection: "consumed", FedUser: "hub-alice", Prompt: 100})

	all, err := store.FedServedBySource(d, ago(30*24*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].FedUser != "hub-alice" || all[0].Counts.Requests != 2 ||
		all[0].Counts.Prompt != 11 || all[1].FedUser != "hub-bob" {
		t.Errorf("all = %+v", all)
	}
	one, err := store.FedServedBySource(d, ago(30*24*time.Hour), "hub-bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Counts.Prompt != 4 {
		t.Errorf("scoped = %+v", one)
	}
}

func TestFedConsumedSlice(t *testing.T) {
	d := migratedDB(t)
	a := fedUser(t, d, "alice")
	b := fedUser(t, d, "bob")
	mk := func(base, name string) int64 {
		be, err := store.CreateBackend(d, name, base, "")
		if err != nil {
			t.Fatal(err)
		}
		m, err := store.CreateModel(d, be.ID, name+"-up", name)
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	peerM := mk(store.PeerBackendURL(3), "peer-model")
	hubM := mk("hub://federation", "hub-model")
	localM := mk("http://localhost:8080", "local-model")
	insertFed(t, d, &store.UsageEvent{UserID: a, ModelID: peerM, Prompt: 10})
	insertFed(t, d, &store.UsageEvent{UserID: a, ModelID: hubM, Prompt: 5})
	insertFed(t, d, &store.UsageEvent{UserID: a, ModelID: localM, Prompt: 999})
	insertFed(t, d, &store.UsageEvent{UserID: b, ModelID: hubM, Prompt: 1})

	all, err := store.FedConsumedSlice(d, ago(30*24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].User != "alice" || all[0].UserID != a ||
		all[0].Counts.Requests != 2 ||
		all[0].Counts.Prompt != 15 || all[1].User != "bob" || all[1].UserID != b {
		t.Errorf("all = %+v (local-model must be excluded)", all)
	}
	one, err := store.FedConsumedSlice(d, ago(30*24*time.Hour), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Counts.Prompt != 1 {
		t.Errorf("scoped = %+v", one)
	}
}

func TestFedPeerModel(t *testing.T) {
	d := migratedDB(t)
	be, err := store.CreateBackend(d, "peer3", store.PeerBackendURL(3), "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, be.ID, "alpha-up", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.FedPeerModel(d, 3, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != m.ID {
		t.Fatalf("FedPeerModel = %+v, want the materialized row", got)
	}
	// Other peers and other names resolve to (nil, nil): the hub uses it
	// for transit attribution and a missing row is a skip, not an error.
	miss, err := store.FedPeerModel(d, 4, "alpha")
	if err != nil || miss != nil {
		t.Fatalf("other peer = %+v, %v", miss, err)
	}
	miss, err = store.FedPeerModel(d, 3, "beta")
	if err != nil || miss != nil {
		t.Fatalf("other name = %+v, %v", miss, err)
	}
}

// Ticket #12: a hub backend with an owner credits that user with the serving
// when its own hardware serves a spoke (the consumed row with fed_peer_id
// NULL). The spender's consumed credit is untouched — this is the hub mirror
// of the peer-owner served credit, not a re-attribution.
func TestFedUserTotalsHubOwner(t *testing.T) {
	d := migratedDB(t)
	hubOwner := fedUser(t, d, "hub-owner")
	spokeOwner := fedUser(t, d, "spoke-owner")
	be, err := store.CreateBackendWithOwner(d, "hub-gpu", "http://hub-gpu:8080", "", &hubOwner)
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, be.ID, "up", "hub-model")
	if err != nil {
		t.Fatal(err)
	}
	// A spoke user consumes the hub-local model: the hub meters the spender
	// (spoke-owner) as consumed with fed_peer_id NULL (hub hardware served).
	// Before #12 this row credited nobody with serving.
	insertFed(t, d, &store.UsageEvent{UserID: spokeOwner, ModelID: m.ID,
		FedDirection: "consumed", Prompt: 10, Completion: 5, CostTotal: 100})

	all, err := store.FedUserTotals(d, ago(30*24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]store.FedUserTotal{}
	for _, e := range all {
		byName[e.User] = e
	}
	ho := byName["hub-owner"]
	if ho.UserID != hubOwner || ho.Served.Requests != 1 || ho.Served.Prompt != 10 ||
		ho.Served.Completion != 5 || ho.Served.CostMicro != 100 || ho.Consumed.Requests != 0 {
		t.Errorf("hub-owner = %+v, want served 1req/10p/5c/100micro, consumed 0", ho)
	}
	so := byName["spoke-owner"]
	if so.UserID != spokeOwner || so.Consumed.Requests != 1 || so.Consumed.Prompt != 10 ||
		so.Served.Requests != 0 {
		t.Errorf("spoke-owner = %+v, want consumed 1req/10p, served 0", so)
	}
}

// A backend with no owner must leave the ledger byte-identical to before #12:
// the hub-hardware consumed row credits the spender only, and no phantom
// served entry is invented for the ownerless backend.
func TestFedUserTotalsNoOwnerUnchanged(t *testing.T) {
	d := migratedDB(t)
	spokeOwner := fedUser(t, d, "spoke-owner")
	be, err := store.CreateBackend(d, "hub-gpu", "http://hub-gpu:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, be.ID, "up", "hub-model")
	if err != nil {
		t.Fatal(err)
	}
	insertFed(t, d, &store.UsageEvent{UserID: spokeOwner, ModelID: m.ID,
		FedDirection: "consumed", Prompt: 10, Completion: 5, CostTotal: 100})

	all, err := store.FedUserTotals(d, ago(30*24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("entries = %d, want 1 (only the spender): %+v", len(all), all)
	}
	if all[0].User != "spoke-owner" || all[0].Consumed.Prompt != 10 || all[0].Served.Requests != 0 {
		t.Errorf("entry = %+v, want spoke-owner consumed 10p, served 0", all[0])
	}
}

// The hub-owner pass keys on fed_peer_id IS NULL (hub hardware served). A
// peer-hosted served row (fed_peer_id set) on a backend that happens to carry
// an owner must NOT be credited again by that pass — the owner is credited
// exactly once, through the normal served path. Guards against double-counting
// a row on both the peer and hub-owner sides.
func TestFedUserTotalsOwnerNotDoubleCounted(t *testing.T) {
	d := migratedDB(t)
	owner := fedUser(t, d, "owner")
	be, err := store.CreateBackendWithOwner(d, "b", "http://b:1", "", &owner)
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, be.ID, "up", "mm")
	if err != nil {
		t.Fatal(err)
	}
	peerID := int64(7)
	insertFed(t, d, &store.UsageEvent{UserID: owner, ModelID: m.ID,
		FedDirection: "served", FedPeerID: &peerID, Prompt: 4})

	all, err := store.FedUserTotals(d, ago(30*24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].User != "owner" || all[0].Served.Prompt != 4 ||
		all[0].Served.Requests != 1 {
		t.Fatalf("entries = %+v, want owner served 4p/1req exactly once", all)
	}
}

// Ticket #13: the ledger carries TOKEN usage, not just request counts and
// cost — every FedCounts field (prompt/completion/cached/cache_write) must
// roll up per user on BOTH the served and the consumed side.
func TestFedUserTotalsTokenTotals(t *testing.T) {
	d := migratedDB(t)
	alice := fedUser(t, d, "alice")
	peerID := int64(1)
	// Two consumed rows and two served rows, each token kind distinct so a
	// dropped or crossed aggregation cannot pass by coincidence.
	insertFed(t, d, &store.UsageEvent{UserID: alice, FedDirection: "consumed", FedPeerID: &peerID,
		Prompt: 100, Completion: 20, Cached: 40, CacheWrite: 5, CostTotal: 300})
	insertFed(t, d, &store.UsageEvent{UserID: alice, FedDirection: "consumed", FedPeerID: &peerID,
		Prompt: 50, Completion: 10, Cached: 20, CacheWrite: 5, CostTotal: 200})
	insertFed(t, d, &store.UsageEvent{UserID: alice, FedDirection: "served", FedPeerID: &peerID,
		Prompt: 30, Completion: 7, Cached: 10, CacheWrite: 2, CostTotal: 90})
	insertFed(t, d, &store.UsageEvent{UserID: alice, FedDirection: "served", FedPeerID: &peerID,
		Prompt: 20, Completion: 3, Cached: 5, CacheWrite: 1, CostTotal: 60})

	all, err := store.FedUserTotals(d, ago(30*24*time.Hour), alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(all), all)
	}
	got := all[0]
	want := store.FedCounts{Requests: 2, Prompt: 150, Completion: 30, Cached: 60, CacheWrite: 10, CostMicro: 500}
	if got.Consumed != want {
		t.Errorf("consumed = %+v, want %+v", got.Consumed, want)
	}
	want = store.FedCounts{Requests: 2, Prompt: 50, Completion: 10, Cached: 15, CacheWrite: 3, CostMicro: 150}
	if got.Served != want {
		t.Errorf("served = %+v, want %+v", got.Served, want)
	}
}

// Ticket #13: the "no federation activity" state. An account whose window
// usage is entirely hub-local has no ledger row (by design) — the local-only
// rollup names it, so the UI can explain the gap instead of showing a blank.
// A user with any fed row is NOT listed (they are in the ledger already),
// and a user with no usage at all is absent either way (that is "no data").
func TestFedLocalOnlyUsers(t *testing.T) {
	d := migratedDB(t)
	local := fedUser(t, d, "local-only")
	mixed := fedUser(t, d, "mixed")
	peerID := int64(1)
	// local-only: two hub-local rows (15p/5c) and nothing federated.
	insertFed(t, d, &store.UsageEvent{UserID: local, Prompt: 10, Completion: 3, CostTotal: 50})
	insertFed(t, d, &store.UsageEvent{UserID: local, Prompt: 5, Completion: 2, CostTotal: 25})
	// mixed: local usage AND a fed row — already visible in the ledger.
	insertFed(t, d, &store.UsageEvent{UserID: mixed, Prompt: 8})
	insertFed(t, d, &store.UsageEvent{UserID: mixed, FedDirection: "consumed",
		FedPeerID: &peerID, Prompt: 4})
	// "quiet" is created but gets no rows at all: no data, not no-activity.
	fedUser(t, d, "quiet")
	// An old local row (outside the window) must not resurrect an entry.
	insertFed(t, d, &store.UsageEvent{UserID: fedUser(t, d, "stale"),
		TS: ago(40 * 24 * time.Hour), Prompt: 9})

	since := ago(30 * 24 * time.Hour)
	got, err := store.FedLocalOnlyUsers(d, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1 (only local-only; mixed has a fed row, quiet has no usage, stale is out of window): %+v", len(got), got)
	}
	byName := map[string]store.FedLocalOnlyEntry{}
	for _, e := range got {
		byName[e.User] = e
	}
	lo := byName["local-only"]
	if lo.UserID != local || lo.Counts.Requests != 2 || lo.Counts.Prompt != 15 ||
		lo.Counts.Completion != 5 || lo.Counts.CostMicro != 75 {
		t.Errorf("local-only = %+v, want 2req/15p/5c/75micro", lo)
	}
	if _, ok := byName["mixed"]; ok {
		t.Errorf("mixed user with a fed row must not be listed: %+v", got)
	}
	if _, ok := byName["quiet"]; ok {
		t.Errorf("user with no usage at all must not be listed: %+v", got)
	}
	if _, ok := byName["stale"]; ok {
		t.Errorf("out-of-window local row must not count: %+v", got)
	}

	// Scoped: the local-only user's own query finds them; the mixed user's
	// finds nobody (their local row does not make them ledger-invisible).
	one, err := store.FedLocalOnlyUsers(d, since, local)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].User != "local-only" {
		t.Errorf("scoped local-only = %+v, want the one entry", one)
	}
	one, err = store.FedLocalOnlyUsers(d, since, mixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 0 {
		t.Errorf("scoped mixed = %+v, want empty", one)
	}

	// The ledger itself stays honest: the local-only user has NO phantom
	// zero row in FedUserTotals — the no-federation state is absence from
	// the ledger plus presence in this list, never a fake entry.
	totals, err := store.FedUserTotals(d, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range totals {
		if e.UserID == local {
			t.Errorf("local-only user leaked a ledger row: %+v", e)
		}
	}
}
