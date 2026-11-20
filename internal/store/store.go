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
	LastProbeAt    *string `json:"last_probe_at"`
	LastProbeError *string `json:"last_probe_error"`
	ServerVersion  *string `json:"server_version"`
	CreatedAt      string  `json:"created_at"`
	ModelCount     int     `json:"model_count"`
	APIKey         string  `json:"-"`
}

const backendCols = `id, name, base_url, COALESCE(api_key,''), enabled, last_probe_at, last_probe_error, server_version, created_at`

func scanBackend(row interface{ Scan(...any) error }) (*Backend, error) {
	b := &Backend{}
	var key string
	var en int
	if err := row.Scan(&b.ID, &b.Name, &b.BaseURL, &key, &en, &b.LastProbeAt, &b.LastProbeError, &b.ServerVersion, &b.CreatedAt); err != nil {
		return nil, err
	}
	b.HasKey = key != ""
	b.APIKey = key
	b.Enabled = en != 0
	return b, nil
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
		var key string
		var en int
		if err := rows.Scan(&b.ID, &b.Name, &b.BaseURL, &key, &en, &b.LastProbeAt, &b.LastProbeError,
			&b.ServerVersion, &b.CreatedAt, &b.ModelCount); err != nil {
			return nil, err
		}
		b.HasKey = key != ""
		b.APIKey = key
		b.Enabled = en != 0
		out = append(out, *b)
	}
	return out, rows.Err()
}

func UpdateBackend(db *sql.DB, id int64, name, baseURL *string, apiKey *string, enabled *bool) (*Backend, error) {
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
	Enabled          bool    `json:"enabled"`
	PriceIn          int64   `json:"price_in"`
	PriceOut         int64   `json:"price_out"`
	PriceCacheRead   int64   `json:"price_cache_read"`
	PriceCacheWrite  int64   `json:"price_cache_write"`
	ContextLength    int64   `json:"context_length"`
	ConcurrencyLimit int64   `json:"concurrency_limit"`
	BenchTokPerS     float64 `json:"bench_tok_per_s"`
	BenchAt          string  `json:"bench_at"`
}

const modelCols = `m.id, m.backend_id, b.name, m.name, m.upstream_model, m.enabled,
	m.price_in, m.price_out, m.price_cache_read, m.price_cache_write,
	m.context_length, m.concurrency_limit, m.bench_tok_per_s,
	COALESCE(m.bench_at,'')`

func scanModel(row interface{ Scan(...any) error }) (*Model, error) {
	m := &Model{}
	var en int
	if err := row.Scan(&m.ID, &m.BackendID, &m.BackendName, &m.Name, &m.UpstreamModel, &en,
		&m.PriceIn, &m.PriceOut, &m.PriceCacheRead, &m.PriceCacheWrite,
		&m.ContextLength, &m.ConcurrencyLimit, &m.BenchTokPerS, &m.BenchAt); err != nil {
		return nil, err
	}
	m.Enabled = en != 0
	return m, nil
}

func CreateModel(db *sql.DB, backendID int64, upstreamModel, name string) (*Model, error) {
	if name == "" {
		name = upstreamModel
	}
	res, err := db.Exec(`INSERT INTO models(backend_id, upstream_model, name) VALUES(?,?,?)`,
		backendID, upstreamModel, name)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrExists
		}
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

// ModelByName resolves an enabled catalog model together with its enabled backend.
func ModelByName(db *sql.DB, name string) (*Model, *Backend, error) {
	m, err := scanModel(db.QueryRow(`SELECT `+modelCols+`
		FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.name=? AND m.enabled=1 AND b.enabled=1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	b, err := BackendByID(db, m.BackendID)
	if err != nil {
		return nil, nil, err
	}
	return m, b, nil
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

// ListPublicModels returns only models usable right now: enabled model on an
// enabled backend — the set the relay will actually serve.
func ListPublicModels(db *sql.DB) ([]Model, error) {
	rows, err := db.Query(`SELECT ` + modelCols + `
		FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.enabled=1 AND b.enabled=1 ORDER BY m.name`)
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

// UpdateModelMeta sets the admin-managed model meta fields (nil = leave).
// A manually set bench_tok_per_s clears bench_at: the stored value is then a
// configured speed, not a measured benchmark result.
func UpdateModelMeta(db *sql.DB, id int64, contextLength, concurrencyLimit *int64, benchTokPerS *float64) error {
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
	if benchTokPerS != nil {
		if _, err := tx.Exec(`UPDATE models SET bench_tok_per_s=?, bench_at=NULL WHERE id=?`, *benchTokPerS, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateModelBench stores the latest benchmark result for a model.
func UpdateModelBench(db *sql.DB, id int64, tokPerS float64, at string) error {
	_, err := db.Exec(`UPDATE models SET bench_tok_per_s=?, bench_at=? WHERE id=?`, tokPerS, at, id)
	return err
}

// AddModels bulk-inserts discovered upstream ids for one backend in a single
// transaction. A name already taken anywhere in the catalog (names are
// globally unique) is skipped and reported, never silently swallowed.
func AddModels(db *sql.DB, backendID int64, upstreamIDs []string) ([]Model, []string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var ids []int64
	skipped := []string{}
	for _, mid := range upstreamIDs {
		res, err := tx.Exec(`INSERT INTO models(backend_id, upstream_model, name) VALUES(?,?,?)`,
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
		if _, err := tx.Exec(`UPDATE models SET name=? WHERE id=?`, *name, id); err != nil {
			if isUniqueErr(err) {
				return nil, fmt.Errorf("%w: model name taken", ErrConflict)
			}
			return nil, err
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

func DeleteModel(db *sql.DB, id int64) error {
	res, err := db.Exec(`DELETE FROM models WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
