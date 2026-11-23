// Two-relay end-to-end (plan 2026-11-22-relay-federation, task 3): a hub
// and one or two spokes run IN THIS PROCESS — each with its own SQLite DB,
// proxy pipeline and fed.Manager — sharing nothing but the WebSocket
// channel, and each proxying to its own mock engine. This is the only test
// layer that proves catalog sync, materialization, the tunnel, the transit
// path and the serving gates actually fit together.
package fed_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/fed"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
	"llm-relay/internal/usage"
)

// ------------------------------------------------------------ mock engine

// engine is a stand-in llama.cpp: it answers chat completions with a body
// carrying its marker (so a test can prove WHICH engine served a request),
// optionally delays (to hold requests open), and records context
// cancellations (to prove CancelFrame reaches the serving side's upstream).
type engine struct {
	srv      *httptest.Server
	marker   string
	delay    time.Duration
	hits     atomic.Int32
	canceled atomic.Int32
}

func newEngine(t *testing.T, marker string, delay time.Duration) *engine {
	t.Helper()
	e := &engine{marker: marker, delay: delay}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"mock"}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &in)
		if e.delay > 0 {
			select {
			case <-time.After(e.delay):
			case <-r.Context().Done():
				e.canceled.Add(1)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","model":%q,`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"from-%s"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`, in.Model, e.marker)
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// ------------------------------------------------------------ relay harness

// safeBuf collects a Manager's log lines for assertions (log.Logger is
// concurrent-safe; the buffer behind it must be too).
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuf) has(sub string) bool { return strings.Contains(b.String(), sub) }

// relay is one full llm-relay instance minus the listening sockets: DB,
// proxy pipeline, fed.Manager, and an httptest server exposing its /v1
// surface (plus the hub's connect endpoint).
type relay struct {
	t    *testing.T
	name string
	db   *sql.DB
	mgr  *fed.Manager
	logs *safeBuf
	srv  *httptest.Server
	key  string // a client API key for /v1 calls
	uid  int64

	mu      sync.Mutex
	engines map[string]*engine // engines by model name, for hit counting
}

func newRelay(t *testing.T, name string) *relay {
	t.Helper()
	d := migratedDB(t)
	hash, _ := auth.HashPassword("x")
	u, err := store.CreateUser(d, name, "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	plain, kh, prefix, _ := auth.GenerateAPIKey()
	if _, err := store.CreateKey(d, u.ID, "t", kh, prefix); err != nil {
		t.Fatal(err)
	}
	rec := usage.NewRecorder(d)
	rec.Start()
	logs := &safeBuf{}
	ph := proxy.New(d, rec, false)
	mgr := fed.NewManager(d, &fed.Options{
		Log:             log.New(logs, "", 0),
		PingInterval:    500 * time.Millisecond,
		PongTimeout:     400 * time.Millisecond,
		HelloTimeout:    3 * time.Second,
		CatalogDebounce: 30 * time.Millisecond,
		BackoffMin:      40 * time.Millisecond,
		BackoffMax:      200 * time.Millisecond,
		StableAfter:     time.Hour,
		InstanceName:    name,
		Version:         "test",
	})
	ph.Fed = mgr
	ph.Health.Synthetic = mgr.SyntheticOnline
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", ph.Chat)
	mux.HandleFunc("GET /v1/models", ph.Models)
	mgr.AttachServe(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close() })
	t.Cleanup(func() { mgr.Close() })
	t.Cleanup(func() { rec.Close(2 * time.Second) })
	return &relay{t: t, name: name, db: d, mgr: mgr, logs: logs, srv: srv,
		key: plain, uid: u.ID, engines: map[string]*engine{}}
}

// becomeHub flips role=hub and re-reads settings.
func (r *relay) becomeHub() {
	r.t.Helper()
	if err := store.PutFederationSettings(r.db, store.FederationSettings{Role: store.FedRoleHub}); err != nil {
		r.t.Fatal(err)
	}
	r.mgr.Reload()
}

// enroll creates a peer (server token) on the hub, owned by the relay's
// own user.
func (r *relay) enroll(name string) (*store.Peer, string) {
	r.t.Helper()
	return r.enrollAs(name, r.uid)
}

// enrollAs creates a peer owned by a specific user (tests credit distinct
// owners to different spokes).
func (r *relay) enrollAs(name string, owner int64) (*store.Peer, string) {
	r.t.Helper()
	p, tok, err := store.CreatePeer(r.db, name, owner)
	if err != nil {
		r.t.Fatal(err)
	}
	return p, tok
}

// serveConnect mounts the hub's WS connect endpoint on its public mux.
func (r *relay) serveConnect() {
	r.t.Helper()
	r.srv.Config.Handler.(*http.ServeMux).HandleFunc("GET /federation/v1/connect",
		func(w http.ResponseWriter, req *http.Request) {
			tok := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			p, err := store.GetPeerByKeyHash(r.db, auth.HashKey(tok))
			if err != nil || !p.Enabled {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r.mgr.ServeHub(w, req, p)
		})
}

// becomeSpoke flips role=spoke pointed at hub and re-reads settings.
func (r *relay) becomeSpoke(hubURL, token string) {
	r.t.Helper()
	if err := store.PutFederationSettings(r.db, store.FederationSettings{
		Role: store.FedRoleSpoke, HubURL: hubURL, HubToken: token,
	}); err != nil {
		r.t.Fatal(err)
	}
	r.mgr.Reload()
}

// shareLocal creates a local backend+model on this relay and shares it
// with the federation (fed_visibility=all). Returns the model row.
func (r *relay) shareLocal(name, marker string) *store.Model {
	r.t.Helper()
	e := newEngine(r.t, marker, 0)
	return r.shareLocalEngine(name, e)
}

func (r *relay) shareLocalEngine(name string, e *engine) *store.Model {
	r.t.Helper()
	r.mu.Lock()
	r.engines[name] = e
	r.mu.Unlock()
	b, err := store.CreateBackend(r.db, "engine-"+name, e.srv.URL, "")
	if err != nil {
		r.t.Fatal(err)
	}
	m, err := store.CreateModel(r.db, b.ID, name+"-up", name)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := store.SetModelFedVisibility(r.db, m.ID, store.FedVisAll, nil); err != nil {
		r.t.Fatal(err)
	}
	// Mirror the API layer's hook: a sharing change re-triggers the
	// (debounced) catalog push.
	r.mgr.CatalogChanged()
	return m
}

// forgeSyntheticModel plants a model row on a synthetic backend without any
// catalog frame behind it — the stale/forged-catalog shape the serving
// gates must catch. kind is "hub" or a peer id string ("5").
func (r *relay) forgeSyntheticModel(kind, name string) {
	r.t.Helper()
	base := "hub://federation"
	if kind != "hub" {
		base = "peer://" + kind
	}
	var bid int64
	err := r.db.QueryRow(`SELECT id FROM backends WHERE base_url=?`, base).Scan(&bid)
	if err == sql.ErrNoRows {
		b, cerr := store.CreateBackend(r.db, "forged "+base, base, "")
		if cerr != nil {
			r.t.Fatal(cerr)
		}
		bid = b.ID
	} else if err != nil {
		r.t.Fatal(err)
	}
	if _, err := store.CreateModel(r.db, bid, name+"-up", name); err != nil {
		r.t.Fatal(err)
	}
}

// ------------------------------------------------------------ client helpers

func (r *relay) get(path string) (*http.Response, []byte) {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+r.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func (r *relay) chat(model, session string) (*http.Response, []byte) {
	return r.chatCtx(context.Background(), model, session)
}

func (r *relay) chatCtx(ctx context.Context, model, session string) (*http.Response, []byte) {
	r.t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello there friend"}]}`, model)
	req, _ := http.NewRequestWithContext(ctx, "POST", r.srv.URL+"/v1/chat/completions",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func modelsList(body []byte) string {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &out)
	names := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		names = append(names, d.ID)
	}
	return strings.Join(names, ",")
}

func poll(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func countRows(db *sql.DB, q string, args ...any) int {
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		return -1
	}
	return n
}

// tunnelReq injects one raw Req frame from the hub straight to peer peerID
// and returns the served status + body. This is the shape of a malicious or
// compromised hub naming whatever it likes at a spoke — no hub-side catalog
// or proxy path involved.
func (r *relay) tunnelReq(t *testing.T, peerID int64, model string) (int, string) {
	t.Helper()
	id := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	ch, release, err := r.mgr.Hub().OpenRequest(peerID, id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello there friend"}]}`, model)
	if err := r.mgr.Send(peerID, &fed.Req{T: fed.TReq, ID: id, Method: "POST",
		Path: "/v1/chat/completions", Body: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	var status int
	var text strings.Builder
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-ch:
			switch fr := f.(type) {
			case *fed.Res:
				status = fr.Status
			case *fed.ResBody:
				text.Write(fr.Data)
			case *fed.ResEnd:
				if fr.Error != "" {
					return status, fr.Error
				}
				return status, text.String()
			}
		case <-deadline:
			t.Fatalf("timeout waiting for tunnel response to %q", model)
		}
	}
}

// federationUp is the shared setup: hub + one spoke sharing model "alpha",
// returning the peer row and the spoke's local model.
func federationUp(t *testing.T) (hub, spoke *relay, peer *store.Peer) {
	t.Helper()
	hub = newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	spoke = newRelay(t, "spoke-a")
	spoke.shareLocal("alpha", "spoke-a")
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online at hub", func() bool { return hub.mgr.Online(peer.ID) })
	pollLog(t, spoke, hub)("alpha materialized at hub", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
			WHERE m.name='alpha' AND b.base_url=?`, store.PeerBackendURL(peer.ID)) == 1
	})
	return
}

// pollLog is poll plus a relay log dump on timeout (materialization
// failures are otherwise invisible: every step logs instead of erroring).
func pollLog(t *testing.T, relays ...*relay) func(string, func() bool) {
	return func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		for _, r := range relays {
			t.Logf("---- %s logs ----\n%s", r.name, r.logs.String())
		}
		t.Fatalf("timeout waiting for %s", what)
	}
}

// ------------------------------------------------------------ tests

// Advertise → materialize → visible in /v1/models → served through the
// tunnel end-to-end, metered double-entry: the hub meters its own client
// (consumed, counterparty = the hosting peer) and the spoke records what
// it served (served, fed_user = the hub-stamped consumer name).
func TestAdvertiseMaterializeServe(t *testing.T) {
	hub, spoke, peer := federationUp(t)

	resp, body := hub.get("/v1/models")
	if resp.StatusCode != 200 || !strings.Contains(modelsList(body), "alpha") {
		t.Fatalf("hub /v1/models = %d %s", resp.StatusCode, body)
	}

	resp, body = hub.chat("alpha", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-spoke-a") {
		t.Fatalf("tunneled chat = %d %s", resp.StatusCode, body)
	}

	poll(t, "hub usage pair", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='alpha'`) == 2
	})
	// The hub meters its OWN client normally (the client's key) and stamps
	// the row consumed: the hub's account spent the peer's capacity. The
	// synthetic federation key appears when a SPOKE asks the hub (see
	// TestNotSharedAbsentAndForbidden) — and on the hub's own mirrored
	// served rows.
	if n := countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE api_key_id=1
		AND fed_direction='consumed' AND fed_peer_id=?`, peer.ID); n != 1 {
		t.Errorf("hub metered %d consumed rows on the client key, want 1", n)
	}
	// The hub's double-entry mirror: the hosting peer's owner is credited
	// with serving the same counts (uniform convention: the served row's
	// fed_peer_id names the HOST it credits — the same peer the consumed
	// row names; fed_user names the consuming hub user).
	poll(t, "hub mirrored served row", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='alpha'
			AND fed_direction='served' AND fed_user='hub' AND fed_peer_id=?
			AND api_key_id=0 AND user_id=? AND prompt_tokens=10 AND completion_tokens=5`,
			peer.ID, hub.uid) == 1
	})
	// The spoke serves with metering (task 4): a served row for the
	// federation identity, the hub-stamped consumer name, real token
	// counts from the engine's usage payload, and no local peer row
	// (the hub has no peers entry on a spoke → fed_peer_id NULL).
	poll(t, "spoke served row", func() bool {
		return countRows(spoke.db, `SELECT COUNT(*) FROM usage_log WHERE fed_direction='served'
			AND fed_user='hub' AND fed_peer_id IS NULL AND prompt_tokens=10
			AND completion_tokens=5 AND model_requested='alpha'`) == 1
	})
	if n := countRows(spoke.db, `SELECT COUNT(*) FROM usage_log WHERE fed_direction=''`); n != 0 {
		t.Errorf("spoke recorded %d unstamped rows, want 0", n)
	}
	_ = peer
}

