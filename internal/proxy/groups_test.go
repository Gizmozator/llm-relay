// Group routing end-to-end: one mock engine per member, real SQLite, real
// recorder — asserting session affinity, weighted spread, hard exclusions,
// catalog shape, metering and the pre-first-byte retry.
package proxy_test

import (
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
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
	"llm-relay/internal/usage"
)

// memberEngine is a mock engine that stamps its name into the reply body so
// tests can tell which member served (the relay forwards the body, not
// upstream headers). /health answers 200 so the HealthCache probe passes.
func memberEngine(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"from-%s"}}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":2}}`, name)
	})
}

// stamp extracts the serving member's name from a mock reply body.
func stamp(body string) string {
	i := strings.Index(body, "from-")
	if i < 0 {
		return ""
	}
	rest := body[i+len("from-"):]
	if j := strings.IndexAny(rest, `"' `); j >= 0 {
		return rest[:j]
	}
	return rest
}

func ptrTrue() *bool  { v := true; return &v }
func ptrFalse() *bool { v := false; return &v }

type groupHarness struct {
	db     *sql.DB
	key    string
	server *httptest.Server
	ph     *proxy.Handler
}

func newGroupHarness(t *testing.T) *groupHarness {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
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
	rec := usage.NewRecorder(d)
	rec.Start()
	t.Cleanup(func() { rec.Close(2 * time.Second) })
	gh := &groupHarness{db: d, key: plain}
	gh.ph = proxy.New(d, rec, false)
	gh.server = httptest.NewServer(gh.ph.Mux())
	t.Cleanup(gh.server.Close)
	return gh
}

// addMember registers a backend with one model on its own mock engine and
// returns both: tests may close the server to simulate an outage.
func (gh *groupHarness) addMember(t *testing.T, backendName, modelName string) (*store.Model, *httptest.Server) {
	t.Helper()
	return gh.addMemberWith(t, backendName, modelName, memberEngine(backendName))
}

