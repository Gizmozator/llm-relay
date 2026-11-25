// Edge-proxy tests (ticket #17): the connector's local /v1 listener, the
// relay's inbound-Req serving through its own proxy mux, and the guards
// around both — Authorization verbatim (the server's own key auth runs and
// metering lands on the real key/user), the loop guard (a request over
// backend X's channel never routes back to X), fail-fast channel
// admission (503, no queue), mid-stream cancel propagation, and the
// per-channel tier-5 bucket keyed on the remote-backend id.
package remote_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/fed"
	"llm-relay/internal/proxy"
	"llm-relay/internal/ratelimit"
	"llm-relay/internal/remote"
	"llm-relay/internal/store"
	"llm-relay/internal/tlscert"
	"llm-relay/internal/usage"
)

// taggedUpstream is a minimal OpenAI-compatible engine whose answers name
// it, so tests can tell which engine served a response. The "slow-up"
// upstream model streams one chunk and then holds until its request
// context dies (recording the cancellation) — the channel-full and
// mid-stream-cancel proofs ride that endpoint.
type taggedUpstream struct {
	*httptest.Server
	tag       string
	total     atomic.Int64
	inflight  atomic.Int64
	cancelled atomic.Bool
}

func newTaggedUpstream(t *testing.T, tag string) *taggedUpstream {
	t.Helper()
	u := &taggedUpstream{tag: tag}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		u.total.Add(1)
		var in struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Model == "slow-up" {
			u.inflight.Add(1)
			defer u.inflight.Add(-1)
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s-first\n\n", tag)
			fl.Flush()
			<-r.Context().Done()
			u.cancelled.Store(true)
			return
		}
		if !in.Stream {
			fmt.Fprintf(w, `{"choices":[{"message":{"content":"from-%s"}}],`+
				`"usage":{"prompt_tokens":5,"completion_tokens":3}}`, tag)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %s-%d\n\n", tag, i)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	u.Server = httptest.NewServer(mux)
	t.Cleanup(u.Close)
	return u
}

// edgeEnv is a full relay (proxy mux + registry + tier-5 limiter) plus one
// connector box ("box-x") whose engine is upX and whose edge listener the
// test clients dial. upY is a plain http backend, so loop-guard and
// channel scenarios can tell "routed elsewhere" from "routed back" by the
// content marker alone.
type edgeEnv struct {
	db      *sql.DB
	reg     *remote.Registry
	key     string // valid relay API key (user "bob", key "test")
	tokenX  string // box-x's rb- connect token
	bX      *store.Backend
	upX     *taggedUpstream
	upY     *taggedUpstream
	edge    *remote.Edge
	edgeURL string
	muxSrv  *httptest.Server // the relay mux directly (non-edge traffic)
	tokens  map[string]*store.Backend
	srvURL  string
	pin     string
}

func newEdgeEnv(t *testing.T) *edgeEnv {
	return newEdgeEnvWithLimit(t, ratelimit.Rate{Rate: 100, Burst: 200})
}

func newEdgeEnvWithLimit(t *testing.T, channelRate ratelimit.Rate) *edgeEnv {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/edge.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	hash, _ := auth.HashPassword("x")
	user, err := store.CreateUser(d, "bob", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	plain, kh, prefix, _ := auth.GenerateAPIKey()
	if _, err := store.CreateKey(d, user.ID, "test", kh, prefix); err != nil {
		t.Fatal(err)
	}

	env := &edgeEnv{db: d, key: plain, tokens: map[string]*store.Backend{}}

	env.bX, env.tokenX, err = store.CreateRemoteBackend(d, "box-x")
	if err != nil {
		t.Fatal(err)
	}
	env.upX = newTaggedUpstream(t, "X")
	env.upY = newTaggedUpstream(t, "Y")
	bY, err := store.CreateBackend(d, "plain-y", env.upY.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, env.bX.ID, "up-id", "x-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, bY.ID, "up-id", "pub-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, bY.ID, "slow-up", "slow-model"); err != nil {
		t.Fatal(err)
	}

	rec := usage.NewRecorder(d)
	rec.Start()
	ph := proxy.New(d, rec, false)
	env.reg = remote.NewRegistry(d, fastOpts(), "test-relay", "test")
	t.Cleanup(func() { env.reg.CloseAll("teardown") })
	ph.Remote = env.reg
	ph.Health.Remote = env.reg.BackendOnline
	mux := ph.Mux()
	env.reg.AttachServe(mux, ratelimit.NewStore(channelRate, ratelimit.StoreMax))

	// Connect route behind a token check (stands in for RemoteAuth):
	// every box's token maps to its backend row.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		b, ok := env.tokens[tok]
		if !ok {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		env.reg.ServeConn(w, r, b, auth.HashKey(tok))
	}))
	t.Cleanup(srv.Close)
	env.srvURL = srv.URL
	env.pin = tlscert.Fingerprint(srv.Certificate().Raw)
	env.tokens[env.tokenX] = env.bX

	// The box-x connector: engine upX, edge listener wrapped in a plain
	// httptest server (the bind itself is normalizeEdgeAddr's business,
	// covered by the internal unit test).
	env.edge, err = remote.NewEdge("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go remote.RunClient(ctx, remote.ClientConfig{
		ServerURL: env.srvURL, Token: env.tokenX, Pin: env.pin,
		EngineURL: env.upX.URL, Edge: env.edge, Options: fastOpts().Options,
	}, nil)
	edgeSrv := httptest.NewServer(env.edge.Handler())
	t.Cleanup(edgeSrv.Close)
	env.edgeURL = edgeSrv.URL
	env.muxSrv = httptest.NewServer(mux)
	t.Cleanup(env.muxSrv.Close)
	waitFor(t, 5*time.Second, "box-x channel online", func() bool { return env.reg.Online(env.bX.ID) })
	return env
}

