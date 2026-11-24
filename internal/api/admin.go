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
	// The "never zero enabled admins" rule lives inside store.UpdateUser's
	// transaction: counting here would race a concurrent demote of the only
	// other admin (both checks pass, both edits land, no admin is left).
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
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "user not found")
		return
	case errors.Is(err, store.ErrLastAdmin):
		writeErr(w, http.StatusBadRequest, "this is the last enabled admin account")
		return
	case err != nil:
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
	if in.BaseURL != nil {
		// A remote backend's base_url (remote://<id>) is the registry key —
		// *changing* it would detach the live channel from its row. But
		// round-tripping the whole backend object (echoing base_url
		// unchanged while editing name/enabled) is a common admin-client
		// pattern and must keep working: only a real change is refused, an
		// identical echo is dropped so the urlRe check below — which would
		// reject remote:// — never sees it.
		cur, err := store.BackendByID(s.DB, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "backend not found")
			return
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if _, ok := store.RemoteBackendID(cur.BaseURL); ok {
			if *in.BaseURL != cur.BaseURL {
				writeErr(w, http.StatusBadRequest,
					"remote backends are addressed by their connect token, not a base_url")
				return
			}
			in.BaseURL = nil
		}
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
	// Enabled state gates what the catalog shares (both roles push).
	s.catalogChanged()
	// Disabling a remote backend drops its live channel now: the token is
	// dead the moment the row says so, and a connector generating on a
	// disabled backend would only be refused downstream.
	if s.Remote != nil && in.Enabled != nil && !*in.Enabled {
		if _, ok := store.RemoteBackendID(b.BaseURL); ok {
			s.Remote.Kick(id, "backend disabled")
		}
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
		if s.Remote != nil {
			s.Remote.Kick(id, "backend deleted")
		}
		s.catalogChanged()
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
	if _, _, syn := store.SyntheticBaseURL(b.BaseURL); syn {
		// peer://hub:// backends have no HTTP endpoint: their health IS the
		// federation channel, and their model list arrives via catalog
		// frames. Probing one would dial a fake hostname.
		writeErr(w, http.StatusBadRequest, "federation backends are served over the peer channel, not HTTP")
		return
	}
	var res proxy.ProbeResult
	if _, ok := store.RemoteBackendID(b.BaseURL); ok {
		// remote:// backends have no dialable endpoint either — the probe
		// rides the connector's live channel (GET /health + /v1/models
		// frames answered by the box against its engine).
		res = s.probeRemote(r, b)
	} else {
		res = proxy.Probe(r.Context(), b.BaseURL, b.APIKey)
	}

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
	s.catalogChanged()
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
		Visibility       *string  `json:"visibility"`
		PriceIn          *int64   `json:"price_in"`
		PriceOut         *int64   `json:"price_out"`
		PriceCacheRead   *int64   `json:"price_cache_read"`
		PriceCacheWrite  *int64   `json:"price_cache_write"`
		ContextLength    *int64   `json:"context_length"`
		ConcurrencyLimit *int64   `json:"concurrency_limit"`
		BusyThresholdPct *int64   `json:"busy_threshold_pct"`
		BenchTokPerS     *float64 `json:"bench_tok_per_s"`
		BenchPPTokPerS   *float64 `json:"bench_pp_tok_per_s"`
		// Federation sharing (plan 2026-11-22-relay-federation): the
		// per-model switch + allowlist. fed_peer_ids replaces the allowlist
		// wholesale and must ride together with fed_visibility="allowlist".
		FedVisibility *string  `json:"fed_visibility"`
		FedPeerIDs    *[]int64 `json:"fed_peer_ids"`
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
	// A federated (materialized) row mirrors a peer's catalog: every field,
	// `enabled` included, is the peer's — an edit here "succeeds" and the next
	// catalog push silently overwrites the damage (the upsert re-enables a
	// disabled row), so the API refuses the whole PATCH the way the benchmark
	// guard does, before a single column is written. Even an empty body is
	// refused: with no writable field there is nothing a PATCH could legally
	// say. The durable kill switches are the peer/backend disable in the
	// Servers section; removal is per-peer there too.
	federated, err := store.ModelIsFederated(s.DB, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if federated {
		writeErr(w, http.StatusBadRequest, "cannot edit a federated model's settings - managed by the peer")
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
	if in.Visibility != nil && *in.Visibility != "public" && *in.Visibility != "internal" {
		writeErr(w, http.StatusBadRequest, "visibility must be public or internal")
		return
	}
	// Federation sharing fields validate here, with every other field: a
	// PATCH that also renames the model must not commit the rename before
	// discovering fed_visibility is bogus (validate-then-mutate).
	fedVis := ""
	if in.FedVisibility != nil {
		fedVis = *in.FedVisibility
		if fedVis != store.FedVisOff && fedVis != store.FedVisAll && fedVis != store.FedVisAllowlist {
			writeErr(w, http.StatusBadRequest, "fed_visibility must be off, all or allowlist")
			return
		}
	}
	// The allowlist only means something together with the allowlist
	// mode; requiring both in one call keeps the pair unambiguous (an
	// id-only PATCH could silently retarget a list the model no longer
	// uses).
	if in.FedPeerIDs != nil && fedVis != store.FedVisAllowlist {
		writeErr(w, http.StatusBadRequest, "fed_peer_ids requires fed_visibility=allowlist")
		return
	}
	// Unknown peer ids are a client mistake, not a 404 about the model:
	// check them here so the store's ErrNotFound stays model-only.
	if in.FedPeerIDs != nil {
		for _, pid := range *in.FedPeerIDs {
			if _, perr := store.PeerByID(s.DB, pid); errors.Is(perr, store.ErrNotFound) {
				writeErr(w, http.StatusBadRequest, "fed_peer_ids contains an unknown peer")
				return
			} else if perr != nil {
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	// One store transaction for the whole PATCH: the four independent calls
	// this replaces could commit the early fields and fail on a later one,
	// leaving a half-applied edit behind a "failed" response.
	m, err := store.UpdateModelPatch(s.DB, id, store.ModelPatch{
		Name:    in.Name,
		Enabled: in.Enabled,
		Prices:  [4]*int64{in.PriceIn, in.PriceOut, in.PriceCacheRead, in.PriceCacheWrite},
		// Manual visibility edit (group members included: re-exposing a
		// member is allowed, the resolution layer keeps the group in front
		// while it serves).
		Visibility:       in.Visibility,
		ContextLength:    in.ContextLength,
		ConcurrencyLimit: in.ConcurrencyLimit,
		BusyThresholdPct: in.BusyThresholdPct,
		BenchTokPerS:     in.BenchTokPerS,
		BenchPPTokPerS:   in.BenchPPTokPerS,
		// The fed pair was validated up front; the store keeps the PATCH
		// semantics: an unmentioned list does not change (switching to
		// allowlist keeps the stored one), switching to off/all clears it
		// on purpose.
		FedVisibility: fedVis,
		FedPeerIDs:    in.FedPeerIDs,
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "model not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "model name taken")
	case errors.Is(err, store.ErrExists):
		// Rename onto a group-owned name (assertNoGroupName). Fixed
		// string: the store error text is an internal detail.
		writeErr(w, http.StatusConflict, "model name already in use")
	case errors.Is(err, store.ErrFedConfig):
		writeErr(w, http.StatusBadRequest, "fed_visibility must be off, all or allowlist")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "model update failed")
	default:
		s.catalogChanged()
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
	// A synthetic backend is a peer's relay, not an engine (same guard the
	// health probe uses): benchmarking it would time a tunnel, not a
	// model, and burn a peer's upstream for an admin click.
	if _, _, syn := store.SyntheticBaseURL(b.BaseURL); syn {
		writeErr(w, http.StatusBadRequest, "cannot benchmark a federated model")
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
	// Bench numbers ride the federation catalog: peers advertising this
	// model must see the fresh score, not the one from before the run.
	s.catalogChanged()
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
	// Federated rows belong to the peer that shares them: deleting one here
	// only removes the local mirror until the next catalog push recreates it
	// (and it drops the row the local usage history points at). Removal is
	// per-peer on the Servers page.
	federated, err := store.ModelIsFederated(s.DB, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if federated {
		writeErr(w, http.StatusBadRequest, "cannot delete a federated model - remove the peer that shares it")
		return
	}
	if err := store.DeleteModel(s.DB, id); err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "model not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "model delete failed")
		return
	}
	s.catalogChanged()
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- groups

// groupMemberView is one member row for the admin group views: the
// membership fields plus the identity and state the editor shows next to
// them (names are only unique per backend, so the triple goes along).
type groupMemberView struct {
	ModelID        int64   `json:"model_id"`
	Weight         float64 `json:"weight"`
	Position       int     `json:"position"`
	ModelName      string  `json:"model_name"`
	BackendName    string  `json:"backend_name"`
	UpstreamModel  string  `json:"upstream_model"`
	Visibility     string  `json:"visibility"`
	ModelEnabled   bool    `json:"model_enabled"`
	BackendEnabled bool    `json:"backend_enabled"`
	Draining       bool    `json:"backend_draining"`
}

// groupView is a store.Group flattened with its members, the shape every
// group endpoint returns.
type groupView struct {
	store.Group
	Members []groupMemberView `json:"members"`
}

func groupViewOf(g *store.Group, members []store.GroupMember) groupView {
	v := groupView{Group: *g, Members: make([]groupMemberView, 0, len(members))}
	for _, m := range members {
		mv := groupMemberView{ModelID: m.ModelID, Weight: m.Weight, Position: m.Position}
		if m.Model != nil {
			mv.ModelName = m.Model.Name
			mv.UpstreamModel = m.Model.UpstreamModel
			mv.Visibility = m.Model.Visibility
			mv.ModelEnabled = m.Model.Enabled
		}
		if m.Backend != nil {
			mv.BackendName = m.Backend.Name
			mv.BackendEnabled = m.Backend.Enabled
			mv.Draining = m.Backend.Draining
		}
		v.Members = append(v.Members, mv)
	}
	return v
}

// writeGroupErr maps store group errors to the uniform taxonomy:
// ErrInvalid→400, ErrExists/ErrConflict→409, ErrNotFound→404, everything
// else 500 with fallback. Client-facing strings are fixed — raw store
// error text is an internal detail and never echoed.
func writeGroupErr(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, store.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid group name")
	case errors.Is(err, store.ErrExists):
		writeErr(w, http.StatusConflict, "group name taken")
	case errors.Is(err, store.ErrNoEligibleMember):
		writeErr(w, http.StatusConflict, "cannot enable a group with no eligible members")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "model already belongs to a group")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "group not found")
	default:
		writeErr(w, http.StatusInternalServerError, fallback)
	}
}

func (s *Server) AdminListGroups(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	groups, err := store.ListGroups(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]groupView, 0, len(groups))
	for i := range groups {
		g, members, err := store.GetGroup(s.DB, groups[i].ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Deleted between ListGroups and this fetch: it simply
				// drops off the list; failing the whole listing over one
				// vanished row would punish unrelated groups.
				continue
			}
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, groupViewOf(g, members))
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func (s *Server) AdminCreateGroup(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	g, err := store.CreateGroup(s.DB, in.Name)
	if err != nil {
		writeGroupErr(w, err, "group create failed")
		return
	}
	// CreateGroup already re-reads the row; a born-empty group provably has
	// no members, so no second fetch (GetGroup would add a query + join
	// that can only return empty).
	s.catalogChanged()
	writeJSON(w, http.StatusCreated, groupViewOf(g, nil))
}

func (s *Server) AdminGetGroup(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	g, members, err := store.GetGroup(s.DB, id)
	if err != nil {
		writeGroupErr(w, err, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, groupViewOf(g, members))
}

// AdminUpdateGroup applies {name?, enabled?, members?}; nil fields stay
// unchanged. members is a full replacement when present — an empty array
// clears the group — so it decodes as a pointer to tell omitted from empty.
// A member weight of 0/omitted seeds from that model's bench_tok_per_s
// (the best available proxy for relative capacity), falling back to 1.
func (s *Server) AdminUpdateGroup(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
		Members *[]struct {
			ModelID  int64   `json:"model_id"`
			Weight   float64 `json:"weight"`
			Position int     `json:"position"`
		} `json:"members"`
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
	// The current row is needed for the enable-transition pre-check below
	// (and gives an early 404 for an unknown id).
	cur, err := store.GroupByID(s.DB, id)
	if err != nil {
		writeGroupErr(w, err, "internal error")
		return
	}
	var members []store.GroupMemberInput
	if in.Members != nil {
		// Bound the replace-set: the 1 MiB body cap still admits tens of
		// thousands of entries, each costing a ModelByID round-trip here
		// and a placeholder in the store's NOT IN exclusion (SQLite's
		// variable limit is 32766 — an oversized payload would surface as
		// a 500 deep inside a write tx). A group pools the replicas of one
		// model; 256 is generous by orders of magnitude.
		const maxGroupMembers = 256
		if len(*in.Members) > maxGroupMembers {
			writeErr(w, http.StatusBadRequest, "too many members")
			return
		}
		seen := map[int64]bool{}
		members = make([]store.GroupMemberInput, 0, len(*in.Members))
		for _, m := range *in.Members {
			if m.ModelID <= 0 {
				writeErr(w, http.StatusBadRequest, "member model_id required")
				return
			}
			if seen[m.ModelID] {
				writeErr(w, http.StatusConflict, "model listed twice in members")
				return
			}
			seen[m.ModelID] = true
			mm, err := store.ModelByID(s.DB, m.ModelID)
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "model not found")
				return
			}
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			weight := m.Weight
			if weight <= 0 {
				weight = mm.BenchTokPerS
				if weight <= 0 {
					weight = 1
				}
			}
			members = append(members, store.GroupMemberInput{
				ModelID: m.ModelID, Weight: weight, Position: m.Position,
			})
		}
	}
	// Mirror the store's enable rule (writing enabled=1 requires at least
	// one member, regardless of the previous state) so the empty case
	// carries its own fixed string. A group with members that all fail the
	// store's stricter eligibility rule (model and backend enabled) is
	// rejected by the store's ErrNoEligibleMember instead. Checking only
	// the transition would let an already-enabled empty group echo
	// {"enabled":true} through to the store and surface the wrong message.
	if in.Enabled != nil && *in.Enabled {
		n := cur.MemberCount
		if in.Members != nil {
			n = len(members)
		}
		if n == 0 {
			writeErr(w, http.StatusConflict, "cannot enable a group with no members")
			return
		}
	}
	if _, err := store.UpdateGroup(s.DB, id, in.Name, in.Enabled, members); err != nil {
		writeGroupErr(w, err, "group update failed")
		return
	}
	// The write is durable from here on: announce it before the read-back,
	// so a read-back failure cannot swallow the catalog change (peers must
	// learn the new group state even when the echo fails).
	s.catalogChanged()
	g, members2, err := store.GetGroup(s.DB, id)
	if err != nil {
		// The mutation already committed: a read-back failure must not be
		// reported as "update failed" (a concurrent DELETE is ErrNotFound →
		// 404, not a 500 the client would retry). The fallback says what
		// actually happened: the write is durable, only the echo failed.
		writeGroupErr(w, err, "group updated; read-back failed")
		return
	}
	writeJSON(w, http.StatusOK, groupViewOf(g, members2))
}

func (s *Server) AdminDeleteGroup(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := store.DeleteGroup(s.DB, id); err != nil {
		writeGroupErr(w, err, "group delete failed")
		return
	}
	s.catalogChanged()
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
