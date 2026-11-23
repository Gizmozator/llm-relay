// API-level tests: login, keys, admin backend discovery flow, usage scoping.
package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/api"
	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/fed"
	"llm-relay/internal/store"
)

type testEnv struct {
	db    *sql.DB
	srv   *httptest.Server
	token string // admin JWT
}

func newTestEnv(t *testing.T, upstream http.Handler) *testEnv {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	d, err := db.Open(t.TempDir() + "/api.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	hash, _ := auth.HashPassword("adminpass1")
	if _, err := store.CreateUser(d, "admin", "", hash, "admin"); err != nil {
		t.Fatal(err)
	}

	s := &api.Server{DB: d, JWTSecret: "testsecret", JWTTTL: time.Hour}
	mux := http.NewServeMux()
	mux.Handle("POST /api/auth/login", s.RateLimitLogin(s.Login))
	mux.Handle("GET /api/me", s.Auth(s.Me))
	mux.Handle("PATCH /api/me", s.Auth(s.UpdateMe))
	mux.Handle("POST /api/auth/change_password", s.Auth(s.ChangePassword))
	mux.Handle("GET /api/info", s.Auth(s.Info))
	mux.Handle("GET /api/models", s.Auth(s.PublicModels))
	mux.Handle("POST /api/keys", s.Auth(s.CreateKey))
	mux.Handle("GET /api/keys", s.Auth(s.ListKeys))
	mux.Handle("GET /api/usage/summary", s.Auth(s.UsageSummary))
	mux.Handle("GET /api/usage/detail", s.Auth(s.UsageDetail))
	mux.Handle("GET /api/admin/users", s.Admin(s.AdminListUsers))
	mux.Handle("PATCH /api/admin/users/{id}", s.Admin(s.AdminUpdateUser))
	mux.Handle("PATCH /api/admin/models/{id}", s.Admin(s.AdminUpdateModel))
	mux.Handle("POST /api/admin/models/{id}/benchmark", s.Admin(s.AdminBenchmarkModel))
	mux.Handle("POST /api/admin/users", s.Admin(s.AdminCreateUser))
	mux.Handle("GET /api/admin/usage/detail", s.Admin(s.UsageDetail))
	mux.Handle("GET /api/admin/backends", s.Admin(s.AdminListBackends))
	mux.Handle("POST /api/admin/backends", s.Admin(s.AdminCreateBackend))
	mux.Handle("PATCH /api/admin/backends/{id}", s.Admin(s.AdminUpdateBackend))
	mux.Handle("POST /api/admin/backends/{id}/probe", s.Admin(s.AdminProbeBackend))
	mux.Handle("POST /api/admin/backends/{id}/models", s.Admin(s.AdminAddBackendModels))
	mux.Handle("GET /api/admin/groups", s.Admin(s.AdminListGroups))
	mux.Handle("POST /api/admin/groups", s.Admin(s.AdminCreateGroup))
	mux.Handle("GET /api/admin/groups/{id}", s.Admin(s.AdminGetGroup))
	mux.Handle("PATCH /api/admin/groups/{id}", s.Admin(s.AdminUpdateGroup))
	mux.Handle("DELETE /api/admin/groups/{id}", s.Admin(s.AdminDeleteGroup))
	mux.Handle("GET /api/peers", s.Auth(s.ListPeers))
	mux.Handle("POST /api/peers", s.Auth(s.CreatePeer))
	mux.Handle("POST /api/peers/{id}/rotate", s.Auth(s.RotateMyPeer))
	mux.Handle("DELETE /api/peers/{id}", s.Auth(s.DeleteMyPeer))
	mux.Handle("GET /api/admin/peers", s.Admin(s.AdminListPeers))
	mux.Handle("POST /api/admin/peers/{id}/rotate", s.Admin(s.AdminRotatePeer))
	mux.Handle("PATCH /api/admin/peers/{id}", s.Admin(s.AdminUpdatePeer))
	mux.Handle("DELETE /api/admin/peers/{id}", s.Admin(s.AdminDeletePeer))
	mux.Handle("GET /api/admin/federation", s.Admin(s.AdminGetFederation))
	mux.Handle("PUT /api/admin/federation", s.Admin(s.AdminPutFederation))
	mux.Handle("GET /federation/v1/ping", s.PeerAuth(s.FederationPing))
	// Federation channel: the real Manager (production defaults) behind
	// the same PeerAuth + role-gate wiring main.go uses.
	fedMgr := fed.NewManager(d, &fed.Options{Log: log.New(io.Discard, "", 0)})
	t.Cleanup(fedMgr.Close)
	s.Fed = fedMgr
	mux.Handle("GET /federation/v1/connect", s.PeerAuth(s.FederationConnect))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testEnv{db: d, srv: srv}
}

func (e *testEnv) do(t *testing.T, method, path, body string, token string) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func login(t *testing.T, e *testEnv) string {
	t.Helper()
	resp := e.do(t, "POST", "/api/auth/login", `{"username":"admin","password":"adminpass1"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Token == "" {
		t.Fatal("no token")
	}
	return out.Token
}

func TestLoginAndMe(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	resp := e.do(t, "GET", "/api/me", "", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("me status %d", resp.StatusCode)
	}
	var u store.User
	json.NewDecoder(resp.Body).Decode(&u)
	if u.Username != "admin" || u.Role != "admin" {
		t.Errorf("user = %+v", u)
	}
	// No token → 401
	if resp := e.do(t, "GET", "/api/me", "", ""); resp.StatusCode != 401 {
		t.Errorf("unauth me = %d", resp.StatusCode)
	}
}

func TestKeyLifecycle(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)

	resp := e.do(t, "POST", "/api/keys", `{"name":"phone"}`, tok)
	if resp.StatusCode != 201 {
		t.Fatalf("create key status %d", resp.StatusCode)
	}
	var k struct {
		ID   int64  `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&k)
	resp.Body.Close()
	if !startsWith(k.Key, "sk-") || k.Name != "phone" {
		t.Fatalf("key = %+v", k)
	}

	resp = e.do(t, "GET", "/api/keys", "", tok)
	var kl struct {
		Keys []store.APIKey `json:"keys"`
	}
	json.NewDecoder(resp.Body).Decode(&kl)
	resp.Body.Close()
	if len(kl.Keys) != 1 {
		t.Fatalf("keys = %d", len(kl.Keys))
	}
}

func TestBackendDiscoveryFlow(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"version":"9.9.9"}`))
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	e := newTestEnv(t, up)
	tok := login(t, e)

	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	resp := e.do(t, "POST", "/api/admin/backends", `{"name":"fake","base_url":"`+upSrv.URL+`"}`, tok)
	if resp.StatusCode != 201 {
		t.Fatalf("create backend %d", resp.StatusCode)
	}
	var b store.Backend
	json.NewDecoder(resp.Body).Decode(&b)
	resp.Body.Close()
	if b.ID == 0 {
		t.Fatal("backend id 0")
	}

	resp = e.do(t, "POST", pathf("/api/admin/backends/%d/probe", b.ID), "", tok)
	var pr struct {
		OK      bool   `json:"ok"`
		Version string `json:"server_version"`
		Models  []struct {
			ID        string `json:"id"`
			InCatalog bool   `json:"in_catalog"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if !pr.OK || pr.Version != "9.9.9" || len(pr.Models) != 2 {
		t.Fatalf("probe = %+v", pr)
	}

	resp = e.do(t, "POST", pathf("/api/admin/backends/%d/models", b.ID), `{"model_ids":["model-a"]}`, tok)
	if resp.StatusCode != 201 {
		t.Fatalf("add models %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Re-probe: model-a now in catalog.
	resp = e.do(t, "POST", pathf("/api/admin/backends/%d/probe", b.ID), "", tok)
	pr = struct {
		OK      bool   `json:"ok"`
		Version string `json:"server_version"`
		Models  []struct {
			ID        string `json:"id"`
			InCatalog bool   `json:"in_catalog"`
		} `json:"models"`
	}{}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if !pr.Models[0].InCatalog || pr.Models[1].InCatalog {
		t.Errorf("in_catalog flags wrong: %+v", pr.Models)
	}
}

// The Backends page Status column must reflect live reachability (shared
// health cache), not the stored manual-probe fields.
func TestAdminBackendsLiveStatus(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"9.9.9"}`))
	})
	e := newTestEnv(t, up)
	tok := login(t, e)

	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	create := func(name, url string) int64 {
		t.Helper()
		resp := e.do(t, "POST", "/api/admin/backends",
			fmt.Sprintf(`{"name":%q,"base_url":%q}`, name, url), tok)
		if resp.StatusCode != 201 {
			resp.Body.Close()
			t.Fatalf("create %s: %d", name, resp.StatusCode)
		}
		var b store.Backend
		json.NewDecoder(resp.Body).Decode(&b)
		resp.Body.Close()
		return b.ID
	}
	create("live", upSrv.URL)
	create("dead", "http://127.0.0.1:1")
	offID := create("off", upSrv.URL)
	if resp := e.do(t, "PATCH", pathf("/api/admin/backends/%d", offID), `{"enabled":false}`, tok); resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("disable off backend: %d", resp.StatusCode)
	}

	resp := e.do(t, "GET", "/api/admin/backends", "", tok)
	var bl struct {
		Backends []struct {
			Name      string `json:"name"`
			Online    bool   `json:"online"`
			HealthErr string `json:"health_error"`
		} `json:"backends"`
	}
	json.NewDecoder(resp.Body).Decode(&bl)
	resp.Body.Close()
	online := map[string]bool{}
	herr := map[string]string{}
	for _, b := range bl.Backends {
		online[b.Name], herr[b.Name] = b.Online, b.HealthErr
	}
	if !online["live"] {
		t.Errorf("live backend online=false, want true")
	}
	if online["dead"] {
		t.Errorf("dead backend online=true, want false")
	}
	if herr["dead"] == "" {
		t.Errorf("dead backend health_error empty, want probe failure")
	}
	// Disabled backends are offline without a probe, even when reachable.
	if online["off"] {
		t.Errorf("disabled backend online=true, want false")
	}
	if herr["off"] != "backend disabled" {
		t.Errorf("disabled backend health_error = %q, want %q", herr["off"], "backend disabled")
	}
}

func startsWith(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func pathf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// ---- auth & scoping (reviewer-mandated coverage) -------------------------

func loginAs(t *testing.T, e *testEnv, user, pass string) string {
	t.Helper()
	resp := e.do(t, "POST", "/api/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass), "")
	if resp.StatusCode != 200 {
		t.Fatalf("login %s status %d", user, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return out.Token
}

func TestDisabledUserJWTRejected(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	// Create a normal user, log in, then disable them.
	resp := e.do(t, "POST", "/api/admin/users", `{"username":"bob","password":"bobpass123","role":"user"}`, admin)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create user %d", resp.StatusCode)
	}
	bob := loginAs(t, e, "bob", "bobpass123")
	if bob == "" {
		t.Fatal("bob login failed")
	}

	// Disable bob as admin.
	var bobID int64
	{
		resp := e.do(t, "GET", "/api/admin/users", "", admin)
		var out struct {
			Users []store.User `json:"users"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		for _, u := range out.Users {
			if u.Username == "bob" {
				bobID = u.ID
			}
		}
	}
	if bobID == 0 {
		t.Fatal("bob not found")
	}
	// PATCH route is not on the test mux; disable directly in the DB.
	if _, err := store.UpdateUser(e.db, bobID, nil, ptrTrue(), nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	// bob's still-valid JWT must now be rejected.
	if resp := e.do(t, "GET", "/api/me", "", bob); resp.StatusCode != http.StatusForbidden {
		t.Errorf("disabled user me = %d, want 403", resp.StatusCode)
	}
}

func ptrTrue() *bool { b := true; return &b }

func TestPublicModelsCatalog(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	// Two models on an enabled backend (one priced), one disabled model,
	// one model on a disabled backend: only the first two are listed.
	resp := e.do(t, "POST", "/api/admin/backends", `{"name":"b1","base_url":"http://127.0.0.1:1"}`, admin)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create backend %d", resp.StatusCode)
	}
	if _, err := store.CreateModel(e.db, 1, "up-a", "Model A"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(e.db, 1, nil, nil, [4]*int64{ptrI(100), ptrI(200), ptrI(10), ptrI(40)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(e.db, 1, "up-b", "Model B"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(e.db, 2, nil, ptrB(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBackend(e.db, "off", "http://127.0.0.1:2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(e.db, 2, "up-c", "Model C"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateBackend(e.db, 2, nil, nil, nil, ptrB(false), nil); err != nil {
		t.Fatal(err)
	}

	// A normal user sees the catalog.
	resp = e.do(t, "POST", "/api/admin/users", `{"username":"bob","password":"bobpass123","role":"user"}`, admin)
	resp.Body.Close()
	bob := loginAs(t, e, "bob", "bobpass123")
	resp = e.do(t, "GET", "/api/models", "", bob)
	if resp.StatusCode != 200 {
		t.Fatalf("user GET /api/models = %d", resp.StatusCode)
	}
	var out struct {
		Models []struct {
			Name            string `json:"name"`
			PriceIn         int64  `json:"price_in"`
			PriceOut        int64  `json:"price_out"`
			PriceCacheRead  int64  `json:"price_cache_read"`
			PriceCacheWrite int64  `json:"price_cache_write"`
			BackendName     string `json:"backend_name"`
			UpstreamModel   string `json:"upstream_model"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Models) != 1 || out.Models[0].Name != "Model A" {
		t.Fatalf("models = %+v, want only Model A", out.Models)
	}
	m := out.Models[0]
	if m.PriceIn != 100 || m.PriceOut != 200 || m.PriceCacheRead != 10 || m.PriceCacheWrite != 40 {
		t.Errorf("prices not exposed: %+v", m)
	}
	if m.BackendName != "" || m.UpstreamModel != "" {
		t.Errorf("internal fields leaked: %+v", m)
	}

	// Unauthenticated is rejected.
	if resp := e.do(t, "GET", "/api/models", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon GET /api/models = %d, want 401", resp.StatusCode)
	}
}

func ptrI(v int64) *int64 { return &v }
func ptrB(v bool) *bool   { return &v }

func TestChangePasswordAndUpdateMe(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)

	// Wrong old password → 401.
	resp := e.do(t, "POST", "/api/auth/change_password", `{"old_password":"wrong","new_password":"newpass123"}`, tok)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong old password status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Too-short new password → 400.
	resp = e.do(t, "POST", "/api/auth/change_password", `{"old_password":"adminpass1","new_password":"short"}`, tok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Correct change → 204, old login fails, new login works.
	resp = e.do(t, "POST", "/api/auth/change_password", `{"old_password":"adminpass1","new_password":"newpass123"}`, tok)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change status %d", resp.StatusCode)
	}
	resp.Body.Close()
	if r := e.do(t, "POST", "/api/auth/login", `{"username":"admin","password":"adminpass1"}`, ""); r.StatusCode != 401 {
		t.Errorf("old password still works")
	}
	if r := loginAs(t, e, "admin", "newpass123"); r == "" {
		t.Errorf("new password login failed")
	}
	// The password change revoked the pre-change token (see
	// TestPasswordChangeInvalidatesJWT), so continue with a fresh one.
	tok = loginAs(t, e, "admin", "newpass123")

	// Display name round-trip via PATCH /api/me.
	resp = e.do(t, "PATCH", "/api/me", `{"display_name":"Ada Admin"}`, tok)
	var u store.User
	json.NewDecoder(resp.Body).Decode(&u)
	resp.Body.Close()
	if u.DisplayName != "Ada Admin" {
		t.Fatalf("display_name = %q", u.DisplayName)
	}
	resp = e.do(t, "GET", "/api/me", "", tok)
	u = store.User{}
	json.NewDecoder(resp.Body).Decode(&u)
	resp.Body.Close()
	if u.DisplayName != "Ada Admin" {
		t.Errorf("me display_name = %q", u.DisplayName)
	}
}

func TestUsageScopingPerUser(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	for _, name := range []string{"alice", "bob"} {
		resp := e.do(t, "POST", "/api/admin/users",
			fmt.Sprintf(`{"username":%q,"password":"pass123456","role":"user"}`, name), admin)
		resp.Body.Close()
	}
	// One usage row per user, directly in the DB.
	var aliceID, bobID int64
	{
		resp := e.do(t, "GET", "/api/admin/users", "", admin)
		var out struct {
			Users []store.User `json:"users"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		for _, u := range out.Users {
			switch u.Username {
			case "alice":
				aliceID = u.ID
			case "bob":
				bobID = u.ID
			}
		}
	}
	for _, uid := range []int64{aliceID, bobID} {
		if err := store.InsertUsage(e.db, &store.UsageEvent{
			TS: "2026-11-20T10:00:00Z", UserID: uid, KeyID: 99, Endpoint: "chat",
			Prompt: 10, Completion: 5, CostTotal: 7, Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}

	alice := loginAs(t, e, "alice", "pass123456")

	// alice sees exactly her own row.
	resp := e.do(t, "GET", "/api/usage/detail", "", alice)
	var detail struct {
		Total int              `json:"total"`
		Rows  []store.UsageRow `json:"rows"`
	}
	json.NewDecoder(resp.Body).Decode(&detail)
	resp.Body.Close()
	if detail.Total != 1 || (len(detail.Rows) == 1 && detail.Rows[0].Username != "alice") {
		t.Errorf("alice detail = %+v, want only her row", detail)
	}

	// alice cannot reach admin endpoints.
	if resp := e.do(t, "GET", "/api/admin/users", "", alice); resp.StatusCode != http.StatusForbidden {
		t.Errorf("user on admin route = %d, want 403", resp.StatusCode)
	}

	// admin detail (unscoped) sees both rows.
	resp = e.do(t, "GET", "/api/admin/usage/detail", "", admin)
	detail = struct {
		Total int              `json:"total"`
		Rows  []store.UsageRow `json:"rows"`
	}{}
	json.NewDecoder(resp.Body).Decode(&detail)
	resp.Body.Close()
	if detail.Total != 2 {
		t.Errorf("admin detail total = %d, want 2", detail.Total)
	}

	// The personal endpoints scope admins to themselves too (admin has no
	// usage rows here): the all-users view lives only on /api/admin/*.
	resp = e.do(t, "GET", "/api/usage/detail", "", admin)
	detail = struct {
		Total int              `json:"total"`
		Rows  []store.UsageRow `json:"rows"`
	}{}
	json.NewDecoder(resp.Body).Decode(&detail)
	resp.Body.Close()
	if detail.Total != 0 {
		t.Errorf("admin personal detail total = %d, want 0 (own rows only)", detail.Total)
	}
	resp = e.do(t, "GET", "/api/usage/summary?group_by=day", "", admin)
	var sum struct {
		Totals store.UsageGroup `json:"totals"`
	}
	json.NewDecoder(resp.Body).Decode(&sum)
	resp.Body.Close()
	if sum.Totals.Requests != 0 {
		t.Errorf("admin personal summary requests = %d, want 0", sum.Totals.Requests)
	}
}

// model_upstream is internal wiring (the engine-side model id that tells
// group members apart): it must never reach a non-admin usage view, so the
// handler zeroes it on the personal route instead of relying on the UI.
func TestUsageDetailUpstreamAdminOnly(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	resp := e.do(t, "POST", "/api/admin/users",
		`{"username":"alice","password":"pass123456","role":"user"}`, admin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create alice = %d, want 201", resp.StatusCode)
	}
	var users struct {
		Users []store.User `json:"users"`
	}
	resp = e.do(t, "GET", "/api/admin/users", "", admin)
	json.NewDecoder(resp.Body).Decode(&users)
	resp.Body.Close()
	var aliceID int64
	for _, u := range users.Users {
		if u.Username == "alice" {
			aliceID = u.ID
		}
	}
	if aliceID == 0 {
		t.Fatal("alice not found")
	}
	if err := store.InsertUsage(e.db, &store.UsageEvent{
		TS: "2026-11-20T10:00:00Z", UserID: aliceID, KeyID: 99, Endpoint: "chat",
		ModelRequ: "pool-x", ModelUpstream: "engine-variant-b",
		Prompt: 10, Completion: 5, CostTotal: 7, Status: 200,
	}); err != nil {
		t.Fatal(err)
	}

	type detail struct {
		Total int `json:"total"`
		Rows  []struct {
			ModelRequested string  `json:"model_requested"`
			ModelUpstream  *string `json:"model_upstream"`
		} `json:"rows"`
	}
	decode := func(resp *http.Response) detail {
		t.Helper()
		var d detail
		json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()
		if d.Total != 1 || len(d.Rows) != 1 {
			t.Fatalf("detail = %+v, want one row", d)
		}
		return d
	}

	alice := loginAs(t, e, "alice", "pass123456")
	if d := decode(e.do(t, "GET", "/api/usage/detail", "", alice)); d.Rows[0].ModelUpstream != nil {
		t.Errorf("user sees model_upstream = %q, want absent", *d.Rows[0].ModelUpstream)
	}
	d := decode(e.do(t, "GET", "/api/admin/usage/detail", "", admin))
	if d.Rows[0].ModelUpstream == nil || *d.Rows[0].ModelUpstream != "engine-variant-b" {
		t.Errorf("admin sees model_upstream = %v, want engine-variant-b", d.Rows[0].ModelUpstream)
	}
}

func TestUserMultiplierAndModelMeta(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	// bob: normal user, multiplier 2.5.
	resp := e.do(t, "POST", "/api/admin/users", `{"username":"bob","password":"bobpass123","role":"user"}`, admin)
	resp.Body.Close()
	bob := loginAs(t, e, "bob", "bobpass123")
	var bobID int64
	{
		resp := e.do(t, "GET", "/api/admin/users", "", admin)
		var out struct {
			Users []store.User `json:"users"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		for _, u := range out.Users {
			if u.Username == "bob" {
				bobID = u.ID
				if u.PriceMultiplier != 1 {
					t.Errorf("default multiplier = %v, want 1", u.PriceMultiplier)
				}
			}
		}
	}
	if bobID == 0 {
		t.Fatal("bob not found")
	}
	resp = e.do(t, "PATCH", fmt.Sprintf("/api/admin/users/%d", bobID), `{"price_multiplier":2.5}`, admin)
	if resp.StatusCode != 200 {
		t.Fatalf("set multiplier: %d", resp.StatusCode)
	}
	var upd store.User
	json.NewDecoder(resp.Body).Decode(&upd)
	resp.Body.Close()
	if upd.PriceMultiplier != 2.5 {
		t.Errorf("multiplier = %v, want 2.5", upd.PriceMultiplier)
	}
	// Invalid values rejected.
	if resp := e.do(t, "PATCH", fmt.Sprintf("/api/admin/users/%d", bobID), `{"price_multiplier":0}`, admin); resp.StatusCode != 400 {
		t.Errorf("multiplier 0 = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	// The user's own /api/me must not reveal the multiplier.
	resp = e.do(t, "GET", "/api/me", "", bob)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("price_multiplier")) {
		t.Errorf("/api/me leaks price_multiplier: %s", body)
	}
	// ...and neither may PATCH /api/me (same user object on the write path).
	resp = e.do(t, "PATCH", "/api/me", `{"display_name":"Bobby"}`, bob)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("price_multiplier")) {
		t.Errorf("PATCH /api/me leaks price_multiplier: %s", body)
	}
	// Login response must not carry it either.
	resp = e.do(t, "POST", "/api/auth/login", `{"username":"bob","password":"bobpass123"}`, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("price_multiplier")) {
		t.Errorf("login leaks price_multiplier: %s", body)
	}

	// Model meta: context length + concurrency on a real backend/model.
	// Fake upstream: streams SSE deltas + a final usage chunk (the
	// benchmark requests stream=true with include_usage).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"word%d \",\"role\":\"assistant\"}}]}\n\n", i)
			if f != nil {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":100}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f != nil {
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	if _, err := store.CreateBackend(e.db, "bench", up.URL, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(e.db, 1, "up-m", "Bench Model"); err != nil {
		t.Fatal(err)
	}
	resp = e.do(t, "PATCH", "/api/admin/models/1", `{"context_length":262144,"concurrency_limit":4}`, admin)
	if resp.StatusCode != 200 {
		t.Fatalf("model meta patch: %d", resp.StatusCode)
	}
	var mm store.Model
	json.NewDecoder(resp.Body).Decode(&mm)
	resp.Body.Close()
	if mm.ContextLength != 262144 || mm.ConcurrencyLimit != 4 {
		t.Errorf("meta = %d/%d, want 262144/4", mm.ContextLength, mm.ConcurrencyLimit)
	}

	// Busy threshold: >=0 and sane-cap enforced, value round-trips
	// (over 100 = stacks with parallel streams).
	if resp := e.do(t, "PATCH", "/api/admin/models/1", `{"busy_threshold_pct":10001}`, admin); resp.StatusCode != 400 {
		resp.Body.Close()
		t.Errorf("busy_threshold_pct=10001 PATCH = %d, want 400", resp.StatusCode)
	}
	if resp := e.do(t, "PATCH", "/api/admin/models/1", `{"busy_threshold_pct":-1}`, admin); resp.StatusCode != 400 {
		resp.Body.Close()
		t.Errorf("busy_threshold_pct=-1 PATCH = %d, want 400", resp.StatusCode)
	}
	resp = e.do(t, "PATCH", "/api/admin/models/1", `{"busy_threshold_pct":150}`, admin)
	var bt store.Model
	json.NewDecoder(resp.Body).Decode(&bt)
	resp.Body.Close()
	if bt.BusyThresholdPct != 150 {
		t.Errorf("busy_threshold_pct = %d, want 150", bt.BusyThresholdPct)
	}

	// Benchmark: fake upstream reports 100 completion + 10 prompt tokens;
	// both the decode (TG) and prefill (PP) passes must come back measured.
	resp = e.do(t, "POST", "/api/admin/models/1/benchmark", "", admin)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("benchmark: %d %s", resp.StatusCode, body)
	}
	var br struct {
		BenchTokPerS   float64 `json:"bench_tok_per_s"`
		BenchPPTokPerS float64 `json:"bench_pp_tok_per_s"`
		Completion     int64   `json:"completion_tokens"`
		Prompt         int64   `json:"prompt_tokens"`
	}
	json.NewDecoder(resp.Body).Decode(&br)
	resp.Body.Close()
	if br.Completion != 100 || br.BenchTokPerS <= 0 {
		t.Errorf("benchmark result: %+v", br)
	}
	if br.Prompt != 10 || br.BenchPPTokPerS <= 0 {
		t.Errorf("benchmark prefill result: %+v", br)
	}

	// Manual speed override: negative → 400; a set value is stored and
	// bench_at is cleared (configured speed, not a measured result).
	if resp := e.do(t, "PATCH", "/api/admin/models/1", `{"bench_tok_per_s":-1}`, admin); resp.StatusCode != 400 {
		resp.Body.Close()
		t.Errorf("negative speed PATCH = %d, want 400", resp.StatusCode)
	}
	if resp := e.do(t, "PATCH", "/api/admin/models/1", `{"bench_pp_tok_per_s":-1}`, admin); resp.StatusCode != 400 {
		resp.Body.Close()
		t.Errorf("negative pp speed PATCH = %d, want 400", resp.StatusCode)
	}
	resp = e.do(t, "PATCH", "/api/admin/models/1", `{"bench_tok_per_s":70,"bench_pp_tok_per_s":900}`, admin)
	var sm store.Model
	json.NewDecoder(resp.Body).Decode(&sm)
	resp.Body.Close()
	if sm.BenchTokPerS != 70 || sm.BenchPPTokPerS != 900 || sm.BenchAt != "" {
		t.Errorf("manual speed = %v/%v/%q, want 70/900 and cleared bench_at",
			sm.BenchTokPerS, sm.BenchPPTokPerS, sm.BenchAt)
	}

	// Public catalog carries meta + benchmark for users.
	resp = e.do(t, "GET", "/api/models", "", bob)
	var cat struct {
		Models []map[string]any `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&cat)
	resp.Body.Close()
	if len(cat.Models) != 1 {
		t.Fatalf("catalog = %d models", len(cat.Models))
	}
	m := cat.Models[0]
	if m["context_length"] != float64(262144) || m["concurrency_limit"] != float64(4) {
		t.Errorf("catalog meta: %v", m)
	}
	if tp, _ := m["bench_tok_per_s"].(float64); tp <= 0 {
		t.Errorf("catalog bench_tok_per_s: %v", m["bench_tok_per_s"])
	}
	if pp, _ := m["bench_pp_tok_per_s"].(float64); pp != 900 {
		t.Errorf("catalog bench_pp_tok_per_s: %v", m["bench_pp_tok_per_s"])
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	// Keep failing until the bucket drains: each bcrypt compare takes real
	// time under -race, so the exact burst count is timing-dependent; the
	// invariant is that failures out-pace the refill and throttle kicks in.
	throttled := false
	for i := 0; i < 40; i++ {
		resp := e.do(t, "POST", "/api/auth/login", `{"username":"admin","password":"wrongpass9"}`, "")
		if resp.StatusCode == http.StatusTooManyRequests {
			if resp.Header.Get("Retry-After") == "" {
				t.Error("429 without Retry-After header")
			}
			resp.Body.Close()
			throttled = true
			break
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if !throttled {
		t.Fatal("40 login failures did not trigger the limiter")
	}
	// Even the correct password is throttled while the bucket is empty.
	resp := e.do(t, "POST", "/api/auth/login", `{"username":"admin","password":"adminpass1"}`, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("correct password while throttled: %d, want 429", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPasswordChangeInvalidatesJWT(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	resp := e.do(t, "POST", "/api/admin/users", `{"username":"bob","password":"bobpass123","role":"user"}`, admin)
	resp.Body.Close()
	bob := loginAs(t, e, "bob", "bobpass123")

	// RFC3339 second granularity: the change must land after the token's iat.
	time.Sleep(1100 * time.Millisecond)
	resp = e.do(t, "POST", "/api/auth/change_password",
		`{"old_password":"bobpass123","new_password":"bobpass456"}`, bob)
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("change password: %d %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// The pre-change token is dead.
	if resp = e.do(t, "GET", "/api/me", "", bob); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old token after password change: %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	// A fresh login works.
	if resp = e.do(t, "POST", "/api/auth/login", `{"username":"bob","password":"bobpass456"}`, ""); resp.StatusCode != 200 {
		t.Errorf("login with new password: %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPasswordLengthCap(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	long := `{"username":"big","password":"` + strings.Repeat("x", 80) + `","role":"user"}`
	if resp := e.do(t, "POST", "/api/admin/users", long, admin); resp.StatusCode != 400 {
		t.Errorf("80-byte password on create: %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// The same cap guards admin-set and self-changed passwords.
	if resp := e.do(t, "PATCH", "/api/admin/users/1", `{"password":"`+strings.Repeat("y", 80)+`"}`, admin); resp.StatusCode != 400 {
		t.Errorf("80-byte password on admin update: %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "POST", "/api/auth/change_password",
		`{"old_password":"adminpass1","new_password":"`+strings.Repeat("z", 80)+`"}`, admin); resp.StatusCode != 400 {
		t.Errorf("80-byte password on self change: %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestAuthDBErrorIs500(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	e.db.Close() // any later store read fails
	resp := e.do(t, "GET", "/api/me", "", admin)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("DB outage: %d, want 500 (not a fake 401)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLastAdminGuard(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	resp := e.do(t, "POST", "/api/admin/users", `{"username":"carol","password":"carolpass1","role":"admin"}`, admin)
	var carol store.User
	json.NewDecoder(resp.Body).Decode(&carol)
	resp.Body.Close()

	// Demoting the second admin is fine while the caller remains.
	resp = e.do(t, "PATCH", fmt.Sprintf("/api/admin/users/%d", carol.ID), `{"role":"user"}`, admin)
	if resp.StatusCode != 200 {
		t.Fatalf("demote second admin: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The caller is now the only enabled admin: self-demote is refused.
	resp = e.do(t, "GET", "/api/me", "", admin)
	var me map[string]any
	json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	self := int64(me["id"].(float64))
	resp = e.do(t, "PATCH", fmt.Sprintf("/api/admin/users/%d", self), `{"role":"user"}`, admin)
	if resp.StatusCode != 400 {
		t.Errorf("demote last admin: %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp = e.do(t, "PATCH", fmt.Sprintf("/api/admin/users/%d", self), `{"disabled":true}`, admin)
	if resp.StatusCode != 400 {
		t.Errorf("disable last admin: %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUsageInputValidation(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)

	for _, q := range []string{
		"from=2026-13-45",               // impossible date
		"from=2026-2-3",                 // unpadded
		"from=2026-05-01&to=2026-04-01", // inverted range
		"group_by=bogus",
	} {
		resp := e.do(t, "GET", "/api/usage/summary?"+q, "", admin)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("summary ?%s: %d, want 400", q, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if bytes.Contains(body, []byte("sql")) || bytes.Contains(body, []byte("SELECT")) {
			t.Errorf("summary ?%s leaked internals: %s", q, body)
		}
	}
	// Detail route shares the filter + adds pagination.
	for _, q := range []string{"page=abc", "page=99999999999999", "page_size=xyz", "from=not-a-date"} {
		if resp := e.do(t, "GET", "/api/usage/detail?"+q, "", admin); resp.StatusCode != 400 {
			t.Errorf("detail ?%s: %d, want 400", q, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}
	// Valid inputs still work.
	if resp := e.do(t, "GET", "/api/usage/summary?from=2026-01-01&to=2026-12-31&group_by=session", "", admin); resp.StatusCode != 200 {
		t.Errorf("valid summary: %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestBulkAddReportsSkipped(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	up := "http://up.example:1234"
	if _, err := store.CreateBackend(e.db, "A", up, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBackend(e.db, "B", up, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(e.db, 1, "gpt-4o", ""); err != nil {
		t.Fatal(err)
	}
	// Names are unique per backend since 0010: backend B may take gpt-4o too
	// (the import-collision fix), so both ids are created, nothing skipped.
	resp := e.do(t, "POST", "/api/admin/backends/2/models", `{"model_ids":["gpt-4o","new-m"]}`, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bulk add: %d", resp.StatusCode)
	}
	var out struct {
		Created []store.Model `json:"created"`
		Skipped []string      `json:"skipped"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Created) != 2 || out.Created[0].Name != "gpt-4o" || out.Created[1].Name != "new-m" {
		t.Errorf("created = %+v, want [gpt-4o new-m]", out.Created)
	}
	if len(out.Skipped) != 0 {
		t.Errorf("skipped = %v, want []", out.Skipped)
	}
	// Re-adding on the same backend is still reported as skipped, never
	// silently swallowed.
	resp = e.do(t, "POST", "/api/admin/backends/2/models", `{"model_ids":["new-m"]}`, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-bulk add: %d", resp.StatusCode)
	}
	out.Created = nil
	out.Skipped = nil
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Created) != 0 || len(out.Skipped) != 1 || out.Skipped[0] != "new-m" {
		t.Errorf("re-add: created = %+v skipped = %v, want []/[new-m]", out.Created, out.Skipped)
	}
	// Batch cap.
	big := `{"model_ids":[`
	for i := 0; i < 201; i++ {
		if i > 0 {
			big += ","
		}
		big += fmt.Sprintf(`"m%d"`, i)
	}
	big += `]}`
	if resp := e.do(t, "POST", "/api/admin/backends/2/models", big, admin); resp.StatusCode != 400 {
		t.Errorf("201-id batch: %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestBackendRenameGuards(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	up := "http://up.example:1234"
	if _, err := store.CreateBackend(e.db, "A", up, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBackend(e.db, "B", up, ""); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(t, "PATCH", "/api/admin/backends/2", `{"name":"A"}`, admin); resp.StatusCode != 409 {
		t.Errorf("rename onto existing: %d, want 409", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "PATCH", "/api/admin/backends/2", `{"name":"  "}`, admin); resp.StatusCode != 400 {
		t.Errorf("blank rename: %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// Valid rename still works and is trimmed.
	resp := e.do(t, "PATCH", "/api/admin/backends/2", `{"name":" B2 "}`, admin)
	var b store.Backend
	json.NewDecoder(resp.Body).Decode(&b)
	resp.Body.Close()
	if resp.StatusCode != 200 || b.Name != "B2" {
		t.Errorf("rename = %d %q, want 200 B2", resp.StatusCode, b.Name)
	}
}

func TestModelNameTrimmed(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	up := "http://up.example:1234"
	if _, err := store.CreateBackend(e.db, "A", up, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(e.db, 1, "up-m", "Plain"); err != nil {
		t.Fatal(err)
	}
	resp := e.do(t, "PATCH", "/api/admin/models/1", `{"name":" Padded "}`, admin)
	var m store.Model
	json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	if resp.StatusCode != 200 || m.Name != "Padded" {
		t.Errorf("model rename = %d %q, want 200 \"Padded\"", resp.StatusCode, m.Name)
	}
}
