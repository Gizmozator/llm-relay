// Package api implements the JWT-authenticated dashboard JSON API.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

type Server struct {
	DB           *sql.DB
	JWTSecret    string
	JWTTTL       time.Duration
	RelayURL     string
	DashboardURL string
	// Flight sees the proxy's in-flight request counts (nil = no live data).
	Flight FlightSource
	// Fed drives the federation channel (nil = federation endpoints 404).
	Fed FedController
	// Remote drives remote-backend connections (nil = connect 404s and
	// remote probes report not wired).
	Remote RemoteController
	// TrustProxy lets X-Forwarded-Proto: https satisfy the machine-channel
	// TLS gate (the TLS-terminating reverse-proxy deployment). Same trust
	// boundary as the proxy's XFF handling; off by default.
	TrustProxy bool
	// SelfTLS is set when the relay binds its own TLS listener (TLS_PORT!=0).
	// It narrows the TrustProxy branch of the gate: a relay that serves its
	// own TLS has no proxy terminating TLS in front of the machine channels,
	// so a cleartext request on the plain port claiming X-Forwarded-Proto is
	// a forged bypass, not a proxied TLS session — peers must use the wss
	// listener instead. X-Forwarded-Proto is honoured only when SelfTLS is
	// false (the proxied deployment, TLS_PORT=0).
	SelfTLS bool

	loginLimit *loginLimiter
	limitOnce  sync.Once
	// connectLimit guards the remote-backend connect endpoint (token
	// guessing costs a DB query per attempt). Separate bucket space from
	// login: a connector flapping on the relay port must not lock out
	// dashboard logins from the same address.
	connectLimit *loginLimiter
	connectOnce  sync.Once
	health       *proxy.HealthCache
	healthOnce   sync.Once
}

// limiter returns the lazily created login limiter (Server is built as a
// struct literal in main and tests).
func (s *Server) limiter() *loginLimiter {
	s.limitOnce.Do(func() { s.loginLimit = newLoginLimiter() })
	return s.loginLimit
}

// connectLimiter returns the lazily created connect-endpoint limiter.
func (s *Server) connectLimiter() *loginLimiter {
	s.connectOnce.Do(func() { s.connectLimit = newLoginLimiter() })
	return s.connectLimit
}

// ---------------------------------------------------------------- plumbing

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg}})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// maxPassBytes is bcrypt's hard input limit; longer input errors out in the
// hash function, so reject it as plain input validation instead.
const maxPassBytes = 72

// Auth wraps a handler requiring a valid dashboard JWT.
func (s *Server) Auth(next func(w http.ResponseWriter, r *http.Request, c *auth.Claims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.claimsFrom(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		// Re-check the live row: disabled accounts lose access immediately and
		// role changes take effect without waiting for the JWT to expire.
		user, err := store.UserByID(s.DB, c.UserID)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnauthorized, "account unavailable")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if user.Disabled {
			writeErr(w, http.StatusForbidden, "account disabled")
			return
		}
		// Tokens issued before the last password change are dead: rotating a
		// password must revoke sessions a thief may already hold.
		if user.PassChangedAt != "" && c.IssuedAt != nil {
			if changed, perr := time.Parse(time.RFC3339, user.PassChangedAt); perr == nil && c.IssuedAt.Before(changed) {
				writeErr(w, http.StatusUnauthorized, "session expired, sign in again")
				return
			}
		}
		c.Role = user.Role
		next(w, r, c)
	}
}

// Admin wraps a handler requiring an admin JWT.
func (s *Server) Admin(next func(w http.ResponseWriter, r *http.Request, c *auth.Claims)) http.HandlerFunc {
	return s.Auth(func(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
		if c.Role != "admin" {
			writeErr(w, http.StatusForbidden, "admin access required")
			return
		}
		next(w, r, c)
	})
}

func (s *Server) claimsFrom(r *http.Request) (*auth.Claims, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return nil, errors.New("missing token")
	}
	return auth.ParseToken(s.JWTSecret, tok)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// dummyHash is a fixed bcrypt hash of a random string, used to keep Login
// timing uniform when the username does not exist.
var dummyHash = "$2a$10$92IXUNpkjO0rOQ5byMi.Ye4oKoEa3Ro9llC/.og/at2.uheWG/igi"

// ---------------------------------------------------------------- auth

func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	user, hash, err := store.UserByName(s.DB, in.Username)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			// A DB failure is not a bad password: collapsing it into 401
			// hides an outage behind "invalid credentials" and leaves no
			// trace in the log.
			log.Printf("api: login user lookup failed (%q): %v", in.Username, err)
			writeErr(w, http.StatusInternalServerError, "login unavailable")
			return
		}
		// Constant-ish time: still run a bcrypt compare so a missing username
		// cannot be distinguished by response timing.
		_ = auth.CheckPassword(dummyHash, in.Password)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if !auth.CheckPassword(hash, in.Password) {
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if user.Disabled {
		writeErr(w, http.StatusForbidden, "account disabled")
		return
	}
	tok, err := auth.SignToken(s.JWTSecret, user.ID, user.Role, s.JWTTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token signing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "user": userView(user)})
}

