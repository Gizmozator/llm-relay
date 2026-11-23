// Package store holds all SQL queries for llm-relay: domain CRUD and usage
// aggregation. Every function takes the shared *sql.DB.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrConflict marks a request that clashes with existing data (name taken,
	// row still referenced). Handlers map it to 409 without string matching.
	ErrConflict = errors.New("conflict")
	ErrExists   = errors.New("already exists")
	// ErrInvalid marks client input-validation failures (blank name, bad
	// shape) — the caller's request was malformed, nothing clashed.
	// Handlers map it to 400.
	ErrInvalid = errors.New("invalid input")
)

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------- users

type User struct {
	ID              int64   `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"display_name"`
	Role            string  `json:"role"`
	Disabled        bool    `json:"disabled"`
	CreatedAt       string  `json:"created_at"`
	PriceMultiplier float64 `json:"price_multiplier"`
	PassChangedAt   string  `json:"pass_changed_at"`
}

const userCols = `id, username, display_name, role, disabled, created_at, price_multiplier,
	COALESCE(pass_changed_at,'')`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var dis int
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &dis, &u.CreatedAt,
		&u.PriceMultiplier, &u.PassChangedAt); err != nil {
		return nil, err
	}
	u.Disabled = dis != 0
	return u, nil
}

func CreateUser(db *sql.DB, username, displayName, passHash, role string) (*User, error) {
	res, err := db.Exec(`INSERT INTO users(username, display_name, pass_hash, role, created_at) VALUES(?,?,?,?,?)`,
		username, displayName, passHash, role, now())
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, DisplayName: displayName, Role: role, CreatedAt: now()}, nil
}

func UserByName(db *sql.DB, username string) (*User, string, error) {
	u := &User{}
	var hash string
	var dis int
	err := db.QueryRow(`SELECT id, username, display_name, role, disabled, created_at, pass_hash FROM users WHERE username=?`, username).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &dis, &u.CreatedAt, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	u.Disabled = dis != 0
	return u, hash, nil
}