// A model the hub does not share never materializes on the spoke (404 for a
// typo'd name), and a forged materialized row for it is refused with 403
// at the hub's sharing gate — the stale/forged-catalog defense.
func TestNotSharedAbsentAndForbidden(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	// A hub-local model that stays unshared.
	hub.shareLocalEngine("secret", newEngine(t, "hub", 0))
	if err := store.SetModelFedVisibility(hub.db,
		mustModelID(t, hub.db, "secret"), store.FedVisOff, nil); err != nil {
		t.Fatal(err)
	}
	spoke := newRelay(t, "spoke-a")
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })

	if _, body := spoke.get("/v1/models"); strings.Contains(modelsList(body), "secret") {
		t.Fatalf("unshared model leaked to spoke catalog: %s", body)
	}
	resp, _ := spoke.chat("secret", "")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown model status = %d, want 404", resp.StatusCode)
	}

	spoke.forgeSyntheticModel("hub", "secret")
	resp, body := spoke.chat("secret", "")
	if resp.StatusCode != 403 || !strings.Contains(string(body), "not shared") {
		t.Fatalf("forged share = %d %s, want 403 not-shared", resp.StatusCode, body)
	}

	// The positive twin: a genuinely shared hub model materializes on the
	// spoke over hub://, is served by the hub, and the hub meters it on
	// the synthetic federation key against the peer's owner user, stamped
	// consumed with fed_peer_id NULL (hub hardware served — no peer to
	// credit; the spender is the row's user).
	hub.shareLocal("open", "hub")
	pollLog(t, spoke, hub)("open materialized on spoke", func() bool {
		return countRows(spoke.db, `SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
			WHERE m.name='open' AND b.base_url='hub://federation'`) == 1
	})
	resp, body = spoke.chat("open", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-hub") {
		t.Fatalf("spoke→hub chat = %d %s", resp.StatusCode, body)
	}
	poll(t, "hub metered the federation key", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM usage_log
			WHERE api_key_id=0 AND model_requested='open'
			AND fed_direction='consumed' AND fed_peer_id IS NULL`) == 1
	})
	// The spoke meters its own local user on the hub:// model as plain
	// local traffic (the consumed-slice is identified by the backend
	// scheme, not by fed columns — no double-stamping).
	poll(t, "spoke metered its local user", func() bool {
		return countRows(spoke.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='open'
			AND fed_direction=''`) == 1
	})
}

