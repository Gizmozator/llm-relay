// Relay-side edge proxy serving (ticket #17): a connected remote-backend
// box may run a local listener (see edge.go) that forwards whatever its
// LAN clients send to /v1/* over the existing backend channel as Req
// frames. This file runs those frames through the relay's OWN proxy mux —
// full pipeline: key auth, catalog, groups, metering, routing to any
// backend — and pumps the answer back as Res/ResBody/ResEnd frames.
//
// This is the mirror image of fed/serve.go, and deliberately NOT a reuse
// of it. The fed path strips Authorization and meters against a synthetic
// federation identity (attribution rides FedUser); the edge path must
// carry the client's sk- key VERBATIM so the relay's own key auth runs
// and usage rows land on the real key and user. The rb- connect token
// authenticates the channel, never a /v1 request. The two paths share
// only the frame codec.
//
// Abuse posture: every forwarded request passes the tier-5 per-channel
// bucket (internal/ratelimit, keyed on the remote-backend id — per-socket
// IP is the wrong key here, one box shares one source for all its users),
// the path gate (/v1/* only), and the loop guard (a request that arrived
// over backend X's channel is never routed back to X — enforced in
// RoundTrip off the context stamp set here).
package remote

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llm-relay/internal/fed"
	"llm-relay/internal/ratelimit"
)

// AttachServe wires the relay's own proxy mux (edge-forwarded client
// requests run through it) and the tier-5 per-channel limiter
// (ratelimit.NewStore(lim.RemoteChannel, ratelimit.StoreMax)). Mirrors
// fed.Manager.AttachServe: nil-safe until called — a channel that
// connects before the wiring gets an explicit error frame, never a
// panic. Called once at startup, before any connector can reach the
// connect route.
func (r *Registry) AttachServe(h http.Handler, channelLimit *ratelimit.Store) {
	r.serveMu.Lock()
	r.serve, r.channelLim = h, channelLimit
	r.serveMu.Unlock()
}

func (r *Registry) edgeWiring() (http.Handler, *ratelimit.Store) {
	r.serveMu.Lock()
	defer r.serveMu.Unlock()
	return r.serve, r.channelLim
}

// channelLimitKey namespaces the tier-5 store: keys live in one namespace
// per Store, and the "rb:" prefix marks them as remote-backend-channel
// identities (the same convention the other tiers use for their classes).
func channelLimitKey(backendID int64) string {
	return "rb:" + strconv.FormatInt(backendID, 10)
}

// onEdgeReq handles a client request forwarded over a backend's channel.
// Runs on the read pump: everything expensive goes to a goroutine.
func (r *Registry) onEdgeReq(c *fed.Conn, backendID int64, backendName, remoteAddr string, f fed.Frame) {
	req, ok := f.(*fed.Req)
	if !ok {
		return
	}
	if len(req.Body) > fed.MaxBody {
		_ = c.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID,
			Error: fmt.Sprintf("request body %d bytes over the %d cap", len(req.Body), fed.MaxBody)})
		return
	}
	handler, limiter := r.edgeWiring()
	if handler == nil {
		_ = c.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID, Error: "edge serving not wired"})
		return
	}
	// Tier 5 (ticket #18's system, applied here): one bucket per channel,
	// keyed on the authenticated backend id — never the socket IP, which
	// every client behind this box shares. A denial is an honest 429 with
	// Retry-After, delivered as a real response (the client sees a rate
	// limit, not a transport failure).
	if limiter != nil {
		if allow, wait := limiter.Allow(channelLimitKey(backendID), time.Now()); !allow {
			w := &frameWriter{conn: c, id: req.ID}
			w.head(http.StatusTooManyRequests, http.Header{
				"Retry-After":  {strconv.Itoa(max(1, int(math.Ceil(wait.Seconds()))))},
				"Content-Type": {"application/json"},
			})
			_ = w.body([]byte(`{"error":{"message":"rate limit exceeded for this remote channel, slow down","type":"rate_limit_error"}}`))
			w.end()
			log.Printf("remote: edge 429 backend %q (#%d) from %s", backendName, backendID, remoteAddr)
			return
		}
	}
	go r.serveEdgeReq(c, handler, backendID, backendName, remoteAddr, req)
}

