// Manager: the process-wide federation singleton (plan
// 2026-11-22-relay-federation, tasks 2–3). It owns the hub registry (always
// constructible; the connect route role-gates acceptance per request) and
// the spoke dialer (only running while role=spoke), plus the task-3 pieces
// built on those channels: the materializer, the catalog sync controller,
// the outbound tunnel (RoundTripFed), and the inbound frame servers.
// Reload() re-reads the federation settings and reconciles: spoke→standalone
// tears the channel down, standalone→spoke starts it — so
// PUT /api/admin/federation takes effect live, no restart. Safe under
// concurrent Reload (reloadMu serializes whole calls; the dialer stop
// runs outside m.mu — see Reload's locking rule).
package fed

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"llm-relay/internal/store"
	"llm-relay/internal/version"
)

// Manager ties the store-backed settings to every federation mechanism.
type Manager struct {
	db   *sql.DB
	opts Options
	hub  *Hub
	mat  *materializer
	cats *catalogCtl

	// reloadMu serializes Reload/Close end-to-end (rare: admin PUTs and
	// startup/shutdown). m.mu alone cannot do it: dialer.stop must run
	// OUTSIDE m.mu (see Reload), and without a second lock two concurrent
	// Reloads could each start a dialer before the other stopped its own.
	reloadMu sync.Mutex

	mu        sync.Mutex
	dialer    *dialer
	roleVal   atomic.Pointer[string] // reconciled role, lock-free reads
	roleKnown bool
	serve     http.Handler // proxy mux for tunneled requests (AttachServe)

	spoke   atomic.Pointer[Conn]          // live spoke channel (role=spoke)
	spokeID atomic.Pointer[hubIdent]      // hub identity from its hello_ack
	served  map[string]context.CancelFunc // tunneled requests being served
	transit map[string]int64              // hub transit: request id → target peer

	// events carries hub registry connect/die events to the single
	// ordered worker (see peerEvent).
	events chan peerEventMsg

	servedWg       sync.WaitGroup // served requests (Close waits, bounded)
	servedShutdown bool           // Close began: refuse new served work
}

// peerEventMsg is one queued registry liveness event.
type peerEventMsg struct {
	peerID int64
	online bool
}

// peerEventQueue bounds the pending-event queue. Connect/die events are
// rare (channel churn); a full queue is pathological and drops with a log
// rather than stalling the registry.
const peerEventQueue = 64

// NewManager builds the singleton. opts == nil means production defaults.
func NewManager(db *sql.DB, opts *Options) *Manager {
	o := Options{}
	if opts != nil {
		o = *opts
	}
	o = o.withDefaults() // zero fields get production defaults
	if o.InstanceName == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			o.InstanceName = hn
		} else {
			o.InstanceName = "llm-relay"
		}
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.Log == nil {
		o.Log = log.Default()
	}
	m := &Manager{db: db, opts: o,
		hub:     NewHub(db, o, o.InstanceName, o.Version),
		spoke:   atomic.Pointer[Conn]{},
		served:  map[string]context.CancelFunc{},
		transit: map[string]int64{},
		events:  make(chan peerEventMsg, peerEventQueue)}
	m.mat = &materializer{db: db, logf: o.Log.Printf}
	m.cats = newCatalogCtl(m)
	m.hub.SetHooks(m.peerEvent, m.attachChannel)
	go m.runPeerEvents()
	return m
}

// AttachServe wires the /v1 pipeline tunneled requests are served from
// (the proxy's Mux). Called once at startup before Reload; nil-safe until
// set (frame serving answers an explicit error).
func (m *Manager) AttachServe(h http.Handler) {
	m.mu.Lock()
	m.serve = h
	m.mu.Unlock()
}

func (m *Manager) serveHandler() http.Handler {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.serve
}

