// Internal connector-side tests (package remote to reach engineServer).
package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"llm-relay/internal/fed"
)

// The gate must be exact: legitimate paths (trailing slash, embedded dots
// in names) pass; dot segments die in BOTH the raw and the unescaped form
// — an honest relay can only send decoded paths (URL.Path), but a hostile
// one can pre-encode %2e%2e, which the engine's front would decode.
func TestServedPathOK(t *testing.T) {
	allowed := []string{"/v1/chat/completions", "/v1/models/", "/v1/models", "/health", "/v1/files/a..b"}
	refused := []string{
		"/v1/../admin", "/v1/./x", "/v1/%2e%2e/admin", "/v1/%2E%2E/admin",
		"/v1/%2e/admin", "v1/models", "/admin", "/v1", "//v1/x",
	}
	for _, p := range allowed {
		if !servedPathOK("POST", p) && p != "/health" {
			t.Errorf("path %q must be allowed", p)
		}
	}
	if !servedPathOK("GET", "/health") {
		t.Error("/health GET must be allowed")
	}
	if servedPathOK("POST", "/health") {
		t.Error("/health POST must be refused")
	}
	for _, p := range refused {
		if servedPathOK("GET", p) {
			t.Errorf("path %q must be refused", p)
		}
	}
}

// A hostile relay that ignores its own OpenRequest cap must not be able to
// pile unlimited engine calls onto the connector: onReq sheds beyond
// fed.MaxInflight, mirroring the relay-side backpressure.
func TestConnectorInflightCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connCh := make(chan *fed.Conn, 1)
	opts := fed.Options{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := fed.NewConn(ws, opts.WithDefaults())
		connCh <- c
		go c.Serve(ctx)
	}))
	defer hs.Close()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http")+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	relay := fed.NewConn(ws, opts.WithDefaults())
	go relay.Serve(ctx)

	conn := <-connCh
	srv := &engineServer{
		cfg:    ClientConfig{EngineURL: "http://127.0.0.1:1"}, // never dialed: slots pre-filled
		conn:   conn,
		logf:   func(string, ...any) {},
		client: http.DefaultClient,
		served: map[string]context.CancelFunc{},
	}
	conn.On(fed.TReq, srv.onReq)

	ends := make(chan *fed.ResEnd, 4)
	relay.On(fed.TResEnd, func(_ *fed.Conn, f fed.Frame) {
		if re, ok := f.(*fed.ResEnd); ok {
			select {
			case ends <- re:
			default:
			}
		}
	})

	srv.mu.Lock()
	for i := 0; i < fed.MaxInflight; i++ {
		srv.served[fmt.Sprintf("fill-%d", i)] = func() {}
	}
	srv.mu.Unlock()

	if err := relay.Send(&fed.Req{T: fed.TReq, ID: "over", Method: "GET", Path: "/v1/models"}); err != nil {
		t.Fatal(err)
	}
	select {
	case re := <-ends:
		if re.ID != "over" || re.Error == "" {
			t.Fatalf("ResEnd = %+v, want id=over with a capacity error", re)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("over-capacity request was not shed")
	}
}
