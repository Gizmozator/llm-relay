package api

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
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
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	burstTokens    = 10              // allowed consecutive failures
	refillInterval = 5 * time.Second // one retry credit per interval
	maxBuckets     = 1024            // GC threshold
	gcInterval     = 10 * time.Minute
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{buckets: make(map[string]*bucket), lastGC: time.Now()}
}

// allow consumes one token for key, reporting whether the attempt may run
// and how long until the next credit when it may not.
func (l *loginLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > maxBuckets && now.Sub(l.lastGC) > gcInterval {
		for k, b := range l.buckets {
			if now.Sub(b.last) > burstTokens*refillInterval {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
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
// X-Forwarded-For is trusted here on purpose: the dashboard runs behind a
// reverse proxy, and without it every user would share one bucket.
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

// clientKey extracts the client address: first XFF entry (trusted proxy) or
// the peer host.
func clientKey(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if first, _, _ := strings.Cut(xf, ","); first != "" {
			return strings.TrimSpace(first)
		}
	}
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