func UserByID(db *sql.DB, id int64) (*User, error) {
	u, err := scanUser(db.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// PasswordHashByUserID returns the stored bcrypt hash for password checks.
func PasswordHashByUserID(db *sql.DB, id int64) (string, error) {
	var hash string
	err := db.QueryRow(`SELECT pass_hash FROM users WHERE id=?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return hash, err
}

func ListUsers(db *sql.DB) ([]User, error) {
	rows, err := db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// isUniqueErr reports whether err is a UNIQUE constraint violation. The
// typed driver check is primary; the substring match stays as a fallback so
// wrapped or driver-agnostic errors are still classified.
func isUniqueErr(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case 19, 2067: // SQLITE_CONSTRAINT, SQLITE_CONSTRAINT_UNIQUE
			return true
		}
	}
	return strings.Contains(err.Error(), "UNIQUE")
}

func UpdateUser(db *sql.DB, id int64, passwordHash *string, disabled *bool, role *string, displayName *string, priceMultiplier *float64) (*User, error) {
	// One transaction: a mid-way failure must not leave a half-applied edit.
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if priceMultiplier != nil {
		if _, err := tx.Exec(`UPDATE users SET price_multiplier=? WHERE id=?`, *priceMultiplier, id); err != nil {
			return nil, err
		}
	}
	if displayName != nil {
		if _, err := tx.Exec(`UPDATE users SET display_name=? WHERE id=?`, *displayName, id); err != nil {
			return nil, err
		}
	}
	if passwordHash != nil {
		// Stamping pass_changed_at invalidates JWTs issued earlier (see Auth).
		if _, err := tx.Exec(`UPDATE users SET pass_hash=?, pass_changed_at=? WHERE id=?`, *passwordHash, now(), id); err != nil {
			return nil, err
		}
	}
	if disabled != nil {
		v := 0
		if *disabled {
			v = 1
		}
		if _, err := tx.Exec(`UPDATE users SET disabled=? WHERE id=?`, v, id); err != nil {
			return nil, err
		}
	}
	if role != nil {
		if _, err := tx.Exec(`UPDATE users SET role=? WHERE id=?`, *role, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return UserByID(db, id)
}

func UserCount(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountEnabledAdminsExcept counts enabled admins other than excludeID.
// AdminUpdateUser uses it to refuse removing the last one.
func CountEnabledAdminsExcept(db *sql.DB, excludeID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin' AND disabled=0 AND id<>?`, excludeID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- api keys

type APIKey struct {
	ID         int64   `json:"id"`
	UserID     int64   `json:"-"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	RevokedAt  *string `json:"revoked_at"`
}

func CreateKey(db *sql.DB, userID int64, name, hash, prefix string) (*APIKey, error) {
	res, err := db.Exec(`INSERT INTO api_keys(user_id, name, key_hash, key_prefix, created_at) VALUES(?,?,?,?,?)`,
		userID, name, hash, prefix, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &APIKey{ID: id, UserID: userID, Name: name, Prefix: prefix, CreatedAt: now()}, nil
}

// KeyByHash resolves an active (non-revoked) key together with its owner.
func KeyByHash(db *sql.DB, hash string) (*APIKey, *User, error) {
	k := &APIKey{}
	u := &User{}
	var dis int
	err := db.QueryRow(`SELECT k.id, k.user_id, k.name, k.key_prefix, k.created_at, k.last_used_at, k.revoked_at,
	                       u.id, u.username, u.role, u.disabled, u.price_multiplier
	                    FROM api_keys k JOIN users u ON u.id = k.user_id
	                    WHERE k.key_hash = ?`, hash).
		Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt,
			&u.ID, &u.Username, &u.Role, &dis, &u.PriceMultiplier)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if k.RevokedAt != nil || dis != 0 {
		return nil, nil, ErrNotFound
	}
	u.Disabled = dis != 0
	return k, u, nil
}

func ListKeys(db *sql.DB, userID int64) ([]APIKey, error) {
	rows, err := db.Query(`SELECT id, user_id, name, key_prefix, created_at, last_used_at, revoked_at
		FROM api_keys WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func RevokeKey(db *sql.DB, id, userID int64) error {
	res, err := db.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`, now(), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func TouchKey(db *sql.DB, id int64, ts string) error {
	_, err := db.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, ts, id)
	return err
}

// ---------------------------------------------------------------- backends

type Backend struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	BaseURL        string  `json:"base_url"`
	HasKey         bool    `json:"has_key"`
	Enabled        bool    `json:"enabled"`
	Draining       bool    `json:"draining"`
	LastProbeAt    *string `json:"last_probe_at"`
	LastProbeError *string `json:"last_probe_error"`
	ServerVersion  *string `json:"server_version"`
	CreatedAt      string  `json:"created_at"`
	ModelCount     int     `json:"model_count"`
	APIKey         string  `json:"-"`
}

const backendCols = `id, name, base_url, COALESCE(api_key,''), enabled, draining, last_probe_at, last_probe_error, server_version, created_at`

func scanBackend(row interface{ Scan(...any) error }) (*Backend, error) {
	b := &Backend{}
	targets, finalize := backendScanTargets(b)
	if err := row.Scan(targets...); err != nil {
		return nil, err
	}
	finalize()
	return b, nil
}

// backendScanTargets is the single source of truth for the backendCols /
// backendJoinCols scan order: scanBackend and modelBackendScanTargets share
// it, so a reorder or same-width swap can never mis-bind one scan and not
// the other. finalize applies the int→bool conversions after a successful
// Scan.
func backendScanTargets(b *Backend) (targets []any, finalize func()) {
	var key string
	var en, dr int
	targets = []any{
		&b.ID, &b.Name, &b.BaseURL, &key, &en, &dr, &b.LastProbeAt, &b.LastProbeError,
		&b.ServerVersion, &b.CreatedAt,
	}
	finalize = func() {
		b.HasKey = key != ""
		b.APIKey = key
		b.Enabled = en != 0
		b.Draining = dr != 0
	}
	return targets, finalize
}

func CreateBackend(db *sql.DB, name, baseURL, apiKey string) (*Backend, error) {
	res, err := db.Exec(`INSERT INTO backends(name, base_url, api_key, created_at) VALUES(?,?,?,?)`,
		name, strings.TrimRight(baseURL, "/"), nullable(apiKey), now())
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return BackendByID(db, id)
}

func BackendByID(db *sql.DB, id int64) (*Backend, error) {
	b, err := scanBackend(db.QueryRow(`SELECT `+backendCols+` FROM backends WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err == nil {
		b.ModelCount, err = BackendModelCount(db, id)
	}
	return b, err
}

func ListBackends(db *sql.DB) ([]Backend, error) {
	rows, err := db.Query(`SELECT ` + backendCols + `,
		(SELECT COUNT(*) FROM models m WHERE m.backend_id = backends.id)
		FROM backends ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Backend{}
	for rows.Next() {
		b := &Backend{}
		targets, finalize := backendScanTargets(b)
		// backendCols plus the trailing ModelCount subquery.
		if err := rows.Scan(append(targets, &b.ModelCount)...); err != nil {
			return nil, err
		}
		finalize()
		out = append(out, *b)
	}
	return out, rows.Err()
}

func UpdateBackend(db *sql.DB, id int64, name, baseURL *string, apiKey *string, enabled, draining *bool) (*Backend, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if name != nil {
		if _, err := tx.Exec(`UPDATE backends SET name=? WHERE id=?`, *name, id); err != nil {
			if isUniqueErr(err) {
				return nil, fmt.Errorf("%w: backend name taken", ErrConflict)
			}
			return nil, err
		}
	}
	if baseURL != nil {
		if _, err := tx.Exec(`UPDATE backends SET base_url=? WHERE id=?`, strings.TrimRight(*baseURL, "/"), id); err != nil {
			return nil, err
		}
	}
	if apiKey != nil {
		if _, err := tx.Exec(`UPDATE backends SET api_key=? WHERE id=?`, nullable(*apiKey), id); err != nil {
			return nil, err
		}
	}
	if enabled != nil {
		v := 0
		if *enabled {
			v = 1
		}
		if _, err := tx.Exec(`UPDATE backends SET enabled=? WHERE id=?`, v, id); err != nil {
			return nil, err
		}
	}
	if draining != nil {
		v := 0
		if *draining {
			v = 1
		}
		if _, err := tx.Exec(`UPDATE backends SET draining=? WHERE id=?`, v, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return BackendByID(db, id)
}

func DeleteBackend(db *sql.DB, id int64) error {
	n, err := BackendModelCount(db, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: backend still has catalog models; remove them first", ErrConflict)
	}
	res, err := db.Exec(`DELETE FROM backends WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func BackendModelCount(db *sql.DB, id int64) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM models WHERE backend_id=?`, id).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func RecordProbe(db *sql.DB, id int64, probeErr, version *string) error {
	_, err := db.Exec(`UPDATE backends SET last_probe_at=?, last_probe_error=?,
		server_version=COALESCE(?, server_version) WHERE id=?`,
		now(), probeErr, version, id)
	return err
}

// ---------------------------------------------------------------- models

type Model struct {
	ID               int64   `json:"id"`
	BackendID        int64   `json:"backend_id"`
	BackendName      string  `json:"backend_name"`
	Name             string  `json:"name"`
	UpstreamModel    string  `json:"upstream_model"`
	Visibility       string  `json:"visibility"`
	Enabled          bool    `json:"enabled"`
	PriceIn          int64   `json:"price_in"`
	PriceOut         int64   `json:"price_out"`
	PriceCacheRead   int64   `json:"price_cache_read"`
	PriceCacheWrite  int64   `json:"price_cache_write"`
	ContextLength    int64   `json:"context_length"`
	ConcurrencyLimit int64   `json:"concurrency_limit"`
	BusyThresholdPct int64   `json:"busy_threshold_pct"`
	BenchTokPerS     float64 `json:"bench_tok_per_s"`
	BenchPPTokPerS   float64 `json:"bench_pp_tok_per_s"`
	BenchAt          string  `json:"bench_at"`
}

const modelCols = `m.id, m.backend_id, b.name, m.name, m.upstream_model, m.visibility, m.enabled,
	m.price_in, m.price_out, m.price_cache_read, m.price_cache_write,
	m.context_length, m.concurrency_limit, m.busy_threshold_pct, m.bench_tok_per_s,
	m.bench_pp_tok_per_s, COALESCE(m.bench_at,'')`

// backendJoinCols is backendCols qualified for joins where backends is b.
const backendJoinCols = `b.id, b.name, b.base_url, COALESCE(b.api_key,''), b.enabled, b.draining,
	b.last_probe_at, b.last_probe_error, b.server_version, b.created_at`

func scanModel(row interface{ Scan(...any) error }) (*Model, error) {
	m := &Model{}
	targets, finalize := modelScanTargets(m)
	if err := row.Scan(targets...); err != nil {
		return nil, err
	}
	finalize()
	return m, nil
}

// modelScanTargets is the single source of truth for the modelCols scan
// order: scanModel and modelBackendScanTargets share it, so a reorder or
// same-width swap inside modelCols can never mis-bind one scan and not the
// other. finalize applies the int→bool conversions after a successful Scan.
func modelScanTargets(m *Model) (targets []any, finalize func()) {
	var en int
	targets = []any{
		&m.ID, &m.BackendID, &m.BackendName, &m.Name, &m.UpstreamModel, &m.Visibility, &en,
		&m.PriceIn, &m.PriceOut, &m.PriceCacheRead, &m.PriceCacheWrite,
		&m.ContextLength, &m.ConcurrencyLimit, &m.BusyThresholdPct, &m.BenchTokPerS,
		&m.BenchPPTokPerS, &m.BenchAt,
	}
	finalize = func() { m.Enabled = en != 0 }
	return targets, finalize
}

// modelBackendScanTargets returns the scan targets for modelCols +
// backendJoinCols in exact projection order — shared by scanModelAndBackend
// and scanGroupMember (which prepends its own membership targets), so no
// scan hand-copies the projection.
func modelBackendScanTargets(m *Model, b *Backend) (targets []any, finalize func()) {
	mt, mfin := modelScanTargets(m)
	bt, bfin := backendScanTargets(b)
	targets = append(mt, bt...)
	finalize = func() {
		mfin()
		bfin()
	}
	return targets, finalize
}

// scanModelAndBackend reads modelCols + backendJoinCols from one row: the
// joined shape group members and name resolution share. Backend.ModelCount
// stays zero — the join does not count.
func scanModelAndBackend(row interface{ Scan(...any) error }) (*Model, *Backend, error) {
	m := &Model{}
	b := &Backend{}
	targets, finalize := modelBackendScanTargets(m, b)
	if err := row.Scan(targets...); err != nil {
		return nil, nil, err
	}
	finalize()
	return m, b, nil
}

func CreateModel(db *sql.DB, backendID int64, upstreamModel, name string) (*Model, error) {
	if name == "" {
		name = upstreamModel
	}
	// Guard and insert in one tx (see CreateGroup): the group-name check and
	// the write must not interleave with a concurrent CreateGroup.
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := assertNoGroupName(tx, name); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO models(backend_id, upstream_model, name, busy_threshold_pct) VALUES(?,?,?,80)`,
		backendID, upstreamModel, name)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return ModelByID(db, id)
}

func ModelByID(db *sql.DB, id int64) (*Model, error) {
	m, err := scanModel(db.QueryRow(`SELECT `+modelCols+` FROM models m JOIN backends b ON b.id=m.backend_id WHERE m.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// ModelByName resolves an enabled catalog model together with its enabled
// backend. Names are unique per backend since 0010, so several rows can share
// one name: public visibility wins, then the lowest id. Internal members stay
// directly resolvable (pin/debug). Group-aware resolution is ResolveTarget in
// groups.go — this function is the model-only path.
func ModelByName(db *sql.DB, name string) (*Model, *Backend, error) {
	m, b, err := scanModelAndBackend(db.QueryRow(`SELECT `+modelCols+`, `+backendJoinCols+`
		FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.name=? AND m.enabled=1 AND b.enabled=1
		ORDER BY CASE WHEN m.visibility='public' THEN 0 ELSE 1 END, m.id
		LIMIT 1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return m, b, err
}

func ListModels(db *sql.DB) ([]Model, error) {
	rows, err := db.Query(`SELECT ` + modelCols + `
		FROM models m JOIN backends b ON b.id=m.backend_id ORDER BY m.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ListPublicModels returns only models visible and usable right now: enabled,
// visibility=public, on an enabled backend — the set the relay lists and
// serves directly. Internal members are hidden but stay requestable by name.
// Ordered by name then id so a shared name appears in ModelByName's
// tie-break order (ListPublicCatalog keeps the first, i.e. lowest id).
// For the /v1/models listing that also shows groups use ListPublicCatalog.
func ListPublicModels(db *sql.DB) ([]Model, error) {
	rows, err := db.Query(`SELECT ` + modelCols + `
		FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.enabled=1 AND b.enabled=1 AND m.visibility='public' ORDER BY m.name, m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Model{}
	seen := map[string]bool{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		// Since 0010 two backends may carry the same name; the public
		// catalog is keyed by name (both callers emit one entry per row),
		// so keep the first row per name — ordered by (name, id), so the
		// lowest id wins, matching ModelByName's tie-break.
		if seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		out = append(out, *m)
	}
	return out, rows.Err()
}

// UpdateModelMeta sets the admin-managed model meta fields (nil = leave).
// A manually set bench speed clears bench_at: the stored value is then a
// configured speed, not a measured benchmark result.
func UpdateModelMeta(db *sql.DB, id int64, contextLength, concurrencyLimit, busyThresholdPct *int64, benchTokPerS, benchPPTokPerS *float64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if contextLength != nil {
		if _, err := tx.Exec(`UPDATE models SET context_length=? WHERE id=?`, *contextLength, id); err != nil {
			return err
		}
	}
	if concurrencyLimit != nil {
		if _, err := tx.Exec(`UPDATE models SET concurrency_limit=? WHERE id=?`, *concurrencyLimit, id); err != nil {
			return err
		}
	}
	if busyThresholdPct != nil {
		if _, err := tx.Exec(`UPDATE models SET busy_threshold_pct=? WHERE id=?`, *busyThresholdPct, id); err != nil {
			return err
		}
	}
	if benchTokPerS != nil {
		if _, err := tx.Exec(`UPDATE models SET bench_tok_per_s=?, bench_at=NULL WHERE id=?`, *benchTokPerS, id); err != nil {
			return err
		}
	}
	if benchPPTokPerS != nil {
		if _, err := tx.Exec(`UPDATE models SET bench_pp_tok_per_s=?, bench_at=NULL WHERE id=?`, *benchPPTokPerS, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateModelBench stores the latest benchmark result for a model. A nil PP
// speed (the prefill pass failed) keeps the previously stored value: one
// flaky pass must not wipe a good measurement.
func UpdateModelBench(db *sql.DB, id int64, tokPerS float64, ppTokPerS *float64, at string) error {
	if ppTokPerS == nil {
		_, err := db.Exec(`UPDATE models SET bench_tok_per_s=?, bench_at=? WHERE id=?`, tokPerS, at, id)
		return err
	}
	_, err := db.Exec(`UPDATE models SET bench_tok_per_s=?, bench_pp_tok_per_s=?, bench_at=? WHERE id=?`,
		tokPerS, *ppTokPerS, at, id)
	return err
}

// AddModels bulk-inserts discovered upstream ids for one backend in a single
// transaction. A name already taken on this backend is skipped and reported,
// never silently swallowed. The same name on a different backend is a normal
// insert since 0010 (UNIQUE(backend_id, name)): two machines may each serve
// qwen3-coder; grouping them is a later, human decision. A name owned by a
// group is deliberately NOT skipped: a freshly added replica syncing its
// catalog is exactly the row meant to join that group, so the row is created
// and sits shadowed by the group (resolution and the public catalog prefer
// the group) until an admin claims it as a member.
func AddModels(db *sql.DB, backendID int64, upstreamIDs []string) ([]Model, []string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var ids []int64
	skipped := []string{}
	for _, mid := range upstreamIDs {
		// busy_threshold_pct=80 explicitly: the column default is 0
		// (SQLite cannot ALTER defaults), and synced models must show the
		// real default like everything else.
		res, err := tx.Exec(`INSERT INTO models(backend_id, upstream_model, name, busy_threshold_pct) VALUES(?,?,?,80)`,
			backendID, mid, mid)
		if err != nil {
			if isUniqueErr(err) {
				skipped = append(skipped, mid)
				continue
			}
			return nil, nil, err
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	created := make([]Model, 0, len(ids))
	for _, id := range ids {
		m, err := ModelByID(db, id)
		if err != nil {
			return created, skipped, err
		}
		created = append(created, *m)
	}
	return created, skipped, nil
}

func ModelIDsForBackend(db *sql.DB, backendID int64) (map[string]int64, error) {
	rows, err := db.Query(`SELECT upstream_model, id FROM models WHERE backend_id=?`, backendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var mid int64
		if err := rows.Scan(&id, &mid); err != nil {
			return nil, err
		}
		out[id] = mid
	}
	return out, rows.Err()
}

func UpdateModel(db *sql.DB, id int64, name *string, enabled *bool, prices [4]*int64) (*Model, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if name != nil {
		// No-op rename (the UI PUT echoes the current name) must not trip
		// the group guard: a member model legitimately carries the group's
		// name, and rejecting its own name back to the caller would make
		// such rows uneditable.
		var cur string
		if err := tx.QueryRow(`SELECT name FROM models WHERE id=?`, id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if *name != cur {
			if err := assertNoGroupName(tx, *name); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE models SET name=? WHERE id=?`, *name, id); err != nil {
				if isUniqueErr(err) {
					return nil, fmt.Errorf("%w: model name taken", ErrConflict)
				}
				return nil, err
			}
		}
	}
	if enabled != nil {
		v := 0
		if *enabled {
			v = 1
		}
		if _, err := tx.Exec(`UPDATE models SET enabled=? WHERE id=?`, v, id); err != nil {
			return nil, err
		}
	}
	cols := []string{"price_in", "price_out", "price_cache_read", "price_cache_write"}
	for i, p := range prices {
		if p != nil {
			q := fmt.Sprintf(`UPDATE models SET %s=? WHERE id=?`, cols[i])
			if _, err := tx.Exec(q, *p, id); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ModelByID(db, id)
}

// UpdateModelVisibility is the manual visibility editor (admin API): it
// applies 'public' or 'internal' verbatim, ErrInvalid for anything else and
// ErrNotFound for an unknown id. Membership still overrides it: joining a
// group forces internal and leaving restores public (UpdateGroup), so a
// hand-set value on a member survives only until the next membership edit.
// Re-exposing a serving group's member as public is allowed on purpose —
// ResolveTarget keeps the group in front while it can serve.
func UpdateModelVisibility(db *sql.DB, id int64, visibility string) (*Model, error) {
	if visibility != "public" && visibility != "internal" {
		return nil, fmt.Errorf("%w: visibility must be public or internal", ErrInvalid)
	}
	res, err := db.Exec(`UPDATE models SET visibility=? WHERE id=?`, visibility, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return ModelByID(db, id)
}

func DeleteModel(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Membership rows first: model_group_members references models(id) and
	// the FK has no cascade — without this the delete aborts with an opaque
	// constraint error for grouped models.
	if _, err := tx.Exec(`DELETE FROM model_group_members WHERE model_id=?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM models WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- settings

func GetSetting(db *sql.DB, key string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func SetSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---------------------------------------------------------------- helpers

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
