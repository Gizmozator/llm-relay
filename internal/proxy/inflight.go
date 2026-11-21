// In-flight request tracking: what the relay is forwarding at this second.
// Memory-only by design — a restart clears it, and that is correct because
// the streams it counts died with the process.
package proxy

import (
	"sort"
	"sync"
	"time"
)

// Stream is one active proxied request, recorded at routing time.
type Stream struct {
	ID        int64     `json:"-"`
	UserID    int64     `json:"-"`
	User      string    `json:"user"`
	Key       string    `json:"key"`
	Model     string    `json:"model"`
	Endpoint  string    `json:"endpoint"`
	Session   string    `json:"session,omitempty"`
	IP        string    `json:"ip,omitempty"`
	Streaming bool      `json:"streaming"`
	Start     time.Time `json:"-"`
}

// InFlight tracks active proxied requests by opaque id so the live view
// can count them per model, scope them per user, fold their busy time and
// (for admins) list who is on what right now.
type InFlight struct {
	mu      sync.Mutex
	next    int64
	streams map[int64]*Stream
}

func NewInFlight() *InFlight {
	return &InFlight{streams: map[int64]*Stream{}}
}

// Enter records a stream starting now and returns its release id.
func (f *InFlight) Enter(s Stream) int64 {
	return f.EnterAt(s, time.Now())
}

// EnterAt records a stream with an explicit start time (tests; Enter is
// the production path). Nil receiver is a no-op so handlers built without
// a tracker stay safe, and the map is lazily created so a zero-value
// &InFlight{} works too (a nil-map write would panic on the hot path).
func (f *InFlight) EnterAt(s Stream, start time.Time) int64 {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	if f.streams == nil {
		f.streams = map[int64]*Stream{}
	}
	f.next++
	s.ID = f.next
	s.Start = start
	f.streams[s.ID] = &s
	id := s.ID
	f.mu.Unlock()
	return id
}

// Leave releases the stream with the given id (0 / unknown is a no-op).
func (f *InFlight) Leave(id int64) {
	if f == nil || id == 0 {
		return
	}
	f.mu.Lock()
	delete(f.streams, id)
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
	for _, s := range f.streams {
		if userID != 0 && s.UserID != userID {
			continue
		}
		out[s.Model]++
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
	for _, s := range f.streams {
		if userID != 0 && s.UserID != userID {
			continue
		}
		out[s.Model] = append(out[s.Model], s.Start)
	}
	f.mu.Unlock()
	return out
}

// List returns copies of the active streams, oldest first. userID 0 means
// "all users".
func (f *InFlight) List(userID int64) []Stream {
	if f == nil {
		return []Stream{}
	}
	f.mu.Lock()
	out := []Stream{}
	for _, s := range f.streams {
		if userID != 0 && s.UserID != userID {
			continue
		}
		out = append(out, *s)
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
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