func mustModelID(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM models WHERE name=?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Reviewer round-1 (security): the sharing gate must also run on the SPOKE
// for hub-originated frames. The hub's own traffic arrives with
// OriginPeer=0 (a spoke has no peer rows), so the gate keys on LocalOnly
// and SharedWithPeer(model, 0): 'off' → 403, 'all' → served, and a
// federated group cannot smuggle a member that is 'off' on this spoke.
func TestSpokeServingSharingGate(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	// Hub side: a model shared 'all' inside a group — the spoke resolves
	// that member name to its OWN local copy (local-first), which is
	// fed_visibility='off' there.
	hub.shareLocal("pool-member", "hub")
	hm := mustModelID(t, hub.db, "pool-member")
	g, err := store.CreateGroup(hub.db, "pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(hub.db, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: hm, Weight: 1}}); err != nil {
		t.Fatal(err)
	}

	spoke := newRelay(t, "spoke-a")
	spoke.shareLocal("shared-all", "spoke-eng")
	// Two local models that stay fed_visibility='off' (the CreateModel
	// default): one the hub will name directly, one smuggled through the
	// federated group.
	privateEng := newEngine(t, "off-eng", 0)
	for name, eng := range map[string]*engine{"private": privateEng, "pool-member": newEngine(t, "off-eng2", 0)} {
		b, err := store.CreateBackend(spoke.db, "off-"+name, eng.srv.URL, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateModel(spoke.db, b.ID, name+"-up", name); err != nil {
			t.Fatal(err)
		}
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })
	pollLog(t, spoke, hub)("group pool materialized with the LOCAL pool-member", func() bool {
		var n int
		err := spoke.db.QueryRow(`SELECT COUNT(*) FROM model_group_members gm
			JOIN model_groups g ON g.id=gm.group_id
			JOIN models m ON m.id=gm.model_id
			JOIN backends b ON b.id=m.backend_id
			WHERE g.name='pool' AND m.name='pool-member'
			  AND b.base_url NOT LIKE 'hub://%' AND b.base_url NOT LIKE 'peer://%'`).Scan(&n)
		return err == nil && n == 1
	})

	// 1. 'off' model named directly → 403, and its engine is never touched.
	if status, body := hub.tunnelReq(t, peer.ID, "private"); status != 403 ||
		!strings.Contains(body, "not shared") {
		t.Fatalf("off-model tunnel = %d %s, want 403 not-shared", status, body)
	}
	if n := privateEng.hits.Load(); n != 0 {
		t.Errorf("off-model engine took %d hits, want 0", n)
	}
	// 2. The same model smuggled through the federated group: the 'off'
	// member is filtered, the pool empties → 403 at the gate.
	if status, body := hub.tunnelReq(t, peer.ID, "pool"); status != 403 ||
		!strings.Contains(body, "not shared") {
		t.Fatalf("off-member group tunnel = %d %s, want 403 not-shared", status, body)
	}
	// 3. Positive control: an 'all' model still serves through the tunnel.
	if status, body := hub.tunnelReq(t, peer.ID, "shared-all"); status != 200 ||
		!strings.Contains(body, "from-spoke-eng") {
		t.Fatalf("shared-model tunnel = %d %s, want 200 from spoke engine", status, body)
	}
}

// Spoke A reaches spoke B's model through the hub's transit path; the
// engine marker proves B served it, and the hub's fairness ledger carries
// the double-entry pair (consumed on A's owner, served on B's owner with
// A's owner as fed_user). B also meters its own served row with the
// hub-stamped identity — spoke-local names never left A.
func TestSpokeToSpokeTransit(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	pa, tokA := hub.enroll("spoke-a")
	pb, tokB := hub.enroll("spoke-b")
	hub.serveConnect()

	spokeB := newRelay(t, "spoke-b")
	spokeB.shareLocal("gamma", "spoke-b")
	spokeB.becomeSpoke(hub.srv.URL, tokB)
	poll(t, "B online", func() bool { return hub.mgr.Online(pb.ID) })

	spokeA := newRelay(t, "spoke-a")
	spokeA.becomeSpoke(hub.srv.URL, tokA)
	poll(t, "A online", func() bool { return hub.mgr.Online(pa.ID) })
	poll(t, "gamma relayed to A", func() bool {
		return countRows(spokeA.db, `SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
			WHERE m.name='gamma' AND b.base_url=?`, store.PeerBackendURL(pb.ID)) == 1
	})

	resp, body := spokeA.chat("gamma", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-spoke-b") {
		t.Fatalf("transit chat = %d %s", resp.StatusCode, body)
	}
	if n := spokeB.engineHits("gamma"); n != 1 {
		t.Errorf("B engine hits = %d, want 1", n)
	}
	// Hub ledger: consumed (A's owner spent B's capacity)…
	poll(t, "hub consumed row", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='gamma'
			AND fed_direction='consumed' AND fed_peer_id=? AND prompt_tokens=10
			AND completion_tokens=5`, pb.ID) == 1
	})
	// …and served (B's owner gave it — the served row credits the HOST B,
	// attributed to A's owner by name).
	poll(t, "hub served row", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='gamma'
			AND fed_direction='served' AND fed_peer_id=? AND fed_user='hub'`, pb.ID) == 1
	})
	// B's own box: served row with the hub-stamped identity (A's owner
	// name — never A's local username), no local peer row for the hub.
	poll(t, "B served row", func() bool {
		return countRows(spokeB.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='gamma'
			AND fed_direction='served' AND fed_user='hub' AND fed_peer_id IS NULL
			AND prompt_tokens=10 AND completion_tokens=5`) == 1
	})
	// A's own box: its local user's row stays plain (consumed-slice is
	// identified by the peer:// backend scheme, not by fed columns).
	poll(t, "A local row unstamped", func() bool {
		return countRows(spokeA.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='gamma'
			AND fed_direction=''`) == 1
	})
}

