// Client side of the remote-backend channel (plan 2026-11-24-remote-backend-v1):
// the headless box's connector. It dials the relay's /remote/v1/connect with
// the rb- token, then serves req frames against its local engine: the relay
// rewrites the model name and POSTs to remote://<id>/v1/...; the connector
// forwards method + path + body to ENGINE_URL and streams the answer back as
// res/res_body/res_end frames. Cancel frames abort in-flight engine calls.
//
// The connector is a dumb pipe by design: no catalog, no pricing, no state.
// It only ever talks to the relay it was configured with and to its one
// engine URL, and it restricts served paths to /v1/* and /health so a
// hostile or compromised relay cannot use the tunnel to probe its LAN.
package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/fed"
)

// ClientConfig is one connector's whole configuration.
type ClientConfig struct {
	ServerURL   string // relay root, e.g. http://relay.example.fi:8080
	Token       string // rb- connect token (shown once at backend create)
	EngineURL   string // local engine root, e.g. http://localhost:8080
	EngineToken string // optional upstream API key, sent as Authorization: Bearer
	Name        string // hello identity (default: hostname set by caller)
	Version     string
	// Options tunes the channel (dial timeout, backoff ladder, ping).
	Options fed.Options
}

// RunClient connects and serves until ctx is cancelled, redialing with
// jittered exponential backoff (same ladder as the federation dialer). It
// only returns when ctx is done.
func RunClient(ctx context.Context, c ClientConfig, logf func(string, ...any)) error {
	if logf == nil {
		logf = log.Printf
	}
	if c.Options.InstanceName == "" {
		c.Options.InstanceName = orDefaultStr(c.Name, "llm-relay-headless")
	}
	if c.Options.Version == "" {
		c.Options.Version = orDefaultStr(c.Version, "dev")
	}
	o := c.Options.WithDefaults()
	backoff := o.BackoffMin
	for {
		started := time.Now()
		err := runOnce(ctx, c, o, logf)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(started) >= o.StableAfter {
			backoff = o.BackoffMin // a held connection resets the ladder
		}
		sleep := backoff/2 + time.Duration(rand.Int63n(int64(backoff/2)+1))
		logf("headless: relay link down (%v); redial in %s", err, sleep.Round(time.Millisecond))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff *= 2; backoff > o.BackoffMax {
			backoff = o.BackoffMax
		}
	}
}

// runOnce performs one full connect: upgrade, hello/hello_ack, then serve
// frames until the channel dies.
func runOnce(ctx context.Context, c ClientConfig, o fed.Options, logf func(string, ...any)) error {
	url := strings.TrimSuffix(c.ServerURL, "/") + "/remote/v1/connect"
	dctx, dcancel := context.WithTimeout(ctx, o.DialTimeout)
	ws, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}},
	})
	dcancel()
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	// Bound the socket BEFORE the handshake reads: hello_ack has
	// authenticated nothing yet, and the Conn's own SetReadLimit only lands
	// after the handshake.
	ws.SetReadLimit(fed.ReadLimit)
	if err := handshakeClient(ctx, ws, o, c); err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "handshake failed")
		return err
	}
	conn := fed.NewConn(ws, o)
	srv := &engineServer{
		cfg:  c,
		conn: conn,
		logf: logf,
		client: &http.Client{
			// No overall timeout: generations run for minutes. The
			// request context (Cancel frame / channel death) is the bound.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		served: map[string]context.CancelFunc{},
	}
	logf("headless: connected to %s; engine %s", c.ServerURL, c.EngineURL)
	// The channel dying aborts everything it was serving: a generation must
	// not keep burning the engine into a socket that can no longer deliver.
	go func() {
		<-conn.Done()
		srv.cancelAll()
	}()
	conn.On(fed.TReq, srv.onReq)
	conn.On(fed.TCancel, srv.onCancel)
	return conn.Serve(ctx)
}

// handshakeClient sends hello and waits for a matching hello_ack (raw reads
// — the Conn pumps start after this returns).
func handshakeClient(ctx context.Context, ws *websocket.Conn, o fed.Options, c ClientConfig) error {
	hello, err := fed.Encode(&fed.Hello{T: fed.THello, Proto: fed.Protocol,
		Name: o.InstanceName, Version: o.Version})
	if err != nil {
		return err
	}
	wctx, wcancel := context.WithTimeout(ctx, o.WriteTimeout)
	defer wcancel()
	if err := ws.Write(wctx, websocket.MessageText, hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	rctx, rcancel := context.WithTimeout(ctx, o.HelloTimeout)
	defer rcancel()
	_, data, err := ws.Read(rctx)
	if err != nil {
		return fmt.Errorf("hello_ack: %w", err)
	}
	f, err := fed.Decode(data)
	if err != nil {
		return err
	}
	ack, ok := f.(*fed.HelloAck)
	if !ok {
		return fmt.Errorf("expected hello_ack")
	}
	if ack.Proto != fed.Protocol {
		return fmt.Errorf("relay proto mismatch: %d", ack.Proto)
	}
	return nil
}

// engineServer serves req frames against one local engine.
type engineServer struct {
	cfg    ClientConfig
	conn   *fed.Conn
	logf   func(string, ...any)
	client *http.Client

	mu     sync.Mutex
	served map[string]context.CancelFunc
}

func (s *engineServer) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cancel := range s.served {
		cancel()
		delete(s.served, id)
	}
}

