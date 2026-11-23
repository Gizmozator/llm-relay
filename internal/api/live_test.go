// Live dashboard endpoint tests: minute buckets, scoping, window clamping,
// in-flight counts and model online status.
package api_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/api"
	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// fakeFlight serves per-user in-flight counts like proxy.InFlight.Snapshot:
// userID 0 aggregates all users, anything else filters to that caller.
type fakeFlight struct {
	byUser  map[int64]map[string]int
	since   map[int64]map[string][]time.Time
	streams map[int64][]proxy.Stream
	recent  map[int64][]proxy.Stream
}

func (f fakeFlight) Snapshot(userID int64) map[string]int {
	out := map[string]int{}
	for uid, counts := range f.byUser {
		if userID != 0 && uid != userID {
			continue
		}
		for m, n := range counts {
			out[m] += n
		}
	}
	return out
}

// SnapshotByModel derives id-keyed counts from the fake's stream records,
// mirroring proxy.InFlight: the streams list is the source of truth for
// per-member (ModelID) counts.
func (f fakeFlight) SnapshotByModel(userID int64) map[int64]int {
	out := map[int64]int{}
	for uid, streams := range f.streams {
		if userID != 0 && uid != userID {
			continue
		}
		for _, s := range streams {
			out[s.ModelID]++
		}
	}
	return out
}

func (f fakeFlight) BusySince(userID int64) map[string][]time.Time {
	out := map[string][]time.Time{}
	for uid, perModel := range f.since {
		if userID != 0 && uid != userID {
			continue
		}
		for m, ts := range perModel {
			out[m] = append(out[m], ts...)
		}
	}
	return out
}

func (f fakeFlight) List(userID int64) []proxy.Stream {
	out := []proxy.Stream{}
	for uid, streams := range f.streams {
		if userID != 0 && uid != userID {
			continue
		}
		out = append(out, streams...)
	}
	return out
}

func (f fakeFlight) Recent(userID int64) []proxy.Stream {
	out := []proxy.Stream{}
	for uid, streams := range f.recent {
		if userID != 0 && uid != userID {
			continue
		}
		out = append(out, streams...)
	}
	return out
}

type liveEnv struct {
	db  *sql.DB
	srv *httptest.Server
}

