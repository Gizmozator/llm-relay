// Package proxy is the OpenAI/Anthropic-compatible request forwarder: it
// authenticates relay API keys, routes the requested model to its backend,
// passes requests and streams through untouched (modulo model rewrite), and
// records metered usage for every request.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/pricing"
	"llm-relay/internal/store"
	"llm-relay/internal/usage"
)

const maxBody = 64 << 20 // 64 MiB request body cap

// statusClientClosed is nginx's non-standard 499: the client hung up.
const statusClientClosed = 499

type Handler struct {
	DB                *sql.DB
	Rec               *usage.Recorder
	TrustProxyHeaders bool
	Client            *http.Client
}

func New(db *sql.DB, rec *usage.Recorder, trustProxyHeaders bool) *Handler {
	return &Handler{
		DB:                db,
		Rec:               rec,
		TrustProxyHeaders: trustProxyHeaders,
		// No client timeout: streams may run for many minutes.
		Client: &http.Client{},
	}
}

// Mux returns a handler with all relay routes registered — used by main and
// by tests.
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", h.Chat)
	mux.HandleFunc("POST /v1/completions", h.Completions)
	mux.HandleFunc("POST /v1/responses", h.Responses)
	mux.HandleFunc("POST /v1/messages/count_tokens", h.CountTokens)
	mux.HandleFunc("POST /v1/messages", h.Messages)
	mux.HandleFunc("POST /v1/embeddings", h.Embeddings)
	mux.HandleFunc("GET /v1/models", h.Models)
	return mux
}

// Route handlers wired by main.

func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, EpChat, true)
}
func (h *Handler) Completions(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, EpCompletions, true)
}
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, EpResponses, true)
}
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, EpMessages, true)
}
func (h *Handler) CountTokens(w http.ResponseWriter, r *http.Request) {
	// Pure pass-through: no generation happens, nothing to meter.
	h.handle(w, r, EpMessages, false)
}
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, EpEmbeddings, true)
}

// Models lists the enabled catalog under the OpenAI /v1/models shape.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	if _, _, err := h.authKey(r); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	models, err := store.ListPublicModels(h.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	data := []map[string]string{}
	for _, m := range models {
		if m.Enabled {
			data = append(data, map[string]string{"id": m.Name, "object": "model", "owned_by": "llm-relay"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) handle(w http.ResponseWriter, r *http.Request, endpoint string, record bool) {
	start := time.Now()

	key, user, err := h.authKey(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &peek); err != nil || peek.Model == "" {
		writeError(w, http.StatusBadRequest, "missing or invalid 'model' field")
		return
	}

	model, backend, err := store.ModelByName(h.DB, peek.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", peek.Model))
		return
	}

	session := r.Header.Get("X-Session-Id")
	if session == "" {
		session = DeriveSession(endpoint, body, key.ID, model.Name)
	}

	fwdBody := rewriteBody(body, model.UpstreamModel, endpoint, peek.Stream)
	url := strings.TrimRight(backend.BaseURL, "/") + r.URL.Path
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(fwdBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if backend.APIKey != "" {
		if endpoint == EpMessages {
			req.Header.Set("x-api-key", backend.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+backend.APIKey)
		}
	}

	resp, err := h.Client.Do(req)
	if err != nil {
		// Full error (dial targets, internals) goes to the server log only.
		log.Printf("relay: upstream %s failed: %v", url, err)
		h.record(record, start, user, key, r, endpoint, session, model, peek.Model,
			pricing.Tokens{}, pricing.Costs{}, false, false, http.StatusBadGateway, "upstream request failed")
		writeError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	stream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	var (
		tok       pricing.Tokens
		found     bool
		estimated bool
		status    = resp.StatusCode
		errMsg    string
	)

	if resp.StatusCode == http.StatusOK {
		if stream {
			acc := newSSEAccum(endpoint)
			status = h.copyStream(w, resp.Body, acc, r)
			switch status {
			case http.StatusOK:
			case statusClientClosed:
				errMsg = "client disconnected mid-stream"
			default:
				errMsg = "upstream stream interrupted"
			}
			// Record whatever usage is known — partial streams still consumed
			// tokens (plan §7.7); costs stay at zero for non-200 rows.
			if tok, found = acc.result(); !found {
				tok = EstimateTokens(int64(len(body)), acc.textChars, acc.textWords)
				estimated = true
			}
		} else {
			respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
			switch {
			case rerr != nil:
				status = http.StatusInternalServerError
				errMsg = "reading upstream response: " + rerr.Error()
			default:
				if tok, found = ExtractUsage(endpoint, respBody); !found {
					chars, words := completionTextStats(endpoint, respBody)
					tok = EstimateTokens(int64(len(body)), chars, words)
					estimated = true
				}
			}
			_, _ = w.Write(respBody)
		}
	} else {
		// Pass the upstream error body through untouched; zero tokens billed.
		_, _ = io.Copy(w, io.LimitReader(resp.Body, maxBody))
		errMsg = fmt.Sprintf("upstream status %d", resp.StatusCode)
	}

	var costs pricing.Costs
	if status == http.StatusOK {
		costs = pricing.Compute(model, tok)
	}
	h.record(record, start, user, key, r, endpoint, session, model, peek.Model,
		tok, costs, stream, estimated, status, errMsg)
}

// copyStream relays SSE bytes verbatim, flushing every chunk, while the
// accumulator scans data payloads. Returns 499 on client hang-up, 500 if the
// upstream stream broke, 200 on clean EOF.
func (h *Handler) copyStream(w http.ResponseWriter, body io.Reader, acc *sseAccum, r *http.Request) int {
	fl, _ := w.(http.Flusher)
	br := bufio.NewReader(body)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return statusClientClosed
			}
			if fl != nil {
				fl.Flush()
			}
			if trimmed := bytes.TrimSpace(line); bytes.HasPrefix(trimmed, []byte("data:")) {
				acc.feed(bytes.TrimSpace(trimmed[5:]))
			}
		}
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				return http.StatusOK
			case errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled):
				return statusClientClosed
			default:
				return http.StatusInternalServerError
			}
		}
	}
}

