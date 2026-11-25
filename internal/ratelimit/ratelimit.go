// Package ratelimit implements the server-wide abuse/DDoS protection tiers
// (ticket #18): in-memory token buckets behind a size-capped keyed store, an
// exponential login-failure lockout, and a global in-flight shed. The relay
// is a single-instance app with no external dependencies, so every limiter
// lives in-process; the price of that is that each structure must bound its
// own memory (see Store's eviction sweep) — a limiter that grows with the
// attack is the attack.
//
// Time is injected (a now passed per call) so tests drive refill and
// lockout windows deterministically instead of sleeping on wall-clock.
package ratelimit

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StoreMax is the hard cap on live buckets per keyed store. Buckets are tiny
// (~50 B), so 64k of them is a few MB per tier — bounded memory even when
// every bucket belongs to a distinct spoofed key, while staying far above
// any legitimate deployment's distinct-client count.
const StoreMax = 65536

// Rate is one tier's token-bucket shape: a sustained rate (tokens/second)
// and a burst capacity. A non-positive field disables the tier (the "0 =
// that tier off" config escape hatch for dev/LAN deployments).
type Rate struct {
	Rate  float64
	Burst float64
}

func (r Rate) enabled() bool { return r.Rate > 0 && r.Burst > 0 }

// bucket is one keyed token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

// Store is a set of per-key token buckets with a hard size cap. Eviction
// mirrors the login limiter it grew out of: on every call that finds the map
// at or over cap, buckets idle past the full-refill window (they can no
// longer hold credits, so deleting them is semantically identical to a
// reset) go first, then least-recently-seen ones until the map fits one
// more insert — making the cap a hard bound a unique-key flood cannot
// outgrow.
//
// A single mutex guards the map. Every operation is O(1) amortized (the
// sweep only runs while over cap), and the buckets live behind HTTP handlers
// that already cost orders of magnitude more (bcrypt, DB, upstream proxy),
// so sharding would add complexity without removing a real contention
// point. Callers may share one Store across routes; keys live in one
// namespace per Store, so callers must prefix keys per identity class.
type Store struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	ttl     time.Duration // idle past this = refilled to full = evictable
	max     int
	on      bool
}

// NewStore builds a store for one tier's Rate. rate/burst <= 0 yields a
// disabled store whose Allow always passes (tier off). A non-positive max
// falls back to StoreMax: a zero/negative cap would make the eviction sweep
// fire on the first insert and could panic the LRU slice bound (target < 0),
// so an enabled store always has a sane hard bound. Mirrors NewFailureTracker.
func NewStore(r Rate, max int) *Store {
	if max <= 0 {
		max = StoreMax
	}
	s := &Store{buckets: make(map[string]*bucket), rate: r.Rate, burst: r.Burst, max: max, on: r.enabled()}
	if s.on {
		// Full refill time: after this much idle a bucket is back at burst,
		// so an evicted bucket and a fresh one behave identically.
		s.ttl = time.Duration(r.Burst/r.Rate*float64(time.Second)) + time.Second
	}
	return s
}

// Allow consumes one token for key, reporting whether the call may run and,
// when it may not, how long until the next credit (the Retry-After value).
func (s *Store) Allow(key string, now time.Time) (bool, time.Duration) {
	if !s.on {
		return true, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Evict at >= cap (not >): the sweep must leave room for the bucket
	// this call may create, or the cap would be exceeded by one on every
	// other insert.
	if len(s.buckets) >= s.max {
		s.evictLocked(now)
	}
	b := s.buckets[key]
	if b == nil {
		b = &bucket{tokens: s.burst, last: now}
		s.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * s.rate
	if b.tokens > s.burst {
		b.tokens = s.burst
	}
	b.last = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / s.rate * float64(time.Second))
		return false, wait
	}
	b.tokens--
	return true, 0
}

// Reset drops key's bucket (full credit restored): a successful login or a
// valid connect token proves this client is not the attacker, so their
// history is cleared rather than drained.
func (s *Store) Reset(key string) {
	if !s.on {
		return
	}
	s.mu.Lock()
	delete(s.buckets, key)
	s.mu.Unlock()
}

// Len reports live buckets (test hook for the size bound).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buckets)
}

