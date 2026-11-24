// Package config loads all runtime configuration from environment variables.
package config

import (
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	ListenAddr        string `env:"LISTEN_ADDR"`
	Port              string `env:"PORT" envDefault:"8080"`
	AdminListenAddr   string `env:"ADMIN_ADDR"`
	AdminPort         string `env:"ADMIN_PORT" envDefault:"8081"`
	DBPath            string `env:"DB_PATH" envDefault:"/data/llm-relay.db"`
	JWTSecret         string `env:"JWT_SECRET"`
	AdminUsername     string `env:"ADMIN_USERNAME"`
	AdminPassword     string `env:"ADMIN_PASSWORD"`
	TrustProxyHeaders bool   `env:"TRUST_PROXY_HEADERS" envDefault:"false"`
	// URLs advertised in the dashboard copy buttons and interpolated into
	// the agent guide, so users copy the right endpoints (behind a reverse
	// proxy these differ from the listen ports). Used verbatim — include the
	// scheme; a hostname or a plain IP:port both work (direct LAN hosting:
	// http://192.0.2.10:8080). PUBLIC_RELAY_URL is the server ROOT (e.g.
	// https://llm.example.com, no /v1); the dashboard appends /v1 where the
	// OpenAI-style base URL is shown, and the agent endpoints link off the
	// root.
	PublicRelayURL     string `env:"PUBLIC_RELAY_URL"`
	PublicDashboardURL string `env:"PUBLIC_DASHBOARD_URL"`

	// Headless connector mode (plan 2026-11-24-remote-backend-v1): the box
	// binds nothing and runs one job — dial a relay's /remote/v1/connect
	// and expose ENGINE_URL through it as a remote backend. When
	// HEADLESS=true, RELAY_URL, RELAY_TOKEN and ENGINE_URL are required
	// (fail-fast at config.Load); when false (the default) every var in
	// this block is ignored and behavior is unchanged. A headless box
	// needs no JWT_SECRET — it never serves the admin surface.
	Headless    bool   `env:"HEADLESS" envDefault:"false"`
	RelayURL    string `env:"RELAY_URL"`
	RelayToken  string `env:"RELAY_TOKEN"`
	EngineURL   string `env:"ENGINE_URL"`
	EngineToken string `env:"ENGINE_TOKEN"`
}

// Load parses the environment and resolves the effective listen addresses.
func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":" + c.Port
	}
	if c.AdminListenAddr == "" {
		c.AdminListenAddr = ":" + c.AdminPort
	}
	// JWT_SECRET signs the dashboard/admin session tokens. A headless box
	// never binds the admin listener, so it never signs or parses a JWT and
	// needs no secret at all; a full server must have a real one. When set in
	// any mode it must clear the HS256 floor (RFC 7518: key ≥ hash output =
	// 32 bytes) — a short secret is offline-bruteforceable.
	if c.JWTSecret != "" && len(c.JWTSecret) < 32 {
		return nil, fmt.Errorf("config: JWT_SECRET must be at least 32 characters (openssl rand -hex 32)")
	}
	if !c.Headless && c.JWTSecret == "" {
		return nil, fmt.Errorf("config: JWT_SECRET is required (openssl rand -hex 32)")
	}
	// A headless box has no dashboard to fix a typo in: every var the
	// connector needs must be present and non-empty at parse time, or it
	// would boot into a half-configured state it can never escape.
	if c.Headless {
		// Trim in place: a padded value passes the non-empty check but
		// would otherwise be used verbatim, surfacing later as a confusing
		// dial/probe failure. A headless box has no dashboard to fix the
		// typo, so normalize here.
		for _, req := range []struct {
			key string
			val *string
		}{
			{"RELAY_URL", &c.RelayURL}, {"RELAY_TOKEN", &c.RelayToken},
			{"ENGINE_URL", &c.EngineURL},
		} {
			v := strings.TrimSpace(*req.val)
			*req.val = v
			if v == "" {
				return nil, fmt.Errorf("config: %s is required when HEADLESS=true", req.key)
			}
		}
		// Shape, not just presence: a scheme-less URL ("localhost:8080")
		// would pass the empty check and then fail per request inside the
		// connector — the exact confusing late failure this block exists
		// to prevent.
		for _, u := range []struct {
			key string
			val string
		}{
			{"RELAY_URL", c.RelayURL}, {"ENGINE_URL", c.EngineURL},
		} {
			if !strings.HasPrefix(u.val, "http://") && !strings.HasPrefix(u.val, "https://") {
				return nil, fmt.Errorf("config: %s must start with http:// or https:// (got %q)", u.key, u.val)
			}
		}
		c.EngineToken = strings.TrimSpace(c.EngineToken)
	}
	// A connector binds no listeners at all, so the clash rule is moot
	// there — refusing boot over a stale server env file's identical
	// addresses would be refusing for an irrelevant reason.
	if !c.Headless && c.ListenAddr == c.AdminListenAddr {
		return nil, fmt.Errorf("config: relay and admin listeners must not share an address (%s)", c.ListenAddr)
	}
	return c, nil
}
