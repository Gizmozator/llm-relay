package proxy

import (
	"sync"
	"testing"
	"time"
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

func TestInFlightBusySince(t *testing.T) {
	f := NewInFlight()
	t0 := time.Now().Add(-2 * time.Minute)
	t1 := time.Now().Add(-time.Minute)
	f.EnterAt(1, "a", t0)
	f.EnterAt(1, "a", t1)
	f.Enter(2, "a")

	bs := f.BusySince(1)
	if len(bs["a"]) != 2 || !bs["a"][0].Equal(t0) || !bs["a"][1].Equal(t1) {
		t.Fatalf("user 1 starts = %v, want [t0 t1]", bs["a"])
	}
	if len(f.BusySince(0)["a"]) != 3 {
		t.Fatal("global BusySince must count all users")
	}
	if len(f.BusySince(3)) != 0 {
		t.Fatal("unknown user must see no streams")
	}
	// Leave releases the oldest start; the copy stays valid afterwards.
	f.Leave(1, "a")
	if len(bs["a"]) != 2 {
		t.Error("returned slice must be a copy")
	}
	if rest := f.BusySince(1)["a"]; len(rest) != 1 || !rest[0].Equal(t1) {
		t.Errorf("after Leave = %v, want [t1] (oldest released)", rest)
	}
	var nilF *InFlight
	if nilF.BusySince(0) == nil {
		t.Fatal("nil BusySince must return an empty map, not nil")
	}
}

func TestFoldBusyWindows(t *testing.T) {
	windowStart := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	now := windowStart.Add(4*time.Minute + 30*time.Second)

	// Stream older than the window: clipped at windowStart, runs to now.
	v := make([]int64, 5)
	FoldBusyWindows(v, windowStart, []time.Time{windowStart.Add(-time.Minute)}, now)
	want := []int64{60_000, 60_000, 60_000, 60_000, 30_000}
	for i := range want {
		if v[i] != want[i] {
			t.Fatalf("clipped stream bucket %d = %d, want %d", i, v[i], want[i])
		}
	}

	// Mid-window start distributes; a future start contributes nothing.
	v = make([]int64, 5)
	FoldBusyWindows(v, windowStart, []time.Time{
		windowStart.Add(3*time.Minute + 30*time.Second),
		now.Add(time.Minute),
	}, now)
	want = []int64{0, 0, 0, 30_000, 30_000}
	for i := range want {
		if v[i] != want[i] {
			t.Fatalf("mid-window bucket %d = %d, want %d", i, v[i], want[i])
		}
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
