// Hub side of the WS channel (plan 2026-11-22-relay-federation task 2):
// role gate, hello handshake rules (proto mismatch, hello timeout),
// replace-on-reconnect and the liveness pinger, all against the real
// Manager behind a PeerAuth-shaped wrapper.
package fed_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/fed"
	"llm-relay/internal/store"
)

// closeReason extracts the WS close reason from a read error (the
// library only exports CloseStatus; the reason rides on CloseError).
func closeReason(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

func quietOpts() fed.Options {
	return fed.Options{Log: log.New(io.Discard, "", 0),
		PingInterval: time.Hour, PongTimeout: time.Hour}
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/fed.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

type hubEnv struct {
	db      *sql.DB
	mgr     *fed.Manager
	srv     *httptest.Server
	peer    *store.Peer
	token   string
	ownerID int64
}

// newHubEnv wires the connect route the way main.go does (token auth in
// front, Manager behind it) so the tests exercise the real gate order.
func newHubEnv(t *testing.T, opts *fed.Options) *hubEnv {
	t.Helper()
	d := migratedDB(t)
	if opts == nil {
		opts = &fed.Options{}
	}
	opts.Log = log.New(io.Discard, "", 0)
	mgr := fed.NewManager(d, opts)
	t.Cleanup(mgr.Close)
	mux := http.NewServeMux()
	mux.Handle("GET /federation/v1/connect", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			http.Error(w, `{"error":{"message":"authentication required"}}`, http.StatusUnauthorized)
			return
		}
		p, err := store.GetPeerByKeyHash(d, auth.HashKey(tok))
		if err != nil {
			http.Error(w, `{"error":{"message":"invalid server token"}}`, http.StatusUnauthorized)
			return
		}
		if !p.Enabled {
			http.Error(w, `{"error":{"message":"peer disabled"}}`, http.StatusForbidden)
			return
		}
		mgr.ServeHub(w, r, p)
	}))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	hash, _ := auth.HashPassword("x")
	u, err := store.CreateUser(d, "alice", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	p, token, err := store.CreatePeer(d, "spoke-one", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &hubEnv{db: d, mgr: mgr, srv: srv, peer: p, token: token, ownerID: u.ID}
}

func (e *hubEnv) setRole(t *testing.T, role string) {
	t.Helper()
	in := store.FederationSettings{Role: role}
	if role == store.FedRoleSpoke {
		// Nothing listens on :1 — the dialer just retries quietly.
		in.HubURL, in.HubToken = "https://127.0.0.1:1", "fed-x"
	}
	if err := store.PutFederationSettings(e.db, in); err != nil {
		t.Fatal(err)
	}
	e.mgr.Reload()
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/federation/v1/connect"
}

func wsDial(t *testing.T, url, token string, client *http.Client) (*websocket.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	return c, err
}

// clientHello runs the spoke half of the handshake over a fresh socket.
func clientHello(t *testing.T, c *websocket.Conn, proto int) *fed.HelloAck {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	msg, _ := fed.Encode(&fed.Hello{T: fed.THello, Proto: proto,
		Name: "test-spoke", Version: "testver"})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	f, err := fed.Decode(data)
	if err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	ack, ok := f.(*fed.HelloAck)
	if !ok {
		t.Fatalf("first reply is %T, want hello_ack", f)
	}
	return ack
}

// expectRejected asserts the connect gate answers with an HTTP status
// (checked with a plain GET — the gate runs before the upgrade) and that
// a WS dial is refused accordingly.
func expectRejected(t *testing.T, e *hubEnv, token string, want int) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/federation/v1/connect", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Errorf("gate status = %d, want %d", resp.StatusCode, want)
	}
	if _, err := wsDial(t, wsURL(e.srv), token, e.srv.Client()); err == nil {
		t.Errorf("ws dial accepted, want rejection %d", want)
	}
}

