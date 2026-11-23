// Manager: the process-wide federation singleton (plan
// 2026-11-22-relay-federation, task 2). It owns the hub registry (always
// constructible; the connect route role-gates acceptance per request) and
// the spoke dialer (only running while role=spoke). Reload() re-reads the
// federation settings and reconciles: spoke→standalone tears the channel
// down, standalone→spoke starts it — so PUT /api/admin/federation takes
// effect live, no restart. Safe under concurrent Reload (one mutex, old
// dialer fully stopped before a new one starts).
package fed

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"sync"

	"llm-relay/internal/store"
)

// Manager ties the store-backed settings to the two channel sides.
type Manager struct {
	db   *sql.DB
	opts Options
	hub  *Hub

	mu     sync.Mutex
	dialer *dialer
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
	return &Manager{db: db, opts: o, hub: NewHub(db, o, o.InstanceName, o.Version)}
}

// Reload reconciles the running spoke dialer with the stored settings.
// The hub side needs nothing to START (acceptance is role-checked per
// request) but must TEAR DOWN live channels when the role moves away
// from hub. The settings read happens under m.mu: reading before locking
// lets interleaved Reloads start a dialer from a stale role.
func (m *Manager) Reload() {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := store.GetFederationSettings(m.db)
	if m.dialer != nil {
		m.dialer.stop()
		m.dialer = nil
	}
	if err != nil {
		m.opts.Log.Printf("fed: reload: settings read failed: %v", err)
		return
	}
	if f.Role != store.FedRoleHub {
		// Async per connection (CloseAll spawns goroutines), so holding
		// m.mu here is fine.
		m.hub.CloseAll("role changed")
	}
	if f.Role == store.FedRoleSpoke {
		m.dialer = newDialer(m.db, f, m.opts)
		m.dialer.start()
	}
}

// Close stops the spoke dialer and every live hub channel (process
// shutdown: http.Server.Shutdown does not wait for hijacked connections,
// so the hub's sockets are ours to close).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dialer != nil {
		m.dialer.stop()
		m.dialer = nil
	}
	m.hub.CloseAll("shutdown")
}

// Hub exposes the live-peer registry (task 3 routing + Servers page).
func (m *Manager) Hub() *Hub { return m.hub }

// Online / Peers / Send delegate to the hub registry, so callers (task 3,
// the API layer) only ever touch the Manager.
func (m *Manager) Online(peerID int64) bool         { return m.hub.Online(peerID) }
func (m *Manager) Peers() []PeerInfo                { return m.hub.Peers() }
func (m *Manager) Send(peerID int64, f Frame) error { return m.hub.Send(peerID, f) }

// KickPeer drops a peer's live channel (disable/rotate/delete on the
// peer endpoints route through here).
func (m *Manager) KickPeer(peerID int64, reason string) { m.hub.Kick(peerID, reason) }

// ServeHub is the connect-route body behind PeerAuth: role gate + WS
// handshake + registry (see Hub.ServeConn).
func (m *Manager) ServeHub(w http.ResponseWriter, r *http.Request, p *store.Peer) {
	m.hub.ServeConn(w, r, p)
}
