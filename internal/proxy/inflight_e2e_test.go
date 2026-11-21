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
	// The admin connections view reads the same tracker: the record must
	// carry who (user + key), what (model + endpoint) and the session.
	list := h.ph.Flight.List(0)
	if len(list) != 1 || list[0].Model != "pub-model" || list[0].Endpoint != "chat" ||
		list[0].User == "" || list[0].Key == "" || list[0].Session == "" {
		t.Fatalf("connection record = %+v, want user/key/model/endpoint/session set", list)
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
	if n := len(h.ph.Flight.List(0)); n != 0 {
		t.Fatalf("connections after completion = %d, want 0", n)
	}
	// The finished stream stays listed briefly (recent memory) so short
	// requests are visible between dashboard polls.
	if rec := h.ph.Flight.Recent(0); len(rec) != 1 || rec[0].Model != "pub-model" || rec[0].End.IsZero() {
		t.Fatalf("recent after completion = %+v, want one finished pub-model record", rec)
	}
}