// evictLocked sweeps expired buckets, then LRU-evicts down to a low-water
// mark so the bucket the current call is about to create still fits AND the
// sweep amortizes. Caller holds s.mu.
func (s *Store) evictLocked(now time.Time) {
	for k, b := range s.buckets {
		if now.Sub(b.last) > s.ttl {
			delete(s.buckets, k)
		}
	}
	if len(s.buckets) < s.max {
		return // the TTL sweep already left room for the new bucket
	}
	// Still at/over cap after the TTL sweep: LRU-evict down to ~90% of max,
	// NOT just one slot below it. Evicting a single slot would make Allow
	// re-sweep on EVERY subsequent new-key insert while the store sits pinned
	// at the cap — an O(N) map scan plus an O(N log N) sort of ~StoreMax keys
	// under the mutex per request — so a unique-key flood (spoofed XFF, or
	// many valid keys) collapses latency even though memory stays bounded:
	// the "limiter that grows with the attack" the package doc forbids.
	// Dropping ~10% amortizes one sweep across the next ~max/10 inserts.
	// The target is capped at max-1 so the hard bound still holds for small
	// max (where max/10 rounds to 0).
	target := s.max - s.max/10
	if target >= s.max {
		target = s.max - 1
	}
	if over := len(s.buckets) - target; over > 0 {
		all := make([]string, 0, len(s.buckets))
		for k := range s.buckets {
			all = append(all, k)
		}
		sort.Slice(all, func(i, j int) bool {
			return s.buckets[all[i]].last.Before(s.buckets[all[j]].last)
		})
		for _, k := range all[:over] {
			delete(s.buckets, k)
		}
	}
}

// ---------------------------------------------------------------- lockout

// Lockout configures the per-(source, username) login-failure backoff:
// after Threshold consecutive failures the pair is locked out for Base,
// doubling per additional failure up to Max; a pair idle past IdleTTL
// forgets its streak (a typo yesterday must not escalate today).
// Threshold/Base <= 0 disables the tracker.
type Lockout struct {
	Threshold  int
	Base       time.Duration
	Max        time.Duration
	IdleTTL    time.Duration
	MaxEntries int
}

func (c Lockout) enabled() bool { return c.Threshold > 0 && c.Base > 0 }

type failEntry struct {
	fails     int
	lockUntil time.Time
	last      time.Time
}

// FailureTracker records consecutive authentication failures per key and
// reports exponential lockouts. Like Store it is size-capped: the key space
// here is (IP, username) and an attacker enumerating usernames must not be
// able to grow the tracker without bound.
type FailureTracker struct {
	mu  sync.Mutex
	m   map[string]*failEntry
	cfg Lockout
}

func NewFailureTracker(cfg Lockout) *FailureTracker {
	// A non-positive cap would make the eviction sweep fire on the first
	// insert (degenerating to a ~1-entry tracker that forgets real streaks).
	// No shipped config does this, but harden the constructor so a zero
	// MaxEntries falls back to the same bound the keyed stores use.
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = StoreMax
	}
	return &FailureTracker{m: make(map[string]*failEntry), cfg: cfg}
}

// Locked reports whether key is currently locked out and how long remains
// (the Retry-After value). Observing an expired window resets the streak:
// after serving a lockout the pair starts clean, so escalation reflects a
// sustained attack, not a history of separate incidents.
func (f *FailureTracker) Locked(key string, now time.Time) (bool, time.Duration) {
	if !f.cfg.enabled() {
		return false, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.m[key]
	if e == nil {
		return false, 0
	}
	if !e.lockUntil.IsZero() {
		if now.Before(e.lockUntil) {
			return true, e.lockUntil.Sub(now)
		}
		e.fails, e.lockUntil = 0, time.Time{}
	}
	return false, 0
}

// Record notes one authentication failure for key and returns the lockout
// it triggered (0 when the failure did not lock the pair).
func (f *FailureTracker) Record(key string, now time.Time) time.Duration {
	if !f.cfg.enabled() {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.m) >= f.cfg.MaxEntries {
		f.evictLocked(now)
	}
	e := f.m[key]
	if e == nil {
		e = &failEntry{}
		f.m[key] = e
	}
	if now.Sub(e.last) > f.cfg.IdleTTL {
		e.fails, e.lockUntil = 0, time.Time{}
	}
	e.last = now
	e.fails++
	if e.fails < f.cfg.Threshold {
		return 0
	}
	dur := f.cfg.Base
	for i := e.fails - f.cfg.Threshold; i > 0 && dur < f.cfg.Max; i-- {
		dur *= 2
	}
	if dur > f.cfg.Max {
		dur = f.cfg.Max
	}
	e.lockUntil = now.Add(dur)
	return dur
}

// Clear forgets key's streak (a successful login).
func (f *FailureTracker) Clear(key string) {
	if !f.cfg.enabled() {
		return
	}
	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
}

// Len reports live entries (test hook for the size bound).
func (f *FailureTracker) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}