// servedPaths gates what the relay may ask this connector to do: the OpenAI
// surface plus the health endpoint. Anything else (LAN hosts, other ports,
// file paths) is refused — the tunnel must never become a proxy into this
// box's network. Dot segments are rejected outright: req.Path arrives
// decoded (URL.Path), so "/v1/%2e%2e/admin" is already "/v1/../admin" here,
// and Go's http.Client forwards dot segments verbatim — an engine behind a
// normalizing front (nginx, ServeMux) would serve the escaped path.
// An honest relay can only send decoded paths, but a hostile one can
// pre-encode (%2e%2e survives the gate verbatim and the engine front
// decodes it), so the unescaped form is checked too. path.Clean equality
// is too strict (it would reject a legitimate trailing slash); the segment
// check is exact.
func servedPathOK(method, path string) bool {
	if !strings.HasPrefix(path, "/") || !noDotSegments(path) {
		return false
	}
	if dec, err := url.PathUnescape(path); err == nil && !noDotSegments(dec) {
		return false
	}
	if path == "/health" && method == http.MethodGet {
		return true
	}
	return strings.HasPrefix(path, "/v1/")
}

func noDotSegments(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func (s *engineServer) onReq(_ *fed.Conn, f fed.Frame) {
	req, ok := f.(*fed.Req)
	if !ok {
		return
	}
	if !servedPathOK(req.Method, req.Path) {
		_ = s.conn.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID,
			Error: fmt.Sprintf("path %q not served by this connector", req.Path)})
		return
	}
	if len(req.Body) > fed.MaxBody {
		_ = s.conn.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID,
			Error: fmt.Sprintf("request body %d bytes over the %d cap", len(req.Body), fed.MaxBody)})
		return
	}
	s.mu.Lock()
	// Backpressure mirror of the relay side: an honest relay never exceeds
	// fed.MaxInflight (OpenRequest's semaphore caps it), but the package
	// treats a hostile relay as in scope — each req frame costs a goroutine
	// plus an engine generation, so the connector caps its own concurrency
	// and sheds the excess instead of being resource-exhausted.
	if len(s.served) >= fed.MaxInflight {
		s.mu.Unlock()
		_ = s.conn.Send(&fed.ResEnd{T: fed.TResEnd, ID: req.ID,
			Error: fmt.Sprintf("connector at capacity (%d in flight)", fed.MaxInflight)})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.served[req.ID] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.served, req.ID)
			s.mu.Unlock()
			cancel()
		}()
		s.serveFrame(ctx, req)
	}()
}

func (s *engineServer) onCancel(_ *fed.Conn, f fed.Frame) {
	c, ok := f.(*fed.Cancel)
	if !ok {
		return
	}
	s.mu.Lock()
	cancel := s.served[c.ID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// serveFrame forwards one request to the engine and pumps the answer back
// as frames. Every failure path ends in a ResEnd carrying the reason, so
// the relay's failover logic sees an honest error instead of a hang.
func (s *engineServer) serveFrame(ctx context.Context, req *fed.Req) {
	w := &frameWriter{conn: s.conn, id: req.ID}
	url := strings.TrimRight(s.cfg.EngineURL, "/") + req.Path
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url,
		bytes.NewReader(req.Body))
	if err != nil {
		w.fail("bad request: " + err.Error())
		return
	}
	httpReq.Header = fed.WireRequestHeader(req.Headers)
	if httpReq.Header.Get("Content-Type") == "" && len(req.Body) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if s.cfg.EngineToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+s.cfg.EngineToken)
	}
	resp, err := s.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			w.fail("cancelled")
			return
		}
		w.fail("engine request failed: " + err.Error())
		return
	}
	defer resp.Body.Close()
	w.head(resp.StatusCode, resp.Header)
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if werr := w.body(buf[:n]); werr != nil {
				s.logf("headless: streaming %s %s aborted: %v", req.Method, req.Path, werr)
				return
			}
		}
		if rerr == io.EOF {
			w.end()
			return
		}
		if rerr != nil {
			if ctx.Err() != nil {
				w.fail("cancelled")
			} else {
				w.fail("engine stream failed: " + rerr.Error())
			}
			return
		}
	}
}

// frameWriter emits res/res_body/res_end for one served request.
type frameWriter struct {
	conn   *fed.Conn
	id     string
	headAt sync.Once
}

func (w *frameWriter) head(status int, hdr http.Header) {
	w.headAt.Do(func() {
		_ = w.conn.Send(&fed.Res{T: fed.TRes, ID: w.id, Status: status,
			Headers: fed.ResponseHeaders(hdr)})
	})
}

func (w *frameWriter) body(p []byte) error {
	w.head(http.StatusOK, nil)
	for len(p) > 0 {
		c := min(len(p), fed.MaxBody)
		if err := w.conn.Send(&fed.ResBody{T: fed.TResBody, ID: w.id, Data: p[:c]}); err != nil {
			return err
		}
		p = p[c:]
	}
	return nil
}

func (w *frameWriter) end() {
	_ = w.conn.Send(&fed.ResEnd{T: fed.TResEnd, ID: w.id})
}

func (w *frameWriter) fail(reason string) {
	_ = w.conn.Send(&fed.ResEnd{T: fed.TResEnd, ID: w.id, Error: reason})
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
