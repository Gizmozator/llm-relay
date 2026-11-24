// Remote-backend API surface (plan 2026-11-24-remote-backend-v1): the
// connect endpoint a headless box dials (rb- token auth, on the relay port
// beside the federation one) and the admin endpoints that mint its connect
// token. A remote backend is created here and only here: the connector never
// self-registers — an admin always mints the token first.
package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// RemoteController is the remote-backend channel's control surface
// (implemented by *remote.Registry): the API layer stays free of the
// channel machinery.
type RemoteController interface {
	// ServeConn handles an authenticated connect request (WS handshake +
	// registry). tokenHash is the hash the bearer token resolved to —
	// the channel's revocation baseline.
	ServeConn(w http.ResponseWriter, r *http.Request, b *store.Backend, tokenHash string)
	// Kick drops a backend's live channel now (delete / disable / rotate
	// must not wait for the next pong timeout).
	Kick(backendID int64, reason string)
	// RoundTrip tunnels one remote:// request (the admin probe's
	// transport).
	RoundTrip(req *http.Request) (*http.Response, error)
	// BackendOnline answers the health cache for a remote:// backend
	// (online = live channel), so the dashboard's status badge reads the
	// registry, not a phantom HTTP probe.
	BackendOnline(b store.Backend) bool
}

// RemoteAuth resolves the rb- connect token to its backend row and hands
// the matched hash to the handler (the channel's revocation baseline). A
// disabled backend is refused distinctly (403) so an operator sees the real
// reason; an unknown token gets the fixed 401 phrase.
func (s *Server) RemoteAuth(next func(w http.ResponseWriter, r *http.Request, b *store.Backend, tokenHash string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Throttle before the DB lookup: this is the only endpoint that
		// answers unauthenticated callers with a query per attempt, so
		// token guessing must not be free. Successful auth resets the
		// bucket — a legitimately reconnecting connector never drains it.
		key := clientKey(r)
		if ok, wait := s.connectLimiter().allow(key, time.Now()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeErr(w, http.StatusTooManyRequests, "too many connect attempts, try again later")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		hash := auth.HashKey(tok)
		b, err := store.BackendByRemoteTokenHash(s.DB, hash)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusUnauthorized, "invalid connect token")
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "internal error")
		case !b.Enabled:
			writeErr(w, http.StatusForbidden, "backend disabled")
		default:
			s.connectLimiter().reset(key)
			next(w, r, b, hash)
		}
	}
}

// RemoteConnect upgrades the authenticated connector onto the registry.
func (s *Server) RemoteConnect(w http.ResponseWriter, r *http.Request, b *store.Backend, tokenHash string) {
	if s.Remote == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	s.Remote.ServeConn(w, r, b, tokenHash)
}

// AdminCreateRemoteBackend mints a remote backend + its connect token. The
// raw token is in this one response only — the DB keeps the hash.
func (s *Server) AdminCreateRemoteBackend(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	if s.Remote == nil {
		// Without the registry the token could never attach: refuse the
		// mint instead of handing out a dead credential.
		writeErr(w, http.StatusServiceUnavailable, "remote backend support not wired")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	b, token, err := store.CreateRemoteBackend(s.DB, in.Name)
	switch {
	case errors.Is(err, store.ErrExists):
		writeErr(w, http.StatusConflict, "backend name taken")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "backend create failed")
	default:
		writeJSON(w, http.StatusCreated, map[string]any{"backend": b, "token": token})
	}
}

// AdminRotateRemoteToken replaces a remote backend's connect token (the old
// one dies with this call) and kicks the live channel so a running
// connector cannot keep serving on a revoked token.
func (s *Server) AdminRotateRemoteToken(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	token, err := store.RotateRemoteToken(s.DB, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "backend not found")
	case errors.Is(err, store.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "backend is not a remote backend")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "token rotate failed")
	default:
		if s.Remote != nil {
			s.Remote.Kick(id, "token rotated")
		}
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
	}
}

// remoteProbeTimeout bounds one tunneled probe (both the /health and the
// /v1/models leg). A var so tests can shrink it.
var remoteProbeTimeout = 10 * time.Second

// probeRemote runs discovery for a remote backend through its live channel.
// The connector answers /health and /v1/models from its engine; an offline
// backend fails fast (RoundTrip is dial-class). An ONLINE connector whose
// engine wedges would otherwise hold the admin request open forever — the
// request context has no deadline of its own — so the probe carries one,
// mirroring proxy.Probe's bounded client. The remote RoundTrip honors the
// context: expiry sends a Cancel frame and unblocks the body read.
func (s *Server) probeRemote(r *http.Request, b *store.Backend) proxy.ProbeResult {
	if s.Remote == nil {
		return proxy.ProbeResult{Error: "remote backend support not wired"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteProbeTimeout)
	defer cancel()
	client := &http.Client{
		Transport: s.Remote,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return proxy.ProbeWith(ctx, client, b.BaseURL, "")
}
