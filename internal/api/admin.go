// Admin API: users, backends + discovery, model catalog, settings.
package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"llm-relay/internal/auth"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// ---------------------------------------------------------------- users

func (s *Server) AdminListUsers(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	users, err := store.ListUsers(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) AdminCreateUser(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "username required; password must be at least 8 characters")
		return
	}
	if in.Role != "admin" && in.Role != "user" {
		in.Role = "user"
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	user, err := store.CreateUser(s.DB, in.Username, hash, in.Role)
	if err == store.ErrExists {
		writeErr(w, http.StatusConflict, "username taken")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user create failed")
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) AdminUpdateUser(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Password *string `json:"password"`
		Disabled *bool   `json:"disabled"`
		Role     *string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// An admin must not lock themselves out.
	if id == c.UserID && ((in.Disabled != nil && *in.Disabled) || (in.Role != nil && *in.Role != "admin")) {
		writeErr(w, http.StatusBadRequest, "you cannot disable or demote your own account")
		return
	}
	var hash *string
	if in.Password != nil {
		if len(*in.Password) < 8 {
			writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}
		h, err := auth.HashPassword(*in.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		hash = &h
	}
	if in.Role != nil && *in.Role != "admin" && *in.Role != "user" {
		writeErr(w, http.StatusBadRequest, "role must be admin or user")
		return
	}
	user, err := store.UpdateUser(s.DB, id, hash, in.Disabled, in.Role)
	if err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user update failed")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// ---------------------------------------------------------------- backends

var urlRe = regexp.MustCompile(`^https?://[^ ]+$`)

func (s *Server) AdminListBackends(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	backends, err := store.ListBackends(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": backends})
}

func (s *Server) AdminCreateBackend(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	if in.Name == "" || !urlRe.MatchString(in.BaseURL) {
		writeErr(w, http.StatusBadRequest, "name required and base_url must start with http:// or https://")
		return
	}
	b, err := store.CreateBackend(s.DB, in.Name, in.BaseURL, in.APIKey)
	if err == store.ErrExists {
		writeErr(w, http.StatusConflict, "backend name taken")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "backend create failed")
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) AdminUpdateBackend(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Name    *string `json:"name"`
		BaseURL *string `json:"base_url"`
		APIKey  *string `json:"api_key"`
		Enabled *bool   `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.BaseURL != nil && !urlRe.MatchString(*in.BaseURL) {
		writeErr(w, http.StatusBadRequest, "base_url must start with http:// or https://")
		return
	}
	b, err := store.UpdateBackend(s.DB, id, in.Name, in.BaseURL, in.APIKey, in.Enabled)
	if err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "backend not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "backend update failed")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) AdminDeleteBackend(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	err = store.DeleteBackend(s.DB, id)
	switch {
	case err == store.ErrNotFound:
		writeErr(w, http.StatusNotFound, "backend not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "backend delete failed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// AdminProbeBackend probes the backend and returns discovered models diffed
// against the catalog. Discovery proposes; nothing is auto-added.
func (s *Server) AdminProbeBackend(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	b, err := store.BackendByID(s.DB, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "backend not found")
		return
	}
	res := proxy.Probe(r.Context(), b.BaseURL, b.APIKey)

	var probeErr, version *string
	if !res.OK {
		probeErr = &res.Error
	}
	if res.Version != "" {
		version = &res.Version
	}
	_ = store.RecordProbe(s.DB, id, probeErr, version)

	type disc struct {
		ID        string `json:"id"`
		InCatalog bool   `json:"in_catalog"`
		ModelID   *int64 `json:"model_id"`
	}
	out := struct {
		OK      bool   `json:"ok"`
		Version string `json:"server_version"`
		Error   string `json:"error"`
		Models  []disc `json:"models"`
	}{OK: res.OK, Version: res.Version, Error: res.Error}

	if res.OK {
		inCatalog, _ := store.ModelIDsForBackend(s.DB, id)
		for _, mid := range res.Models {
			d := disc{ID: mid}
			if id, ok := inCatalog[mid]; ok {
				d.InCatalog = true
				d.ModelID = &id
			}
			out.Models = append(out.Models, d)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// AdminAddBackendModels bulk-adds discovered model ids to the catalog.
func (s *Server) AdminAddBackendModels(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if _, err := store.BackendByID(s.DB, id); err != nil {
		writeErr(w, http.StatusNotFound, "backend not found")
		return
	}
	var in struct {
		ModelIDs []string `json:"model_ids"`
	}
	if err := decode(r, &in); err != nil || len(in.ModelIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "model_ids required")
		return
	}
	created := []store.Model{}
	for _, mid := range in.ModelIDs {
		m, err := store.CreateModel(s.DB, id, mid, "")
		if err == store.ErrExists {
			continue
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "model create failed")
			return
		}
		created = append(created, *m)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": created})
}

// ---------------------------------------------------------------- models

func (s *Server) AdminListModels(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	models, err := store.ListModels(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) AdminUpdateModel(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Name            *string `json:"name"`
		Enabled         *bool   `json:"enabled"`
		PriceIn         *int64  `json:"price_in"`
		PriceOut        *int64  `json:"price_out"`
		PriceCacheRead  *int64  `json:"price_cache_read"`
		PriceCacheWrite *int64  `json:"price_cache_write"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name cannot be empty")
		return
	}
	for _, p := range []*int64{in.PriceIn, in.PriceOut, in.PriceCacheRead, in.PriceCacheWrite} {
		if p != nil && *p < 0 {
			writeErr(w, http.StatusBadRequest, "prices cannot be negative")
			return
		}
	}
	m, err := store.UpdateModel(s.DB, id, in.Name, in.Enabled,
		[4]*int64{in.PriceIn, in.PriceOut, in.PriceCacheRead, in.PriceCacheWrite})
	switch {
	case err == store.ErrNotFound:
		writeErr(w, http.StatusNotFound, "model not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "model name taken")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "model update failed")
	default:
		writeJSON(w, http.StatusOK, m)
	}
}

func (s *Server) AdminDeleteModel(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := store.DeleteModel(s.DB, id); err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "model not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "model delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- settings

var currencyRe = regexp.MustCompile(`^[A-Za-z]{2,5}$`)

func (s *Server) AdminGetSettings(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	cur, err := store.GetSetting(s.DB, "currency")
	if err != nil {
		cur = "EUR"
	}
	writeJSON(w, http.StatusOK, map[string]string{"currency": cur})
}

func (s *Server) AdminPutSettings(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		Currency string `json:"currency"`
	}
	if err := decode(r, &in); err != nil || !currencyRe.MatchString(in.Currency) {
		writeErr(w, http.StatusBadRequest, "currency must be a 2-5 letter code")
		return
	}
	if err := store.SetSetting(s.DB, "currency", strings.ToUpper(in.Currency)); err != nil {
		writeErr(w, http.StatusInternalServerError, "settings update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"currency": strings.ToUpper(in.Currency)})
}
