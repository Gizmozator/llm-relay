// Materialization (plan 2026-11-22-relay-federation, task 3): a peer's
// advertised catalog becomes ordinary rows in the local catalog — models on
// a synthetic backend (peer://<id>, or hub://federation on a spoke), and,
// on a spoke, the hub's federated groups as federated-marked group rows.
// From here on the whole existing machinery (HRW affinity, health, drain,
// /v1/models, metering) treats federated models exactly like local ones.
//
// Collision policy (plan): a shared model whose name collides with a local
// group name or a local (non-peer) public model name is NOT materialized —
// skipped with a logged warning; the serving admin resolves it by renaming
// on their side. Two peers advertising the same name is fine (distinct
// synthetic backends — that is the grouping use case).
//
// Lifecycle: catalog frame → ensure backend (enabled) + upsert models +
// prune absent names; channel died → disable backends (rows stay, model ids
// survive the reconnect); peer deleted → backend + rows removed.
package fed

import (
	"database/sql"
	"errors"
	"strconv"

	"llm-relay/internal/store"
)

type materializer struct {
	db   *sql.DB
	logf func(string, ...any)
}

// peerBackend applies one spoke's catalog frame on the hub.
func (m *materializer) peerBackend(peerID int64, peerName string, cat *Catalog) {
	b, err := store.EnsureSyntheticBackend(m.db, "federation: "+peerName,
		store.PeerBackendURL(peerID))
	if err != nil {
		m.logf("fed: materialize peer %d backend: %v", peerID, err)
		return
	}
	m.applyModels(b.ID, cat.Models)
	if err := store.SetBackendEnabled(m.db, b.ID, true); err != nil {
		m.logf("fed: enable peer %d backend: %v", peerID, err)
	}
}

// hubBackend applies one frame of the hub's merged catalog on a spoke:
// Origin 0 lands on the hub:// backend (and carries the groups), Origin A
// (another spoke's relayed entries) lands on peer://A.
func (m *materializer) hubFrame(cat *Catalog) {
	var b *store.Backend
	var err error
	if cat.Origin == 0 {
		b, err = store.EnsureSyntheticBackend(m.db, "federation: hub", store.FedHubBaseURL)
	} else {
		// The frame carries no name for a relayed peer; the id is all the
		// spoke has to label it.
		b, err = store.EnsureSyntheticBackend(m.db,
			"federation: peer #"+strconv.FormatInt(cat.Origin, 10), store.PeerBackendURL(cat.Origin))
	}
	if err != nil {
		m.logf("fed: materialize origin %d backend: %v", cat.Origin, err)
		return
	}
	m.applyModels(b.ID, cat.Models)
	if err := store.SetBackendEnabled(m.db, b.ID, true); err != nil {
		m.logf("fed: enable origin %d backend: %v", cat.Origin, err)
	}
	if cat.Origin == 0 && cat.Groups != nil {
		// nil Groups = "this frame carries no group data" (partial or
		// malformed frame): skip entirely, never prune. An explicit []
		// (non-nil, empty) is a legitimate "no groups" and does prune.
		m.applyGroups(cat.Groups)
	}
}

// applyModels upserts every advertised model that passes the collision
// policy, then prunes rows the frame no longer advertises. A nil list is
// "no data in this frame" (see the Catalog wire comment) and must never
// prune; [] legitimately means "shares nothing".
func (m *materializer) applyModels(backendID int64, models []CatalogModel) {
	if models == nil {
		return
	}
	keep := make([]string, 0, len(models))
	for _, cm := range models {
		reason, bad, cerr := store.FedModelCollision(m.db, cm.Name)
		if cerr != nil {
			// Transient failure: keep the existing row (add to keep) — a
			// collision verdict here is unknown, and leaving the name out
			// would let one SQLITE_BUSY delete a live materialized model.
			m.logf("fed: collision check for shared model %q: %v (keeping previous row)", cm.Name, cerr)
			keep = append(keep, cm.Name)
			continue
		}
		if bad {
			m.logf("fed: skipping shared model %q: %s", cm.Name, reason)
			continue
		}
		in := store.FedModelInput{
			Name: cm.Name, UpstreamModel: cm.UpstreamModel,
			PriceIn: cm.PriceIn, PriceOut: cm.PriceOut,
			PriceCacheRead: cm.PriceCacheRead, PriceCacheWrite: cm.PriceCacheWrite,
			ContextLength: cm.ContextLength, BenchTokPerS: cm.BenchTokPerS,
			ConcurrencyLimit: cm.ConcurrencyLimit, BusyThresholdPct: cm.BusyThresholdPct,
		}
		if err := store.UpsertFedModel(m.db, backendID, in); err != nil {
			// Transient failure: keep the existing row (add to keep) —
			// leaving it out would let one SQLITE_BUSY delete a live
			// materialized model and its group memberships, churning the
			// model id that usage rows reference.
			m.logf("fed: upsert shared model %q: %v (keeping previous row)", cm.Name, err)
			keep = append(keep, cm.Name)
			continue
		}
		keep = append(keep, cm.Name)
	}
	if err := store.DeleteFedModelsNotIn(m.db, backendID, keep); err != nil {
		m.logf("fed: prune backend %d models: %v", backendID, err)
	}
}

