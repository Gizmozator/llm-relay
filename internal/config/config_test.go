package config

import (
	"os"
	"strings"
	"testing"

	"llm-relay/internal/ratelimit"
)

func TestLoadValidatesSecret(t *testing.T) {
	t.Setenv("JWT_SECRET", "short")
	if _, err := Load(); err == nil {
		t.Fatal("short JWT_SECRET must be rejected")
	}
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcde") // 31 chars
	if _, err := Load(); err == nil {
		t.Fatal("31-char JWT_SECRET must be rejected (HS256 floor is 32)")
	}
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("PORT", "8080")
	t.Setenv("ADMIN_PORT", "8081")
	if _, err := Load(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// A full server signs dashboard/admin session JWTs, so it must have a real
// secret; an unset one is refused.
func TestLoadRequiresJWTForServer(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("JWT_SECRET must be required when HEADLESS=false")
	}
}

// A headless box never binds the admin listener, so it never signs or parses a
// JWT — JWT_SECRET is not required there.
func TestLoadHeadlessNoJWTNeeded(t *testing.T) {
	headlessEnv(t) // deliberately sets no JWT_SECRET
	c, err := Load()
	if err != nil {
		t.Fatalf("headless config without JWT_SECRET rejected: %v", err)
	}
	if c.JWTSecret != "" {
		t.Errorf("JWTSecret = %q, want empty", c.JWTSecret)
	}
}

// If a headless box DOES set JWT_SECRET it must still clear the HS256 floor —
// a short secret is never acceptable, even if it happens to go unused.
func TestLoadHeadlessRejectsShortJWT(t *testing.T) {
	headlessEnv(t)
	t.Setenv("JWT_SECRET", "tooshort")
	if _, err := Load(); err == nil {
		t.Fatal("short JWT_SECRET must be rejected even when HEADLESS=true")
	}
}

func TestLoadRejectsSharedAddr(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("ADMIN_ADDR", ":9000")
	if _, err := Load(); err == nil {
		t.Fatal("relay and admin on the same address must be rejected")
	}
}

// headlessEnv is a complete, valid HEADLESS=true environment; each
// subtest breaks one required var. Note it sets NO JWT_SECRET: a headless
// box never serves the admin surface, so it needs none (see below).
func headlessEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HEADLESS", "true")
	t.Setenv("RELAY_URL", "https://relay.example.fi")
	t.Setenv("RELAY_TOKEN", "rb-secret-token")
	t.Setenv("ENGINE_URL", "http://localhost:8080")
}

func TestLoadHeadlessRequiresVars(t *testing.T) {
	for _, key := range []string{"RELAY_URL", "RELAY_TOKEN", "ENGINE_URL"} {
		t.Run(key+"_empty", func(t *testing.T) {
			headlessEnv(t)
			t.Setenv(key, "")
			if _, err := Load(); err == nil {
				t.Fatalf("%s must be required when HEADLESS=true", key)
			}
		})
		t.Run(key+"_whitespace", func(t *testing.T) {
			headlessEnv(t)
			t.Setenv(key, "  ")
			if _, err := Load(); err == nil {
				t.Fatalf("whitespace-only %s must be rejected when HEADLESS=true", key)
			}
		})
	}
}

func TestLoadHeadlessValid(t *testing.T) {
	headlessEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("valid headless config rejected: %v", err)
	}
	if !c.Headless || c.RelayURL != "https://relay.example.fi" ||
		c.RelayToken != "rb-secret-token" || c.EngineURL != "http://localhost:8080" {
		t.Fatalf("parsed config wrong: %+v", c)
	}
	// ENGINE_TOKEN is the only optional var in the block.
	if c.EngineToken != "" {
		t.Errorf("EngineToken = %q, want empty", c.EngineToken)
	}
	t.Setenv("ENGINE_TOKEN", "eng-key")
	c, err = Load()
	if err != nil {
		t.Fatalf("valid headless config rejected: %v", err)
	}
	if c.EngineToken != "eng-key" {
		t.Errorf("EngineToken = %q, want eng-key", c.EngineToken)
	}
}

// Padded required values are trimmed in place (not just for the empty check),
// so a stray space in the env does not surface later as a dial failure.
func TestLoadHeadlessTrimsRequired(t *testing.T) {
	headlessEnv(t)
	t.Setenv("RELAY_URL", "  https://relay.example.fi  ")
	t.Setenv("RELAY_TOKEN", " rb-secret-token ")
	t.Setenv("ENGINE_URL", "\thttp://localhost:8080 ")
	c, err := Load()
	if err != nil {
		t.Fatalf("padded headless config rejected: %v", err)
	}
	if c.RelayURL != "https://relay.example.fi" ||
		c.RelayToken != "rb-secret-token" ||
		c.EngineURL != "http://localhost:8080" {
		t.Errorf("values not trimmed: %q %q %q", c.RelayURL, c.RelayToken, c.EngineURL)
	}
}

// HEADLESS=false (the default) must ignore the whole headless block: an env
// without any of the new vars parses exactly as before.
func TestLoadHeadlessOffIgnoresBlock(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	c, err := Load()
	if err != nil {
		t.Fatalf("config without headless vars rejected: %v", err)
	}
	if c.Headless {
		t.Error("HEADLESS must default to false")
	}
}

