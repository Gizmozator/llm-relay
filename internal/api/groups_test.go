// Admin groups API: CRUD round-trip, weight seeding, error taxonomy and the
// model visibility editor (plan 2026-11-22, task 3).
package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"llm-relay/internal/store"
)

// groupResp mirrors the group endpoint response shape.
type groupResp struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	MemberCount int    `json:"member_count"`
	Members     []struct {
		ModelID     int64   `json:"model_id"`
		Weight      float64 `json:"weight"`
		Position    int     `json:"position"`
		ModelName   string  `json:"model_name"`
		BackendName string  `json:"backend_name"`
		Visibility  string  `json:"visibility"`
	} `json:"members"`
}

// newCatalogModel creates a backend (unless already created under that name)
// plus one model on it, straight through the store.
func newCatalogModel(t *testing.T, e *testEnv, backendName, modelName string) *store.Model {
	t.Helper()
	backends, err := store.ListBackends(e.db)
	if err != nil {
		t.Fatal(err)
	}
	var b *store.Backend
	for i := range backends {
		if backends[i].Name == backendName {
			b = &backends[i]
		}
	}
	if b == nil {
		nb, err := store.CreateBackend(e.db, backendName, "http://127.0.0.1:1", "")
		if err != nil {
			t.Fatal(err)
		}
		b = nb
	}
	m, err := store.CreateModel(e.db, b.ID, "up-"+modelName, modelName)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func getGroup(t *testing.T, e *testEnv, tok string, id int64) (int, groupResp) {
	t.Helper()
	resp := e.do(t, "GET", pathf("/api/admin/groups/%d", id), "", tok)
	var g groupResp
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	return resp.StatusCode, g
}

func TestAdminGroupCRUDRoundTrip(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	m1 := newCatalogModel(t, e, "node-a", "qwen-a")
	m2 := newCatalogModel(t, e, "node-b", "qwen-b")

	// Create: born disabled, no members.
	resp := e.do(t, "POST", "/api/admin/groups", `{"name":"qwen3-coder"}`, tok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group = %d, want 201", resp.StatusCode)
	}
	var g groupResp
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if g.ID == 0 || g.Enabled || g.MemberCount != 0 {
		t.Fatalf("created group = %+v, want enabled=false, 0 members", g)
	}

	// Appears in the list.
	resp = e.do(t, "GET", "/api/admin/groups", "", tok)
	var list struct {
		Groups []groupResp `json:"groups"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Groups) != 1 || list.Groups[0].Name != "qwen3-coder" {
		t.Fatalf("list = %+v, want one qwen3-coder", list.Groups)
	}

	// PATCH members + enable in one call: members flip to internal via the
	// store, order follows position.
	resp = e.do(t, "PATCH", pathf("/api/admin/groups/%d", g.ID),
		fmt.Sprintf(`{"enabled":true,"members":[{"model_id":%d,"position":1},`+
			`{"model_id":%d,"position":0,"weight":2.5}]}`, m1.ID, m2.ID), tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put members = %d, want 200", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if !g.Enabled || g.MemberCount != 2 || len(g.Members) != 2 {
		t.Fatalf("after members PATCH: %+v", g)
	}
	if g.Members[0].ModelID != m2.ID || g.Members[1].ModelID != m1.ID {
		t.Errorf("member order = %d,%d, want m2,m1 by position", g.Members[0].ModelID, g.Members[1].ModelID)
	}
	if g.Members[0].ModelName != "qwen-b" || g.Members[0].BackendName != "node-b" {
		t.Errorf("member display names wrong: %+v", g.Members[0])
	}
	for _, mem := range g.Members {
		if mem.Visibility != "internal" {
			t.Errorf("member %d visibility = %q, want internal", mem.ModelID, mem.Visibility)
		}
	}

	// GET shows the same members.
	code, got := getGroup(t, e, tok, g.ID)
	if code != http.StatusOK || got.MemberCount != 2 {
		t.Fatalf("get group = %d %+v", code, got)
	}

	// Blank rename → 400.
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", g.ID), `{"name":"  "}`, tok); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blank rename = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// DELETE: gone, and the members return to public.
	if resp := e.do(t, "DELETE", pathf("/api/admin/groups/%d", g.ID), "", tok); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	if code, _ := getGroup(t, e, tok, g.ID); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
	for _, id := range []int64{m1.ID, m2.ID} {
		m, err := store.ModelByID(e.db, id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Visibility != "public" {
			t.Errorf("model %d visibility after delete = %q, want public", id, m.Visibility)
		}
	}
}

func TestAdminGroupWeightSeeding(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	fast := newCatalogModel(t, e, "node-a", "fast")
	slow := newCatalogModel(t, e, "node-b", "slow")
	manual := newCatalogModel(t, e, "node-c", "manual")
	if err := store.UpdateModelMeta(e.db, fast.ID, nil, nil, nil, ptrF(42.5), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateModelMeta(e.db, slow.ID, nil, nil, nil, ptrF(0), nil); err != nil {
		t.Fatal(err)
	}

	resp := e.do(t, "POST", "/api/admin/groups", `{"name":"pool"}`, tok)
	var g groupResp
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()

	resp = e.do(t, "PATCH", pathf("/api/admin/groups/%d", g.ID),
		fmt.Sprintf(`{"members":[{"model_id":%d},{"model_id":%d},{"model_id":%d,"weight":3}]}`,
			fast.ID, slow.ID, manual.ID), tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put members = %d, want 200", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	want := map[int64]float64{fast.ID: 42.5, slow.ID: 1, manual.ID: 3}
	if len(g.Members) != 3 {
		t.Fatalf("members = %+v, want 3", g.Members)
	}
	for _, mem := range g.Members {
		if mem.Weight != want[mem.ModelID] {
			t.Errorf("member %d weight = %v, want %v", mem.ModelID, mem.Weight, want[mem.ModelID])
		}
	}
}

func ptrF(v float64) *float64 { return &v }

func createGroup(t *testing.T, e *testEnv, tok, name string) groupResp {
	t.Helper()
	resp := e.do(t, "POST", "/api/admin/groups", fmt.Sprintf(`{"name":%q}`, name), tok)
	var g groupResp
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group %q = %d, want 201", name, resp.StatusCode)
	}
	return g
}

func TestAdminGroupErrorTaxonomy(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	m1 := newCatalogModel(t, e, "node-a", "pub-model")
	newCatalogModel(t, e, "node-b", "other-model")

	// Invalid name → 400.
	if resp := e.do(t, "POST", "/api/admin/groups", `{"name":"  "}`, tok); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blank name = %d, want 400", resp.StatusCode)
	}
	// Group name colliding with a public model name → 409.
	if resp := e.do(t, "POST", "/api/admin/groups", `{"name":"pub-model"}`, tok); resp.StatusCode != http.StatusConflict {
		t.Errorf("public-model-name group = %d, want 409", resp.StatusCode)
	}
	// Duplicate group name → 409.
	pool := createGroup(t, e, tok, "pool")
	if resp := e.do(t, "POST", "/api/admin/groups", `{"name":"pool"}`, tok); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate group = %d, want 409", resp.StatusCode)
	}
	// Oversized members array → 400 (bounded replace-set; checked before
	// any per-member work).
	big := `{"members":[`
	for i := 0; i < 257; i++ {
		if i > 0 {
			big += ","
		}
		big += fmt.Sprintf(`{"model_id":%d}`, 90000+i)
	}
	big += "]}"
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool.ID), big, tok); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized members = %d, want 400", resp.StatusCode)
	}

	// Unknown id → 404 on GET, PATCH and DELETE.
	if resp := e.do(t, "GET", "/api/admin/groups/9999", "", tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get unknown = %d, want 404", resp.StatusCode)
	}
	if resp := e.do(t, "PATCH", "/api/admin/groups/9999", `{}`, tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("put unknown = %d, want 404", resp.StatusCode)
	}
	if resp := e.do(t, "DELETE", "/api/admin/groups/9999", "", tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete unknown = %d, want 404", resp.StatusCode)
	}

	// Enabling an empty group → 409.
	pool2 := createGroup(t, e, tok, "pool2")
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool2.ID), `{"enabled":true}`, tok); resp.StatusCode != http.StatusConflict {
		t.Errorf("enable empty = %d, want 409", resp.StatusCode)
	}

	// An already-enabled group that was emptied (the designed transient)
	// and echoes {"enabled":true} back must get the enable rule's own
	// message, not the generic conflict mapping: the store requires ≥1
	// member on EVERY enabled=1 write, not only on the transition.
	pool3 := createGroup(t, e, tok, "pool3")
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool3.ID),
		fmt.Sprintf(`{"enabled":true,"members":[{"model_id":%d}]}`, m1.ID), tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable pool3 = %d, want 200", resp.StatusCode)
	}
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool3.ID), `{"members":[]}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("empty pool3 = %d, want 200", resp.StatusCode)
	}
	resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool3.ID), `{"enabled":true}`, tok)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("enabled echo on emptied group = %d, want 409", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "no members") {
		t.Errorf("enabled echo message = %s, want the enable-rule string", b)
	}
	resp.Body.Close()

	// Member already in another group → 409; duplicate member listing → 409;
	// unknown member model → 404.
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool2.ID),
		fmt.Sprintf(`{"members":[{"model_id":%d}]}`, m1.ID), tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim m1 for pool2 = %d, want 200", resp.StatusCode)
	}
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool.ID),
		fmt.Sprintf(`{"members":[{"model_id":%d}]}`, m1.ID), tok); resp.StatusCode != http.StatusConflict {
		t.Errorf("model in another group = %d, want 409", resp.StatusCode)
	}
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool.ID),
		fmt.Sprintf(`{"members":[{"model_id":%d},{"model_id":%d}]}`, m1.ID, m1.ID), tok); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate member = %d, want 409", resp.StatusCode)
	}
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool.ID), `{"members":[{"model_id":9999}]}`, tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown member model = %d, want 404", resp.StatusCode)
	}
	// pool (empty) renaming onto a still-public model name → 409. (Renaming
	// onto pub-model would now pass: pool2's claim made it internal.)
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", pool.ID), `{"name":"other-model"}`, tok); resp.StatusCode != http.StatusConflict {
		t.Errorf("rename onto public model = %d, want 409", resp.StatusCode)
	}
}

