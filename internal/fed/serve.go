// The inbound tunnel: serving tunneled frames (plan 2026-11-22-relay-federation,
// task 3). Every ReqFrame is answered on its own goroutine (handlers run on
// the read pump and must never block it) against this relay's own /v1
// pipeline, with a proxy.FedServe identity injected:
//
//   - Hub, TargetPeer=0: serve as the requesting peer's OWNER user (the
//     hub meters the traffic against its own account; attribution columns
//     land with task 4). The sharing gate (403) and the loop-guard member
//     exclusion run inside the pipeline off OriginPeer.
//   - Hub, TargetPeer=A: transit — forward to A with OriginPeer set to the
//     requester, never back to the origin, and pump A's answer frames on to
//     the requester. A's channel slot is held open for the duration, so
//     transit respects A's in-flight cap too.
//   - Spoke: local http(s) members only (LocalOnly: never re-forward to the
//     hub or another spoke), stored backend keys applied locally by the
//     normal proxy path, no served-side metering (task 3 scope).
//
// CancelFrame aborts the served request's context on either side; the hub
// forwards cancels along the transit path.
package fed

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// attachChannel wires the task-3 handlers onto a live channel. peer is the
// authenticated hub-side peer (nil on a spoke's channel to its hub).
func (m *Manager) attachChannel(c *Conn, peer *store.Peer) {
	c.On(TCatalog, func(c *Conn, f Frame) { m.onCatalog(c, peer, f) })
	// Status frames flow hub→spoke ONLY: a peer that could push
	// status{victim} would flip another peer's materialized backend at the
	// hub, so hub-side channels register no handler and the pump ignores
	// the frame (unknown-to-this-channel types are dropped, not fatal).
	if peer == nil {
		c.On(TStatus, func(c *Conn, f Frame) { m.onStatus(f) })
	}
	c.On(TReq, func(c *Conn, f Frame) { m.onReq(c, peer, f) })
	c.On(TCancel, func(c *Conn, f Frame) { m.onCancel(f) })
}

// ------------------------------------------------------------- catalog/status

func (m *Manager) onCatalog(c *Conn, peer *store.Peer, f Frame) {
	cat, ok := f.(*Catalog)
	if !ok {
		return
	}
	// Role/channel re-check before touching the DB: a dying channel's pump
	// can dispatch a queued frame shortly after a Reload purged every
	// synthetic row (role transition) or a reconnect replaced the channel;
	// re-materializing from that stale frame would resurrect pruned rows on
	// an instance that no longer speaks this direction.
	switch {
	case peer != nil && m.currentRole() != store.FedRoleHub:
		m.opts.Log.Printf("fed: catalog from peer %d dropped: no longer hub", peer.ID)
		return
	case peer != nil && m.hub.connFor(peer.ID) != c:
		m.opts.Log.Printf("fed: catalog from peer %d dropped: channel replaced", peer.ID)
		return
	case peer == nil && m.currentRole() != store.FedRoleSpoke:
		m.opts.Log.Printf("fed: catalog from hub dropped: no longer spoke")
		return
	case peer == nil && c != m.spokeConn():
		m.opts.Log.Printf("fed: catalog on replaced channel dropped")
		return
	}
	if peer != nil {
		// Hub side: a spoke's own catalog. Keep it in memory (relay source
		// for the other spokes) and materialize the peer:// backend.
		m.cats.set(peer.ID, cat.Models)
		m.mat.peerBackend(peer.ID, peer.Name, cat)
		// Other spokes must see the new entries (and stale ones pruned).
		m.cats.changed()
		return
	}
	// Spoke side: a frame of the hub's merged catalog (origin-labeled).
	m.mat.hubFrame(cat)
}

func (m *Manager) onStatus(f Frame) {
	st, ok := f.(*Status)
	if !ok {
		return
	}
	// Only spokes receive these (the hub is the broadcaster): a peer went
	// online/offline — flip its synthetic backend. Rows stay for id stability.
	m.mat.setPeerOnline(st.PeerID, st.Online)
}

// ------------------------------------------------------------- request serving

// servedWriter is an http.ResponseWriter that emits res/res_body/res_end
// frames: WriteHeader becomes the Res head, every Write becomes one (or
// more) ResBody chunk(s) at MaxBody granularity, and the handler returning
// is the ResEnd. Flush is a no-op — each Write already ships.
type servedWriter struct {
	id     string
	send   func(Frame) error
	header http.Header
	once   sync.Once
	bad    bool
}