// A scheme-less URL passes the non-empty check but then fails per request
// inside the connector (unsupported protocol scheme) — Load must reject the
// shape, not just the absence, because a headless box has no dashboard to
// fix the typo in.
func TestLoadHeadlessRejectsBadURLShape(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"RELAY_URL", "relay.example.fi"},
		{"RELAY_URL", "ws://relay.example.fi"},
		{"ENGINE_URL", "localhost:8080"},
		{"ENGINE_URL", "https:/relay.example.fi"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			headlessEnv(t)
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q must be rejected", tc.key, tc.val)
			}
		})
	}
}

// A connector binds no listeners at all: a server env file whose relay and
// admin addresses collide must not refuse the connector boot.
func TestLoadHeadlessIgnoresAddressClash(t *testing.T) {
	headlessEnv(t)
	t.Setenv("LISTEN_ADDR", ":8080")
	t.Setenv("ADMIN_ADDR", ":8080")
	if _, err := Load(); err != nil {
		t.Fatalf("headless with colliding addresses rejected: %v", err)
	}
}

// Hard TLS rule (plan 2026-11-24-remote-tls-v1): the connector never dials
// cleartext — no opt-out exists, so an http:// RELAY_URL must refuse boot
// (the shape check alone would happily accept it).
func TestLoadHeadlessRequiresHTTPS(t *testing.T) {
	headlessEnv(t)
	t.Setenv("RELAY_URL", "http://relay.example.fi")
	if _, err := Load(); err == nil {
		t.Fatal("http:// RELAY_URL must be refused")
	}
}

// A fingerprint typo on a dashboard-less box would otherwise loop forever
// on handshake failures — ParsePin validates at boot.
func TestLoadHeadlessPinValidation(t *testing.T) {
	headlessEnv(t)
	t.Setenv("RELAY_PIN", "sha256:"+strings.Repeat("ab", 32))
	if _, err := Load(); err != nil {
		t.Fatalf("valid pin refused: %v", err)
	}
	t.Setenv("RELAY_PIN", "sha256:not-hex")
	if _, err := Load(); err == nil {
		t.Fatal("malformed RELAY_PIN must be refused")
	}
}

// TLS_PORT: 0 (off) or a real port, and never colliding with the other
// listeners — checked at boot, not at first handshake.
func TestLoadTLSPort(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSPort != "8443" || c.TLSListenAddr != ":8443" {
		t.Fatalf("default TLS = %s / %s, want 8443", c.TLSPort, c.TLSListenAddr)
	}
	t.Setenv("TLS_PORT", "0")
	if c, err := Load(); err != nil || c.TLSPort != "0" {
		t.Fatalf("TLS_PORT=0: (%v, %v)", c, err)
	}
	for _, bad := range []string{"abc", "-1", "70000"} {
		t.Setenv("TLS_PORT", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("TLS_PORT=%q must be refused", bad)
		}
	}
	t.Setenv("TLS_PORT", "8080")
	if _, err := Load(); err == nil {
		t.Fatal("TLS port colliding with the relay port must be refused")
	}
}

// The envDefault strings on the RATELIMIT_* fields must equal
// ratelimit.DefaultConfig: the package owns the sizing, the env block owns
// the plumbing, and a silent drift between them would ship defaults nobody
// reviewed. This test loads a clean environment (every RATELIMIT_* var
// unset) and compares the assembled config field by field.
func TestRateLimitDefaultsMatchesPackage(t *testing.T) {
	for _, key := range []string{
		"RATELIMIT_LOGIN_RATE", "RATELIMIT_LOGIN_BURST",
		"RATELIMIT_CONNECT_RATE", "RATELIMIT_CONNECT_BURST",
		"RATELIMIT_V1_IP_RATE", "RATELIMIT_V1_IP_BURST",
		"RATELIMIT_V1_KEY_RATE", "RATELIMIT_V1_KEY_BURST",
		"RATELIMIT_ADMIN_RATE", "RATELIMIT_ADMIN_BURST",
		"RATELIMIT_LOGIN_LOCK_AFTER", "RATELIMIT_LOGIN_LOCK_SECONDS",
		"RATELIMIT_GLOBAL_MAX_INFLIGHT",
	} {
		if old, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { os.Setenv(key, old) })
		}
		os.Unsetenv(key)
	}
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("PORT", "8080")
	t.Setenv("ADMIN_PORT", "8081")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, want := c.RateLimits(), ratelimit.DefaultConfig(); got != want {
		t.Errorf("env defaults drifted from ratelimit.DefaultConfig:\n got %+v\nwant %+v", got, want)
	}
}

// Explicit zeros must reach the config as zeros (0 = tier off).
func TestRateLimitZeroDisables(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("PORT", "8080")
	t.Setenv("ADMIN_PORT", "8081")
	t.Setenv("RATELIMIT_LOGIN_RATE", "0")
	t.Setenv("RATELIMIT_GLOBAL_MAX_INFLIGHT", "0")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lim := c.RateLimits()
	if lim.Login.Rate != 0 {
		t.Errorf("RATELIMIT_LOGIN_RATE=0 did not reach config: %+v", lim.Login)
	}
	if lim.GlobalMax != 0 {
		t.Errorf("RATELIMIT_GLOBAL_MAX_INFLIGHT=0 did not reach config: %d", lim.GlobalMax)
	}
}
