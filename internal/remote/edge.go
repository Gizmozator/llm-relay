// Connector-side edge proxy (ticket #17): an OPT-IN local listener that
// turns the headless box into a LAN-side gateway. Clients on the remote
// network point at this listener instead of the relay; whatever they send
// to /v1/* is forwarded over the existing remote-backend channel as a Req
// frame, and the relay runs it through its full proxy — key auth, catalog,
// groups, metering, routing. The client's sk- key rides verbatim; the rb-
// connect token authenticates the channel, never a /v1 request.
//
// DDoS design rules (the owner's, from ticket #17):
//
//   - Bind 127.0.0.1 by default: a bare port or ":port" gets the loopback
//     host. Only an explicit host (0.0.0.0, a LAN address) binds elsewhere,
//     and that is a loud operator decision — the relay cannot defend a
//     public connector box (README carries the warning).
//   - Fail fast, never queue: the channel's fed.MaxInflight tunnel slots
//     are claimed with OpenRequest's non-blocking semaphore; when they are
//     full the listener answers 503 immediately. A flood finds no local
//     backlog to pile into.
//   - Local pre-auth caps: concurrency (edgeMaxConcurrent), read/header
//     timeouts and a body-size limit — cheap, applied before any server
//     round-trip, and together they blunt slow-loris.
//
// Cancel propagates both ways on the stream: the local client hanging up
// sends a Cancel frame (the relay aborts the served request), and the
// channel dying ends the stream.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llm-relay/internal/fed"
)

const (
	// edgeMaxConcurrent is the pre-auth local concurrency cap: beyond it
	// requests are shed with 503, never queued. It sits above the
	// channel's fed.MaxInflight so the channel, not this cap, is the
	// usual bound — its job is bounding local work (body reads, pumps)
	// before the channel check even runs.
	edgeMaxConcurrent = 64
	// edgeReadHeaderTimeout / edgeReadTimeout bound slow-loris: headers
	// must arrive within 15 s and the whole request body within 120 s
	// (the body is also size-capped at fed.MaxBody). No WriteTimeout:
	// SSE streams must survive.
	edgeReadHeaderTimeout = 15 * time.Second
	edgeReadTimeout       = 120 * time.Second
	edgeIdleTimeout       = 120 * time.Second
)

// Edge is the connector's local proxy listener. One Edge survives
// reconnects: runOnce publishes each live channel to it (attach) and
// unpublishes it on death (detach); between channels the listener answers
// 502 — fail fast, no local queue waiting for the redial.
type Edge struct {
	// Addr is the normalized host:port the listener binds.
	Addr string
	logf func(string, ...any)

	slots chan struct{} // pre-auth concurrency cap

	mu   sync.Mutex
	conn *fed.Conn
}

// NewEdge resolves the PROXY_LISTEN value. An empty value is the caller's
// signal to not build an Edge at all (the connector binds nothing unless
// asked); here it is an error so a misconfigured call cannot silently
// create a listener-less Edge.
func NewEdge(listen string, logf func(string, ...any)) (*Edge, error) {
	addr, err := normalizeEdgeAddr(listen)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = log.Printf
	}
	return &Edge{Addr: addr, logf: logf, slots: make(chan struct{}, edgeMaxConcurrent)}, nil
}

// normalizeEdgeAddr resolves the PROXY_LISTEN value to a host:port. A
// bare port ("8080") or ":port" binds loopback — the default is NEVER a
// public bind. Only an explicit host binds elsewhere: "0.0.0.0:8080" or a
// concrete address mean "I know what I am doing" (see the README warning:
// the relay cannot defend a public connector box).
func normalizeEdgeAddr(listen string) (string, error) {
	v := strings.TrimSpace(listen)
	if v == "" {
		return "", errors.New("edge: empty listen address")
	}
	if !strings.Contains(v, ":") {
		v = "127.0.0.1:" + v
	} else if strings.HasPrefix(v, ":") {
		v = "127.0.0.1" + v
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("edge: bad PROXY_LISTEN %q: %w", listen, err)
	}
	if host == "" {
		return "", fmt.Errorf("edge: bad PROXY_LISTEN %q", listen)
	}
	// Reject a non-numeric port here rather than at bind time: a typo in
	// the env file should name itself at boot, not surface as an opaque
	// "listen tcp: unknown port" from net.Listen.
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return "", fmt.Errorf("edge: bad PROXY_LISTEN %q: invalid port %q", listen, port)
	}
	return net.JoinHostPort(host, port), nil
}

// attach publishes a live channel to the listener (runOnce, after the
// handshake). A reconnect simply replaces the old channel.
func (e *Edge) attach(c *fed.Conn) {
	e.mu.Lock()
	e.conn = c
	e.mu.Unlock()
}

// detach drops the channel only if it still names c — the same CAS rule
// the registry's unregister uses: a replaced channel must not be evicted
// by the old one's exit.
func (e *Edge) detach(c *fed.Conn) {
	e.mu.Lock()
	if e.conn == c {
		e.conn = nil
	}
	e.mu.Unlock()
}

func (e *Edge) current() *fed.Conn {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conn
}

