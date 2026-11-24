package api

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"llm-relay/internal/ratelimit"
)

// The api-side abuse tiers (ticket #18) are thin wirings over
// internal/ratelimit — the bucket/eviction machinery that grew out of this
// file's original hand-rolled login limiter now lives there, shared with the
// relay mux. Identity comes from ratelimit.SourceIP: X-Forwarded-For is
// consulted ONLY when TrustProxy (TRUST_PROXY_HEADERS) is on. That gate is
// the anti-forgery rule the old clientKey comment demanded: without a
// trusted-proxy setting any XFF header is client-forgeable, and trusting it
// would let an attacker mint one fresh bucket per spoofed value and defeat
// every per-IP tier. The admin mux is the reverse-proxied listener, so the
// flag — not blind peer-IP-only keying — is now the correct trust boundary
// (see ratelimit.SourceIP for the full reasoning).

// retryAfter renders a limiter wait as the Retry-After header value.
func retryAfter(wait time.Duration) string {
	return strconv.Itoa(int(math.Ceil(wait.Seconds())))
}

// RateLimitLogin wraps the login handler with tier 1: a per-source-IP token
// bucket guarding both password guessing and the bcrypt CPU each attempt
// buys. A successful login (200) resets the bucket, so legitimate users are
// never locked out by their own activity. The per-(IP, username) failure
// backoff (tier 2) lives in Login itself — only the handler knows the
// username and which outcomes were credential failures.
func (s *Server) RateLimitLogin(next func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ratelimit.SourceIP(r, s.TrustProxy)
		ok, wait := s.loginLimiter().Allow(key, time.Now())
		if !ok {
			w.Header().Set("Retry-After", retryAfter(wait))
			writeErr(w, http.StatusTooManyRequests, "too many login attempts, try again later")
			return
		}
		limitRecorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(limitRecorder, r)
		if limitRecorder.status == http.StatusOK {
			s.loginLimiter().Reset(key)
		}
	})
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