// userView is the user-facing shape of a user: price_multiplier is admin-only
// and must never reach the user's own client (login, /api/me, PATCH /api/me).
func userView(u *store.User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "display_name": u.DisplayName,
		"role": u.Role, "disabled": u.Disabled, "created_at": u.CreatedAt,
	}
}

func (s *Server) Me(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	user, err := store.UserByID(s.DB, c.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, userView(user))
}

// currencySetting returns the configured currency, or the documented EUR
// default when unset. ok=false means the store failed (caller: 500).
func (s *Server) currencySetting() (string, bool) {
	cur, err := store.GetSetting(s.DB, "currency")
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && cur == "":
		return "EUR", true
	case err != nil:
		return "", false
	}
	return cur, true
}

// Info exposes the public endpoint URLs (for copy buttons in the UI) and the
// install currency.
func (s *Server) Info(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	currency, ok := s.currencySetting()
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := map[string]string{
		"relay_url":     s.RelayURL,
		"dashboard_url": s.DashboardURL,
		"currency":      currency,
	}
	// The instance cert fingerprint, when one is configured (plan
	// 2026-11-24-remote-tls-v1). Public-safe: a fingerprint is a hash of a
	// public certificate, and the SetupGuide must show it to any user
	// connecting a spoke to a self-signed hub (they need it as hub_pin).
	// Absent only when no cert exists (a CA-cert / proxied hub); a real
	// lookup failure is logged, not silently rendered as "no cert" (which
	// would tell a spoke admin to skip hub_pin on a transient DB error).
	switch t, err := store.RemoteTLSGet(s.DB); {
	case err == nil:
		out["tls_fingerprint"] = t.Fingerprint
	case errors.Is(err, store.ErrNotFound):
		// no certificate configured — omit the key
	default:
		log.Printf("api: info TLS fingerprint lookup failed: %v", err)
	}
	writeJSON(w, http.StatusOK, out)
}