// Serve binds the listener and serves until ctx is done. A bind failure
// returns to the caller — main treats it as fatal: an operator who asked
// for the edge expects it to actually be up, not silently absent.
func (e *Edge) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              e.Addr,
		Handler:           e.Handler(),
		ReadHeaderTimeout: edgeReadHeaderTimeout,
		ReadTimeout:       edgeReadTimeout,
		IdleTimeout:       edgeIdleTimeout,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	e.logf("headless: edge proxy listening on %s (LAN clients point their /v1 here)", e.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler exposes the listener's handler (bound or test-wrapped).
func (e *Edge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", e.forward)
	return mux
}

// forward serves one client request out over the relay channel.
func (e *Edge) forward(w http.ResponseWriter, r *http.Request) {
	// Path gate: the OpenAI surface only. The listener is a /v1 gateway,
	// not a general proxy onto whatever else the relay might serve.
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeEdgeError(w, http.StatusNotFound, "not found")
		return
	}
	// Pre-auth concurrency cap: shed immediately, never queue.
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeEdgeError(w, http.StatusServiceUnavailable, "connector at capacity, try again shortly")
		return
	}
	conn := e.current()
	if conn == nil || conn.Closed() {
		w.Header().Set("Retry-After", "1")
		writeEdgeError(w, http.StatusBadGateway, "no relay connection")
		return
	}
	id := newRequestID()
	ch, release, err := conn.OpenRequest(id)
	if err != nil {
		// fed.ErrTooManyInflight: all fed.MaxInflight tunnel slots are
		// busy. Fail fast — 503 now; a flood must find no local backlog
		// to pile into (the whole point of the cap being honest).
		w.Header().Set("Retry-After", "1")
		if errors.Is(err, fed.ErrClosed) {
			writeEdgeError(w, http.StatusBadGateway, "no relay connection")
		} else {
			writeEdgeError(w, http.StatusServiceUnavailable, "relay channel at capacity, try again shortly")
		}
		return
	}
	// Body cap: the tunnel's Req frame limit is fed.MaxBody, so anything
	// larger could never cross — refuse locally with an honest 413
	// instead of paying a round-trip for a transport error.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, fed.MaxBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeEdgeError(w, http.StatusRequestEntityTooLarge, "request body over the tunnel cap")
			return
		}
		writeEdgeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	hdr := edgeRequestHeaders(r.Header)
	// Stamp the direct client's address. edgeRequestHeaders already dropped
	// any client-supplied X-Forwarded-For / X-Real-IP (the connector cannot
	// vouch for what a LAN client claims), so this append is the ONLY
	// X-Forwarded-For on the frame: the relay (with TRUST_PROXY_HEADERS on)
	// sees exactly the socket peer this operator-owned box accepted — the
	// honest meaning of "trust your own proxy."
	if ip, _, perr := net.SplitHostPort(r.RemoteAddr); perr == nil {
		hdr = append(hdr, [2]string{"X-Forwarded-For", ip})
	}
	frame := &fed.Req{T: fed.TReq, ID: id, Method: r.Method, Path: r.URL.Path,
		Headers: hdr, Body: body}
	if err := conn.Send(frame); err != nil {
		release()
		writeEdgeError(w, http.StatusBadGateway, "relay channel unavailable")
		return
	}
	// Wait for the response head. A ResEnd before any Res is a serving-
	// side refusal (path gate, oversized body, edge not wired) — surface
	// it as a 502 with the reason.
	for {
		select {
		case f := <-ch:
			switch t := f.(type) {
			case *fed.Res:
				pumpEdgeResponse(w, conn, id, ch, release, t, r.Context())
				return
			case *fed.ResEnd:
				release()
				if t.Error != "" {
					writeEdgeError(w, http.StatusBadGateway, t.Error)
				} else {
					writeEdgeError(w, http.StatusBadGateway, "relay ended the response before headers")
				}
				return
			}
		case <-conn.Done():
			release()
			writeEdgeError(w, http.StatusBadGateway, "relay channel lost")
			return
		case <-r.Context().Done():
			// The local client hung up before the head.
			_ = conn.Send(&fed.Cancel{T: fed.TCancel, ID: id})
			release()
			return
		}
	}
}

// pumpEdgeResponse writes the head and streams the body: every ResBody
// chunk goes to the local client and flushes (SSE must flow live), and
// either side dying sends a Cancel so the relay stops generating into a
// dead stream.
func pumpEdgeResponse(w http.ResponseWriter, conn *fed.Conn, id string, ch <-chan fed.Frame, release func(), head *fed.Res, ctx context.Context) {
	defer release()
	for k, vs := range fed.WireResponseHeader(head.Headers) {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(head.Status)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case f := <-ch:
			switch t := f.(type) {
			case *fed.ResBody:
				if len(t.Data) == 0 {
					continue
				}
				if _, err := w.Write(t.Data); err != nil {
					// Local client hung up mid-stream: tell the relay so
					// its upstream call (and metering) sees the hang-up.
					_ = conn.Send(&fed.Cancel{T: fed.TCancel, ID: id})
					return
				}
				if fl != nil {
					fl.Flush()
				}
			case *fed.ResEnd:
				// t.Error mid-stream leaves the body truncated; the
				// client's parser sees a closed stream — the honest
				// maximum after the status line already went out.
				return
			}
		case <-conn.Done():
			return
		case <-ctx.Done():
			_ = conn.Send(&fed.Cancel{T: fed.TCancel, ID: id})
			return
		}
	}
}

// writeEdgeError mirrors the relay's error envelope so clients see one
// shape from either end of the tunnel.
func writeEdgeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}