func (r *relay) engineHits(model string) int {
	r.mu.Lock()
	e := r.engines[model]
	r.mu.Unlock()
	if e == nil {
		return -1
	}
	return int(e.hits.Load())
}

// Spoke A asks the hub to SERVE-HERE a real hub ADMIN GROUP whose members
// are a hub-local model and a synthetic peer://B row (the store guard that
// used to reject synthetic members is gone by design): the hub's pipeline
// resolves the group to the synthetic member and tunnels to B, so the
// ledger carries the full pair — consumed on A's owner with the HOSTING
// peer B as counterparty, and a mirrored served row crediting B's owner
// (counterparty = A, fed_user = A's owner name). Distinct owners (hub vs
// bob) prove the credit lands on the right account.
func TestHubGroupServeDoubleEntry(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	hash, _ := auth.HashPassword("x")
	bob, err := store.CreateUser(hub.db, "bob", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	pa, tokA := hub.enroll("spoke-a") // owner: the hub's own user
	pb, tokB := hub.enrollAs("spoke-b", bob.ID)
	hub.serveConnect()

	spokeB := newRelay(t, "spoke-b")
	spokeB.shareLocal("gamma", "spoke-b")
	spokeB.becomeSpoke(hub.srv.URL, tokB)
	poll(t, "B online", func() bool { return hub.mgr.Online(pb.ID) })

	// A real admin group: hub-local member + the synthetic peer://B member.
	// The local member is then disabled so the walk deterministically
	// lands on the synthetic one (the point of this test).
	local := hub.shareLocal("hubone", "hub-eng")
	gammaID := mustModelID(t, hub.db, "gamma")
	g, err := store.CreateGroup(hub.db, "mix")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(hub.db, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: local.ID, Weight: 1}, {ModelID: gammaID, Weight: 1}}); err != nil {
		t.Fatalf("admin group with synthetic member: %v", err)
	}
	if _, err := store.UpdateModel(hub.db, local.ID, nil, ptrBool(false), [4]*int64{}); err != nil {
		t.Fatal(err)
	}

	// A (raw spoke client) asks the hub to serve-here the group name; the
	// only eligible member is the synthetic peer://B row.
	ws := rawSpoke(t, hub.srv.URL, tokA)
	status, text := rawReq(t, ws, &fed.Req{T: fed.TReq, ID: "syn-1", Method: "POST",
		Path: "/v1/chat/completions", Body: chatBody("mix")})
	if status != 200 || !strings.Contains(text, "from-spoke-b") {
		t.Fatalf("serve-here group = %d %s", status, text)
	}
	// consumed: A's owner spent, counterparty = the HOSTING peer B.
	poll(t, "hub consumed row (host peer)", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='mix'
			AND fed_direction='consumed' AND fed_peer_id=? AND user_id=?`, pb.ID, hub.uid) == 1
	})
	// mirrored served: B's owner credited, HOST B as counterparty (the
	// uniform host stamp) and A's owner name as fed_user, same counts, the
	// synthetic member row.
	poll(t, "hub mirrored served row", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='mix'
			AND fed_direction='served' AND fed_peer_id=? AND fed_user='hub' AND user_id=?
			AND model_id=? AND prompt_tokens=10 AND completion_tokens=5`,
			pb.ID, bob.ID, gammaID) == 1
	})
	// Exactly the pair — one request, one double-entry.
	if n := countRows(hub.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='mix'`); n != 2 {
		t.Errorf("hub ledger = %d rows, want exactly 2 (consumed + served)", n)
	}
	// B's own box meters its served row too (hub-stamped consumer identity).
	poll(t, "B served row", func() bool {
		return countRows(spokeB.db, `SELECT COUNT(*) FROM usage_log WHERE model_requested='gamma'
			AND fed_direction='served' AND fed_user='hub'`) == 1
	})
	// The rollups reflect the pair.
	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	totals, err := store.FedUserTotals(hub.db, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	byUser := map[string]store.FedUserTotal{}
	for _, e := range totals {
		byUser[e.User] = e
	}
	if byUser["hub"].Consumed.Prompt != 10 || byUser["bob"].Served.Prompt != 10 {
		t.Errorf("user totals = %+v", totals)
	}
	peers, err := store.FedPeerTotals(hub.db, since)
	if err != nil {
		t.Fatal(err)
	}
	byPeer := map[int64]store.FedPeerTotal{}
	for _, e := range peers {
		byPeer[e.PeerID] = e
	}
	// Uniform buckets: B's served = the mirror crediting its hardware;
	// A's consumed = owner-level (A's owner — the hub user — spent 10).
	// The other two corners stay empty: A served nothing, and bob's
	// account spent nothing.
	if byPeer[pb.ID].Served.Prompt != 10 || byPeer[pa.ID].Consumed.Prompt != 10 ||
		byPeer[pa.ID].Served.Prompt != 0 || byPeer[pb.ID].Consumed.Prompt != 0 {
		t.Errorf("peer totals = %+v", peers)
	}
}

// The synthetic-member lifecycle inside an admin group (the C6
// correction): a peer disconnect only DISABLES the backend — the group
// keeps serving from its local member — and a catalog prune of the
// synthetic row clears the membership and leaves the group intact and
// consistent in /v1/models.
func TestAdminGroupSyntheticLifecycle(t *testing.T) {
	hub, spoke, peer := federationUp(t) // spoke shares "alpha"
	local := hub.shareLocal("localone", "hub-eng")
	alphaID := mustModelID(t, hub.db, "alpha")
	g, err := store.CreateGroup(hub.db, "mix")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(hub.db, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: local.ID, Weight: 1}, {ModelID: alphaID, Weight: 1}}); err != nil {
		t.Fatalf("admin group with synthetic member: %v", err)
	}
	// Sanity: the group serves while both members are eligible.
	if resp, body := hub.chat("mix", ""); resp.StatusCode != 200 {
		t.Fatalf("group chat = %d %s", resp.StatusCode, body)
	}
	// (a) Peer disconnect: the synthetic backend goes disabled, the member
	// is excluded — the group keeps serving from the local member.
	before := spoke.engineHits("alpha")
	if err := store.SetPeerEnabled(hub.db, peer.ID, false); err != nil {
		t.Fatal(err)
	}
	hub.mgr.KickPeer(peer.ID, "test disconnect")
	poll(t, "alpha backend offline", func() bool {
		var en int
		err := hub.db.QueryRow(`SELECT enabled FROM backends WHERE base_url=?`,
			store.PeerBackendURL(peer.ID)).Scan(&en)
		return err == nil && en == 0
	})
	resp, body := hub.chat("mix", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-hub-eng") {
		t.Fatalf("group chat while peer offline = %d %s, want local serve", resp.StatusCode, body)
	}
	if n := spoke.engineHits("alpha"); n != before {
		t.Errorf("offline peer's engine took %d extra hits, want 0", n-before)
	}
	// (b) Back online, then the spoke un-shares alpha: the hub's next
	// frame prunes the row — membership cleared before the delete, group
	// intact with its remaining member, catalog consistent.
	if err := store.SetPeerEnabled(hub.db, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	poll(t, "peer back online", func() bool { return hub.mgr.Online(peer.ID) })
	if err := store.SetModelFedVisibility(spoke.db, mustModelID(t, spoke.db, "alpha"),
		store.FedVisOff, nil); err != nil {
		t.Fatal(err)
	}
	spoke.mgr.CatalogChanged()
	poll(t, "alpha pruned on hub", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM models WHERE name='alpha'`) == 0
	})
	if n := countRows(hub.db, `SELECT COUNT(*) FROM model_group_members WHERE model_id=?`, alphaID); n != 0 {
		t.Errorf("pruned synthetic member left %d membership rows behind", n)
	}
	if n := countRows(hub.db, `SELECT COUNT(*) FROM model_group_members WHERE group_id=?`, g.ID); n != 1 {
		t.Errorf("group members after prune = %d, want 1 (local member intact)", n)
	}
	_, body = hub.get("/v1/models")
	if !strings.Contains(modelsList(body), "mix") || strings.Contains(modelsList(body), "alpha") {
		t.Errorf("/v1/models after prune = %s, want mix without alpha", body)
	}
	if resp, body := hub.chat("mix", ""); resp.StatusCode != 200 ||
		!strings.Contains(string(body), "from-hub-eng") {
		t.Fatalf("group chat after prune = %d %s", resp.StatusCode, body)
	}
}

