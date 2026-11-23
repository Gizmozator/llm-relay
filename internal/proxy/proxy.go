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
	// Sessions tracks recent session activity for drain mode (which
	// sessions count as "currently running").
	Sessions *SessionTracker
	// Health caches backend reachability for the agent endpoints; the
	// dashboard API keeps its own instance (same rules, same TTL).
	Health *HealthCache
	// Fed tunnels peer:// and hub:// backends through a federation channel
	// (nil = standalone: synthetic schemes fail as unroutable).
	Fed FedTransport
	// RelayURL is the public relay ROOT (PUBLIC_RELAY_URL, no /v1),
	// used to render the agent guide. Empty renders the placeholder as-is.
	RelayURL string
	// DashboardURL is the public dashboard ROOT (PUBLIC_DASHBOARD_URL),
	// shown in the agent guide so it can point its human at the key
	// creation page. Empty drops the mention.
	DashboardURL string
}

func New(db *sql.DB, rec *usage.Recorder, trustProxyHeaders bool) *Handler {
	h := &Handler{
		DB:                db,
		Rec:               rec,
		TrustProxyHeaders: trustProxyHeaders,
		Flight:            NewInFlight(),
		Sessions:          NewSessionTracker(),
		Health:            NewHealthCache(),
	}
	inner := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 8,
	}
	// No overall client timeout: generations may run for many minutes
	// (OCR sends 10+ min non-streaming requests). Bounded at the
	// transport level instead: dial and TLS handshake only.
	h.Client = &http.Client{Transport: &schemeDispatch{inner: inner, h: h}}
	return h
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

