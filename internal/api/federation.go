// Federation HTTP surface (plan 2026-11-22-relay-federation, task 1):
// peer self-service + admin management, the federation settings pair, and
// the server-token middleware guarding /federation/*. The WS channel itself
// is task 2; FederationPing is a stub so the auth matrix is testable now.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/ratelimit"
	"llm-relay/internal/store"
	"llm-relay/internal/version"
)

// PeerAuth wraps a /federation/* handler requiring a valid server token
// (Bearer, same shape as the relay's sk- keys). Type scoping is structural:
// server tokens live in peers, API keys in api_keys — neither table can
// satisfy the other's lookup, so a client key can never enroll a peer and a
// server token can never proxy models. The peer rides to the handler as the
// third argument (the Claims pattern), not in the request context.
func (s *Server) PeerAuth(next func(w http.ResponseWriter, r *http.Request, p *store.Peer)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Tier 1 (ticket #18): pre-auth per-source-IP bucket, shared with
		// /remote/v1/connect. Every attempt costs a hash + DB lookup
		// before the 401, so token guessing must not be free. A valid
		// token resets the bucket: a legitimately reconnecting spoke never
		// drains its own credits.
		key := ratelimit.SourceIP(r, s.TrustProxy)
		if ok, wait := s.connectLimiter().Allow(key, time.Now()); !ok {
			w.Header().Set("Retry-After", retryAfter(wait))
			writeErr(w, http.StatusTooManyRequests, "too many connect attempts, try again later")
			return
		}
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
			s.connectLimiter().Reset(key)
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
	// VersionWarnings lists the version mismatches on live federation
	// channels (hub: its spokes; spoke: the hub), state relative to this
	// build. Empty when unwired or everything matches.
	VersionWarnings() []version.Warn
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
// writePeerList stamps each row's version_state (this build vs the peer's
// last reported version, plan 2026-11-25-version-awareness) and writes the
// list envelope. "older" drives the small Federation-page note.
func (s *Server) writePeerList(w http.ResponseWriter, peers []store.Peer) {
	for i := range peers {
		peers[i].VersionState = version.State(s.Version, peers[i].Version)
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
}

// ListPeers serves GET /api/peers: the caller's own servers.
func (s *Server) ListPeers(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	peers, err := store.ListPeersByOwner(s.DB, c.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writePeerList(w, peers)
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
	s.audit(c, "peer.create", p.Name, "owner="+s.actorName(c))
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
	s.audit(c, "peer.rotate", fmt.Sprintf("#%d", id), "own server")
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
	s.audit(c, "peer.delete", fmt.Sprintf("#%d", id), "own server")
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
	s.writePeerList(w, peers)
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
	s.audit(c, "peer.rotate", fmt.Sprintf("#%d", id), "admin")
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
	s.audit(c, "peer.update", p.Name, fmt.Sprintf("enabled=%v", *in.Enabled))
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
	s.audit(c, "peer.delete", fmt.Sprintf("#%d", id), "admin")
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
		"hub_pin": f.HubPin,
		// Version awareness (plan 2026-11-25-version-awareness): this
		// build's stamp plus every live-channel mismatch (both directions,
		// spokes + remote backends) so the Federation page can annotate
		// versions without a second round-trip.
		"version": s.Version, "version_warnings": s.versionWarnings()}
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
	// The token never enters the audit trail — role and hub address only.
	s.audit(c, "federation.update", "instance",
		fmt.Sprintf("role=%s hub_url=%s", f.Role, f.HubURL))
	writeJSON(w, http.StatusOK, f)
}

// fedSince parses ?days=N (default 30, bounded 1..365) into the UTC
// RFC3339 lower bound the federation stats queries take.
// fedWindow resolves the stats window from the query string:
//
//	month=YYYY-MM → that UTC calendar month, [first day, first day of next)
//	days=N        → rolling last-N-days window (default 30), open upper bound
//
// month wins when both are present. ts is UTC RFC3339 text, so the
// lexicographic bounds are exact; the response echoes both so the caller
// can label the window it got.
func fedWindow(r *http.Request) (since, until string, err error) {
	if m := r.URL.Query().Get("month"); m != "" {
		t, perr := time.Parse("2006-01", m)
		if perr != nil {
			return "", "", errors.New("month must be YYYY-MM")
		}
		return t.Format(time.RFC3339), t.AddDate(0, 1, 0).Format(time.RFC3339), nil
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			return "", "", errors.New("days must be 1..365")
		}
		days = n
	}
	return time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339), "", nil
}

// AdminFedStats serves GET /api/admin/federation/stats?days=N|month=YYYY-MM
// — the fairness ledger, role-dependent:
//
//	hub:   {"role":"hub","since":…,"until":…,"users":[FedUserTotal…],
//	       "peers":[FedPeerTotal…],"local_only":[FedLocalOnlyEntry…]}
//	       (the fairness ledger: cross-peer flows and hub-local traffic on
//	       another account's hardware, double-entered as the host's served
//	       and the spender's consumed; local_only names the accounts whose
//	       window usage is attributable to nobody else — self-use or
//	       ownerless backends only — present on the Usage page, absent from
//	       the ledger by design, so the UI can say "nothing to attribute"
//	       instead of showing a blank. until is "" for rolling windows.)
//	spoke: {"role":"spoke","since":…,"until":…,"users":[FedUserTotal…],
//	       "local_only":[FedLocalOnlyEntry…],"served_by_source":[FedSourceTotals…],
//	       "consumed":[FedConsumedEntry…]}
//	       (the spoke's own fairness ledger (plan 2026-11-25-spoke-fairness):
//	       per local user served/consumed — local cross-user traffic, their
//	       hub-model spending as consumed, incoming hub traffic credited to
//	       their owned backends — plus the Federation virtual account (user_id
//	       -1) mirroring this box's hub-side owner account. The two box-level
//	       panels stay: what this box served to which hub-stamped identity,
//	       and what local users spent on federated models)
//	other: {"role":"standalone"}
func (s *Server) AdminFedStats(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	since, until, err := fedWindow(r)
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
		users, local, err := store.FedLedger(s.DB, since, until, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		peers, err := store.FedPeerTotals(s.DB, since, until)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since, "until": until, "users": users,
			"peers": peers, "local_only": local,
		})
	case store.FedRoleSpoke:
		// The spoke's own fairness ledger (plan 2026-11-25-spoke-fairness):
		// per local user served/consumed (local cross-user traffic, their
		// spending on hub-hosted models as consumed, incoming hub traffic
		// credited to their owned backends) plus the Federation virtual
		// account — the mirror of this box's hub-side owner account. The
		// two box-level panels stay: served_by_source (who on the hub we
		// served) and consumed (the raw federated-model slice per user).
		users, local, err := store.FedLedger(s.DB, since, until, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		served, err := store.FedServedBySource(s.DB, since, until, "")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		consumed, err := store.FedConsumedSlice(s.DB, since, until, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since, "until": until,
			"users": users, "local_only": local,
			"served_by_source": served, "consumed": consumed,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"role": role})
	}
}

