// Headless bootstrap tests (plan 2026-11-24-headless-spoke): the env→DB
// writes must be exact, idempotent, and safe — a collision with another
// backend's model is fatal, an unreachable backend at boot is not.
package headless_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-relay/internal/db"
	"llm-relay/internal/headless"
	"llm-relay/internal/store"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// fakeBackend answers /v1/models with the given ids; "" args simulate a
// backend that does not serve that id.
func fakeBackend(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testConf(backendURL string) headless.Config {
	return headless.Config{
		BackendURL: backendURL, BackendToken: "sk-up", ModelName: "qwen3-coder",
		HubURL: "https://relay.example.fi", HubToken: "fed-token",
		PriceIn: 100, PriceOut: 200, PriceCacheRead: 3, PriceCacheWrite: 4,
	}
}

// captureLog collects logf lines for assertions.
func captureLog() (func(string, ...any), func() string) {
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	return logf, func() string { return strings.Join(lines, "\n") }
}

// modelByName resolves the model row by display name (backend discarded).
func modelByName(t *testing.T, d *sql.DB, name string) *store.Model {
	t.Helper()
	m, _, err := store.ModelByName(d, name)
	if err != nil {
		t.Fatalf("model %q: %v", name, err)
	}
	return m
}

func TestBootstrapCreatesRows(t *testing.T) {
	d := testDB(t)
	eng := fakeBackend(t, "qwen3-coder")
	logf, logs := captureLog()
	if err := headless.Bootstrap(d, testConf(eng.URL), logf); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	b, err := store.BackendByName(d, headless.BackendName)
	if err != nil {
		t.Fatalf("backend row missing: %v", err)
	}
	if b.BaseURL != eng.URL || b.APIKey != "sk-up" || !b.Enabled {
		t.Errorf("backend = %+v, want url=%s key=sk-up enabled", b, eng.URL)
	}

	m := modelByName(t, d, "qwen3-coder")
	if m.UpstreamModel != "qwen3-coder" || m.BackendID != b.ID {
		t.Errorf("model = %+v, want upstream qwen3-coder on backend %d", m, b.ID)
	}
	if !m.Enabled {
		t.Error("model must be enabled")
	}
	if m.FedVisibility != store.FedVisAll {
		t.Errorf("fed_visibility = %q, want all", m.FedVisibility)
	}
	if m.PriceIn != 100 || m.PriceOut != 200 || m.PriceCacheRead != 3 || m.PriceCacheWrite != 4 {
		t.Errorf("prices = %d/%d/%d/%d, want 100/200/3/4",
			m.PriceIn, m.PriceOut, m.PriceCacheRead, m.PriceCacheWrite)
	}

	f, err := store.GetFederationSettings(d)
	if err != nil {
		t.Fatal(err)
	}
	if f.Role != store.FedRoleSpoke || f.HubURL != "https://relay.example.fi" || f.HubToken != "fed-token" {
		t.Errorf("federation settings = %+v, want spoke/relay.example.fi/fed-token", f)
	}
	if strings.Contains(logs(), "WARNING") {
		t.Errorf("served model must not warn, logs:\n%s", logs())
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	d := testDB(t)
	eng := fakeBackend(t, "qwen3-coder")
	logf, _ := captureLog()
	if err := headless.Bootstrap(d, testConf(eng.URL), logf); err != nil {
		t.Fatal(err)
	}
	firstModel := modelByName(t, d, "qwen3-coder")
	if err := headless.Bootstrap(d, testConf(eng.URL), logf); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if n := count(d, `SELECT COUNT(*) FROM backends`); n != 1 {
		t.Errorf("backends after re-run = %d, want 1", n)
	}
	if n := count(d, `SELECT COUNT(*) FROM models`); n != 1 {
		t.Errorf("models after re-run = %d, want 1", n)
	}
	second := modelByName(t, d, "qwen3-coder")
	if firstModel.ID != second.ID {
		t.Errorf("re-run replaced the row: id %d → %d", firstModel.ID, second.ID)
	}
}

// The env is the source of truth: a restart re-applies changed values over
// anything edited in between (dashboard edit or previous boot).
func TestBootstrapReappliesEnv(t *testing.T) {
	d := testDB(t)
	eng := fakeBackend(t, "qwen3-coder")
	logf, _ := captureLog()
	if err := headless.Bootstrap(d, testConf(eng.URL), logf); err != nil {
		t.Fatal(err)
	}
	m := modelByName(t, d, "qwen3-coder")
	disabled := false
	if _, err := store.UpdateModel(d, m.ID, nil, &disabled, [4]*int64{}); err != nil {
		t.Fatal(err)
	}

	c := testConf(eng.URL)
	c.PriceIn = 999
	if err := headless.Bootstrap(d, c, logf); err != nil {
		t.Fatal(err)
	}
	m = modelByName(t, d, "qwen3-coder")
	if !m.Enabled {
		t.Error("restart must re-enable the model (env is the source of truth)")
	}
	if m.PriceIn != 999 {
		t.Errorf("price_in = %d, want 999", m.PriceIn)
	}
}

func TestBootstrapNameCollisionRefusesBoot(t *testing.T) {
	d := testDB(t)
	eng := fakeBackend(t, "qwen3-coder")
	other, err := store.CreateBackend(d, "other", "http://other.example.fi:9", "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := store.CreateModel(d, other.ID, "their-upstream", "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}

	logf, _ := captureLog()
	if err := headless.Bootstrap(d, testConf(eng.URL), logf); err == nil {
		t.Fatal("name collision with another backend must refuse to boot")
	}
	// The other backend's row must be untouched.
	m, err := store.ModelByID(d, theirs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.BackendID != other.ID || m.UpstreamModel != "their-upstream" || !m.Enabled {
		t.Errorf("foreign row modified: %+v", m)
	}
	// And no headless model row was created.
	if n := count(d, `SELECT COUNT(*) FROM models WHERE backend_id=(SELECT id FROM backends WHERE name='headless')`); n != 0 {
		t.Errorf("headless model rows after refused boot = %d, want 0", n)
	}
}

// A backend that is down at boot must not stop the relay: the rows are
// written anyway and the health probe tracks the backend from then on.
func TestBootstrapBackendDown(t *testing.T) {
	d := testDB(t)
	dead := fakeBackend(t)
	url := dead.URL
	dead.Close() // nothing listens anymore
	logf, logs := captureLog()
	if err := headless.Bootstrap(d, testConf(url), logf); err != nil {
		t.Fatalf("backend down must not fail the boot: %v", err)
	}
	if n := count(d, `SELECT COUNT(*) FROM models WHERE name='qwen3-coder'`); n != 1 {
		t.Errorf("model rows = %d, want 1", n)
	}
	if !strings.Contains(logs(), "not reachable") {
		t.Errorf("expected an unreachable-backend log, got:\n%s", logs())
	}
}

// The typo catcher: backend answers but does not list MODEL_NAME → loud
// warning, boot still succeeds.
func TestBootstrapWarnsWhenModelNotServed(t *testing.T) {
	d := testDB(t)
	eng := fakeBackend(t, "some-other-model", "llama-70b")
	logf, logs := captureLog()
	if err := headless.Bootstrap(d, testConf(eng.URL), logf); err != nil {
		t.Fatalf("unserved model must not fail the boot: %v", err)
	}
	if !strings.Contains(logs(), `MODEL_NAME="qwen3-coder"`) {
		t.Errorf("expected the not-served warning, got:\n%s", logs())
	}
	if n := count(d, `SELECT COUNT(*) FROM models WHERE name='qwen3-coder'`); n != 1 {
		t.Errorf("model rows = %d, want 1 (warning is non-fatal)", n)
	}
}

func count(db *sql.DB, q string) int {
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return -1
	}
	return n
}
