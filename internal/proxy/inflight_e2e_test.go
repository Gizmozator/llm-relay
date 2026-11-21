package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A request must count as in-flight while the upstream is working and be
// released when the response completes.
func TestProxyInFlightHook(t *testing.T) {
	processing := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(processing)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	h := newHarnessWithURL(t, up)

	done := make(chan *http.Response, 1)
	go func() {
		// Not h.post: t.Fatal must not fire from this goroutine.
		req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"pub-model","messages":[{"role":"user","content":"x"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- nil
			return
		}
		done <- resp
	}()

	select {
	case <-processing:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
	}
	if n := h.ph.Flight.Snapshot(0)["pub-model"]; n != 1 {
		t.Fatalf("in-flight during upstream = %d, want 1", n)
	}

	close(release)
	resp := <-done
	if resp == nil {
		t.Fatal("relay request failed")
	}
	resp.Body.Close()
	if n := h.ph.Flight.Snapshot(0)["pub-model"]; n != 0 {
		t.Fatalf("in-flight after completion = %d, want 0", n)
	}
}
