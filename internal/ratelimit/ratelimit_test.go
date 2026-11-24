package ratelimit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// ok is a trivial pass-through handler for middleware tests.
func ok(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// ---------------------------------------------------------------- bucket

// Token-bucket math with an injected clock: burst consumption, denial with
// a sane Retry-After wait, linear refill, burst ceiling, reset.
func TestBucketMath(t *testing.T) {
	s := NewStore(Rate{Rate: 0.2, Burst: 10}, 100)
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		if allowed, _ := s.Allow("k", t0); !allowed {
			t.Fatalf("attempt %d denied inside burst", i+1)
		}
	}
	allowed, wait := s.Allow("k", t0)
	if allowed {
		t.Fatal("attempt past burst must be denied")
	}
	if wait <= 0 || wait > 10*time.Duration(10)*time.Second {
		t.Errorf("wait = %v, want 0 < w <= full-refill window", wait)
	}
	// A different key has its own bucket.
	if allowed, _ := s.Allow("other", t0); !allowed {
		t.Fatal("distinct keys must not share a bucket")
	}
	// One credit costs exactly one refill interval (1/rate = 5 s).
	if allowed, _ := s.Allow("k", t0.Add(5*time.Second)); !allowed {
		t.Fatal("one credit expected after a full refill interval")
	}
	// Refill is capped at burst: an hour of idle cannot bank more than 10.
	s.Reset("k")
	if allowed, _ := s.Allow("k", t0.Add(time.Hour)); !allowed {
		t.Fatal("reset must restore the burst")
	}
	// Drain the restored burst and check the ceiling: after a long idle
	// exactly burst more attempts pass, no more.
	s2 := NewStore(Rate{Rate: 1, Burst: 3}, 100)
	s2.Allow("k", t0)
	s2.Allow("k", t0.Add(time.Hour)) // refills to burst=3, consumes 1 → 2 left
	for i := 0; i < 2; i++ {
		if allowed, _ := s2.Allow("k", t0.Add(time.Hour)); !allowed {
			t.Fatalf("post-idle attempt %d: burst ceiling wrong", i+1)
		}
	}
	if allowed, _ := s2.Allow("k", t0.Add(time.Hour)); allowed {
		t.Fatal("idle must not bank more than burst credits")
	}
}

// The store is the DoS-proofing: a flood of unique keys must not grow the
// map past the cap (mutation M1 target: removing the cap fails this).
func TestStoreEvictionBound(t *testing.T) {
	const cap = 500
	s := NewStore(Rate{Rate: 1000, Burst: 1000}, cap)
	t0 := time.Now()
	for i := 0; i < 3*cap+100; i++ {
		s.Allow(fmt.Sprintf("ip-%d", i), t0)
		if n := s.Len(); n > cap {
			t.Fatalf("bucket map grew to %d past cap %d at flood step %d", n, cap, i)
		}
	}
	if n := s.Len(); n > cap {
		t.Errorf("final size %d > cap %d", n, cap)
	}
}

// Idle buckets (idle past the full-refill window, so they can no longer
// hold credits) are swept on the next over-cap event.
func TestStoreTTLExpiry(t *testing.T) {
	const cap = 50
	s := NewStore(Rate{Rate: 1, Burst: 10}, cap) // ttl ≈ 11 s
	t0 := time.Now()
	for i := 0; i < cap; i++ {
		s.Allow(fmt.Sprintf("old-%d", i), t0)
	}
	// Long after the TTL, a fresh flood evicts every stale bucket during
	// the sweep — the map must not need LRU at all.
	for i := 0; i < cap+10; i++ {
		s.Allow(fmt.Sprintf("new-%d", i), t0.Add(time.Hour))
	}
	if n := s.Len(); n > cap {
		t.Errorf("size %d > cap %d after TTL sweep", n, cap)
	}
}

// ---------------------------------------------------------------- lockout