// Reload reconciles the running spoke dialer with the stored settings.
// The hub side needs nothing to START (acceptance is role-checked per
// request) but must TEAR DOWN live channels when the role moves away
// from hub. reloadMu serializes whole calls, so the settings read runs
// BEFORE m.mu: no interleaved Reload can act on a stale role, and a DB
// read (busy_timeout up to 5 s) must not hold m.mu — the served registry
// and serveHandler run through it on every tunneled request.
//
// A settings read failure keeps the running configuration untouched (see
// below): a read error is not a role change. The error reaches the caller
// so a failed reconcile is visible instead of silently half-applied.
//
// CRITICAL LOCKING RULE: dialer.stop waits for the dialer goroutine to
// exit, and that goroutine may be inside conn.Serve dispatching a frame
// handler — handlers take m.mu (served registry, cancel). Stopping the
// dialer while holding m.mu therefore deadlocks Reload against its own
// channel. So: state swap under m.mu, stop (and start) after releasing it.
// reloadMu keeps concurrent Reloads ordered so exactly one dialer is
// stopped and at most one started per call, in order.
func (m *Manager) Reload() error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	f, err := store.GetFederationSettings(m.db)
	if err != nil {
		// A transient read failure is NOT a role change: keep the running
		// dialer and role exactly as they are (same D1 convention as the
		// hub's Touch sweep — SQLITE_BUSY must not tear down a healthy
		// spoke permanently). The caller reports the failed reconcile;
		// the next Reload retries.
		return fmt.Errorf("fed: reload: settings read failed: %w", err)
	}

	// Tear live hub channels down before the purge: a dying channel's
	// queued catalog frame must not re-materialize rows the purge is
	// about to delete (the onCatalog role check cannot reject them until
	// the new role is committed below). CloseAll is async per connection
	// and takes only h.mu.
	if f.Role != store.FedRoleHub {
		m.hub.CloseAll("role changed")
	}

	// Materialized rows are rebuilt from live catalog frames, so anything
	// left in the DB at startup or across a role change is stale by
	// definition — purge it before anything can serve from it. The purge
	// must succeed BEFORE the new role is committed: committing past a
	// failed purge marks the transition done while the stale rows are
	// still there, and nothing ever retries it. roleKnown/roleVal are
	// Reload-only state (reloadMu), so reading them needs no m.mu.
	prev := ""
	if p := m.roleVal.Load(); p != nil {
		prev = *p
	}
	if !m.roleKnown || prev != f.Role {
		if err := store.PurgeSynthetic(m.db); err != nil {
			return fmt.Errorf("fed: purge synthetic backends: %w", err)
		}
		m.roleKnown = true
	}

	m.mu.Lock()
	old := m.dialer
	m.dialer = nil
	role := f.Role
	m.roleVal.Store(&role)
	var next *dialer
	if f.Role == store.FedRoleSpoke {
		next = newDialer(m.db, f, m.opts, m)
		m.dialer = next
	}
	m.mu.Unlock()

	if old != nil {
		old.stop()
	}
	if next != nil {
		next.start()
	}
	return nil
}

// Close stops the spoke dialer and every live hub channel (process
// shutdown: http.Server.Shutdown does not wait for hijacked connections,
// so the hub's sockets are ours to close), then aborts served tunnel
// requests and waits briefly for them to unwind.
func (m *Manager) Close() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.Lock()
	d := m.dialer
	m.dialer = nil
	m.mu.Unlock()
	if d != nil {
		d.stop() // outside m.mu — see Reload's locking rule
	}
	m.hub.CloseAll("shutdown")

	// Served tunnel requests: their sockets are gone, so their work is
	// pointless — cancel and give them a bounded moment to drain.
	m.mu.Lock()
	m.servedShutdown = true
	for _, cancel := range m.served {
		cancel()
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.servedWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		m.opts.Log.Printf("fed: close: served requests still running after 5s")
	}
}

// Hub exposes the live-peer registry (task 3 routing + Servers page).
func (m *Manager) Hub() *Hub { return m.hub }

// Role reports the reconciled federation role (the API layer's role-
// dependent stats payloads key on it). A manager that has never re-read
// settings behaves as standalone (the stored default).
func (m *Manager) Role() string {
	if r := m.currentRole(); r != "" {
		return r
	}
	return store.FedRoleStandalone
}

// IsHub answers proxy.FedTransport: only the hub's usage_log is the
// fairness ledger, so only hub-side synthetic-backend traffic earns the
// 'consumed' stamp (a spoke's local consumption is identified by the
// backend scheme in the stats query instead).
func (m *Manager) IsHub() bool { return m.currentRole() == store.FedRoleHub }

