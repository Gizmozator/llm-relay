// Admin API: users, backends + discovery, model catalog, settings.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

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
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		Role        string `json:"role"`
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
	if len(in.Password) > maxPassBytes {
		writeErr(w, http.StatusBadRequest, "password must be at most 72 bytes")
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
	user, err := store.CreateUser(s.DB, in.Username, strings.TrimSpace(in.DisplayName), hash, in.Role)
	if err == store.ErrExists {
		writeErr(w, http.StatusConflict, "username taken")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user create failed")
		return
	}
	// Re-read so the response carries the DB defaults (price_multiplier=1).
	user, err = store.UserByID(s.DB, user.ID)
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
		Password        *string  `json:"password"`
		Disabled        *bool    `json:"disabled"`
		Role            *string  `json:"role"`
		DisplayName     *string  `json:"display_name"`
		PriceMultiplier *float64 `json:"price_multiplier"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.PriceMultiplier != nil && (*in.PriceMultiplier <= 0 || *in.PriceMultiplier > 1000) {
		writeErr(w, http.StatusBadRequest, "price_multiplier must be greater than 0 and at most 1000")
		return
	}
	// An admin must not lock themselves out.
	if id == c.UserID && ((in.Disabled != nil && *in.Disabled) || (in.Role != nil && *in.Role != "admin")) {
		writeErr(w, http.StatusBadRequest, "you cannot disable or demote your own account")
		return
	}
	// Nor may any edit leave the system with zero enabled admins: that locks
	// every /api/admin/* route and is unrecoverable without DB surgery.
	if (in.Disabled != nil && *in.Disabled) || (in.Role != nil && *in.Role != "admin") {
		target, err := store.UserByID(s.DB, id)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if target.Role == "admin" && !target.Disabled {
			others, err := store.CountEnabledAdminsExcept(s.DB, id)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			if others == 0 {
				writeErr(w, http.StatusBadRequest, "this is the last enabled admin account")
				return
			}
		}
	}
	var hash *string
	if in.Password != nil {
		if len(*in.Password) < 8 {
			writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}
		if len(*in.Password) > maxPassBytes {
			writeErr(w, http.StatusBadRequest, "password must be at most 72 bytes")
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
	user, err := store.UpdateUser(s.DB, id, hash, in.Disabled, in.Role, in.DisplayName, in.PriceMultiplier)
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
		Name             *string `json:"name"`
		Enabled          *bool   `json:"enabled"`
		PriceIn          *int64  `json:"price_in"`
		PriceOut         *int64  `json:"price_out"`
		PriceCacheRead   *int64  `json:"price_cache_read"`
		PriceCacheWrite  *int64  `json:"price_cache_write"`
		ContextLength    *int64  `json:"context_length"`
		ConcurrencyLimit *int64  `json:"concurrency_limit"`
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
	for _, v := range []*int64{in.ContextLength, in.ConcurrencyLimit} {
		if v != nil && *v < 0 {
			writeErr(w, http.StatusBadRequest, "values cannot be negative")
			return
		}
	}
	m, err := store.UpdateModel(s.DB, id, in.Name, in.Enabled,
		[4]*int64{in.PriceIn, in.PriceOut, in.PriceCacheRead, in.PriceCacheWrite})
	if err == nil && (in.ContextLength != nil || in.ConcurrencyLimit != nil) {
		if err = store.UpdateModelMeta(s.DB, id, in.ContextLength, in.ConcurrencyLimit); err == nil {
			m, err = store.ModelByID(s.DB, id)
		}
	}
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

// AdminBenchmarkModel runs a fixed generation against the model's upstream
// and stores the measured completion speed. Deliberately serialized: parallel
// benchmarks would skew each other's tokens/s numbers.
var benchMu sync.Mutex

func (s *Server) AdminBenchmarkModel(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	m, err := store.ModelByID(s.DB, id)
	if err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "model not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	b, err := store.BackendByID(s.DB, m.BackendID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !benchMu.TryLock() {
		writeErr(w, http.StatusConflict, "a benchmark is already running")
		return
	}
	defer benchMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	buf, _ := json.Marshal(map[string]any{
		"model": m.UpstreamModel,
		"messages": []map[string]string{
			{"role": "user", "content": "Count from 1 to 400, one number per line. No commentary."},
		},
		"max_tokens":  700,
		"temperature": 0,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode))
		return
	}
	var parsed struct {
		Usage struct {
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Usage.CompletionTokens <= 0 {
		writeErr(w, http.StatusBadGateway, "upstream returned no usage; cannot benchmark")
		return
	}
	secs := time.Since(start).Seconds()
	tokPerS := float64(parsed.Usage.CompletionTokens) / secs
	at := time.Now().UTC().Format(time.RFC3339)
	if err := store.UpdateModelBench(s.DB, id, tokPerS, at); err != nil {
		writeErr(w, http.StatusInternalServerError, "benchmark save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bench_tok_per_s":   tokPerS,
		"completion_tokens": parsed.Usage.CompletionTokens,
		"seconds":           secs,
		"bench_at":          at,
	})
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
