// The outbound tunnel (plan 2026-11-22-relay-federation, task 3): one
// tunneled HTTP request per proxy attempt, multiplexed over the peer's
// channel by request id. The proxy's shared Client carries a
// scheme-dispatching transport, so a peer:// or hub:// backend reaches here
// exactly like a dial error reaches the group retry loop: any failure
// before the response head (channel gone, in-flight cap, oversized body) is
// returned as a RoundTrip error, which the pre-first-byte failover already
// knows how to handle.
//
// Routing: a spoke sends hub:// requests to the hub with TargetPeer=0
// ("serve there") and peer://<id> requests with TargetPeer=<id> ("transit
// to that spoke"). The hub sends peer://<id> straight to that peer with
// TargetPeer=0. OriginPeer is left 0 on the wire: each receiver labels a
// frame with the peer id of the channel it arrived on (a spoke does not
// know its own id; the hub does the relabeling on transit).
package fed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// newRequestID mints a 128-bit hex id for one tunneled request. Random,
// not sequential: ids from different spokes share the hub's transit space
// and must never collide.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Linux; a failing reader must not
		// take the tunnel down, so fall back to a fixed prefix — the
		// duplicate-id rejection in OpenRequest turns a real collision
		// into a clean error, never a crossed stream.
		return "deadbeefdeadbeefdeadbeefdeadbee0"
	}
	return hex.EncodeToString(b[:])
}

// stripRequestHeaders drops what must never cross a federation hop: the
// caller's relay credentials (the serving side applies its own backend keys
// locally), hop-by-hop framing, and Host. Everything else — Content-Type,
// X-Session-Id (affinity must survive the hop), anthropic-version — rides
// through untouched.
func stripRequestHeaders(h http.Header) Headers {
	var out Headers
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "cookie", "proxy-authorization",
			"host", "content-length", "connection", "keep-alive", "proxy-connection",
			"te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vs {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

// responseHeaders filters a served response's headers for the return trip:
// hop-by-hop framing goes, Content-Length goes (the pipe-delivered body is
// authoritative), Content-Type and every API-relevant header stays.
func responseHeaders(h http.Header) Headers {
	var out Headers
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailer", "transfer-encoding", "upgrade", "content-length",
			"date", "server":
			continue
		}
		for _, v := range vs {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

// tunnel is the resolved route for one synthetic URL: how to send frames,
// how to open a request-id stream, and when the path died. The hub-side
// variant has no single Conn (the registry resolves the peer per call), so
// the route is closures, not a Conn.
type tunnel struct {
	send   func(Frame) error
	open   func(id string) (<-chan Frame, func(), error)
	done   <-chan struct{}
	target int64 // Req.TargetPeer: 0 = serve at the far end
}

// tunnelRoute resolves a synthetic URL to its tunnel, ErrNotOnline when
// the channel it rides is not up.
func (m *Manager) tunnelRoute(u *url.URL) (*tunnel, error) {
	switch u.Scheme {
	case "hub":
		conn := m.spokeConn()
		if conn == nil || conn.Closed() {
			return nil, ErrNotOnline
		}
		return &tunnel{send: conn.Send, open: conn.OpenRequest, done: conn.Done()}, nil
	case "peer":
		peerID, err := strconv.ParseInt(u.Host, 10, 64)
		if err != nil || peerID <= 0 {
			return nil, fmt.Errorf("bad peer backend %q", u.String())
		}
		if m.currentRole() == store.FedRoleHub {
			// Hub → peer: straight to that peer, serve there.
			return &tunnel{
				send: func(f Frame) error { return m.hub.Send(peerID, f) },
				open: func(id string) (<-chan Frame, func(), error) {
					return m.hub.OpenRequest(peerID, id)
				},
				done: m.hub.connDone(peerID),
			}, nil
		}
		// Spoke → peer: via the hub, transit to <id>.
		conn := m.spokeConn()
		if conn == nil || conn.Closed() {
			return nil, ErrNotOnline
		}
		return &tunnel{send: conn.Send, open: conn.OpenRequest, done: conn.Done(),
			target: peerID}, nil
	}
	return nil, fmt.Errorf("not a federation scheme: %q", u.Scheme)
}

// RoundTripFed answers one peer:// or hub:// request (proxy.FedTransport).
// Errors here are dial-class: the proxy's group walk retries the next
// member on them (plan: the in-flight cap surfaces as a failover signal).
func (m *Manager) RoundTripFed(req *http.Request) (*http.Response, error) {
	tun, err := m.tunnelRoute(req.URL)
	if err != nil {
		return nil, err
	}
	id := newRequestID()
	ch, release, err := tun.open(id)
	if err != nil {
		// ErrTooManyInflight / ErrClosed: a dial error, so the group walk
		// fails over to the next member (plan: cap surfaces as backpressure).
		return nil, err
	}
	var body []byte
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, MaxBody+1))
		if err != nil {
			release()
			return nil, err
		}
	}
	if len(body) > MaxBody {
		release()
		return nil, fmt.Errorf("federation tunnel: request body %d bytes over the %d cap", len(body), MaxBody)
	}
	frame := &Req{T: TReq, ID: id, TargetPeer: tun.target, Method: req.Method,
		Path: req.URL.Path, Headers: stripRequestHeaders(req.Header), Body: body,
		FedUser: m.attributionFor(req.Context())}
	if err := tun.send(frame); err != nil {
		release()
		return nil, err
	}
	// Wait for the response head. A ResEnd before any Res is a serving-side
	// failure (unknown model, loop guard, oversize) — surface it as a dial
	// error so failover still applies.
	for {
		select {
		case f := <-ch:
			switch t := f.(type) {
			case *Res:
				pr, pw := io.Pipe()
				go m.pumpTunnel(tun, id, ch, release, pw, req.Context())
				return &http.Response{
					Status:     http.StatusText(t.Status),
					StatusCode: t.Status,
					Proto:      "HTTP/1.1",
					ProtoMajor: 1, ProtoMinor: 1,
					Header:        headersFrom(t.Headers),
					Body:          pr,
					ContentLength: -1,
					Request:       req,
				}, nil
			case *ResEnd:
				release()
				if t.Error != "" {
					return nil, fmt.Errorf("federation: %s", t.Error)
				}
				return nil, errors.New("federation: response ended before headers")
			}
			// Anything else before the head is protocol noise: ignore.
		case <-tun.done:
			release()
			return nil, ErrClosed
		case <-req.Context().Done():
			// The consumer hung up before the head: tell the far end (it
			// may already be dialing its upstream) and release the slot.
			_ = tun.send(&Cancel{T: TCancel, ID: id})
			release()
			return nil, req.Context().Err()
		}
	}
}