func TestAdminModelVisibility(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	m := newCatalogModel(t, e, "node-a", "vis-model")

	patch := func(body string) (int, store.Model) {
		resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID), body, tok)
		var out store.Model
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp.StatusCode, out
	}

	if code, out := patch(`{"visibility":"internal"}`); code != http.StatusOK || out.Visibility != "internal" {
		t.Fatalf("set internal = %d %q", code, out.Visibility)
	}
	if code, out := patch(`{"visibility":"public"}`); code != http.StatusOK || out.Visibility != "public" {
		t.Fatalf("set public = %d %q", code, out.Visibility)
	}
	if code, _ := patch(`{"visibility":"bogus"}`); code != http.StatusBadRequest {
		t.Errorf("bogus visibility = %d, want 400", code)
	}
	// Unknown model → 404.
	if resp := e.do(t, "PATCH", "/api/admin/models/9999", `{"visibility":"internal"}`, tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown model = %d, want 404", resp.StatusCode)
	}

	// Re-exposing a serving group's member as public is allowed (the
	// resolution layer keeps the group in front while it serves).
	resp := e.do(t, "POST", "/api/admin/groups", `{"name":"vis-pool"}`, tok)
	var g groupResp
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", g.ID),
		fmt.Sprintf(`{"enabled":true,"members":[{"model_id":%d}]}`, m.ID), tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable member group = %d, want 200", resp.StatusCode)
	}
	if code, out := patch(`{"visibility":"public"}`); code != http.StatusOK || out.Visibility != "public" {
		t.Fatalf("re-expose member = %d %q, want 200 public", code, out.Visibility)
	}
}

