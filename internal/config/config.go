// Package config loads all runtime configuration from environment variables.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"llm-relay/internal/ratelimit"
	"llm-relay/internal/tlscert"
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

	// TLS listener for machine channels (plan 2026-11-24-remote-tls-v1):
	// the relay mux is ALSO served over TLS on this port using the
	// dashboard-generated (or installed) self-signed certificate; peers pin
	// its fingerprint. "0" disables the listener (e.g. when a reverse
	// proxy terminates TLS instead). The plain relay port is untouched.
	TLSListenAddr string `env:"TLS_ADDR"`
	TLSPort       string `env:"TLS_PORT" envDefault:"8443"`

	// Headless connector mode (plan 2026-11-24-remote-backend-v1): the box
	// binds nothing and runs one job — dial a relay's /remote/v1/connect
	// and expose ENGINE_URL through it as a remote backend. When
	// HEADLESS=true, RELAY_URL, RELAY_TOKEN and ENGINE_URL are required
	// (fail-fast at config.Load); when false (the default) every var in
	// this block is ignored and behavior is unchanged. A headless box
	// needs no JWT_SECRET — it never serves the admin surface.
	// RELAY_PIN (plan 2026-11-24-remote-tls-v1) is the optional
	// sha256 fingerprint the relay cert must match — required in practice
	// for self-signed relays, empty for a CA-issued cert (proxy case).
	Headless    bool   `env:"HEADLESS" envDefault:"false"`
	RelayURL    string `env:"RELAY_URL"`
	RelayToken  string `env:"RELAY_TOKEN"`
	RelayPin    string `env:"RELAY_PIN"`
	EngineURL   string `env:"ENGINE_URL"`
	EngineToken string `env:"ENGINE_TOKEN"`
	// ProxyListen (ticket #17) opts the connector into a local edge proxy:
	// a /v1/* listener that forwards client traffic over the channel to
	// the relay (the relay authenticates each request's sk- key itself).
	// Empty (the default) keeps the connector's "binds nothing" invariant.
	// A bare port or ":port" binds 127.0.0.1; only an explicit host binds
	// elsewhere — see remote.NewEdge and the README warning.
	ProxyListen string `env:"PROXY_LISTEN"`

	// Abuse/DDoS protection tiers (ticket #18): one rate/burst pair per
	// tier plus the login lockout and the global in-flight cap. Every
	// default is non-zero and sized so normal traffic never sees a
	// 429/503; setting a tier's rate or burst to 0 turns that tier off
	// (the dev/LAN escape). The envDefault strings below must match
	// ratelimit.DefaultConfig — TestRateLimitDefaultsMatchesPackage
	// guards the pairing so the two lists cannot drift.
	RateLoginRate       float64 `env:"RATELIMIT_LOGIN_RATE" envDefault:"0.2"`
	RateLoginBurst      float64 `env:"RATELIMIT_LOGIN_BURST" envDefault:"10"`
	RateConnectRate     float64 `env:"RATELIMIT_CONNECT_RATE" envDefault:"0.2"`
	RateConnectBurst    float64 `env:"RATELIMIT_CONNECT_BURST" envDefault:"10"`
	RateV1IPRate        float64 `env:"RATELIMIT_V1_IP_RATE" envDefault:"100"`
	RateV1IPBurst       float64 `env:"RATELIMIT_V1_IP_BURST" envDefault:"200"`
	RateV1KeyRate       float64 `env:"RATELIMIT_V1_KEY_RATE" envDefault:"50"`
	RateV1KeyBurst      float64 `env:"RATELIMIT_V1_KEY_BURST" envDefault:"100"`
	RateRemoteChanRate  float64 `env:"RATELIMIT_REMOTE_CHANNEL_RATE" envDefault:"100"`
	RateRemoteChanBurst float64 `env:"RATELIMIT_REMOTE_CHANNEL_BURST" envDefault:"200"`
	RateAdminRate       float64 `env:"RATELIMIT_ADMIN_RATE" envDefault:"100"`
	RateAdminBurst      float64 `env:"RATELIMIT_ADMIN_BURST" envDefault:"200"`
	RateLockAfter       int     `env:"RATELIMIT_LOGIN_LOCK_AFTER" envDefault:"5"`
	RateLockSeconds     int     `env:"RATELIMIT_LOGIN_LOCK_SECONDS" envDefault:"30"`
	RateGlobalMax       int64   `env:"RATELIMIT_GLOBAL_MAX_INFLIGHT" envDefault:"1024"`
}

