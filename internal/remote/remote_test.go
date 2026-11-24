// End-to-end tests for the remote-backend channel (plan
// 2026-11-24-remote-backend-v1): registry + connector + a fake engine in one
// process. Covers connect, tunneled probe-shaped requests, streaming, mid-
// stream cancel, path gating, offline failover, reconnect, and
// replace-on-reconnect.
package remote_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/fed"
	"llm-relay/internal/remote"
	"llm-relay/internal/store"
)

// fastOpts shrinks the channel timers so the test does not sleep on
// production backoff ladders.
func fastOpts() *remote.Options {
	return &remote.Options{Options: fed.Options{
		PingInterval: 200 * time.Millisecond,
		PongTimeout:  2 * time.Second,
		BackoffMin:   50 * time.Millisecond,
		BackoffMax:   200 * time.Millisecond,
		StableAfter:  time.Second,
	}}
}

// fakeEngine is a minimal OpenAI-compatible upstream: /health, /v1/models,
// a non-streaming and a streaming chat completion, and /v1/slow which
// streams forever until the caller hangs up (records whether the request
// context was cancelled, proving the Cancel frame reached the engine).
type fakeEngine struct {
	*httptest.Server
	cancelled atomic.Bool
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	e := &fakeEngine{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"engine-1.2.3"}`)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"alpha"},{"id":"beta"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if !in.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"hello"}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	mux.HandleFunc("POST /v1/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: first\n\n")
		fl.Flush()
		<-r.Context().Done()
		e.cancelled.Store(true)
	})
	e.Server = httptest.NewServer(mux)
	t.Cleanup(e.Close)
	return e
}

// remoteEnv wires a registry behind the connect route (token-checked like
// RemoteAuth) plus a running connector.
type remoteEnv struct {
	db     *sql.DB
	reg    *remote.Registry
	eng    *fakeEngine
	srvURL string
}

func newRemoteEnv(t *testing.T, firstOpts fed.Options) (*remoteEnv, *store.Backend, string) {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/remote.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	b, token, err := store.CreateRemoteBackend(d, "test-box")
	if err != nil {
		t.Fatal(err)
	}

	eng := newFakeEngine(t)
	reg := remote.NewRegistry(d, fastOpts(), "test-relay", "test")
	t.Cleanup(func() { reg.CloseAll("test teardown") })

	mux := http.NewServeMux()
	mux.Handle("GET /remote/v1/connect", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		reg.ServeConn(w, r, b, auth.HashKey(token))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go remote.RunClient(ctx, remote.ClientConfig{
		ServerURL: srv.URL, Token: token, EngineURL: eng.URL,
		Version: "v1", Options: firstOpts,
	}, nil)
	return &remoteEnv{db: d, reg: reg, eng: eng, srvURL: srv.URL}, b, token
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func remoteURL(b *store.Backend, path string) string {
	return b.BaseURL + path
}

func TestRemoteConnectAndServe(t *testing.T) {
	env, b, _ := newRemoteEnv(t, fastOpts().Options)
	waitFor(t, 5*time.Second, "channel online", func() bool { return env.reg.Online(b.ID) })

	// Tunneled GET /v1/models.
	req := httptest.NewRequest("GET", remoteURL(b, "/v1/models"), nil)
	resp, err := env.reg.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "alpha") {
		t.Fatalf("models: %d %s", resp.StatusCode, body)
	}

	// Non-streaming chat completion (POST with body).
	req = httptest.NewRequest("POST", remoteURL(b, "/v1/chat/completions"),
		strings.NewReader(`{"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = env.reg.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "hello") {
		t.Fatalf("chat: %d %s", resp.StatusCode, body)
	}

	// Streaming chat completion: SSE lines arrive through the tunnel.
	req = httptest.NewRequest("POST", remoteURL(b, "/v1/chat/completions"),
		strings.NewReader(`{"stream":true}`))
	resp, err = env.reg.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if s := strings.TrimPrefix(sc.Text(), "data: "); s != "" {
			chunks = append(chunks, s)
		}
	}
	resp.Body.Close()
	if err := sc.Err(); err != nil {
		t.Fatalf("stream read: %v", err)
	}
	if len(chunks) != 5 || chunks[4] != "[DONE]" {
		t.Fatalf("stream chunks = %v", chunks)
	}
}

func TestRemoteCancelMidStream(t *testing.T) {
	env, b, _ := newRemoteEnv(t, fastOpts().Options)
	waitFor(t, 5*time.Second, "channel online", func() bool { return env.reg.Online(b.ID) })

	req := httptest.NewRequest("POST", remoteURL(b, "/v1/slow"), nil)
	resp, err := env.reg.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || !strings.Contains(sc.Text(), "first") {
		t.Fatalf("first chunk: %v", sc.Err())
	}
	// Hang up mid-stream: the pump must send a Cancel frame and the engine
	// must see its request context cancelled.
	resp.Body.Close()
	waitFor(t, 5*time.Second, "engine cancellation via Cancel frame",
		func() bool { return env.eng.cancelled.Load() })
}

