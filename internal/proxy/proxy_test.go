// End-to-end proxy tests: fake upstreams per dialect, real SQLite, real
// recorder — asserting the usage rows that land in the database.
package proxy_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
	"llm-relay/internal/usage"
)

type harness struct {
	db     *sql.DB
	key    string // plaintext relay API key
	server *httptest.Server
	rec    *usage.Recorder
	ph     *proxy.Handler // live handler (in-flight counts, ...)
}

// newHarness boots a full relay against a fake upstream serving the given
// canned responses per path.
func newHarness(t *testing.T, upstream http.Handler) *harness {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	return newHarnessWithURL(t, up)
}

// newHarnessWithURL is newHarness for callers that own the upstream server.
func newHarnessWithURL(t *testing.T, up *httptest.Server) *harness {
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
	backend, err := store.CreateBackend(d, "fake", up.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, backend.ID, "up-id", "pub-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModel(d, 1, nil, nil, [4]*int64{ptr(100), ptr(200), ptr(10), ptr(0)}); err != nil {
		t.Fatal(err)
	}

	rec := usage.NewRecorder(d)
	rec.Start()
	h := &harness{db: d, key: plain, rec: rec}
	h.ph = proxy.New(d, rec, false)
	h.server = httptest.NewServer(h.ph.Mux())
	t.Cleanup(h.server.Close)
	return h
}

func ptr(v int64) *int64 { return &v }

