// Internal unit tests for the edge listener's address handling: the
// loopback default is a security guarantee (a bare port must NEVER bind a
// public interface), so it gets a direct test, not just an e2e side-effect.
package remote

import (
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeEdgeAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"8080", "127.0.0.1:8080"},   // bare port → loopback
		{":8080", "127.0.0.1:8080"},  // :port → loopback
		{" 8080 ", "127.0.0.1:8080"}, // padded
		{"0.0.0.0:9", "0.0.0.0:9"},   // explicit wildcard host honored
		{"10.0.0.5:9", "10.0.0.5:9"}, // explicit concrete host honored
		{"[::1]:9", "[::1]:9"},       // explicit IPv6 host honored
	} {
		got, err := normalizeEdgeAddr(tc.in)
		if err != nil {
			t.Fatalf("normalizeEdgeAddr(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("normalizeEdgeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "  ", "host:", ":99999x", "a:b:c"} {
		if _, err := normalizeEdgeAddr(bad); err == nil {
			t.Errorf("normalizeEdgeAddr(%q) must fail", bad)
		}
	}
}

// The edge can attest only to its own socket peer, so a LAN client's
// self-reported X-Forwarded-For / X-Real-IP must never cross the tunnel —
// otherwise the relay (TRUST_PROXY_HEADERS on) reads the forged first XFF
// line and records a spoofed client_ip. Credentials must still pass (that is
// the path's contract), and framing must still be stripped.
func TestEdgeRequestHeadersDropsClientIP(t *testing.T) {
	in := http.Header{}
	in.Add("X-Forwarded-For", "1.2.3.4")
	in.Add("X-Forwarded-For", "5.6.7.8") // two client lines
	in.Add("X-Real-IP", "9.9.9.9")
	in.Add("Authorization", "Bearer sk-real")
	in.Add("X-Api-Key", "sk-key")
	in.Add("X-Kept", "yes")
	in.Add("Connection", "keep-alive") // framing: dropped

	got := edgeRequestHeaders(in)
	have := map[string][]string{}
	for _, kv := range got {
		have[strings.ToLower(kv[0])] = append(have[strings.ToLower(kv[0])], kv[1])
	}
	for _, gone := range []string{"x-forwarded-for", "x-real-ip", "connection"} {
		if _, ok := have[gone]; ok {
			t.Errorf("%q must be dropped by edgeRequestHeaders, got %v", gone, have[gone])
		}
	}
	if got := have["authorization"]; len(got) != 1 || got[0] != "Bearer sk-real" {
		t.Errorf("Authorization must pass verbatim, got %v", have["authorization"])
	}
	if got := have["x-api-key"]; len(got) != 1 || got[0] != "sk-key" {
		t.Errorf("x-api-key must pass verbatim, got %v", have["x-api-key"])
	}
	if got := have["x-kept"]; len(got) != 1 || got[0] != "yes" {
		t.Errorf("ordinary header must pass, got %v", have["x-kept"])
	}
}
