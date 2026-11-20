// Package api implements the JWT-authenticated dashboard JSON API.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
)

type Server struct {
	DB           *sql.DB
	JWTSecret    string
	JWTTTL       time.Duration
	RelayURL     string
	DashboardURL string
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
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "account unavailable")
			return
		}
		if user.Disabled {
			writeErr(w, http.StatusForbidden, "account disabled")
			return
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
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "user": user})
}

func (s *Server) Me(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	user, err := store.UserByID(s.DB, c.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// Info exposes the public endpoint URLs (for copy buttons in the UI) and the
// install currency.
func (s *Server) Info(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	currency, _ := store.GetSetting(s.DB, "currency")
	writeJSON(w, http.StatusOK, map[string]string{
		"relay_url":     s.RelayURL,
		"dashboard_url": s.DashboardURL,
		"currency":      currency,
	})
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
	user, err := store.UpdateUser(s.DB, c.UserID, nil, nil, nil, &name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, user)
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
	if _, err := store.UpdateUser(s.DB, c.UserID, &newHash, nil, nil, nil); err != nil {
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

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func usageFilter(r *http.Request, scopeUserID int64) (*store.UsageFilter, error) {
	f := &store.UsageFilter{UserID: scopeUserID}
	// Contract default: last 30 days when a bound is omitted.
	if v := r.URL.Query().Get("from"); v != "" {
		if !dateRe.MatchString(v) {
			return nil, errors.New("from must be YYYY-MM-DD")
		}
		f.From = v
	} else {
		f.From = time.Now().UTC().AddDate(0, 0, -29).Format("2006-01-02")
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if !dateRe.MatchString(v) {
			return nil, errors.New("to must be YYYY-MM-DD")
		}
		f.To = v
	} else {
		f.To = time.Now().UTC().Format("2006-01-02")
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

// UsageSummary serves /api/usage/summary and the admin variant: admins get
// an optional user_id filter, regular users are always scoped to themselves.
func (s *Server) UsageSummary(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	scope := c.UserID
	if c.Role == "admin" {
		scope = 0
	}
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
	totals, groups, err := store.SummarizeUsage(s.DB, f, groupBy)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	currency, _ := store.GetSetting(s.DB, "currency")
	writeJSON(w, http.StatusOK, map[string]any{
		"currency": currency, "totals": totals, "groups": groups,
	})
}

func (s *Server) UsageDetail(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	scope := c.UserID
	if c.Role == "admin" {
		scope = 0
	}
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
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	total, rows, err := store.UsageDetail(s.DB, f, page, size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": rows})
}