// Online / Peers / Send delegate to the hub registry, so callers (task 3,
// the API layer) only ever touch the Manager.
func (m *Manager) Online(peerID int64) bool         { return m.hub.Online(peerID) }
func (m *Manager) Peers() []PeerInfo                { return m.hub.Peers() }
func (m *Manager) Send(peerID int64, f Frame) error { return m.hub.Send(peerID, f) }

// KickPeer drops a peer's live channel (disable/rotate/delete on the
// peer endpoints route through here).
func (m *Manager) KickPeer(peerID int64, reason string) { m.hub.Kick(peerID, reason) }

// PeerRemoved tears down a deleted peer's materialized rows (backend +
// models) and stops relaying its catalog. The live channel is the API
// layer's kick to drop; this is the bookkeeping.
//
// Hub-role only: a spoke's local peers.id space and the origin ids stamped
// on materialized rows (hub frames) are independent namespaces, so on a
// spoke a local peer delete would wipe rows belonging to an unrelated
// origin. A spoke materializes nothing keyed by its own peer table — there
// is nothing here it should legitimately remove.
func (m *Manager) PeerRemoved(peerID int64) {
	if m.currentRole() != store.FedRoleHub {
		return
	}
	m.mat.removePeer(peerID)
	m.cats.set(peerID, nil)
	m.cats.changed()
}

// CatalogChanged is the API-layer hook: model fed_visibility/enabled,
// backend-enabled and spoke group edits all re-trigger the (debounced)
// catalog push. No-op cost on a standalone relay (no channels to push to).
func (m *Manager) CatalogChanged() { m.cats.changed() }

// ServeHub is the connect-route body behind PeerAuth: role gate + WS
// handshake + registry (see Hub.ServeConn).
func (m *Manager) ServeHub(w http.ResponseWriter, r *http.Request, p *store.Peer) {
	m.hub.ServeConn(w, r, p)
}

// currentRole reports the reconciled federation role. Lock-free on
// purpose: the hub fires registry events under h.mu, and a role read that
// took m.mu there would invert the m.mu→hub.mu order Reload uses.
func (m *Manager) currentRole() string {
	if p := m.roleVal.Load(); p != nil {
		return *p
	}
	return ""
}

// hubIdent is the hub's self-report (hello_ack name + version) that a
// spoke's live channel shook hands with; published with the channel so the
// admin UI can show which hub it is connected to.
type hubIdent struct{ name, version string }

// setSpokeConn publishes the live channel together with the hub identity
// its handshake produced (the dialer reads hello_ack before the pumps run).
func (m *Manager) setSpokeConn(c *Conn, hubName, hubVersion string) {
	m.spokeID.Store(&hubIdent{name: hubName, version: hubVersion})
	m.spoke.Store(c)
	// Version awareness (plan 2026-11-25-version-awareness): a hub newer
	// than this spoke means THIS server is the old one — error line for
	// the operator (the dashboard mirrors it as the red banner for all
	// users). An older hub is just a note on the Federation page.
	switch st := version.State(m.opts.Version, hubVersion); st {
	case version.Newer:
		m.opts.Log.Printf("VERSION WARNING: hub %q runs %s, newer than this server (%s) - update this relay", hubName, hubVersion, m.opts.Version)
	case version.Older:
		m.opts.Log.Printf("fed: hub %q runs %s, older than this server (%s)", hubName, hubVersion, m.opts.Version)
	}
}

// VersionWarnings reports the version mismatches on this instance's live
// federation channels: connected spokes (hub side) or the hub (spoke
// side). Empty on a standalone box or when every version matches (or is
// unorderable). kind is "spoke" or "hub"; state is older|newer relative
// to THIS build.
func (m *Manager) VersionWarnings() []version.Warn {
	switch m.currentRole() {
	case store.FedRoleHub:
		return m.hub.VersionWarnings()
	case store.FedRoleSpoke:
		if id := m.spokeID.Load(); id != nil && m.spokeConn() != nil {
			st := version.State(m.opts.Version, id.version)
			if st == version.Older || st == version.Newer {
				return []version.Warn{{Name: id.name, Kind: "hub", Version: id.version, State: st}}
			}
		}
	}
	return nil
}

