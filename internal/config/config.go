// Package config loads all runtime configuration from environment variables.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	ListenAddr        string `env:"LISTEN_ADDR"`
	Port              string `env:"PORT" envDefault:"8080"`
	AdminListenAddr   string `env:"ADMIN_ADDR"`
	AdminPort         string `env:"ADMIN_PORT" envDefault:"8081"`
	DBPath            string `env:"DB_PATH" envDefault:"/data/llm-relay.db"`
	JWTSecret         string `env:"JWT_SECRET,required"`
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
	// A short or empty HS256 secret makes tokens offline-bruteforceable.
	// HS256 (RFC 7518) wants a key at least as long as the hash output —
	// 32 bytes; anything less is not a real secret for this algorithm.
	if len(c.JWTSecret) < 32 {
		return nil, fmt.Errorf("config: JWT_SECRET must be at least 32 characters (openssl rand -hex 32)")
	}
	if c.ListenAddr == c.AdminListenAddr {
		return nil, fmt.Errorf("config: relay and admin listeners must not share an address (%s)", c.ListenAddr)
	}
	return c, nil
}