// addMemberWith is addMember over a custom handler (tests that block or
// inspect upstream traffic).
func (gh *groupHarness) addMemberWith(t *testing.T, backendName, modelName string, handler http.Handler) (*store.Model, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	b, err := store.CreateBackend(gh.db, backendName, up.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(gh.db, b.ID, "up-"+modelName, modelName)
	if err != nil {
		t.Fatal(err)
	}
	return m, up
}

// makeGroup creates a group over the given members and enables it (groups
// are born disabled; enabling requires at least one member).
func (gh *groupHarness) makeGroup(t *testing.T, name string, members []store.GroupMemberInput) {
	t.Helper()
	g, err := store.CreateGroup(gh.db, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(gh.db, g.ID, nil, ptrTrue(), members); err != nil {
		t.Fatal(err)
	}
}

// chat posts a conversation-shaped request (so a session derives even
// without the header) and returns the status plus the raw reply body.
func (gh *groupHarness) chat(t *testing.T, model, session string) (int, string) {
	t.Helper()
	return gh.chatBody(t, fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model), session)
}

func (gh *groupHarness) chatBody(t *testing.T, body, session string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gh.server.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+gh.key)
	if session != "" {
		req.Header.Set("X-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(all)
}

// usageRow waits until the recorder has flushed at least wantRows rows and
// returns the newest row's (model_id, model_requested) — the pair the group
// contract pins. wantRows > 1 is needed when an earlier request already left
// a row: without it the poll would return that stale row instantly.
func (gh *groupHarness) usageRow(t *testing.T, wantRows int) (int64, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := gh.db.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= wantRows {
			var modelID int64
			var requested string
			if err := gh.db.QueryRow(`SELECT model_id, model_requested FROM usage_log ORDER BY id DESC LIMIT 1`).
				Scan(&modelID, &requested); err != nil {
				t.Fatal(err)
			}
			return modelID, requested
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage rows = %d, want >= %d", n, wantRows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGroupAffinityAndWeightedSpread(t *testing.T) {
	gh := newGroupHarness(t)
	mA, _ := gh.addMember(t, "node-a", "m-a")
	mB, _ := gh.addMember(t, "node-b", "m-b")
	gh.makeGroup(t, "pool", []store.GroupMemberInput{
		{ModelID: mA.ID, Weight: 3}, {ModelID: mB.ID, Weight: 1},
	})

	// Same session across N requests: always the same member.
	first := ""
	for i := 0; i < 5; i++ {
		st, body := gh.chat(t, "pool", "fixed-session")
		if st != 200 {
			t.Fatalf("status = %d, want 200 (%s)", st, body)
		}
		who := stamp(body)
		if first == "" {
			first = who
		} else if who != first {
			t.Fatalf("session affinity broke: %s -> %s", first, who)
		}
	}

	// Distinct sessions spread ~proportional to weight (3:1).
	counts := map[string]int{}
	for i := 0; i < 240; i++ {
		st, body := gh.chat(t, "pool", fmt.Sprintf("s-%d", i))
		if st != 200 {
			t.Fatalf("status = %d, want 200 (%s)", st, body)
		}
		counts[stamp(body)]++
	}
	if counts["node-a"] == 0 || counts["node-b"] == 0 {
		t.Fatalf("spread = %+v, want both members used", counts)
	}
	if r := float64(counts["node-a"]) / float64(counts["node-b"]); r < 1.8 || r > 4.5 {
		t.Errorf("weight ratio: node-a=%d node-b=%d ratio=%.2f, want ~3", counts["node-a"], counts["node-b"], r)
	}
}

func TestGroupSkipsDisabledAndUnhealthyMembers(t *testing.T) {
	gh := newGroupHarness(t)
	mA, _ := gh.addMember(t, "node-a", "m-a")
	mB, _ := gh.addMember(t, "node-b", "m-b")
	// Dead backend: a server that was closed, so /health gets refused.
	dead := httptest.NewServer(memberEngine("node-c"))
	deadURL := dead.URL
	dead.Close()
	bC, err := store.CreateBackend(gh.db, "node-c", deadURL, "")
	if err != nil {
		t.Fatal(err)
	}
	mC, err := store.CreateModel(gh.db, bC.ID, "up-c", "m-c")
	if err != nil {
		t.Fatal(err)
	}
	gh.makeGroup(t, "pool", []store.GroupMemberInput{
		{ModelID: mA.ID}, {ModelID: mB.ID}, {ModelID: mC.ID},
	})

	// Sweep: the unhealthy member never serves; both healthy ones do
	// (proving the sweep actually covers each, so the next assertions bite).
	seen := map[string]int{}
	for i := 0; i < 24; i++ {
		st, body := gh.chat(t, "pool", fmt.Sprintf("s-%d", i))
		if st != 200 {
			t.Fatalf("status = %d, want 200 (%s)", st, body)
		}
		who := stamp(body)
		if who == "node-c" {
			t.Fatal("unhealthy member served")
		}
		seen[who]++
	}
	if seen["node-a"] == 0 || seen["node-b"] == 0 {
		t.Fatalf("sweep too narrow to test exclusions: %+v", seen)
	}

	// A disabled member model is skipped too.
	if _, err := store.UpdateModel(gh.db, mA.ID, nil, ptrFalse(), [4]*int64{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		_, body := gh.chat(t, "pool", fmt.Sprintf("d-%d", i))
		if stamp(body) == "node-a" {
			t.Fatal("disabled member served")
		}
	}
}

func TestGroupDraining(t *testing.T) {
	gh := newGroupHarness(t)
	mA, _ := gh.addMember(t, "node-a", "m-a")
	mB, _ := gh.addMember(t, "node-b", "m-b")
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})

	// One draining member is skipped, the group keeps serving.
	if _, err := store.UpdateBackend(gh.db, mA.BackendID, nil, nil, nil, nil, ptrTrue()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		st, body := gh.chat(t, "pool", fmt.Sprintf("s-%d", i))
		if st != 200 {
			t.Fatalf("status = %d, want 200 (%s)", st, body)
		}
		if stamp(body) == "node-a" {
			t.Fatal("draining member served")
		}
	}

	// All members draining: 503 with the drain-shaped body + Retry-After.
	if _, err := store.UpdateBackend(gh.db, mB.BackendID, nil, nil, nil, nil, ptrTrue()); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, gh.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"pool","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+gh.key)
	req.Header.Set("X-Session-Id", "s-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q, want 60", resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(string(body), "pool") {
		t.Errorf("error body should name the group: %s", body)
	}
}

func TestGroupCatalogAndDirectMember(t *testing.T) {
	gh := newGroupHarness(t)
	mA, _ := gh.addMember(t, "node-a", "m-a")
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}})
	// A plain public model on another backend stays listed.
	bSolo, err := store.CreateBackend(gh.db, "node-solo", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(gh.db, bSolo.ID, "up-s", "solo"); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, gh.server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+gh.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, e := range out.Data {
		ids[e.ID] = true
	}
	if !ids["pool"] {
		t.Errorf("catalog = %v, want the group name listed", ids)
	}
	if !ids["solo"] {
		t.Errorf("catalog = %v, want the plain model listed", ids)
	}
	if ids["m-a"] {
		t.Errorf("catalog = %v, internal member must stay hidden", ids)
	}

	// The member name still resolves directly (TargetModel path).
	st, body := gh.chat(t, "m-a", "direct")
	if st != 200 || stamp(body) != "node-a" {
		t.Errorf("direct member request: status %d body %s, want 200 from node-a", st, body)
	}
}

func TestGroupUsageMetering(t *testing.T) {
	gh := newGroupHarness(t)
	mA, _ := gh.addMember(t, "node-a", "m-a")
	mB, _ := gh.addMember(t, "node-b", "m-b")
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})

	st, body := gh.chat(t, "pool", "sess-meter")
	if st != 200 {
		t.Fatalf("status = %d, want 200 (%s)", st, body)
	}
	wantID := mA.ID
	if stamp(body) == "node-b" {
		wantID = mB.ID
	}
	// The row carries the physical member id but the GROUP name as the
	// requested model (per-pool aggregation keys on model_requested).
	modelID, requested := gh.usageRow(t, 1)
	if modelID != wantID {
		t.Errorf("usage model_id = %d, want %d (the serving member)", modelID, wantID)
	}
	if requested != "pool" {
		t.Errorf("model_requested = %q, want the group name", requested)
	}
}

