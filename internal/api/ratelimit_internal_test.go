package api

import (
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
