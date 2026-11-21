// Health cache internals: TTL reuse, re-probe on expiry, disabled backends,
// and the Reachable status rules (401 = online, 503 = offline).
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm-relay/internal/db"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

func TestHealthCacheStatus(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	b, err := store.CreateBackend(d, "eng", up.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d}
	ctx := context.Background()

	st := s.healthStatus().Status(ctx, []store.Backend{*b})
	if !st[b.ID].Online {
		t.Fatalf("backend should be online: %+v", st[b.ID])
	}

	// Backend dies, but the cached result stands until the TTL expires.
	up.Close()
	st = s.healthStatus().Status(ctx, []store.Backend{*b})
	if !st[b.ID].Online {
		t.Fatal("cached result should still say online")
	}

	// TTL expiry forces a re-probe, which now fails.
	old := proxy.HealthTTL
	proxy.HealthTTL = 0
	defer func() { proxy.HealthTTL = old }()
	st = s.healthStatus().Status(ctx, []store.Backend{*b})
	if st[b.ID].Online {
		t.Fatal("stale entry should have been re-probed to offline")
	}

	// Disabled backends are offline without a probe.
	disabled := false
	if _, err := store.UpdateBackend(d, b.ID, nil, nil, nil, &disabled); err != nil {
		t.Fatal(err)
	}
	b.Enabled = false
	st = s.healthStatus().Status(ctx, []store.Backend{*b})
	if st[b.ID].Online || st[b.ID].Err != "backend disabled" {
		t.Fatalf("disabled backend: %+v", st[b.ID])
	}
}

func TestHealthProbeSurvivesViewerCancel(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/hc.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	b, err := store.CreateBackend(d, "eng", up.URL, "")
	if err != nil {
		t.Fatal(err)
	}

	// The viewer's context is already canceled (tab closed mid-request):
	// the probe must not inherit it, or a false offline gets cached for
	// every viewer for the whole TTL.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{DB: d}
	st := s.healthStatus().Status(ctx, []store.Backend{*b})
	if !st[b.ID].Online {
		t.Fatalf("probe must not inherit the viewer's canceled context: %+v", st[b.ID])
	}
}

func TestReachableStatusRules(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		status int
		online bool
	}{
		{http.StatusOK, true},
		{http.StatusUnauthorized, true}, // keyed but alive
		{http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		err := proxy.Reachable(ctx, up.URL)
		up.Close()
		if (err == nil) != tc.online {
			t.Errorf("status %d: online=%v, want %v (err=%v)", tc.status, err == nil, tc.online, err)
		}
	}
}