// Tier 2: N consecutive failures lock the pair, the window doubles per
// extra failure up to the cap, Retry-After counts down, success clears,
// and an expired window resets the streak.
func TestFailureLockout(t *testing.T) {
	f := NewFailureTracker(Lockout{
		Threshold: 5, Base: 30 * time.Second, Max: 15 * time.Minute,
		IdleTTL: time.Hour, MaxEntries: 1000,
	})
	t0 := time.Now()
	pair := "1.2.3.4|admin"
	for i := 1; i < 5; i++ {
		if d := f.Record(pair, t0); d != 0 {
			t.Fatalf("failure %d must not lock (threshold 5), locked for %v", i, d)
		}
		if locked, _ := f.Locked(pair, t0); locked {
			t.Fatalf("locked before threshold at failure %d", i)
		}
	}
	if d := f.Record(pair, t0); d != 30*time.Second {
		t.Fatalf("5th failure lockout = %v, want 30s", d)
	}
	locked, rem := f.Locked(pair, t0)
	if !locked || rem != 30*time.Second {
		t.Fatalf("locked=%v rem=%v, want true/30s", locked, rem)
	}
	// Countdown: 10 s later 20 s remain.
	if _, rem := f.Locked(pair, t0.Add(10*time.Second)); rem != 20*time.Second {
		t.Errorf("countdown rem = %v, want 20s", rem)
	}
	// Exponential: 6th failure → 60 s, 7th → 120 s, capped at 15 min.
	if d := f.Record(pair, t0); d != 60*time.Second {
		t.Errorf("6th failure lockout = %v, want 60s", d)
	}
	if d := f.Record(pair, t0); d != 120*time.Second {
		t.Errorf("7th failure lockout = %v, want 120s", d)
	}
	for i := 0; i < 20; i++ {
		d := f.Record(pair, t0)
		if d > 15*time.Minute {
			t.Fatalf("lockout %v exceeds the 15m cap", d)
		}
	}
	// Success clears the streak entirely.
	f.Clear(pair)
	if locked, _ := f.Locked(pair, t0); locked {
		t.Fatal("clear must end the lockout")
	}
	if d := f.Record(pair, t0); d != 0 {
		t.Fatalf("post-clear failure must restart the streak at 0, got lock %v", d)
	}
	// An expired window resets the streak: after serving one lockout the
	// pair needs the full threshold again, and escalation restarts.
	g := NewFailureTracker(Lockout{Threshold: 2, Base: 10 * time.Second, Max: time.Minute, IdleTTL: time.Hour, MaxEntries: 100})
	g.Record("p", t0)
	if d := g.Record("p", t0); d != 10*time.Second {
		t.Fatalf("lock = %v, want 10s", d)
	}
	if locked, _ := g.Locked("p", t0.Add(11*time.Second)); locked {
		t.Fatal("window must have expired")
	}
	if d := g.Record("p", t0.Add(11*time.Second)); d != 0 {
		t.Fatalf("post-expiry failure must restart the streak, got lock %v", d)
	}
}

// The (IP, username) key space is attacker-chosen (username enumeration),
// so the tracker must be size-capped like the bucket store.
func TestFailureTrackerBound(t *testing.T) {
	const cap = 300
	f := NewFailureTracker(Lockout{Threshold: 5, Base: time.Minute, Max: time.Hour, IdleTTL: time.Hour, MaxEntries: cap})
	t0 := time.Now()
	for i := 0; i < 3*cap+50; i++ {
		f.Record(fmt.Sprintf("u%d", i), t0)
		if n := f.Len(); n > cap {
			t.Fatalf("tracker grew to %d past cap %d", n, cap)
		}
	}
}

// A disabled tracker never locks (0 = tier off).
func TestLockoutDisabled(t *testing.T) {
	f := NewFailureTracker(Lockout{Threshold: 0, Base: 0})
	for i := 0; i < 100; i++ {
		if d := f.Record("k", time.Now()); d != 0 {
			t.Fatal("disabled tracker must never lock")
		}
	}
}

