// TLS on the remote-backend channel (plan 2026-11-24-remote-tls-v1): the
// connector dials RELAY_URL over TLS and verifies the relay against
// RELAY_PIN. Correct pin → channel comes up; wrong pin or no pin against a
// self-signed cert → the channel never comes up (handshake refused), and
// the connector keeps retrying rather than falling back to cleartext.
package remote_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/fed"
	"llm-relay/internal/remote"
	"llm-relay/internal/store"
	"llm-relay/internal/tlscert"
)

type tlsRelayEnv struct {
	srv   *httptest.Server
	reg   *remote.Registry
	b     *store.Backend
	token string
	pin   string
}

func newTLSRelay(t *testing.T) *tlsRelayEnv {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/remote.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	b, token, err := store.CreateRemoteBackend(d, "tls-box")
	if err != nil {
		t.Fatal(err)
	}
	reg := remote.NewRegistry(d, fastOpts(), "tls-relay", "test")
	t.Cleanup(func() { reg.CloseAll("teardown") })
	mux := http.NewServeMux()
	mux.Handle("GET /remote/v1/connect", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		reg.ServeConn(w, r, b, auth.HashKey(token))
	}))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return &tlsRelayEnv{srv: srv, reg: reg, b: b, token: token,
		pin: tlscert.Fingerprint(srv.Certificate().Raw)}
}

// dial starts a connector against the relay with the given pin and reports
// whether the channel came online within the window. The engine URL is a
// dead port: a channel that reaches "online" proves the TLS handshake and
// hello succeeded, independent of any engine traffic.
func (e *tlsRelayEnv) dial(t *testing.T, pin string) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go remote.RunClient(ctx, remote.ClientConfig{
		ServerURL: e.srv.URL, Token: e.token, Pin: pin,
		EngineURL: "http://127.0.0.1:1", Version: "v1",
		Options: fed.Options{BackoffMin: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond},
	}, nil)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if e.reg.Online(e.b.ID) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestRemotePinHandshake(t *testing.T) {
	e := newTLSRelay(t)
	if !e.dial(t, e.pin) {
		t.Fatal("correct pin: channel never came online")
	}
}

func TestRemoteWrongPinRefused(t *testing.T) {
	e := newTLSRelay(t)
	if e.dial(t, "sha256:"+strings.Repeat("ab", 32)) {
		t.Fatal("wrong pin must never bring the channel up")
	}
}

func TestRemoteNoPinSelfSignedRefused(t *testing.T) {
	e := newTLSRelay(t)
	if e.dial(t, "") {
		t.Fatal("no pin against a self-signed cert must be refused")
	}
}
