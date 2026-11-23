// The inbound tunnel: serving tunneled frames (plan 2026-11-22-relay-federation,
// task 3). Every ReqFrame is answered on its own goroutine (handlers run on
// the read pump and must never block it) against this relay's own /v1
// pipeline, with a proxy.FedServe identity injected:
//
//   - Hub, TargetPeer=0: serve as the requesting peer's OWNER user (the
//     hub meters the traffic against its own account, stamped
//     fed_direction='consumed' — counterparty = the hosting peer when
//     the resolved member is synthetic, NULL when hub hardware served;
//     the pipeline mirrors the served row for synthetic members). The
//     sharing gate (403) and the loop-guard member exclusion run
//     inside the pipeline off OriginPeer.
//   - Hub, TargetPeer=A: transit — forward to A with the requester's
//     hub-side identity stamped on FedUser, never back to the origin,
//     and pump A's answer frames on to the requester. A's channel slot is
//     held open for the duration, so transit respects A's in-flight cap
//     too. The hub meters transit from the pumped frames (consumed +
//     served pair, meterTransit).
//   - Spoke: local http(s) members only (LocalOnly: never re-forward to the
//     hub or another spoke), stored backend keys applied locally by the
//     normal proxy path, served rows stamped fed_direction='served' with
//     the hub-stamped FedUser.
//
// CancelFrame aborts the served request's context on either side; the hub
// forwards cancels along the transit path.
package fed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"llm-relay/internal/pricing"
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
		// key (usage_log.api_key_id has no FK); the row is the peer's
		// account CONSUMING capacity — fed_peer_id = the hosting peer
		// when the resolved member is synthetic, NULL when hub hardware
		// served (the hub has no peer row for itself; see proxy.fedOf).
		owner, err := store.UserByID(m.db, peer.OwnerUserID)
		if err != nil {
			m.opts.Log.Printf("fed: peer %d owner lookup: %v", peer.ID, err)
			_ = c.Send(&ResEnd{T: TResEnd, ID: req.ID, Error: "peer owner unavailable"})
			return
		}
		fs.User = owner
		fs.Key = &store.APIKey{ID: 0, Name: "federation"}
		fs.FedDirection = proxy.FedConsumed
	} else {
		// Spoke side: the federation user identity meters this box's
		// SERVED rows; the hub-stamped consumer name rides the frame
		// (fed_user) so the spoke can say who it served without ever
		// seeing a spoke-local username from anyone else. Local engines
		// only.
		fs.User = &store.User{ID: 0, Username: "federation", PriceMultiplier: 1}
		fs.Key = &store.APIKey{ID: 0, Name: "federation"}
		fs.LocalOnly = true
		fs.FedDirection = proxy.FedServed
		fs.FedUser = req.FedUser
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
// keeps its id, gains the requester's hub-side identity (FedUser), and the
// answer frames are pumped back to the requesting channel. A's in-flight
// slot is held by OpenRequest, so transit respects the target's cap.
//
// Transit bypasses the proxy pipeline, so the hub meters it here from the
// frames it pumps: a consumed row (A's account spent B's capacity) and a
// served row (B served A), both with the token counts parsed out of the
// response — the same counts B meters on its own served row.
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
	// The target derives the origin from the channel it arrives on (the
	// hub), so OriginPeer on the wire is decorative — cleared, not
	// trusted. Attribution rides FedUser instead: the origin peer's owner
	// name, which only the hub can stamp.
	fwd.OriginPeer = 0
	if name, err := m.peerOwnerName(fromPeer); err == nil {
		fwd.FedUser = name
	} else {
		m.opts.Log.Printf("fed: transit attribution peer %d: %v", fromPeer, err)
	}
	// From the target's view this is a serve-here request: the routing
	// hop is over (and a spoke would reject a non-zero TargetPeer — it has
	// nobody to transit to).
	fwd.TargetPeer = 0
	mt := &transitMeter{start: time.Now(), path: req.Path, reqBody: req.Body}
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
				switch fr := f.(type) {
				case *Res:
					mt.status = fr.Status
					mt.stream = strings.Contains(headersFrom(fr.Headers).Get("Content-Type"), "text/event-stream")
				case *ResBody:
					mt.feed(fr.Data)
				case *ResEnd:
					if fr.Error == "" {
						m.meterTransit(fromPeer, req.TargetPeer, mt)
					}
				}
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

// transitMeter captures what the hub needs to meter one transit request:
// the response head (status, stream-ness) and a tail-capped copy of the
// body (usage payloads ride the END of both JSON bodies and SSE streams,
// so keeping the tail is enough and a long stream stays bounded).
type transitMeter struct {
	start   time.Time
	path    string
	reqBody []byte
	status  int
	stream  bool
	tail    []byte
}

const transitMeterCap = 512 << 10

func (mt *transitMeter) feed(p []byte) {
	if len(mt.tail) >= transitMeterCap {
		return
	}
	mt.tail = append(mt.tail, p...)
	if len(mt.tail) > transitMeterCap {
		mt.tail = append(mt.tail[:0:0], mt.tail[len(mt.tail)-transitMeterCap:]...)
	}
}

// meterTransit writes the hub's double-entry pair for one completed
// spoke→spoke request: consumed (origin peer's owner account, counterparty
// = target host) and served (target's owner, counterparty = target host —
// the uniform host stamp — fed_user = origin owner name, what the target's
// own served row will carry). Costs
// are nominal (no user multiplier): the fairness ledger compares capacity,
// not billed money.
func (m *Manager) meterTransit(fromPeer, targetPeer int64, mt *transitMeter) {
	endpoint := fedEndpointFor(mt.path)
	if endpoint == "" {
		return // not a metered endpoint (defensive: serveFrame gates /v1/*)
	}
	var tok pricing.Tokens
	var ok bool
	if mt.stream {
		tok, ok = proxy.StreamUsage(endpoint, mt.tail)
	} else {
		tok, ok = proxy.ExtractUsage(endpoint, mt.tail)
	}
	if !ok {
		m.opts.Log.Printf("fed: transit %s: no usage in response, no ledger rows", mt.path)
		return
	}
	var in struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(mt.reqBody, &in); err != nil || in.Model == "" {
		m.opts.Log.Printf("fed: transit %s: request model name unreadable, no ledger rows", mt.path)
		return
	}
	mdl, err := store.FedPeerModel(m.db, targetPeer, in.Model)
	if err != nil || mdl == nil {
		m.opts.Log.Printf("fed: transit %s: no materialized row for %q on peer %d: %v",
			mt.path, in.Model, targetPeer, err)
		return
	}
	fromP, err := store.PeerByID(m.db, fromPeer)
	if err != nil {
		m.opts.Log.Printf("fed: transit origin peer %d: %v", fromPeer, err)
		return
	}
	toP, err := store.PeerByID(m.db, targetPeer)
	if err != nil {
		m.opts.Log.Printf("fed: transit target peer %d: %v", targetPeer, err)
		return
	}
	fromU, err := store.UserByID(m.db, fromP.OwnerUserID)
	if err != nil {
		m.opts.Log.Printf("fed: transit origin owner: %v", err)
		return
	}
	toU, err := store.UserByID(m.db, toP.OwnerUserID)
	if err != nil {
		m.opts.Log.Printf("fed: transit target owner: %v", err)
		return
	}
	costs := pricing.Compute(mdl, tok)
	base := store.UsageEvent{
		TS:            time.Now().UTC().Format(time.RFC3339),
		KeyID:         0, // synthetic federation key (no FK)
		ClientIP:      "federation:0",
		Endpoint:      endpoint,
		ModelID:       mdl.ID,
		ModelRequ:     in.Model,
		ModelUpstream: mdl.UpstreamModel,
		Prompt:        tok.Prompt,
		Completion:    tok.Completion,
		Cached:        tok.Cached,
		CacheWrite:    tok.CacheWrite,
		CostIn:        costs.In,
		CostOut:       costs.Out,
		CostCache:     costs.Cache,
		CostWrite:     costs.Write,
		CostTotal:     costs.Total,
		Streamed:      mt.stream,
		Status:        mt.status,
		LatencyMS:     time.Since(mt.start).Milliseconds(),
	}
	consumed := base
	consumed.UserID = fromU.ID
	consumed.FedDirection = proxy.FedConsumed
	consumed.FedPeerID = &targetPeer
	served := base
	served.UserID = toU.ID
	served.FedDirection = proxy.FedServed
	served.FedPeerID = &targetPeer // uniform: a served row names the HOST it credits
	served.FedUser = fromU.Username
	if err := store.InsertUsage(m.db, &consumed); err != nil {
		m.opts.Log.Printf("fed: transit consumed row: %v", err)
	}
	if err := store.InsertUsage(m.db, &served); err != nil {
		m.opts.Log.Printf("fed: transit served row: %v", err)
	}
}

// fedEndpointFor maps a served path to the usage_log endpoint name
// (mirrors the proxy's own endpoint constants; "" = not metered).
func fedEndpointFor(path string) string {
	switch path {
	case "/v1/chat/completions":
		return proxy.EpChat
	case "/v1/completions":
		return proxy.EpCompletions
	case "/v1/messages":
		return proxy.EpMessages
	case "/v1/embeddings":
		return proxy.EpEmbeddings
	}
	return ""
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