// addBox starts a second connector box (own backend row, engine, edge
// listener) on the same relay — the per-channel keying proof needs two
// live channels.
func (env *edgeEnv) addBox(t *testing.T, name string) (*store.Backend, *taggedUpstream, string) {
	t.Helper()
	b, token, err := store.CreateRemoteBackend(env.db, name)
	if err != nil {
		t.Fatal(err)
	}
	env.tokens[token] = b
	up := newTaggedUpstream(t, name)
	edge, err := remote.NewEdge("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go remote.RunClient(ctx, remote.ClientConfig{
		ServerURL: env.srvURL, Token: token, Pin: env.pin,
		EngineURL: up.URL, Edge: edge, Options: fastOpts().Options,
	}, nil)
	srv := httptest.NewServer(edge.Handler())
	t.Cleanup(srv.Close)
	waitFor(t, 5*time.Second, "channel online for "+name, func() bool { return env.reg.Online(b.ID) })
	return b, up, srv.URL
}

// edgeDo sends one request through an edge listener URL.
func edgeDo(t *testing.T, url, method, body, key string) *http.Response {
	t.Helper()
	return edgeDoTimeout(t, 0, url, method, body, key)
}

// edgeDoTimeout bounds the whole exchange: a client timeout is how the
// fail-fast proof distinguishes "immediate 503" from "queued forever".
func edgeDoTimeout(t *testing.T, timeout time.Duration, url, method, body, key string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("edge request %s %s: %v", method, url, err)
	}
	return resp
}

