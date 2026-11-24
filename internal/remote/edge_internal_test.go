// Internal unit tests for the edge listener's address handling: the
// loopback default is a security guarantee (a bare port must NEVER bind a
// public interface), so it gets a direct test, not just an e2e side-effect.
package remote

import "testing"

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
