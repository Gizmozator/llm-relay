// Conn: one federation WebSocket channel. All writes are serialized
// through a single writer goroutine (websocket.Conn allows only one
// concurrent writer/reader); the read pump decodes frames and dispatches
// them to per-type handlers or to the request-id registry. Both sides run
// the same liveness pinger. Hub and spoke wrap the accepted/dialed socket
// in a Conn after their hello handshake.
package fed

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ReadLimit caps one inbound frame (4 MiB): generous for a base64 body
// chunk, small enough that a hostile peer cannot OOM the relay with one
// frame.
const ReadLimit = 4 << 20

// MaxInflight is the per-peer cap on concurrent tunneled requests (task 3
// multiplexes req/res streams over the one channel).
const MaxInflight = 16

// ErrClosed is returned by Send once the channel is gone.
var ErrClosed = errors.New("fed channel closed")

// ErrTooManyInflight rejects an OpenRequest beyond MaxInflight.
var ErrTooManyInflight = errors.New("fed too many in-flight requests")

// Options carries the timing knobs. Zero fields get production defaults;
// tests shorten them freely (every timer in this package reads Options).
type Options struct {
	PingInterval  time.Duration // send ping every (default 20s)
	PongTimeout   time.Duration // close if no inbound frame within this after a ping (10s)
	WriteTimeout  time.Duration // per-frame write deadline (10s)
	HelloTimeout  time.Duration // hub: hello must arrive within this (5s)
	TouchThrottle time.Duration // TouchPeer at most once per (30s)
	BackoffMin    time.Duration // spoke reconnect backoff floor (1s)
	BackoffMax    time.Duration // spoke reconnect backoff ceiling (60s)
	StableAfter   time.Duration // connection alive this long resets backoff (60s)
	DialTimeout   time.Duration // spoke: dial+upgrade budget per attempt (10s)
	// CatalogDebounce coalesces a burst of local catalog edits (model
	// toggles, group edits) into one catalog re-push (default 500ms).
	CatalogDebounce time.Duration
	// InstanceName / Version identify this instance in hello/hello_ack.
	InstanceName string
	Version      string
	// Touch, when set, is the hub-side liveness bookkeeping (TouchPeer).
	// Called from the read pump at most once per TouchThrottle.
	Touch func()
	// Log receives protocol-level notices; nil = log.Default().
	Log *log.Logger
}

func (o *Options) withDefaults() Options {
	d := *o
	if d.PingInterval == 0 {
		d.PingInterval = 20 * time.Second
	}
	if d.PongTimeout == 0 {
		d.PongTimeout = 10 * time.Second
	}
	if d.WriteTimeout == 0 {
		d.WriteTimeout = 10 * time.Second
	}
	if d.HelloTimeout == 0 {
		d.HelloTimeout = 5 * time.Second
	}
	if d.TouchThrottle == 0 {
		d.TouchThrottle = 30 * time.Second
	}
	if d.BackoffMin == 0 {
		d.BackoffMin = time.Second
	}
	if d.BackoffMax == 0 {
		d.BackoffMax = 60 * time.Second
	}
	if d.StableAfter == 0 {
		d.StableAfter = 60 * time.Second
	}
	if d.DialTimeout == 0 {
		d.DialTimeout = 10 * time.Second
	}
	if d.CatalogDebounce == 0 {
		d.CatalogDebounce = 500 * time.Millisecond
	}
	if d.Log == nil {
		d.Log = log.Default()
	}
	return d
}

// Handler processes one inbound frame on the read pump's goroutine; keep
// it short (task 3's handlers hand work to other goroutines).
type Handler func(c *Conn, f Frame)

// Conn wraps a websocket connection with the writer goroutine, read pump,
// pinger and the request-id multiplexing registry.
type Conn struct {
	ws   *websocket.Conn
	opts Options

	out chan rawFrame

	mu       sync.Mutex
	handlers map[string]Handler
	inflight map[string]chan Frame
	sem      chan struct{}

	lastInbound atomic.Int64 // unix nanos of the last inbound frame
	lastTouch   atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}

	serving  atomic.Bool // Serve has started (its defer marks the Conn dead)
	deadOnce sync.Once
	dead     chan struct{} // closed exactly once when the Conn is fully dead
}

