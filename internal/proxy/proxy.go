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
	Flight            *InFlight
	// Health caches backend reachability for the agent endpoints; the
	// dashboard API keeps its own instance (same rules, same TTL).
	Health *HealthCache
	// RelayURL is the public relay base URL (PUBLIC_RELAY_URL), used to
	// render the agent guide. Empty renders the placeholder as-is.
	RelayURL string
}

func New(db *sql.DB, rec *usage.Recorder, trustProxyHeaders bool) *Handler {
	return &Handler{
		DB:                db,
		Rec:               rec,
		TrustProxyHeaders: trustProxyHeaders,
		Flight:            NewInFlight(),
		Health:            NewHealthCache(),
		// No overall client timeout: generations may run for many minutes
		// (OCR sends 10+ min non-streaming requests). Bounded at the
		// transport level instead: dial and TLS handshake only.
		Client: &http.Client{
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
				MaxIdleConnsPerHost: 8,
			},
		},
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
	mux.HandleFunc("GET /v1/agent-guide", h.AgentGuide)
	mux.HandleFunc("GET /v1/utilization", h.UtilizationREST)
	mux.HandleFunc("POST /mcp", h.MCP)
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if int64(len(body)) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
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
	// Counted in-flight from successful routing until the handler returns:
	// the stream is fully copied (or the body read) before this deferred
	// Leave runs. Keyed by user so the live view can scope active streams.
	h.Flight.Enter(user.ID, model.Name)
	defer h.Flight.Leave(user.ID, model.Name)

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
	ct := resp.Header.Get("Content-Type")

	var (
		tok       pricing.Tokens
		found     bool
		estimated bool
		status    = resp.StatusCode
		errMsg    string
	)

	if resp.StatusCode == http.StatusOK {
		if stream {
			// Headers must go out before the first chunk.
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(status)
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
			// Buffer the whole body before committing the status line: a
			// mid-body failure then surfaces as an error instead of a silent
			// 200 with a truncated JSON body.
			respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
			switch {
			case rerr != nil:
				status = http.StatusInternalServerError
				errMsg = "reading upstream response: " + rerr.Error()
				writeError(w, status, "upstream response failed")
			case int64(len(respBody)) > maxBody:
				status = http.StatusBadGateway
				errMsg = "upstream response too large"
				writeError(w, status, "upstream response too large")
			default:
				if ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				w.WriteHeader(status)
				_, _ = w.Write(respBody)
				if tok, found = ExtractUsage(endpoint, respBody); !found {
					chars, words := completionTextStats(endpoint, respBody)
					tok = EstimateTokens(int64(len(body)), chars, words)
					estimated = true
				}
			}
		}
	} else {
		// Pass the upstream error body through untouched; zero tokens billed.
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(status)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, maxBody))
		errMsg = fmt.Sprintf("upstream status %d", resp.StatusCode)
	}

	var costs pricing.Costs
	if status == http.StatusOK {
		costs = pricing.Compute(model, tok)
		pricing.ApplyMultiplier(&costs, user.PriceMultiplier)
	}
	h.record(record, start, user, key, r, endpoint, session, model, peek.Model,
		tok, costs, stream, estimated, status, errMsg)
}

// keepaliveInterval is the maximum upstream silence tolerated in a stream
// before a keepalive comment is injected toward the client. Cloudflare (and
// other edge proxies) cut connections after ~100 s without packets; long
// prefills on big prompts easily exceed that before the first token. SSE
// comment lines are ignored by every compliant parser.
var keepaliveInterval = 20 * time.Second

var keepaliveLine = []byte(": keepalive\n\n")

type streamLine struct {
	line []byte
	err  error
}

// copyStream relays SSE bytes line-by-line, flushing every chunk, while the
// accumulator scans data payloads. Lines are capped at 16 MiB (a longer
// "line" is a broken or hostile upstream). During upstream silence a keepalive
// comment is written so edge proxies keep the connection. Returns 499 on
// client hang-up, 500 if the upstream stream broke, 200 on clean EOF.
func (h *Handler) copyStream(w http.ResponseWriter, body io.Reader, acc *sseAccum, r *http.Request) int {
	fl, _ := w.(http.Flusher)
	lines := make(chan streamLine, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for sc.Scan() {
			// Copy out of the scanner buffer: the next Scan() may shift or
			// refill it while the main loop is still writing this line.
			b := sc.Bytes()
			line := make([]byte, len(b)+1)
			copy(line, b)
			line[len(b)] = '\n'
			select {
			case lines <- streamLine{line: line}:
			case <-done:
				return
			}
		}
		if err := sc.Err(); err != nil {
			select {
			case lines <- streamLine{err: err}:
			case <-done:
			}
		}
	}()
	tk := time.NewTicker(keepaliveInterval)
	defer tk.Stop()
	for {
		select {
		case sl, ok := <-lines:
			if !ok {
				return http.StatusOK
			}
			if sl.err != nil {
				switch {
				case errors.Is(sl.err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled):
					return statusClientClosed
				default:
					return http.StatusInternalServerError
				}
			}
			if _, werr := w.Write(sl.line); werr != nil {
				return statusClientClosed
			}
			if fl != nil {
				fl.Flush()
			}
			if trimmed := bytes.TrimSpace(sl.line); bytes.HasPrefix(trimmed, []byte("data:")) {
				acc.feed(bytes.TrimSpace(trimmed[5:]))
			}
			tk.Reset(keepaliveInterval)
		case <-tk.C:
			if _, werr := w.Write(keepaliveLine); werr != nil {
				return statusClientClosed
			}
			if fl != nil {
				fl.Flush()
			}
		case <-r.Context().Done():
			return statusClientClosed
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
			// Rightmost entry: everything to its left was appended by
			// clients or earlier hops and is spoofable; the rightmost one
			// was appended by our own reverse proxy.
			parts := strings.Split(xf, ",")
			return strings.TrimSpace(parts[len(parts)-1])
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
	if json.Unmarshal(body, &m) != nil || m == nil {
		// non-JSON or JSON null body: nothing to rewrite
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
		// "null" unmarshals to a nil map — reset it before writing.
		if err := json.Unmarshal(raw, &opts); err != nil || opts == nil {
			opts = map[string]json.RawMessage{}
		}
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
	// OpenAI-style error type, chosen by status so clients can branch on it.
	typ := "invalid_request_error"
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		typ = "authentication_error"
	case http.StatusNotFound:
		typ = "not_found_error"
	case http.StatusRequestEntityTooLarge:
		typ = "request_too_large"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		typ = "api_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": typ},
	})
}