// ---------------------------------------------------------------- shed

// Tier 6: saturation sheds with 503 + Retry-After, and the in-flight count
// decrements on completion so the shed recovers (mutation M4 target: a
// shed that never decrements fails the recovery leg).
func TestShed(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(200)
	})
	shed := NewShed(2)
	srv := httptest.NewServer(shed.Middleware(handler))
	defer srv.Close()

	// Two slow requests fill the shed.
	for i := 0; i < 2; i++ {
		go func() { _, _ = http.Get(srv.URL) }()
		<-entered
	}
	if shed.InFlight() != 2 {
		t.Fatalf("in-flight = %d, want 2", shed.InFlight())
	}
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("saturated request = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 without Retry-After")
	}
	resp.Body.Close()
	// Release both: the count must return to zero and traffic flow again.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for shed.InFlight() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if shed.InFlight() != 0 {
		t.Fatalf("in-flight leaked: %d after release", shed.InFlight())
	}
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("post-recovery request = %d, want 200", resp.StatusCode)
	}
}

func TestShedDisabled(t *testing.T) {
	shed := NewShed(0)
	if !shed.Enter() || !shed.Enter() || !shed.Enter() {
		t.Fatal("max<=0 must disable the shed")
	}
}

// ---------------------------------------------------------------- middleware

// A denied tier answers 429 with an integer Retry-After and the standard
// error envelope (mutation M3 target: dropping Retry-After fails this).
func TestMiddlewareDenies(t *testing.T) {
	s := NewStore(Rate{Rate: 0.001, Burst: 1}, StoreMax) // one credit, glacial refill
	mw := Middleware(s, func(r *http.Request) string { return SourceIP(r, false) }, "slow down")
	srv := httptest.NewServer(mw(ok(t)))
	defer srv.Close()

	req := func() *http.Response {
		r, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	resp := req()
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first request = %d, want 200", resp.StatusCode)
	}
	resp = req()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want integer >= 1", ra)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error.Message != "slow down" {
		t.Errorf("body = %+v, want the tier message", body)
	}
}

// A disabled tier (rate or burst 0) passes everything (dev/LAN escape).
func TestMiddlewareDisabled(t *testing.T) {
	for _, r := range []Rate{{Rate: 0, Burst: 10}, {Rate: 10, Burst: 0}, {-1, -1}} {
		s := NewStore(r, StoreMax)
		srv := httptest.NewServer(Middleware(s, func(*http.Request) string { return "x" }, "nope")(ok(t)))
		for i := 0; i < 50; i++ {
			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("disabled tier (%+v) denied request %d", r, i+1)
			}
		}
		srv.Close()
	}
}

// PathPrefix applies the tier only under its prefixes.
func TestPathPrefixScoping(t *testing.T) {
	s := NewStore(Rate{Rate: 0.001, Burst: 1}, StoreMax)
	mw := Middleware(s, func(*http.Request) string { return "all" }, "limited")
	handler := PathPrefix([]string{"/v1/"}, mw)(ok(t))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/v1/chat/completions", 200}, // one shared bucket: first /v1 request spends the credit
		{"/healthz", 200},             // outside the prefix: untouched
		{"/v1/models", 429},           // second /v1 request hits the wall
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}
}

// ---------------------------------------------------------------- identity

