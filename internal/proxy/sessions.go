// Session recency tracking for drain mode: which sessions are "currently
// running" so a draining backend can keep them while refusing new ones.
// Memory-only like the in-flight tracker — a relay restart falls back to
// usage_log (store.SessionRecent).
package proxy

import (
	"sync"
	"time"
)

// drainSessionWindow is how recently a session must have been active to
// count as "currently running" while its backend drains (owner decision:
// 15 minutes). A var so tests can shrink it.
var drainSessionWindow = 15 * time.Minute

// SessionTracker remembers the last activity time per session id. Mark is
// called on every proxied request (not only while draining) so the set is
// warm the moment an admin flips a backend to draining.
type SessionTracker struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	marks int
}

func NewSessionTracker() *SessionTracker {
	return &SessionTracker{seen: map[string]time.Time{}}
}

// Mark records session activity now and lazily prunes stale entries
// (bounded work: a full sweep every 128 marks).
func (s *SessionTracker) Mark(session string) {
	if s == nil || session == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	s.seen[session] = now
	s.marks++
	if s.marks >= 128 {
		s.marks = 0
		for k, t := range s.seen {
			if now.Sub(t) > drainSessionWindow {
				delete(s.seen, k)
			}
		}
	}
	s.mu.Unlock()
}

// Recent reports whether the session was active within the window.
func (s *SessionTracker) Recent(session string) bool {
	if s == nil || session == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.seen[session]
	return ok && time.Since(t) <= drainSessionWindow
}
