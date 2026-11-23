// Federation HTTP surface (plan 2026-11-22-relay-federation, task 1):
// peer self-service + admin management, the federation settings pair, and
// the server-token middleware guarding /federation/*. The WS channel itself
// is task 2; FederationPing is a stub so the auth matrix is testable now.
package api

import (
	"errors"
	"net/http"
	"strings"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
)

// PeerAuth wraps a /federation/* handler requiring a valid server token
// (Bearer, same shape as the relay's sk- keys). Type scoping is structural:
// server tokens live in peers, API keys in api_keys — neither table can
// satisfy the other's lookup, so a client key can never enroll a peer and a
// server token can never proxy models. The peer rides to the handler as the
// third argument (the Claims pattern), not in the request context.
func (s *Server) PeerAuth(next func(w http.ResponseWriter, r *http.Request, p *store.Peer)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		p, err := store.GetPeerByKeyHash(s.DB, auth.HashKey(tok))
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusUnauthorized, "invalid server token")
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "internal error")
		case !p.Enabled:
			// Known but disabled: distinct from "unknown" so an operator
			// debugging a disabled peer sees the real reason.
			writeErr(w, http.StatusForbidden, "peer disabled")
		default:
			next(w, r, p)
		}
	}
}

// FedController is the federation channel's control surface (implemented
// by *fed.Manager). The API layer talks to it through this interface only,
// so it stays free of the channel machinery.
type FedController interface {
	// Reload re-reads the federation settings and reconciles the spoke
	// dialer (start/stop); called after every successful settings PUT.
	Reload()
	// ServeHub handles an authenticated connect request (role gate +
	// WS handshake + registry).
	ServeHub(w http.ResponseWriter, r *http.Request, p *store.Peer)
	// KickPeer drops a peer's live channel now (disable/rotate/delete
	// must not wait for the hub's revocation sweep).
	KickPeer(peerID int64, reason string)
	// CatalogChanged signals that this instance's shareable catalog may
	// have changed (model fed_visibility/enabled, backend-enabled, group
	// edits); the manager debounces and re-pushes to every live channel.
	CatalogChanged()
	// PeerRemoved tears down a deleted peer's materialized rows and
	// stops relaying its catalog (called after the peer row is deleted).
	PeerRemoved(peerID int64)
	// SyntheticOnline answers the health cache for a peer://hub://
	// backend: its health is the channel, there is nothing to probe.
	SyntheticOnline(b store.Backend) bool
}

// catalogChanged tells the federation manager the shareable catalog may
// have moved (nil-safe when federation is not wired; a no-op cost on a
// standalone relay — the debounced push finds no channels to push to).
func (s *Server) catalogChanged() {
	if s.Fed != nil {
		s.Fed.CatalogChanged()
	}
}

// kickPeer drops the peer's live federation channel if one exists
// (nil-safe when the federation manager is not wired).
func (s *Server) kickPeer(id int64, reason string) {
	if s.Fed != nil {
		s.Fed.KickPeer(id, reason)
	}
}

// peerRemoved is the delete-side companion of kickPeer: drop the live
// channel AND the peer's materialized rows (nil-safe, see kickPeer).
func (s *Server) peerRemoved(id int64) {
	if s.Fed != nil {
		s.Fed.PeerRemoved(id)
	}
}

// FederationConnect serves GET /federation/v1/connect (relay port) behind
// PeerAuth: the fed- token is already verified here; the Manager applies
// the role gate (404 unless role=hub) and runs the WS upgrade + hello
// handshake (task 2).
func (s *Server) FederationConnect(w http.ResponseWriter, r *http.Request, p *store.Peer) {
	if s.Fed == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	s.Fed.ServeHub(w, r, p)
}

// FederationPing is the task-1 token-path probe, kept alongside the WS
// channel: a cheap way to verify a fed- token works without opening a
// channel (the connect endpoint cannot answer a plain GET).
func (s *Server) FederationPing(w http.ResponseWriter, r *http.Request, p *store.Peer) {
	writeJSON(w, http.StatusOK, map[string]any{
		"pong": true, "peer_id": p.ID, "peer_name": p.Name,
	})
}

// ---------------------------------------------------------------- self-service

// The peer shape both list endpoints return is store.Peer directly: the key
// hash never leaves the store; the raw token appears only in create/rotate
// responses.
func writePeerList(w http.ResponseWriter, peers []store.Peer) {
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
}

