// Group member selection: weighted rendezvous (HRW) hashing with hard
// exclusions. A group request resolves to one member per session so each
// engine's prompt cache stays warm; the ranking is stateless — recomputed
// per request from (session, backend id, weight) — so it is stable across
// restarts and identical on every relay instance.
package proxy

import (
	"cmp"
	"context"
	"hash/fnv"
	"math"
	"slices"
	"strconv"

	"llm-relay/internal/store"
)

// hrwScore is one member's weighted rendezvous-hash score for one session:
// score = weight / -ln(u), u uniform in (0,1) from fnv1a(session || 0x1f ||
// member model id) plus a splitmix64 finalizer. Highest score wins; a
// member's share of distinct sessions is proportional to its weight.
// Deterministic: same inputs, same score, forever.
//
// The hash keys on the member's MODEL id, not the plan's backend_id: the
// distribution unit is the member, and two members sharing one backend must
// still split sessions (with a backend-id key they get byte-identical u
// and weight alone decides, starving the loser). In the normal
// one-member-per-backend shape the uniformity is identical.
func hrwScore(session string, memberID int64, weight float64) float64 {
	if weight <= 0 {
		weight = 1 // defensive: the store normalizes too
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(session))
	_, _ = h.Write([]byte{0x1f})
	var buf [20]byte
	_, _ = h.Write(strconv.AppendInt(buf[:0], memberID, 10))
	// fnv1a has weak avalanche: inputs differing only at the end differ in
	// only a few bits, so mapping raw hash bits to u would leave members
	// near-identical scores and let weight alone decide every session. The
	// splitmix64 finalizer spreads every input bit across the whole word.
	z := h.Sum64()
	z ^= z >> 30
	z *= 0xbf58476d1ce4e5b9
	z ^= z >> 27
	z *= 0x94d049bb133111eb
	z ^= z >> 31
	return weight / -math.Log(hrwU(z))
}

// hrwU maps a finalized hash to u in (0,1): the top 53 bits convert to
// float64 exactly, and the half-step offset keeps u above 0. The clamp
// keeps u below 1: for the top 2^11 hashes the offset sum is exactly
// 1-2^-54, a rounding tie that ties-to-even resolves to 1.0 — and
// -ln(1.0) is -0.0, so weight/-0.0 would flip the member's highest
// possible score to -Inf, ranking it last instead of first.
func hrwU(z uint64) float64 {
	const scale = 1 / float64(int64(1)<<53)
	u := float64(z>>11)*scale + scale/2
	if u >= 1 {
		u = math.Nextafter(1, 0)
	}
	return u
}

// memberScore ranks a member; a member without a backend (cannot happen
// through the store join, but be defensive) sorts last instead of panicking.
func memberScore(session string, mem store.GroupMember) float64 {
	if mem.Backend == nil {
		return math.Inf(-1)
	}
	return hrwScore(session, mem.ModelID, mem.Weight)
}

// rankMembers orders members as pick candidates. With a session id the
// order is HRW (descending score). An empty session id — no header and not
// conversation-shaped — has nothing to hash, so the least-loaded member
// (fewest in-flight streams on its model row, excluding the caller's own
// provisional stream) goes first, position order breaking ties. The
// per-member counts are read ONCE up front (one lock, one scan) — never
// inside the comparator. The store returns members in position order and
// both sorts are stable, so the result is deterministic either way.
func rankMembers(members []store.GroupMember, session string, flight *InFlight, excludeStreamID int64) []store.GroupMember {
	ranked := slices.Clone(members)
	if session == "" {
		ids := make([]int64, 0, len(ranked))
		for _, mem := range ranked {
			ids = append(ids, mem.ModelID)
		}
		counts := flight.CountModels(ids, excludeStreamID)
		slices.SortStableFunc(ranked, func(a, b store.GroupMember) int {
			return cmp.Compare(counts[a.ModelID], counts[b.ModelID])
		})
		return ranked
	}
	scores := make(map[int64]float64, len(ranked))
	for _, mem := range ranked {
		scores[mem.ModelID] = memberScore(session, mem)
	}
	slices.SortStableFunc(ranked, func(a, b store.GroupMember) int {
		return cmp.Compare(scores[b.ModelID], scores[a.ModelID])
	})
	return ranked
}

// eligibleMembers applies the hard exclusions — member model disabled,
// backend disabled, backend draining, backend failing the health cache —
// keeping the store's position order. Saturation deliberately does NOT
// reroute (plan Decision 3): a hashed member stays chosen even when its
// engine queue is long; InFlight only orders session-less requests.
// Ranking is rankMembers' job; the split lets the caller register the
// request in InFlight between exclusion and ranking.
func (h *Handler) eligibleMembers(ctx context.Context, members []store.GroupMember) []store.GroupMember {
	// Cheap exclusions first (no network), then ONE batched health-cache
	// call for the survivors: Status is slice-based on purpose — one lock
	// acquisition, one clock read, one result map — where per-member calls
	// would take the shared mutex once per member on every group request.
	// Members sharing a backend are deduped: one cache entry per backend.
	candidates := make([]store.GroupMember, 0, len(members))
	var backends []store.Backend
	seen := make(map[int64]bool, len(members))
	for _, mem := range members {
		if mem.Model == nil || mem.Backend == nil || !mem.Model.Enabled || !mem.Backend.Enabled {
			continue
		}
		if mem.Backend.Draining {
			continue
		}
		candidates = append(candidates, mem)
		if h.Health != nil && !seen[mem.Backend.ID] {
			seen[mem.Backend.ID] = true
			backends = append(backends, *mem.Backend)
		}
	}
	var online map[int64]HealthEntry
	// len(backends) > 0 implies h.Health != nil (only collected then): an
	// all-excluded group must not touch the cache mutex (another request
	// may hold it across probes) just to fetch an empty map.
	if len(backends) > 0 {
		online = h.Health.Status(ctx, backends)
	}
	out := make([]store.GroupMember, 0, len(candidates))
	for _, mem := range candidates {
		if h.Health != nil && !online[mem.Backend.ID].Online {
			continue
		}
		out = append(out, mem)
	}
	return out
}