// applyGroups materializes the hub's federated groups on a spoke: member
// names map to local rows first, materialized rows second (local-first
// resolution falls out of the group membership itself — a spoke with its own
// copy hashes to it). A group name owned by a local group or a public model
// is skipped with a log; unresolvable members are dropped.
//
// Transient errors never degrade a group: a member whose resolution FAILS
// (DB error, not "no row") aborts re-applying THAT group — its current
// composition stays until the next frame — and the group is kept in the
// prune-keep set so the sweep leaves it alone. Same for a failed upsert
// (except the ownership conflicts, where the federated row legitimately
// must not exist under that name).
func (m *materializer) applyGroups(groups []CatalogGroup) {
	keep := make([]string, 0, len(groups))
	for _, g := range groups {
		var ids []int64
		var weights []float64
		broken := false
		for _, mem := range g.Members {
			id, ok, err := store.ResolveFedMember(m.db, mem.ModelName, g.Name)
			if err != nil {
				m.logf("fed: group %q: member %q resolve failed: %v (keeping current group)",
					g.Name, mem.ModelName, err)
				broken = true
				break
			}
			if !ok {
				m.logf("fed: group %q: member %q not resolvable, dropping", g.Name, mem.ModelName)
				continue
			}
			ids = append(ids, id)
			weights = append(weights, mem.Weight)
		}
		if broken {
			keep = append(keep, g.Name) // preserve the current composition
			continue
		}
		if len(ids) == 0 {
			m.logf("fed: group %q: no resolvable members, skipping", g.Name)
			continue
		}
		if err := store.UpsertFederatedGroup(m.db, g.Name, ids, weights); err != nil {
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrExists) {
				m.logf("fed: group %q skipped: %v", g.Name, err)
			} else {
				m.logf("fed: group %q: %v (keeping current group)", g.Name, err)
				keep = append(keep, g.Name) // transient: preserve the row
			}
			continue
		}
		keep = append(keep, g.Name)
	}
	if err := store.DeleteFederatedGroupsNotIn(m.db, keep); err != nil {
		m.logf("fed: prune federated groups: %v", err)
	}
}

// setPeerOnline enables/disables one peer's synthetic backend (hub side:
// channel came up / died; spoke side: a hub StatusFrame about a third
// spoke). Rows are kept either way — model ids stay stable across a
// reconnect; only visibility and routing flip.
func (m *materializer) setPeerOnline(peerID int64, online bool) {
	if err := store.SetPeerBackendEnabled(m.db, peerID, online); err != nil {
		m.logf("fed: set peer %d backend online=%v: %v", peerID, online, err)
	}
}

// allOffline disables every synthetic backend (this spoke's channel to the
// hub died: nothing materialized is reachable).
func (m *materializer) allOffline() {
	if err := store.SetSyntheticBackendsEnabled(m.db, false); err != nil {
		m.logf("fed: disable synthetic backends: %v", err)
	}
}

// removePeer deletes a peer's synthetic backend and its model rows for
// good (the peer row itself is gone; nothing will re-materialize it).
func (m *materializer) removePeer(peerID int64) {
	b, err := store.BackendByBaseURL(m.db, store.PeerBackendURL(peerID))
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		m.logf("fed: lookup peer %d backend: %v", peerID, err)
		return
	}
	if err := store.DeleteBackendCascade(m.db, b.ID); err != nil {
		m.logf("fed: remove peer %d backend: %v", peerID, err)
	}
}
