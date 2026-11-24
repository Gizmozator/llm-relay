// The machine-channel TLS gate (plan 2026-11-24-remote-tls-v1): the three
// connect endpoints sit behind RequireTLS, which must refuse cleartext with
// 403, accept a direct TLS handshake, and accept X-Forwarded-Proto: https
// only when TrustProxy is on (the TLS-terminating-proxy deployment).
// These use a bare Server + stub handler: the gate runs before any auth, so
// nothing else needs wiring.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func gateEnv(t *testing.T, trustProxy bool) (*httptest.Server, *httptest.Server) {
	t.Helper()
	s := &Server{TrustProxy: trustProxy}
	mux := http.NewServeMux()
	teapot := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mux.Handle("GET /federation/v1/connect", s.RequireTLS(teapot))
	mux.Handle("GET /federation/v1/ping", s.RequireTLS(teapot))
	mux.Handle("GET /remote/v1/connect", s.RequireTLS(teapot))
	plain := httptest.NewServer(mux)
	tlsSrv := httptest.NewTLSServer(mux)
	t.Cleanup(func() { plain.Close(); tlsSrv.Close() })
	return plain, tlsSrv
}

func gateGet(t *testing.T, url string, headers map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// Both gated paths answer 403 over cleartext — the token would be in the
// clear on the wire, so auth never runs.
func TestGateCleartextRefused(t *testing.T) {
	plain, _ := gateEnv(t, false)
	for _, path := range []string{"/federation/v1/connect", "/remote/v1/connect"} {
		if got := gateGet(t, plain.URL+path, nil); got != http.StatusForbidden {
			t.Errorf("%s cleartext = %d, want 403", path, got)
		}
	}
}

// A direct TLS handshake satisfies the gate (403 gone; the stub's 418
// proves the handler ran).
func TestGateTLSPasses(t *testing.T) {
	_, tlsSrv := gateEnv(t, false)
	for _, path := range []string{"/federation/v1/connect", "/remote/v1/connect"} {
		resp, err := tlsSrv.Client().Get(tlsSrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTeapot {
			t.Errorf("%s over TLS = %d, want pass-through", path, resp.StatusCode)
		}
	}
}

// X-Forwarded-Proto: https is the proxy's claim, forgeable by any client
// that can reach the origin port: it counts only with TrustProxy on.
func TestGateForwardedProto(t *testing.T) {
	plain, _ := gateEnv(t, false)
	if got := gateGet(t, plain.URL+"/remote/v1/connect",
		map[string]string{"X-Forwarded-Proto": "https"}); got != http.StatusForbidden {
		t.Errorf("XFP without TrustProxy = %d, want 403", got)
	}
	plain2, _ := gateEnv(t, true)
	if got := gateGet(t, plain2.URL+"/remote/v1/connect",
		map[string]string{"X-Forwarded-Proto": "https"}); got != http.StatusTeapot {
		t.Errorf("XFP with TrustProxy = %d, want pass-through", got)
	}
	// A proxy that forwards something else (http, garbage) does not pass.
	if got := gateGet(t, plain2.URL+"/federation/v1/connect",
		map[string]string{"X-Forwarded-Proto": "http"}); got != http.StatusForbidden {
		t.Errorf("XFP=http with TrustProxy = %d, want 403", got)
	}
	// Case and padding tolerance: real proxies vary.
	if got := gateGet(t, plain2.URL+"/federation/v1/ping",
		map[string]string{"X-Forwarded-Proto": " HTTPS "}); got != http.StatusTeapot {
		t.Errorf("padded XFP = %d, want gate pass", got)
	}
}

// SelfTLS hardening (ocr finding): when the relay binds its own TLS listener
// (TLS_PORT!=0) there is no proxy terminating TLS in front of the machine
// channels, so X-Forwarded-Proto must NOT be honoured even with TrustProxy on
// (which operators also need for client-IP handling) — otherwise a cleartext
// request on the plain port forging the header would bypass the gate and leak
// the token. Direct TLS on the wss listener still passes.
func TestGateSelfTLSIgnoresForwardedProto(t *testing.T) {
	s := &Server{TrustProxy: true, SelfTLS: true}
	mux := http.NewServeMux()
	teapot := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mux.Handle("GET /federation/v1/connect", s.RequireTLS(teapot))
	mux.Handle("GET /remote/v1/connect", s.RequireTLS(teapot))
	plain := httptest.NewServer(mux)
	tlsSrv := httptest.NewTLSServer(mux)
	t.Cleanup(func() { plain.Close(); tlsSrv.Close() })

	for _, path := range []string{"/federation/v1/connect", "/remote/v1/connect"} {
		if got := gateGet(t, plain.URL+path,
			map[string]string{"X-Forwarded-Proto": "https"}); got != http.StatusForbidden {
			t.Errorf("%s XFP with TrustProxy+SelfTLS = %d, want 403 (forged bypass must fail)", path, got)
		}
		resp, err := tlsSrv.Client().Get(tlsSrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTeapot {
			t.Errorf("%s over direct TLS with SelfTLS = %d, want pass-through", path, resp.StatusCode)
		}
	}
}