func TestConnectRoleAndAuthMatrix(t *testing.T) {
	e := newHubEnv(t, nil)
	// standalone (default) and spoke do not accept channels at all.
	expectRejected(t, e, e.token, http.StatusNotFound)
	e.setRole(t, store.FedRoleSpoke)
	expectRejected(t, e, e.token, http.StatusNotFound)
	// hub: PeerAuth still guards the upgrade.
	e.setRole(t, store.FedRoleHub)
	expectRejected(t, e, "fed-wrong", http.StatusUnauthorized)
	expectRejected(t, e, "", http.StatusUnauthorized)

	// hub + good token: upgrade + hello/hello_ack exchange.
	c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ack := clientHello(t, c, fed.Protocol)
	if ack.Proto != fed.Protocol || ack.Name == "" || ack.Version == "" {
		t.Errorf("ack = %+v", ack)
	}
	// Registration lands right after the ack write; give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for !e.mgr.Online(e.peer.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !e.mgr.Online(e.peer.ID) {
		t.Error("peer not registered after handshake")
	}
}

// The version injected into fed.Options is what the hub answers in
// hello_ack — the spoke's "Hub version" card reads exactly that value,
// so a stamped build must show its stamp, not the "dev" default.
func TestHubHelloAckCarriesInjectedVersion(t *testing.T) {
	e := newHubEnv(t, &fed.Options{Version: "v9.8.7"})
	e.setRole(t, store.FedRoleHub)
	c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ack := clientHello(t, c, fed.Protocol)
	if ack.Version != "v9.8.7" {
		t.Errorf("hello_ack version = %q, want the injected v9.8.7", ack.Version)
	}
}

// Without an injected version the hub still reports the honest default.
func TestHubHelloAckDefaultsToDev(t *testing.T) {
	e := newHubEnv(t, nil)
	e.setRole(t, store.FedRoleHub)
	c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ack := clientHello(t, c, fed.Protocol)
	if ack.Version != "dev" {
		t.Errorf("hello_ack version = %q, want dev", ack.Version)
	}
}

func TestProtoMismatch(t *testing.T) {
	e := newHubEnv(t, nil)
	e.setRole(t, store.FedRoleHub)
	c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	msg, _ := fed.Encode(&fed.Hello{T: fed.THello, Proto: 99, Name: "x", Version: "y"})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusProtocolError {
		t.Errorf("close code = %v, want 1002", got)
	}
	if got := closeReason(err); got != "proto mismatch" {
		t.Errorf("close reason = %q, want %q", got, "proto mismatch")
	}
}

func TestHelloTimeout(t *testing.T) {
	e := newHubEnv(t, &fed.Options{HelloTimeout: 100 * time.Millisecond})
	e.setRole(t, store.FedRoleHub)
	c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err) // the upgrade itself succeeds; hello is what times out
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = c.Read(ctx) // we never send hello
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close code = %v, want 1008", got)
	}
	if got := closeReason(err); got != "hello required" {
		t.Errorf("close reason = %q, want %q", got, "hello required")
	}
}

func TestReplaceSamePeer(t *testing.T) {
	e := newHubEnv(t, nil)
	e.setRole(t, store.FedRoleHub)
	a, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseNow()
	clientHello(t, a, fed.Protocol)

	// A spoke restart (second connect, same token) takes over at once.
	b, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseNow()
	clientHello(t, b, fed.Protocol)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = a.Read(ctx) // the old socket must be closed 'replaced'
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("old conn close code = %v, want 1008", got)
	}
	if got := closeReason(err); got != "replaced" {
		t.Errorf("old conn close reason = %q, want %q", got, "replaced")
	}
	if !e.mgr.Online(e.peer.ID) {
		t.Error("registry lost the peer after replace")
	}
	// The old conn's exit must not evict the new entry (identity check).
	b.CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for e.mgr.Online(e.peer.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.mgr.Online(e.peer.ID) {
		t.Error("registry still online after the live conn closed")
	}
}

