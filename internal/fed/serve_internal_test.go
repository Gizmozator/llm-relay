// Internal (package fed) regression tests for the raw2 fix round.
package fed

import (
	"bytes"
	"testing"
)

// Item 43: the meter window must SLIDE to the last cap bytes — usage
// payloads ride the END of the body, and the old early-return froze the
// window at the FIRST cap bytes, so any response over the cap lost its
// usage tail and went unmetered.
func TestTransitMeterWindowSlides(t *testing.T) {
	mt := &transitMeter{}
	first := bytes.Repeat([]byte("A"), transitMeterCap)
	mt.feed(first)
	if len(mt.tail) != transitMeterCap {
		t.Fatalf("window = %d bytes, want cap %d", len(mt.tail), transitMeterCap)
	}
	// A later chunk — the usage tail of a long response — must land in
	// the window (and push the old bytes out).
	tail := []byte(`data: {"usage":{"completion_tokens":9}}`)
	mt.feed(tail)
	if !bytes.HasSuffix(mt.tail, tail) {
		t.Error("window froze at the first cap bytes; the usage tail was dropped")
	}
	if len(mt.tail) != transitMeterCap {
		t.Errorf("window = %d bytes after trim, want cap %d", len(mt.tail), transitMeterCap)
	}
	// A chunk larger than the cap keeps its own tail.
	huge := append(bytes.Repeat([]byte("B"), transitMeterCap*2), "END"...)
	mt.feed(huge)
	if !bytes.HasSuffix(mt.tail, []byte("END")) || len(mt.tail) != transitMeterCap {
		t.Errorf("oversized feed: suffix %q len %d, want END/%d", mt.tail[len(mt.tail)-3:], len(mt.tail), transitMeterCap)
	}
}
