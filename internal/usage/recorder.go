// Package usage contains the buffered usage recorder: request handlers push
// events onto a channel and a single goroutine writes them to SQLite, so no
// proxied request ever blocks on the database.
package usage

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"llm-relay/internal/store"
)

type Recorder struct {
	db      *sql.DB
	events  chan *store.UsageEvent
	done    chan struct{}
	stop    chan struct{} // Close's abandon-queue signal (see Close)
	mu      sync.Mutex
	closed  bool
	started bool // Start ran: a writer goroutine exists and will close done
	drops   int64
}

func NewRecorder(db *sql.DB) *Recorder {
	return &Recorder{
		db:     db,
		events: make(chan *store.UsageEvent, 1024),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
	}
}

// Start launches the writer goroutine.
func (r *Recorder) Start() {
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	go r.run()
}

// Submit queues an event. It never blocks: if the buffer is full the event is
// dropped with a log line rather than stalling a live request. After Close it
// drops instead of panicking — a stream finishing past the shutdown deadline
// may still call it.
func (r *Recorder) Submit(e *store.UsageEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		log.Printf("usage recorder closed, dropping event (user=%d key=%d)", e.UserID, e.KeyID)
		return
	}
	select {
	case r.events <- e:
	default:
		// A saturated buffer is a sustained condition; logging every drop
		// would be an unbounded log flood exactly when the box is loaded.
		r.drops++
		if r.drops == 1 || r.drops%100 == 0 {
			log.Printf("usage recorder full, dropped %d events (latest user=%d key=%d)", r.drops, e.UserID, e.KeyID)
		}
	}
}

// Close drains the queue, waiting up to timeout for pending writes. It
// always returns with the writer goroutine finished: if the drain overruns
// the timeout the queue is abandoned (counted in the log) and Close still
// waits for run() to exit — returning while an insert is in flight would
// let the caller close the DB underneath it. The extra wait is bounded:
// one insert is at most busy_timeout long.
func (r *Recorder) Close(timeout time.Duration) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	started := r.started
	close(r.events)
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-time.After(timeout):
		if !started {
			return // no writer goroutine was ever launched
		}
		log.Printf("usage recorder: drain exceeded %v, abandoning queued events", timeout)
		close(r.stop)
		<-r.done
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	for {
		select {
		case e, ok := <-r.events:
			if !ok {
				return
			}
			if err := store.InsertUsage(r.db, e); err != nil {
				log.Printf("usage insert failed: %v", err)
				continue
			}
			if e.KeyID != 0 {
				if err := store.TouchKey(r.db, e.KeyID, e.TS); err != nil {
					log.Printf("key touch failed: %v", err)
				}
			}
		case <-r.stop:
			// Close gave up on the drain: count what is still queued and
			// exit promptly so the process can close the DB safely.
			n := 0
			for {
				select {
				case _, ok := <-r.events:
					if !ok {
						log.Printf("usage recorder: shutdown abandoned queue, %d events dropped", n)
						return
					}
					n++
				default:
					log.Printf("usage recorder: shutdown abandoned queue, %d events dropped", n)
					return
				}
			}
		}
	}
}
