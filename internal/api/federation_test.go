// Federation API surfaces (plan 2026-11-22-relay-federation, task 1):
// self-service peers, admin peer management, the federation settings pair,
// the model sharing PATCH, and the token type-scoping matrix (API keys
// cannot touch /federation/*; the /v1 side of that proof lives in
// internal/proxy).
package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
)

// userToken creates a regular user through the store and returns its JWT.
func userToken(t *testing.T, e *testEnv, name string) string {
	t.Helper()
	hash, _ := auth.HashPassword("userpass1")
	if _, err := store.CreateUser(e.db, name, "", hash, "user"); err != nil {
		t.Fatal(err)
	}
	resp := e.do(t, "POST", "/api/auth/login", `{"username":"`+name+`","password":"userpass1"}`, "")
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Token == "" {
		t.Fatal("no token for " + name)
	}
	return out.Token
}

// createPeer creates a peer through the self-service endpoint and returns
// (id, raw token). The create response is the ONLY place the raw token
// appears — the tests lean on that.
func createPeer(t *testing.T, e *testEnv, tok, name string) (int64, string) {
	t.Helper()
	resp := e.do(t, "POST", "/api/peers", `{"name":"`+name+`"}`, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create peer = %d, want 201", resp.StatusCode)
	}
	var out struct {
		Peer struct {
			ID int64 `json:"id"`
		} `json:"peer"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Token, "fed-") {
		t.Fatalf("token %q, want fed- prefix", out.Token)
	}
	return out.Peer.ID, out.Token
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

func TestPeerSelfService(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	alice := userToken(t, e, "alice")
	bob := userToken(t, e, "bob")

	// Create: raw token shown once, peer listed under the owner.
	id, token := createPeer(t, e, alice, "home")
	resp := e.do(t, "GET", "/api/peers", "", alice)
	body := bodyString(t, resp)
	var list struct {
		Peers []store.Peer `json:"peers"`
	}
	json.Unmarshal([]byte(body), &list)
	if len(list.Peers) != 1 || list.Peers[0].Name != "home" || list.Peers[0].OwnerName != "alice" {
		t.Fatalf("alice's list = %+v", list.Peers)
	}
	// The list must not carry the raw token (or any hash of it).
	if strings.Contains(body, token) {
		t.Error("peer list leaked the raw token")
	}

	// Bob sees none of it.
	resp = e.do(t, "GET", "/api/peers", "", bob)
	body = bodyString(t, resp)
	list.Peers = nil
	json.Unmarshal([]byte(body), &list)
	if len(list.Peers) != 0 {
		t.Errorf("bob's list = %s, want empty", body)
	}

	// Cross-user isolation: bob cannot rotate or delete alice's peer —
	// and gets a plain 404 (no existence leak, the RevokeKey pattern).
	if resp := e.do(t, "POST", pathf("/api/peers/%d/rotate", id), "", bob); resp.StatusCode != http.StatusNotFound {
		bodyString(t, resp)
		t.Errorf("bob rotate alice peer = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "DELETE", pathf("/api/peers/%d", id), "", bob); resp.StatusCode != http.StatusNotFound {
		bodyString(t, resp)
		t.Errorf("bob delete alice peer = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Rotate (own): new token shown once, the rotated-away token dies.
	resp = e.do(t, "POST", pathf("/api/peers/%d/rotate", id), "", alice)
	var rot struct {
		Token string `json:"token"`
	}
	body = bodyString(t, resp)
	json.Unmarshal([]byte(body), &rot)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(rot.Token, "fed-") || rot.Token == token {
		t.Fatalf("rotate = %d %q", resp.StatusCode, rot.Token)
	}
	if resp := e.do(t, "GET", "/federation/v1/ping", "", token); resp.StatusCode != http.StatusUnauthorized {
		bodyString(t, resp)
		t.Error("rotated-away token still authenticates")
	} else {
		resp.Body.Close()
	}

	// Delete (own).
	if resp := e.do(t, "DELETE", pathf("/api/peers/%d", id), "", alice); resp.StatusCode != http.StatusNoContent {
		bodyString(t, resp)
		t.Errorf("delete = %d, want 204", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestAdminPeerManagement(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	alice := userToken(t, e, "alice")
	_, _ = createPeer(t, e, alice, "home")

	// Admin list: every peer with owner name.
	resp := e.do(t, "GET", "/api/admin/peers", "", tok)
	var list struct {
		Peers []store.Peer `json:"peers"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Peers) != 1 || list.Peers[0].OwnerName != "alice" {
		t.Fatalf("admin list = %+v", list.Peers)
	}
	id := list.Peers[0].ID

	// A regular user cannot reach the admin surface.
	if resp := e.do(t, "GET", "/api/admin/peers", "", alice); resp.StatusCode != http.StatusForbidden {
		bodyString(t, resp)
		t.Errorf("user on admin peers = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Disable -> the (freshly rotated) token is refused 403, distinct
	// from unknown-token 401.
	if resp := e.do(t, "PATCH", pathf("/api/admin/peers/%d", id), `{"enabled":false}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable = %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	rotResp := e.do(t, "POST", pathf("/api/admin/peers/%d/rotate", id), "", tok)
	var rot struct {
		Token string `json:"token"`
	}
	json.NewDecoder(rotResp.Body).Decode(&rot)
	rotResp.Body.Close()
	if rot.Token == "" {
		t.Fatal("admin rotate returned no token")
	}
	if resp := e.do(t, "GET", "/federation/v1/ping", "", rot.Token); resp.StatusCode != http.StatusForbidden {
		bodyString(t, resp)
		t.Errorf("disabled peer ping = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Re-enable -> live again; the ping stub names the peer.
	if resp := e.do(t, "PATCH", pathf("/api/admin/peers/%d", id), `{"enabled":true}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable = %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	resp = e.do(t, "GET", "/federation/v1/ping", "", rot.Token)
	var pong map[string]any
	body := bodyString(t, resp)
	json.Unmarshal([]byte(body), &pong)
	if resp.StatusCode != http.StatusOK || pong["pong"] != true || pong["peer_name"] != "home" {
		t.Errorf("ping after enable = %d %s", resp.StatusCode, body)
	}

	// Admin delete, then 404.
	if resp := e.do(t, "DELETE", pathf("/api/admin/peers/%d", id), "", tok); resp.StatusCode != http.StatusNoContent {
		bodyString(t, resp)
		t.Errorf("admin delete = %d, want 204", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "DELETE", pathf("/api/admin/peers/%d", id), "", tok); resp.StatusCode != http.StatusNotFound {
		bodyString(t, resp)
		t.Errorf("double delete = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestFederationSettingsAPI(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	alice := userToken(t, e, "alice")

	// Default read; a user can neither read nor write the pair.
	resp := e.do(t, "GET", "/api/admin/federation", "", tok)
	var f store.FederationSettings
	json.NewDecoder(resp.Body).Decode(&f)
	resp.Body.Close()
	if f.Role != "standalone" {
		t.Fatalf("default = %+v", f)
	}
	if resp := e.do(t, "GET", "/api/admin/federation", "", alice); resp.StatusCode != http.StatusForbidden {
		bodyString(t, resp)
		t.Errorf("user GET federation = %d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "PUT", "/api/admin/federation", `{"role":"hub"}`, alice); resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
	} else {
		resp.Body.Close()
	}

	// Validation taxonomy: bad role and spoke-without-token are 400s.
	if resp := e.do(t, "PUT", "/api/admin/federation", `{"role":"wizard"}`, tok); resp.StatusCode != http.StatusBadRequest {
		bodyString(t, resp)
		t.Error("bad role accepted")
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "PUT", "/api/admin/federation", `{"role":"spoke","hub_url":"https://h.fi"}`, tok); resp.StatusCode != http.StatusBadRequest {
		bodyString(t, resp)
		t.Error("spoke without token accepted")
	} else {
		resp.Body.Close()
	}

	// Spoke round-trip, then leaving spoke clears the config.
	if resp := e.do(t, "PUT", "/api/admin/federation",
		`{"role":"spoke","hub_url":"https://hub.example.fi","hub_token":"fed-abc"}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid spoke PUT = %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	resp = e.do(t, "GET", "/api/admin/federation", "", tok)
	json.NewDecoder(resp.Body).Decode(&f)
	resp.Body.Close()
	if f.Role != "spoke" || f.HubURL != "https://hub.example.fi" || f.HubToken != "fed-abc" {
		t.Errorf("spoke readback = %+v", f)
	}
	if resp := e.do(t, "PUT", "/api/admin/federation", `{"role":"standalone"}`, tok); resp.StatusCode != http.StatusOK {
		bodyString(t, resp)
	} else {
		resp.Body.Close()
	}
	resp = e.do(t, "GET", "/api/admin/federation", "", tok)
	json.NewDecoder(resp.Body).Decode(&f)
	resp.Body.Close()
	if f.Role != "standalone" || f.HubURL != "" || f.HubToken != "" {
		t.Errorf("after clear = %+v", f)
	}
}

// Disabling a user revokes their fed- tokens immediately — the KeyByHash
// contract (auth re-checks the live owner row; store.GetPeerByKeyHash
// returns ErrNotFound for a disabled owner).
func TestPeerAuthRevokedOnUserDisable(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	hash, _ := auth.HashPassword("userpass1")
	u, err := store.CreateUser(e.db, "alice", "", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	resp := e.do(t, "POST", "/api/auth/login", `{"username":"alice","password":"userpass1"}`, "")
	var loginOut struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&loginOut)
	resp.Body.Close()
	if loginOut.Token == "" {
		t.Fatal("no token for alice")
	}
	_, fed := createPeer(t, e, loginOut.Token, "home")

	dis := true
	if _, err := store.UpdateUser(e.db, u.ID, nil, &dis, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(t, "GET", "/federation/v1/ping", "", fed); resp.StatusCode != http.StatusUnauthorized {
		bodyString(t, resp)
		t.Errorf("disabled owner ping = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	dis = false
	if _, err := store.UpdateUser(e.db, u.ID, nil, &dis, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(t, "GET", "/federation/v1/ping", "", fed); resp.StatusCode != http.StatusOK {
		bodyString(t, resp)
		t.Errorf("re-enabled owner ping = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestFederationAuthMatrix(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	alice := userToken(t, e, "alice")

	// An sk- API key must NOT authenticate /federation/*: server tokens
	// and API keys live in different tables, so the scoping is structural
	// (plan Decision 3) — proven here from the client-key side.
	keyResp := e.do(t, "POST", "/api/keys", `{"name":"client"}`, alice)
	var key struct {
		Key string `json:"key"`
	}
	json.NewDecoder(keyResp.Body).Decode(&key)
	keyResp.Body.Close()
	if !strings.HasPrefix(key.Key, "sk-") {
		t.Fatalf("api key = %q", key.Key)
	}
	if resp := e.do(t, "GET", "/federation/v1/ping", "", key.Key); resp.StatusCode != http.StatusUnauthorized {
		bodyString(t, resp)
		t.Errorf("sk- key on /federation = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// fed- token authenticates the stub; anonymous does not.
	_, fed := createPeer(t, e, alice, "spoke-one")
	if resp := e.do(t, "GET", "/federation/v1/ping", "", fed); resp.StatusCode != http.StatusOK {
		bodyString(t, resp)
		t.Errorf("fed token ping = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := e.do(t, "GET", "/federation/v1/ping", "", ""); resp.StatusCode != http.StatusUnauthorized {
		bodyString(t, resp)
		t.Errorf("anonymous ping = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestModelFedVisibilityPATCH(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)
	userToken(t, e, "alice")
	au, _, err := store.UserByName(e.db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	userToken(t, e, "bob")
	bu, _, err := store.UserByName(e.db, "bob")
	if err != nil {
		t.Fatal(err)
	}
	m := newCatalogModel(t, e, "node-a", "qwen")
	ids := strconv.FormatInt(au.ID, 10)

	// Default off on the admin row.
	if m.FedVisibility != "off" {
		t.Fatalf("fresh model fed_visibility = %q", m.FedVisibility)
	}

	// Set allowlist with the user; the row surfaces both fields.
	resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID),
		`{"fed_visibility":"allowlist","fed_user_ids":[`+ids+`]}`, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlist PATCH = %d %s", resp.StatusCode, bodyString(t, resp))
	}
	var got store.Model
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.FedVisibility != "allowlist" || len(got.FedUserIDs) != 1 || got.FedUserIDs[0] != au.ID {
		t.Errorf("model = %q/%v", got.FedVisibility, got.FedUserIDs)
	}

	// fed_user_ids without allowlist mode -> 400; unknown user -> 400;
	// bad visibility -> 400.
	for _, body := range []string{
		`{"fed_user_ids":[1]}`,
		`{"fed_visibility":"allowlist","fed_user_ids":[9999]}`,
		`{"fed_visibility":"yes"}`,
	} {
		if resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID), body, tok); resp.StatusCode != http.StatusBadRequest {
			bodyString(t, resp)
			t.Errorf("PATCH %s = %d, want 400", body, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}

	// 'owner' mode (ticket #21) is accepted and carries no list.
	if resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID),
		`{"fed_visibility":"owner"}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("owner PATCH = %d %s", resp.StatusCode, bodyString(t, resp))
	} else {
		resp.Body.Close()
	}

	// Back to off clears the allowlist.
	resp = e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID), `{"fed_visibility":"off"}`, tok)
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.FedVisibility != "off" || len(got.FedUserIDs) != 0 {
		t.Errorf("after off = %q/%v", got.FedVisibility, got.FedUserIDs)
	}

	// Validation runs before any mutation: a bogus fed_visibility must not
	// rename the model on the way to the 400 (validate-then-mutate).
	if resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID),
		`{"name":"renamed","fed_visibility":"bogus"}`, tok); resp.StatusCode != http.StatusBadRequest {
		bodyString(t, resp)
		t.Fatalf("bogus fed_visibility = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if mm, err := store.ModelByID(e.db, m.ID); err != nil || mm.Name != "qwen" {
		t.Errorf("rename committed despite 400: %+v, %v", mm, err)
	}

	// PATCH semantics: switching to allowlist without naming fed_user_ids
	// keeps the stored list (unmentioned fields do not change); switching
	// to all/off clears it on purpose (those modes share with
	// everybody/nobody — a stale list would be a lie).
	if resp := e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID),
		`{"fed_visibility":"allowlist","fed_user_ids":[`+ids+`,`+strconv.FormatInt(bu.ID, 10)+`]}`, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("seed allowlist = %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	resp = e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID), `{"fed_visibility":"allowlist"}`, tok)
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.FedVisibility != "allowlist" || len(got.FedUserIDs) != 2 {
		t.Errorf("allowlist-only PATCH wiped the list: %q/%v", got.FedVisibility, got.FedUserIDs)
	}
	resp = e.do(t, "PATCH", pathf("/api/admin/models/%d", m.ID), `{"fed_visibility":"all"}`, tok)
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.FedVisibility != "all" || len(got.FedUserIDs) != 0 {
		t.Errorf("switch to all should clear the list: %q/%v", got.FedVisibility, got.FedUserIDs)
	}
}

// The three thin status surfaces banked from the task-5 review: the
// session-auth role endpoint (setup-guide gate), the spoke branch of the
// admin federation GET, and the online stamp on the admin peer list. The
// live-channel half drives a real dialer→hub channel between two wired
// instances (the same Manager the ws tests exercise).

func TestFedRoleEndpoint(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	alice := userToken(t, e, "alice")

	// Session-auth: anonymous is refused (this is not a public probe).
	if resp := e.do(t, "GET", "/api/federation/role", "", ""); resp.StatusCode != http.StatusUnauthorized {
		bodyString(t, resp)
		t.Errorf("anonymous role = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Standalone: exactly one field, the role.
	var out map[string]string
	resp := e.do(t, "GET", "/api/federation/role", "", alice)
	body := bodyString(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("role = %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out["role"] != "standalone" || len(out) != 1 {
		t.Fatalf("standalone payload = %s, want {\"role\":\"standalone\"} only", body)
	}

	// Spoke role: still role-only — the stored hub_url/token must not ride
	// this endpoint (it is readable by every user, the admin pair is not).
	setFedRole(t, e, admin, "spoke", "https://127.0.0.1:1", "fed-secret-token", "")
	resp = e.do(t, "GET", "/api/federation/role", "", alice)
	body = bodyString(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spoke role = %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "127.0.0.1") || strings.Contains(body, "fed-secret-token") {
		t.Errorf("role endpoint leaked spoke config: %s", body)
	}
	out = map[string]string{}
	json.Unmarshal([]byte(body), &out)
	if out["role"] != "spoke" || len(out) != 1 {
		t.Errorf("spoke payload = %s", body)
	}

	// The admin GET's spoke branch while the dialer cannot reach anything:
	// the key is present, connected=false, identity empty.
	resp = e.do(t, "GET", "/api/admin/federation", "", admin)
	var st struct {
		Role  string `json:"role"`
		Spoke *struct {
			Connected  bool   `json:"connected"`
			HubName    string `json:"hub_name"`
			HubVersion string `json:"hub_version"`
		} `json:"spoke"`
	}
	body = bodyString(t, resp)
	json.Unmarshal([]byte(body), &st)
	if st.Role != "spoke" || st.Spoke == nil || st.Spoke.Connected ||
		st.Spoke.HubName != "" || st.Spoke.HubVersion != "" {
		t.Errorf("disconnected spoke branch = %s", body)
	}
	// The role endpoint follows the stored role for admins too.
	if resp := e.do(t, "GET", "/api/federation/role", "", admin); resp.StatusCode != http.StatusOK {
		bodyString(t, resp)
		t.Error("admin role read failed")
	} else {
		resp.Body.Close()
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

func TestSpokeStatusAndPeerOnline(t *testing.T) {
	hub := newTestEnv(t, http.NotFoundHandler())
	hadmin := login(t, hub)
	sp := newTestEnv(t, http.NotFoundHandler())
	sadmin := login(t, sp)

	_, token := createPeer(t, hub, hadmin, "home")
	setFedRole(t, hub, hadmin, "hub", "", "", "")
	setFedRole(t, sp, sadmin, "spoke", hub.srv.URL, token, pinOf(hub))

	type spokeBranch struct {
		Connected  bool   `json:"connected"`
		HubName    string `json:"hub_name"`
		HubVersion string `json:"hub_version"`
	}
	readSpoke := func() *spokeBranch {
		resp := sp.do(t, "GET", "/api/admin/federation", "", sadmin)
		defer resp.Body.Close()
		var st struct {
			Spoke *spokeBranch `json:"spoke"`
		}
		json.NewDecoder(resp.Body).Decode(&st)
		return st.Spoke
	}
	readOnline := func() bool {
		resp := hub.do(t, "GET", "/api/admin/peers", "", hadmin)
		defer resp.Body.Close()
		var list struct {
			Peers []struct {
				Name   string `json:"name"`
				Online bool   `json:"online"`
			} `json:"peers"`
		}
		json.NewDecoder(resp.Body).Decode(&list)
		for _, p := range list.Peers {
			if p.Name == "home" {
				return p.Online
			}
		}
		return false
	}

	// Connect: the spoke branch flips to connected and carries the hub's
	// hello_ack identity (default manager version is "dev").
	waitFor(t, "spoke channel connected", func() bool {
		s := readSpoke()
		return s != nil && s.Connected
	})
	if s := readSpoke(); s.HubName == "" || s.HubVersion != "dev" {
		t.Errorf("hub identity = %+v, want hello_ack name + version \"dev\"", s)
	}
	// The hub stamps the same channel onto the peer row.
	waitFor(t, "peer online stamp", readOnline)

	// Disable the peer: the kick drops the channel, and both stamps flip
	// back (the spoke's reconnect attempts are refused 403 while disabled).
	pid := peerIDByName(t, hub, hadmin, "home")
	if resp := hub.do(t, "PATCH", "/api/admin/peers/"+strconv.FormatInt(pid, 10),
		`{"enabled":false}`, hadmin); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable = %s", bodyString(t, resp))
	} else {
		resp.Body.Close()
	}
	waitFor(t, "spoke branch flips to disconnected", func() bool {
		s := readSpoke()
		return s != nil && !s.Connected && s.HubName == ""
	})
	waitFor(t, "peer online stamp clears", func() bool { return !readOnline() })
}

// peerIDByName finds a peer's id through the admin list (tests create
// peers by name).
func peerIDByName(t *testing.T, e *testEnv, admin, name string) int64 {
	t.Helper()
	resp := e.do(t, "GET", "/api/admin/peers", "", admin)
	defer resp.Body.Close()
	var list struct {
		Peers []store.Peer `json:"peers"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	for _, p := range list.Peers {
		if p.Name == name {
			return p.ID
		}
	}
	t.Fatalf("peer %q not found", name)
	return 0
}
