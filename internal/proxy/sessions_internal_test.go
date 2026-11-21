// Internal tests for the session tracker: recency window and pruning.
package proxy

import (
	"testing"
	"time"
)

func TestSessionTrackerWindow(t *testing.T) {
	old := drainSessionWindow
	defer func() { drainSessionWindow = old }()

	s := NewSessionTracker()
	s.Mark("a")
	if !s.Recent("a") {
		t.Fatal("fresh mark must be recent")
	}
	if s.Recent("b") {
		t.Fatal("unknown session must not be recent")
	}
	s.Mark("") // empty session ids are never tracked
	if s.Recent("") {
		t.Fatal("empty session must not be recent")
	}

	drainSessionWindow = time.Nanosecond
	time.Sleep(time.Millisecond)
	if s.Recent("a") {
		t.Fatal("entry older than the window must expire")
	}

	// Pruning sweeps stale entries on the mark cadence.
	drainSessionWindow = time.Nanosecond
	for i := 0; i < 200; i++ {
		s.Mark(string(rune('a' + i%26)))
	}
	s.mu.Lock()
	n := len(s.seen)
	s.mu.Unlock()
	if n > 30 {
		t.Errorf("prune left %d entries, want the stale map swept", n)
	}

	var nilS *SessionTracker
	nilS.Mark("x") // nil receiver is a no-op
	if nilS.Recent("x") {
		t.Fatal("nil tracker is never recent")
	}
}