func TestLivenessAndTouch(t *testing.T) {
	e := newHubEnv(t, &fed.Options{
		PingInterval:  50 * time.Millisecond,
		PongTimeout:   50 * time.Millisecond,
		TouchThrottle: 10 * time.Millisecond,
	})
	e.setRole(t, store.FedRoleHub)
	c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	clientHello(t, c, fed.Protocol)

	// The handshake itself touched the peer row (version + last_seen).
	p, err := store.PeerByID(e.db, e.peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "testver" || p.LastSeen == "" {
		t.Fatalf("after hello: version=%q last_seen=%q", p.Version, p.LastSeen)
	}
	first := p.LastSeen

	// Alive phase: answer every hub ping. last_seen must advance (the
	// throttled TouchPeer runs on inbound frames).
	var respond atomic.Bool
	respond.Store(true)
	dead := make(chan error, 1)
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, data, err := c.Read(ctx)
			cancel()
			if err != nil {
				dead <- err
				return
			}
			if f, err := fed.Decode(data); err == nil && f.FrameType() == fed.TPing && respond.Load() {
				ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
				pong, _ := fed.Encode(&fed.Pong{T: fed.TPong})
				_ = c.Write(ctx2, websocket.MessageText, pong)
				cancel2()
			}
		}
	}()
	time.Sleep(1200 * time.Millisecond) // >1s so the RFC3339 stamp moves
	if !e.mgr.Online(e.peer.ID) {
		t.Fatal("channel dropped while the peer was answering pings")
	}
	if p, err := store.PeerByID(e.db, e.peer.ID); err != nil || p.LastSeen == first {
		t.Errorf("last_seen did not advance while alive: %q vs %q (%v)", p.LastSeen, first, err)
	}

	// Dead phase: stop answering — the hub must close and deregister.
	respond.Store(false)
	select {
	case err := <-dead:
		if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
			t.Errorf("liveness close code = %v, want 1008", got)
		}
		if got := closeReason(err); got != "pong timeout" && got != "liveness timeout" {
			t.Errorf("liveness close reason = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hub never closed the silent channel")
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.mgr.Online(e.peer.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.mgr.Online(e.peer.ID) {
		t.Error("registry still online after the liveness close")
	}
}

// ------------------------------------------------------- revocation & teardown

// autoPong answers every hub ping so the channel stays alive; the
// revocation sweep rides that pong traffic (Touch fires on inbound
// frames, and the pinger guarantees at least one every PingInterval).
// The channel carries the terminal read error — the close frame lands on
// THIS reader, so the test must read it here, not via waitForClose.
func autoPong(t *testing.T, c *websocket.Conn) <-chan error {
	t.Helper()
	out := make(chan error, 1)
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, data, err := c.Read(ctx)
			cancel()
			if err != nil {
				out <- err
				return
			}
			if f, err := fed.Decode(data); err == nil && f.FrameType() == fed.TPing {
				ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
				pong, _ := fed.Encode(&fed.Pong{T: fed.TPong})
				_ = c.Write(ctx2, websocket.MessageText, pong)
				cancel2()
			}
		}
	}()
	return out
}

// waitForClose drains reads until the socket dies and returns the read
// error carrying the close frame.
func waitForClose(t *testing.T, c *websocket.Conn) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _, err := c.Read(ctx)
		cancel()
		if err != nil {
			return err
		}
	}
	t.Fatal("socket never closed")
	return nil
}

func waitOffline(t *testing.T, e *hubEnv) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for e.mgr.Online(e.peer.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return !e.mgr.Online(e.peer.ID)
}

