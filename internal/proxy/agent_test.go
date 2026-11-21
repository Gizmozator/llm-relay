// Agent-facing endpoints on the relay port: GET /v1/utilization (sk-key),
// POST /mcp (stateless JSON-RPC) and the public GET /v1/agent-guide.
package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

func agentUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
}

func agentGet(t *testing.T, url, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type utilResp struct {
	WindowMinutes int `json:"window_minutes"`
	Models        []struct {
		Name             string  `json:"name"`
		Online           bool    `json:"online"`
		Active           int     `json:"active"`
		ConcurrencyLimit int64   `json:"concurrency_limit"`
		UtilizationPct   float64 `json:"utilization_pct"`
		Status           string  `json:"status"`
	} `json:"models"`
}

func TestUtilizationREST(t *testing.T) {
	h := newHarness(t, agentUpstream())

	// No key → 401 (OpenAI-style error envelope).
	if resp := agentGet(t, h.server.URL+"/v1/utilization", ""); resp.StatusCode != 401 {
		t.Fatalf("no-key status = %d, want 401", resp.StatusCode)
	}

	resp := agentGet(t, h.server.URL+"/v1/utilization", h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r utilResp
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()

	if r.WindowMinutes != 5 {
		t.Errorf("window = %d, want 5", r.WindowMinutes)
	}
	if len(r.Models) != 1 {
		t.Fatalf("models = %+v, want one", r.Models)
	}
	m := r.Models[0]
	if m.Name != "pub-model" || !m.Online || m.Status != "free" {
		t.Errorf("model = %+v, want pub-model online free", m)
	}

	// A request that was busy for the whole 5-min window → 100 % → busy.
	err := store.InsertUsage(h.db, &store.UsageEvent{
		TS: time.Now().UTC().Format(time.RFC3339), UserID: 1,
		Endpoint: "chat", ModelRequ: "pub-model", LatencyMS: 5 * 60 * 1000, Status: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp = agentGet(t, h.server.URL+"/v1/utilization", h.key)
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	m = r.Models[0]
	if m.UtilizationPct < 79.0 || m.Status != "busy" {
		t.Errorf("after full-window load: pct=%.1f status=%s, want >=80 busy", m.UtilizationPct, m.Status)
	}

	// Disabled backend → offline (beats the busy verdict).
	off := false
	if _, err := store.UpdateBackend(h.db, 1, nil, nil, nil, &off); err != nil {
		t.Fatal(err)
	}
	resp = agentGet(t, h.server.URL+"/v1/utilization", h.key)
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if m = r.Models[0]; m.Online || m.Status != "offline" {
		t.Errorf("disabled backend: %+v, want offline", m)
	}
}

func TestUtilizationConcurrencyBusy(t *testing.T) {
	h := newHarness(t, agentUpstream())
	four := int64(4)
	if err := store.UpdateModelMeta(h.db, 1, nil, &four, nil); err != nil {
		t.Fatal(err)
	}
	// Fill all 4 slots in the tracker (as the proxy hook would).
	for i := 0; i < 4; i++ {
		h.ph.Flight.Enter(1, "pub-model")
	}
	defer func() {
		for i := 0; i < 4; i++ {
			h.ph.Flight.Leave(1, "pub-model")
		}
	}()

	resp := agentGet(t, h.server.URL+"/v1/utilization", h.key)
	var r utilResp
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	m := r.Models[0]
	if m.Active != 4 || m.ConcurrencyLimit != 4 || m.Status != "busy" {
		t.Errorf("full slots: %+v, want active=4 limit=4 busy", m)
	}
}

// mcpCall posts one JSON-RPC request and returns status + decoded response.
func mcpCall(t *testing.T, h *harness, body, key string) (int, map[string]any) {
	t.Helper()
	if key == "" {
		key = h.key
	}
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("not JSON (%d): %s", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out
}

func TestMCPEndpoint(t *testing.T) {
	h := newHarness(t, agentUpstream())

	// Auth first, at the HTTP layer.
	if st, _ := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "sk-wrong"); st != 401 {
		t.Fatalf("bad key status = %d, want 401", st)
	}

	// initialize: supported version echoed, serverInfo present.
	st, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize",
		"params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, "")
	if st != 200 || out["id"].(float64) != 1 {
		t.Fatalf("initialize: status=%d out=%v", st, out)
	}
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want echo of 2025-03-26", res["protocolVersion"])
	}
	if res["serverInfo"].(map[string]any)["name"] != "llm-relay" {
		t.Errorf("serverInfo = %v", res["serverInfo"])
	}

	// notifications/initialized → 202, no body.
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+h.key)
	nresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	nresp.Body.Close()
	if nresp.StatusCode != http.StatusAccepted {
		t.Errorf("notification status = %d, want 202", nresp.StatusCode)
	}

	// tools/list advertises check_utilization.
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "")
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want one", tools)
	}
	if tools[0].(map[string]any)["name"] != "check_utilization" {
		t.Errorf("tool name = %v", tools[0].(map[string]any)["name"])
	}

	// tools/call returns the same payload as REST, in content + structuredContent.
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call",
		"params":{"name":"check_utilization","arguments":{}}}`, "")
	callRes := out["result"].(map[string]any)
	if callRes["isError"].(bool) {
		t.Fatalf("tools/call errored: %v", callRes)
	}
	sc := callRes["structuredContent"].(map[string]any)
	models := sc["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["name"] != "pub-model" {
		t.Fatalf("structuredContent models = %v", sc["models"])
	}
	text := callRes["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "pub-model") {
		t.Errorf("text content missing model: %s", text)
	}

	// model filter narrows (and can return nothing).
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call",
		"params":{"name":"check_utilization","arguments":{"model":"other"}}}`, "")
	sc = out["result"].(map[string]any)["structuredContent"].(map[string]any)
	if len(sc["models"].([]any)) != 0 {
		t.Errorf("filtered models = %v, want empty", sc["models"])
	}

	// Unknown tool → protocol error -32602 (spec); unknown method → -32601.
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call",
		"params":{"name":"nope"}}`, "")
	if e, ok := out["error"].(map[string]any); !ok || e["code"].(float64) != -32602 {
		t.Errorf("unknown tool: %v, want -32602", out)
	}
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":6,"method":"resources/list"}`, "")
	if e, ok := out["error"].(map[string]any); !ok || e["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v, want -32601", out)
	}

	// Garbage body → -32700 parse error.
	_, out = mcpCall(t, h, `{not json`, "")
	if e, ok := out["error"].(map[string]any); !ok || e["code"].(float64) != -32700 {
		t.Errorf("parse error: %v, want -32700", out)
	}
}

func TestAgentGuidePublic(t *testing.T) {
	h := newHarness(t, agentUpstream())
	// RelayURL is the server root (PUBLIC_RELAY_URL convention: no /v1);
	// the guide links /v1/... and /mcp off it.
	h.ph.RelayURL = "https://relay.example"

	resp := agentGet(t, h.server.URL+"/v1/agent-guide", "") // no key needed
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	md := string(body)
	for _, want := range []string{"https://relay.example/v1/utilization", "check_utilization", "sk-"} {
		if !strings.Contains(md, want) {
			t.Errorf("guide missing %q", want)
		}
	}
	if strings.Contains(md, "{{RELAY_URL}}") {
		t.Error("placeholder not substituted")
	}
}

// The health cache moved to proxy; the handler must use it (not a nil deref).
func TestHandlerHealthCacheWired(t *testing.T) {
	h := newHarness(t, agentUpstream())
	if h.ph.Health == nil {
		t.Fatal("proxy.New must create a HealthCache")
	}
	var _ *proxy.HealthCache = h.ph.Health
}
