// Audit read endpoint + user-delete flow (plan 2026-11-25-audit-log-v1).
package api_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
)

func TestAuditEndpoint(t *testing.T) {
	e := newTestEnv(t, nil)
	admin := loginAs(t, e, "admin", "adminpass1")

	// A mutation the endpoint must have recorded.
	resp := e.do(t, "POST", "/api/admin/users", `{"username":"carol","password":"carolpass1","role":"user"}`, admin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user = %d", resp.StatusCode)
	}

	resp = e.do(t, "GET", "/api/admin/audit?page_size=10", "", admin)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit = %d", resp.StatusCode)
	}
	var out struct {
		Entries []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Target string `json:"target"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Total < 1 || len(out.Entries) == 0 {
		t.Fatalf("audit empty after a mutation: %+v", out)
	}
	top := out.Entries[0]
	if top.Actor != "admin" || top.Action != "user.create" || top.Target != "carol" {
		t.Errorf("top entry = %+v, want admin/user.create/carol", top)
	}

	// Category filter narrows to nothing when no such action exists yet.
	resp = e.do(t, "GET", "/api/admin/audit?category=tls", "", admin)
	resp.Body.Close()
	var tlsOut struct {
		Total int `json:"total"`
	}
	r2 := e.do(t, "GET", "/api/admin/audit?category=tls", "", admin)
	json.NewDecoder(r2.Body).Decode(&tlsOut)
	r2.Body.Close()
	if tlsOut.Total != 0 {
		t.Errorf("tls category = %d, want 0", tlsOut.Total)
	}

	// Regular users cannot read the trail.
	hash, _ := auth.HashPassword("userpass1")
	if _, err := store.CreateUser(e.db, "viewer", "", hash, "user"); err != nil {
		t.Fatal(err)
	}
	viewer := loginAs(t, e, "viewer", "userpass1")
	resp = e.do(t, "GET", "/api/admin/audit", "", viewer)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("user audit read = %d, want 403", resp.StatusCode)
	}
}

func TestAdminDeleteUserFlow(t *testing.T) {
	e := newTestEnv(t, nil)
	admin := loginAs(t, e, "admin", "adminpass1")

	resp := e.do(t, "POST", "/api/admin/users", `{"username":"gone","password":"gonepass1","role":"user"}`, admin)
	var created struct {
		ID int `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Self-deletion is refused (the admin's own id).
	resp = e.do(t, "GET", "/api/me", "", admin)
	var me struct {
		ID int `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	resp = e.do(t, "DELETE", "/api/admin/users/"+itoa(me.ID), "", admin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("self delete = %d, want 400", resp.StatusCode)
	}
	// The last enabled admin cannot be deleted either way — same guard.
	resp = e.do(t, "DELETE", "/api/admin/users/"+itoa(created.ID), "", admin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	// And the deletion itself is in the trail.
	resp = e.do(t, "GET", "/api/admin/audit?category=user", "", admin)
	var out struct {
		Entries []struct {
			Action string `json:"action"`
			Target string `json:"target"`
		} `json:"entries"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	found := false
	for _, en := range out.Entries {
		if en.Action == "user.delete" && en.Target == "gone" {
			found = true
		}
	}
	if !found {
		t.Errorf("user.delete not in the trail: %+v", out.Entries)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