// liveChannel dials + handshakes a channel on a hub-role env. The ack is
// written before the hub registers the entry, so wait for the registry
// before acting on it (kick/close must find the entry).
func liveChannel(t *testing.T, e *hubEnv) *websocket.Conn {
	t.Helper()
	c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	clientHello(t, c, fed.Protocol)
	deadline := time.Now().Add(2 * time.Second)
	for !e.mgr.Online(e.peer.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !e.mgr.Online(e.peer.ID) {
		t.Fatal("peer never registered")
	}
	return c
}

// Item 1c: immediate kick — the peer endpoints call KickPeer on the
// mutation itself, no sweep wait.
func TestKickClosesChannel(t *testing.T) {
	e := newHubEnv(t, nil)
	e.setRole(t, store.FedRoleHub)
	c := liveChannel(t, e)

	e.mgr.KickPeer(e.peer.ID, "rotated")
	err := waitForClose(t, c)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close code = %v, want 1008", got)
	}
	if got := closeReason(err); got != "rotated" {
		t.Errorf("close reason = %q, want %q", got, "rotated")
	}
	if !waitOffline(t, e) {
		t.Error("registry still online after kick")
	}
}

// Item 1c: the revocation sweep catches state changes that never passed a
// peer endpoint — here the peer row disabled out of band (the shape of a
// user disabled via the admin users API taking its peers down).
func TestRevocationSweepDisabled(t *testing.T) {
	e := newHubEnv(t, &fed.Options{
		PingInterval:  50 * time.Millisecond,
		PongTimeout:   50 * time.Millisecond,
		TouchThrottle: 10 * time.Millisecond,
	})
	e.setRole(t, store.FedRoleHub)
	c := liveChannel(t, e)
	dead := autoPong(t, c)

	if err := store.SetPeerEnabled(e.db, e.peer.ID, false); err != nil {
		t.Fatal(err)
	}
	err := recv(t, dead, "sweep close")
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close code = %v, want 1008", got)
	}
	if got := closeReason(err); got != "revoked" {
		t.Errorf("close reason = %q, want %q", got, "revoked")
	}
	if !waitOffline(t, e) {
		t.Error("registry still online after revocation")
	}
}

// Item 1c: owner disabled → same sweep verdict ("revoked"), matching what
// PeerAuth would answer on a fresh dial (401 via the KeyByHash contract).
func TestRevocationSweepOwnerDisabled(t *testing.T) {
	e := newHubEnv(t, &fed.Options{
		PingInterval:  50 * time.Millisecond,
		PongTimeout:   50 * time.Millisecond,
		TouchThrottle: 10 * time.Millisecond,
	})
	e.setRole(t, store.FedRoleHub)
	c := liveChannel(t, e)
	dead := autoPong(t, c)

	dis := true
	if _, err := store.UpdateUser(e.db, e.ownerID, nil, &dis, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	err := recv(t, dead, "sweep close")
	if got := closeReason(err); got != "revoked" {
		t.Errorf("close reason = %q, want %q", got, "revoked")
	}
	if !waitOffline(t, e) {
		t.Error("registry still online after owner revocation")
	}
}

// Item 2: moving the role away from hub tears down live channels.
func TestRoleChangeClosesLiveChannels(t *testing.T) {
	e := newHubEnv(t, nil)
	e.setRole(t, store.FedRoleHub)
	c := liveChannel(t, e)

	e.setRole(t, store.FedRoleStandalone) // PUT-shaped change → Reload
	err := waitForClose(t, c)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close code = %v, want 1008", got)
	}
	if got := closeReason(err); got != "role changed" {
		t.Errorf("close reason = %q, want %q", got, "role changed")
	}
	if !waitOffline(t, e) {
		t.Error("registry still online after role change")
	}
}

// Item 6: Manager.Close (process shutdown) closes live hub channels —
// http.Server.Shutdown does not wait for hijacked connections.
func TestManagerCloseClosesChannels(t *testing.T) {
	e := newHubEnv(t, nil)
	e.setRole(t, store.FedRoleHub)
	c := liveChannel(t, e)

	e.mgr.Close() // t.Cleanup calls it again; Close is idempotent
	err := waitForClose(t, c)
	if got := closeReason(err); got != "shutdown" {
		t.Errorf("close reason = %q, want %q", got, "shutdown")
	}
}