func TestGroupEmptySessionLeastLoaded(t *testing.T) {
	gh := newGroupHarness(t)
	mA, _ := gh.addMember(t, "node-a", "m-a")
	mB, _ := gh.addMember(t, "node-b", "m-b")
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})

	// Self-exclusion pin: the request's own provisional stream is excluded
	// from its own ranking, so with everything idle all counts are zero
	// and the position tie-break picks the FIRST member. Siblings still
	// count — see the primed phase below and the concurrency test.
	st, body := gh.chatBody(t, `{"model":"pool","messages":[{"role":"system","content":"x"}]}`, "")
	if st != 200 {
		t.Fatalf("status = %d, want 200 (%s)", st, body)
	}
	if got := stamp(body); got != "node-a" {
		t.Errorf("idle session-less pick served by %s, want node-a (self-count excluded, tie → first member)", got)
	}

	// Prime: one more in-flight stream on member A (never leaves). A body
	// with no user message derives no session, so the pick is least-loaded
	// → B.
	gh.ph.Flight.Enter(proxy.Stream{ModelID: mA.ID, Model: "m-a", User: "bob", Key: "test", Endpoint: "chat"})
	st, body = gh.chatBody(t, `{"model":"pool","messages":[{"role":"system","content":"x"}]}`, "")
	if st != 200 {
		t.Fatalf("status = %d, want 200 (%s)", st, body)
	}
	if got := stamp(body); got != "node-b" {
		t.Errorf("empty-session pick served by %s, want node-b (least loaded)", got)
	}
}

func TestGroupRetryNextMemberPreFirstByte(t *testing.T) {
	gh := newGroupHarness(t)
	mA, upA := gh.addMember(t, "node-a", "m-a")
	mB, upB := gh.addMember(t, "node-b", "m-b")
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})

	// First request: both alive. It also warms the health cache for both
	// members, so the outage below stays invisible to selection and the
	// request really does reach the dead member before retrying.
	st, body := gh.chat(t, "pool", "pin")
	if st != 200 {
		t.Fatalf("status = %d, want 200 (%s)", st, body)
	}
	pinned := stamp(body)
	var dead *httptest.Server
	liveName := "node-b"
	if pinned == "node-a" {
		dead = upA
	} else {
		dead = upB
		liveName = "node-a"
	}
	dead.Close()

	// Same session: HRW still ranks the dead member first; the dial fails
	// pre-first-byte and the next ranked member serves.
	st, body = gh.chat(t, "pool", "pin")
	if st != 200 {
		t.Fatalf("retry status = %d, want 200 from the next member (%s)", st, body)
	}
	if got := stamp(body); got != liveName {
		t.Errorf("retry served by %s, want %s", got, liveName)
	}
	// Metering follows the member that actually served.
	wantID := mB.ID
	if liveName == "node-a" {
		wantID = mA.ID
	}
	modelID, requested := gh.usageRow(t, 2)
	if modelID != wantID || requested != "pool" {
		t.Errorf("retry usage row = (%d, %q), want (%d, pool)", modelID, requested, wantID)
	}
}

