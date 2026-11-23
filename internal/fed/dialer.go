// Spoke side (plan 2026-11-22-relay-federation, task 2): dial the hub's
// /federation/v1/connect with the stored server token, run the hello
// handshake, then serve the channel until it dies — and reconnect with
// jittered exponential backoff (BackoffMin→BackoffMax, reset once a
// connection has held for StableAfter).
package fed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/store"
)

type dialer struct {
	db      *sql.DB
	hubURL  string
	token   string
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	dialSem chan struct{} // cap 1: one attempt at a time; also dial counter seam
}

func newDialer(db *sql.DB, f store.FederationSettings, opts Options) *dialer {
	ctx, cancel := context.WithCancel(context.Background())
	return &dialer{
		db: db, hubURL: f.HubURL, token: f.HubToken, opts: opts,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		dialSem: make(chan struct{}, 1),
	}
}

func (d *dialer) start() { go d.run() }

// stop cancels the loop and waits for it to exit: after stop returns, no
// dial can still fire (Reload relies on this for "no further dials").
func (d *dialer) stop() {
	d.cancel()
	<-d.done
}

func (d *dialer) run() {
	defer close(d.done)
	backoff := d.opts.BackoffMin
	for {
		started := time.Now()
		err := d.dialOnce()
		if d.ctx.Err() != nil {
			return
		}
		if time.Since(started) >= d.opts.StableAfter {
			backoff = d.opts.BackoffMin // a held connection resets the ladder
		}
		sleep := jitter(backoff)
		d.opts.Log.Printf("fed: spoke channel down (%v); redial in %s", err, sleep.Round(time.Millisecond))
		select {
		case <-time.After(sleep):
		case <-d.ctx.Done():
			return
		}
		if backoff *= 2; backoff > d.opts.BackoffMax {
			backoff = d.opts.BackoffMax
		}
	}
}

// jitter spreads a retry over [d/2, d] so a hub restart does not meet a
// synchronized thundering herd of spokes.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// dialOnce performs one full connect: upgrade, hello/hello_ack, then
// Serve until the channel dies.
func (d *dialer) dialOnce() error {
	d.dialSem <- struct{}{}
	defer func() { <-d.dialSem }()

	url := strings.TrimSuffix(d.hubURL, "/") + "/federation/v1/connect"
	dctx, dcancel := context.WithTimeout(d.ctx, d.opts.DialTimeout)
	ws, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + d.token}},
	})
	dcancel()
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	if err := d.handshake(ws); err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "handshake failed")
		return err
	}
	conn := NewConn(ws, d.opts)
	return conn.Serve(d.ctx)
}

// handshake sends hello and waits for a matching hello_ack (raw reads —
// the Conn pumps start after this returns).
func (d *dialer) handshake(ws *websocket.Conn) error {
	hello, err := Encode(&Hello{T: THello, Proto: Protocol,
		Name: d.opts.InstanceName, Version: d.opts.Version})
	if err != nil {
		return err
	}
	wctx, wcancel := context.WithTimeout(d.ctx, d.opts.WriteTimeout)
	defer wcancel()
	if err := ws.Write(wctx, websocket.MessageText, hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	rctx, rcancel := context.WithTimeout(d.ctx, d.opts.HelloTimeout)
	defer rcancel()
	_, data, err := ws.Read(rctx)
	if err != nil {
		return fmt.Errorf("hello_ack: %w", err)
	}
	f, err := Decode(data)
	if err != nil {
		return err
	}
	ack, ok := f.(*HelloAck)
	if !ok {
		return errors.New("expected hello_ack")
	}
	if ack.Proto != Protocol {
		return fmt.Errorf("hub proto mismatch: %d", ack.Proto)
	}
	return nil
}