// FedStats serves GET /api/federation/stats?days=N|month=YYYY-MM — the
// caller's own fairness totals (session auth, always scoped to the caller).
// Both role payloads carry "currency" (the display currency, EUR default)
// so the personal page needs no admin endpoint:
//
//	hub:   {"role":"hub","since":…,"until":…,"totals":[FedUserTotal],"local_only":[…]}
//	       — the caller's served/consumed entry: what their hosted models
//	       served to other accounts and what they spent on others'
//	       hardware, local and federated alike (empty when nothing is
//	       attributable to them; local_only then carries their own
//	       unattributable usage, so "no fairness activity" stays
//	       distinguishable from "no data at all").
//	spoke: {"role":"spoke","since":…,"until":…,"totals":[one FedUserTotal],
//	       "local_only":[caller's entry or empty],"consumed":[caller's slice]} —
//	       the caller's own fairness entry (their owned backends serving others
//	       vs their spending, local cross-user and hub-model alike) plus the
//	       raw federated-model slice. Served rows to the hub belong to the box,
//	       not to a session user, so they only appear on the admin view.
//	other: {"role":"standalone"}
func (s *Server) FedStats(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	since, until, err := fedWindow(r)
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
		totals, local, err := store.FedLedger(s.DB, since, until, c.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		cur, ok := s.currencySetting()
		if !ok {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since, "until": until, "totals": totals,
			"local_only": local, "currency": cur,
		})
	case store.FedRoleSpoke:
		// The caller's own fairness entry (scoped): their served (owned
		// backends serving other local accounts or the hub) and consumed
		// (local cross-user + their spending on hub-hosted models). The
		// Federation virtual account is box-level and stays on the admin
		// view. The consumed slice (raw federated-model spending) stays.
		totals, local, err := store.FedLedger(s.DB, since, until, c.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		consumed, err := store.FedConsumedSlice(s.DB, since, until, c.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		cur, ok := s.currencySetting()
		if !ok {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"role": role, "since": since, "until": until,
			"totals": totals, "local_only": local, "consumed": consumed, "currency": cur,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"role": role})
	}
}
