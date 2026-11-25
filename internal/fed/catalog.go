// Catalog sync (plan 2026-11-22-relay-federation, task 3).
//
// Spoke → hub: one catalog frame with the spoke's fed_visibility='all'
// models ('allowlist' shares nobody on a spoke — its allowlist would name
// the spoke's own peer table, which a spoke does not have).
//
// Hub → spoke: a sequence of catalog frames, one per ORIGIN: relayed spoke
// entries first (Origin=<peer id>), then the hub's own frame (Origin=0)
// carrying its SharedWithPeer-filtered models plus the federated groups —
// last, so group member names already map when the group frame lands.
//
// Re-push is debounced (~500 ms): a burst of admin edits (model toggles,
// group edits) lands as one push per side, not one per write.
package fed

import (
	"database/sql"
	"sync"
	"time"

	"llm-relay/internal/store"
)

// catalogCtl owns the hub-side in-memory copy of every spoke's catalog (the
// relay source for other spokes) and the debounced push loop for both sides.
type catalogCtl struct {
	db       *sql.DB
	mgr      *Manager
	debounce time.Duration

	mu    sync.Mutex
	spoke map[int64][]CatalogModel // hub side: last catalog per peer
	timer *time.Timer

	// pushMu serializes push runs: two interleaved pushAll bodies (the
	// debounce timer, the dialer's post-connect push) could otherwise
	// deliver frames out of order — a stale snapshot landing after a
	// fresh one. A change that lands mid-push re-arms the debounce timer,
	// whose push queues behind the running one and reads fresh data.
	pushMu sync.Mutex
}

func newCatalogCtl(mgr *Manager) *catalogCtl {
	return &catalogCtl{db: mgr.db, mgr: mgr,
		debounce: mgr.opts.CatalogDebounce, spoke: map[int64][]CatalogModel{}}
}

// spokeCatalog records (or clears, on nil) what a peer advertises.
func (cc *catalogCtl) set(peerID int64, models []CatalogModel) {
	cc.mu.Lock()
	if models == nil {
		delete(cc.spoke, peerID)
	} else {
		cc.spoke[peerID] = models
	}
	cc.mu.Unlock()
}

func (cc *catalogCtl) others(except int64) map[int64][]CatalogModel {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	out := make(map[int64][]CatalogModel, len(cc.spoke))
	for id, m := range cc.spoke {
		if id != except {
			out[id] = m
		}
	}
	return out
}

// changed schedules a debounced re-push (the FedController hook: model
// fed_visibility/enabled, backend-enabled and spoke group edits all route
// here through the API layer).
func (cc *catalogCtl) changed() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.timer != nil {
		cc.timer.Stop()
	}
	cc.timer = time.AfterFunc(cc.debounce, func() {
		cc.mu.Lock()
		cc.timer = nil
		cc.mu.Unlock()
		cc.pushAll()
	})
}

// pushAll re-pushes the catalog to every live channel: the hub rebuilds the
// merged catalog per peer, a spoke re-sends its own. Serialized on pushMu
// (see the field comment): concurrent triggers — the debounce timer, the
// dialer's post-connect push — never interleave frames.
func (cc *catalogCtl) pushAll() {
	cc.pushMu.Lock()
	defer cc.pushMu.Unlock()
	switch cc.mgr.currentRole() {
	case store.FedRoleHub:
		for _, p := range cc.mgr.hub.Peers() {
			cc.pushHubCatalog(p.PeerID)
		}
	case store.FedRoleSpoke:
		cc.pushSpokeLocked()
	}
}

// pushHubCatalog sends peer peerID its merged catalog: every other spoke's
// relayed entries (Origin=their id) first, then the hub's own frame with
// the groups last (see the file comment for the ordering rationale).
//
// ANY list failure aborts the whole push: a half-built frame (models
// without groups) would make the spoke prune every federated group,
// turning one transient query error into fleet-wide data loss.
func (cc *catalogCtl) pushHubCatalog(peerID int64) {
	for origin, models := range cc.others(peerID) {
		if err := cc.mgr.hub.Send(peerID, &Catalog{T: TCatalog, Origin: origin, Models: models}); err != nil {
			cc.mgr.opts.Log.Printf("fed: relay catalog origin %d → peer %d: %v", origin, peerID, err)
		}
	}
	models, err := store.ListHubShareable(cc.db, peerID)
	if err != nil {
		cc.mgr.opts.Log.Printf("fed: hub shareable list for peer %d: %v", peerID, err)
		return
	}
	groups, err := store.ListHubGroups(cc.db)
	if err != nil {
		cc.mgr.opts.Log.Printf("fed: hub group list for peer %d: %v", peerID, err)
		return
	}
	vis, err := cc.groupVisibility(peerID)
	if err != nil {
		cc.mgr.opts.Log.Printf("fed: hub group visibility for peer %d: %v", peerID, err)
		return
	}
	frame := &Catalog{T: TCatalog, Models: catalogModels(models),
		Groups: visibleGroups(groups, peerID, vis)}
	if err := cc.mgr.hub.Send(peerID, frame); err != nil {
		cc.mgr.opts.Log.Printf("fed: hub catalog → peer %d: %v", peerID, err)
	}
}