func (w *servedWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *servedWriter) WriteHeader(code int) {
	w.once.Do(func() {
		if err := w.send(&Res{T: TRes, ID: w.id, Status: code,
			Headers: responseHeaders(w.header)}); err != nil {
			w.bad = true
		}
	})
}

func (w *servedWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.bad {
		return 0, ErrClosed
	}
	n := len(p)
	for len(p) > 0 {
		c := min(len(p), MaxBody)
		if err := w.send(&ResBody{T: TResBody, ID: w.id, Data: p[:c]}); err != nil {
			w.bad = true
			return 0, err
		}
		p = p[c:]
	}
	return n, nil
}

func (w *servedWriter) Flush() {}

// onReq handles a tunneled request on the receiving side.
func (m *Manager) onReq(c *Conn, peer *store.Peer, f Frame) {
	req, ok := f.(*Req)
	if !ok {
		return
	}
	if len(req.Body) > MaxBody {
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID,
			Error: fmt.Sprintf("request body %d bytes over the %d cap", len(req.Body), MaxBody)})
		return
	}
	// OriginPeer is RECEIVER-derived, never sender-supplied: on a hub
	// channel the origin is ALWAYS the authenticated peer (a spoke must not
	// be able to claim another peer's id on the frame and inherit that
	// peer's sharing rights), and on a spoke channel the counterparty is
	// the hub → 0. Transit stamps the onward origin at the hub
	// (transitFrame), where it is trustworthy.
	var origin int64
	if peer != nil {
		origin = peer.ID
	}
	switch {
	case req.TargetPeer == 0:
		go m.serveFrame(c, peer, origin, req)
	case peer != nil && req.TargetPeer == peer.ID:
		// Loop guard: never route a request back to where it came from.
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "loop guard: request returned to its origin"})
	case peer != nil:
		go m.transitFrame(peer.ID, origin, req)
	default:
		// A spoke never transits (it has no other spokes).
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "spoke cannot transit requests"})
	}
}

// serveFrame runs one tunneled request through the local /v1 pipeline and
// pumps the answer back as frames on c.
func (m *Manager) serveFrame(c *Conn, peer *store.Peer, origin int64, req *Req) {
	handler := m.serveHandler()
	if handler == nil {
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "federation serving not wired"})
		return
	}
	// Only the model pipeline is exposed to the federation; the connect
	// endpoints and MCP live on the same mux but are not for tunneling.
	if !strings.HasPrefix(req.Path, "/v1/") {
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "path not served over the federation"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	// The channel dying aborts everything it was serving: a generation
	// must not keep burning an upstream into a socket that can no longer
	// deliver it. (Close also cancels every served ctx at shutdown.)
	watchStop := make(chan struct{})
	go func() {
		select {
		case <-c.Done():
			cancel()
		case <-watchStop:
		}
	}()
	if !m.registerServed(req.ID, cancel) {
		close(watchStop)
		cancel()
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "federation serving shutting down"})
		return
	}
	defer func() {
		close(watchStop)
		m.unregisterServed(req.ID)
		cancel()
	}()

	fs := &proxy.FedServe{OriginPeer: origin}
	if peer != nil {
		// Hub side: meter against the peer's owner user with a synthetic
		// key (usage_log.api_key_id has no FK; task 4 adds attribution).
		owner, err := store.UserByID(m.db, peer.OwnerUserID)
		if err != nil {
			m.opts.Log.Printf("fed: peer %d owner lookup: %v", peer.ID, err)
			_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "peer owner unavailable"})
			return
		}
		fs.User = owner
		fs.Key = &store.APIKey{ID: 0, Name: "federation"}
	} else {
		// Spoke side: anonymous identity, local engines only, no metering.
		fs.User = &store.User{ID: 0, Username: "federation", PriceMultiplier: 1}
		fs.Key = &store.APIKey{ID: 0, Name: "federation"}
		fs.LocalOnly = true
		fs.NoMeter = true
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, "http://federation"+req.Path,
		bytes.NewReader(req.Body))
	if err != nil {
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "bad request: " + err.Error()})
		return
	}
	httpReq.Header = headersFrom(req.Headers)
	if httpReq.Header.Get("Content-Type") == "" && len(req.Body) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq = httpReq.WithContext(proxy.WithFedServe(httpReq.Context(), fs))
	// A recorded request needs a RemoteAddr-shaped value; the recorder only
	// stores it, so a fixed federation marker is honest and leak-free.
	httpReq.RemoteAddr = "federation:0"

	w := &servedWriter{id: req.ID, send: c.Send}
	defer func() {
		if r := recover(); r != nil {
			m.opts.Log.Printf("fed: served request %s panicked: %v", req.ID, r)
			_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: fmt.Sprintf("internal error: %v", r)})
			return
		}
		_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID})
	}()
	handler.ServeHTTP(w, httpReq)
}

