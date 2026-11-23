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
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"llm-relay/internal/store"
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
	served  map[string]context.CancelFunc // tunneled requests being served
	transit map[string]int64              // hub transit: request id → target peer

	servedWg       sync.WaitGroup // served requests (Close waits, bounded)
	servedShutdown bool           // Close began: refuse new served work
}

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
		served:  map[string]context.CancelFunc{},
		transit: map[string]int64{}}
	m.mat = &materializer{db: db, logf: o.Log.Printf}
	m.cats = newCatalogCtl(m)
	m.hub.SetHooks(m.peerEvent, m.attachChannel)
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
// from hub. The settings read happens under m.mu: reading before locking
// lets interleaved Reloads start a dialer from a stale role.
//
// CRITICAL LOCKING RULE: dialer.stop waits for the dialer goroutine to
// exit, and that goroutine may be inside conn.Serve dispatching a frame
// handler — handlers take m.mu (served registry, cancel). Stopping the
// dialer while holding m.mu therefore deadlocks Reload against its own
// channel. So: state swap under m.mu, stop (and start) after releasing it.
// reloadMu keeps concurrent Reloads ordered so exactly one dialer is
// stopped and at most one started per call, in order.
func (m *Manager) Reload() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.Lock()
	f, err := store.GetFederationSettings(m.db)
	old := m.dialer
	m.dialer = nil
	if err != nil {
		m.opts.Log.Printf("fed: reload: settings read failed: %v", err)
		m.mu.Unlock()
		if old != nil {
			old.stop()
		}
		return
	}
	if f.Role != store.FedRoleHub {
		// Async per connection (CloseAll spawns goroutines), so holding
		// m.mu here is fine.
		m.hub.CloseAll("role changed")
	}
	prev := ""
	if p := m.roleVal.Load(); p != nil {
		prev = *p
	}
	// Materialized rows are rebuilt from live catalog frames, so anything
	// left in the DB at startup or across a role change is stale by
	// definition — purge it before anything can serve from it.
	if !m.roleKnown || prev != f.Role {
		if err := store.PurgeSynthetic(m.db); err != nil {
			m.opts.Log.Printf("fed: purge synthetic backends: %v", err)
		}
	}
	m.roleKnown = true
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

func (m *Manager) setSpokeConn(c *Conn) { m.spoke.Store(c) }

// clearSpokeConn drops the pointer only if it still names c (a replaced
// channel must not be evicted by the old one's exit).
func (m *Manager) clearSpokeConn(c *Conn) {
	for {
		cur := m.spoke.Load()
		if cur != c {
			return
		}
		if m.spoke.CompareAndSwap(cur, nil) {
			return
		}
	}
}

func (m *Manager) spokeConn() *Conn { return m.spoke.Load() }

// peerEvent is the hub registry's connect/die hook: propagate liveness to
// the other spokes (StatusFrame), flip the local synthetic backend, and
// schedule the catalog re-push so relayed entries appear/vanish everywhere.
//
// It runs UNDER the hub registry lock (so online/offline events deliver in
// registry order — an old channel's offline must never land after the new
// channel's online). Two rules follow from that: no Hub callbacks
// synchronously (h.mu is not reentrant) and no m.mu (lock order). The
// Status fan-out therefore runs on its own goroutine: Conn.Send blocks on
// a full write queue (up to WriteTimeout), and a stalled peer must not
// stall register() for every new connection.
func (m *Manager) peerEvent(peerID int64, online bool) {
	if m.currentRole() != store.FedRoleHub {
		return
	}
	m.mat.setPeerOnline(peerID, online)
	if !online {
		// A dead peer's catalog stops being relayed; it re-sends on connect.
		m.cats.set(peerID, nil)
	}
	go func() {
		for _, p := range m.hub.Peers() {
			if p.PeerID == peerID {
				continue
			}
			if err := m.hub.Send(p.PeerID, &Status{T: TStatus, PeerID: peerID, Online: online}); err != nil {
				m.opts.Log.Printf("fed: status peer %d online=%v → peer %d: %v", peerID, online, p.PeerID, err)
			}
		}
	}()
	m.cats.changed()
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
