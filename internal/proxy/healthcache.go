// Backend health cache: probes reachability at most once per TTL and shares
// the result across all consumers (dashboard viewers, agent endpoints).
// Lives here so the proxy's agent-facing endpoints and the dashboard API
// answer "is it online" with exactly the same rules.
package proxy

import (
	"context"
	"sync"
	"time"

	"llm-relay/internal/store"
)

// HealthTTL is how long a backend reachability result is reused. Models do
// not flap on/off constantly, so one probe per minute per backend is enough
// even though the dashboard polls every few seconds (owner decision).
var HealthTTL = 60 * time.Second

// HealthEntry is one cached probe result.
type HealthEntry struct {
	Online bool
	Err    string
	At     time.Time
}

// HealthCache probes backends at most once per TTL and shares the result
// across the consumers (dashboard viewers, agent endpoints). Probes run
// OUTSIDE the mutex (a 2 s timeout on one dead backend must not stall
// every other caller's cache reads); stampede dedupe is explicit via the
// inflight map — concurrent callers for the same stale backend share one
// probe.
type HealthCache struct {
	mu       sync.Mutex
	entries  map[int64]HealthEntry
	inflight map[int64]chan struct{}
	// Synthetic answers "is this federation backend reachable" for
	// peer://hub:// backends, which have no HTTP endpoint to probe: the
	// fed.Manager wires it to the channel registry (channel online =
	// healthy). nil = treat synthetic backends as online (materialization
	// already disables the backend when its channel dies).
	Synthetic func(b store.Backend) bool
}

func NewHealthCache() *HealthCache {
	return &HealthCache{entries: map[int64]HealthEntry{}, inflight: map[int64]chan struct{}{}}
}

// Status returns per-backend reachability, probing only entries older than
// the TTL. Disabled backends are offline without a probe.
func (hc *HealthCache) Status(ctx context.Context, backends []store.Backend) map[int64]HealthEntry {
	// Fast pass under the lock: everything answerable from cache or from
	// cheap local state. Stale entries are collected and probed after the
	// lock is released.
	out := make(map[int64]HealthEntry, len(backends))
	var stale []store.Backend
	// entries is intentionally never pruned: keys are backend IDs, bounded
	// by the number of backends ever created (tiny on a homelab), and a
	// stale entry for a deleted ID is simply never read again.
	hc.mu.Lock()
	for _, b := range backends {
		e, fresh := hc.entries[b.ID], hc.entries[b.ID].At.After(time.Now().Add(-HealthTTL))
		switch {
		case !b.Enabled:
			e = HealthEntry{Online: false, Err: "backend disabled", At: time.Now()}
		case isSyntheticURL(b.BaseURL):
			// A federation backend has no HTTP endpoint: its health IS the
			// channel. Never probed, never cached (the answer is a map
			// lookup against the live registry).
			online := true
			if hc.Synthetic != nil {
				online = hc.Synthetic(b)
			}
			e = HealthEntry{Online: online, At: time.Now()}
			if !online {
				e.Err = "federation channel offline"
			}
		case fresh:
			// cached result stands
		default:
			stale = append(stale, b)
			continue
		}
		out[b.ID] = e
	}
	hc.mu.Unlock()

	for _, b := range stale {
		out[b.ID] = hc.probe(ctx, b)
	}
	return out
}

// probe runs one backend reachability check outside hc.mu, deduplicated:
// the first caller for a backend performs the probe and writes the cache;
// callers arriving while it runs wait and share its result. Without the
// dedupe a dashboard poll wave (every few seconds, many viewers) would
// fire a probe per viewer per stale backend.
func (hc *HealthCache) probe(ctx context.Context, b store.Backend) HealthEntry {
	hc.mu.Lock()
	if ch, ok := hc.inflight[b.ID]; ok {
		hc.mu.Unlock()
		<-ch
		hc.mu.Lock()
		e := hc.entries[b.ID]
		hc.mu.Unlock()
		return e
	}
	ch := make(chan struct{})
	hc.inflight[b.ID] = ch
	hc.mu.Unlock()

	// The probe result is cached and shared for the whole TTL, so it
	// must not inherit this one caller's request context: a client
	// disconnecting mid-probe would poison the cache with a false
	// "offline" for every caller. Reachable bounds itself at 2 s.
	err := Reachable(context.WithoutCancel(ctx), b.BaseURL)

	hc.mu.Lock()
	e := HealthEntry{Online: err == nil, At: time.Now()}
	if err != nil {
		e.Err = err.Error()
	}
	hc.entries[b.ID] = e
	delete(hc.inflight, b.ID)
	close(ch)
	hc.mu.Unlock()
	return e
}