// NewConn takes ownership of ws (read limit applied) and starts the
// writer and pinger goroutines. They exit when the Conn closes.
func NewConn(ws *websocket.Conn, opts Options) *Conn {
	o := opts.withDefaults()
	ws.SetReadLimit(ReadLimit)
	c := &Conn{
		ws:       ws,
		opts:     o,
		out:      make(chan rawFrame, 64),
		handlers: map[string]Handler{},
		inflight: map[string]chan Frame{},
		sem:      make(chan struct{}, MaxInflight),
		closed:   make(chan struct{}),
		dead:     make(chan struct{}),
	}
	c.lastInbound.Store(time.Now().UnixNano())
	go c.writeLoop()
	go c.pinger()
	return c
}

// On registers the handler for one frame type (replaces any previous).
// ping/pong are handled internally and cannot be overridden.
func (c *Conn) On(typ string, h Handler) {
	c.mu.Lock()
	c.handlers[typ] = h
	c.mu.Unlock()
}

// Send queues a frame for the writer goroutine. It blocks while the write
// queue is full (backpressure) and fails once the Conn is closed.
func (c *Conn) Send(f Frame) error {
	data, err := Encode(f)
	if err != nil {
		return err
	}
	select {
	case c.out <- rawFrame{data: data}:
		return nil
	case <-c.closed:
		return ErrClosed
	}
}

// rawFrame pre-renders into the queue so the writer never marshals.
type rawFrame struct{ data []byte }

func (c *Conn) writeLoop() {
	for {
		select {
		case rf := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), c.opts.WriteTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, rf.data)
			cancel()
			if err != nil {
				c.Close(websocket.StatusInternalError, "write failed")
				return
			}
		case <-c.closed:
			return
		}
	}
}

// pinger runs the liveness loop: every PingInterval, an idle channel is
// closed (idle = no inbound frame for PingInterval+PongTimeout); otherwise
// a ping goes out and the channel must show *some* inbound frame within
// PongTimeout (any frame counts as liveness evidence, not just pong).
func (c *Conn) pinger() {
	tick := time.NewTicker(c.opts.PingInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-c.closed:
			return
		}
		now := time.Now()
		if now.UnixNano()-c.lastInbound.Load() > int64(c.opts.PingInterval+c.opts.PongTimeout) {
			c.Close(websocket.StatusPolicyViolation, "liveness timeout")
			return
		}
		sent := now.UnixNano()
		if err := c.Send(&Ping{T: TPing}); err != nil {
			return
		}
		time.AfterFunc(c.opts.PongTimeout, func() {
			if c.lastInbound.Load() <= sent {
				c.Close(websocket.StatusPolicyViolation, "pong timeout")
			}
		})
	}
}

// Serve runs the read pump until the connection dies or is closed. It
// blocks, so callers that want to keep serving run it in a goroutine.
func (c *Conn) Serve(ctx context.Context) error {
	c.serving.Store(true)
	defer c.markDead() // Done fires exactly when the pump is gone
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	// The pump reads on a ctx that dies shortly after Close: cancelling a
	// pending websocket.Read is the only exported way to free the
	// library's readMu, and ws.Close's close handshake needs it — without
	// this, Close wedges behind the pump for the full 5s handshake
	// timeout. The delay lets Close's close frame reach the wire first
	// (an instant cancel would kill the socket before it flushes).
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.closed:
			select {
			case <-time.After(closeForceDelay):
				cancel()
			case <-readCtx.Done():
			}
		case <-readCtx.Done():
		}
	}()
	for {
		if ctx.Err() != nil {
			c.Close(websocket.StatusNormalClosure, "bye")
			return ctx.Err()
		}
		_, data, err := c.ws.Read(readCtx)
		if err != nil {
			c.Close(websocket.StatusNormalClosure, "read ended")
			// A real close frame (code > 0) is a clean peer goodbye;
			// anything else (EOF, ctx cancel, transport rot) propagates
			// honestly — CloseStatus returns -1 (NoStatusReceived), never
			// 0, for non-close errors.
			if websocket.CloseStatus(err) > 0 {
				return ErrClosed
			}
			return err
		}
		c.lastInbound.Store(time.Now().UnixNano())
		c.maybeTouch()
		typ, id, err := Peek(data)
		if err != nil {
			c.Close(websocket.StatusUnsupportedData, "bad frame")
			return err
		}
		// Request-id routed frames (task 3's res/res_body/res_end) go to
		// the opener's channel; the registry decides before handlers do.
		if id != "" && c.routeInflight(id, typ, data) {
			continue
		}
		f, err := decodeWith(typ, data)
		if err != nil {
			// Unknown types are forward-compat noise from a newer peer:
			// ignore, never kill the channel over them.
			if typ != "" {
				c.opts.Log.Printf("fed: dropping frame: %v", err)
			}
			continue
		}
		switch f.(type) {
		case *Ping:
			_ = c.Send(&Pong{T: TPong})
			continue
		case *Pong:
			continue // liveness already recorded
		}
		c.mu.Lock()
		h := c.handlers[typ]
		c.mu.Unlock()
		if h != nil {
			h(c, f)
		}
		// Frames this channel does not serve (e.g. a Status arriving at a
		// hub, which only ever sends them — see attachChannel) are ignored:
		// a channel stays alive even if one feature is dark.
	}
}