// attributionFor resolves the FedUser stamp for one outbound Req frame
// (task 4 attribution): the HUB is the authority. A hub-direct consumer
// travels under its own username; a request the hub forwards on a spoke's
// behalf travels under the ORIGIN peer's owner name (the hub holds that
// identity — spoke-local usernames never leave the spoke). A spoke sends
// nothing.
func (m *Manager) attributionFor(ctx context.Context) string {
	if m.currentRole() != store.FedRoleHub {
		return ""
	}
	if fs := proxy.FedServeFrom(ctx); fs != nil {
		if fs.OriginPeer == 0 {
			return ""
		}
		name, err := m.peerOwnerName(fs.OriginPeer)
		if err != nil {
			m.opts.Log.Printf("fed: attribution for origin peer %d: %v", fs.OriginPeer, err)
		}
		return name
	}
	return proxy.RequestUserFrom(ctx)
}

// peerOwnerName resolves a peer's owner username (the hub-side identity
// stamped onto transit frames and transit usage rows).
func (m *Manager) peerOwnerName(peerID int64) (string, error) {
	p, err := store.PeerByID(m.db, peerID)
	if err != nil {
		return "", err
	}
	u, err := store.UserByID(m.db, p.OwnerUserID)
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// pumpTunnel copies routed frames into the response pipe until the stream
// ends, the channel dies, or the consumer's context goes away (then a
// CancelFrame tells the serving side to abort its upstream call).
func (m *Manager) pumpTunnel(tun *tunnel, id string, ch <-chan Frame, release func(), pw *io.PipeWriter, ctx context.Context) {
	defer release()
	defer pw.Close()
	for {
		select {
		case f := <-ch:
			switch t := f.(type) {
			case *ResBody:
				if len(t.Data) == 0 {
					continue
				}
				if len(t.Data) > MaxBody {
					pw.CloseWithError(fmt.Errorf("federation: body chunk %d over cap", len(t.Data)))
					return
				}
				if _, err := pw.Write(t.Data); err != nil {
					return // pipe closed by the consumer
				}
			case *ResEnd:
				if t.Error != "" {
					pw.CloseWithError(errors.New(t.Error))
				}
				return
			}
		case <-tun.done:
			pw.CloseWithError(ErrClosed)
			return
		case <-ctx.Done():
			_ = tun.send(&Cancel{T: TCancel, ID: id})
			pw.CloseWithError(ctx.Err())
			return
		}
	}
}

// headersFrom rebuilds an http.Header from wire pairs (repeated keys fold
// back into the slice).
func headersFrom(h Headers) http.Header {
	out := http.Header{}
	for _, kv := range h {
		out.Add(kv[0], kv[1])
	}
	return out
}
