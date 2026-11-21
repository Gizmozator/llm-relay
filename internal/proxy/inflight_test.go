package proxy

import (
	"sync"
	"testing"
)

func TestInFlightTracker(t *testing.T) {
	f := NewInFlight()
	f.Enter(1, "a")
	f.Enter(1, "a")
	f.Enter(2, "a")
	f.Leave(1, "a")
	if got := f.Snapshot(0)["a"]; got != 2 {
		t.Fatalf("global count = %d, want 2", got)
	}
	// Per-user scoping: user 1 sees one, user 2 sees one, user 3 sees none.
	if got := f.Snapshot(1)["a"]; got != 1 {
		t.Fatalf("user 1 count = %d, want 1", got)
	}
	if got := f.Snapshot(2)["a"]; got != 1 {
		t.Fatalf("user 2 count = %d, want 1", got)
	}
	if got := f.Snapshot(3); len(got) != 0 {
		t.Fatalf("user 3 snapshot = %v, want empty", got)
	}
	// Stray leaves must not go negative or leave zero entries behind.
	f.Leave(1, "a")
	f.Leave(1, "a")
	if got := f.Snapshot(0); len(got) != 1 || got["a"] != 1 {
		t.Fatalf("snapshot = %v, want only a:1", got)
	}
	// Nil receiver is a no-op (handlers built without a tracker).
	var nilF *InFlight
	nilF.Enter(1, "x")
	nilF.Leave(1, "x")
	if nilF.Snapshot(0) == nil {
		t.Fatal("nil Snapshot must return an empty map, not nil")
	}
}

func TestInFlightConcurrent(t *testing.T) {
	f := NewInFlight()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Enter(1, "m")
			f.Snapshot(0)
			f.Leave(1, "m")
		}()
	}
	wg.Wait()
	if got := f.Snapshot(0); len(got) != 0 {
		t.Fatalf("snapshot = %v, want empty", got)
	}
}
