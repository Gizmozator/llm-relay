// Proxy routing tests for remote:// backends (plan 2026-11-24-remote-backend-v1):
// the scheme-dispatch transport must send remote URLs to the Remote seam
// (never HTTP), the health cache must answer from the registry hook, and a
// dead channel must surface as a gateway error like a dead socket.
package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/proxy"
	"llm-relay/internal/remote"
	"llm-relay/internal/store"
	"llm-relay/internal/usage"
)

// remoteStub stands in for *remote.Registry: records what was tunneled and
// answers with a canned completion, or fails dial-class when "offline".
type remoteStub struct {
	mu     sync.Mutex
	paths  []string
	online atomic.Bool
}

func (s *remoteStub) RoundTrip(req *http.Request) (*http.Response, error) {
	if !s.online.Load() {
		return nil, remote.ErrNotConnected
	}
	s.mu.Lock()
	s.paths = append(s.paths, req.URL.String())
	s.mu.Unlock()
	body := `{"choices":[{"message":{"content":"from-remote"}}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	return &http.Response{
		StatusCode:    200,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: -1,
		Request:       req,
	}, nil
}

func (s *remoteStub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func newRemoteHarness(t *testing.T) (*harness, *remoteStub, *store.Backend) {
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
	b, _, err := store.CreateRemoteBackend(d, "gpu-box")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b.ID, "up-id", "pub-model"); err != nil {
		t.Fatal(err)
	}

	stub := &remoteStub{}
	rec := usage.NewRecorder(d)
	rec.Start()
	h := &harness{db: d, key: plain, rec: rec}
	h.ph = proxy.New(d, rec, false)
	h.ph.Remote = stub
	h.ph.Health.Remote = func(store.Backend) bool { return stub.online.Load() }
	h.server = httptest.NewServer(h.ph.Mux())
	t.Cleanup(h.server.Close)
	return h, stub, b
}

func TestRemoteBackendRouting(t *testing.T) {
	h, stub, b := newRemoteHarness(t)
	stub.online.Store(true)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "from-remote") {
		t.Fatalf("body = %s", body)
	}
	want := b.BaseURL + "/v1/chat/completions"
	if got := stub.seen(); len(got) != 1 || got[0] != want {
		t.Fatalf("tunneled paths = %v, want [%s]", got, want)
	}
}

func TestRemoteBackendOffline(t *testing.T) {
	h, stub, _ := newRemoteHarness(t)
	stub.online.Store(false)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode < 500 {
		t.Fatalf("offline status = %d, want 5xx", resp.StatusCode)
	}
	resp.Body.Close()
	if got := stub.seen(); len(got) != 0 {
		t.Fatalf("offline backend was dialed: %v", got)
	}
}

// The health cache must answer remote backends from the hook without ever
// dialing: online = live channel.
func TestRemoteHealthCacheHook(t *testing.T) {
	h, stub, b := newRemoteHarness(t)

	stub.online.Store(true)
	if e := h.ph.Health.Status(context.Background(), []store.Backend{*b})[b.ID]; !e.Online {
		t.Fatalf("online hook: %+v, want Online", e)
	}
	stub.online.Store(false)
	if e := h.ph.Health.Status(context.Background(), []store.Backend{*b})[b.ID]; e.Online {
		t.Fatalf("offline hook: %+v, want offline", e)
	}
}