func TestRemotePathGate(t *testing.T) {
	env, b, _ := newRemoteEnv(t, fastOpts().Options)
	waitFor(t, 5*time.Second, "channel online", func() bool { return env.reg.Online(b.ID) })

	// The connector refuses anything outside /v1/* and /health: the relay
	// must not be able to use the tunnel to probe the box's network.
	req := httptest.NewRequest("GET", remoteURL(b, "/../../etc/passwd"), nil)
	if _, err := env.reg.RoundTrip(req); err == nil {
		t.Fatal("path outside /v1 must be refused")
	}
	// Dot-segment escape: req.Path arrives decoded (URL.Path), so
	// %2e-encoded dots are already ".." by the time the gate sees them —
	// and Go's http.Client forwards dot segments verbatim, so a
	// normalizing engine front would serve the escaped path.
	for _, p := range []string{"/v1/../etc/passwd", "/v1/./x", "/v1/%2e%2e/admin"} {
		req := httptest.NewRequest("GET", remoteURL(b, p), nil)
		if _, err := env.reg.RoundTrip(req); err == nil {
			t.Errorf("dot-segment path %q must be refused", p)
		}
	}
	// /health is allowed (the admin probe uses it).
	req = httptest.NewRequest("GET", remoteURL(b, "/health"), nil)
	resp, err := env.reg.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "engine-1.2.3") {
		t.Fatalf("health body = %s", body)
	}
}

func TestRemoteOfflineAndReconnect(t *testing.T) {
	env, b, _ := newRemoteEnv(t, fastOpts().Options)
	waitFor(t, 5*time.Second, "channel online", func() bool { return env.reg.Online(b.ID) })

	// Kick the channel: the connector is offline until it redials, and
	// RoundTrip fails dial-class (group failover signal) in the meantime.
	env.reg.Kick(b.ID, "test kick")
	waitFor(t, 5*time.Second, "channel offline after kick", func() bool { return !env.reg.Online(b.ID) })

	req := httptest.NewRequest("GET", remoteURL(b, "/v1/models"), nil)
	if _, err := env.reg.RoundTrip(req); !errors.Is(err, remote.ErrNotConnected) {
		t.Fatalf("offline RoundTrip = %v, want ErrNotConnected", err)
	}

	// The connector redials on its own and service is restored.
	waitFor(t, 5*time.Second, "reconnect", func() bool { return env.reg.Online(b.ID) })
	resp, err := env.reg.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// The first connector gets an hour of backoff: once the second connector
// takes over, the first must stay down for the test's duration, so the
// takeover is unambiguous.
func TestRemoteReplaceOnReconnect(t *testing.T) {
	env, b, token := newRemoteEnv(t, fed.Options{BackoffMin: time.Hour, BackoffMax: time.Hour})
	waitFor(t, 5*time.Second, "first connector online", func() bool { return env.reg.Online(b.ID) })

	// A second connector with the same token takes over immediately (a
	// restarted box replaces its old channel instead of queueing behind it).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go remote.RunClient(ctx, remote.ClientConfig{
		ServerURL: env.srvURL, Token: token, EngineURL: env.eng.URL, Version: "v2",
		Options: fed.Options{BackoffMin: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond},
	}, nil)
	waitFor(t, 5*time.Second, "second connector takes over", func() bool {
		on, _, ver, _ := env.reg.Connected(b.ID)
		return on && ver == "v2"
	})
}

// A revocation that lands between RemoteAuth and registration (or bypasses
// the kick endpoints entirely) must still drop the channel: the throttled
// Touch sweep revalidates the row against the hash that authenticated the
// connect. Backoff is an hour so the connector cannot redial over the
// rotated token and mask the assertion.
func TestRemoteRevocationSweep(t *testing.T) {
	env, b, _ := newRemoteEnv(t, fed.Options{
		PingInterval:  100 * time.Millisecond,
		TouchThrottle: 50 * time.Millisecond,
		BackoffMin:    time.Hour, BackoffMax: time.Hour,
	})
	waitFor(t, 5*time.Second, "channel online", func() bool { return env.reg.Online(b.ID) })

	// Rotate straight in the DB — no API call, so no Kick ever ran.
	if _, err := store.RotateRemoteToken(env.db, b.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "sweep closes the channel on rotation",
		func() bool { return !env.reg.Online(b.ID) })
}