func (h *harness) post(t *testing.T, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// usageRow waits for the recorder to flush and returns the single usage_log row.
func (h *harness) usageRow(t *testing.T) store.UsageRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, rows, err := store.UsageDetail(h.db, &store.UsageFilter{}, 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			return rows[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 1 usage row, got %d", len(rows))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- fake upstreams -----------------------------------------------------

func chatUpstream(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "up-id" {
			t.Errorf("model not rewritten: %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":40,
			"prompt_tokens_details":{"cached_tokens":60}}}`))
	})
}

func TestProxyChatNonStreamingRecordsUsage(t *testing.T) {
	h := newHarness(t, chatUpstream(t))
	resp := h.post(t, "/v1/chat/completions", `{"model":"pub-model","messages":[{"role":"user","content":"hello world this is a test prompt"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp.Body.Close()

	row := h.usageRow(t)
	if row.Prompt != 100 || row.Completion != 40 || row.Cached != 60 {
		t.Errorf("tokens: %+v", row)
	}
	if row.Estimated {
		t.Error("usage should not be estimated")
	}
	if row.Endpoint != "chat" || row.Status != 200 {
		t.Errorf("endpoint/status: %s %d", row.Endpoint, row.Status)
	}
	// cost: in = (100-60)*100/1e6 = 0 (rounds down), out = 40*200/1e6 = 0,
	// cache = 60*10/1e6 = 0 → total 0 at these token counts.
	if row.CostMicro != 0 {
		t.Errorf("cost = %d, want 0", row.CostMicro)
	}
}

func TestProxySessionHeaderAndDerive(t *testing.T) {
	h := newHarness(t, chatUpstream(t))
	resp := h.post(t, "/v1/chat/completions", `{"model":"pub-model","messages":[{"role":"user","content":"x"}]}`,
		map[string]string{"X-Session-Id": "explicit-sess"})
	resp.Body.Close()
	row := h.usageRow(t)
	if row.SessionID == nil || *row.SessionID != "explicit-sess" {
		t.Errorf("session = %v, want explicit-sess", row.SessionID)
	}
}

func TestProxyUnknownModel404(t *testing.T) {
	h := newHarness(t, chatUpstream(t))
	resp := h.post(t, "/v1/chat/completions", `{"model":"nope","messages":[]}`, nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProxyBadKey401(t *testing.T) {
	h := newHarness(t, chatUpstream(t))
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-model"}`))
	req.Header.Set("Authorization", "Bearer sk-notreal")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProxyRevokedKeyBlocked(t *testing.T) {
	h := newHarness(t, chatUpstream(t))
	if err := store.RevokeKey(h.db, 1, 1); err != nil {
		t.Fatal(err)
	}
	resp := h.post(t, "/v1/chat/completions", `{"model":"pub-model","messages":[]}`, nil)
	if resp.StatusCode != 401 {
		t.Errorf("revoked key status = %d, want 401", resp.StatusCode)
	}
}

func TestProxyMessagesDialect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,
			"output_tokens":4,"cache_read_input_tokens":100,"cache_creation_input_tokens":6}}`))
	}))
	defer up.Close()
	h := newHarnessWithURL(t, up)
	resp := h.post(t, "/v1/messages", `{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	resp.Body.Close()
	row := h.usageRow(t)
	if row.Endpoint != "messages" {
		t.Fatalf("endpoint = %s", row.Endpoint)
	}
	if row.Prompt != 116 || row.Cached != 100 || row.CacheWrite != 6 || row.Completion != 4 {
		t.Errorf("anthropic normalization: %+v", row)
	}
}

func TestProxyResponsesDialect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":40,"output_tokens":12,
			"input_tokens_details":{"cached_tokens":20}}}`))
	}))
	defer up.Close()
	h := newHarnessWithURL(t, up)
	resp := h.post(t, "/v1/responses", `{"model":"pub-model","input":"hi"}`, nil)
	resp.Body.Close()
	row := h.usageRow(t)
	if row.Endpoint != "responses" || row.Prompt != 40 || row.Cached != 20 || row.Completion != 12 {
		t.Errorf("responses dialect: %+v", row)
	}
}

func TestProxyEstimationFallbackStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello there my friend\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()
	h := newHarnessWithURL(t, up)
	body := `{"model":"pub-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := h.post(t, "/v1/chat/completions", body, nil)
	buf := make([]byte, 4096)
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			break
		}
	}
	resp.Body.Close()
	row := h.usageRow(t)
	if !row.Estimated {
		t.Error("row should be marked estimated")
	}
	// "hello there my friend" = 21 chars → 5, words = 4 → max = 5.
	if row.Completion != 5 {
		t.Errorf("completion = %d, want 5", row.Completion)
	}
	if row.Prompt != int64(len(body))/4 {
		t.Errorf("prompt = %d, want %d", row.Prompt, len(body)/4)
	}
}

func TestProxyUpstreamErrorRow(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	h := newHarnessWithURL(t, up)
	resp := h.post(t, "/v1/chat/completions", `{"model":"pub-model","messages":[]}`, nil)
	if resp.StatusCode != 500 {
		t.Errorf("client status = %d, want 500 passthrough", resp.StatusCode)
	}
	resp.Body.Close()
	row := h.usageRow(t)
	if row.Status != 500 || row.Prompt != 0 || row.CostMicro != 0 {
		t.Errorf("error row: %+v", row)
	}
}

func TestProxyCountTokensNotLogged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":5}`))
	}))
	defer up.Close()
	h := newHarnessWithURL(t, up)
	resp := h.post(t, "/v1/messages/count_tokens", `{"model":"pub-model","messages":[]}`, nil)
	resp.Body.Close()
	h.rec.Close(2 * time.Second)
	_, rows, _ := store.UsageDetail(h.db, &store.UsageFilter{}, 1, 10)
	if len(rows) != 0 {
		t.Errorf("count_tokens must not log usage, got %d rows", len(rows))
	}
}

func TestProxyStreamingCapturesUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":33,\"completion_tokens\":2}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	d, _ := db.Open(t.TempDir() + "/t.db")
	db.Migrate(d)
	defer d.Close()
	hash, _ := auth.HashPassword("x")
	u, _ := store.CreateUser(d, "b", "", hash, "user")
	plain, kh, pfx, _ := auth.GenerateAPIKey()
	store.CreateKey(d, u.ID, "k", kh, pfx)
	b, _ := store.CreateBackend(d, "f", up.URL, "")
	store.CreateModel(d, b.ID, "up-id", "pub")

	rec := usage.NewRecorder(d)
	rec.Start()
	srv := httptest.NewServer(proxy.New(d, rec, false).Mux())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"pub","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Drain the stream fully.
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		_ = n
		if err != nil {
			break
		}
	}
	resp.Body.Close()

	var rows []store.UsageRow
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, rows, _ = store.UsageDetail(d, &store.UsageFilter{}, 1, 10)
		if len(rows) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r0 := rows[0]
	if r0.Prompt != 33 || r0.Completion != 2 {
		t.Errorf("stream usage: %+v", r0)
	}
	if !r0.Streamed {
		t.Error("row should be marked streamed")
	}
	if r0.Estimated {
		t.Error("usage came from the final chunk, should not be estimated")
	}
}
