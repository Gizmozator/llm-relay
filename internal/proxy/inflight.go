// In-flight request tracking: what the relay is forwarding at this second.
// Memory-only by design — a restart clears it, and that is correct because
// the streams it counts died with the process.
package proxy

import (
	"sync"
	"time"
)

type inflightKey struct {
	userID int64
	model  string
}

// InFlight tracks active proxied requests per (user, model) pair, keeping
// each stream's start time so the live view can both scope counts to the
// caller and fold still-running busy time into the minute buckets (usage
// rows only exist once a request completes).
type InFlight struct {
	mu     sync.Mutex
	starts map[inflightKey][]time.Time
}

func NewInFlight() *InFlight {
	return &InFlight{starts: map[inflightKey][]time.Time{}}
}

// Enter records one request starting now on the model for the user.
func (f *InFlight) Enter(userID int64, model string) {
	f.EnterAt(userID, model, time.Now())
}

// EnterAt records a stream with an explicit start time (tests; Enter is the
// production path). Nil receiver is a no-op so handlers built without a
// tracker stay safe, and the map is lazily created so a zero-value
// &InFlight{} works too (a nil-map write would panic on the hot path).
func (f *InFlight) EnterAt(userID int64, model string, start time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.starts == nil {
		f.starts = map[inflightKey][]time.Time{}
	}
	k := inflightKey{userID, model}
	f.starts[k] = append(f.starts[k], start)
	f.mu.Unlock()
}

// Leave records one request finishing; it never drives a count negative,
// so a stray Leave cannot corrupt the gauge. The oldest stream's start is
// released — streams on one model are interchangeable for utilization.
func (f *InFlight) Leave(userID int64, model string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	k := inflightKey{userID, model}
	if st := f.starts[k]; len(st) > 1 {
		f.starts[k] = st[1:]
	} else {
		delete(f.starts, k)
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
	for k, st := range f.starts {
		if userID != 0 && k.userID != userID {
			continue
		}
		out[k.model] += len(st)
	}
	f.mu.Unlock()
	return out
}

// BusySince returns, per model, the start times of streams still running
// (copy, safe to keep). userID 0 means "all users". Each entry says "this
// model has been continuously busy at least since t", which is what the
// utilization fold needs to credit in-flight work before its usage row
// exists.
func (f *InFlight) BusySince(userID int64) map[string][]time.Time {
	if f == nil {
		return map[string][]time.Time{}
	}
	f.mu.Lock()
	out := map[string][]time.Time{}
	for k, st := range f.starts {
		if userID != 0 && k.userID != userID {
			continue
		}
		out[k.model] = append(out[k.model], st...)
	}
	f.mu.Unlock()
	return out
}

// FoldBusyWindows adds the in-flight busy interval [max(s, windowStart), now]
// for every start time in since to the per-minute busy series values, which
// must align with store.BusyByModel (bucket i covers windowStart+i·min).
// Streams older than the window are clipped at its start; a stream that
// started in the future contributes nothing.
func FoldBusyWindows(values []int64, windowStart time.Time, since []time.Time, now time.Time) {
	for _, s := range since {
		begin := s
		if begin.Before(windowStart) {
			begin = windowStart
		}
		if !begin.Before(now) {
			continue
		}
		i0 := int(begin.Sub(windowStart) / time.Minute)
		i1 := int(now.Sub(windowStart) / time.Minute)
		for i := i0; i <= i1 && i < len(values); i++ {
			mStart := windowStart.Add(time.Duration(i) * time.Minute)
			mEnd := mStart.Add(time.Minute)
			ovStart, ovEnd := begin, now
			if ovStart.Before(mStart) {
				ovStart = mStart
			}
			if ovEnd.After(mEnd) {
				ovEnd = mEnd
			}
			if d := ovEnd.Sub(ovStart); d > 0 {
				values[i] += d.Milliseconds()
			}
		}
	}
}