// The hub never routes a request back to its origin: a spoke whose catalog
// (mis)names its own peer backend gets a dial failure, not a loop.
func TestLoopGuard(t *testing.T) {
	_, spoke, peer := federationUp(t)
	spoke.forgeSyntheticModel(fmt.Sprint(peer.ID), "selfloop")
	resp, _ := spoke.chat("selfloop", "")
	if resp.StatusCode != 502 {
		t.Fatalf("self-routed request status = %d, want 502", resp.StatusCode)
	}
	// The rejection is a dial-class failure (the ResEnd error surfaces as
	// the proxy's "upstream unavailable"); the request never reached any
	// engine as a second hop.
}

// A dead channel disables the materialized backend (the model vanishes
// from /v1/models) and the reconnect brings the SAME model ids back —
// usage_log references survive a federation flap.
func TestDisconnectVanishReconnectReturn(t *testing.T) {
	hub, spoke, peer := federationUp(t)
	var idBefore int64
	if err := hub.db.QueryRow(`SELECT id FROM models WHERE name='alpha'`).Scan(&idBefore); err != nil {
		t.Fatal(err)
	}

	hub.mgr.KickPeer(peer.ID, "test-flap")
	poll(t, "peer offline at hub", func() bool { return !hub.mgr.Online(peer.ID) })
	poll(t, "alpha vanishes from hub catalog", func() bool {
		_, body := hub.get("/v1/models")
		return !strings.Contains(modelsList(body), "alpha")
	})

	poll(t, "peer reconnects", func() bool { return hub.mgr.Online(peer.ID) })
	poll(t, "alpha returns", func() bool {
		_, body := hub.get("/v1/models")
		return strings.Contains(modelsList(body), "alpha")
	})
	var idAfter int64
	if err := hub.db.QueryRow(`SELECT id FROM models WHERE name='alpha'`).Scan(&idAfter); err != nil {
		t.Fatal(err)
	}
	if idAfter != idBefore {
		t.Errorf("model id changed across reconnect: %d -> %d", idBefore, idAfter)
	}
	resp, body := hub.chat("alpha", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-spoke-a") {
		t.Fatalf("post-reconnect chat = %d %s", resp.StatusCode, body)
	}
	_ = spoke
}

