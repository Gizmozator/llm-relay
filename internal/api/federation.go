// Federation HTTP surface (plan 2026-11-22-relay-federation, task 1):
// peer self-service + admin management, the federation settings pair, and
// the server-token middleware guarding /federation/*. The WS channel itself
// is task 2; FederationPing is a stub so the auth matrix is testable now.
package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// A non-nil error means the settings are stored but the live reconcile
	// failed (transient settings read, failed purge); the next PUT or a
	// restart retries it.
	Reload() error
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
	// Role reports the reconciled federation role ("hub"/"spoke"/
	// "standalone"); the stats endpoints shape their payload by it.
	Role() string
	// Online reports whether a peer's WS channel is live at this hub
	// (false on a standalone/spoke box — the registry is empty there).
	Online(peerID int64) bool
	// SpokeStatus reports the spoke's own channel: live? plus the hub's
	// hello_ack identity (empty when disconnected).
	SpokeStatus() (connected bool, hubName, hubVersion string)
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
// shared-model count included (Servers page data), plus the live-channel
// flag stamped from the hub registry.
func (s *Server) AdminListPeers(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	peers, err := store.ListPeers(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for i := range peers {
		if s.Fed != nil {
			peers[i].Online = s.Fed.Online(peers[i].ID)
		}
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
// For a spoke the response carries a "spoke" object with the live channel
// state (connected? + the hub's hello_ack identity) so the Federation page
// can show connection status without a second round-trip.
func (s *Server) AdminGetFederation(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	f, err := store.GetFederationSettings(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := map[string]any{"role": f.Role, "hub_url": f.HubURL, "hub_token": f.HubToken,
		"hub_pin": f.HubPin}
	if f.Role == store.FedRoleSpoke {
		connected, hubName, hubVersion := false, "", ""
		if s.Fed != nil {
			connected, hubName, hubVersion = s.Fed.SpokeStatus()
		}
		resp["spoke"] = map[string]any{
			"connected": connected, "hub_name": hubName, "hub_version": hubVersion,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// FedRole serves GET /api/federation/role (session auth, any user): the
// reconciled role only — the UI surface that is hub-specific (the setup
// guide) gates on it without needing the admin settings payload.
func (s *Server) FedRole(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	role := store.FedRoleStandalone
	if s.Fed != nil {
		role = s.Fed.Role()
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": role})
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
	// request, so nothing else needs to happen). A failed reconcile
	// leaves the stored settings in place — report it distinctly so the
	// operator knows the live state may lag until the next PUT/restart.
	if s.Fed != nil {
		if err := s.Fed.Reload(); err != nil {
			writeErr(w, http.StatusInternalServerError, "settings saved but live reload failed")
			return
		}
	}
	f, err := store.GetFederationSettings(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// fedSince parses ?days=N (default 30, bounded 1..365) into the UTC
// RFC3339 lower bound the federation stats queries take.
func fedSince(r *http.Request) (string, error) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			return "", errors.New("days must be 1..365")
		}
		days = n
	}
	return time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339), nil
}

// AdminFedStats serves GET /api/admin/federation/stats?days=N — the
// federation ledger, role-dependent:
//
//	hub:   {"role":"hub","since":…,"users":[FedUserTotal…],"peers":[FedPeerTotal…]}
//	       (the fairness ledger: every cross-peer flow wrote a consumed row
//	       on the spender and a served row on the server)
//	spoke: {"role":"spoke","since":…,"served_by_source":[FedSourceTotals…],
//	       "consumed":[FedConsumedEntry…]}
//	       (what this box served to which hub-stamped identity, and what
//	       local users spent on federated models)
//	other: {"role":"standalone"}
func (s *Server) AdminFedStats(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	since, err := fedSince(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	role := store.FedRoleStandalone
	if s.Fed != nil {
		role = s.Fed.Role()
	}
	switch role {
	case store.FedRoleHub:
		users, err := store.FedUserTotals(s.DB, since, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		peers, err := store.FedPeerTotals(s.DB, since)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since, "users": users, "peers": peers,
		})
	case store.FedRoleSpoke:
		served, err := store.FedServedBySource(s.DB, since, "")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		consumed, err := store.FedConsumedSlice(s.DB, since, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since,
			"served_by_source": served, "consumed": consumed,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"role": role})
	}
}

// FedStats serves GET /api/federation/stats?days=N — the caller's own
// federation totals (session auth, always scoped to the caller):
//
//	hub:   {"role":"hub","since":…,"totals":[FedUserTotal]} — the caller's
//	       served/consumed entry (empty when they have no federation rows).
//	spoke: {"role":"spoke","since":…,"consumed":[FedConsumedEntry]} — what
//	       the caller spent on federated models. Served rows belong to the
//	       box, not to a session user, so they only appear on the admin view.
//	other: {"role":"standalone"}
func (s *Server) FedStats(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	since, err := fedSince(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	role := store.FedRoleStandalone
	if s.Fed != nil {
		role = s.Fed.Role()
	}
	switch role {
	case store.FedRoleHub:
		totals, err := store.FedUserTotals(s.DB, since, c.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since, "totals": totals,
		})
	case store.FedRoleSpoke:
		consumed, err := store.FedConsumedSlice(s.DB, since, c.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since, "consumed": consumed,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"role": role})
	}
}
