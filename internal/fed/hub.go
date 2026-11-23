// Hub side (plan 2026-11-22-relay-federation, task 2): accept spoke
// upgrades, run the hello handshake, keep one live Conn per peer. A new
// connection for the same peer REPLACES the old one (spoke restarts must
// not wait out a pong timeout on the dead socket). The registry is the
// seam task 3 builds on: Send routes frames to an online peer, Online
// gates routing decisions.
package fed

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/store"
)

// ErrNotOnline is returned by Send for a peer with no live channel.
var ErrNotOnline = errors.New("fed peer not online")

// PeerInfo describes one live channel (Servers page / task 3 routing).
type PeerInfo struct {
	PeerID      int64     `json:"peer_id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	ConnectedAt time.Time `json:"connected_at"`
}

// entry pairs a live Conn with its hello-reported identity.
type entry struct {
	conn *Conn
	info PeerInfo
}

// Hub owns the live peer registry. It is always constructible; whether
// connections are *accepted* is a role decision made per request (the
// connect route 404s unless role=hub), so a role flip needs no restart.
type Hub struct {
	db      *sql.DB
	opts    Options
	name    string // instance name + version echoed in hello_ack
	version string

	mu      sync.Mutex
	entries map[int64]*entry
}

func NewHub(db *sql.DB, opts Options, name, version string) *Hub {
	// Same defaulting contract as NewManager: a zero Options must not
	// mean nil Log or a zero HelloTimeout that rejects every peer.
	return &Hub{db: db, opts: opts.withDefaults(), name: name, version: version,
		entries: map[int64]*entry{}}
}

// Online reports whether the peer currently holds a live channel.
func (h *Hub) Online(peerID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[peerID]
	return ok && !e.conn.Closed()
}

// Peers lists the live channels.
func (h *Hub) Peers() []PeerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]PeerInfo, 0, len(h.entries))
	for _, e := range h.entries {
		if !e.conn.Closed() {
			out = append(out, e.info)
		}
	}
	return out
}

// Send queues a frame to an online peer; ErrNotOnline when it has no
// channel (task 3's tunnel turns that into a routing miss).
func (h *Hub) Send(peerID int64, f Frame) error {
	h.mu.Lock()
	e, ok := h.entries[peerID]
	h.mu.Unlock()
	if !ok || e.conn.Closed() {
		return ErrNotOnline
	}
	return e.conn.Send(f)
}

// Kick closes the peer's live channel with 1008 + reason (async: the
// close handshake is bounded but never the caller's problem). Used by the
// peer mutation endpoints — disable, rotate, delete all kill the live
// channel immediately, not at the next sweep. A spoke that survives on a
// rotated token reconnects with the new one if its config was updated.
func (h *Hub) Kick(peerID int64, reason string) {
	h.mu.Lock()
	e, ok := h.entries[peerID]
	h.mu.Unlock()
	if ok {
		go e.conn.Close(websocket.StatusPolicyViolation, reason)
	}
}

// CloseAll closes every live channel (role moved away from hub, process
// shutdown). Async per connection, like Kick.
func (h *Hub) CloseAll(reason string) {
	h.mu.Lock()
	entries := make([]*entry, 0, len(h.entries))
	for _, e := range h.entries {
		entries = append(entries, e)
	}
	h.mu.Unlock()
	for _, e := range entries {
		go e.conn.Close(websocket.StatusPolicyViolation, reason)
	}
}

// ServeConn is the /federation/v1/connect body, reached behind PeerAuth
// (the peer is authenticated already). Order: role gate → WS upgrade →
// hello within HelloTimeout → version check → hello_ack → registry.
func (h *Hub) ServeConn(w http.ResponseWriter, r *http.Request, peer *store.Peer) {
	f, err := store.GetFederationSettings(h.db)
	if err != nil || f.Role != store.FedRoleHub {
		// Not a hub right now: pretend the endpoint does not exist
		// (standalone/spoke must not advertise a federation surface).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		return
	}
	// InsecureSkipVerify: the client is a server, not a browser — it
	// sends no Origin header. The fed- bearer token is the authenticator
	// (PeerAuth already checked it); cross-site WS hijacking does not
	// apply to a non-cookie auth surface.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.opts.Log.Printf("fed: accept peer %d: %v", peer.ID, err)
		return
	}
	ver, err := h.handshake(ws, peer)
	if err != nil {
		h.opts.Log.Printf("fed: handshake peer %d: %v", peer.ID, err)
		return
	}
	// Per-connection opts: Touch is this peer's bookkeeping closure, so
	// h.opts itself is never mutated (concurrent connects share it). It
	// doubles as the revocation sweep: at most once per TouchThrottle it
	// revalidates the peer row — gone, disabled, or owner disabled closes
	// the channel with "revoked". That catches revocations that did not
	// come through the kick endpoints (e.g. a user disabled via the
	// admin users API). Touch only runs from the read pump, which starts
	// after conn is assigned.
	opts := h.opts
	var conn *Conn
	opts.Touch = func() {
		exists, enabled, ownerDis, err := store.PeerAuthState(h.db, peer.ID)
		if err != nil {
			h.opts.Log.Printf("fed: revalidate peer %d: %v", peer.ID, err)
			return // transient DB error: keep the channel, retry next sweep
		}
		if !exists || !enabled || ownerDis {
			conn.Close(websocket.StatusPolicyViolation, "revoked")
			return
		}
		if err := store.TouchPeer(h.db, peer.ID, time.Now().UTC().Format(time.RFC3339), ver); err != nil {
			h.opts.Log.Printf("fed: touch peer %d: %v", peer.ID, err)
		}
	}
	conn = NewConn(ws, opts)
	e := &entry{conn: conn, info: PeerInfo{
		PeerID: peer.ID, Name: peer.Name, Version: ver,
		ConnectedAt: time.Now(),
	}}
	h.register(e)
	// The handler may return while the channel lives: Accept hijacked the
	// connection, so net/http will not close it on handler exit, and the
	// pumps run on their own context (never r.Context(), which dies with
	// the request).
	go func() {
		conn.Serve(context.Background())
		h.unregister(peer.ID, conn)
	}()
}

// handshake reads+validates hello and replies hello_ack, using raw socket
// reads: the Conn pumps only start afterwards, so exactly one frame is
// consumed here. It returns the spoke's reported version.
//
// The hello read runs on its own goroutine rather than a context timeout:
// cancelling a pending websocket.Read tears the socket down without a
// close frame, and the contract is a proper 1008 close on timeout.
func (h *Hub) handshake(ws *websocket.Conn, peer *store.Peer) (string, error) {
	type readRes struct {
		data []byte
		err  error
	}
	reads := make(chan readRes, 1)
	go func() {
		_, data, err := ws.Read(context.Background())
		reads <- readRes{data, err}
	}()
	var data []byte
	select {
	case r := <-reads:
		if r.err != nil {
			_ = ws.Close(websocket.StatusPolicyViolation, "hello required")
			return "", errors.New("hello not received")
		}
		data = r.data
	case <-time.After(h.opts.HelloTimeout):
		// Close unblocks the pending read (after its internal handshake
		// wait), so it goes from a goroutine.
		go func() { _ = ws.Close(websocket.StatusPolicyViolation, "hello required") }()
		return "", errors.New("hello timeout")
	}
	f, err := Decode(data)
	if err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "hello required")
		return "", err
	}
	hello, ok := f.(*Hello)
	if !ok {
		_ = ws.Close(websocket.StatusPolicyViolation, "hello required")
		return "", errors.New("first frame is not hello")
	}
	if hello.Proto != Protocol {
		_ = ws.Close(websocket.StatusProtocolError, "proto mismatch")
		return "", errors.New("proto mismatch")
	}
	ack, err := Encode(&HelloAck{T: THelloAck, Proto: Protocol,
		Name: h.name, Version: h.version})
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "encode ack")
		return "", err
	}
	wctx, wcancel := context.WithTimeout(context.Background(), h.opts.WriteTimeout)
	defer wcancel()
	if err := ws.Write(wctx, websocket.MessageText, ack); err != nil {
		// The socket is hijacked — net/http will not close it on handler
		// exit, so an abandoned handshake must close it explicitly.
		_ = ws.Close(websocket.StatusInternalError, "ack write failed")
		return "", err
	}
	// Version bookkeeping starts here; the Conn's throttled Touch keeps
	// last_seen fresh while the channel lives.
	if err := store.TouchPeer(h.db, peer.ID, time.Now().UTC().Format(time.RFC3339), hello.Version); err != nil {
		h.opts.Log.Printf("fed: touch peer %d: %v", peer.ID, err)
	}
	return hello.Version, nil
}

// register installs e, replacing (and closing) any previous channel for
// the same peer: a spoke restart takes over immediately instead of
// leaving the hub waiting for the dead socket's pong timeout.
func (h *Hub) register(e *entry) {
	h.mu.Lock()
	old := h.entries[e.info.PeerID]
	h.entries[e.info.PeerID] = e
	h.mu.Unlock()
	if old != nil && old.conn != e.conn {
		go old.conn.Close(websocket.StatusPolicyViolation, "replaced")
	}
}

// unregister drops the peer only if the entry is still this conn — a
// replacement must not be evicted by the old channel's exit.
func (h *Hub) unregister(peerID int64, c *Conn) {
	h.mu.Lock()
	if e, ok := h.entries[peerID]; ok && e.conn == c {
		delete(h.entries, peerID)
	}
	h.mu.Unlock()
}
