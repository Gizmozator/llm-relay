// Server side of the remote-backend channel (plan 2026-11-24-remote-backend-v1):
// a registry of live WebSocket channels, one per remote backend id, keyed by
// the backend row that authenticated the connection. A remote box is NOT a
// federation peer: no catalog frames, no role gate, no owner user — it is a
// backend whose transport is a tunnel instead of a dial. The proxy reaches it
// through RoundTrip (remote://<id> URLs); the health cache answers "online =
// live channel"; the admin probe runs /health + /v1/models through the same
// RoundTrip.
//
// The channel itself is the ordinary fed.Conn (writer goroutine, pinger,
// request-id multiplexing) speaking the fed frame codec — hello/hello_ack to
// open, then req/res/res_body/res_end/cancel per request.
package remote

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/fed"
	"llm-relay/internal/store"
)

// ErrNotConnected is returned by RoundTrip for a backend with no live
// channel — a dial-class error, so the proxy's group failover treats an
// offline remote backend exactly like a dead socket.
var ErrNotConnected = errors.New("remote backend not connected")

// Options carries the channel timing knobs (fed.Options: hello budget,
// ping/pong, write deadline, body caps come from fed itself).
type Options struct {
	fed.Options
}

// entry pairs a live channel with the backend row that authenticated it.
type entry struct {
	conn        *fed.Conn
	backendID   int64
	name        string
	version     string
	connectedAt time.Time
}

// Registry owns the live remote-backend channels. Always constructible;
// connections are accepted on any relay role (the connect route is not
// role-gated — a remote backend is a backend, not a federation member).
type Registry struct {
	db   *sql.DB
	opts fed.Options

	mu      sync.Mutex
	entries map[int64]*entry
}

func NewRegistry(db *sql.DB, opts *Options, name, version string) *Registry {
	o := fed.Options{}
	if opts != nil {
		o = opts.Options
	}
	o = o.WithDefaults()
	if o.InstanceName == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			o.InstanceName = hn
		} else {
			o.InstanceName = "llm-relay"
		}
	}
	if name != "" {
		o.InstanceName = name
	}
	if version != "" {
		o.Version = version
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	return &Registry{db: db, opts: o, entries: map[int64]*entry{}}
}

// Online reports whether the backend currently holds a live channel.
func (r *Registry) Online(backendID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[backendID]
	return ok && !e.conn.Closed()
}

// BackendOnline answers the health cache for a remote:// backend: the
// channel IS the health (no HTTP to probe on a NATted box).
func (r *Registry) BackendOnline(b store.Backend) bool {
	id, ok := store.RemoteBackendID(b.BaseURL)
	if !ok {
		return true
	}
	return r.Online(id)
}

// Connected reports the live channel's hello-reported identity (admin view).
func (r *Registry) Connected(backendID int64) (connected bool, name, version string, since time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[backendID]
	if !ok || e.conn.Closed() {
		return false, "", "", time.Time{}
	}
	return true, e.name, e.version, e.connectedAt
}

// Kick closes a backend's live channel (delete / disable / rotate token):
// the revocation lands now, not at the next pong timeout.
func (r *Registry) Kick(backendID int64, reason string) {
	r.mu.Lock()
	e := r.entries[backendID]
	r.mu.Unlock()
	if e != nil {
		go e.conn.Close(websocket.StatusPolicyViolation, reason)
	}
}

