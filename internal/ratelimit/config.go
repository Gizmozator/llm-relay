package ratelimit

import "time"

// Config is the resolved shape of every abuse-protection tier (ticket #18).
// internal/config builds it from the environment; a zero Rate (or a zero
// GlobalMax) turns that tier off — the documented escape hatch for dev/LAN
// deployments. DefaultConfig is what a fresh deployment runs: every tier is
// sized so legitimate traffic never sees a 429/503 (see the per-field
// comments for the sizing argument), so default-config behaviour under
// normal load is identical to the pre-ticket server.
type Config struct {
	// Login is tier 1a: per-source-IP bucket on POST /api/auth/login.
	// Defaults match the pre-existing login limiter exactly (10 burst,
	// one credit per 5 s): a human mistyping a password a few times never
	// drains it, a guesser hits the wall after ten.
	Login Rate
	// Connect is tier 1b: per-source-IP bucket on the machine-channel
	// endpoints (/federation/v1/*, /remote/v1/connect). Same sizing as
	// login: token guessing costs a hash + DB lookup per attempt, and a
	// healthy connector resets the bucket on every successful connect, so
	// only failing traffic drains it.
	Connect Rate
	// V1IP is tier 3a: per-source-IP bucket on the public /v1 surface
	// (pre-auth, so bad-key floods are bounded too). 100 req/s sustained
	// with a 200 burst from ONE client IP is far beyond any honest
	// deployment — LLM requests take seconds each, so even a busy batch
	// client sits in the single digits per second; floods run thousands/s.
	V1IP Rate
	// V1Key is tier 3b: per-resolved-key bucket applied after auth,
	// before proxy work. 50 req/s sustained per API key: honest clients
	// on one key are limited by generation latency, not by this.
	V1Key Rate
	// Admin is tier 4: per-user bucket on the authenticated dashboard API.
	// Deliberately generous — dashboard polling is chatty but human: even
	// a page load firing a dozen parallel fetches every few seconds uses
	// a fraction of 100 req/s sustained.
	Admin Rate
	// Lockout is tier 2: per-(source IP, username) login-failure backoff.
	// Five consecutive wrong passwords lock the pair for 30 s, doubling
	// per additional failure up to 15 min. A human typo streak ends at
	// the first window; a password spray escalates fast.
	Lockout Lockout
	// GlobalMax is tier 6: server-wide in-flight request cap (503 when
	// saturated). Long LLM streams and live WS channels each hold a slot
	// for their whole duration, so the cap must sit far above realistic
	// concurrency; 1024 concurrent requests is a saturated box already.
	GlobalMax int64
}

// DefaultConfig returns the production defaults (see field comments).
func DefaultConfig() Config {
	return Config{
		Login:   Rate{Rate: 0.2, Burst: 10},
		Connect: Rate{Rate: 0.2, Burst: 10},
		V1IP:    Rate{Rate: 100, Burst: 200},
		V1Key:   Rate{Rate: 50, Burst: 100},
		Admin:   Rate{Rate: 100, Burst: 200},
		Lockout: Lockout{
			Threshold:  5,
			Base:       30 * time.Second,
			Max:        15 * time.Minute,
			IdleTTL:    15 * time.Minute,
			MaxEntries: StoreMax,
		},
		GlobalMax: 1024,
	}
}
