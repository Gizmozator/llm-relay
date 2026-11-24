// Headless spoke end-to-end (plan 2026-11-24-headless-spoke): the bootstrap
// writes backend + model + federation settings in one shot, and the ordinary
// dialer/catalog code takes over — the single shared model must materialize
// on the hub with the env prices intact.
package fed_test

import (
	"log"
	"strings"
	"testing"

	"llm-relay/internal/headless"
	"llm-relay/internal/store"
)

func TestHeadlessSpokeMaterializesAtHub(t *testing.T) {
	hub := newRelay(t, "hub")
	hub.becomeHub()
	peer, tok := hub.enroll("headless-a")
	hub.serveConnect()

	spoke := newRelay(t, "headless-a")
	e := newEngine(t, "headless-a", 0)
	// The full boot path: Bootstrap owns the federation settings write, so
	// the test never touches becomeSpoke — Reload alone must start the
	// dialer with everything the env defined.
	if err := headless.Bootstrap(spoke.db, headless.Config{
		BackendURL: e.srv.URL, ModelName: "alpha",
		HubURL: hub.srv.URL, HubToken: tok,
		PriceIn: 100, PriceOut: 200,
	}, log.Printf); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	spoke.mgr.CatalogChanged()
	if err := spoke.mgr.Reload(); err != nil {
		t.Fatal(err)
	}

	poll(t, "peer online at hub", func() bool { return hub.mgr.Online(peer.ID) })
	pollLog(t, spoke, hub)("alpha materialized at hub", func() bool {
		return countRows(hub.db, `SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
			WHERE m.name='alpha' AND b.base_url=?`, store.PeerBackendURL(peer.ID)) == 1
	})

	// The env prices rode the catalog frame through to the materialized row.
	if n := countRows(hub.db, `SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.name='alpha' AND b.base_url=? AND m.price_in=100 AND m.price_out=200`,
		store.PeerBackendURL(peer.ID)); n != 1 {
		t.Errorf("materialized row with env prices: %d, want 1", n)
	}
	// And the hub lists + serves it like any other catalog row.
	resp, body := hub.get("/v1/models")
	if resp.StatusCode != 200 || !strings.Contains(modelsList(body), "alpha") {
		t.Fatalf("hub /v1/models = %d %s", resp.StatusCode, body)
	}
	resp, body = hub.chat("alpha", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from-headless-a") {
		t.Fatalf("tunneled chat = %d %s", resp.StatusCode, body)
	}
}