// CloseAll closes every live channel (process shutdown).
func (r *Registry) CloseAll(reason string) {
	r.mu.Lock()
	entries := make([]*entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	r.mu.Unlock()
	for _, e := range entries {
		go e.conn.Close(websocket.StatusPolicyViolation, reason)
	}
}

// ServeConn is the /remote/v1/connect body, reached behind RemoteAuth (the
// backend row is authenticated already): WS upgrade → hello within
// HelloTimeout → proto check → hello_ack → registry (replacing any previous
// channel for the same backend — a connector restart takes over at once).
// tokenHash is the hash RemoteAuth matched the bearer token against — the
// baseline the channel's revocation sweep revalidates against.
func (r *Registry) ServeConn(w http.ResponseWriter, req *http.Request, b *store.Backend, tokenHash string) {
	// InsecureSkipVerify: the client is a headless box, not a browser — it
	// sends no Origin header. The rb- bearer token is the authenticator
	// (RemoteAuth already checked it); cross-site WS hijacking does not
	// apply to a non-cookie auth surface.
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("remote: accept backend %d: %v", b.ID, err)
		return
	}
	ver, err := r.handshake(ws, b)
	if err != nil {
		log.Printf("remote: handshake backend %d: %v", b.ID, err)
		return
	}
	// Per-connection opts: Touch is this backend's revocation sweep, so
	// r.opts itself is never mutated (concurrent connects share it). At
	// most once per TouchThrottle it revalidates the row — deleted,
	// disabled, or rotated past tokenHash closes the channel with
	// "revoked". That catches revocations that raced past the kick
	// endpoints (auth passed, then rotate kicked an empty slot, then this
	// registration landed). Touch only runs from the read pump, which
	// starts after conn is assigned.
	opts := r.opts
	var conn *fed.Conn
	opts.Touch = func() {
		if !store.RemoteTokenLive(r.db, b.ID, tokenHash) {
			conn.Close(websocket.StatusPolicyViolation, "revoked")
			return
		}
	}
	conn = fed.NewConn(ws, opts)
	e := &entry{conn: conn, backendID: b.ID, name: b.Name, version: ver, connectedAt: time.Now()}
	r.register(e)
	log.Printf("remote: backend %q (#%d) connected (client %s)", b.Name, b.ID, orUnknown(ver))
	// The handler returns while the channel lives: Accept hijacked the
	// connection and the pumps run on their own context (never req.Context,
	// which dies with the request).
	go func() {
		conn.Serve(context.Background())
		if r.unregister(b.ID, conn) {
			log.Printf("remote: backend %q (#%d) disconnected", b.Name, b.ID)
		}
	}()
}

// handshake reads+validates hello and replies hello_ack with raw socket
// reads (the Conn pumps only start afterwards). Returns the client's
// reported version. Same shape as the hub's: a proper 1008 close on timeout,
// because cancelling a pending Read tears the socket down without one.
func (r *Registry) handshake(ws *websocket.Conn, b *store.Backend) (string, error) {
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
	case res := <-reads:
		if res.err != nil {
			_ = ws.Close(websocket.StatusPolicyViolation, "hello required")
			return "", errors.New("hello not received")
		}
		data = res.data
	case <-time.After(r.opts.HelloTimeout):
		go func() { _ = ws.Close(websocket.StatusPolicyViolation, "hello required") }()
		return "", errors.New("hello timeout")
	}
	f, err := fed.Decode(data)
	if err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "hello required")
		return "", err
	}
	hello, ok := f.(*fed.Hello)
	if !ok {
		_ = ws.Close(websocket.StatusPolicyViolation, "hello required")
		return "", errors.New("first frame is not hello")
	}
	if hello.Proto != fed.Protocol {
		_ = ws.Close(websocket.StatusProtocolError, "proto mismatch")
		return "", fmt.Errorf("proto mismatch: %d", hello.Proto)
	}
	ack, err := fed.Encode(&fed.HelloAck{T: fed.THelloAck, Proto: fed.Protocol,
		Name: r.opts.InstanceName, Version: r.opts.Version})
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "encode ack")
		return "", err
	}
	wctx, wcancel := context.WithTimeout(context.Background(), r.opts.WriteTimeout)
	defer wcancel()
	if err := ws.Write(wctx, websocket.MessageText, ack); err != nil {
		_ = ws.Close(websocket.StatusInternalError, "ack write failed")
		return "", err
	}
	return hello.Version, nil
}

// register installs e, closing any previous channel for the same backend.
func (r *Registry) register(e *entry) {
	r.mu.Lock()
	old := r.entries[e.backendID]
	r.entries[e.backendID] = e
	r.mu.Unlock()
	if old != nil && old.conn != e.conn {
		go old.conn.Close(websocket.StatusPolicyViolation, "replaced")
	}
}

// unregister drops the backend only if the entry is still this conn (a
// replacement must not be evicted by the old channel's exit). Reports
// whether the live channel was really this one's.
func (r *Registry) unregister(backendID int64, c *fed.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[backendID]
	if ok && e.conn == c {
		delete(r.entries, backendID)
		return true
	}
	return false
}

// connFor returns a backend's live channel (nil when offline).
func (r *Registry) connFor(backendID int64) *fed.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[backendID]; ok && !e.conn.Closed() {
		return e.conn
	}
	return nil
}

// newRequestID mints a 128-bit hex id for one tunneled request.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "deadbeefdeadbeefdeadbeefdeadbee1"
	}
	return hex.EncodeToString(b[:])
}

