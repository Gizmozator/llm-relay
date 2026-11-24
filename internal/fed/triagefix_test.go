// Regression tests for the final whole-repo scan fix round (fed side):
// Reload keeps the running dialer when the settings read fails (5) and a
// panicking frame handler must not kill the read pump (11).
package fed_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/fed"
	"llm-relay/internal/store"
)

// Item 5: a transient settings-read failure is NOT a role change — Reload
// must report it and leave the live spoke channel (and dialer) alone.
func TestReloadKeepsDialerOnSettingsReadError(t *testing.T) {
	// A hub that handshakes and then HOLDS the socket (the shared stubHub
	// drops idle connections after 50 ms, which would fake a teardown).
	handshakes := make(chan struct{}, 4)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		ack, _ := fed.Encode(&fed.HelloAck{T: fed.THelloAck, Proto: fed.Protocol,
			Name: "stub-hub", Version: "0.0.1"})
		if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
			return
		}
		handshakes <- struct{}{}
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	d := migratedDB(t)
	mgr := fed.NewManager(d, &fed.Options{
		Log:         log.New(io.Discard, "", 0),
		BackoffMin:  50 * time.Millisecond,
		BackoffMax:  200 * time.Millisecond,
		StableAfter: time.Hour,
	})
	defer mgr.Close()
	if err := store.PutFederationSettings(d, store.FederationSettings{
		Role: store.FedRoleSpoke, HubURL: srv.URL, HubToken: "fed-tok", HubPin: pinOf(srv),
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(); err != nil {
		t.Fatalf("healthy reload: %v", err)
	}
	select {
	case <-handshakes:
	case <-time.After(5 * time.Second):
		t.Fatal("spoke never dialed the stub hub")
	}
	deadline := time.Now().Add(5 * time.Second)
	for connected, _, _ := mgr.SpokeStatus(); !connected; connected, _, _ = mgr.SpokeStatus() {
		if time.Now().After(deadline) {
			t.Fatal("spoke status never reported connected")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Break the settings read (closed DB) — the reconcile must fail loudly
	// and change nothing.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(); err == nil {
		t.Fatal("Reload must report a failed settings read")
	}
	time.Sleep(400 * time.Millisecond)
	if connected, _, _ := mgr.SpokeStatus(); !connected {
		t.Fatal("failed Reload tore down the live spoke channel")
	}
	select {
	case extra := <-handshakes:
		_ = extra
		t.Error("failed Reload triggered a redial")
	default:
	}
}

// Item 11: a frame handler that panics must not take the read pump (and
// with it the process) down — the frame is dropped, the channel lives.
func TestDispatchPanicDoesNotKillPump(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		accepted <- c
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dcancel()
	client, _, err := websocket.Dial(dctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()

	ws := <-accepted
	conn := fed.NewConn(ws, quietOpts())
	defer conn.Close(websocket.StatusNormalClosure, "done")

	panicked := make(chan struct{})
	survived := make(chan struct{})
	conn.On(fed.TStatus, func(*fed.Conn, fed.Frame) {
		close(panicked)
		panic("test panic")
	})
	conn.On(fed.TCancel, func(*fed.Conn, fed.Frame) { close(survived) })
	go conn.Serve(context.Background())

	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	for _, msg := range []string{`{"t":"status","peer_id":1,"online":true}`, `{"t":"cancel","id":"x"}`} {
		if err := client.Write(wctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	recv(t, panicked, "panicking handler invocation")
	recv(t, survived, "frame after the panic — pump must still run")
}
