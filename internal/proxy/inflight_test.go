package proxy

import (
	"sync"
	"testing"
	"time"
)

func TestInFlightTracker(t *testing.T) {
	f := NewInFlight()
	id1 := f.Enter(Stream{UserID: 1, Model: "a"})
	f.Enter(Stream{UserID: 1, Model: "a"})
	f.Enter(Stream{UserID: 2, Model: "a"})
	f.Leave(id1)
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
	// Unknown/zero ids are no-ops.
	f.Leave(0)
	f.Leave(9999)
	if got := f.Snapshot(0); len(got) != 1 || got["a"] != 2 {
		t.Fatalf("snapshot = %v, want only a:2", got)
	}
	// Nil receiver is a no-op (handlers built without a tracker).
	var nilF *InFlight
	nilF.Enter(Stream{Model: "x"})
	nilF.Leave(1)
	if nilF.Snapshot(0) == nil {
		t.Fatal("nil Snapshot must return an empty map, not nil")
	}
}

func TestInFlightBusySince(t *testing.T) {
	f := NewInFlight()
	t0 := time.Now().Add(-2 * time.Minute)
	t1 := time.Now().Add(-time.Minute)
	id0 := f.EnterAt(Stream{UserID: 1, Model: "a"}, t0)
	f.EnterAt(Stream{UserID: 1, Model: "a"}, t1)
	f.Enter(Stream{UserID: 2, Model: "a"})

	bs := f.BusySince(1)
	if len(bs["a"]) != 2 {
		t.Fatalf("user 1 starts = %v, want 2 entries", bs["a"])
	}
	if len(f.BusySince(0)["a"]) != 3 {
		t.Fatal("global BusySince must count all users")
	}
	if len(f.BusySince(3)) != 0 {
		t.Fatal("unknown user must see no streams")
	}
	// Leave removes exactly that stream; the copy stays valid afterwards.
	f.Leave(id0)
	if len(bs["a"]) != 2 {
		t.Error("returned slice must be a copy")
	}
	if rest := f.BusySince(1)["a"]; len(rest) != 1 || !rest[0].Equal(t1) {
		t.Errorf("after Leave = %v, want [%v] (exact id removed)", rest, t1)
	}
	var nilF *InFlight
	if nilF.BusySince(0) == nil {
		t.Fatal("nil BusySince must return an empty map, not nil")
	}
}

func TestInFlightList(t *testing.T) {
	f := NewInFlight()
	now := time.Now()
	// Enter out of start order: List must sort oldest first.
	late := f.EnterAt(Stream{UserID: 2, Model: "b", User: "u2"}, now.Add(-time.Minute))
	f.EnterAt(Stream{UserID: 1, Model: "a", User: "u1", Key: "k1"}, now.Add(-3*time.Minute))

	all := f.List(0)
	if len(all) != 2 || all[0].Model != "a" || all[1].Model != "b" {
		t.Fatalf("List(0) = %+v, want [a b] oldest first", all)
	}
	if all[0].User != "u1" || all[0].Key != "k1" || all[0].Start.Equal(now) {
		t.Errorf("record fields lost: %+v", all[0])
	}
	if mine := f.List(1); len(mine) != 1 || mine[0].Model != "a" {
		t.Errorf("List(1) = %+v, want only a", mine)
	}
	// Copies: mutating the result must not touch the tracker, and Leave
	// removes exactly one entry by id.
	all[0].Model = "tampered"
	f.Leave(late)
	if got := f.List(0); len(got) != 1 || got[0].Model != "a" {
		t.Fatalf("after Leave = %+v, want only a (untampered)", got)
	}
	var nilF *InFlight
	if nilF.List(0) == nil {
		t.Fatal("nil List must return an empty slice, not nil")
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
			id := f.Enter(Stream{UserID: 1, Model: "m"})
			f.Snapshot(0)
			f.Leave(id)
		}()
	}
	wg.Wait()
	if got := f.Snapshot(0); len(got) != 0 {
		t.Fatalf("snapshot = %v, want empty", got)
	}
}