// RateLimits assembles the resolved tier configuration from the environment.
// Values are passed through verbatim: a zero rate/burst (or zero global cap)
// means "tier off", which only the operator can set — Load's envDefaults
// guarantee the shipped defaults are never zero.
func (c *Config) RateLimits() ratelimit.Config {
	d := ratelimit.DefaultConfig()
	return ratelimit.Config{
		Login:         ratelimit.Rate{Rate: c.RateLoginRate, Burst: c.RateLoginBurst},
		Connect:       ratelimit.Rate{Rate: c.RateConnectRate, Burst: c.RateConnectBurst},
		V1IP:          ratelimit.Rate{Rate: c.RateV1IPRate, Burst: c.RateV1IPBurst},
		V1Key:         ratelimit.Rate{Rate: c.RateV1KeyRate, Burst: c.RateV1KeyBurst},
		RemoteChannel: ratelimit.Rate{Rate: c.RateRemoteChanRate, Burst: c.RateRemoteChanBurst},
		Admin:         ratelimit.Rate{Rate: c.RateAdminRate, Burst: c.RateAdminBurst},
		Lockout: ratelimit.Lockout{
			Threshold: c.RateLockAfter,
			Base:      time.Duration(c.RateLockSeconds) * time.Second,
			// Escalation cap and idle reset are not env-tunable: they
			// are safety shape, not sizing.
			Max:        d.Lockout.Max,
			IdleTTL:    d.Lockout.IdleTTL,
			MaxEntries: d.Lockout.MaxEntries,
		},
		GlobalMax: c.RateGlobalMax,
	}
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
		// Hard TLS rule (plan 2026-11-24-remote-tls-v1): the connect token
		// and all tunneled traffic ride this URL — cleartext is never
		// acceptable, and there is deliberately no opt-out. ENGINE_URL may
		// stay http: that hop is the box's own LAN/loopback.
		if !strings.HasPrefix(c.RelayURL, "https://") {
			return nil, fmt.Errorf("config: RELAY_URL must be https:// — remote connections require TLS (got %q)", c.RelayURL)
		}
		// Validate the pin at boot: a typo in a fingerprint on a box with
		// no dashboard would otherwise surface as an endless
		// handshake-failure redial loop.
		c.RelayPin = strings.TrimSpace(c.RelayPin)
		if c.RelayPin != "" {
			if _, err := tlscert.ParsePin(c.RelayPin); err != nil {
				return nil, fmt.Errorf("config: RELAY_PIN: %w", err)
			}
		}
		c.EngineToken = strings.TrimSpace(c.EngineToken)
		// The edge listener address is optional; trimmed so a padded value
		// does not fail NewEdge's host:port parse with a confusing message
		// at boot on a box nobody can open a dashboard on.
		c.ProxyListen = strings.TrimSpace(c.ProxyListen)
	}
	// A connector binds no listeners at all, so the clash rule is moot
	// there — refusing boot over a stale server env file's identical
	// addresses would be refusing for an irrelevant reason.
	if !c.Headless && c.ListenAddr == c.AdminListenAddr {
		return nil, fmt.Errorf("config: relay and admin listeners must not share an address (%s)", c.ListenAddr)
	}
	// TLS listener (server modes only): "0" disables; anything else must
	// be a real port, checked at boot rather than at first handshake.
	if !c.Headless {
		if c.TLSListenAddr == "" {
			c.TLSListenAddr = ":" + c.TLSPort
		}
		if c.TLSPort != "0" {
			p, err := strconv.Atoi(c.TLSPort)
			if err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("config: TLS_PORT must be 0 (off) or a port number (got %q)", c.TLSPort)
			}
			if c.TLSListenAddr == c.ListenAddr || c.TLSListenAddr == c.AdminListenAddr {
				return nil, fmt.Errorf("config: TLS listener must not share an address (%s)", c.TLSListenAddr)
			}
		}
	}
	return c, nil
}
