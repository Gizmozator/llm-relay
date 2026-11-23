// Regression tests for the task-3 review triage (plan 2026-11-22-relay-
// federation): the origin-derivation and status-direction security gates,
// the Reload/dialer lock-order deadlock, the null-vs-empty catalog prune
// rule, federated-group re-push stability, and the purge/synthetic-member
// store invariants.
package fed_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/fed"
	"llm-relay/internal/store"

	"github.com/coder/websocket"
)

// ------------------------------------------------------------ raw spoke

// rawSpoke dials the hub's connect endpoint with a raw WebSocket (as an
// enrolled peer's token holder) and completes the hello handshake. This is
// the only way a test can put hand-crafted frames — forged origins, forged
// status, malformed catalogs — on a hub channel; a real spoke never sends
// those.
func rawSpoke(t *testing.T, hubURL, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, hubURL+"/federation/v1/connect",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	raw, _ := fed.Encode(&fed.Hello{T: fed.THello, Proto: fed.Protocol, Name: "raw", Version: "raw"})
	if err := ws.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(data, &ack); err != nil || ack.T != fed.THelloAck {
		t.Fatalf("first frame = %s, want hello_ack", data)
	}
	return ws
}

// rawSend writes one encoded frame on a raw spoke socket.
func rawSend(t *testing.T, ws *websocket.Conn, f fed.Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := fed.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}

// rawReq sends one Req frame and reads the channel until the request's
// res_end, returning the served status and body (or the res_end error).
func rawReq(t *testing.T, ws *websocket.Conn, req *fed.Req) (int, string) {
	t.Helper()
	rawSend(t, ws, req)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status int
	var text strings.Builder
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		f, err := fed.Decode(data)
		if err != nil {
			continue
		}
		switch fr := f.(type) {
		case *fed.Res:
			status = fr.Status
		case *fed.ResBody:
			text.Write(fr.Data)
		case *fed.ResEnd:
			if fr.Error != "" {
				return status, fr.Error
			}
			return status, text.String()
		}
	}
}

func chatBody(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello there friend"}]}`)
}

// A1 (HIGH): OriginPeer is receiver-derived. A spoke (peer C) that claims
// another peer's id on a Req frame must NOT inherit that peer's sharing
// rights: the hub re-labels the frame with C, and the model shared only
// with B stays forbidden.
func TestForgedOriginPeerDenied(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	_, tokB := hub.enroll("spoke-b")
	_, tokC := hub.enroll("spoke-c")
	_ = tokB
	hub.serveConnect()
	peerB := mustPeerID(t, hub.db, "spoke-b")

	hub.shareLocal("b-only", "hub")
	if err := store.SetModelFedVisibility(hub.db, mustModelID(t, hub.db, "b-only"),
		store.FedVisAllowlist, []int64{peerB}); err != nil {
		t.Fatal(err)
	}
	hub.shareLocal("open", "hub")

	ws := rawSpoke(t, hub.srv.URL, tokC)
	// C forges OriginPeer=B: the hub must derive the origin from the
	// channel (C) and deny the B-only model.
	if status, body := rawReq(t, ws, &fed.Req{T: fed.TReq, ID: "forged-1",
		OriginPeer: peerB, Method: "POST", Path: "/v1/chat/completions",
		Body: chatBody("b-only")}); status != 403 || !strings.Contains(body, "not shared") {
		t.Fatalf("forged-origin request = %d %s, want 403 not-shared", status, body)
	}
	// Positive control: the same frame shape serves a model shared with
	// everyone — the 403 above is the gate, not a broken tunnel.
	if status, body := rawReq(t, ws, &fed.Req{T: fed.TReq, ID: "forged-2",
		OriginPeer: peerB, Method: "POST", Path: "/v1/chat/completions",
		Body: chatBody("open")}); status != 200 || !strings.Contains(body, "from-hub") {
		t.Fatalf("shared-model request = %d %s, want 200 from-hub", status, body)
	}
}

