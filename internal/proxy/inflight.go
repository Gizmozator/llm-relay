// In-flight request tracking: what the relay is forwarding at this second.
// Memory-only by design — a restart clears it, and that is correct because
// the streams it counts died with the process.
package proxy

import "sync"

type inflightKey struct {
	userID int64
	model  string
}

// InFlight counts active proxied requests per (user, model) pair, so the
// live view can scope active streams to the caller exactly like every
// other usage surface.
type InFlight struct {
	mu     sync.Mutex
	counts map[inflightKey]int
}

func NewInFlight() *InFlight {
	return &InFlight{counts: map[inflightKey]int{}}
}

// Enter records one request starting on the model for the user. Nil
// receiver is a no-op so handlers built without a tracker (tests) stay safe.
func (f *InFlight) Enter(userID int64, model string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.counts[inflightKey{userID, model}]++
	f.mu.Unlock()
}

// Leave records one request finishing; it never drives a count negative,
// so a stray Leave cannot corrupt the gauge.
func (f *InFlight) Leave(userID int64, model string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	k := inflightKey{userID, model}
	if n := f.counts[k]; n > 1 {
		f.counts[k] = n - 1
	} else {
		delete(f.counts, k)
	}
	f.mu.Unlock()
}

// Snapshot returns the current per-model counts (copy, safe to keep).
// userID 0 means "all users" (the admin view).
func (f *InFlight) Snapshot(userID int64) map[string]int {
	if f == nil {
		return map[string]int{}
	}
	f.mu.Lock()
	out := map[string]int{}
	for k, n := range f.counts {
		if userID != 0 && k.userID != userID {
			continue
		}
		out[k.model] += n
	}
	f.mu.Unlock()
	return out
}
