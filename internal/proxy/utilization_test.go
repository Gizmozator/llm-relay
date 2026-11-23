// Visibility filtering on the agent-facing surfaces: non-admin sk-keys get
// the ADVERTISED catalog on GET /v1/utilization and MCP check_utilization
// (synthetic pool rows for enabled groups + the public rows /v1/models
// serves); admins get the physical rows including internal wiring.
package proxy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"llm-relay/internal/auth"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// adminKey creates an admin user with a fresh sk-key on the harness db and
// returns the plaintext key.
func adminKey(t *testing.T, h *harness) string {
	t.Helper()
	hash, _ := auth.HashPassword("x")
	adm, err := store.CreateUser(h.db, "root", "", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	plain, kh, prefix, _ := auth.GenerateAPIKey()
	if _, err := store.CreateKey(h.db, adm.ID, "root", kh, prefix); err != nil {
		t.Fatal(err)
	}
	return plain
}

// utilNames fetches GET /v1/utilization and returns the emitted rows by name.
func utilNames(t *testing.T, h *harness, key string) map[string][]proxy.ModelStatus {
	t.Helper()
	resp := agentGet(t, h.server.URL+"/v1/utilization", key)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r struct {
		Models []proxy.ModelStatus `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	out := map[string][]proxy.ModelStatus{}
	for _, m := range r.Models {
		out[m.Name] = append(out[m.Name], m)
	}
	return out
}

func TestUtilizationInternalRowsAdminOnly(t *testing.T) {
	h := newHarness(t, agentUpstream())
	secret, err := store.CreateModel(h.db, 1, "up-secret", "secret-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateModelVisibility(h.db, secret.ID, "internal"); err != nil {
		t.Fatal(err)
	}

	// Non-admin key: public rows present, the plain internal row absent.
	if got := utilNames(t, h, h.key); len(got["pub-model"]) != 1 || len(got["secret-model"]) != 0 {
		t.Errorf("non-admin rows = %v, want pub-model only", got)
	}
	// Admin key: both present (the pre-fix behavior, unchanged).
	if got := utilNames(t, h, adminKey(t, h)); len(got["pub-model"]) != 1 || len(got["secret-model"]) != 1 {
		t.Errorf("admin rows = %v, want pub-model and secret-model", got)
	}

	// MCP check_utilization with the non-admin key: no internal name in
	// either payload form (the agent-facing text or structuredContent).
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"check_utilization","arguments":{}}}`, h.key)
	callRes := out["result"].(map[string]any)
	if callRes["isError"] == true {
		t.Fatalf("tools/call errored: %v", callRes)
	}
	text := callRes["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "secret-model") {
		t.Errorf("MCP text leaks internal name: %s", text)
	}
	if !strings.Contains(text, "pub-model") {
		t.Errorf("MCP text lost the public row: %s", text)
	}
	sc := callRes["structuredContent"].(map[string]any)
	for _, m := range sc["models"].([]any) {
		if name := m.(map[string]any)["name"].(string); name == "secret-model" {
			t.Errorf("MCP structuredContent leaks internal row: %v", m)
		}
	}
}

func TestUtilizationGroupPoolRows(t *testing.T) {
	h := newHarness(t, agentUpstream())
	yes := true
	g, err := store.CreateGroup(h.db, "pool-g")
	if err != nil {
		t.Fatal(err)
	}
	m1, err := store.CreateModel(h.db, 1, "up-m1", "m-one")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateModel(h.db, 1, "up-m2", "m-two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(h.db, g.ID, nil, &yes, []store.GroupMemberInput{
		{ModelID: m1.ID, Position: 0}, {ModelID: m2.ID, Position: 1},
	}); err != nil {
		t.Fatal(err)
	}
	three, four := int64(3), int64(4)
	if err := store.UpdateModelMeta(h.db, m1.ID, nil, &three, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateModelMeta(h.db, m2.ID, nil, &four, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// One stream on a member (streams carry the RESOLVED member name).
	flight := h.ph.Flight.Enter(proxy.Stream{UserID: 1, Model: "m-one", ModelID: m1.ID})
	defer h.ph.Flight.Leave(flight)

	// Non-admin: ONE synthetic pool row for the group — negative id, the
	// summed member active and limit — and no member names.
	got := utilNames(t, h, h.key)
	if len(got["pool-g"]) != 1 {
		t.Fatalf("non-admin pool-g rows = %v, want exactly one", got["pool-g"])
	}
	p := got["pool-g"][0]
	if p.ID != -g.ID || p.Active != 1 || p.ConcurrencyLimit != 7 ||
		!p.Online || p.Draining || p.Status != "free" {
		t.Errorf("pool row = %+v, want id %d active 1 limit 7 online free", p, -g.ID)
	}
	if len(got["pub-model"]) != 1 {
		t.Errorf("non-admin rows = %v, want pub-model + pool-g", got)
	}
	for _, member := range []string{"m-one", "m-two"} {
		if len(got[member]) != 0 {
			t.Errorf("non-admin rows leak member %q: %v", member, got)
		}
	}

	// Admin: physical rows (both members, no synthetic pool row).
	adm := utilNames(t, h, adminKey(t, h))
	if len(adm["m-one"]) != 1 || len(adm["m-two"]) != 1 || len(adm["pool-g"]) != 0 {
		t.Errorf("admin rows = %v, want both member rows and no pool row", adm)
	}

	// MCP with a non-admin key: pool row present, member names absent.
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"check_utilization","arguments":{}}}`, h.key)
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "pool-g") {
		t.Errorf("MCP text missing the pool row: %s", text)
	}
	for _, member := range []string{"m-one", "m-two"} {
		if strings.Contains(text, member) {
			t.Errorf("MCP text leaks member %q: %s", member, text)
		}
	}
}

func TestUtilizationAdvertisedCatalogEdges(t *testing.T) {
	h := newHarness(t, agentUpstream())
	yes := true

	// Disabled group: not advertised, members stay hidden.
	gd, err := store.CreateGroup(h.db, "pool-d")
	if err != nil {
		t.Fatal(err)
	}
	m3, err := store.CreateModel(h.db, 1, "up-m3", "m-three")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(h.db, gd.ID, nil, nil,
		[]store.GroupMemberInput{{ModelID: m3.ID, Position: 0}}); err != nil {
		t.Fatal(err)
	}

	// Enabled group whose name is ALSO carried by an external public row
	// (AddModels import — the guard-free path): the pool shadows it.
	gs, err := store.CreateGroup(h.db, "shadowed")
	if err != nil {
		t.Fatal(err)
	}
	m4, err := store.CreateModel(h.db, 1, "up-m4", "m-four")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(h.db, gs.ID, nil, &yes,
		[]store.GroupMemberInput{{ModelID: m4.ID, Position: 0}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddModels(h.db, 1, []string{"shadowed"}); err != nil {
		t.Fatal(err)
	}

	// Row on a DISABLED backend: not advertised at all.
	off := false
	b2, err := store.CreateBackend(h.db, "off", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateBackend(h.db, b2.ID, nil, nil, nil, &off, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateModel(h.db, b2.ID, "up-off", "off-model"); err != nil {
		t.Fatal(err)
	}

	// Two same-name PUBLIC rows on different enabled backends: only the
	// ResolveTarget winner (lowest id) is advertised.
	b3, err := store.CreateBackend(h.db, "fake3", "http://127.0.0.1:2", "")
	if err != nil {
		t.Fatal(err)
	}
	t1, err := store.CreateModel(h.db, 1, "up-t1", "twins")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := store.CreateModel(h.db, b3.ID, "up-t2", "twins")
	if err != nil {
		t.Fatal(err)
	}

	got := utilNames(t, h, h.key)
	if len(got["pool-d"]) != 0 || len(got["m-three"]) != 0 {
		t.Errorf("disabled group leaked: %v", got)
	}
	if len(got["shadowed"]) != 1 || got["shadowed"][0].ID != -gs.ID {
		t.Errorf("shadowed rows = %v, want only the pool row id %d", got["shadowed"], -gs.ID)
	}
	if len(got["m-four"]) != 0 {
		t.Errorf("member of the shadowing group leaked: %v", got)
	}
	if len(got["off-model"]) != 0 {
		t.Errorf("row on a disabled backend leaked: %v", got)
	}
	low, high := t1.ID, t2.ID
	if high < low {
		low, high = high, low
	}
	if len(got["twins"]) != 1 || got["twins"][0].ID != low {
		t.Errorf("twins rows = %v, want only the lowest-id winner %d", got["twins"], low)
	}

	// Admin: physical rows — both twins, the member + the external
	// same-name standalone row, the disabled-backend row (offline), the
	// disabled group's member.
	adm := utilNames(t, h, adminKey(t, h))
	if len(adm["twins"]) != 2 {
		t.Errorf("admin twins = %v, want both rows", adm["twins"])
	}
	if len(adm["shadowed"]) != 1 || len(adm["m-four"]) != 1 {
		t.Errorf("admin shadowed/m-four = %v / %v, want the external standalone and the member row",
			adm["shadowed"], adm["m-four"])
	}
	if len(adm["off-model"]) != 1 || adm["off-model"][0].Status != "offline" {
		t.Errorf("admin off-model = %v, want one offline row", adm["off-model"])
	}
	if len(adm["m-three"]) != 1 {
		t.Errorf("admin missing the disabled group's member: %v", adm)
	}
}
