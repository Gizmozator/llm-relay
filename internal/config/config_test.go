package config

import "testing"

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

func TestLoadRejectsSharedAddr(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("ADMIN_ADDR", ":9000")
	if _, err := Load(); err == nil {
		t.Fatal("relay and admin on the same address must be rejected")
	}
}

// headlessEnv is a complete, valid HEADLESS=true environment; each
// subtest breaks one required var.
func headlessEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("HEADLESS", "true")
	t.Setenv("BACKEND_URL", "http://backend.example.fi:8080")
	t.Setenv("MODEL_NAME", "qwen3-coder")
	t.Setenv("HUB_URL", "https://relay.example.fi")
	t.Setenv("HUB_TOKEN", "fed-secret-token")
}

func TestLoadHeadlessRequiresVars(t *testing.T) {
	for _, key := range []string{"BACKEND_URL", "MODEL_NAME", "HUB_URL", "HUB_TOKEN"} {
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
	if !c.Headless || c.ModelName != "qwen3-coder" {
		t.Fatalf("parsed config wrong: %+v", c)
	}
	// Price defaults: 0 (free) unless set.
	if c.PriceIn != 0 || c.PriceOut != 0 || c.PriceCacheRead != 0 || c.PriceCacheWrite != 0 {
		t.Errorf("price defaults = %d/%d/%d/%d, want 0/0/0/0",
			c.PriceIn, c.PriceOut, c.PriceCacheRead, c.PriceCacheWrite)
	}
	t.Setenv("PRICE_IN", "1000")
	t.Setenv("PRICE_CACHE_WRITE", "7")
	c, err = Load()
	if err != nil {
		t.Fatalf("valid headless config rejected: %v", err)
	}
	if c.PriceIn != 1000 || c.PriceCacheWrite != 7 {
		t.Errorf("prices = %d/%d, want 1000/7", c.PriceIn, c.PriceCacheWrite)
	}
}

// A negative price would ride the catalog frame to the hub and bill negative;
// the admin API refuses it, so the headless env path must too.
func TestLoadHeadlessRejectsNegativePrice(t *testing.T) {
	for _, key := range []string{"PRICE_IN", "PRICE_OUT", "PRICE_CACHE_READ", "PRICE_CACHE_WRITE"} {
		t.Run(key, func(t *testing.T) {
			headlessEnv(t)
			t.Setenv(key, "-100")
			if _, err := Load(); err == nil {
				t.Fatalf("%s=-100 must be rejected when HEADLESS=true", key)
			}
		})
	}
}

// Padded required values are trimmed in place (not just for the empty check),
// so a stray space in the env does not surface later as a probe/dial failure.
func TestLoadHeadlessTrimsRequired(t *testing.T) {
	headlessEnv(t)
	t.Setenv("BACKEND_URL", "  http://backend.example.fi:8080  ")
	t.Setenv("MODEL_NAME", " qwen3-coder ")
	t.Setenv("HUB_URL", "\thttps://relay.example.fi ")
	c, err := Load()
	if err != nil {
		t.Fatalf("padded headless config rejected: %v", err)
	}
	if c.BackendURL != "http://backend.example.fi:8080" ||
		c.ModelName != "qwen3-coder" ||
		c.HubURL != "https://relay.example.fi" {
		t.Errorf("values not trimmed: %q %q %q", c.BackendURL, c.ModelName, c.HubURL)
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
