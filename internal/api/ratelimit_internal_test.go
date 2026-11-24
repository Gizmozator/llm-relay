package api

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginBucket(t *testing.T) {
	l := newLoginLimiter()
	t0 := time.Now()
	for i := 0; i < burstTokens; i++ {
		if ok, _ := l.allow("1.2.3.4", t0); !ok {
			t.Fatalf("attempt %d denied within burst", i+1)
		}
	}
	if ok, wait := l.allow("1.2.3.4", t0); ok {
		t.Fatal("attempt past burst must be denied")
	} else if wait <= 0 || wait > time.Duration(burstTokens)*refillInterval {
		t.Errorf("Retry-After wait = %v, want 0 < w <= %v", wait, time.Duration(burstTokens)*refillInterval)
	}
	if ok, _ := l.allow("5.6.7.8", t0); !ok {
		t.Fatal("a different client must not share the bucket")
	}
	// One refill credit costs refillInterval.
	if ok, _ := l.allow("1.2.3.4", t0.Add(refillInterval)); !ok {
		t.Fatal("expected one credit after a full refill interval")
	}
	l.reset("1.2.3.4")
	if ok, _ := l.allow("1.2.3.4", t0.Add(refillInterval)); !ok {
		t.Fatal("reset must restore the full burst")
	}
}

// Item 2: the login limiter keys on the peer address only. X-Forwarded-For
// is client-forgeable here (no trusted-proxy setting on the admin side), so
// trusting it would let an attacker mint one fresh bucket per spoofed value.
func TestClientKeyIgnoresXFF(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.7:52100"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.4")
	if got := clientKey(r); got != "192.0.2.7" {
		t.Errorf("clientKey = %q, want peer host 192.0.2.7 (XFF must be ignored)", got)
	}
	// Same peer with different forged XFF values must share one bucket.
	r2 := httptest.NewRequest("POST", "/api/auth/login", nil)
	r2.RemoteAddr = "192.0.2.7:52101"
	r2.Header.Set("X-Forwarded-For", "203.0.113.10")
	if clientKey(r) != clientKey(r2) {
		t.Error("same peer produced two bucket keys via XFF")
	}
}

// Item 3: maxBuckets is a hard bound — the GC fires on size alone (the old
// condition also required a 10-minute interval, so a wave of distinct
// addresses grew the map unboundedly between sweeps).
func TestLoginLimiterSizeBound(t *testing.T) {
	l := newLoginLimiter()
	t0 := time.Now()
	for i := 0; i < maxBuckets+500; i++ {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256), t0)
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > maxBuckets {
		t.Errorf("bucket map grew to %d, want hard cap %d", n, maxBuckets)
	}
}
