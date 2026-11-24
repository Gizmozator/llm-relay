package api

import (
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// loginLimiter is a per-client token bucket guarding the login endpoint:
// unbounded attempts mean both password guessing and bcrypt CPU amplification.
// A token is consumed on every attempt; a successful login (200) resets the
// bucket, so legitimate users are never locked out by their own activity.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	burstTokens    = 10              // allowed consecutive failures
	refillInterval = 5 * time.Second // one retry credit per interval
	maxBuckets     = 1024            // hard cap on live buckets (GC-driven)
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{buckets: make(map[string]*bucket)}
}

// allow consumes one token for key, reporting whether the attempt may run
// and how long until the next credit when it may not.
func (l *loginLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Size-driven GC. The old condition also required a minimum interval,
	// so between sweeps the map could grow far past the cap (every new
	// client address leaves a bucket). Expired buckets — idle longer than
	// the full refill window, so they can no longer hold credits — go
	// first; if live ones alone still exceed the cap, evict the
	// least-recently-seen until it fits. That makes maxBuckets a hard
	// bound: a wave of distinct addresses cannot balloon the map.
	if len(l.buckets) > maxBuckets {
		for k, b := range l.buckets {
			if now.Sub(b.last) > burstTokens*refillInterval {
				delete(l.buckets, k)
			}
		}
		// Evict down to maxBuckets-1 so the bucket this call is about to
		// create still fits: maxBuckets stays a hard bound.
		if over := len(l.buckets) - maxBuckets + 1; over > 0 {
			type keyed struct {
				key string
				b   *bucket
			}
			all := make([]keyed, 0, len(l.buckets))
			for k, b := range l.buckets {
				all = append(all, keyed{k, b})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].b.last.Before(all[j].b.last) })
			for _, e := range all[:over] {
				delete(l.buckets, e.key)
			}
		}
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: burstTokens, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() / (float64(refillInterval) / float64(time.Second))
	if b.tokens > burstTokens {
		b.tokens = burstTokens
	}
	b.last = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) * float64(refillInterval))
		return false, wait
	}
	b.tokens--
	return true, 0
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}

// RateLimitLogin wraps a login handler with the per-client limiter.
// The client is identified by the peer address only — see clientKey for
// why X-Forwarded-For is deliberately not consulted here.
func (s *Server) RateLimitLogin(next func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientKey(r)
		ok, wait := s.limiter().allow(key, time.Now())
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeErr(w, http.StatusTooManyRequests, "too many login attempts, try again later")
			return
		}
		limitRecorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(limitRecorder, r)
		if limitRecorder.status == http.StatusOK {
			s.limiter().reset(key)
		}
	})
}

// clientKey extracts the client address for the login limiter: the peer
// host only. X-Forwarded-For is deliberately NOT consulted: the admin
// listener has no trusted-proxy setting (unlike the /v1 relay side, which
// gates XFF behind TrustProxyHeaders), so any XFF header here is
// client-forgeable — trusting it let an attacker mint one fresh bucket per
// spoofed value, defeating the password-guessing/bcrypt-amplification
// guard and growing the bucket map at will. If the dashboard is ever put
// behind a reverse proxy, add a trusted-proxy setting first; do not
// re-enable XFF parsing here. (Trade-off accepted: behind an unconfigured
// proxy all users share one bucket.)
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusRecorder captures the response status so the limiter can tell a
// failed login (4xx) from a successful one (2xx).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