// transitFrame forwards a spoke→spoke request onward (hub side): the frame
// keeps its id, gains the requester as OriginPeer, and the answer frames
// are pumped back to the requesting channel. A's in-flight slot is held by
// OpenRequest, so transit respects the target's cap.
func (m *Manager) transitFrame(fromPeer, origin int64, req *Req) {
	if origin == req.TargetPeer {
		_ = m.hub.Send(fromPeer, &ResEnd{T: TResEnd, ID: req.ID,
			Error: "loop guard: request returned to its origin"})
		return
	}
	ch, release, err := m.hub.OpenRequest(req.TargetPeer, req.ID)
	if err != nil {
		_ = m.hub.Send(fromPeer, &ResEnd{T: TResEnd, ID: req.ID, Error: err.Error()})
		return
	}
	fwd := *req
	fwd.OriginPeer = origin
	// From the target's view this is a serve-here request: the routing
	// hop is over (and a spoke would reject a non-zero TargetPeer — it has
	// nobody to transit to).
	fwd.TargetPeer = 0
	if err := m.hub.Send(req.TargetPeer, &fwd); err != nil {
		release()
		_ = m.hub.Send(fromPeer, &ResEnd{T: TResEnd, ID: req.ID, Error: err.Error()})
		return
	}
	m.registerTransit(req.ID, req.TargetPeer)
	from := m.hub.connFor(fromPeer)
	go func() {
		defer func() {
			m.unregisterTransit(req.ID)
			release()
		}()
		for {
			select {
			case f := <-ch:
				_, done := f.(*ResEnd)
				if from == nil || from.Closed() || from.Send(f) != nil {
					// The requester vanished mid-stream: stop the target
					// from generating into the void (a clean ResEnd means
					// the answer was complete, nothing to abort).
					if !done {
						_ = m.hub.Send(req.TargetPeer, &Cancel{T: TCancel, ID: req.ID})
					}
					return
				}
				if done {
					return
				}
			case <-m.hub.connDone(fromPeer):
				// The requester died mid-flight: tell the target to stop.
				_ = m.hub.Send(req.TargetPeer, &Cancel{T: TCancel, ID: req.ID})
				return
			}
		}
	}()
}

func (m *Manager) onCancel(f Frame) {
	cancel, ok := f.(*Cancel)
	if !ok {
		return
	}
	if fn := m.takeServed(cancel.ID); fn != nil {
		fn()
		return
	}
	// Transit cancel (hub side): forward along the path the request took.
	if target := m.takeTransit(cancel.ID); target != 0 {
		_ = m.hub.Send(target, &Cancel{T: TCancel, ID: cancel.ID})
	}
}

// ------------------------------------------------------------- registries

// registerServed tracks a served request's cancel and counts it on the
// shutdown WaitGroup. It returns false once Close has begun shutdown: the
// caller must refuse the request rather than start work nobody will wait
// for (and adding to the WaitGroup during Close's Wait would be misuse).
func (m *Manager) registerServed(id string, cancel context.CancelFunc) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.servedShutdown {
		return false
	}
	m.served[id] = cancel
	m.servedWg.Add(1)
	return true
}

func (m *Manager) unregisterServed(id string) {
	m.mu.Lock()
	delete(m.served, id)
	m.mu.Unlock()
	m.servedWg.Done()
}

func (m *Manager) takeServed(id string) context.CancelFunc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.served[id]
}

func (m *Manager) registerTransit(id string, target int64) {
	m.mu.Lock()
	m.transit[id] = target
	m.mu.Unlock()
}

func (m *Manager) unregisterTransit(id string) {
	m.mu.Lock()
	delete(m.transit, id)
	m.mu.Unlock()
}

func (m *Manager) takeTransit(id string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transit[id]
}

// compile-time proof that the frame writer satisfies the ResponseWriter
// contract the proxy relies on (Header/Write/WriteHeader + optional Flusher).
var (
	_ http.ResponseWriter = (*servedWriter)(nil)
	_ http.Flusher        = (*servedWriter)(nil)
)