// PublicModels serves GET /api/models: the catalog every user may call —
// the same entries the proxy advertises on /v1/models (enabled groups plus
// enabled public models, store.ListPublicCatalog) — with the prices models
// are billed at. Internal wiring (backend, upstream id) stays hidden, and
// internal group members never appear: the group row stands for the pool,
// so its per-member fields are zero (the frontend renders them as "—").
func (s *Server) PublicModels(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	entries, err := store.ListPublicCatalog(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	type pubModel struct {
		Kind             string  `json:"kind"` // "model" | "group"
		Name             string  `json:"name"`
		PriceIn          int64   `json:"price_in"`
		PriceOut         int64   `json:"price_out"`
		PriceCacheRead   int64   `json:"price_cache_read"`
		PriceCacheWrite  int64   `json:"price_cache_write"`
		ContextLength    int64   `json:"context_length"`
		ConcurrencyLimit int64   `json:"concurrency_limit"`
		BenchTokPerS     float64 `json:"bench_tok_per_s"`
		BenchPPTokPerS   float64 `json:"bench_pp_tok_per_s"`
		BenchAt          string  `json:"bench_at"`
	}
	out := make([]pubModel, 0, len(entries))
	for _, en := range entries {
		if en.Kind == store.TargetGroup {
			out = append(out, pubModel{Kind: "group", Name: en.Name})
			continue
		}
		m := en.Model
		out = append(out, pubModel{
			Kind: "model", Name: m.Name,
			PriceIn: m.PriceIn, PriceOut: m.PriceOut,
			PriceCacheRead: m.PriceCacheRead, PriceCacheWrite: m.PriceCacheWrite,
			ContextLength: m.ContextLength, ConcurrencyLimit: m.ConcurrencyLimit,
			BenchTokPerS: m.BenchTokPerS, BenchPPTokPerS: m.BenchPPTokPerS, BenchAt: m.BenchAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// UpdateMe lets a user edit their own display name.
func (s *Server) UpdateMe(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		DisplayName *string `json:"display_name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.DisplayName == nil {
		writeErr(w, http.StatusBadRequest, "display_name required")
		return
	}
	name := strings.TrimSpace(*in.DisplayName)
	if len(name) > 64 {
		writeErr(w, http.StatusBadRequest, "display name too long (max 64)")
		return
	}
	user, err := store.UpdateUser(s.DB, c.UserID, nil, nil, nil, &name, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, userView(user))
}

// ChangePassword verifies the current password before setting a new one.
func (s *Server) ChangePassword(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(in.NewPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if len(in.NewPassword) > maxPassBytes {
		writeErr(w, http.StatusBadRequest, "new password must be at most 72 bytes")
		return
	}
	hash, err := store.PasswordHashByUserID(s.DB, c.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !auth.CheckPassword(hash, in.OldPassword) {
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	newHash, err := auth.HashPassword(in.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := store.UpdateUser(s.DB, c.UserID, &newHash, nil, nil, nil, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, "password change failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- keys

func (s *Server) ListKeys(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	keys, err := store.ListKeys(s.DB, c.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) CreateKey(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "key name required")
		return
	}
	plain, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	k, err := store.CreateKey(s.DB, c.UserID, strings.TrimSpace(in.Name), hash, prefix)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "key create failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "name": k.Name, "key": plain, "prefix": k.Prefix,
	})
}

func (s *Server) RevokeKey(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	err = store.RevokeKey(s.DB, id, c.UserID)
	switch {
	case err == store.ErrNotFound:
		writeErr(w, http.StatusNotFound, "key not found")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "key revoke failed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------------------------------------------------------------- usage

// maxPage bounds the page parameter so (page-1)*page_size cannot overflow
// the int64 OFFSET (page_size is clamped to 500 in the store).
const maxPage = 10_000_000

func usageFilter(r *http.Request, scopeUserID int64) (*store.UsageFilter, error) {
	// scopeUserID == 0 is the admin all-users view (see usageScope): say so
	// explicitly, the store refuses a silent zero scope.
	f := &store.UsageFilter{UserID: scopeUserID, AllUsers: scopeUserID == 0}
	// tz_offset: the viewer's UTC offset in minutes, east-positive (the
	// dashboard sends it so day buckets and the default range follow the
	// viewer's clock). Absent = 0 = UTC, the historical behaviour.
	if v := r.URL.Query().Get("tz_offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < -1440 || n > 1440 {
			return nil, errors.New("tz_offset must be integer minutes east of UTC (-1440..1440)")
		}
		f.TZOffset = n
	}
	// Contract default: last 30 days when a bound is omitted. With an
	// offset the anchor is the viewer's local now, so "today" is theirs.
	now := time.Now().UTC().Add(time.Duration(f.TZOffset) * time.Minute)
	if v := r.URL.Query().Get("from"); v != "" {
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return nil, errors.New("from must be YYYY-MM-DD")
		}
		f.From = v
	} else {
		f.From = now.AddDate(0, 0, -29).Format("2006-01-02")
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return nil, errors.New("to must be YYYY-MM-DD")
		}
		f.To = v
	} else {
		f.To = now.Format("2006-01-02")
	}
	if f.From > f.To {
		return nil, errors.New("from must not be after to")
	}
	if v := r.URL.Query().Get("key_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, errors.New("bad key_id")
		}
		f.KeyID = id
	}
	f.Model = r.URL.Query().Get("model")
	return f, nil
}

// usageScope returns the user_id scope for a usage query: 0 means all users.
// The all-users view is a property of the endpoint, not of the role: only the
// /api/admin/* routes are unscoped for admins; the personal routes always show
// the caller their own rows, even when the caller is an admin.
func usageScope(r *http.Request, c *auth.Claims) int64 {
	if c.Role == "admin" && strings.HasPrefix(r.URL.Path, "/api/admin/") {
		return 0
	}
	return c.UserID
}

// UsageSummary serves /api/usage/summary and the admin variant: the admin
// endpoint is unscoped with an optional user_id filter; the personal endpoint
// always scopes to the caller, admins included.
func (s *Server) UsageSummary(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	scope := usageScope(r, c)
	f, err := usageFilter(r, scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if scope == 0 {
		if v := r.URL.Query().Get("user_id"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad user_id")
				return
			}
			f.UserID = id
		}
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "day"
	}
	if !store.ValidGroupBy(groupBy) {
		writeErr(w, http.StatusBadRequest, "bad group_by")
		return
	}
	totals, groups, err := store.SummarizeUsage(s.DB, f, groupBy)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	currency, ok := s.currencySetting()
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"currency": currency, "totals": totals, "groups": groups,
	})
}

func (s *Server) UsageDetail(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	scope := usageScope(r, c)
	f, err := usageFilter(r, scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if scope == 0 {
		if v := r.URL.Query().Get("user_id"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad user_id")
				return
			}
			f.UserID = id
		}
	}
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil && r.URL.Query().Get("page") != "" {
		writeErr(w, http.StatusBadRequest, "bad page")
		return
	}
	if page > maxPage {
		writeErr(w, http.StatusBadRequest, "page out of range")
		return
	}
	size, err := strconv.Atoi(r.URL.Query().Get("page_size"))
	if err != nil && r.URL.Query().Get("page_size") != "" {
		writeErr(w, http.StatusBadRequest, "bad page_size")
		return
	}
	total, rows, err := store.UsageDetail(s.DB, f, page, size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if scope != 0 {
		// model_upstream is internal wiring (the engine-side model id, which
		// tells group members apart) and fed_peer names another user's
		// hosting peer: personal views must not learn either, so they are
		// zeroed here rather than filtered in the frontend.
		for i := range rows {
			rows[i].ModelUpstream = nil
			rows[i].FedPeer = nil
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": rows})
}
