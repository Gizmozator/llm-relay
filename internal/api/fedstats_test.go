// The federation stats pair (plan 2026-11-22-relay-federation, task 4):
// role-dependent admin payload, caller-scoped personal payload, and the
// days validation. The rows themselves are seeded through the store — the
// stamping paths are proven end-to-end in internal/fed.
package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"llm-relay/internal/store"
)

type fedCountsJSON struct {
	Requests   int64 `json:"requests"`
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

type fedUserJSON struct {
	UserID   int64         `json:"user_id"`
	User     string        `json:"user"`
	Served   fedCountsJSON `json:"served"`
	Consumed fedCountsJSON `json:"consumed"`
}

type fedLocalJSON struct {
	UserID int64         `json:"user_id"`
	User   string        `json:"user"`
	Counts fedCountsJSON `json:"counts"`
}

func getJSON(t *testing.T, e *testEnv, path, tok string, out any) int {
	t.Helper()
	resp := e.do(t, "GET", path, "", tok)
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

func insertFedRow(t *testing.T, e *testEnv, ev *store.UsageEvent) {
	t.Helper()
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339)
	}
	ev.Endpoint = "chat"
	if err := store.InsertUsage(e.db, ev); err != nil {
		t.Fatal(err)
	}
}

func TestFedStatsEndpoints(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	alice := userToken(t, e, "alice")

	// Standalone: the endpoints answer, role-only payload.
	var out map[string]any
	if code := getJSON(t, e, "/api/admin/federation/stats", tok, &out); code != 200 {
		t.Fatalf("admin stats standalone = %d", code)
	}
	if out["role"] != "standalone" || out["users"] != nil {
		t.Errorf("standalone payload = %v", out)
	}
	// days validation happens before the role switch.
	if code := getJSON(t, e, "/api/admin/federation/stats?days=0", tok, nil); code != http.StatusBadRequest {
		t.Errorf("days=0 = %d, want 400", code)
	}
	if code := getJSON(t, e, "/api/federation/stats?days=abc", alice, nil); code != http.StatusBadRequest {
		t.Errorf("days=abc = %d, want 400", code)
	}
	// month=YYYY-MM is the calendar-window alternative; junk is rejected
	// before the role switch, a valid month is accepted.
	if code := getJSON(t, e, "/api/admin/federation/stats?month=bogus", tok, nil); code != http.StatusBadRequest {
		t.Errorf("month=bogus = %d, want 400", code)
	}
	if code := getJSON(t, e, "/api/admin/federation/stats?month=2026-13", tok, nil); code != http.StatusBadRequest {
		t.Errorf("month=2026-13 = %d, want 400", code)
	}
	if code := getJSON(t, e, "/api/federation/stats?month=2026-11", alice, nil); code != http.StatusOK {
		t.Errorf("month=2026-11 = %d, want 200", code)
	}
	// A plain user cannot reach the admin view.
	if code := getJSON(t, e, "/api/admin/federation/stats", alice, nil); code != http.StatusForbidden {
		t.Errorf("user admin stats = %d, want 403", code)
	}

	// ---- hub role ----
	if resp := e.do(t, "PUT", "/api/admin/federation", `{"role":"hub"}`, tok); resp.StatusCode != 200 {
		t.Fatalf("hub PUT: %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	var adminID int64
	if err := e.db.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	var aliceID int64
	if err := e.db.QueryRow(`SELECT id FROM users WHERE username='alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	peer, _, err := store.CreatePeer(e.db, "spoke-a", adminID)
	if err != nil {
		t.Fatal(err)
	}
	pid := peer.ID
	// admin consumed 10p from the peer; alice (the peer's owner here is
	// admin, but the ledger keys on rows) served 7p to someone.
	insertFedRow(t, e, &store.UsageEvent{UserID: adminID, FedDirection: "consumed",
		FedPeerID: &pid, Prompt: 10, Completion: 5, CostTotal: 42})
	insertFedRow(t, e, &store.UsageEvent{UserID: adminID, FedDirection: "served",
		FedPeerID: &pid, FedUser: "remote", Prompt: 7})
	// Ticket #13: alice has hub-local usage only — invisible in the ledger
	// by design, so the payload must name her in local_only (the UI's
	// "no federation activity" signal, distinct from "no data at all").
	insertFedRow(t, e, &store.UsageEvent{UserID: aliceID, Prompt: 6, Completion: 2, CostTotal: 30})

	var hub struct {
		Role  string        `json:"role"`
		Users []fedUserJSON `json:"users"`
		Peers []struct {
			PeerID int64  `json:"peer_id"`
			Peer   string `json:"peer"`
		} `json:"peers"`
		LocalOnly []fedLocalJSON `json:"local_only"`
	}
	if code := getJSON(t, e, "/api/admin/federation/stats", tok, &hub); code != 200 {
		t.Fatalf("admin stats hub = %d", code)
	}
	if hub.Role != "hub" || len(hub.Users) != 1 || hub.Users[0].User != "admin" ||
		hub.Users[0].UserID != adminID ||
		hub.Users[0].Consumed.Prompt != 10 || hub.Users[0].Served.Prompt != 7 ||
		len(hub.Peers) != 1 || hub.Peers[0].Peer != "spoke-a" {
		t.Errorf("hub payload = %+v", hub)
	}
	// local_only: exactly alice (admin has federation rows and must not be
	// listed), carrying her local token totals.
	if len(hub.LocalOnly) != 1 || hub.LocalOnly[0].User != "alice" ||
		hub.LocalOnly[0].UserID != aliceID || hub.LocalOnly[0].Counts.Prompt != 6 ||
		hub.LocalOnly[0].Counts.Completion != 2 {
		t.Errorf("hub local_only = %+v, want only alice 6p/2c", hub.LocalOnly)
	}
	// Personal view: only the caller's entry — alice has no federation rows,
	// and her local usage must not produce a phantom ledger row; her own
	// payload carries the local_only entry instead (#13).
	var mine struct {
		Role      string         `json:"role"`
		Totals    []fedUserJSON  `json:"totals"`
		LocalOnly []fedLocalJSON `json:"local_only"`
		Currency  string         `json:"currency"`
	}
	if code := getJSON(t, e, "/api/federation/stats", alice, &mine); code != 200 {
		t.Fatalf("alice stats = %d", code)
	}
	if mine.Role != "hub" || len(mine.Totals) != 0 {
		t.Errorf("alice totals = %+v, want empty", mine)
	}
	if mine.Currency == "" {
		t.Errorf("personal payload must carry the display currency (personal page): %+v", mine)
	}
	if len(mine.LocalOnly) != 1 || mine.LocalOnly[0].User != "alice" ||
		mine.LocalOnly[0].Counts.Requests != 1 {
		t.Errorf("alice local_only = %+v, want her one local row", mine.LocalOnly)
	}
	if code := getJSON(t, e, "/api/federation/stats", tok, &mine); code != 200 {
		t.Fatalf("admin stats personal = %d", code)
	}
	if len(mine.Totals) != 1 || mine.Totals[0].User != "admin" ||
		mine.Totals[0].Consumed.Requests != 1 {
		t.Errorf("admin totals = %+v", mine)
	}
	if len(mine.LocalOnly) != 0 {
		t.Errorf("admin local_only = %+v, want empty (admin has fed rows)", mine.LocalOnly)
	}

	// ---- spoke role ----
	if resp := e.do(t, "PUT", "/api/admin/federation",
		`{"role":"spoke","hub_url":"https://hub.example.fi","hub_token":"fed-abc"}`, tok); resp.StatusCode != 200 {
		t.Fatalf("spoke PUT: %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	// Seed the spoke shape: a served row (hub-stamped source) and a local
	// user consuming a federated (peer://) model — plain row, scheme-
	// identified slice.
	insertFedRow(t, e, &store.UsageEvent{UserID: 0, FedDirection: "served",
		FedUser: "hub-alice", Prompt: 12, CostTotal: 9})
	be, err := store.CreateBackend(e.db, "peer3", store.PeerBackendURL(3), "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(e.db, be.ID, "alpha-up", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	insertFedRow(t, e, &store.UsageEvent{UserID: aliceID, ModelID: m.ID, Prompt: 4})

	var spoke struct {
		Role           string `json:"role"`
		ServedBySource []struct {
			FedUser string        `json:"fed_user"`
			Counts  fedCountsJSON `json:"counts"`
		} `json:"served_by_source"`
		Consumed []struct {
			UserID int64         `json:"user_id"`
			User   string        `json:"user"`
			Counts fedCountsJSON `json:"counts"`
		} `json:"consumed"`
		Users []struct {
			UserID   int64         `json:"user_id"`
			User     string        `json:"user"`
			Served   fedCountsJSON `json:"served"`
			Consumed fedCountsJSON `json:"consumed"`
		} `json:"users"`
		LocalOnly []struct {
			UserID int64 `json:"user_id"`
		} `json:"local_only"`
	}
	if code := getJSON(t, e, "/api/admin/federation/stats", tok, &spoke); code != 200 {
		t.Fatalf("admin stats spoke = %d", code)
	}
	// (the "remote" served row from the hub-phase seeding is still in the
	// same DB — the query is over all served rows, so look up by source.)
	bySource := map[string]int64{}
	for _, s := range spoke.ServedBySource {
		bySource[s.FedUser] = s.Counts.Prompt
	}
	if spoke.Role != "spoke" || bySource["hub-alice"] != 12 ||
		len(spoke.Consumed) != 1 || spoke.Consumed[0].User != "alice" ||
		spoke.Consumed[0].UserID != aliceID ||
		spoke.Consumed[0].Counts.Prompt != 4 {
		t.Errorf("spoke payload = %+v", spoke)
	}
	// The spoke's own fairness ledger (plan 2026-11-25-spoke-fairness):
	// alice consumed the hub-hosted model (4p) and the Federation virtual
	// account carries both sides of the box-level traffic — served to
	// alice (4p), consumed from this box (the 12p served row).
	fedRow, aliceRow := 0, 0
	for _, u := range spoke.Users {
		switch u.UserID {
		case -1:
			fedRow++
			if u.User != "Federation" || u.Served.Prompt != 4 || u.Consumed.Prompt != 12 {
				t.Errorf("Federation row = %+v; want served 4, consumed 12", u)
			}
		case aliceID:
			aliceRow++
			if u.Consumed.Prompt != 4 || u.Served.Prompt != 0 {
				t.Errorf("alice fairness row = %+v", u)
			}
		}
	}
	if fedRow != 1 || aliceRow != 1 {
		t.Errorf("spoke users = %+v; want alice + Federation", spoke.Users)
	}
	for _, lo := range spoke.LocalOnly {
		if lo.UserID == aliceID {
			t.Errorf("alice is in the ledger and must be dropped from local_only")
		}
	}
	// Personal spoke view: the caller's consumed slice plus their own
	// fairness entry (never the box-level Federation row).
	var mineSpoke struct {
		Role     string `json:"role"`
		Currency string `json:"currency"`
		Consumed []struct {
			User   string        `json:"user"`
			Counts fedCountsJSON `json:"counts"`
		} `json:"consumed"`
		Totals []struct {
			UserID   int64         `json:"user_id"`
			User     string        `json:"user"`
			Consumed fedCountsJSON `json:"consumed"`
		} `json:"totals"`
	}
	if code := getJSON(t, e, "/api/federation/stats", alice, &mineSpoke); code != 200 {
		t.Fatalf("alice spoke stats = %d", code)
	}
	if mineSpoke.Role != "spoke" || len(mineSpoke.Consumed) != 1 ||
		mineSpoke.Consumed[0].User != "alice" {
		t.Errorf("alice spoke slice = %+v", mineSpoke)
	}
	if mineSpoke.Currency == "" {
		t.Errorf("spoke personal payload must carry the display currency: %+v", mineSpoke)
	}
	// Alice's personal fairness entry: her hub-model spending (4p consumed),
	// and never the box-level Federation row.
	if len(mineSpoke.Totals) != 1 || mineSpoke.Totals[0].User != "alice" ||
		mineSpoke.Totals[0].Consumed.Prompt != 4 {
		t.Errorf("alice spoke totals = %+v; want her own entry (consumed 4p)", mineSpoke.Totals)
	}
	if code := getJSON(t, e, "/api/federation/stats", tok, &mineSpoke); code != 200 {
		t.Fatalf("admin spoke stats = %d", code)
	}
	if len(mineSpoke.Consumed) != 0 {
		t.Errorf("admin spoke slice = %+v, want empty", mineSpoke)
	}
	for _, t2 := range mineSpoke.Totals {
		if t2.UserID == -1 {
			t.Errorf("Federation row leaked into a personal view: %+v", t2)
		}
	}
}

// Plan 2026-11-25-fairness-ledger through the endpoints: alice spending on
// a backend owned by admin is double-entered (admin served / alice
// consumed) on the hub ledger, alice drops out of local_only, and both
// personal views agree. The calendar-month window must carry the same row.
func TestFedStatsLocalCrossUserCredit(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	alice := userToken(t, e, "alice")
	if resp := e.do(t, "PUT", "/api/admin/federation", `{"role":"hub"}`, tok); resp.StatusCode != 200 {
		t.Fatalf("hub PUT: %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	var adminID, aliceID int64
	if err := e.db.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(`SELECT id FROM users WHERE username='alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	be, err := store.CreateBackendWithOwner(e.db, "gpu", "http://gpu:8080", "", &adminID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(e.db, be.ID, "up", "hosted")
	if err != nil {
		t.Fatal(err)
	}
	// One clock read: the row's TS and the month window derive from the
	// same instant, so a UTC month rollover between the insert and the
	// month query cannot move the row out of the month it was stamped in.
	now := time.Now().UTC()
	insertFedRow(t, e, &store.UsageEvent{TS: now.Format(time.RFC3339), UserID: aliceID, ModelID: m.ID,
		Prompt: 8, Completion: 3, CostTotal: 60})

	var hub struct {
		Users     []fedUserJSON  `json:"users"`
		LocalOnly []fedLocalJSON `json:"local_only"`
	}
	if code := getJSON(t, e, "/api/admin/federation/stats", tok, &hub); code != 200 {
		t.Fatalf("admin stats = %d", code)
	}
	byName := map[string]fedUserJSON{}
	for _, u := range hub.Users {
		byName[u.User] = u
	}
	if len(hub.Users) != 2 || byName["admin"].Served.Prompt != 8 ||
		byName["admin"].Consumed.Prompt != 0 || byName["alice"].Consumed.Prompt != 8 ||
		byName["alice"].Consumed.Completion != 3 || byName["alice"].Served.Prompt != 0 {
		t.Errorf("hub ledger = %+v, want admin served 8p / alice consumed 8p/3c", hub.Users)
	}
	if len(hub.LocalOnly) != 0 {
		t.Errorf("local_only = %+v, want empty (both accounts are in the ledger now)", hub.LocalOnly)
	}

	// Personal views: each side sees only its own entry.
	var mine struct {
		Totals    []fedUserJSON  `json:"totals"`
		LocalOnly []fedLocalJSON `json:"local_only"`
	}
	if code := getJSON(t, e, "/api/federation/stats", alice, &mine); code != 200 {
		t.Fatalf("alice stats = %d", code)
	}
	if len(mine.Totals) != 1 || mine.Totals[0].User != "alice" ||
		mine.Totals[0].Consumed.Prompt != 8 || len(mine.LocalOnly) != 0 {
		t.Errorf("alice totals = %+v / %+v", mine.Totals, mine.LocalOnly)
	}
	if code := getJSON(t, e, "/api/federation/stats", tok, &mine); code != 200 {
		t.Fatalf("admin stats personal = %d", code)
	}
	if len(mine.Totals) != 1 || mine.Totals[0].User != "admin" ||
		mine.Totals[0].Served.Prompt != 8 {
		t.Errorf("admin totals = %+v", mine.Totals)
	}

	// The calendar-month window (this UTC month) carries the same row —
	// the row was stamped now, and the month covers [1st, 1st of next).
	var monthHub struct {
		Users []fedUserJSON `json:"users"`
		Until string        `json:"until"`
	}
	month := now.Format("2006-01")
	if code := getJSON(t, e, "/api/admin/federation/stats?month="+month, tok, &monthHub); code != 200 {
		t.Fatalf("month stats = %d", code)
	}
	if len(monthHub.Users) != 2 || monthHub.Until == "" {
		t.Errorf("month ledger = %+v until=%q, want the same pair and a closed window",
			monthHub.Users, monthHub.Until)
	}
	// A month far in the past: closed window, nothing in it.
	var emptyHub struct {
		Users []fedUserJSON `json:"users"`
	}
	if code := getJSON(t, e, "/api/admin/federation/stats?month=2020-01", tok, &emptyHub); code != 200 {
		t.Fatalf("past month stats = %d", code)
	}
	if len(emptyHub.Users) != 0 {
		t.Errorf("2020-01 ledger = %+v, want empty", emptyHub.Users)
	}
}
