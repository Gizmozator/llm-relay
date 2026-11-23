// Spoke dialer e2e (plan 2026-11-22-relay-federation task 2): the
// Manager's spoke side must dial the hub with the stored token, run the
// hello handshake, reconnect after a drop (backoff), and stop for good
// when the role changes away from spoke (Reload tears the channel down
// and no further dials fire).
package fed_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/fed"
	"llm-relay/internal/store"
)

// stubHub is a minimal federation hub: it records every hello it sees and
// every Authorization header, answers the handshake, and lets the test
// kick connections (drop) or observe the spoke closing them.
type stubHub struct {
	hellos       chan *fed.Hello
	auth         chan string
	kick         chan chan struct{} // push a kick channel to drop that conn
	remoteClosed chan chan struct{} // popped after the spoke closed its side
}

func newStubHub(t *testing.T) (*stubHub, *httptest.Server) {
	t.Helper()
	s := &stubHub{
		hellos:       make(chan *fed.Hello, 16),
		auth:         make(chan string, 16),
		kick:         make(chan chan struct{}, 16),
		remoteClosed: make(chan chan struct{}, 16),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/federation/v1/connect" {
			http.NotFound(w, r)
			return
		}
		s.auth <- r.Header.Get("Authorization")
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		f, err := fed.Decode(data)
		if err != nil {
			return
		}
		if h, ok := f.(*fed.Hello); ok {
			s.hellos <- h
		}
		ack, _ := fed.Encode(&fed.HelloAck{T: fed.THelloAck, Proto: fed.Protocol,
			Name: "stub-hub", Version: "0.0.1"})
		if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
			return
		}
		// Hold the connection until the test kicks it or the spoke closes.
		kick := make(chan struct{})
		s.kick <- kick
		closed := make(chan struct{})
		s.remoteClosed <- closed
		for {
			select {
			case <-kick:
				return // handler exit closes the socket
			default:
			}
			rctx, rcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, _, err := c.Read(rctx)
			rcancel()
			if err != nil {
				close(closed)
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		var zero T
		return zero
	}
}

func TestSpokeDialerLifecycle(t *testing.T) {
	stub, url := newStubHub(t)
	d := migratedDB(t)
	mgr := fed.NewManager(d, &fed.Options{
		Log:         log.New(io.Discard, "", 0),
		BackoffMin:  50 * time.Millisecond,
		BackoffMax:  200 * time.Millisecond,
		StableAfter: time.Hour, // keep the backoff ladder low for the test
	})
	defer mgr.Close()

	if err := store.PutFederationSettings(d, store.FederationSettings{
		Role: store.FedRoleSpoke, HubURL: url.URL, HubToken: "fed-tok",
	}); err != nil {
		t.Fatal(err)
	}
	mgr.Reload()

	// First dial: correct endpoint auth + hello seen + ack consumed.
	h1 := recv(t, stub.hellos, "first hello")
	if h1.Proto != fed.Protocol || h1.Name == "" || h1.Version == "" {
		t.Errorf("hello = %+v", h1)
	}
	if got := recv(t, stub.auth, "auth header"); got != "Bearer fed-tok" {
		t.Errorf("hub saw Authorization %q", got)
	}

	// Hub drops the connection → the spoke redials (second hello).
	kick := recv(t, stub.kick, "kick channel")
	recv(t, stub.remoteClosed, "first conn close watcher")
	close(kick)
	h2 := recv(t, stub.hellos, "reconnect hello")
	if h2.Proto != fed.Protocol {
		t.Errorf("reconnect hello = %+v", h2)
	}
	closed2 := recv(t, stub.remoteClosed, "second conn close watcher")

	// Role change to standalone → live channel closed, no further dials.
	if err := store.PutFederationSettings(d, store.FederationSettings{Role: store.FedRoleStandalone}); err != nil {
		t.Fatal(err)
	}
	mgr.Reload()
	<-closed2 // the stub saw the spoke close its socket
	select {
	case extra := <-stub.hellos:
		t.Errorf("dial after leaving spoke role: %+v", extra)
	case <-time.After(500 * time.Millisecond):
	}
}

// Item 4: Reload reads the settings under the manager lock, so
// interleaved Reloads cannot leave a dialer running for a stale role.
// Best-effort stress (the race detector watches the lock) plus a hard
// convergence assertion: after settling on standalone, no dial ever
// reaches the stub hub again.
func TestConcurrentReloadConverges(t *testing.T) {
	stub, url := newStubHub(t)
	d := migratedDB(t)
	mgr := fed.NewManager(d, &fed.Options{
		Log:        log.New(io.Discard, "", 0),
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	})
	defer mgr.Close()

	// Keep the stub's recording channels from backing up while the
	// flippers churn.
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-stop:
				return
			case <-stub.hellos:
			case <-stub.auth:
			case <-stub.kick:
			case <-stub.remoteClosed:
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				role := store.FedRoleStandalone
				if j%2 == 0 {
					role = store.FedRoleSpoke
				}
				if err := store.PutFederationSettings(d, store.FederationSettings{
					Role: role, HubURL: url.URL, HubToken: "fed-tok"}); err != nil {
					return // busy DB under stress: this flipper is done
				}
				mgr.Reload()
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	<-drained

	// Settle on standalone: the final Reload must leave nothing dialing.
	if err := store.PutFederationSettings(d, store.FederationSettings{
		Role: store.FedRoleStandalone}); err != nil {
		t.Fatal(err)
	}
	mgr.Reload()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-stub.hellos: // straggler from before the final Reload: fine
		case <-time.After(300 * time.Millisecond):
			return // quiet for 300 ms: converged, no stale dialer
		}
	}
	t.Error("dials still arriving after settling on standalone")
}