// A client hanging up mid-generation reaches the serving side's engine as
// a context cancellation (CancelFrame), not a completed generation.
func TestCancelAbortsUpstream(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	spoke := newRelay(t, "spoke-a")
	e := newEngine(t, "spoke-a", 2*time.Second)
	b, err := store.CreateBackend(spoke.db, "slow", e.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(spoke.db, b.ID, "slow-up", "slow")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelFedVisibility(spoke.db, m.ID, store.FedVisAll, nil); err != nil {
		t.Fatal(err)
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })
	poll(t, "slow materialized", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM models WHERE name='slow'`) == 1
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	req, _ := http.NewRequestWithContext(ctx, "POST", hub.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"slow","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+hub.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	poll(t, "engine observed cancellation", func() bool { return e.canceled.Load() == 1 })
	if e.hits.Load() != 1 {
		t.Errorf("engine hits = %d, want 1", e.hits.Load())
	}
}

// The per-peer in-flight cap (16) surfaces as a dial failure: the 17th
// concurrent request to a saturated spoke fails instead of queueing.
func TestInflightCapFailsOver(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	spoke := newRelay(t, "spoke-a")
	e := newEngine(t, "spoke-a", 700*time.Millisecond)
	b, err := store.CreateBackend(spoke.db, "slow", e.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(spoke.db, b.ID, "slow-up", "slow")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelFedVisibility(spoke.db, m.ID, store.FedVisAll, nil); err != nil {
		t.Fatal(err)
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })
	poll(t, "slow materialized", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM models WHERE name='slow'`) == 1
	})

	const n = fed.MaxInflight + 1
	var wg sync.WaitGroup
	var ok, bad atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := hub.chat("slow", "")
			if resp.StatusCode == 200 {
				ok.Add(1)
			} else {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() == 0 {
		t.Errorf("all %d requests passed a saturated channel (ok=%d): the cap never bit", n, ok.Load())
	}
	if int(ok.Load()) > fed.MaxInflight {
		t.Errorf("%d concurrent streams exceeded the cap of %d", ok.Load(), fed.MaxInflight)
	}
}

