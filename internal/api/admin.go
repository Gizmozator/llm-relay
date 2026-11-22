// Admin API: users, backends + discovery, model catalog, settings.
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
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

// backendView is a store.Backend plus the live reachability verdict from the
// shared health cache (re-probed at most once per TTL). The stored
// last_probe_* fields only change when the admin presses Probe, so the page
// must not rely on them for "is it up right now".
type backendView struct {
	store.Backend
	Online    bool   `json:"online"`
	HealthErr string `json:"health_error,omitempty"`
}

func (s *Server) AdminListBackends(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	backends, err := store.ListBackends(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := s.healthStatus().Status(r.Context(), backends)
	views := make([]backendView, 0, len(backends))
	for _, b := range backends {
		v := backendView{Backend: b, Online: status[b.ID].Online}
		if h := status[b.ID]; !h.Online {
			v.HealthErr = h.Err
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": views})
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
		Name     *string `json:"name"`
		BaseURL  *string `json:"base_url"`
		APIKey   *string `json:"api_key"`
		Enabled  *bool   `json:"enabled"`
		Draining *bool   `json:"draining"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.BaseURL != nil && !urlRe.MatchString(*in.BaseURL) {
		writeErr(w, http.StatusBadRequest, "base_url must start with http:// or https://")
		return
	}
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" {
			writeErr(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		in.Name = &trimmed
	}
	b, err := store.UpdateBackend(s.DB, id, in.Name, in.BaseURL, in.APIKey, in.Enabled, in.Draining)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "backend not found")
		return
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "backend name taken")
		return
	case err != nil:
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
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "backend not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
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
		inCatalog, err := store.ModelIDsForBackend(s.DB, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
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
	_, err = store.BackendByID(s.DB, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "backend not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	var in struct {
		ModelIDs []string `json:"model_ids"`
	}
	if err := decode(r, &in); err != nil || len(in.ModelIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "model_ids required")
		return
	}
	if len(in.ModelIDs) > 200 {
		writeErr(w, http.StatusBadRequest, "too many model_ids in one batch (max 200)")
		return
	}
	created, skipped, err := store.AddModels(s.DB, id, in.ModelIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "model create failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": created, "skipped": skipped})
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
		Name             *string  `json:"name"`
		Enabled          *bool    `json:"enabled"`
		PriceIn          *int64   `json:"price_in"`
		PriceOut         *int64   `json:"price_out"`
		PriceCacheRead   *int64   `json:"price_cache_read"`
		PriceCacheWrite  *int64   `json:"price_cache_write"`
		ContextLength    *int64   `json:"context_length"`
		ConcurrencyLimit *int64   `json:"concurrency_limit"`
		BusyThresholdPct *int64   `json:"busy_threshold_pct"`
		BenchTokPerS     *float64 `json:"bench_tok_per_s"`
		BenchPPTokPerS   *float64 `json:"bench_pp_tok_per_s"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" {
			writeErr(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		in.Name = &trimmed
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
	// Over 100 is meaningful: utilization stacks with parallel streams,
	// so e.g. 300 means "busy from 3 concurrent generations".
	if in.BusyThresholdPct != nil && (*in.BusyThresholdPct < 0 || *in.BusyThresholdPct > 10000) {
		writeErr(w, http.StatusBadRequest, "busy_threshold_pct must be 0-10000 (0 = default 80)")
		return
	}
	if in.BenchTokPerS != nil && *in.BenchTokPerS < 0 {
		writeErr(w, http.StatusBadRequest, "bench_tok_per_s cannot be negative")
		return
	}
	if in.BenchPPTokPerS != nil && *in.BenchPPTokPerS < 0 {
		writeErr(w, http.StatusBadRequest, "bench_pp_tok_per_s cannot be negative")
		return
	}
	m, err := store.UpdateModel(s.DB, id, in.Name, in.Enabled,
		[4]*int64{in.PriceIn, in.PriceOut, in.PriceCacheRead, in.PriceCacheWrite})
	if err == nil && (in.ContextLength != nil || in.ConcurrencyLimit != nil ||
		in.BusyThresholdPct != nil || in.BenchTokPerS != nil || in.BenchPPTokPerS != nil) {
		if err = store.UpdateModelMeta(s.DB, id, in.ContextLength, in.ConcurrencyLimit,
			in.BusyThresholdPct, in.BenchTokPerS, in.BenchPPTokPerS); err == nil {
			m, err = store.ModelByID(s.DB, id)
		}
	}
	switch {
	case err == store.ErrNotFound:
		writeErr(w, http.StatusNotFound, "model not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "model name taken")
	case errors.Is(err, store.ErrExists):
		// Rename onto a group-owned name (assertNoGroupName). Fixed
		// string: the store error text is an internal detail.
		writeErr(w, http.StatusConflict, "model name already in use")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "model update failed")
	default:
		writeJSON(w, http.StatusOK, m)
	}
}

// AdminBenchmarkModel runs two timed streaming calls against the model's
// upstream: a decode pass (natural prose prompt, first→last token → TG
// tok/s) so speculative/MTP decoding is measured at realistic acceptance
// rates, and a prefill pass (~10k-token randomized prompt, max_tokens=1,
// request→first token → PP tok/s). Deliberately serialized: parallel
// benchmarks would skew each other's tokens/s numbers.
var benchMu sync.Mutex

// benchVocab feeds the prefill-pass filler prompt: ordinary prose words,
// because only the token count and a cache-missing order matter.
const benchVocab = `the harbor light folded over water while morning carts carried flour
timber and letters across cobbled streets; children counted gulls, sailors mended nets,
and an old clock keeper wrote weather into a ledger nobody would read. Somewhere beyond
the breakwater a quiet engine turned, and the day began again with bread, salt, rope,
rain and the patient sound of stone under footsteps. Long evenings gathered neighbors
around warm windows, where stories of distant railways crossed the table with tea.`

// benchFillerWords builds ~tokens worth of randomized filler prose
// (≈3 words per 4 tokens). Randomized per run so the engine's prefix cache
// misses: a repeated prompt would measure cache reads, not prefill.
func benchFillerWords(tokens int64) string {
	words := strings.Fields(benchVocab)
	var b strings.Builder
	prev := -1
	for i := int64(0); i < max(tokens*3/4, 16); i++ {
		j := rand.Intn(len(words))
		for j == prev {
			j = rand.Intn(len(words))
		}
		prev = j
		switch {
		case i == 0:
		case i%12 == 0:
			b.WriteString(". ")
		default:
			b.WriteByte(' ')
		}
		b.WriteString(words[j])
	}
	b.WriteByte('.')
	return b.String()
}

// benchRun is one timed streaming exchange with the upstream: start is when
// the request was sent; firstChunk/firstTok/lastTok mark the first chunk
// with choices, and the first and last non-empty content delta; prompt and
// completion token counts come from the trailing usage chunk.
type benchRun struct {
	start                         time.Time
	firstChunk, firstTok, lastTok time.Time
	prompt, completion            int64
}

// benchStreamRun POSTs one streaming chat completion (the caller's body must
// set stream and stream_options.include_usage) and times its token stream.
func benchStreamRun(ctx context.Context, b *store.Backend, body map[string]any) (*benchRun, error) {
	buf, _ := json.Marshal(body)
	r := &benchRun{start: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, errors.New("internal error")
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.New("upstream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			r.prompt = max(r.prompt, chunk.Usage.PromptTokens)
			r.completion = max(r.completion, chunk.Usage.CompletionTokens)
		}
		if len(chunk.Choices) > 0 {
			now := time.Now()
			if r.firstChunk.IsZero() {
				r.firstChunk = now
			}
			if chunk.Choices[0].Delta.Content != "" {
				if r.firstTok.IsZero() {
					r.firstTok = now
				}
				r.lastTok = now
			}
		}
	}
	if sc.Err() != nil {
		return nil, errors.New("upstream stream failed")
	}
	return r, nil
}

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

	// Decode pass (TG). Natural-prose prompt on purpose: trivially
	// predictable text (counting, repetition) lets speculative/MTP decoding
	// accept nearly every draft token, inflating the measurement well past
	// real-traffic speed. The request streams so the timer covers only
	// first-token → last-token (decode time), not prefill.
	tg, err := benchStreamRun(ctx, b, map[string]any{
		"model": m.UpstreamModel,
		"messages": []map[string]string{
			{"role": "user", "content": "Write a detailed, original essay about how harbor cities changed when steam shipping replaced sailing ships. Discuss trade, migration, shipyard work and daily life, with concrete examples. Write in flowing prose without lists."},
		},
		"max_tokens":     700,
		"temperature":    0,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if tg.completion <= 0 || tg.firstTok.IsZero() || !tg.lastTok.After(tg.firstTok) {
		writeErr(w, http.StatusBadGateway, "upstream returned no usage or too few streamed tokens; cannot benchmark")
		return
	}
	// first→last spans completion-1 token intervals.
	n := tg.completion - 1
	if n < 1 {
		n = 1
	}
	tgSecs := tg.lastTok.Sub(tg.firstTok).Seconds()
	tokPerS := float64(n) / tgSecs

	// Prefill pass (PP): ~10k-token filler prompt, one generated token.
	// Prefill completes before the first token is emitted, so request →
	// first token is the prefill time. The prompt shrinks to ¾ of the
	// model's context when that is smaller, so small-context upstreams do
	// not reject it. A failure here never voids the decode result: TG is
	// stored and the old PP value is kept.
	target := int64(10000)
	if m.ContextLength > 0 && m.ContextLength < target*4/3 {
		target = m.ContextLength * 3 / 4
	}
	pp, ppErr := benchStreamRun(ctx, b, map[string]any{
		"model":          m.UpstreamModel,
		"messages":       []map[string]string{{"role": "user", "content": benchFillerWords(target)}},
		"max_tokens":     1,
		"temperature":    0,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	var ppTokPerS *float64
	var ppSecs float64
	ppErrMsg := ""
	switch {
	case ppErr != nil:
		ppErrMsg = ppErr.Error()
	case pp.prompt <= 0:
		ppErrMsg = "prefill pass returned no prompt token count"
	default:
		// The first content delta is the true first token; an engine that
		// streams no content field at all has still finished prefill once
		// any choices chunk arrived, so fall back to that.
		first := pp.firstTok
		if first.IsZero() {
			first = pp.firstChunk
		}
		if first.IsZero() || !first.After(pp.start) {
			ppErrMsg = "prefill pass streamed no token"
		} else {
			ppSecs = first.Sub(pp.start).Seconds()
			v := float64(pp.prompt) / ppSecs
			ppTokPerS = &v
		}
	}

	at := time.Now().UTC().Format(time.RFC3339)
	if err := store.UpdateModelBench(s.DB, id, tokPerS, ppTokPerS, at); err != nil {
		writeErr(w, http.StatusInternalServerError, "benchmark save failed")
		return
	}
	res := map[string]any{
		"bench_tok_per_s":   tokPerS,
		"completion_tokens": tg.completion,
		"seconds":           tgSecs,
		"bench_at":          at,
	}
	if ppTokPerS != nil {
		res["bench_pp_tok_per_s"] = *ppTokPerS
		res["prompt_tokens"] = pp.prompt
		res["pp_seconds"] = ppSecs
	} else {
		res["pp_error"] = ppErrMsg
	}
	writeJSON(w, http.StatusOK, res)
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
	if errors.Is(err, store.ErrNotFound) {
		cur = "EUR" // unset: the documented default
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
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