// HRW keys on the member's model id, so two members sharing ONE backend
// still split sessions ~50/50. With backend-id keying both members hash
// byte-identically and one member takes everything forever.
func TestGroupSameBackendSplit(t *testing.T) {
	gh := newGroupHarness(t)
	// One engine for both members; it echoes the upstream model it was
	// called with, so the members stay distinguishable.
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		upstream, _ := body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"from-%s"}}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":2}}`, upstream)
	})
	up := httptest.NewServer(echo)
	t.Cleanup(up.Close)
	b, err := store.CreateBackend(gh.db, "node-a", up.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := store.CreateModel(gh.db, b.ID, "up-1", "m-1")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(gh.db, b.ID, "up-2", "m-2")
	if err != nil {
		t.Fatal(err)
	}
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: m1.ID}, {ModelID: m2.ID}})
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		st, body := gh.chat(t, "pool", fmt.Sprintf("s-%d", i))
		if st != 200 {
			t.Fatalf("status = %d, want 200 (%s)", st, body)
		}
		counts[stamp(body)]++ // "up-1" / "up-2"
	}
	if counts["up-1"] < 30 || counts["up-2"] < 30 {
		t.Errorf("same-backend split = %+v, want ~50/50", counts)
	}
}

// After a pre-first-byte retry the LIVE InFlight entry must show the member
// that actually served, not the failed one the request started on — the
// admin live view and CountModel read it for the request's whole life.
func TestGroupRetryUpdatesInFlight(t *testing.T) {
	gh := newGroupHarness(t)
	// Both engines answer normally once (the warm-up), then block: headers
	// flush immediately (so the relay's Do returns and SetModel runs) but
	// the body waits for release, keeping the stream in-flight while the
	// test inspects it.
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blocked := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
				return
			}
			if calls.Add(1) == 1 {
				memberEngine(name).ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			entered <- struct{}{}
			<-release
			fmt.Fprintf(w, `{"choices":[{"message":{"content":"from-%s"}}],`+
				`"usage":{"prompt_tokens":5,"completion_tokens":2}}`, name)
		})
	}
	mA, upA := gh.addMemberWith(t, "node-a", "m-a", blocked("node-a"))
	mB, upB := gh.addMemberWith(t, "node-b", "m-b", blocked("node-b"))
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})

	st, body := gh.chat(t, "pool", "pin")
	if st != 200 {
		t.Fatalf("warm-up status = %d (%s)", st, body)
	}
	pinned := stamp(body)
	liveName, liveModel, liveID := "node-b", "m-b", mB.ID
	if pinned == "node-b" {
		liveName, liveModel, liveID = "node-a", "m-a", mA.ID
		upB.Close()
	} else {
		upA.Close()
	}

	type res struct {
		st   int
		body string
	}
	done := make(chan res, 1)
	go func() { s, b := gh.chat(t, "pool", "pin"); done <- res{s, b} }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("live member never reached — retry did not happen?")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var shown string
		var shownID int64
		for _, s := range gh.ph.Flight.List(0) {
			if s.Session == "pin" {
				shown, shownID = s.Model, s.ModelID
			}
		}
		if shown == liveModel && shownID == liveID {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("in-flight stream during retry shows %q/%d, want %s/%d (the serving member)",
				shown, shownID, liveModel, liveID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	r := <-done
	if r.st != 200 || stamp(r.body) != liveName {
		t.Errorf("held request finished: status %d stamp %q, want 200/%s", r.st, stamp(r.body), liveName)
	}
}

// A member whose base_url an admin edited into garbage must not abort the
// fan-out with a 500: build failures fan out like dial failures, and the
// all-failed path keeps its metered 502.
func TestGroupMalformedURLFailsOver(t *testing.T) {
	gh := newGroupHarness(t)
	mA, upA := gh.addMember(t, "node-a", "m-a")
	mB, _ := gh.addMember(t, "node-b", "m-b")
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})
	// Warm the health cache for both while both are reachable, so the
	// outage + bad URL below stay invisible to selection (the request
	// really does reach the broken members).
	if st, _ := gh.chat(t, "pool", "warm"); st != 200 {
		t.Fatalf("warm-up status = %d", st)
	}
	upA.Close()
	bad := "://bad"
	if _, err := store.UpdateBackend(gh.db, mB.BackendID, nil, &bad, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	st, body := gh.chat(t, "pool", "warm")
	if st != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", st, body)
	}
	// Metered: the failure row exists (warm-up row + failure row).
	if _, requested := gh.usageRow(t, 2); requested != "pool" {
		t.Errorf("failure row model_requested = %q, want pool", requested)
	}
}