// GET /api/models (the dashboard catalog) mirrors the proxy's /v1/models:
// an enabled group appears as a kind=group row and its internal members
// stay hidden — the group row stands for the whole pool.
func TestPublicModelsShowsGroups(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	member := newCatalogModel(t, e, "node-a", "qwen-a")
	solo := newCatalogModel(t, e, "node-b", "solo-model")

	resp := e.do(t, "POST", "/api/admin/groups", `{"name":"qwen3-coder"}`, tok)
	var g groupResp
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", g.ID),
		fmt.Sprintf(`{"enabled":true,"members":[{"model_id":%d}]}`, member.ID), tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable group = %d, want 200", resp.StatusCode)
	}

	resp = e.do(t, "GET", "/api/models", "", tok)
	var out struct {
		Models []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	byName := map[string]string{}
	for _, m := range out.Models {
		byName[m.Name] = m.Kind
	}
	if byName["qwen3-coder"] != "group" {
		t.Errorf("qwen3-coder kind = %q, want group (entries: %+v)", byName["qwen3-coder"], out.Models)
	}
	if _, ok := byName["qwen-a"]; ok {
		t.Errorf("internal member qwen-a leaked into the public catalog: %+v", out.Models)
	}
	if byName[solo.Name] != "model" {
		t.Errorf("%s kind = %q, want model", solo.Name, byName[solo.Name])
	}

	// Disabling the group drops the row again (members stay internal, so
	// nothing takes its place).
	if resp := e.do(t, "PATCH", pathf("/api/admin/groups/%d", g.ID), `{"enabled":false}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable group = %d", resp.StatusCode)
	}
	resp = e.do(t, "GET", "/api/models", "", tok)
	out.Models = nil
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	for _, m := range out.Models {
		if m.Name == "qwen3-coder" || m.Name == "qwen-a" {
			t.Errorf("disabled group or its member still listed: %+v", out.Models)
		}
	}
}
