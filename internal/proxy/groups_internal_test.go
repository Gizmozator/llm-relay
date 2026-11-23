// HRW ranking internals (white-box): score determinism and weight scaling,
// session-less least-loaded ordering.
package proxy

import (
	"fmt"
	"math"
	"testing"

	"llm-relay/internal/store"
)

func TestHRWScoreDeterministic(t *testing.T) {
	a := hrwScore("session-1", 7, 1)
	if b := hrwScore("session-1", 7, 1); a != b {
		t.Errorf("score not deterministic: %v vs %v", a, b)
	}
	if a <= 0 || math.IsInf(a, 0) || math.IsNaN(a) {
		t.Errorf("score out of range: %v", a)
	}
	// Doubling the weight doubles the score: a member's share of sessions
	// is proportional to its weight.
	if w := hrwScore("session-1", 7, 2); w != 2*a {
		t.Errorf("weight scaling: %v vs %v", w, 2*a)
	}
	// Weight <= 0 normalizes to 1 (defensive, mirrors the store).
	if z := hrwScore("session-1", 7, 0); z != a {
		t.Errorf("zero weight = %v, want %v", z, a)
	}
	// Distinct sessions produce distinct scores (no constant collapse).
	seen := map[float64]bool{}
	for i := range 64 {
		seen[hrwScore(fmt.Sprintf("s-%d", i), 1, 1)] = true
	}
	if len(seen) < 60 {
		t.Errorf("poor spread: %d distinct scores from 64 sessions", len(seen))
	}
}

func TestRankMembers(t *testing.T) {
	mk := func(id, backendID int64, weight float64) store.GroupMember {
		return store.GroupMember{ModelID: id, Weight: weight, Position: int(id),
			Backend: &store.Backend{ID: backendID}}
	}
	members := []store.GroupMember{mk(1, 11, 1), mk(2, 22, 1), mk(3, 33, 1)}

	// HRW order is deterministic across calls.
	first, second := rankMembers(members, "sess", nil, 0), rankMembers(members, "sess", nil, 0)
	for i := range first {
		if first[i].ModelID != second[i].ModelID {
			t.Fatalf("ranking not deterministic: %v vs %v", first, second)
		}
	}

	// Empty session: least in-flight first. Member 2 carries two streams,
	// member 1 one, member 3 none → order 3, 1, 2.
	f := NewInFlight()
	sid := f.Enter(Stream{ModelID: 1})
	f.Enter(Stream{ModelID: 2})
	f.Enter(Stream{ModelID: 2})
	if c := f.CountModels([]int64{1, 2, 3}, 0); c[1] != 1 || c[2] != 2 || c[3] != 0 {
		t.Errorf("CountModels = %v, want map[1:1 2:2 3:0]", c)
	}
	// Self-exclusion: the caller's own stream (on member 1) drops out of
	// its own ranking counts — no phantom self-count.
	if c := f.CountModels([]int64{1, 2, 3}, sid); c[1] != 0 || c[2] != 2 || c[3] != 0 {
		t.Errorf("CountModels(exclude own) = %v, want map[1:0 2:2 3:0]", c)
	}
	ranked := rankMembers(members, "", f, 0)
	if ranked[0].ModelID != 3 || ranked[1].ModelID != 1 || ranked[2].ModelID != 2 {
		t.Errorf("least-loaded order = [%d %d %d], want [3 1 2]",
			ranked[0].ModelID, ranked[1].ModelID, ranked[2].ModelID)
	}
}

// The u mapping must stay strictly inside (0,1) for EVERY hash input: the
// top 2^11 hashes round the offset sum to exactly 1.0 (ties-to-even), and
// -ln(1.0) == -0.0 would turn weight/-0.0 into -Inf, ranking the member
// last instead of first. math.MaxUint64 is the worst case (all top 53
// bits set); 0 is the mirror case for the lower bound.
func TestHRWUnitInterval(t *testing.T) {
	for _, z := range []uint64{0, 1, math.MaxUint64, math.MaxUint64 - 1, 1 << 63} {
		u := hrwU(z)
		if !(u > 0 && u < 1) {
			t.Errorf("hrwU(%d) = %v, want strictly inside (0,1)", z, u)
		}
		if s := hrwScore("s", 1, 1); math.IsInf(s, 0) || math.IsNaN(s) {
			t.Errorf("score not finite: %v", s)
		}
	}
	// The clamped top value must still out-rank everything else: its
	// score is huge and positive, not -Inf.
	top := hrwU(math.MaxUint64)
	if s := 1 / -math.Log(top); !(s > 1e15) {
		t.Errorf("clamped top score = %v, want a huge positive value", s)
	}
}
