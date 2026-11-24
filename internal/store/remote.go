// Remote-backend store layer (plan 2026-11-24-remote-backend-v1): backend
// rows a remote box connects to over /remote/v1/connect. A remote backend is
// an ordinary backends row whose base_url names the synthetic remote://<id>
// location instead of a network address, plus a hashed connect token
// (remote_token_hash, migration 0015). Unlike the federation synthetic
// backends these are NOT materialized: the admin creates them by hand, their
// model rows are local catalog rows (shareable, group-eligible, priced
// here), and only the transport is a tunnel.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"llm-relay/internal/auth"
)

// RemoteScheme is the base_url scheme of a remote backend.
const RemoteScheme = "remote"

// RemoteBaseURL is the synthetic base_url for one remote backend id.
func RemoteBaseURL(id int64) string {
	return RemoteScheme + "://" + strconv.FormatInt(id, 10)
}

// RemoteBackendID parses a remote:// base_url back to its backend id.
// ok=false for every http(s) and federation URL.
func RemoteBackendID(baseURL string) (id int64, ok bool) {
	rest, found := strings.CutPrefix(baseURL, RemoteScheme+"://")
	if !found {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// CreateRemoteBackend creates a remote backend and returns its connect token
// — the only time the raw token exists anywhere but the caller's hands. The
// insert and the base_url back-fill (which needs the row id) run in one tx,
// so a half-written row (remote backend without its remote:// key) is never
// observable.
func CreateRemoteBackend(db *sql.DB, name string) (*Backend, string, error) {
	plain, hash, err := auth.GenerateRemoteToken()
	if err != nil {
		return nil, "", err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO backends(name, base_url, remote_token_hash, created_at)
		VALUES(?, '', ?, ?)`, name, hash, now())
	if err != nil {
		if isUniqueErr(err) {
			return nil, "", ErrExists
		}
		return nil, "", err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`UPDATE backends SET base_url=? WHERE id=?`, RemoteBaseURL(id), id); err != nil {
		return nil, "", err
	}
	b, err := scanBackend(tx.QueryRow(`SELECT `+backendCols+` FROM backends WHERE id=?`, id))
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	b.ModelCount = 0
	return b, plain, nil
}

// RotateRemoteToken replaces a remote backend's connect token and returns
// the new raw token (shown once). ErrInvalid for a non-remote backend.
func RotateRemoteToken(db *sql.DB, id int64) (string, error) {
	b, err := BackendByID(db, id)
	if err != nil {
		return "", err
	}
	if _, ok := RemoteBackendID(b.BaseURL); !ok {
		return "", fmt.Errorf("%w: backend %q is not a remote backend", ErrInvalid, b.Name)
	}
	plain, hash, err := auth.GenerateRemoteToken()
	if err != nil {
		return "", err
	}
	res, err := db.Exec(`UPDATE backends SET remote_token_hash=? WHERE id=?`, hash, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return plain, nil
}

// BackendByRemoteTokenHash resolves a connect token for auth. The enabled
// check lives in the middleware so it can answer "backend disabled"
// distinctly from "unknown token", exactly like PeerAuth does for peers.
func BackendByRemoteTokenHash(db *sql.DB, hash string) (*Backend, error) {
	if hash == "" {
		return nil, ErrNotFound
	}
	b, err := scanBackend(db.QueryRow(`SELECT `+backendCols+` FROM backends
		WHERE remote_token_hash=?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// RemoteTokenLive revalidates a live connector's credential (the throttled
// Touch backstop on the channel): the row still exists, is enabled, and
// still carries the token hash the connect authenticated with. Catches
// rotate/disable/delete revocations that raced past the kick endpoints —
// same role TouchPeer revalidation plays for federation peers.
func RemoteTokenLive(db *sql.DB, id int64, hash string) bool {
	var cur string
	var enabled bool
	err := db.QueryRow(`SELECT COALESCE(remote_token_hash,''), enabled FROM backends WHERE id=?`, id).
		Scan(&cur, &enabled)
	return err == nil && enabled && cur != "" && cur == hash
}