func newLiveEnv(t *testing.T, flight api.FlightSource) *liveEnv {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	d, err := db.Open(t.TempDir() + "/live.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	ah, _ := auth.HashPassword("adminpass1")
	if _, err := store.CreateUser(d, "admin", "", ah, "admin"); err != nil {
		t.Fatal(err)
	}
	uh, _ := auth.HashPassword("userpass1")
	if _, err := store.CreateUser(d, "friend", "", uh, "user"); err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateBackend(d, "eng", up.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b.ID, "up-id", "test-model"); err != nil {
		t.Fatal(err)
	}

	s := &api.Server{DB: d, JWTSecret: "testsecret", JWTTTL: time.Hour, Flight: flight}
	mux := http.NewServeMux()
	mux.Handle("POST /api/auth/login", s.RateLimitLogin(s.Login))
	mux.Handle("GET /api/live", s.Auth(s.Live))
	mux.Handle("GET /api/admin/live", s.Admin(s.Live))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &liveEnv{db: d, srv: srv}
}

func liveLogin(t *testing.T, e *liveEnv, username, password string) string {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	resp, err := http.Post(e.srv.URL+"/api/auth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login %s: status %d", username, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Token == "" {
		t.Fatal("no token")
	}
	return out.Token
}

func liveGet(t *testing.T, e *liveEnv, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type liveResp struct {
	Buckets []store.LiveBucket `json:"buckets"`
	Active  []struct {
		Model string `json:"model"`
		Count int    `json:"count"`
	} `json:"active"`
	Models []api.LiveModel `json:"models"`
	Util   []struct {
		Model            string    `json:"model"`
		Values           []float64 `json:"values"`
		ConcurrencyLimit int64     `json:"concurrency_limit"`
	} `json:"util"`
}

func (r liveResp) sum() (requests int64) {
	for _, b := range r.Buckets {
		requests += b.Requests
	}
	return
}

func userID(t *testing.T, d *sql.DB, name string) int64 {
	t.Helper()
	u, _, err := store.UserByName(d, name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func seedUsage(t *testing.T, d *sql.DB, userID int64, ts time.Time, prompt, completion, cost int64) {
	t.Helper()
	err := store.InsertUsage(d, &store.UsageEvent{
		TS: ts.UTC().Format(time.RFC3339), UserID: userID,
		Endpoint: "chat", ModelRequ: "test-model",
		Prompt: prompt, Completion: completion, CostTotal: cost, Status: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLiveServerWideBuckets(t *testing.T) {
	e := newLiveEnv(t, nil)
	admID := userID(t, e.db, "admin")
	friID := userID(t, e.db, "friend")
	now := time.Now().UTC()
	seedUsage(t, e.db, admID, now.Add(-5*time.Minute), 100, 50, 1000)
	seedUsage(t, e.db, friID, now.Add(-2*time.Minute), 10, 5, 2000)
	seedUsage(t, e.db, admID, now.Add(-2*time.Hour), 999, 999, 999) // outside window

	admTok := liveLogin(t, e, "admin", "adminpass1")
	friTok := liveLogin(t, e, "friend", "userpass1")

	// Admin route: all users, full window, zero-filled.
	resp := liveGet(t, e, "/api/admin/live?window=60", admTok)
	if resp.StatusCode != 200 {
		t.Fatalf("admin live status %d", resp.StatusCode)
	}
	var all liveResp
	json.NewDecoder(resp.Body).Decode(&all)
	resp.Body.Close()
	if len(all.Buckets) != 60 {
		t.Fatalf("buckets = %d, want 60", len(all.Buckets))
	}
	reqs := all.sum()
	if reqs != 2 {
		t.Errorf("admin totals = %d reqs, want 2", reqs)
	}

	// Personal routes are server-wide too: the live view answers "is the
	// server free" for every user (owner decision), so both callers see
	// all traffic.
	resp = liveGet(t, e, "/api/live", admTok)
	var own liveResp
	json.NewDecoder(resp.Body).Decode(&own)
	resp.Body.Close()
	if reqs := own.sum(); reqs != 2 {
		t.Errorf("admin personal totals = %d reqs, want 2", reqs)
	}

	// Personal route as user: also all users' rows.
	resp = liveGet(t, e, "/api/live", friTok)
	var fri liveResp
	json.NewDecoder(resp.Body).Decode(&fri)
	resp.Body.Close()
	if reqs := fri.sum(); reqs != 2 {
		t.Errorf("friend totals = %d reqs, want 2", reqs)
	}
	// user_id is ignored on the personal route (admin-only drill-down).
	resp = liveGet(t, e, fmt.Sprintf("/api/live?user_id=%d", admID), friTok)
	var spoof liveResp
	json.NewDecoder(resp.Body).Decode(&spoof)
	resp.Body.Close()
	if reqs := spoof.sum(); reqs != 2 {
		t.Errorf("personal route with user_id = %d reqs, want 2 (filter ignored)", reqs)
	}

	// user_id filter on the admin route narrows to one user.
	resp = liveGet(t, e, fmt.Sprintf("/api/admin/live?user_id=%d", friID), admTok)
	var filtered liveResp
	json.NewDecoder(resp.Body).Decode(&filtered)
	resp.Body.Close()
	if reqs := filtered.sum(); reqs != 1 {
		t.Errorf("user_id filter: %d reqs, want 1", reqs)
	}

	// Non-admin may not use the admin route.
	if resp := liveGet(t, e, "/api/admin/live", friTok); resp.StatusCode != 403 {
		t.Errorf("friend admin live status = %d, want 403", resp.StatusCode)
		resp.Body.Close()
	}
}

func TestLiveWindowClamping(t *testing.T) {
	e := newLiveEnv(t, nil)
	tok := liveLogin(t, e, "admin", "adminpass1")

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"window=100000", 360},
		{"window=1", 5},
		{"window=10", 10},
	} {
		resp := liveGet(t, e, "/api/live?"+tc.query, tok)
		if resp.StatusCode != 200 {
			t.Fatalf("%s status %d", tc.query, resp.StatusCode)
		}
		var r liveResp
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if len(r.Buckets) != tc.want {
			t.Errorf("%s → %d buckets, want %d", tc.query, len(r.Buckets), tc.want)
		}
	}
	if resp := liveGet(t, e, "/api/live?window=x", tok); resp.StatusCode != 400 {
		t.Errorf("bad window status = %d, want 400", resp.StatusCode)
		resp.Body.Close()
	}
}

func TestLiveUtilization(t *testing.T) {
	e := newLiveEnv(t, nil)
	admID := userID(t, e.db, "admin")
	models, err := store.ListModels(e.db)
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %+v, err = %v", models, err)
	}
	four := int64(4)
	if err := store.UpdateModelMeta(e.db, models[0].ID, nil, &four, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// A 90 s request completed one minute ago: total busy = 90 s = 150 %
	// of a minute, split across the minutes it actually occupied.
	err = store.InsertUsage(e.db, &store.UsageEvent{
		TS: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), UserID: admID,
		Endpoint: "chat", ModelRequ: "test-model", LatencyMS: 90_000, Status: 200,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok := liveLogin(t, e, "admin", "adminpass1")
	resp := liveGet(t, e, "/api/live", tok)
	var r liveResp
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()

	if len(r.Util) != 1 {
		t.Fatalf("util = %+v, want one model", r.Util)
	}
	u := r.Util[0]
	if u.Model != "test-model" || u.ConcurrencyLimit != 4 {
		t.Errorf("util row = %s limit=%d, want test-model limit=4", u.Model, u.ConcurrencyLimit)
	}
	if len(u.Values) != 60 {
		t.Fatalf("util values = %d, want 60 (window-aligned)", len(u.Values))
	}
	var total float64
	for _, v := range u.Values {
		total += v
	}
	if diff := total - 150.0; diff > 0.5 || diff < -0.5 {
		t.Errorf("total busy = %.1f%%, want ~150%%", total)
	}
}

// A stream running for 90 s with no usage row yet (rows are written at
// completion) must already count as busy — that is the dip-then-peak fix:
// total busy = 90 s = 150 % of a minute across the buckets it covers, and
// the bucket before the current one is always fully covered (≥30 s).
func TestLiveUtilizationInFlight(t *testing.T) {
	fl := fakeFlight{
		byUser: map[int64]map[string]int{1: {"test-model": 1}},
		since:  map[int64]map[string][]time.Time{},
	}
	e := newLiveEnv(t, fl)
	tok := liveLogin(t, e, "friend", "userpass1")
	// Pin the start right before the request: the server folds busy time
	// up to ITS now, so any setup time (bcrypt, HTTP) would inflate it.
	fl.since[1] = map[string][]time.Time{"test-model": {time.Now().Add(-90 * time.Second)}}
	resp := liveGet(t, e, "/api/live", tok)
	var r liveResp
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()

	if len(r.Util) != 1 {
		t.Fatalf("util = %+v, want one model (in-flight only, no usage rows)", r.Util)
	}
	u := r.Util[0]
	if len(u.Values) != 60 {
		t.Fatalf("util values = %d, want 60", len(u.Values))
	}
	var total float64
	for _, v := range u.Values {
		total += v
	}
	if diff := total - 150.0; diff > 0.5 || diff < -0.5 {
		t.Errorf("total busy = %.1f%%, want ~150%% from the running stream", total)
	}
	if u.Values[58] < 49.9 {
		t.Errorf("bucket -2 = %.1f%%, want ≥50%% (always ≥30 s covered)", u.Values[58])
	}
}

func TestLiveActiveServerWideAndModels(t *testing.T) {
	flights := map[int64]map[string]int{}
	streams := map[int64][]proxy.Stream{}
	e := newLiveEnv(t, fakeFlight{byUser: flights, streams: streams})
	admID := userID(t, e.db, "admin")
	friID := userID(t, e.db, "friend")
	models, err := store.ListModels(e.db)
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %+v, err = %v", models, err)
	}
	mid := models[0].ID
	flights[admID] = map[string]int{"test-model": 2}
	flights[friID] = map[string]int{"test-model": 5}
	// The id-keyed view (per-member row counts) reads the stream records.
	for i := 0; i < 2; i++ {
		streams[admID] = append(streams[admID], proxy.Stream{UserID: admID, ModelID: mid, Model: "test-model"})
	}
	for i := 0; i < 5; i++ {
		streams[friID] = append(streams[friID], proxy.Stream{UserID: friID, ModelID: mid, Model: "test-model"})
	}

	admTok := liveLogin(t, e, "admin", "adminpass1")
	friTok := liveLogin(t, e, "friend", "userpass1")

	// Personal route: server-wide streams (a user must see that a model
	// is saturated even when someone else is using it), model row too.
	resp := liveGet(t, e, "/api/live", admTok)
	var r liveResp
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if len(r.Active) != 1 || r.Active[0].Model != "test-model" || r.Active[0].Count != 7 {
		t.Errorf("admin personal active = %+v, want [{test-model 7}]", r.Active)
	}
	if len(r.Models) != 1 {
		t.Fatalf("models = %+v, want one entry", r.Models)
	}
	m := r.Models[0]
	if m.Name != "test-model" || m.Backend != "eng" || !m.Online || m.Active != 7 {
		t.Errorf("model = %+v, want test-model/eng online active=7", m)
	}

	// Personal route as friend: same server-wide counts.
	resp = liveGet(t, e, "/api/live", friTok)
	var fri liveResp
	json.NewDecoder(resp.Body).Decode(&fri)
	resp.Body.Close()
	if len(fri.Active) != 1 || fri.Active[0].Count != 7 {
		t.Errorf("friend active = %+v, want [{test-model 7}]", fri.Active)
	}

	// Admin route: same global view; user_id filter narrows active too.
	resp = liveGet(t, e, "/api/admin/live", admTok)
	var all liveResp
	json.NewDecoder(resp.Body).Decode(&all)
	resp.Body.Close()
	if len(all.Active) != 1 || all.Active[0].Count != 7 {
		t.Errorf("admin active = %+v, want [{test-model 7}]", all.Active)
	}
	resp = liveGet(t, e, fmt.Sprintf("/api/admin/live?user_id=%d", friID), admTok)
	var filtered liveResp
	json.NewDecoder(resp.Body).Decode(&filtered)
	resp.Body.Close()
	if len(filtered.Active) != 1 || filtered.Active[0].Count != 5 {
		t.Errorf("user_id-filtered active = %+v, want [{test-model 5}]", filtered.Active)
	}
}

// Group pool view (plan 2026-11-22 open question): two members share one
// public name. The active list and the utilization series stay name-keyed
// (pool totals — that is the pool view), while the model rows are per
// member: each row carries its own id and its own stream count, and the
// util limit is the SUM of the members' limits (0 = any member unset).
func TestLiveSameNamePool(t *testing.T) {
	fl := fakeFlight{streams: map[int64][]proxy.Stream{}, since: map[int64]map[string][]time.Time{},
		byUser: map[int64]map[string]int{}}
	e := newLiveEnv(t, fl)
	admID := userID(t, e.db, "admin")

	members, err := store.ListModels(e.db)
	if err != nil || len(members) != 1 {
		t.Fatalf("models = %+v, err = %v", members, err)
	}
	mA := members[0].ID
	b2, err := store.CreateBackend(e.db, "eng2", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	mBModel, err := store.CreateModel(e.db, b2.ID, "up-id-2", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	two, three := int64(2), int64(3)
	if err := store.UpdateModelMeta(e.db, mA, nil, &two, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateModelMeta(e.db, mBModel.ID, nil, &three, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	// 2 streams on member A, 1 on member B; busy credit keeps the util
	// series alive for the name.
	for i := 0; i < 2; i++ {
		fl.streams[admID] = append(fl.streams[admID], proxy.Stream{UserID: admID, ModelID: mA, Model: "test-model"})
	}
	fl.streams[admID] = append(fl.streams[admID], proxy.Stream{UserID: admID, ModelID: mBModel.ID, Model: "test-model"})
	fl.byUser[admID] = map[string]int{"test-model": 3}
	fl.since[admID] = map[string][]time.Time{"test-model": {time.Now().Add(-10 * time.Second)}}

	tok := liveLogin(t, e, "admin", "adminpass1")
	resp := liveGet(t, e, "/api/admin/live", tok)
	var r liveResp
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()

	// Pool view: ONE name-keyed active row with the total.
	if len(r.Active) != 1 || r.Active[0].Model != "test-model" || r.Active[0].Count != 3 {
		t.Errorf("active = %+v, want [{test-model 3}] (pool total)", r.Active)
	}
	// Member rows: two rows, distinct ids, per-member counts (not 3/3).
	if len(r.Models) != 2 {
		t.Fatalf("models = %+v, want two member rows", r.Models)
	}
	// Deterministic (name, id) order: mA predates mBModel, so it leads.
	if r.Models[0].ID != mA || r.Models[1].ID != mBModel.ID {
		t.Errorf("row order = %d,%d, want %d,%d (id tie-break for same-name rows)",
			r.Models[0].ID, r.Models[1].ID, mA, mBModel.ID)
	}
	byID := map[int64]api.LiveModel{}
	for _, m := range r.Models {
		byID[m.ID] = m
	}
	if byID[mA].Active != 2 || byID[mBModel.ID].Active != 1 {
		t.Errorf("member actives = A:%d B:%d, want A:2 B:1 (per-member, not pool total)",
			byID[mA].Active, byID[mBModel.ID].Active)
	}
	// Util limit: summed pool slots 2+3=5.
	if len(r.Util) != 1 || r.Util[0].Model != "test-model" || r.Util[0].ConcurrencyLimit != 5 {
		t.Errorf("util = %+v, want one test-model row with limit 5 (pool sum)", r.Util)
	}

	// One member without a limit makes the pool limit unknown (0): the
	// frontend auto-scales instead of clipping the axis below real capacity.
	zero := int64(0)
	if err := store.UpdateModelMeta(e.db, mBModel.ID, nil, &zero, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	resp = liveGet(t, e, "/api/admin/live", tok)
	var r2 liveResp
	json.NewDecoder(resp.Body).Decode(&r2)
	resp.Body.Close()
	if len(r2.Util) != 1 || r2.Util[0].ConcurrencyLimit != 0 {
		t.Errorf("util limit = %+v, want 0 (member unset → pool unknown)", r2.Util)
	}

	// A disabled member must not inflate the pool denominator: with B back
	// at limit 3 but disabled, only A's 2 slots count.
	off := false
	if err := store.UpdateModelMeta(e.db, mBModel.ID, nil, &three, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(e.db, mBModel.ID, nil, &off, [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	resp = liveGet(t, e, "/api/admin/live", tok)
	var r3 liveResp
	json.NewDecoder(resp.Body).Decode(&r3)
	resp.Body.Close()
	if len(r3.Util) != 1 || r3.Util[0].ConcurrencyLimit != 2 {
		t.Errorf("util limit = %+v, want 2 (disabled member excluded from the pool sum)", r3.Util)
	}

	// Same for a member whose BACKEND is disabled: the store's eligibility
	// rule excludes it from serving, so its slots must not count either.
	// (B back on with limit 3, on backend eng2.)
	online := true
	if _, err := store.UpdateModel(e.db, mBModel.ID, nil, &online, [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateBackend(e.db, b2.ID, nil, nil, nil, &online, nil); err != nil {
		t.Fatal(err)
	}
	resp = liveGet(t, e, "/api/admin/live", tok)
	var r4 liveResp
	json.NewDecoder(resp.Body).Decode(&r4)
	resp.Body.Close()
	if len(r4.Util) != 1 || r4.Util[0].ConcurrencyLimit != 5 {
		t.Errorf("util limit = %+v, want 5 (backend re-enabled, member counts again)", r4.Util)
	}
	if _, err := store.UpdateBackend(e.db, b2.ID, nil, nil, nil, &off, nil); err != nil {
		t.Fatal(err)
	}
	resp = liveGet(t, e, "/api/admin/live", tok)
	var r5 liveResp
	json.NewDecoder(resp.Body).Decode(&r5)
	resp.Body.Close()
	if len(r5.Util) != 1 || r5.Util[0].ConcurrencyLimit != 2 {
		t.Errorf("util limit = %+v, want 2 (backend-disabled member excluded from the pool sum)", r5.Util)
	}
}

// The personal live view is group-aware: one synthetic pool row per ENABLED
// group with a servable member (group name, folded active, summed limit, no
// wiring), and the group OWNS its name — same-name public standalone rows are
// suppressed (shadowing, matching the catalog). Plain internal models and
// disabled groups are hidden; internal member names fold into the group name
// in active/util for EVERY group, public member names stay as requested.
// Admins keep the per-member view.
func TestLiveInternalRowsAdminOnly(t *testing.T) {
	fl := fakeFlight{streams: map[int64][]proxy.Stream{}, since: map[int64]map[string][]time.Time{},
		byUser: map[int64]map[string]int{}}
	e := newLiveEnv(t, fl)
	members, err := store.ListModels(e.db)
	if err != nil || len(members) != 1 {
		t.Fatalf("models = %+v, err = %v", members, err)
	}
	engID := members[0].BackendID
	b2, err := store.CreateBackend(e.db, "eng2", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	meta := func(id, limit int64) {
		t.Helper()
		if err := store.UpdateModelMeta(e.db, id, nil, &limit, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	// pool-x: enabled group, two same-name members (limits 2+3), one
	// stream each — the canonical pool.
	gx, err := store.CreateGroup(e.db, "pool-x")
	if err != nil {
		t.Fatal(err)
	}
	p1, err := store.CreateModel(e.db, engID, "up-p1", "pool-x")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := store.CreateModel(e.db, b2.ID, "up-p2", "pool-x")
	if err != nil {
		t.Fatal(err)
	}
	meta(p1.ID, 2)
	meta(p2.ID, 3)
	if _, err := store.UpdateGroup(e.db, gx.ID, nil, &yes,
		[]store.GroupMemberInput{{ModelID: p1.ID, Position: 0}, {ModelID: p2.ID, Position: 1}}); err != nil {
		t.Fatal(err)
	}

	// pool-y: enabled group whose member kept its own name (renamed after
	// joining) — the pool row must carry the GROUP name with folded active.
	gy, err := store.CreateGroup(e.db, "pool-y")
	if err != nil {
		t.Fatal(err)
	}
	q1, err := store.CreateModel(e.db, engID, "up-q1", "q-renamed")
	if err != nil {
		t.Fatal(err)
	}
	meta(q1.ID, 4)
	if _, err := store.UpdateGroup(e.db, gy.ID, nil, &yes,
		[]store.GroupMemberInput{{ModelID: q1.ID, Position: 0}}); err != nil {
		t.Fatal(err)
	}

	// pool-z: member joined but the group stayed DISABLED → no user row.
	gz, err := store.CreateGroup(e.db, "pool-z")
	if err != nil {
		t.Fatal(err)
	}
	z1, err := store.CreateModel(e.db, engID, "up-z1", "z-hidden")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(e.db, gz.ID, nil, nil,
		[]store.GroupMemberInput{{ModelID: z1.ID, Position: 0}}); err != nil {
		t.Fatal(err)
	}

	// pool-w: member shares the group name but was re-exposed to public →
	// the group OWNS the name (shadowing rule): the user sees the POOL row,
	// the standalone is suppressed (the store resolves the name to the
	// group, so the live view must not show the row that cannot serve it).
	gw, err := store.CreateGroup(e.db, "pool-w")
	if err != nil {
		t.Fatal(err)
	}
	w1, err := store.CreateModel(e.db, engID, "up-w1", "pool-w")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(e.db, gw.ID, nil, &yes,
		[]store.GroupMemberInput{{ModelID: w1.ID, Position: 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModelVisibility(e.db, w1.ID, "public"); err != nil {
		t.Fatal(err)
	}

	// pool-v: a PUBLIC member under its own name — its name must NOT fold
	// into the group name (it has its own visible row and is what the user
	// requested), but its streams still count in the pool aggregate.
	gv, err := store.CreateGroup(e.db, "pool-v")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := store.CreateModel(e.db, engID, "up-v1", "v-public")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(e.db, gv.ID, nil, &yes,
		[]store.GroupMemberInput{{ModelID: v1.ID, Position: 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModelVisibility(e.db, v1.ID, "public"); err != nil {
		t.Fatal(err)
	}

	// shadow-x: enabled group whose name is ALSO carried by an external
	// public row (AddModels import — the guard-free path). The group wins:
	// users see only the pool row; admins still see both rows.
	gs, err := store.CreateGroup(e.db, "shadow-x")
	if err != nil {
		t.Fatal(err)
	}
	sh1, err := store.CreateModel(e.db, engID, "up-sh1", "shadow-x")
	if err != nil {
		t.Fatal(err)
	}
	meta(sh1.ID, 6)
	if _, err := store.UpdateGroup(e.db, gs.ID, nil, &yes,
		[]store.GroupMemberInput{{ModelID: sh1.ID, Position: 0}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddModels(e.db, b2.ID, []string{"shadow-x"}); err != nil {
		t.Fatal(err)
	}

	// A plain internal model (admin flip, not a member): hidden entirely.
	secret, err := store.CreateModel(e.db, engID, "up-s", "secret-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModelVisibility(e.db, secret.ID, "internal"); err != nil {
		t.Fatal(err)
	}

	admID := userID(t, e.db, "admin")
	fl.streams[admID] = []proxy.Stream{
		{UserID: admID, ModelID: p1.ID, Model: "pool-x"},
		{UserID: admID, ModelID: p2.ID, Model: "pool-x"},
		{UserID: admID, ModelID: q1.ID, Model: "q-renamed"},
		{UserID: admID, ModelID: z1.ID, Model: "z-hidden"},
		{UserID: admID, ModelID: v1.ID, Model: "v-public"},
		{UserID: admID, ModelID: sh1.ID, Model: "shadow-x"},
		{UserID: admID, ModelID: secret.ID, Model: "secret-model"},
		// No catalog row at all: a deleted/renamed model with a live
		// stream — nothing internal about its name, it keeps showing.
		{UserID: admID, ModelID: 999999, Model: "ghost-model"},
	}
	fl.byUser[admID] = map[string]int{
		"pool-x": 2, "q-renamed": 1, "z-hidden": 1, "v-public": 1,
		"shadow-x": 1, "secret-model": 1, "ghost-model": 1,
	}
	fl.since[admID] = map[string][]time.Time{
		"q-renamed":    {time.Now().Add(-10 * time.Second)},
		"z-hidden":     {time.Now().Add(-10 * time.Second)},
		"secret-model": {time.Now().Add(-10 * time.Second)},
		"ghost-model":  {time.Now().Add(-10 * time.Second)},
		"v-public":     {time.Now().Add(-10 * time.Second)},
	}

	live := func(tok, path string) liveResp {
		t.Helper()
		resp := liveGet(t, e, path, tok)
		var r liveResp
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		return r
	}
	count := func(ms []api.LiveModel, name string) []api.LiveModel {
		var out []api.LiveModel
		for _, m := range ms {
			if m.Name == name {
				out = append(out, m)
			}
		}
		return out
	}

	admTok := liveLogin(t, e, "admin", "adminpass1")
	ar := live(admTok, "/api/admin/live")
	if px := count(ar.Models, "pool-x"); len(px) != 2 {
		t.Fatalf("admin pool-x rows = %+v, want two per-member rows", px)
	} else {
		for _, m := range px {
			if m.Active != 1 || m.Backend == "" || m.Upstream == "" {
				t.Errorf("admin member row = %+v, want per-member active 1 with backend+upstream", m)
			}
		}
	}
	for _, want := range []string{"q-renamed", "z-hidden", "secret-model", "pool-w", "v-public", "test-model"} {
		if len(count(ar.Models, want)) != 1 {
			t.Errorf("admin rows missing %q: %+v", want, ar.Models)
		}
	}
	// Admins see both shadowed rows: the group member and the external
	// same-name public import (the shadow is a user-facing view rule).
	if sx := count(ar.Models, "shadow-x"); len(sx) != 2 {
		t.Errorf("admin shadow-x rows = %+v, want member + external standalone", sx)
	}

	// Admins see the internal name in active/util — that is the point of
	// the admin route.
	admAct := map[string]int{}
	for _, a := range ar.Active {
		admAct[a.Model] = a.Count
	}
	if admAct["secret-model"] != 1 {
		t.Errorf("admin active = %+v, want secret-model:1", ar.Active)
	}
	admUtil := false
	for _, u := range ar.Util {
		if u.Model == "secret-model" {
			admUtil = true
		}
	}
	if !admUtil {
		t.Errorf("admin util missing secret-model series: %+v", ar.Util)
	}

	usrTok := liveLogin(t, e, "friend", "userpass1")
	ur := live(usrTok, "/api/live")
	// pool-x: one synthetic row, id = -groupID (can never collide with a
	// real row id), folded active, summed limit.
	px := count(ur.Models, "pool-x")
	if len(px) != 1 {
		t.Fatalf("user pool-x rows = %+v, want exactly one synthetic row", px)
	}
	if px[0].ID != -gx.ID || !px[0].Pool || px[0].Active != 2 || px[0].ConcurrencyLimit != 5 ||
		px[0].Backend != "" || px[0].Online || px[0].Draining {
		t.Errorf("user pool-x row = %+v, want id %d (synthetic -groupID), pool marker, active 2, limit 5, empty wiring",
			px[0], -gx.ID)
	}
	// pool-y: one row under the GROUP name with the member's folded active.
	py := count(ur.Models, "pool-y")
	if len(py) != 1 || py[0].ID != -gy.ID || py[0].Active != 1 || py[0].ConcurrencyLimit != 4 ||
		py[0].Backend != "" {
		t.Errorf("user pool-y row = %+v, want one row id %d active 1 limit 4, empty wiring", py, -gy.ID)
	}
	// Hidden: the member's own name, the disabled group's member, the
	// plain internal model, and the disabled group itself.
	for _, gone := range []string{"q-renamed", "z-hidden", "secret-model", "pool-z"} {
		if len(count(ur.Models, gone)) != 0 {
			t.Errorf("user rows must not contain %q: %+v", gone, ur.Models)
		}
	}
	// pool-w: the group owns the name — the user sees the POOL row and the
	// re-exposed standalone is suppressed (the store resolves the name to
	// the group, so showing the standalone would be a lie).
	pw := count(ur.Models, "pool-w")
	if len(pw) != 1 || pw[0].ID != -gw.ID || !pw[0].Pool || pw[0].Backend != "" {
		t.Errorf("user pool-w rows = %+v, want the single synthetic pool row id %d", pw, -gw.ID)
	}
	// shadow-x: an external public same-name row (imported) is shadowed by
	// the enabled group — users see only the pool row.
	sx := count(ur.Models, "shadow-x")
	if len(sx) != 1 || sx[0].ID != -gs.ID || !sx[0].Pool || sx[0].Backend != "" ||
		sx[0].Active != 1 || sx[0].ConcurrencyLimit != 6 {
		t.Errorf("user shadow-x rows = %+v, want only the pool row id %d active 1 limit 6", sx, -gs.ID)
	}
	// pool-v: a PUBLIC member keeps its own visible standalone row (not
	// folded, not suppressed — the pool row carries a different name), and
	// its stream still counts in the pool aggregate.
	pv := count(ur.Models, "pool-v")
	if len(pv) != 1 || pv[0].ID != -gv.ID || !pv[0].Pool || pv[0].Active != 1 {
		t.Errorf("user pool-v rows = %+v, want pool row id %d with the public member's stream counted",
			pv, -gv.ID)
	}
	vp := count(ur.Models, "v-public")
	if len(vp) != 1 || vp[0].Backend == "" || vp[0].Pool {
		t.Errorf("user v-public rows = %+v, want the real standalone public row", vp)
	}
	if len(count(ur.Models, "test-model")) != 1 {
		t.Errorf("user rows missing the public test-model: %+v", ur.Models)
	}
	// Active list: internal member names fold into the group name — even
	// for the DISABLED group (their streams would otherwise leak the
	// internal name); public member names stay as requested; internal-only
	// names (plain internal model) are dropped; a name with no row at all
	// (deleted model with a live stream) keeps showing.
	act := map[string]int{}
	for _, a := range ur.Active {
		act[a.Model] = a.Count
	}
	if act["pool-x"] != 2 || act["pool-y"] != 1 || act["pool-z"] != 1 || act["v-public"] != 1 {
		t.Errorf("user active = %+v, want pool-x:2 pool-y:1 pool-z:1 v-public:1", ur.Active)
	}
	if act["ghost-model"] != 1 {
		t.Errorf("user active = %+v, want the rowless ghost-model:1 kept", ur.Active)
	}
	for _, leaked := range []string{"q-renamed", "z-hidden", "secret-model"} {
		if _, ok := act[leaked]; ok {
			t.Errorf("user active leaks internal name %q: %+v", leaked, ur.Active)
		}
	}
	// Util series: in-flight busy credits fold to the group name too
	// (disabled group included); public member names stay put.
	utilNames := map[string]int64{}
	for _, u := range ur.Util {
		utilNames[u.Model] = u.ConcurrencyLimit
	}
	if _, ok := utilNames["pool-y"]; !ok {
		t.Errorf("user util missing folded pool-y series: %+v", ur.Util)
	}
	if utilNames["pool-y"] != 4 {
		t.Errorf("pool-y util limit = %d, want 4 (pool sum)", utilNames["pool-y"])
	}
	if _, ok := utilNames["pool-z"]; !ok {
		t.Errorf("user util missing folded pool-z series (disabled group): %+v", ur.Util)
	}
	for _, leaked := range []string{"q-renamed", "z-hidden", "secret-model"} {
		if _, ok := utilNames[leaked]; ok {
			t.Errorf("user util leaks internal name %q: %+v", leaked, ur.Util)
		}
	}
	if _, ok := utilNames["ghost-model"]; !ok {
		t.Errorf("user util dropped the rowless ghost-model series: %+v", ur.Util)
	}
	// pool-v: the public member's in-flight busy credits BOTH series —
	// its own (direct requests keep their name) and the pool's (the pool
	// limit sums its slots, so the numerator must cover them too).
	lastVal := func(model string) (float64, bool) {
		for _, u := range ur.Util {
			if u.Model == model && len(u.Values) > 0 {
				return u.Values[len(u.Values)-1], true
			}
		}
		return 0, false
	}
	if v, ok := lastVal("pool-v"); !ok || v <= 0 {
		t.Errorf("user util missing pool-v credit from the public member's stream: %+v", ur.Util)
	}
	if v, ok := lastVal("v-public"); !ok || v <= 0 {
		t.Errorf("user util dropped the public member's own series: %+v", ur.Util)
	}
}

func TestLiveConnectionsAdminOnly(t *testing.T) {
	fl := fakeFlight{streams: map[int64][]proxy.Stream{
		1: {{UserID: 1, User: "admin", Key: "cli", Model: "test-model", Endpoint: "chat",
			Session: "sess-abc123", IP: "10.0.0.5", Streaming: true,
			Start: time.Now().Add(-90 * time.Second)}},
	}, recent: map[int64][]proxy.Stream{
		2: {{UserID: 2, User: "friend", Key: "laptop", Model: "test-model", Endpoint: "chat",
			Start: time.Now().Add(-20 * time.Second), End: time.Now().Add(-15 * time.Second)}},
	}}
	e := newLiveEnv(t, fl)
	seedUsage(t, e.db, userID(t, e.db, "friend"), time.Now().Add(-5*time.Minute), 10, 5, 100)

	admTok := liveLogin(t, e, "admin", "adminpass1")
	resp := liveGet(t, e, "/api/admin/live", admTok)
	var r struct {
		Connections []struct {
			User      string `json:"user"`
			Key       string `json:"key"`
			Model     string `json:"model"`
			Session   string `json:"session"`
			Streaming bool   `json:"streaming"`
			Running   bool   `json:"running"`
			EndedAt   string `json:"ended_at"`
			Seconds   int64  `json:"seconds"`
		} `json:"connections"`
		LastUsed []struct {
			User     string `json:"user"`
			LastUsed string `json:"last_used"`
		} `json:"last_used"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()

	if len(r.Connections) != 2 {
		t.Fatalf("connections = %+v, want 2 rows (1 active + 1 recent)", r.Connections)
	}
	byUser := map[string]bool{}
	secs := map[string]int64{}
	for _, c := range r.Connections {
		byUser[c.User] = c.Running
		secs[c.User] = c.Seconds
		if c.Model != "test-model" || c.Key == "" {
			t.Errorf("connection row = %+v, want model + key set", c)
		}
	}
	if running, ok := byUser["admin"]; !ok || !running {
		t.Errorf("admin row running = %v (present=%v), want true", byUser["admin"], ok)
	}
	if s := secs["admin"]; s < 85 || s > 95 {
		t.Errorf("admin seconds = %d, want ~90", s)
	}
	if running, ok := byUser["friend"]; !ok || running {
		t.Errorf("friend row running = %v (present=%v), want false", byUser["friend"], ok)
	}
	for _, c := range r.Connections {
		if c.User == "friend" {
			if c.EndedAt == "" {
				t.Error("finished row missing ended_at")
			}
			if c.Seconds < 4 || c.Seconds > 6 {
				t.Errorf("friend ran %d s, want ~5", c.Seconds)
			}
		}
	}
	if len(r.LastUsed) != 1 || r.LastUsed[0].User != "friend" {
		t.Errorf("last_used = %+v, want [friend]", r.LastUsed)
	}

	// Personal route must not leak other users' connection rows.
	friTok := liveLogin(t, e, "friend", "userpass1")
	resp = liveGet(t, e, "/api/live", friTok)
	var raw map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	if _, ok := raw["connections"]; ok {
		t.Error("personal live leaked connections")
	}
	if _, ok := raw["last_used"]; ok {
		t.Error("personal live leaked last_used")
	}
}