// maybeTouch runs the throttled liveness bookkeeping (hub side).
func (c *Conn) maybeTouch() {
	if c.opts.Touch == nil {
		return
	}
	now := time.Now()
	last := c.lastTouch.Load()
	if now.UnixNano()-last < int64(c.opts.TouchThrottle) {
		return
	}
	if c.lastTouch.CompareAndSwap(last, now.UnixNano()) {
		c.opts.Touch()
	}
}

// ---------------------------------------------------------------- multiplexing

// OpenRequest registers a request id and returns the channel its routed
// frames arrive on, plus a done func that must be called when the request
// finishes (releases the per-peer in-flight slot). Task 3's tunnel is the
// consumer; the registry and the cap exist now so the wire format and the
// backpressure story are settled. NOTE: the frame channel receives no
// sentinel when the Conn dies — consumers must select on c.Done().
func (c *Conn) OpenRequest(id string) (<-chan Frame, func(), error) {
	select {
	case c.sem <- struct{}{}:
	case <-c.closed:
		return nil, nil, ErrClosed
	default:
		// Cap is a hard limit: callers must not queue behind it (task 3
		// surfaces this as a routing backpressure signal).
		return nil, nil, ErrTooManyInflight
	}
	ch := make(chan Frame, 32)
	c.mu.Lock()
	if _, dup := c.inflight[id]; dup {
		c.mu.Unlock()
		<-c.sem
		return nil, nil, errors.New("fed duplicate request id")
	}
	c.inflight[id] = ch
	c.mu.Unlock()
	var once sync.Once
	done := func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.inflight, id)
			c.mu.Unlock()
			<-c.sem
		})
	}
	return ch, done, nil
}

// routeInflight decodes and delivers a frame to the registry entry for id.
// Returns false when no request owns the id. typ is the already-peeked
// frame type (single-parse path, see decodeWith).
func (c *Conn) routeInflight(id, typ string, data []byte) bool {
	c.mu.Lock()
	ch, ok := c.inflight[id]
	c.mu.Unlock()
	if !ok {
		return false
	}
	f, err := decodeWith(typ, data)
	if err != nil {
		c.opts.Log.Printf("fed: dropping routed frame: %v", err)
		return true
	}
	select {
	case ch <- f:
	case <-c.closed:
	}
	return true
}

// ---------------------------------------------------------------- close

// closeForceDelay bounds Conn.Close: after Close, the read pump's socket
// is torn down at most this long later (see Serve), so Done — and anything
// waiting on it, like dialer.stop — lands within ~1s even against a dead
// peer that never answers the close handshake.
const closeForceDelay = time.Second

// Close shuts the channel down once: pumps stop, the socket closes with
// code+reason. It returns immediately — close-frame delivery is
// best-effort and happens in the background (the frame itself goes out
// within milliseconds; the handshake reply wait is bounded by the pump's
// forced teardown). Safe from any goroutine, including the pumps.
func (c *Conn) Close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		close(c.closed)
		// Detached: ws.Close writes our close frame right away, then
		// waits for the peer's reply — a wait that can outlive the
		// socket (the pump force-cancel frees the library's readMu at
		// closeForceDelay). Callers must not pay for it.
		go func() { _ = c.ws.Close(code, reason) }()
		if !c.serving.Load() {
			// Nobody runs the read pump to mark death (Serve was never
			// called, or races just after this check — its defer then
			// fires the same once).
			c.markDead()
		}
	})
}

// Done returns a channel closed exactly once when the Conn is fully dead
// (after the read pump exits). In-flight request channels get NO sentinel
// frame on death — task 3's tunnel consumers must always select on Done()
// alongside their frame channel.
func (c *Conn) Done() <-chan struct{} { return c.dead }

func (c *Conn) markDead() {
	c.deadOnce.Do(func() { close(c.dead) })
}

// Closed reports whether the channel has been closed.
func (c *Conn) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}
