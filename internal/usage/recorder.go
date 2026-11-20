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
	db     *sql.DB
	events chan *store.UsageEvent
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

func NewRecorder(db *sql.DB) *Recorder {
	return &Recorder{
		db:     db,
		events: make(chan *store.UsageEvent, 1024),
		done:   make(chan struct{}),
	}
}

// Start launches the writer goroutine.
func (r *Recorder) Start() {
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
		log.Printf("usage recorder full, dropping event (user=%d key=%d)", e.UserID, e.KeyID)
	}
}

// Close drains the queue, waiting up to timeout for pending writes.
func (r *Recorder) Close(timeout time.Duration) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.events)
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-time.After(timeout):
		log.Printf("usage recorder: writer did not finish within %v", timeout)
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	for e := range r.events {
		if err := store.InsertUsage(r.db, e); err != nil {
			log.Printf("usage insert failed: %v", err)
			continue
		}
		if e.KeyID != 0 {
			if err := store.TouchKey(r.db, e.KeyID, e.TS); err != nil {
				log.Printf("key touch failed: %v", err)
			}
		}
	}
}