// A shared model whose name collides with a local public model is skipped
// with a logged warning — the local row keeps serving its own backend.
func TestCollisionSkipsAndLogs(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	hub.shareLocal("dup", "hub")

	spoke := newRelay(t, "spoke-a")
	local := newEngine(t, "spoke-local", 0)
	sb, err := store.CreateBackend(spoke.db, "local", local.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	lm, err := store.CreateModel(spoke.db, sb.ID, "dup-up", "dup")
	if err != nil {
		t.Fatal(err)
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })
	poll(t, "collision logged", func() bool { return spoke.logs.has(`skipping shared model "dup"`) })

	var backendID int64
	if err := spoke.db.QueryRow(`SELECT backend_id FROM models WHERE name='dup'`).Scan(&backendID); err != nil {
		t.Fatal(err)
	}
	if backendID != sb.ID {
		t.Errorf("local dup moved to backend %d, want %d", backendID, sb.ID)
	}
	if lm.Name != "dup" {
		t.Fatal("local model vanished")
	}
}

// The hub's groups ride the catalog: on a spoke whose local model shares a
// member name, the federated group resolves to the LOCAL row (local-first
// falls out of HRW: the group contains the local row, sessions hash
// locally) and is served from the spoke's own engine.
func TestFederatedGroupLocalFirst(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	hub.shareLocal("m1", "hub-eng")
	hm := mustModelID(t, hub.db, "m1")
	g, err := store.CreateGroup(hub.db, "pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(hub.db, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: hm, Weight: 1}}); err != nil {
		t.Fatal(err)
	}

	spoke := newRelay(t, "spoke-a")
	local := newEngine(t, "spoke-eng", 0)
	sb, err := store.CreateBackend(spoke.db, "local", local.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	lm, err := store.CreateModel(spoke.db, sb.ID, "m1-up", "m1")
	if err != nil {
		t.Fatal(err)
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })
	poll(t, "federated group materialized", func() bool {
		return countRows(spoke.db, `SELECT COUNT(*) FROM model_groups WHERE name='pool' AND federated=1`) == 1
	})

	var memberModel int64
	err = spoke.db.QueryRow(`SELECT gm.model_id FROM model_group_members gm
		JOIN model_groups g ON g.id=gm.group_id WHERE g.name='pool'`).Scan(&memberModel)
	if err != nil {
		t.Fatal(err)
	}
	if memberModel != lm.ID {
		t.Errorf("group member = %d, want local row %d (local-first)", memberModel, lm.ID)
	}

	resp, body := spoke.chat("pool", "sess-1")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-spoke-eng") {
		t.Fatalf("group chat = %d %s", resp.StatusCode, body)
	}
}

func ptrBool(b bool) *bool { return &b }