func mustPeerID(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM peers WHERE name=?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A2: Status frames flow hub→spoke only. A spoke pushing
// status{victim, offline} must not flip another peer's materialized
// backend at the hub — the hub's channels register no status handler.
func TestHubIgnoresInboundStatus(t *testing.T) {
	hub, spoke, peerA := federationUp(t)
	_, tokC := hub.enroll("spoke-c")
	ws := rawSpoke(t, hub.srv.URL, tokC)

	rawSend(t, ws, &fed.Status{T: fed.TStatus, PeerID: peerA.ID, Online: false})
	time.Sleep(300 * time.Millisecond) // a processed frame lands near-instantly

	if n := countRows(hub.db, `SELECT COUNT(*) FROM backends b
		JOIN models m ON m.backend_id=b.id
		WHERE m.name='alpha' AND b.base_url=? AND b.enabled=1`,
		store.PeerBackendURL(peerA.ID)); n != 1 {
		t.Fatalf("inbound status changed peer A's backend (enabled rows=%d, want 1)", n)
	}
	if resp, body := hub.chat("alpha", ""); resp.StatusCode != 200 {
		t.Fatalf("alpha after forged status = %d %s, want 200", resp.StatusCode, body)
	}
	_ = spoke
}

// B1 (CRITICAL): Reload stops the spoke dialer OUTSIDE m.mu. The dialer's
// pump dispatches frame handlers that take m.mu (served registry, role),
// so a stop under the lock deadlocks Reload against its own channel. A
// Reload storm while catalog and req frames flow must terminate.
func TestReloadStormUnderTraffic(t *testing.T) {
	hub, spoke, peer := federationUp(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 25; i++ {
			spoke.mgr.Reload()
		}
	}()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-done:
			poll(t, "spoke reconnects after the storm", func() bool { return hub.mgr.Online(peer.ID) })
			poll(t, "alpha serves after the storm", func() bool {
				resp, _ := hub.chat("alpha", "")
				return resp.StatusCode == 200
			})
			return
		case <-deadline:
			t.Fatal("Reload storm did not terminate (dialer.stop deadlocked under m.mu?)")
		default:
		}
		hub.mgr.CatalogChanged() // hub→spoke catalog frames: handlers take m.mu
		hub.chat("alpha", "")    // hub→spoke req frames: serveFrame takes m.mu
		time.Sleep(5 * time.Millisecond)
	}
}

