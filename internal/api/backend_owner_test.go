// The backend owner field on the admin API (ticket #12): create/patch accept
// owner_user_id, the list returns it, clearing works, and a non-existent user
// is a 400 rather than a raw FK error.
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"llm-relay/internal/store"
)

type backendOwnerJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	OwnerUserID *int64 `json:"owner_user_id"`
}

func TestBackendOwnerAPI(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)

	carol, err := store.CreateUser(e.db, "carol", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser(e.db, "bob", "", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Create with an owner: the 201 body carries owner_user_id.
	resp := e.do(t, "POST", "/api/admin/backends",
		fmt.Sprintf(`{"name":"gpu","base_url":"http://gpu:8080","owner_user_id":%d}`, carol.ID), tok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, bodyString(t, resp))
	}
	var b backendOwnerJSON
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if b.OwnerUserID == nil || *b.OwnerUserID != carol.ID {
		t.Fatalf("create owner = %v, want carol(%d)", b.OwnerUserID, carol.ID)
	}

	// The list surfaces it.
	var list struct {
		Backends []backendOwnerJSON `json:"backends"`
	}
	if code := getJSON(t, e, "/api/admin/backends", tok, &list); code != 200 {
		t.Fatalf("list = %d", code)
	}
	var gpu *backendOwnerJSON
	for i := range list.Backends {
		if list.Backends[i].ID == b.ID {
			gpu = &list.Backends[i]
		}
	}
	if gpu == nil || gpu.OwnerUserID == nil || *gpu.OwnerUserID != carol.ID {
		t.Fatalf("list owner = %+v, want carol(%d)", gpu, carol.ID)
	}

	// Re-point the owner at another user.
	resp = e.do(t, "PATCH", fmt.Sprintf("/api/admin/backends/%d", b.ID),
		fmt.Sprintf(`{"owner_user_id":%d}`, bob.ID), tok)
	if resp.StatusCode != 200 {
		t.Fatalf("patch owner = %d %s", resp.StatusCode, bodyString(t, resp))
	}
	var patched backendOwnerJSON
	json.NewDecoder(resp.Body).Decode(&patched)
	resp.Body.Close()
	if patched.OwnerUserID == nil || *patched.OwnerUserID != bob.ID {
		t.Fatalf("after patch owner = %v, want bob(%d)", patched.OwnerUserID, bob.ID)
	}

	// owner_user_id: 0 clears it back to uncredited hub hardware.
	resp = e.do(t, "PATCH", fmt.Sprintf("/api/admin/backends/%d", b.ID), `{"owner_user_id":0}`, tok)
	if resp.StatusCode != 200 {
		t.Fatalf("clear owner = %d %s", resp.StatusCode, bodyString(t, resp))
	}
	var cleared backendOwnerJSON
	json.NewDecoder(resp.Body).Decode(&cleared)
	resp.Body.Close()
	if cleared.OwnerUserID != nil {
		t.Fatalf("after clear owner = %v, want null", *cleared.OwnerUserID)
	}

	// A non-existent owner is a 400 on both create and patch (not a 500 FK error).
	resp = e.do(t, "PATCH", fmt.Sprintf("/api/admin/backends/%d", b.ID), `{"owner_user_id":99999}`, tok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("patch bad owner = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp = e.do(t, "POST", "/api/admin/backends",
		`{"name":"gpu9","base_url":"http://gpu9:1","owner_user_id":99999}`, tok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create bad owner = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