func TestEdgeChatThroughRelay(t *testing.T) {
	env := newEdgeEnv(t)

	// Non-streaming chat: client → edge listener → channel → relay mux →
	// plain http backend Y (NOT back to X — see the loop-guard test).
	resp := edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`, env.key)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-Y") {
		t.Fatalf("chat: %d %s", resp.StatusCode, body)
	}

	// Streaming chat: SSE chunks must flow live through both tunnels.
	resp = edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, env.key)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stream status %d: %s", resp.StatusCode, body)
	}
	var chunks []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if s := strings.TrimPrefix(sc.Text(), "data: "); s != "" {
			chunks = append(chunks, s)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("stream read: %v", err)
	}
	resp.Body.Close()
	if len(chunks) != 4 || chunks[3] != "[DONE]" || chunks[0] != "Y-0" {
		t.Fatalf("stream chunks = %v", chunks)
	}

	// GET /v1/models rides the same path (auth included).
	resp = edgeDo(t, env.edgeURL+"/v1/models", "GET", "", env.key)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "pub-model") {
		t.Fatalf("models: %d %s", resp.StatusCode, body)
	}
}

// Metering honesty: the server's own key auth runs on the carried sk-
// (bad key → 401 from the relay, not the connector), and the usage row
// records the REAL client key and user — not the connector's identity.
func TestEdgeAuthAndMetering(t *testing.T) {
	env := newEdgeEnv(t)

	// Invalid sk- → 401 from the relay's authKey (Authorization survived
	// the tunnel; the rb- channel token granted nothing by itself).
	resp := edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model","messages":[]}`, "sk-not-a-real-key")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(string(body), "invalid API key") {
		t.Fatalf("bad key: %d %s", resp.StatusCode, body)
	}
	// No key at all → 401 too.
	resp = edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model","messages":[]}`, "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("missing key: want 401, got %d", resp.StatusCode)
	}

	// Valid key → 200, and exactly one usage row — metered against the
	// real key/user carried in the Authorization header.
	resp = edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`, env.key)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("valid key: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, rows, err := store.UsageDetail(env.db, &store.UsageFilter{AllUsers: true}, 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			row := rows[0]
			if row.Username != "bob" || row.KeyName != "test" || !row.KeyLocal {
				t.Fatalf("metering not honest: user=%q key=%q local=%v", row.Username, row.KeyName, row.KeyLocal)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected exactly 1 usage row (the two 401s must not meter), got %d", len(rows))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Loop guard: a request that arrived over backend X's channel must never
// be routed back to X — a group falls over to the other member, a
// single-model request on X is refused, and X's engine never sees it.
// Ordinary (non-edge) traffic still routes to X normally.
func TestEdgeLoopGuard(t *testing.T) {
	env := newEdgeEnv(t)
	en := true
	g, err := store.CreateGroup(env.db, "g")
	if err != nil {
		t.Fatal(err)
	}
	xm, err := store.ListModels(env.db)
	if err != nil {
		t.Fatal(err)
	}
	var xID, yID int64
	for _, m := range xm {
		switch m.Name {
		case "x-model":
			xID = m.ID
		case "pub-model":
			yID = m.ID
		}
	}
	if xID == 0 || yID == 0 {
		t.Fatal("models not found for group members")
	}
	if _, err := store.UpdateGroup(env.db, g.ID, nil, &en,
		[]store.GroupMemberInput{{ModelID: xID, Weight: 1}, {ModelID: yID, Weight: 1}}); err != nil {
		t.Fatal(err)
	}

	// Group over X's edge: must land on Y, and X's engine must stay clean.
	resp := edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"g","messages":[{"role":"user","content":"hi"}]}`, env.key)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-Y") {
		t.Fatalf("group via X: %d %s", resp.StatusCode, body)
	}
	if got := env.upX.total.Load(); got != 0 {
		t.Fatalf("loop guard failed: X engine saw %d request(s)", got)
	}

	// Single model hosted on X, requested over X's own channel: refused
	// (dial-class → 502), X's engine still untouched.
	resp = edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"x-model","messages":[{"role":"user","content":"hi"}]}`, env.key)
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("single model on origin: want 502, got %d", resp.StatusCode)
	}
	if got := env.upX.total.Load(); got != 0 {
		t.Fatalf("loop guard failed: X engine saw %d request(s)", got)
	}

	// Sanity: the guard is edge-scoped only — ordinary relay traffic still
	// routes to X over the same channel.
	req, _ := http.NewRequest("POST", env.muxSrv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"x-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.key)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-X") {
		t.Fatalf("direct x-model: %d %s", resp.StatusCode, body)
	}
	if got := env.upX.total.Load(); got != 1 {
		t.Fatalf("direct request should have hit X once, saw %d", got)
	}
}

// Channel admission is fail-fast: with all fed.MaxInflight tunnel slots
// held by slow streams, the next request gets an immediate 503 — it must
// never queue behind the backlog (a bounded-timeout client proves it).
func TestEdgeChannelFullFailFast(t *testing.T) {
	env := newEdgeEnv(t)
	const n = fed.MaxInflight

	type held struct {
		resp *http.Response
	}
	heldCh := make(chan held, n)
	ready := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.DefaultClient.Do(mustReq(env.edgeURL+"/v1/chat/completions",
				`{"model":"slow-model","stream":true}`, env.key))
			if err != nil {
				ready <- err
				return
			}
			sc := bufio.NewScanner(resp.Body)
			if !sc.Scan() || !strings.Contains(sc.Text(), "first") {
				resp.Body.Close()
				ready <- fmt.Errorf("no first chunk (status %d)", resp.StatusCode)
				return
			}
			ready <- nil
			heldCh <- held{resp} // keep the tunnel slot: body stays open
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-ready; err != nil {
			t.Fatalf("slow stream %d: %v", i, err)
		}
	}
	waitFor(t, 5*time.Second, "all slow streams in flight",
		func() bool { return env.upY.inflight.Load() == n })

	// The (n+1)-th: immediate 503. A client timeout bounds the wait — a
	// queueing implementation would hang here instead of answering.
	start := time.Now()
	resp := edgeDoTimeout(t, 5*time.Second, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"slow-model","stream":true}`, env.key)
	elapsed := time.Since(start)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("channel-full: want 503, got %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("channel-full 503 must carry Retry-After")
	}
	if elapsed > 3*time.Second {
		t.Errorf("channel-full took %v — must fail fast, not queue", elapsed)
	}

	// Release the held slots: service recovers immediately. Each held
	// response is drained exactly once (no close: a sender may still be
	// on its way to the buffered channel).
	for i := 0; i < n; i++ {
		h := <-heldCh
		h.resp.Body.Close()
	}
	wg.Wait()
	waitFor(t, 5*time.Second, "slots released", func() bool { return env.upY.inflight.Load() < n })
	resp = edgeDoTimeout(t, 5*time.Second, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model"}`, env.key)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("after release: want 200, got %d: %s", resp.StatusCode, body)
	}
}