// C1: on the wire, null means "this frame carries no data" (skip, never
// prune) and [] means "shares nothing" (prune). Both sections always
// serialize, and the spoke honors the distinction.
func TestCatalogNullVsEmptyPruneRule(t *testing.T) {
	raw, err := json.Marshal(&fed.Catalog{T: fed.TCatalog,
		Models: []fed.CatalogModel{}, Groups: []fed.CatalogGroup{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"models":[]`) || !strings.Contains(string(raw), `"groups":[]`) {
		t.Fatalf("empty catalog sections must serialize as [], got %s", raw)
	}

	hub, spoke, peer := federationUp(t)
	hub.shareLocal("m1", "hub")
	g, err := store.CreateGroup(hub.db, "pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(hub.db, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: mustModelID(t, hub.db, "m1"), Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	pollLog(t, spoke, hub)("pool + m1 materialized on spoke", func() bool {
		return countRows(spoke.db, `SELECT COUNT(*) FROM model_groups WHERE name='pool' AND federated=1`) == 1 &&
			countRows(spoke.db, `SELECT COUNT(*) FROM models WHERE name='m1'`) == 1
	})
	// Take the group out of the hub's catalog WITHOUT triggering a push, so
	// the next frame on the channel is exactly what this test injects.
	if _, err := store.UpdateGroup(hub.db, g.ID, nil, ptrBool(false), nil); err != nil {
		t.Fatal(err)
	}

	// null = no data: neither section prunes.
	if err := hub.mgr.Send(peer.ID, &fed.Catalog{T: fed.TCatalog}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := countRows(spoke.db, `SELECT COUNT(*) FROM model_groups WHERE name='pool' AND federated=1`); n != 1 {
		t.Fatalf("null-groups frame pruned the federated group (%d rows, want 1)", n)
	}
	if n := countRows(spoke.db, `SELECT COUNT(*) FROM models WHERE name='m1'`); n != 1 {
		t.Fatalf("null-models frame pruned materialized models (%d rows, want 1)", n)
	}

	// [] = shares nothing: both sections prune.
	if err := hub.mgr.Send(peer.ID, &fed.Catalog{T: fed.TCatalog,
		Models: []fed.CatalogModel{}, Groups: []fed.CatalogGroup{}}); err != nil {
		t.Fatal(err)
	}
	pollLog(t, spoke, hub)("empty-list frame pruned pool and m1", func() bool {
		return countRows(spoke.db, `SELECT COUNT(*) FROM model_groups WHERE name='pool'`) == 0 &&
			countRows(spoke.db, `SELECT COUNT(*) FROM models WHERE name='m1'`) == 0
	})
}

// C2: re-applying a federated group's own frame must resolve its current
// members (they are, by definition, in that group). The old resolver
// excluded rows in ANY group — including the one being re-applied — so a
// second frame re-resolved the member to the materialized row and rewrote
// the membership, silently breaking local-first.
func TestFederatedGroupRepushKeepsLocalFirst(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	hub.shareLocal("m1", "hub-eng")
	g, err := store.CreateGroup(hub.db, "pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(hub.db, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: mustModelID(t, hub.db, "m1"), Weight: 1}}); err != nil {
		t.Fatal(err)
	}

	spoke := newRelay(t, "spoke-a")
	local := newEngine(t, "spoke-eng", 0)
	sb, err := store.CreateBackend(spoke.db, "local", local.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	lm, err := store.CreateModel(spoke.db, sb.ID, "m1-up", "m1")
	if err != nil {
		t.Fatal(err)
	}
	// Hide the local copy so the materialized hub:// row also exists: the
	// second frame then has a wrong-but-resolvable alternative to degrade
	// to — exactly what the old resolver picked.
	if _, err := store.UpdateModelVisibility(spoke.db, lm.ID, "internal"); err != nil {
		t.Fatal(err)
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	pollLog(t, spoke, hub)("pool materialized with the local m1", func() bool {
		var n int
		err := spoke.db.QueryRow(`SELECT COUNT(*) FROM model_group_members gm
			JOIN model_groups g ON g.id=gm.group_id WHERE g.name='pool' AND gm.model_id=?`,
			lm.ID).Scan(&n)
		return err == nil && n == 1
	})

	hub.mgr.CatalogChanged() // force a second hub frame over the live channel
	time.Sleep(500 * time.Millisecond)

	var backend string
	if err := spoke.db.QueryRow(`SELECT b.base_url FROM model_group_members gm
		JOIN model_groups g ON g.id=gm.group_id
		JOIN models m ON m.id=gm.model_id
		JOIN backends b ON b.id=m.backend_id WHERE g.name='pool'`).Scan(&backend); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(backend, "hub://") || strings.HasPrefix(backend, "peer://") {
		t.Errorf("group member moved to %s after re-push, want the local backend", backend)
	}
	if resp, body := spoke.chat("pool", "sess-1"); resp.StatusCode != 200 ||
		!strings.Contains(string(body), "from-spoke-eng") {
		t.Fatalf("group chat after re-push = %d %s, want 200 from-spoke-eng", resp.StatusCode, body)
	}
	_ = peer
}

// C5: leaving the federation must undo what a federated group did to a
// local model — PurgeSynthetic restores members' public visibility (the
// mirror of UpsertFederatedGroup flipping them internal).
func TestPurgeSyntheticRestoresMemberVisibility(t *testing.T) {
	d := migratedDB(t)
	b, err := store.CreateBackend(d, "local", "http://127.0.0.1:1/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "m1-up", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFederatedGroup(d, "pool", []int64{m.ID}, []float64{1}); err != nil {
		t.Fatal(err)
	}
	if v := modelVisibility(t, d, m.ID); v != "internal" {
		t.Fatalf("group member visibility = %s, want internal", v)
	}
	if err := store.PurgeSynthetic(d); err != nil {
		t.Fatal(err)
	}
	if v := modelVisibility(t, d, m.ID); v != "public" {
		t.Fatalf("after purge visibility = %s, want public (leaving must undo the group's claim)", v)
	}
}

func modelVisibility(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var v string
	if err := db.QueryRow(`SELECT visibility FROM models WHERE id=?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// C6: materialized models are owned by the materializer — an admin group
// cannot adopt them (the row may be pruned or re-created under it).
func TestUpdateGroupRejectsSyntheticMember(t *testing.T) {
	d := migratedDB(t)
	sb, err := store.EnsureSyntheticBackend(d, "peer 7", store.PeerBackendURL(7))
	if err != nil {
		t.Fatal(err)
	}
	sm, err := store.CreateModel(d, sb.ID, "sm-up", "sm")
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.CreateGroup(d, "adm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: sm.ID, Weight: 1}}); !errors.Is(err, store.ErrSyntheticMember) {
		t.Fatalf("UpdateGroup with synthetic member = %v, want ErrSyntheticMember", err)
	}
	// The guard must not bite ordinary local models.
	b, err := store.CreateBackend(d, "loc", "http://127.0.0.1:1/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	lm, err := store.CreateModel(d, b.ID, "lm-up", "lm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(d, g.ID, nil, ptrBool(true),
		[]store.GroupMemberInput{{ModelID: lm.ID, Weight: 1}}); err != nil {
		t.Fatalf("local member rejected: %v", err)
	}
}

// E2: a channel dying aborts everything it was serving — the served
// request's upstream sees a context cancellation the moment the socket
// goes away, not after the generation completes into a dead pipe.
func TestChannelDeathAbortsServed(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	spoke := newRelay(t, "spoke-a")
	e := newEngine(t, "spoke-a", 5*time.Second)
	b, err := store.CreateBackend(spoke.db, "slow", e.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(spoke.db, b.ID, "slow-up", "slow")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelFedVisibility(spoke.db, m.ID, store.FedVisAll, nil); err != nil {
		t.Fatal(err)
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })
	poll(t, "slow materialized", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM models WHERE name='slow'`) == 1
	})

	go hub.chat("slow", "") // abandoned mid-generation by the kick below
	poll(t, "engine started", func() bool { return e.hits.Load() == 1 })
	hub.mgr.KickPeer(peer.ID, "flap")
	poll(t, "served upstream aborted with the channel", func() bool { return e.canceled.Load() == 1 })
}

// E2 (shutdown half): Close cancels the served requests and waits for
// them with a bound — shutdown does not block on a 5-second upstream
// generation that can no longer deliver its answer.
func TestCloseAbortsServedRequests(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("spoke-a")
	hub.serveConnect()
	spoke := newRelay(t, "spoke-a")
	e := newEngine(t, "spoke-a", 5*time.Second)
	b, err := store.CreateBackend(spoke.db, "slow", e.srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(spoke.db, b.ID, "slow-up", "slow")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelFedVisibility(spoke.db, m.ID, store.FedVisAll, nil); err != nil {
		t.Fatal(err)
	}
	spoke.becomeSpoke(hub.srv.URL, tok)
	poll(t, "peer online", func() bool { return hub.mgr.Online(peer.ID) })
	poll(t, "slow materialized", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM models WHERE name='slow'`) == 1
	})

	go hub.chat("slow", "")
	poll(t, "engine started", func() bool { return e.hits.Load() == 1 })
	start := time.Now()
	spoke.mgr.Close()
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Close waited %v, want the bounded served drain", d)
	}
	poll(t, "upstream aborted at shutdown", func() bool { return e.canceled.Load() == 1 })
}