func (h *Handler) record(record bool, start time.Time, u *store.User, k *store.APIKey, r *http.Request,
	endpoint, session string, m *store.Model, requested string,
	tok pricing.Tokens, costs pricing.Costs, stream, estimated bool, status int, errMsg string) {
	if !record {
		return
	}
	h.Rec.Submit(&store.UsageEvent{
		TS:            time.Now().UTC().Format(time.RFC3339),
		UserID:        u.ID,
		KeyID:         k.ID,
		ClientIP:      h.clientIP(r),
		SessionID:     session,
		Endpoint:      endpoint,
		ModelID:       m.ID,
		ModelRequ:     requested,
		ModelUpstream: m.UpstreamModel,
		Prompt:        tok.Prompt,
		Completion:    tok.Completion,
		Cached:        tok.Cached,
		CacheWrite:    tok.CacheWrite,
		CostIn:        costs.In,
		CostOut:       costs.Out,
		CostCache:     costs.Cache,
		CostWrite:     costs.Write,
		CostTotal:     costs.Total,
		Streamed:      stream,
		Estimated:     estimated,
		Status:        status,
		LatencyMS:     time.Since(start).Milliseconds(),
		Error:         errMsg,
	})
}

// authKey resolves the relay API key from Authorization: Bearer sk-... or,
// for Anthropic-dialect clients, x-api-key.
func (h *Handler) authKey(r *http.Request) (*store.APIKey, *store.User, error) {
	key := ""
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		key = strings.TrimPrefix(authz, "Bearer ")
	} else if xk := r.Header.Get("x-api-key"); xk != "" {
		key = xk
	}
	if key == "" {
		return nil, nil, errors.New("no key")
	}
	return store.KeyByHash(h.DB, auth.HashKey(key))
}

func (h *Handler) clientIP(r *http.Request) string {
	if h.TrustProxyHeaders {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rewriteBody swaps the model name for the upstream id and, for streaming
// chat requests, asks the upstream to include usage in the final chunk.
func rewriteBody(body []byte, upstreamModel, endpoint string, stream bool) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	mu, err := json.Marshal(upstreamModel)
	if err != nil {
		return body
	}
	m["model"] = mu
	if endpoint == EpChat && stream {
		m["stream_options"] = forceIncludeUsage(m["stream_options"])
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// forceIncludeUsage merges include_usage:true into the client's
// stream_options (overriding an explicit false — usage capture is mandatory).
func forceIncludeUsage(raw json.RawMessage) json.RawMessage {
	opts := map[string]json.RawMessage{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &opts)
	}
	opts["include_usage"] = json.RawMessage("true")
	if out, err := json.Marshal(opts); err == nil {
		return out
	}
	return json.RawMessage(`{"include_usage":true}`)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": "invalid_request_error"},
	})
}