// serveEdgeReq rebuilds an HTTP request from the frame, runs it through
// the relay's proxy mux, and pumps the answer back as frames.
func (r *Registry) serveEdgeReq(c *fed.Conn, handler http.Handler, backendID int64, backendName, remoteAddr string, req *fed.Req) {
	// Path gate: only the OpenAI surface is forwarded-client territory.
	// The connect endpoint, healthz and MCP live on the same mux but are
	// not for traffic arriving over a backend channel (mirrors the fed
	// frame server's identical gate).
	if !strings.HasPrefix(req.Path, "/v1/") {
		_ = c.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID, Error: "path not served over the edge proxy"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	// The channel dying aborts everything it was serving: a generation
	// must not keep burning an upstream into a socket that can no longer
	// deliver it.
	watchStop := make(chan struct{})
	go func() {
		select {
		case <-c.Done():
			cancel()
		case <-watchStop:
		}
	}()
	r.edgeMu.Lock()
	r.edgeServed[req.ID] = cancel
	r.edgeMu.Unlock()
	defer func() {
		close(watchStop)
		r.edgeMu.Lock()
		delete(r.edgeServed, req.ID)
		r.edgeMu.Unlock()
		cancel()
	}()

	// The origin stamp feeds the loop guard in RoundTrip: routing may
	// reach every backend EXCEPT the one this request came in on.
	httpReq, err := http.NewRequestWithContext(withEdgeOrigin(ctx, backendID),
		req.Method, "http://edge"+req.Path, bytes.NewReader(req.Body))
	if err != nil {
		_ = c.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID, Error: "bad request: " + err.Error()})
		return
	}
	// Authorization and x-api-key survive verbatim — that is the whole
	// point of this path (the relay's own authKey runs on the client's
	// sk-). Framing headers are still stripped (edgeDropHeader): an
	// authenticated connector is trusted with credentials but not with
	// the relay's request framing.
	httpReq.Header = edgeWireRequestHeader(req.Headers)
	if httpReq.Header.Get("Content-Type") == "" && len(req.Body) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// Metering's client address: the connector box's socket (the honest
	// direct peer). The edge listener appended the LAN client to
	// X-Forwarded-For, so with TRUST_PROXY_HEADERS on the proxy rule
	// picks the real client (rightmost entry = appended by the
	// operator's own box); with it off the box's address is recorded —
	// exactly what the trust model promises either way.
	if remoteAddr != "" {
		httpReq.RemoteAddr = remoteAddr
	} else {
		httpReq.RemoteAddr = "remote:0"
	}

	w := &edgeWriter{id: req.ID, send: c.Send, status: http.StatusOK}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("remote: edge request %s panicked: %v", req.ID, rec)
			_ = c.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID, Error: fmt.Sprintf("internal error: %v", rec)})
			return
		}
		_ = c.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID})
		// Channel-attributed access line: which box forwarded what, for
		// whom, and how it ended.
		log.Printf("remote: edge %s %s via backend %q (#%d) from %s: %d",
			req.Method, req.Path, backendName, backendID, remoteAddr, w.status)
	}()
	handler.ServeHTTP(w, httpReq)
}

// onEdgeCancel aborts a forwarded request the connector's client hung up
// on: the edge listener sends Cancel when its local HTTP request dies,
// and cancelling the internal request's context unwinds the whole chain
// (proxy → upstream call → its own tunnel).
func (r *Registry) onEdgeCancel(f fed.Frame) {
	cancel, ok := f.(*fed.Cancel)
	if !ok {
		return
	}
	r.edgeMu.Lock()
	fn := r.edgeServed[cancel.ID]
	r.edgeMu.Unlock()
	if fn != nil {
		fn()
	}
}

// edgeWriter turns one proxy-mux response into Res/ResBody/ResEnd frames
// on the backend channel: WriteHeader becomes the Res head, every Write
// one or more ResBody chunks (each send already ships, so Flush is a
// no-op), and the handler returning is the ResEnd. Same shape as the fed
// frame server's servedWriter, kept separate so the two paths can drift
// without entangling.
type edgeWriter struct {
	id     string
	send   func(fed.Frame) error
	header http.Header
	once   sync.Once
	status int
	bad    bool
}

func (w *edgeWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *edgeWriter) WriteHeader(code int) {
	w.once.Do(func() {
		w.status = code
		if err := w.send(&fed.Res{T: fed.TRes, ID: w.id, Status: code,
			Headers: fed.ResponseHeaders(w.header)}); err != nil {
			w.bad = true
		}
	})
}

func (w *edgeWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.bad {
		return 0, fed.ErrClosed
	}
	n := len(p)
	for len(p) > 0 {
		c := min(len(p), fed.MaxBody)
		if err := w.send(&fed.ResBody{T: fed.TResBody, ID: w.id, Data: p[:c]}); err != nil {
			w.bad = true
			return 0, err
		}
		p = p[c:]
	}
	return n, nil
}

func (w *edgeWriter) Flush() {}

var (
	_ http.ResponseWriter = (*edgeWriter)(nil)
	_ http.Flusher        = (*edgeWriter)(nil)
)

// ------------------------------------------------------------- headers

// edgeDropHeader names the headers that never cross the edge tunnel in
// either direction: transport framing a spoofed value could use for
// request smuggling, and cookies (the relay's /v1 surface is bearer-key
// authenticated; cookies have no business here). Authorization and
// x-api-key are deliberately NOT in the list — the whole point of the
// edge path is that the client's sk- reaches the relay's own key auth
// verbatim (the fed path strips both; this path must not).
func edgeDropHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "proxy-connection", "te", "trailer", "transfer-encoding",
		"upgrade", "cookie":
		return true
	}
	return false
}

// edgeRequestHeaders serializes the listener's request headers onto the
// Req frame verbatim except for edgeDropHeader.
func edgeRequestHeaders(h http.Header) fed.Headers {
	var out fed.Headers
	for k, vs := range h {
		if edgeDropHeader(k) {
			continue
		}
		for _, v := range vs {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

// edgeWireRequestHeader rebuilds a frame's header set for the local proxy
// mux. The sender is an authenticated connector, but the framing strip is
// kept as defense in depth: a compromised box must not smuggle framing
// into the relay's pipeline. Credentials pass — that is the contract.
func edgeWireRequestHeader(h fed.Headers) http.Header {
	out := http.Header{}
	for _, kv := range h {
		if edgeDropHeader(kv[0]) {
			continue
		}
		out.Add(kv[0], kv[1])
	}
	return out
}

// ------------------------------------------------------------- origin

type edgeOriginKey struct{}

// withEdgeOrigin stamps the backend channel a forwarded client request
// arrived on onto the internal request's context. RoundTrip reads it to
// refuse routing the request back to its origin — the loop guard,
// receiver-derived like the fed path's OriginPeer (the frame's own
// origin fields are never trusted).
func withEdgeOrigin(ctx context.Context, backendID int64) context.Context {
	return context.WithValue(ctx, edgeOriginKey{}, backendID)
}

// edgeOriginFrom returns the origin backend id (0 = ordinary traffic).
func edgeOriginFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(edgeOriginKey{}).(int64)
	return id
}
