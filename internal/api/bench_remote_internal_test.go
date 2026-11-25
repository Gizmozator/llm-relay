// Internal test: benchmarking a model on a remote backend must ride the
// connector's live channel (the same RoundTripper the tunneled probe uses),
// not dial the remote:// base_url as if it were an HTTP address — the first
// remote-backend benchmark answered 502 "upstream request failed" because
// benchStreamRun used http.DefaultClient for every backend. Lives in
// package api to call the handler directly with a fake controller.
package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/store"
)

// tunnelController stands in for a connected remote.Registry: RoundTrip
// answers with a small SSE token stream — the shape benchStreamRun times —
// and records the request URL so the test proves the run went through the
// tunnel, not at a dialable address.
type tunnelController struct{ url string }

func (tunnelController) ServeConn(http.ResponseWriter, *http.Request, *store.Backend, string) {
}
func (tunnelController) Kick(int64, string)               {}
func (tunnelController) BackendOnline(store.Backend) bool { return true }
func (t *tunnelController) RoundTrip(req *http.Request) (*http.Response, error) {
	t.url = req.URL.String()
	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{\"content\":\"word%d \",\"role\":\"assistant\"}}]}\n\n", i)
		time.Sleep(time.Millisecond)
	}
	b.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":100}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(b.String())),
	}, nil
}

// newBenchDB makes a migrated db holding one remote backend with one model.
func newBenchDB(t *testing.T) (*sql.DB, *store.Backend) {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/bench.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	b, _, err := store.CreateRemoteBackend(d, "test_backend")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(d, b.ID, "gibberish-mini", "gibberish-mini"); err != nil {
		t.Fatal(err)
	}
	return d, b
}

// benchReq builds the benchmark request for the db's single model, with the
// path value the production mux would have set.
func benchReq(t *testing.T, d *sql.DB) *http.Request {
	t.Helper()
	ms, err := store.ListModels(d)
	if err != nil || len(ms) == 0 {
		t.Fatalf("models: %v", err)
	}
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/admin/models/%d/benchmark", ms[0].ID), nil)
	req.SetPathValue("id", strconv.FormatInt(ms[0].ID, 10))
	return req
}

func TestBenchmarkRemoteBackendRidesTunnel(t *testing.T) {
	d, b := newBenchDB(t)

	tun := &tunnelController{}
	s := &Server{DB: d, Remote: tun}
	rec := httptest.NewRecorder()
	s.AdminBenchmarkModel(rec, benchReq(t, d), &auth.Claims{UserID: 1, Role: "admin"})

	if rec.Code != http.StatusOK {
		t.Fatalf("benchmark = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		BenchTokPerS   float64 `json:"bench_tok_per_s"`
		BenchPPTokPerS float64 `json:"bench_pp_tok_per_s"`
		Completion     int64   `json:"completion_tokens"`
		Prompt         int64   `json:"prompt_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Completion != 100 || out.BenchTokPerS <= 0 {
		t.Errorf("decode pass = %+v, want 100 completion tokens measured", out)
	}
	if out.Prompt != 10 || out.BenchPPTokPerS <= 0 {
		t.Errorf("prefill pass = %+v, want 10 prompt tokens measured", out)
	}
	if want := store.RemoteBaseURL(b.ID) + "/v1/chat/completions"; tun.url != want {
		t.Errorf("tunnel request = %q, want %q", tun.url, want)
	}
}

// An unwired registry is a local wiring state, not an upstream failure: 503
// like every other "not wired" answer, never a 502 blaming the upstream.
func TestBenchmarkRemoteNotWiredIs503(t *testing.T) {
	d, _ := newBenchDB(t)

	s := &Server{DB: d} // Remote nil: remote support not wired
	rec := httptest.NewRecorder()
	s.AdminBenchmarkModel(rec, benchReq(t, d), &auth.Claims{UserID: 1, Role: "admin"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("benchmark = %d %s, want 503", rec.Code, rec.Body.String())
	}
}