// A body over fed.MaxBody (or an aborted upload) is refused locally with an
// honest 413/400 — but OpenRequest already claimed a tunnel slot, so those
// error paths MUST release it. Before the fix each oversized body leaked one
// of fed.MaxInflight slots: MaxInflight of them wedged the tunnel (every
// later request 503'd "channel at capacity") and left a dead channel
// registered so the relay's Res frames stalled the read pump. Any LAN client
// could trigger this deliberately. Proof: MaxInflight oversized bodies, then
// a normal request must still succeed.
func TestEdgeBodyErrorReleasesSlot(t *testing.T) {
	env := newEdgeEnv(t)
	big := strings.Repeat("a", fed.MaxBody+1)
	for i := 0; i < fed.MaxInflight; i++ {
		resp := edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST", big, env.key)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized body %d: want 413, got %d", i, resp.StatusCode)
		}
	}
	// Every slot was released by the 413 path, so the tunnel is still usable.
	resp := edgeDoTimeout(t, 5*time.Second, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model"}`, env.key)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("after %d oversized bodies: want 200, got %d: %s",
			fed.MaxInflight, resp.StatusCode, body)
	}
}

func mustReq(url, body, key string) *http.Request {
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

// Cancel mid-stream propagates the whole chain: the local client hangs up
// → the edge sends a Cancel frame → the relay cancels the served request
// → the upstream engine call is aborted.
func TestEdgeCancelMidStream(t *testing.T) {
	env := newEdgeEnv(t)
	resp := edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"slow-model","stream":true}`, env.key)
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || !strings.Contains(sc.Text(), "first") {
		resp.Body.Close()
		t.Fatalf("first chunk: %v", sc.Err())
	}
	// Hang up mid-stream: closing an undrained body tears the connection,
	// the edge sees r.Context().Done and must Cancel.
	resp.Body.Close()
	waitFor(t, 5*time.Second, "engine cancellation via the Cancel chain",
		func() bool { return env.upY.cancelled.Load() })
}

// Tier 5 keys on the remote-backend id, not the socket IP: a drained
// channel 429s its own traffic while a second channel — dialing in from
// the very same source address — keeps its fresh bucket.
func TestEdgePerChannelRateLimit(t *testing.T) {
	env := newEdgeEnvWithLimit(t, ratelimit.Rate{Rate: 0.5, Burst: 2})

	var codes []int
	for i := 0; i < 8; i++ {
		resp := edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
			`{"model":"pub-model"}`, env.key)
		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
	}
	var ok, limited int
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 429:
			limited++
		default:
			t.Fatalf("unexpected status %d in %v", c, codes)
		}
	}
	if ok < 2 || limited < 4 {
		t.Fatalf("tier-5 bucket did not bite: %v", codes)
	}
	// The 429 must carry Retry-After (one honest shape for every limiter).
	resp := edgeDo(t, env.edgeURL+"/v1/chat/completions", "POST",
		`{"model":"pub-model"}`, env.key)
	resp.Body.Close()
	if resp.StatusCode == 429 && resp.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	// Second box, same source IP, fresh per-channel bucket → served.
	if _, _, edgeZ := env.addBox(t, "box-z"); true {
		resp := edgeDo(t, edgeZ+"/v1/chat/completions", "POST",
			`{"model":"pub-model"}`, env.key)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("second channel shares the drained bucket (per-IP keying!): %d %s", resp.StatusCode, body)
		}
	}
}
