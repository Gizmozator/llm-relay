package fed_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/fed"
	"llm-relay/internal/store"
	"llm-relay/internal/version"
)

// helloVer runs the client side of the handshake with an explicit version
// stamp (clientHello hard-codes "testver", which is deliberately not a
// semver — this variant needs real ones).
func helloVer(t *testing.T, c *websocket.Conn, ver string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	msg, _ := fed.Encode(&fed.Hello{T: fed.THello, Proto: fed.Protocol,
		Name: "test-spoke", Version: ver})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
}

// A spoke whose hello version is older than the hub's shows up as an
// "older" warning (the Federation-page note), a newer one as "newer"
// (this hub is the outdated server). Unparseable stamps (dev builds,
// bare shas) carry no ordering and must never be flagged.
func TestHubVersionWarnings(t *testing.T) {
	for _, tc := range []struct {
		hubVer, spokeVer, want string
	}{
		{"v0.2.0", "v0.1.0", version.Older},
		{"v0.1.0", "v0.2.0", version.Newer},
		{"v0.1.0", "v0.1.0", version.Same},
		{"v0.1.0", "testver", version.Unknown},
	} {
		e := newHubEnv(t, &fed.Options{Version: tc.hubVer})
		e.setRole(t, store.FedRoleHub)
		c, err := wsDial(t, wsURL(e.srv), e.token, e.srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		helloVer(t, c, tc.spokeVer)
		defer c.CloseNow()
		// Registration lands right after the ack; wait for it.
		deadline := time.Now().Add(2 * time.Second)
		for !e.mgr.Online(e.peer.ID) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !e.mgr.Online(e.peer.ID) {
			t.Fatalf("hub %s spoke %s: peer not registered", tc.hubVer, tc.spokeVer)
		}
		got := e.mgr.VersionWarnings()
		if tc.want == version.Same || tc.want == version.Unknown {
			if len(got) != 0 {
				t.Errorf("hub %s vs spoke %s: warnings = %+v, want none", tc.hubVer, tc.spokeVer, got)
			}
			continue
		}
		if len(got) != 1 {
			t.Fatalf("hub %s vs spoke %s: warnings = %+v, want one", tc.hubVer, tc.spokeVer, got)
		}
		w := got[0]
		if w.Kind != "spoke" || w.State != tc.want || w.Version != tc.spokeVer {
			t.Errorf("hub %s vs spoke %s: warn = %+v, want spoke/%s/%s",
				tc.hubVer, tc.spokeVer, w, tc.want, tc.spokeVer)
		}
	}
}