func (f *FailureTracker) evictLocked(now time.Time) {
	for k, e := range f.m {
		if now.Sub(e.last) > f.cfg.IdleTTL && (e.lockUntil.IsZero() || !now.Before(e.lockUntil)) {
			delete(f.m, k)
		}
	}
	if over := len(f.m) - f.cfg.MaxEntries + 1; over > 0 {
		// Still over cap: evict least-recently-seen regardless of lock
		// state — the cap is the hard bound. A flood fast enough to evict
		// live lockouts is itself throttled by the per-IP tier long before
		// it reaches this many entries.
		all := make([]string, 0, len(f.m))
		for k := range f.m {
			all = append(all, k)
		}
		sort.Slice(all, func(i, j int) bool {
			return f.m[all[i]].last.Before(f.m[all[j]].last)
		})
		for _, k := range all[:over] {
			delete(f.m, k)
		}
	}
}

// ---------------------------------------------------------------- shed

// Shed is the server-wide in-flight cap: the last-resort bound on total
// concurrent work. Long LLM streams and live WS channels legitimately hold
// a slot for their whole duration, so the cap must sit far above normal
// concurrency; it exists to fail fast (503) instead of melting when
// something floods the box.
type Shed struct {
	cur atomic.Int64
	max int64
}

func NewShed(max int64) *Shed { return &Shed{max: max} }

// Enter claims an in-flight slot, reporting false when saturated.
func (s *Shed) Enter() bool {
	if s == nil || s.max <= 0 {
		return true
	}
	if s.cur.Add(1) > s.max {
		s.cur.Add(-1)
		return false
	}
	return true
}

// Leave releases a slot. Handlers must call it via defer immediately after
// a successful Enter so a panicking or hijacked handler cannot leak counts.
func (s *Shed) Leave() {
	if s == nil || s.max <= 0 {
		return
	}
	s.cur.Add(-1)
}

// InFlight reports the current count (test/observability hook).
func (s *Shed) InFlight() int64 { return s.cur.Load() }

// ---------------------------------------------------------------- HTTP

// writeJSONError mirrors the api/proxy error envelope
// ({"error":{"message":…}}) so limited clients see one shape.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}

// retryAfter renders a wait as the Retry-After header (whole seconds,
// rounded up — a client that retries early just burns another bucket
// credit).
func retryAfter(wait time.Duration) string {
	return strconv.Itoa(int(math.Ceil(wait.Seconds())))
}

// Middleware wraps handlers with a Store: the key function extracts the
// bucket identity from the request; denials answer 429 + Retry-After.
func Middleware(s *Store, key func(*http.Request) string, msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, wait := s.Allow(key(r), time.Now())
			if !ok {
				w.Header().Set("Retry-After", retryAfter(wait))
				writeJSONError(w, http.StatusTooManyRequests, msg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// PathPrefix applies wrap only to requests whose path starts with one of
// prefixes; everything else reaches next untouched. Used to scope the /v1
// per-IP tier to the relay surface without touching /healthz or the
// machine-channel endpoints (which carry their own stricter buckets).
func PathPrefix(prefixes []string, wrap func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		limited := wrap(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, p := range prefixes {
				if strings.HasPrefix(r.URL.Path, p) {
					limited.ServeHTTP(w, r)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Middleware sheds saturated servers with 503 + Retry-After. The slot is
// released by defer so every completion path — normal write, panic, hijacked
// WebSocket, finished SSE stream — decrements exactly once.
func (s *Shed) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Enter() {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusServiceUnavailable, "server at capacity, try again shortly")
			return
		}
		defer s.Leave()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- identity

// SourceIP resolves the client address a per-IP tier keys on.
//
// X-Forwarded-For is consulted ONLY when trust is on (TRUST_PROXY_HEADERS):
// without that gate an attacker mints one fresh bucket per spoofed header
// value and every per-IP tier evaporates. When trust is on the RIGHTMOST
// entry wins — everything left of it was appended by clients or earlier
// hops and is spoofable; the rightmost entry was appended by our own
// reverse proxy (the same rule the proxy's metering uses).
//
// The trust gate here is TrustProxy alone, deliberately NOT the narrower
// TrustProxy && !SelfTLS gate the X-Forwarded-Proto TLS check uses: that
// gate guards a TLS bypass (a relay bound to its own TLS port has no proxy
// in front of its machine channels), while IP identity follows the
// codebase-wide rule that TRUST_PROXY_HEADERS means "every request arrives
// through my proxy" — including the plain relay port of a relay that also
// binds its own TLS listener for machine channels. Requiring !SelfTLS here
// would silently collapse every proxied client onto the proxy's IP, which
// is exactly the mass-lockout the flag exists to prevent.
func SourceIP(r *http.Request, trust bool) string {
	if trust {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			parts := strings.Split(xf, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