// RoundTrip answers one remote://<id>/... request (the proxy's Remote seam
// and the admin probe's transport). Errors are dial-class: the group walk
// fails over to the next member on them, exactly like a refused TCP connect.
func (r *Registry) RoundTrip(req *http.Request) (*http.Response, error) {
	backendID, err := parseRemoteHost(req.URL)
	if err != nil {
		return nil, err
	}
	conn := r.connFor(backendID)
	if conn == nil {
		return nil, ErrNotConnected
	}
	id := newRequestID()
	ch, release, err := conn.OpenRequest(id)
	if err != nil {
		return nil, err // ErrTooManyInflight / ErrClosed: failover signal
	}
	var body []byte
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, fed.MaxBody+1))
		if err != nil {
			release()
			return nil, err
		}
	}
	if len(body) > fed.MaxBody {
		release()
		return nil, fmt.Errorf("remote: request body %d bytes over the %d cap", len(body), fed.MaxBody)
	}
	frame := &fed.Req{T: fed.TReq, ID: id, Method: req.Method, Path: req.URL.Path,
		Headers: fed.StripRequestHeaders(req.Header), Body: body}
	if err := conn.Send(frame); err != nil {
		release()
		return nil, err
	}
	for {
		select {
		case f := <-ch:
			switch t := f.(type) {
			case *fed.Res:
				pr, pw := io.Pipe()
				hung := make(chan struct{})
				go pump(conn, id, ch, release, pw, req.Context(), hung)
				return &http.Response{
					Status:        http.StatusText(t.Status),
					StatusCode:    t.Status,
					Proto:         "HTTP/1.1",
					ProtoMajor:    1,
					ProtoMinor:    1,
					Header:        fed.WireResponseHeader(t.Headers),
					Body:          &closeNotify{PipeReader: pr, hung: hung},
					ContentLength: -1,
					Request:       req,
				}, nil
			case *fed.ResEnd:
				release()
				if t.Error != "" {
					return nil, fmt.Errorf("remote: %s", t.Error)
				}
				return nil, errors.New("remote: response ended before headers")
			}
		case <-conn.Done():
			release()
			return nil, ErrNotConnected
		case <-req.Context().Done():
			_ = conn.Send(&fed.Cancel{T: fed.TCancel, ID: id})
			release()
			return nil, req.Context().Err()
		}
	}
}

// closeNotify wraps the response body so the pump can notice a consumer
// hang-up even when the upstream has gone silent: io.Pipe only reports a
// closed reader on the NEXT write, and a stalled engine never writes again
// — without this, the connector's engine call would leak until the channel
// died.
type closeNotify struct {
	*io.PipeReader
	hung chan struct{}
	once sync.Once
}

func (c *closeNotify) Close() error {
	c.once.Do(func() { close(c.hung) })
	return c.PipeReader.Close()
}

// pump copies the backend's answer frames into the response pipe until the
// stream ends, the channel dies, or the consumer hangs up (then a Cancel
// tells the connector to abort its engine call).
func pump(conn *fed.Conn, id string, ch <-chan fed.Frame, release func(), pw *io.PipeWriter, ctx context.Context, hung <-chan struct{}) {
	defer release()
	defer pw.Close()
	for {
		select {
		case f := <-ch:
			switch t := f.(type) {
			case *fed.ResBody:
				if len(t.Data) == 0 {
					continue
				}
				if len(t.Data) > fed.MaxBody {
					pw.CloseWithError(fmt.Errorf("remote: body chunk %d over cap", len(t.Data)))
					return
				}
				if _, err := pw.Write(t.Data); err != nil {
					_ = conn.Send(&fed.Cancel{T: fed.TCancel, ID: id})
					return
				}
			case *fed.ResEnd:
				if t.Error != "" {
					pw.CloseWithError(errors.New(t.Error))
				}
				return
			}
		case <-hung:
			// Consumer hung up while the engine was mid-generation: abort
			// it instead of streaming tokens into a closed pipe.
			_ = conn.Send(&fed.Cancel{T: fed.TCancel, ID: id})
			return
		case <-conn.Done():
			pw.CloseWithError(ErrNotConnected)
			return
		case <-ctx.Done():
			_ = conn.Send(&fed.Cancel{T: fed.TCancel, ID: id})
			pw.CloseWithError(ctx.Err())
			return
		}
	}
}

// parseRemoteHost reads the backend id out of a remote://<id>/... URL.
func parseRemoteHost(u *url.URL) (int64, error) {
	if u.Scheme != store.RemoteScheme {
		return 0, fmt.Errorf("not a remote scheme: %q", u.Scheme)
	}
	id, err := strconv.ParseInt(u.Host, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("bad remote backend %q", u.String())
	}
	return id, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