// clearSpokeConn drops the pointer only if it still names c (a replaced
// channel must not be evicted by the old one's exit). The hub identity
// rides the same CAS: it is cleared only together with the channel it
// belongs to, so a replacement's fresh identity is never evicted.
func (m *Manager) clearSpokeConn(c *Conn) {
	for {
		cur := m.spoke.Load()
		if cur != c {
			return
		}
		if m.spoke.CompareAndSwap(cur, nil) {
			m.spokeID.Store(nil)
			return
		}
	}
}

// SpokeStatus reports the spoke-side channel for the admin UI: whether a
// live channel to the hub exists, and the hub's hello_ack identity (empty
// strings when disconnected or before the identity was recorded).
func (m *Manager) SpokeStatus() (connected bool, hubName, hubVersion string) {
	if c := m.spokeConn(); c == nil || c.Closed() {
		return false, "", ""
	}
	if p := m.spokeID.Load(); p != nil {
		return true, p.name, p.version
	}
	return true, "", ""
}

func (m *Manager) spokeConn() *Conn { return m.spoke.Load() }

// peerEvent is the hub registry's connect/die hook: propagate liveness to
// the other spokes (StatusFrame), flip the local synthetic backend, and
// schedule the catalog re-push so relayed entries appear/vanish everywhere.
//
// It runs UNDER the hub registry lock (so online/offline events deliver in
// registry order — an old channel's offline must never land after the new
// channel's online). Nothing expensive may run there: the synthetic flip is
// a DB write (busy_timeout up to 5 s) and the registry lock gates every
// other peer's connect and disconnect for its duration. So the hook only
// enqueues — the buffered channel preserves registry order — and the
// worker does the work off the lock.
func (m *Manager) peerEvent(peerID int64, online bool) {
	if m.currentRole() != store.FedRoleHub {
		return
	}
	select {
	case m.events <- peerEventMsg{peerID: peerID, online: online}:
	default:
		// Pathological: a full queue means the worker is wedged (DB stall).
		// Drop with a loud log rather than stall the registry; the next
		// event re-syncs the peer's state anyway.
		m.opts.Log.Printf("fed: peer event queue full: dropping peer %d online=%v", peerID, online)
	}
}

// runPeerEvents drains the registry event queue in order, outside h.mu.
// One worker keeps events ordered (an offline must not overtake the online
// that follows it). Each event may block on DB writes and on Conn.Send
// (a stalled peer's full write queue, up to WriteTimeout) — safe here,
// nothing on the registry path waits for this worker.
func (m *Manager) runPeerEvents() {
	for ev := range m.events {
		if m.currentRole() != store.FedRoleHub {
			continue // role moved while the event was queued
		}
		m.mat.setPeerOnline(ev.peerID, ev.online)
		if !ev.online {
			// A dead peer's catalog stops being relayed; it re-sends on connect.
			m.cats.set(ev.peerID, nil)
		}
		for _, p := range m.hub.Peers() {
			if p.PeerID == ev.peerID {
				continue
			}
			if err := m.hub.Send(p.PeerID, &Status{T: TStatus, PeerID: ev.peerID, Online: ev.online}); err != nil {
				m.opts.Log.Printf("fed: status peer %d online=%v → peer %d: %v", ev.peerID, ev.online, p.PeerID, err)
			}
		}
		m.cats.changed()
	}
}

// SyntheticOnline answers the health cache for a synthetic backend: the
// channel IS the health. peer:// on the hub consults the registry; on a
// spoke everything materialized rides the single hub channel (a dead third
// spoke is handled by the StatusFrame disabling its backend).
func (m *Manager) SyntheticOnline(b store.Backend) bool {
	kind, peerID, ok := store.SyntheticBaseURL(b.BaseURL)
	if !ok {
		return true
	}
	if m.currentRole() == store.FedRoleHub {
		return m.hub.Online(peerID)
	}
	_ = kind
	c := m.spokeConn()
	return c != nil && !c.Closed()
}
