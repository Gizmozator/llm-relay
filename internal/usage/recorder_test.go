package usage

import (
	"testing"
	"time"

	"llm-relay/internal/store"
)

// A stream finishing after the shutdown deadline may still Submit; that must
// drop the event, not panic on the closed channel.
func TestSubmitAfterCloseDrops(t *testing.T) {
	r := NewRecorder(nil)
	// No writer goroutine: Close returns after its wait timeout.
	r.Close(50 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		r.Submit(&store.UsageEvent{UserID: 1, KeyID: 1})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit after Close did not return")
	}
	r.Close(time.Second) // second Close is a no-op
}