// ListPeers serves GET /api/peers: the caller's own servers.
func (s *Server) ListPeers(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	peers, err := store.ListPeersByOwner(s.DB, c.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writePeerList(w, peers)
}

// CreatePeer serves POST /api/peers {name}: mint a server token, show the
// raw string exactly once (the create response is the only place it exists).
func (s *Server) CreatePeer(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "peer name required")
		return
	}
	p, plain, err := store.CreatePeer(s.DB, in.Name, c.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "peer create failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"peer": p, "token": plain})
}

// ownPeer resolves a peer for a self-service call: another user's peer is
// reported as ErrNotFound — indistinguishable from a missing one (404, no
// existence leak, the RevokeKey pattern). Genuine store errors pass
// through so handlers can answer 500 instead of a misleading 404.
func (s *Server) ownPeer(id int64, c *auth.Claims) (*store.Peer, error) {
	p, err := store.PeerByID(s.DB, id)
	if err != nil {
		return nil, err
	}
	if p.OwnerUserID != c.UserID {
		return nil, store.ErrNotFound
	}
	return p, nil
}

// RotateMyPeer serves POST /api/peers/{id}/rotate (own only): the old token
// dies with the hash swap; the new raw token rides this response only.
func (s *Server) RotateMyPeer(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := s.ownPeer(id, c); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "peer not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	plain, err := store.RotatePeer(s.DB, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "peer rotate failed")
		return
	}
	// The old token is dead: the live channel rides it, so it dies too.
	// A spoke reconnects with the new token once its config is updated.
	s.kickPeer(id, "rotated")
	writeJSON(w, http.StatusOK, map[string]any{"token": plain})
}

// DeleteMyPeer serves DELETE /api/peers/{id} (own only).
func (s *Server) DeleteMyPeer(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := s.ownPeer(id, c); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "peer not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := store.DeletePeer(s.DB, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "peer delete failed")
		return
	}
	s.kickPeer(id, "deleted")
	s.peerRemoved(id)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- admin

// AdminListPeers serves GET /api/admin/peers: every peer, owner name and
// shared-model count included (Servers page data).
func (s *Server) AdminListPeers(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	peers, err := store.ListPeers(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writePeerList(w, peers)
}

// AdminRotatePeer serves POST /api/admin/peers/{id}/rotate: admins may
// rotate any peer (the Servers page's break-glass path).
func (s *Server) AdminRotatePeer(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	plain, err := store.RotatePeer(s.DB, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "peer not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "peer rotate failed")
		return
	}
	s.kickPeer(id, "rotated")
	writeJSON(w, http.StatusOK, map[string]any{"token": plain})
}

// AdminUpdatePeer serves PATCH /api/admin/peers/{id} {enabled}: disable is
// the incident lever — it drops the peer's live federation channel at
// once (kick), not at the next revocation sweep.
func (s *Server) AdminUpdatePeer(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil || in.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "enabled required")
		return
	}
	err = store.SetPeerEnabled(s.DB, id, *in.Enabled)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "peer not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "peer update failed")
		return
	}
	if !*in.Enabled {
		s.kickPeer(id, "disabled")
	}
	p, err := store.PeerByID(s.DB, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// AdminDeletePeer serves DELETE /api/admin/peers/{id}.
func (s *Server) AdminDeletePeer(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	err = store.DeletePeer(s.DB, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "peer not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "peer delete failed")
		return
	}
	s.kickPeer(id, "deleted")
	s.peerRemoved(id)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- settings

// AdminGetFederation serves GET /api/admin/federation. hub_token is echoed
// back: it is the hub's token the admin pasted in (not a secret this server
// minted), and the spoke admin needs to read it to verify the config.
func (s *Server) AdminGetFederation(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	f, err := store.GetFederationSettings(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// AdminPutFederation serves PUT /api/admin/federation {role, hub_url,
// hub_token} — full replacement (PUT convention, cf. settings). Validation
// lives in the store; ErrFedConfig is the 400 signal.
func (s *Server) AdminPutFederation(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in store.FederationSettings
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := store.PutFederationSettings(s.DB, in); err != nil {
		if errors.Is(err, store.ErrFedConfig) {
			// The wrapper text is a fixed validation phrase (no internal
			// detail), safe to surface verbatim.
			writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "invalid federation config: "))
			return
		}
		writeErr(w, http.StatusInternalServerError, "federation settings update failed")
		return
	}
	// Live-safe role change: the Manager reconciles the spoke dialer
	// with the stored config (spoke→standalone drops the channel,
	// standalone→spoke starts it; hub acceptance is role-checked per
	// request, so nothing else needs to happen).
	if s.Fed != nil {
		s.Fed.Reload()
	}
	f, err := store.GetFederationSettings(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, f)
}
