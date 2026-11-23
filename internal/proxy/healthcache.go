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
// across the consumers (dashboard viewers, agent endpoints). The mutex is
// held across probes, which doubles as probe-stampede dedupe.
type HealthCache struct {
	mu      sync.Mutex
	entries map[int64]HealthEntry
	// Synthetic answers "is this federation backend reachable" for
	// peer://hub:// backends, which have no HTTP endpoint to probe: the
	// fed.Manager wires it to the channel registry (channel online =
	// healthy). nil = treat synthetic backends as online (materialization
	// already disables the backend when its channel dies).
	Synthetic func(b store.Backend) bool
}

func NewHealthCache() *HealthCache {
	return &HealthCache{entries: map[int64]HealthEntry{}}
}

// Status returns per-backend reachability, probing only entries older than
// the TTL. Disabled backends are offline without a probe.
func (hc *HealthCache) Status(ctx context.Context, backends []store.Backend) map[int64]HealthEntry {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	out := make(map[int64]HealthEntry, len(backends))
	// entries is intentionally never pruned: keys are backend IDs, bounded
	// by the number of backends ever created (tiny on a homelab), and a
	// stale entry for a deleted ID is simply never read again.
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
			// The probe result is cached and shared for the whole TTL, so it
			// must not inherit this one caller's request context: a client
			// disconnecting mid-probe would poison the cache with a false
			// "offline" for every caller. Reachable bounds itself at 2 s.
			err := Reachable(context.WithoutCancel(ctx), b.BaseURL)
			e = HealthEntry{Online: err == nil, At: time.Now()}
			if err != nil {
				e.Err = err.Error()
			}
			hc.entries[b.ID] = e
		}
		out[b.ID] = e
	}
	return out
}
