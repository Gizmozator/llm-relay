// API-level tests: login, keys, admin backend discovery flow, usage scoping.
package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm-relay/internal/api"
	"llm-relay/internal/auth"
	"llm-relay/internal/db"
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
	mux.HandleFunc("POST /api/auth/login", s.Login)
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
	mux.Handle("POST /api/admin/users", s.Admin(s.AdminCreateUser))
	mux.Handle("GET /api/admin/usage/detail", s.Admin(s.UsageDetail))
	mux.Handle("POST /api/admin/backends", s.Admin(s.AdminCreateBackend))
	mux.Handle("POST /api/admin/backends/{id}/probe", s.Admin(s.AdminProbeBackend))
	mux.Handle("POST /api/admin/backends/{id}/models", s.Admin(s.AdminAddBackendModels))
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
	if _, err := store.UpdateUser(e.db, bobID, nil, ptrTrue(), nil, nil); err != nil {
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
	if _, err := store.UpdateBackend(e.db, 2, nil, nil, nil, ptrB(false)); err != nil {
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
}
