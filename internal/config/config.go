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

	// Headless spoke (plan 2026-11-24-headless-spoke): boot fully configured
	// by env — one backend, exactly one model, one hub, zero dashboard
	// interaction. When HEADLESS=true, BACKEND_URL, MODEL_NAME, HUB_URL and
	// HUB_TOKEN are required (fail-fast like JWT_SECRET); when false (the
	// default) every var in this block is ignored and behavior is unchanged.
	Headless     bool   `env:"HEADLESS" envDefault:"false"`
	BackendURL   string `env:"BACKEND_URL"`
	BackendToken string `env:"BACKEND_TOKEN"`
	ModelName    string `env:"MODEL_NAME"`
	HubURL       string `env:"HUB_URL"`
	HubToken     string `env:"HUB_TOKEN"`
	// Prices for the single model row, integer micro-units per 1M tokens
	// (same unit as the dashboard price fields); 0 = free.
	PriceIn         int64 `env:"PRICE_IN" envDefault:"0"`
	PriceOut        int64 `env:"PRICE_OUT" envDefault:"0"`
	PriceCacheRead  int64 `env:"PRICE_CACHE_READ" envDefault:"0"`
	PriceCacheWrite int64 `env:"PRICE_CACHE_WRITE" envDefault:"0"`
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
	// A headless box has no dashboard to fix a typo in: every var the
	// bootstrap needs must be present and non-empty at parse time, or the
	// relay would boot into a half-configured state it can never escape.
	if c.Headless {
		// Trim in place: a padded value passes the non-empty check but would
		// otherwise be stored verbatim (only hub_url is trimmed downstream),
		// surfacing later as a confusing probe/dial failure. A headless box
		// has no dashboard to fix the typo, so normalize here.
		for _, req := range []struct {
			key string
			val *string
		}{
			{"BACKEND_URL", &c.BackendURL}, {"MODEL_NAME", &c.ModelName},
			{"HUB_URL", &c.HubURL}, {"HUB_TOKEN", &c.HubToken},
		} {
			v := strings.TrimSpace(*req.val)
			*req.val = v
			if v == "" {
				return nil, fmt.Errorf("config: %s is required when HEADLESS=true", req.key)
			}
		}
		// The admin API refuses negative prices; headless writes the row
		// through the store directly, so enforce the same floor here — a
		// negative price would ride the catalog frame to the hub and bill
		// negative.
		for _, p := range []struct {
			key string
			val int64
		}{
			{"PRICE_IN", c.PriceIn}, {"PRICE_OUT", c.PriceOut},
			{"PRICE_CACHE_READ", c.PriceCacheRead}, {"PRICE_CACHE_WRITE", c.PriceCacheWrite},
		} {
			if p.val < 0 {
				return nil, fmt.Errorf("config: %s must not be negative (got %d)", p.key, p.val)
			}
		}
	}
	if c.ListenAddr == c.AdminListenAddr {
		return nil, fmt.Errorf("config: relay and admin listeners must not share an address (%s)", c.ListenAddr)
	}
	return c, nil
}