// Models lists the public catalog under the OpenAI /v1/models shape:
// enabled groups plus enabled public models — internal members hidden,
// name shadowing and dedupe resolved store-side (ListPublicCatalog).
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	if _, _, err := h.authKey(r); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	entries, err := store.ListPublicCatalog(h.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	data := []map[string]string{}
	for _, e := range entries {
		data = append(data, map[string]string{"id": e.Name, "object": "model", "owned_by": "llm-relay"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) handle(w http.ResponseWriter, r *http.Request, endpoint string, record bool) {
	start := time.Now()

	// A tunneled request arrives with an injected identity (fed frame
	// server); ordinary traffic authenticates its relay API key.
	fs := FedServeFrom(r.Context())
	var key *store.APIKey
	var user *store.User
	if fs != nil && fs.User != nil && fs.Key != nil {
		key, user = fs.Key, fs.User
	} else {
		var err error
		key, user, err = h.authKey(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
	}
	// Task 4: served federation traffic is metered too — the spoke records
	// what it served (fed_direction='served' + the hub-stamped consumer),
	// the hub records what its account consumed. Attribution rides on fs /
	// the resolved backend, computed once below into fedOf().
	meter := record
	// The outbound tunnel attributes a hub-direct consumer by name (only
	// the fed layer on a hub ever reads this; a spoke never stamps).
	r = r.WithContext(WithRequestUser(r.Context(), user.Username))

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

	tgt, err := store.ResolveTarget(h.DB, peek.Model)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", peek.Model))
			return
		}
		// A DB failure is not a typo: a transient SQLITE_BUSY must not
		// masquerade as "model not found" with no trace (ResolveTarget
		// propagates query errors alongside the ErrNotFound sentinel).
		log.Printf("resolve target %q: %v", peek.Model, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Serving gates for tunneled traffic. A spoke-side served request
	// (LocalOnly) may only touch local engines: a plain model materialized
	// from the federation is not servable here — the spoke must have asked
	// the hub, not the other way around.
	//
	// Both served sides enforce the sharing rules. The hub keys on the
	// requesting peer's id; the spoke keys on LocalOnly with peer id 0 —
	// the hub's own traffic arrives with OriginPeer=0 (a spoke has no peer
	// rows), and SharedWithPeer(model, 0) is exactly the spoke's semantics:
	// 'all' → true, 'allowlist' → no rows for peer 0 → false, 'off' →
	// false. Without the LocalOnly arm a compromised hub could name an
	// 'off' model in a req frame and get it served. Not shared → 403 with
	// a clear body; for a group, invisible members are dropped before
	// ranking (the catalog only advertises visible ones, so an invisible
	// pick means a stale or forged request).
	if fs != nil && fs.LocalOnly && tgt.Kind == store.TargetModel {
		if _, _, syn := store.SyntheticBaseURL(tgt.Backend.BaseURL); syn {
			writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", peek.Model))
			return
		}
	}
	if fs != nil && (fs.OriginPeer != 0 || fs.LocalOnly) {
		switch tgt.Kind {
		case store.TargetGroup:
			keep := make([]store.GroupMember, 0, len(tgt.Members))
			for _, mem := range tgt.Members {
				if fedShareVisible(h.DB, mem, fs.OriginPeer) {
					keep = append(keep, mem)
				}
			}
			if len(keep) == 0 {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("model %q is not shared with this peer", peek.Model))
				return
			}
			tgt.Members = keep
		case store.TargetModel:
			mem := store.GroupMember{Model: tgt.Model, Backend: tgt.Backend}
			if !fedShareVisible(h.DB, mem, fs.OriginPeer) {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("model %q is not shared with this peer", peek.Model))
				return
			}
		}
	}
	// The session key uses the REQUESTED name: for a group that is the group
	// name, so affinity (and metering) stays stable across membership edits.
	session := r.Header.Get("X-Session-Id")
	if session == "" {
		session = DeriveSession(endpoint, body, key.ID, peek.Model)
	}

	// Group requests fan out over the group's members: hard exclusions
	// dropped, then ranked by the session hash (least-loaded when there is
	// no session); the ranked list feeds the pre-first-byte retry below.
	// A plain model is a one-member list with today's exact behavior. Note
	// that a group with no eligible members already fell through to the
	// model path inside ResolveTarget — a group name legitimately lands
	// here as TargetModel.
	var candidates []store.GroupMember
	streamID := int64(0)
	streamFor := func(mem store.GroupMember) Stream {
		return Stream{
			UserID:    user.ID,
			User:      user.Username,
			Key:       key.Name,
			ModelID:   mem.Model.ID,
			Model:     mem.Model.Name,
			Endpoint:  endpoint,
			Session:   session,
			IP:        h.clientIP(r),
			Streaming: peek.Stream,
		}
	}
	if tgt.Kind == store.TargetGroup {
		eligible := h.eligibleMembers(r.Context(), tgt.Members)
		if len(eligible) == 0 {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("group %q has no available member: every member is disabled, draining, or unhealthy", tgt.Group.Name))
			return
		}
		// Enter BEFORE ranking: the session-less ranking reads the InFlight
		// counts, and a burst of siblings must each see the others' streams
		// or they all read identical counts and herd onto one member. The
		// caller's own provisional entry is excluded from its own ranking
		// (no phantom self-count). Provisional identity = first eligible
		// member, corrected right after ranking (and again if a retry moves
		// the request). The deferred Leave covers every exit path from here
		// on; the 503 above stays before Enter so refused requests never
		// enter.
		streamID = h.Flight.Enter(streamFor(eligible[0]))
		defer h.Flight.Leave(streamID)
		candidates = rankMembers(eligible, session, h.Flight, streamID)
		if candidates[0].ModelID != eligible[0].ModelID {
			h.Flight.SetModel(streamID, candidates[0].Model.ID, candidates[0].Model.Name)
		}
	} else {
		candidates = []store.GroupMember{{ModelID: tgt.Model.ID, Model: tgt.Model, Backend: tgt.Backend}}
	}
	model, backend := candidates[0].Model, candidates[0].Backend

	// fedOf derives the usage-row attribution at record time: the backend
	// variable follows the member that actually served (see the walk), so
	// a group that fell over onto a peer:// member stamps the peer that
	// really answered. The hub's ledger is double-entry: EVERY hub row
	// whose resolved member lives on a synthetic peer:// backend carries a
	// mirrored served row crediting that backend's owner (same counts).
	// Transit never reaches this path — the hub pumps those frames outside
	// the pipeline and writes its own pair (meterTransit) — so the two
	// cannot double-write.
	fedOf := func() *fedAttrib {
		kind, hostPeer, syn := store.SyntheticBaseURL(backend.BaseURL)
		syn = syn && kind == store.FedSchemePeer
		if fs != nil {
			if fs.FedDirection == "" {
				return nil
			}
			a := &fedAttrib{direction: fs.FedDirection, user: fs.FedUser}
			if fs.FedDirection == FedConsumed && syn {
				// Uniform convention: a consumed row names the peer whose
				// hardware SERVED (the resolved member's host); hub
				// hardware has no peer row, so a spoke→hub-local serve
				// leaves fed_peer_id NULL — the spender is already the
				// row's user.
				p := hostPeer
				a.peerID = &p
				a.mirror = &fedMirror{hostPeer: hostPeer, originPeer: fs.OriginPeer}
			}
			return a
		}
		if h.Fed != nil && h.Fed.IsHub() && syn {
			p := hostPeer
			return &fedAttrib{direction: FedConsumed, peerID: &p,
				mirror: &fedMirror{hostPeer: hostPeer, consumer: user.Username}}
		}
		return nil
	}

	// Drain gate: a draining backend refuses NEW sessions so the engine's
	// server can be shut down gracefully, but sessions that are currently
	// running (active within drainSessionWindow) may finish their
	// conversation. Memory first; usage_log covers a relay restart while
	// draining. An empty session id (not conversation-shaped) is a new
	// session by definition.
	if backend.Draining && !h.Sessions.Recent(session) {
		old, dbErr := store.SessionRecent(h.DB, session, time.Now().Add(-drainSessionWindow))
		if dbErr != nil {
			log.Printf("relay: drain session lookup failed: %v", dbErr)
		}
		if !old {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("backend %q is draining: no new sessions; existing sessions may continue", backend.Name))
			return
		}
	}
	h.Sessions.Mark(session)

	// Counted in-flight until the handler returns: the stream is fully
	// copied (or the body read) before this deferred Leave runs. The record
	// carries who/what/where so the admin live view can list active
	// connections; refused (drain) requests never enter it. Group requests
	// already entered above (before ranking, so siblings see this stream in
	// the load counts); the model path enters here, after the drain gate.
	if tgt.Kind != store.TargetGroup {
		streamID = h.Flight.Enter(streamFor(candidates[0]))
		defer h.Flight.Leave(streamID)
	}

	// Forward. Group requests may retry the NEXT ranked member, but only
	// pre-first-byte: Client.Do returns an error only when no response byte
	// arrived, so a retry can never duplicate spent tokens (plan Decision 5).
	// Request-build failures (an admin-edited BaseURL that no longer parses)
	// fan out the same way: one broken member must not abort the rest or
	// skip metering. A plain model has one candidate: today's single-shot
	// behavior.
	var resp *http.Response
	for i, cand := range candidates {
		if cerr := r.Context().Err(); cerr != nil {
			// Client is gone: stop the walk WITHOUT metering — no upstream
			// call completed and nothing was spent, so there is no cost to
			// record (mid-stream disconnects are the ones that earn a row).
			log.Printf("relay: client gone before upstream attempt: %v", cerr)
			return
		}
		fwdBody := rewriteBody(body, upstreamFor(cand), endpoint, peek.Stream)
		url := joinBackendURL(cand.Backend.BaseURL, r.URL.Path)
		var derr error
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(fwdBody))
		if err != nil {
			derr = err // build failure fans out like a dial failure
		} else {
			req.Header.Set("Content-Type", "application/json")
			if cand.Backend.APIKey != "" {
				if endpoint == EpMessages {
					req.Header.Set("x-api-key", cand.Backend.APIKey)
				} else {
					req.Header.Set("Authorization", "Bearer "+cand.Backend.APIKey)
				}
			}
			resp, derr = h.Client.Do(req)
		}
		if derr == nil {
			if cand.Model.ID != model.ID {
				// The request moved to another member: metering and the
				// live in-flight entry follow it (backend too — the
				// consumed stamp names the peer that actually answered).
				model = cand.Model
				backend = cand.Backend
				h.Flight.SetModel(streamID, model.ID, model.Name)
			}
			break
		}
		// Full error (dial targets, internals) goes to the server log only.
		log.Printf("relay: upstream %s failed: %v", url, derr)
		if r.Context().Err() != nil {
			// The failure was the client hanging up: same rule as the
			// loop-top guard — no completed call, nothing spent, no row.
			log.Printf("relay: client gone, abandoning group walk")
			return
		}
		if i == len(candidates)-1 {
			h.record(meter, start, user, key, r, endpoint, session, model, peek.Model,
				pricing.Tokens{}, pricing.Costs{}, false, false, http.StatusBadGateway, "upstream request failed", fedOf())
			writeError(w, http.StatusBadGateway, "upstream unavailable")
			return
		}
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
	h.record(meter, start, user, key, r, endpoint, session, model, peek.Model,
		tok, costs, stream, estimated, status, errMsg, fedOf())
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

// fedAttrib is one usage row's federation attribution (nil = plain local
// traffic, fed columns stay at their empty defaults).
type fedAttrib struct {
	direction string // proxy.FedServed | proxy.FedConsumed
	user      string // hub-stamped hub-side consumer name (served rows)
	peerID    *int64 // counterparty peer (nil = none)
	// mirror is the hub's double-entry counterpart of a consumed row: a
	// served row crediting the hosting peer's owner with the same counts
	// (nil = no mirror: local members, spoke-side rows).
	mirror *fedMirror
}

// fedMirror names one mirrored served row: hostPeer's owner is credited
// with serving; originPeer is the requesting peer (0 = a hub-direct user,
// whose own name rides in consumer).
type fedMirror struct {
	hostPeer   int64
	originPeer int64
	consumer   string
}

// fedMirrorRow builds the mirrored served row from the primary (consumed)
// event: same counts, model row and endpoint, credited to the hosting
// peer's owner under the synthetic federation key. nil (with a log line)
// when the identities cannot be resolved — a peer deleted mid-flight; the
// primary row still stands.
func (h *Handler) fedMirrorRow(m *fedMirror, ev *store.UsageEvent) *store.UsageEvent {
	host, err := store.FedPeerOwner(h.DB, m.hostPeer)
	if err != nil {
		log.Printf("relay: fed mirror host peer %d: %v", m.hostPeer, err)
		return nil
	}
	// fed_user carries the consumer side: the origin peer's owner name for
	// a spoke-originated request, the hub user's own name for a direct one.
	name := m.consumer
	if m.originPeer != 0 {
		o, err := store.FedPeerOwner(h.DB, m.originPeer)
		if err != nil {
			log.Printf("relay: fed mirror origin peer %d: %v", m.originPeer, err)
			return nil
		}
		name = o.Username
	}
	mir := *ev
	mir.UserID = host.ID
	mir.KeyID = 0 // synthetic federation key (usage_log.api_key_id has no FK)
	mir.FedDirection = FedServed
	mir.FedUser = name
	p := m.hostPeer
	mir.FedPeerID = &p // uniform: a served row names the HOST it credits
	return &mir
}

func (h *Handler) record(record bool, start time.Time, u *store.User, k *store.APIKey, r *http.Request,
	endpoint, session string, m *store.Model, requested string,
	tok pricing.Tokens, costs pricing.Costs, stream, estimated bool, status int, errMsg string, fed *fedAttrib) {
	if !record {
		return
	}
	ev := &store.UsageEvent{
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
	}
	if fed != nil {
		ev.FedDirection = fed.direction
		ev.FedUser = fed.user
		ev.FedPeerID = fed.peerID
	}
	h.Rec.Submit(ev)
	if fed != nil && fed.mirror != nil {
		// The hub's double-entry counterpart, submitted after the primary
		// row so the ledger never shows a credit without its spend.
		if mir := h.fedMirrorRow(fed.mirror, ev); mir != nil {
			h.Rec.Submit(mir)
		}
	}
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
