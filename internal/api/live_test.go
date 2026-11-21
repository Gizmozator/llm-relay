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
	if err := store.UpdateModelMeta(e.db, models[0].ID, nil, &four, nil); err != nil {
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
	e := newLiveEnv(t, fakeFlight{byUser: flights})
	admID := userID(t, e.db, "admin")
	friID := userID(t, e.db, "friend")
	flights[admID] = map[string]int{"test-model": 2}
	flights[friID] = map[string]int{"test-model": 5}

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

func TestLiveConnectionsAdminOnly(t *testing.T) {
	fl := fakeFlight{streams: map[int64][]proxy.Stream{
		1: {{UserID: 1, User: "admin", Key: "cli", Model: "test-model", Endpoint: "chat",
			Session: "sess-abc123", IP: "10.0.0.5", Streaming: true,
			Start: time.Now().Add(-90 * time.Second)}},
		2: {{UserID: 2, User: "friend", Key: "laptop", Model: "test-model", Endpoint: "chat",
			Start: time.Now().Add(-10 * time.Second)}},
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
		t.Fatalf("connections = %+v, want 2 rows", r.Connections)
	}
	byUser := map[string]int64{}
	for _, c := range r.Connections {
		byUser[c.User] = c.Seconds
		if c.Model != "test-model" || c.Key == "" {
			t.Errorf("connection row = %+v, want model + key set", c)
		}
	}
	if s, ok := byUser["admin"]; !ok || s < 85 || s > 95 {
		t.Errorf("admin seconds = %d, want ~90", byUser["admin"])
	}
	if s, ok := byUser["friend"]; !ok || s < 5 || s > 15 {
		t.Errorf("friend seconds = %d, want ~10", byUser["friend"])
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