// The XFF trust rule (mutation M2 target): with trust OFF the peer address
// is the only identity — rotating XFF values must NOT mint fresh buckets
// (an attacker resetting their bucket by header spoofing defeats every
// per-IP tier). With trust ON the rightmost XFF entry — the one appended
// by our own reverse proxy — is the client.
func TestSourceIPXFFTrust(t *testing.T) {
	newReq := func(xff string) *http.Request {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.RemoteAddr = "192.0.2.7:52100"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	// Trust off: peer host wins, forged headers are ignored entirely.
	if got := SourceIP(newReq("203.0.113.9, 198.51.100.4"), false); got != "192.0.2.7" {
		t.Errorf("trust off: got %q, want peer 192.0.2.7", got)
	}
	if SourceIP(newReq("203.0.113.1"), false) != SourceIP(newReq("203.0.113.2"), false) {
		t.Error("trust off: rotating XFF minted two identities for one peer")
	}
	// Trust on: rightmost entry (appended by our proxy) is the client;
	// the client-forged left part is ignored.
	if got := SourceIP(newReq("203.0.113.9, 198.51.100.4"), true); got != "198.51.100.4" {
		t.Errorf("trust on: got %q, want rightmost 198.51.100.4", got)
	}
	if got := SourceIP(newReq("198.51.100.4"), true); got != "198.51.100.4" {
		t.Errorf("trust on single entry: got %q", got)
	}
	// Trust on but no header (direct connection): peer host again.
	if got := SourceIP(newReq(""), true); got != "192.0.2.7" {
		t.Errorf("trust on, no XFF: got %q, want peer", got)
	}
	// Bare RemoteAddr without a port must survive SplitHostPort failure.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5"
	if got := SourceIP(r, false); got != "203.0.113.5" {
		t.Errorf("portless RemoteAddr: got %q", got)
	}
}

// ---------------------------------------------------------------- defaults

// A default-configured deployment must wave normal traffic through every
// tier untouched (law 3). Driven with a fake clock at honest human/client
// rates over a simulated hour: zero denials anywhere.
func TestNormalTrafficPassesDefaults(t *testing.T) {
	cfg := DefaultConfig()
	login := NewStore(cfg.Login, StoreMax)
	connect := NewStore(cfg.Connect, StoreMax)
	v1ip := NewStore(cfg.V1IP, StoreMax)
	v1key := NewStore(cfg.V1Key, StoreMax)
	admin := NewStore(cfg.Admin, StoreMax)
	lock := NewFailureTracker(cfg.Lockout)
	shed := NewShed(cfg.GlobalMax)

	t0 := time.Now()
	denials := 0
	deny := func(name string, ok bool) {
		if !ok {
			denials++
			t.Errorf("default tiers denied normal traffic: %s", name)
		}
	}
	// Simulate one hour, one second per tick: a dashboard user polling
	// ~2 req/s, an API client at 5 req/s on one key from one IP, a
	// handful of logins (two typos, then success), one connector reconnect
	// per minute, and 20 concurrent in-flight requests.
	for sec := 0; sec < 3600; sec++ {
		now := t0.Add(time.Duration(sec) * time.Second)
		for i := 0; i < 2; i++ {
			deny("admin poll", func() bool { ok, _ := admin.Allow("u:1", now); return ok }())
		}
		for i := 0; i < 5; i++ {
			deny("v1 ip", func() bool { ok, _ := v1ip.Allow("203.0.113.7", now); return ok }())
			deny("v1 key", func() bool { ok, _ := v1key.Allow("key:42", now); return ok }())
		}
		if sec%60 == 0 {
			deny("connect", func() bool { ok, _ := connect.Allow("198.51.100.9", now); return ok }())
		}
		if sec%300 == 0 {
			deny("login", func() bool { ok, _ := login.Allow("198.51.100.2", now); return ok }())
		}
		if sec%300 == 1 {
			lock.Record("198.51.100.2|admin", now)
			lock.Record("198.51.100.2|admin", now)
			locked, _ := lock.Locked("198.51.100.2|admin", now)
			if locked {
				t.Fatal("two typos must not lock out")
			}
			lock.Clear("198.51.100.2|admin")
		}
		for i := 0; i < 20; i++ {
			if !shed.Enter() {
				t.Fatal("shed denied normal concurrency")
			}
		}
		for i := 0; i < 20; i++ {
			shed.Leave()
		}
	}
	if denials != 0 {
		t.Fatalf("%d denials under normal traffic", denials)
	}
}
