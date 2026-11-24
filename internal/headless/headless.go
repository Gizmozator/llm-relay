// Package headless boots a relay fully configured by env — the "headless
// spoke" (plan 2026-11-24-headless-spoke): one backend, exactly one model,
// one hub, zero dashboard interaction. Bootstrap runs once at startup, before
// the federation manager reads its settings, and writes exactly three things
// through the existing store paths: the backend row, the model row and the
// federation settings block. The env is the source of truth — a restart
// re-applies it; changing anything means editing the env and restarting.
//
// A headless box is still a full relay: the dashboard stays served and
// usable, the dialer and catalog push are the ordinary federation code.
// Nothing here duplicates their logic.
package headless

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"llm-relay/internal/config"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// BackendName is the fixed name of the env-defined backend. backends.name is
// UNIQUE, which makes the backend upsert idempotent across restarts.
const BackendName = "headless"

// Config is the headless slice of the runtime config (see config.Config).
type Config struct {
	BackendURL      string
	BackendToken    string
	ModelName       string
	HubURL          string
	HubToken        string
	PriceIn         int64
	PriceOut        int64
	PriceCacheRead  int64
	PriceCacheWrite int64
}

// FromConfig projects the headless fields out of the parsed environment.
func FromConfig(c *config.Config) Config {
	return Config{
		BackendURL:      c.BackendURL,
		BackendToken:    c.BackendToken,
		ModelName:       c.ModelName,
		HubURL:          c.HubURL,
		HubToken:        c.HubToken,
		PriceIn:         c.PriceIn,
		PriceOut:        c.PriceOut,
		PriceCacheRead:  c.PriceCacheRead,
		PriceCacheWrite: c.PriceCacheWrite,
	}
}

// Bootstrap makes the stored configuration match the env: upsert the
// "headless" backend, upsert its single model row (enabled, shared with
// everyone, priced from env), then persist the spoke federation settings so
// the manager's ordinary Reload starts the dialer. Every write goes through
// the same store functions the admin API uses — validation included.
//
// A name collision with a row on ANOTHER backend is fatal: the single-row
// design has nothing else to do, so continuing with an ambiguous catalog
// would only hide the mistake.
func Bootstrap(db *sql.DB, c Config, logf func(string, ...any)) error {
	b, err := upsertBackend(db, c)
	if err != nil {
		return err
	}
	if err := upsertModel(db, b.ID, c); err != nil {
		return err
	}
	// PutFederationSettings owns all spoke validation (http(s) hub_url,
	// non-empty token) — do not re-implement it here.
	if err := store.PutFederationSettings(db, store.FederationSettings{
		Role: store.FedRoleSpoke, HubURL: c.HubURL, HubToken: c.HubToken,
	}); err != nil {
		return fmt.Errorf("headless: federation settings: %w", err)
	}
	warnIfNotServed(c, logf)
	logf("headless: backend %q → model %q → hub %s", BackendName, c.ModelName, c.HubURL)
	return nil
}

// upsertBackend creates the env backend on first boot and re-applies the env
// on every later one (base_url, api_key, enabled).
func upsertBackend(db *sql.DB, c Config) (*store.Backend, error) {
	b, err := store.BackendByName(db, BackendName)
	if errors.Is(err, store.ErrNotFound) {
		b, err = store.CreateBackend(db, BackendName, c.BackendURL, c.BackendToken)
		if err != nil {
			return nil, fmt.Errorf("headless: create backend: %w", err)
		}
		return b, nil
	}
	if err != nil {
		return nil, fmt.Errorf("headless: find backend: %w", err)
	}
	enabled := true
	b, err = store.UpdateBackend(db, b.ID, nil, &c.BackendURL, &c.BackendToken, &enabled, nil)
	if err != nil {
		return nil, fmt.Errorf("headless: update backend: %w", err)
	}
	return b, nil
}

// upsertModel writes the single model row: UNIQUE(backend_id, upstream_model)
// is the upsert key; every field comes from env, so a restart re-applies the
// truth (an admin edit survives only until the next boot — that is the
// headless contract).
func upsertModel(db *sql.DB, backendID int64, c Config) error {
	taken, err := store.ModelNameTakenOnOtherBackend(db, backendID, c.ModelName)
	if err != nil {
		return fmt.Errorf("headless: model name check: %w", err)
	}
	if taken {
		return fmt.Errorf("headless: model name %q already belongs to another backend; "+
			"rename that row or change MODEL_NAME", c.ModelName)
	}
	ids, err := store.ModelIDsForBackend(db, backendID)
	if err != nil {
		return fmt.Errorf("headless: list backend models: %w", err)
	}
	id, ok := ids[c.ModelName]
	if !ok {
		m, err := store.CreateModel(db, backendID, c.ModelName, c.ModelName)
		if err != nil {
			return fmt.Errorf("headless: create model: %w", err)
		}
		id = m.ID
	}
	enabled := true
	if _, err := store.UpdateModelPatch(db, id, store.ModelPatch{
		Enabled:       &enabled,
		Prices:        [4]*int64{&c.PriceIn, &c.PriceOut, &c.PriceCacheRead, &c.PriceCacheWrite},
		FedVisibility: store.FedVisAll,
	}); err != nil {
		return fmt.Errorf("headless: apply model settings: %w", err)
	}
	return nil
}

// warnIfNotServed is the boot-time typo catcher: if the backend answers
// /v1/models and MODEL_NAME is not in the list, say so loudly. Non-fatal —
// the backend may start after the relay, and the health probe loop keeps
// tracking it either way.
func warnIfNotServed(c Config, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := proxy.Probe(ctx, c.BackendURL, c.BackendToken)
	if !res.OK {
		logf("headless: backend not reachable at boot (%s); model row kept, health probe will track it",
			res.Error)
		return
	}
	if !strings.Contains(" "+strings.Join(res.Models, " ")+" ", " "+c.ModelName+" ") {
		logf("headless: WARNING: backend does not serve MODEL_NAME=%q (it lists: %s); "+
			"check the id or restart after a hot-swap", c.ModelName, strings.Join(res.Models, ", "))
	}
}
