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