// A burst of concurrent session-less requests must spread across members:
// each request enters InFlight before ranking, so siblings are visible in
// the load counts instead of every request reading identical counts.
func TestGroupEmptySessionConcurrentSpread(t *testing.T) {
	gh := newGroupHarness(t)
	const n = 8
	var arrived atomic.Int32
	release := make(chan struct{})
	slow := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
				return
			}
			if arrived.Add(1) == n {
				close(release)
			}
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
			memberEngine(name).ServeHTTP(w, r)
		})
	}
	mA, _ := gh.addMemberWith(t, "node-a", "m-a", slow("node-a"))
	mB, _ := gh.addMemberWith(t, "node-b", "m-b", slow("node-b"))
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})

	body := `{"model":"pool","messages":[{"role":"system","content":"x"}]}`
	var wg sync.WaitGroup
	var mu sync.Mutex
	stamps := make([]string, 0, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, gh.server.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+gh.key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				mu.Lock()
				stamps = append(stamps, "error")
				mu.Unlock()
				return
			}
			all, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			mu.Lock()
			stamps = append(stamps, stamp(string(all)))
			mu.Unlock()
		}()
	}
	wg.Wait()
	counts := map[string]int{}
	for _, s := range stamps {
		counts[s]++
	}
	if counts["node-a"] == 0 || counts["node-b"] == 0 {
		t.Errorf("session-less burst herded onto one member: %+v", counts)
	}
}

// A client that goes away before the relay has a response must NOT earn a
// metered 502: no upstream call completed and nothing was spent (mid-stream
// disconnects are the ones that earn a row). The fan-out walk aborts on the
// cancelled context instead of trying the remaining members.
func TestGroupClientCancelNoMetering(t *testing.T) {
	gh := newGroupHarness(t)
	// Both members answer the FIRST chat call normally (the warm-up pins
	// the session and reveals the member) and HOLD every later call: the
	// pinned member accepts the cancelled request and sits in Client.Do
	// while the test cancels. The other member answers instantly, so if
	// the walk did NOT abort, the retry would serve 200 and meter a row.
	release := make(chan struct{})
	defer close(release)
	blocked := make(chan struct{}, 4)
	holder := func(name string) http.Handler {
		var calls atomic.Int32
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
				return
			}
			if calls.Add(1) == 1 {
				memberEngine(name).ServeHTTP(w, r)
				return
			}
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-release
		})
	}
	mA, _ := gh.addMemberWith(t, "node-a", "m-a", holder("node-a"))
	mB, _ := gh.addMemberWith(t, "node-b", "m-b", holder("node-b"))
	gh.makeGroup(t, "pool", []store.GroupMemberInput{{ModelID: mA.ID}, {ModelID: mB.ID}})

	st, body := gh.chat(t, "pool", "pin")
	if st != 200 {
		t.Fatalf("warm-up status = %d (%s)", st, body)
	}
	var before int
	if err := gh.db.QueryRow(`SELECT COUNT(*) FROM usage_log WHERE model_requested = 'pool'`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gh.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"pool","messages":[{"role":"user","content":"pin"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+gh.key)
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			err = fmt.Errorf("request completed with status %d", resp.StatusCode)
		}
		done <- err
	}()
	select {
	case <-blocked: // the pinned member is holding the request
	case err := <-done:
		t.Fatalf("request finished before it could be cancelled: %v", err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Error("cancelled request reported success")
	}

	// The walk must stop dead: no second attempt, no metering. The client
	// erroring out does not mean the handler is done — give it a generous
	// grace window (an unguarded walk retries and meters within
	// milliseconds) before asserting the row count stayed put.
	time.Sleep(500 * time.Millisecond)
	var after int
	if err := gh.db.QueryRow(`SELECT COUNT(*) FROM usage_log WHERE model_requested = 'pool'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("cancelled request metered %d new usage row(s), want 0", after-before)
	}
}

// A DB failure inside ResolveTarget must not masquerade as a 404: the
// handler distinguishes the ErrNotFound sentinel (typo → 404) from query
// errors (outage → 500). Dropping model_groups makes every group lookup
// fail with "no such table" — a non-sentinel error.
func TestGroupResolveDBErrorIs500(t *testing.T) {
	gh := newGroupHarness(t)
	if _, err := gh.db.Exec(`DROP TABLE model_groups`); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, gh.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"anything","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gh.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (DB failure is not 'model not found')", resp.StatusCode)
	}
}
