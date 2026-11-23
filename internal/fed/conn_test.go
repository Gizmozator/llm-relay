// The request-id multiplexing registry (task 3's seam, defined in task 2):
// OpenRequest claims a slot, inbound frames with a matching id route to
// the opener's channel, done() releases the per-peer semaphore.
package fed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/fed"
)

func TestOpenRequestRouting(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		accepted <- c
		<-release // coder/websocket closes the conn when the handler returns
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
	go conn.Serve(context.Background())

	// Register before the frames arrive: routing is by registration, not
	// by luck of arrival order.
	ch, done, err := conn.OpenRequest("r1")
	if err != nil {
		t.Fatal(err)
	}
	defer done()

	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	for _, msg := range []string{
		`{"t":"res","id":"r1","status":200}`,
		`{"t":"res","id":"nobody","status":500}`, // unregistered: ignored
	} {
		if err := client.Write(wctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case f := <-ch:
		res, ok := f.(*fed.Res)
		if !ok || res.Status != 200 || res.ID != "r1" {
			t.Errorf("routed frame = %v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("res frame never routed to the opener")
	}

	// The in-flight cap: MaxInflight slots total, then rejection; done()
	// frees one.
	dones := []func(){done}
	for i := 0; i < fed.MaxInflight-1; i++ {
		_, d, err := conn.OpenRequest(string(rune('a' + i)))
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		dones = append(dones, d)
	}
	if _, _, err := conn.OpenRequest("overflow"); err != fed.ErrTooManyInflight {
		t.Errorf("overflow = %v, want ErrTooManyInflight", err)
	}
	dones[0]()
	if _, _, err := conn.OpenRequest("after-free"); err != nil {
		t.Errorf("slot after free: %v", err)
	}
}

// livePair builds a server-side websocket.Conn wrapped in nothing yet,
// plus its dialing client: enough to exercise Conn lifecycle.
func livePair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		accepted <- c
		<-release // coder/websocket closes the conn when the handler returns
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dcancel()
	client, _, err := websocket.Dial(dctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.CloseNow() })
	return <-accepted, client
}

// Item 3: Done fires exactly once when the Conn is fully dead — on
// explicit Close, on peer death, and on Close without a running pump.
// Task-3 consumers rely on it: request channels get no sentinel frame.
func TestConnDone(t *testing.T) {
	t.Run("Close", func(t *testing.T) {
		ws, _ := livePair(t)
		conn := fed.NewConn(ws, quietOpts())
		go conn.Serve(context.Background())
		select {
		case <-conn.Done():
			t.Fatal("Done fired while the channel was live")
		case <-time.After(100 * time.Millisecond):
		}
		conn.Close(websocket.StatusNormalClosure, "bye")
		select {
		case <-conn.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done did not fire after Close")
		}
	})
	t.Run("PeerDeath", func(t *testing.T) {
		ws, client := livePair(t)
		conn := fed.NewConn(ws, quietOpts())
		go conn.Serve(context.Background())
		client.CloseNow()
		select {
		case <-conn.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done did not fire after the peer died")
		}
	})
	t.Run("CloseWithoutServe", func(t *testing.T) {
		ws, _ := livePair(t)
		conn := fed.NewConn(ws, quietOpts())
		conn.Close(websocket.StatusNormalClosure, "never served")
		select {
		case <-conn.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done did not fire when Close ran without Serve")
		}
	})
}