// groupVisibility preloads what the group filter needs for one peer (the
// models shared to it beyond 'all': user-allowlist matches through the
// peer's owner plus 'owner' rows it owns — ticket #21) so member
// visibility is evaluated in memory: the per-member query version was
// O(peers×groups×members) queries per push, ListHubGroups already carries
// each member's backend and fed_visibility for exactly this reason.
func (cc *catalogCtl) groupVisibility(peerID int64) (map[int64]bool, error) {
	return store.PeerSharedModelIDs(cc.db, peerID)
}

// visibleGroups filters a hub group down to the members peerID may see
// (local rows: fed_visibility + allowlist set; materialized rows: everyone
// but their own origin) and drops groups with nothing visible left — a
// member the peer cannot use must not even be named to them.
func visibleGroups(groups []store.CatalogGroup, peerID int64, allow map[int64]bool) []CatalogGroup {
	out := []CatalogGroup{}
	for _, g := range groups {
		members := make([]CatalogGroupMember, 0, len(g.Members))
		for _, mem := range g.Members {
			if memberVisible(mem, peerID, allow) {
				members = append(members, CatalogGroupMember{ModelName: mem.ModelName, Weight: mem.Weight})
			}
		}
		if len(members) == 0 {
			continue
		}
		out = append(out, CatalogGroup{Name: g.Name, Members: members})
	}
	return out
}

// memberVisible answers whether the group member is visible to peerID from
// the data ListHubGroups already carried: a materialized row (synthetic
// backend) is visible to everyone but its origin; a local row needs the
// sharing rules, with the allowlist and owner resolutions preloaded in the
// shared set (both were resolved by PeerSharedModelIDs through the peer's
// owner).
func memberVisible(mem store.FedGroupMember, peerID int64, allow map[int64]bool) bool {
	if kind, origin, syn := store.SyntheticBaseURL(mem.BaseURL); syn {
		return kind == store.FedSchemePeer && origin != peerID
	}
	switch mem.FedVisibility {
	case store.FedVisAll:
		return true
	case store.FedVisAllowlist, store.FedVisOwner:
		return allow[mem.ModelID]
	}
	return false
}

// pushSpokeCatalog sends the spoke's own catalog to the hub (the dialer's
// post-connect push). Serialized with every other push via pushMu.
func (cc *catalogCtl) pushSpokeCatalog() {
	cc.pushMu.Lock()
	defer cc.pushMu.Unlock()
	cc.pushSpokeLocked()
}

func (cc *catalogCtl) pushSpokeLocked() {
	conn := cc.mgr.spokeConn()
	if conn == nil || conn.Closed() {
		return
	}
	models, err := store.ListSpokeShareable(cc.db)
	if err != nil {
		cc.mgr.opts.Log.Printf("fed: spoke shareable list: %v", err)
		return
	}
	if err := conn.Send(&Catalog{T: TCatalog, Models: catalogModels(models)}); err != nil {
		cc.mgr.opts.Log.Printf("fed: spoke catalog push: %v", err)
	}
}

func catalogModels(rows []store.CatalogShareableModel) []CatalogModel {
	out := make([]CatalogModel, 0, len(rows))
	for _, r := range rows {
		out = append(out, CatalogModel{
			Name: r.Name, UpstreamModel: r.UpstreamModel,
			PriceIn: r.PriceIn, PriceOut: r.PriceOut,
			PriceCacheRead: r.PriceCacheRead, PriceCacheWrite: r.PriceCacheWrite,
			ContextLength: r.ContextLength, BenchTokPerS: r.BenchTokPerS,
			ConcurrencyLimit: r.ConcurrencyLimit, BusyThresholdPct: r.BusyThresholdPct,
		})
	}
	return out
}
