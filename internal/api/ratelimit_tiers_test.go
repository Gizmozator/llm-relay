// Tier tests for the api-side wirings (ticket #18): the login per-IP bucket
// (T1), the per-(IP,username) failure lockout (T2) and the admin per-user
// bucket (T4) driven through the real handlers. The bucket math, eviction
// bound and XFF trust primitive moved to internal/ratelimit's own tests
// (TestBucketMath, TestStoreEvictionBound, TestSourceIPXFFTrust) when the
// machinery was generalized out of this package — this file keeps the
// end-to-end behaviour the old ratelimit_internal_test.go covered and adds
// the new tiers.
package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"llm-relay/internal/api"
	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/ratelimit"
	"llm-relay/internal/store"
)

// loginEnv is a minimal login-only deployment with caller-chosen tier limits.
func loginEnv(t *testing.T, limits *ratelimit.Config, trustProxy bool) (*api.Server, *httptest.Server) {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/rl.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	hash, _ := auth.HashPassword("goodpass123")
	if _, err := store.CreateUser(d, "alice", "", hash, "user"); err != nil {
		t.Fatal(err)
	}
	s := &api.Server{DB: d, JWTSecret: "test-secret-0123456789abcdefghij", JWTTTL: time.Hour,
		TrustProxy: trustProxy, Limits: limits}
	mux := http.NewServeMux()
	mux.Handle("POST /api/auth/login", s.RateLimitLogin(s.Login))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func tryLogin(t *testing.T, srv *httptest.Server, user, pass, xff string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/api/auth/login",
		bytes.NewReader([]byte(`{"username":"`+user+`","password":"`+pass+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// fastLock is a lockout config with a short window so the recovery leg of
// the test does not sleep on the production 30 s base.
func fastLock() *ratelimit.Config {
	cfg := ratelimit.DefaultConfig()
	cfg.Lockout.Threshold = 3
	cfg.Lockout.Base = 300 * time.Millisecond
	return &cfg
}

// T2: N consecutive failures lock the (IP, username) pair with 429 +
// Retry-After; the correct password is refused while locked and works once
// the window passes.
func TestLoginLockoutWindow(t *testing.T) {
	_, srv := loginEnv(t, fastLock(), false)
	for i := 0; i < 3; i++ {
		resp := tryLogin(t, srv, "alice", "wrongpass", "")
		if resp.StatusCode != http.StatusUnauthorized {
			resp.Body.Close()
			t.Fatalf("failure %d = %d, want 401", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := tryLogin(t, srv, "alice", "wrongpass", "")
	if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("post-threshold attempt = %d, want 429", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	resp.Body.Close()
	if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want integer >= 1", ra)
	}
	// The correct password is locked out too — the window guards the
	// account, not the guesser's mood.
	resp = tryLogin(t, srv, "alice", "goodpass123", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct password while locked = %d, want 429", resp.StatusCode)
	}
	// A different username is a different pair: the lockout must not
	// collateral-damage the whole box.
	resp = tryLogin(t, srv, "mallory", "whatever", "")
	resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("lockout leaked across usernames")
	}
	// After the window the good password works again.
	time.Sleep(450 * time.Millisecond)
	resp = tryLogin(t, srv, "alice", "goodpass123", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good password after lockout window = %d, want 200", resp.StatusCode)
	}
}

// T1 + the XFF trust rule at the HTTP level: with TRUST_PROXY_HEADERS off
// (the default), rotating XFF values must NOT mint fresh buckets — the
// attacker sharing one source address shares one bucket no matter what
// header they forge.
func TestLoginXFFIgnoredWithoutTrust(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.Login = ratelimit.Rate{Rate: 0.001, Burst: 3} // 3 credits, no refill
	_, srv := loginEnv(t, &cfg, false)
	for i := 0; i < 3; i++ {
		resp := tryLogin(t, srv, "alice", "wrongpass", "203.0.113."+strconv.Itoa(i+1))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("attempt %d inside burst got 429", i+1)
		}
	}
	resp := tryLogin(t, srv, "alice", "wrongpass", "198.51.100.77")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th attempt with a fresh forged XFF = %d, want 429 "+
			"(untrusted XFF must not mint a new bucket)", resp.StatusCode)
	}
}

// With trust ON (the reverse-proxied deployment) distinct XFF clients get
// distinct buckets — that is what makes per-IP limits meaningful there.
func TestLoginXFFHonoredWithTrust(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.Login = ratelimit.Rate{Rate: 0.001, Burst: 3}
	_, srv := loginEnv(t, &cfg, true)
	for i := 0; i < 3; i++ {
		resp := tryLogin(t, srv, "alice", "wrongpass", "203.0.113.1")
		resp.Body.Close()
	}
	// Same proxy peer, different trusted client IP: fresh bucket.
	resp := tryLogin(t, srv, "alice", "wrongpass", "203.0.113.2")
	resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("trusted XFF clients shared one bucket")
	}
	// The drained client stays drained behind its own address.
	resp = tryLogin(t, srv, "alice", "wrongpass", "203.0.113.1")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("drained trusted client = %d, want 429", resp.StatusCode)
	}
}

// T4: the admin API throttles per user (not per socket): user A draining
// the bucket does not lock out user B, and the denial carries Retry-After.
func TestAdminPerUserLimit(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/adm.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tok := func(user, pass string) string {
		h, _ := auth.HashPassword(pass)
		u, err := store.CreateUser(d, user, "", h, "user")
		if err != nil {
			t.Fatal(err)
		}
		tk, err := auth.SignToken("test-secret-0123456789abcdefghij", u.ID, u.Role, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	alice, bob := tok("alice", "pass-alice-1"), tok("bob", "pass-bob-1")

	cfg := ratelimit.DefaultConfig()
	cfg.Admin = ratelimit.Rate{Rate: 0.001, Burst: 2} // 2 credits per user
	s := &api.Server{DB: d, JWTSecret: "test-secret-0123456789abcdefghij", JWTTTL: time.Hour, Limits: &cfg}
	mux := http.NewServeMux()
	mux.Handle("GET /api/me", s.Auth(s.Me))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	me := func(jwt string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for i := 0; i < 2; i++ {
		resp := me(alice)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("alice request %d = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp := me(alice)
	if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("alice past burst = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	resp.Body.Close()
	resp = me(bob)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bob (different user) = %d, want 200 — buckets must be per user", resp.StatusCode)
	}
	var body struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Username != "bob" {
		t.Errorf("body = %+v", body)
	}
}
