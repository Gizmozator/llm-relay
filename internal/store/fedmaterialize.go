// Materialization store layer (plan 2026-11-22-relay-federation, task 3):
// the synthetic backends a peer's advertised models hang off, the upsert /
// prune helpers for those rows, the federated-group helpers, and the
// catalog queries both sides push from. The fed package owns WHEN these run
// (frame arrival, connect/die, peer delete); this file owns the SQL.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Synthetic backend schemes. A peer's advertised models are ordinary model
// rows on a backend whose base_url names the federation location instead of
// a network address: peer://<peer-id> on the hub (and on spokes, for other
// spokes' models relayed through the hub), hub://federation on a spoke for
// the hub's own catalog. The proxy's scheme-dispatching transport turns
// requests against these into tunneled frames; nothing else may dial them.
const (
	FedSchemePeer = "peer"
	FedSchemeHub  = "hub"

	// FedHubBaseURL is the spoke-side synthetic backend for the hub's
	// catalog. The host part ("federation") is a placeholder that keeps
	// url.Parse producing a clean Host+Path split; the scheme is what the
	// transport dispatches on.
	FedHubBaseURL = "hub://federation"
)

// SyntheticBaseURL parses a synthetic base_url back into its scheme and peer
// id (hub:// carries no id → 0). ok=false for every http(s) backend.
func SyntheticBaseURL(baseURL string) (kind string, peerID int64, ok bool) {
	if baseURL == FedHubBaseURL || baseURL == "hub://" {
		return FedSchemeHub, 0, true
	}
	if rest, found := strings.CutPrefix(baseURL, "peer://"); found {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err == nil && n > 0 {
			return FedSchemePeer, n, true
		}
	}
	return "", 0, false
}

// PeerBackendURL is the synthetic base_url for one peer id.
func PeerBackendURL(peerID int64) string {
	return "peer://" + strconv.FormatInt(peerID, 10)
}

// syntheticWhere matches synthetic backends in SQL (mirrors
// SyntheticBaseURL: keep the two in sync). Two variants: bare for statements
// that address `backends` directly (UPDATE/DELETE/subselect), b.-qualified
// for the model-join queries below.
const syntheticWhere = `base_url LIKE 'peer://%' OR base_url IN ('hub://federation','hub://')`

const syntheticWhereB = `b.base_url LIKE 'peer://%' OR b.base_url IN ('hub://federation','hub://')`

// ------------------------------------------------------------- backends

// EnsureSyntheticBackend finds the backend with this exact base_url or
// creates it. backends.name is UNIQUE but two peers may share a display
// name (that is the grouping use case), so a name collision falls back to
// "name (base_url)" — base_url is unique per synthetic backend by
// construction.
func EnsureSyntheticBackend(db *sql.DB, name, baseURL string) (*Backend, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM backends WHERE base_url=?`, baseURL).Scan(&id)
	if err == nil {
		return BackendByID(db, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	b, err := CreateBackend(db, name, baseURL, "")
	if errors.Is(err, ErrExists) {
		b, err = CreateBackend(db, fmt.Sprintf("%s (%s)", name, baseURL), baseURL, "")
	}
	return b, err
}

// BackendByBaseURL resolves a backend by its exact base_url (synthetic
// backends are keyed this way).
func BackendByBaseURL(db *sql.DB, baseURL string) (*Backend, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM backends WHERE base_url=?`, baseURL).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return BackendByID(db, id)
}

// SetBackendEnabled flips a backend's enabled flag (materialization: peer
// offline disables the synthetic backend — its model rows stay so model ids
// survive the reconnect; peer deleted removes them via DeleteBackendCascade).
func SetBackendEnabled(db *sql.DB, id int64, enabled bool) error {
	res, err := db.Exec(`UPDATE backends SET enabled=? WHERE id=?`, b2i(enabled), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSyntheticBackendsEnabled enables/disables every synthetic backend in
// one statement (spoke channel died: everything materialized is unreachable).
func SetSyntheticBackendsEnabled(db *sql.DB, enabled bool) error {
	_, err := db.Exec(`UPDATE backends SET enabled=? WHERE `+syntheticWhere, b2i(enabled))
	return err
}

// SetPeerBackendEnabled enables/disables one peer's synthetic backend on
// either side (peer://<id>), no-op when the backend does not exist yet.
func SetPeerBackendEnabled(db *sql.DB, peerID int64, enabled bool) error {
	_, err := db.Exec(`UPDATE backends SET enabled=? WHERE base_url=?`,
		b2i(enabled), PeerBackendURL(peerID))
	return err
}

// DeleteBackendCascade removes a backend with every model row hanging off
// it (group memberships first: model_group_members has no cascade). The
// plain DeleteBackend refuses a backend that still has models — synthetic
// backends are always torn down wholesale, so this is their path.
func DeleteBackendCascade(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM model_group_members
		WHERE model_id IN (SELECT id FROM models WHERE backend_id=?)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM models WHERE backend_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fed_allowlist WHERE model_id IN
		(SELECT id FROM models WHERE backend_id=?)`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM backends WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// PurgeSynthetic clears every materialized artifact (synthetic backends with
// their model rows, federated groups): the startup / role-transition sweep.
// Everything materialized is rebuilt from live catalog frames, so anything
// in the DB before a fresh start is stale by definition.
func PurgeSynthetic(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Members of federated groups return to public (mirror of
	// DeleteFederatedGroupsNotIn): a spoke's local model absorbed by a
	// federated group was flipped internal — leaving the federation must
	// undo that, or the model stays invisible forever.
	if _, err := tx.Exec(`UPDATE models SET visibility='public' WHERE id IN
		(SELECT model_id FROM model_group_members WHERE group_id IN
			(SELECT id FROM model_groups WHERE federated=1))`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_group_members
		WHERE group_id IN (SELECT id FROM model_groups WHERE federated=1)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_groups WHERE federated=1`); err != nil {
		return err
	}
	mods := `SELECT id FROM models WHERE backend_id IN
		(SELECT id FROM backends WHERE ` + syntheticWhere + `)`
	// Clear memberships of the doomed rows in ANY group (not just
	// federated ones): a materialized model an admin group had adopted
	// (possible before UpdateGroup grew the synthetic-member guard) would
	// otherwise FK-block the model sweep below.
	if _, err := tx.Exec(`DELETE FROM model_group_members WHERE model_id IN (` + mods + `)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fed_allowlist WHERE model_id IN (` + mods + `)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM models WHERE backend_id IN
		(SELECT id FROM backends WHERE ` + syntheticWhere + `)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM backends WHERE ` + syntheticWhere); err != nil {
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------- models

// FedModelInput is one advertised model's materializable fields (the catalog
// frame payload minus the wire envelope).
type FedModelInput struct {
	Name             string
	UpstreamModel    string
	PriceIn          int64
	PriceOut         int64
	PriceCacheRead   int64
	PriceCacheWrite  int64
	ContextLength    int64
	BenchTokPerS     float64
	ConcurrencyLimit int64
	BusyThresholdPct int64
}

// FedModelCollision reports why a advertised name may NOT be materialized:
// a non-empty local group owns the name, or a local (non-synthetic) public
// model already serves it. Two peers advertising the same name is fine —
// distinct synthetic backends — so only local (non-synthetic) rows collide.
func FedModelCollision(db *sql.DB, name string) (string, bool) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM model_groups g WHERE g.name=? AND
		EXISTS (SELECT 1 FROM model_group_members gm WHERE gm.group_id=g.id)`, name).Scan(&n); err != nil {
		return err.Error(), true
	}
	if n > 0 {
		return "a local group owns the name", true
	}
	err := db.QueryRow(`SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.name=? AND m.visibility='public' AND NOT (`+syntheticWhereB+`)`, name).Scan(&n)
	if err != nil {
		return err.Error(), true
	}
	if n > 0 {
		return "a local public model owns the name", true
	}
	return "", false
}

// UpsertFedModel inserts or updates one materialized model row, keyed by
// (backend_id, name): an update keeps the row id, so usage_log.model_id
// references and group memberships survive catalog churn (plan: model_id
// stability).
func UpsertFedModel(db *sql.DB, backendID int64, in FedModelInput) error {
	res, err := db.Exec(`UPDATE models SET upstream_model=?, price_in=?, price_out=?,
		price_cache_read=?, price_cache_write=?, context_length=?, bench_tok_per_s=?,
		concurrency_limit=?, busy_threshold_pct=?, enabled=1
		WHERE backend_id=? AND name=?`,
		in.UpstreamModel, in.PriceIn, in.PriceOut, in.PriceCacheRead, in.PriceCacheWrite,
		in.ContextLength, in.BenchTokPerS, in.ConcurrencyLimit, in.BusyThresholdPct,
		backendID, in.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// busy_threshold_pct explicit (column default is 0, cf. AddModels).
	_, err = db.Exec(`INSERT INTO models(backend_id, upstream_model, name, visibility,
		enabled, price_in, price_out, price_cache_read, price_cache_write, context_length,
		bench_tok_per_s, concurrency_limit, busy_threshold_pct)
		VALUES(?,?,?,'public',1,?,?,?,?,?,?,?,?)`,
		backendID, in.UpstreamModel, in.Name, in.PriceIn, in.PriceOut, in.PriceCacheRead,
		in.PriceCacheWrite, in.ContextLength, in.BenchTokPerS, in.ConcurrencyLimit,
		orDefault(in.BusyThresholdPct, 80))
	return err
}

func orDefault(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// DeleteFedModelsNotIn prunes a synthetic backend's model rows that the
// latest catalog no longer advertises (memberships first, no cascade on that
// FK — and for ANY group, admin or federated: a materialized model an admin
// group adopted must not FK-block the sweep). An empty keep set clears the
// backend.
func DeleteFedModelsNotIn(db *sql.DB, backendID int64, keep []string) error {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	var args []any
	for _, k := range keep {
		args = append(args, k)
	}
	clause := ""
	if len(keep) > 0 {
		clause = ` AND name NOT IN (` + ph + `)`
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sel := `SELECT id FROM models WHERE backend_id=?` + clause
	if _, err := tx.Exec(`DELETE FROM model_group_members WHERE model_id IN (`+sel+`)`,
		append([]any{backendID}, args...)...); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fed_allowlist WHERE model_id IN (`+sel+`)`,
		append([]any{backendID}, args...)...); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM models WHERE backend_id=?`+clause,
		append([]any{backendID}, args...)...)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------- groups

// FedGroupMember is one advertised group member: the model NAME plus its
// weight (names, not ids — ids are local to the advertising relay). The
// ModelID / BaseURL / FedVisibility fields carry the row's sharing
// identity so the hub can evaluate per-peer visibility in memory instead
// of one query per member per peer per push.
type FedGroupMember struct {
	ModelName     string
	Weight        float64
	ModelID       int64
	BaseURL       string
	FedVisibility string
}

// ErrSyntheticMember rejects putting a materialized (federation-owned)
// model into an admin group: the materializer prunes and re-creates those
// rows without regard for the group, so such a membership can never be
// stable.
var ErrSyntheticMember = errors.New("federated model cannot join an admin group")

// ResolveFedMember maps a group member name to a model row id: local
// (non-synthetic) rows win over materialized ones (the spoke's local-first
// rule — a spoke with its own copy of a model serves it itself), public
// visibility first, then lowest id. Rows already in a group are skipped
// (one group per model) EXCEPT membership in the federated group named
// groupName: re-applying that group's own frame must still resolve its
// current members, or the second hub frame would degrade the group to
// nothing. ok=false only when no row can serve the name; a non-nil err is
// a real DB failure the caller must NOT treat as "not resolvable" (a
// transient SQLITE_BUSY must not permanently drop a member).
func ResolveFedMember(db *sql.DB, name, groupName string) (int64, bool, error) {
	var id int64
	err := db.QueryRow(`SELECT m.id FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.name=? AND NOT EXISTS (SELECT 1 FROM model_group_members gm
			JOIN model_groups g ON g.id=gm.group_id
			WHERE gm.model_id=m.id AND NOT (g.federated=1 AND g.name=?))
		ORDER BY CASE WHEN (`+syntheticWhereB+`) THEN 1 ELSE 0 END,
			CASE WHEN m.visibility='public' THEN 0 ELSE 1 END, m.id
		LIMIT 1`, name, groupName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// UpsertFederatedGroup creates or replaces one materialized group: the row
// is marked federated=1, born enabled (the caller only sends groups with at
// least one resolvable member), and its membership is wholesale replaced
// with the resolved ids in frame order. A local (federated=0) group already
// owns the name → ErrConflict: the caller skips the frame group and logs.
// A public model owning the name → ErrExists (same rule CreateGroup enforces
// for admin groups).
func UpsertFederatedGroup(db *sql.DB, name string, memberIDs []int64, weights []float64) error {
	if len(memberIDs) == 0 {
		return fmt.Errorf("%w: federated group %q has no resolvable member", ErrInvalid, name)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var fed int
	err = tx.QueryRow(`SELECT federated FROM model_groups WHERE name=?`, name).Scan(&fed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if n, err := countPublicNamed(tx, name); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: a public model owns the name %q", ErrExists, name)
		}
		if _, err := tx.Exec(`INSERT INTO model_groups(name, enabled, federated) VALUES(?,1,1)`,
			name); err != nil {
			return err
		}
	case err != nil:
		return err
	case fed == 0:
		return fmt.Errorf("%w: local group owns the name %q", ErrConflict, name)
	}
	// Departing members become public again (the UpdateGroup rule: the group
	// owns the name while they are members).
	if _, err := tx.Exec(`UPDATE models SET visibility='public' WHERE id IN
		(SELECT model_id FROM model_group_members WHERE group_id=
			(SELECT id FROM model_groups WHERE name=?))`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_group_members WHERE group_id=
		(SELECT id FROM model_groups WHERE name=?)`, name); err != nil {
		return err
	}
	gid, err := groupIDByName(tx, name)
	if err != nil {
		return err
	}
	for i, mid := range memberIDs {
		w := weights[i]
		if w <= 0 {
			w = 1
		}
		if _, err := tx.Exec(`INSERT INTO model_group_members(group_id, model_id, weight, position)
			VALUES(?,?,?,?)`, gid, mid, w, i); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE models SET visibility='internal' WHERE id IN
		(SELECT model_id FROM model_group_members WHERE group_id=?)`, gid); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFederatedGroupsNotIn prunes federated groups the latest hub frame
// no longer advertises; their member models return to public (the members
// themselves are pruned by the per-backend model sweep).
func DeleteFederatedGroupsNotIn(db *sql.DB, keep []string) error {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	clause := ""
	if len(keep) > 0 {
		clause = ` AND name NOT IN (` + ph + `)`
	}
	args := make([]any, 0, len(keep))
	for _, k := range keep {
		args = append(args, k)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sel := `SELECT id FROM model_groups WHERE federated=1` + clause
	if _, err := tx.Exec(`UPDATE models SET visibility='public' WHERE id IN
		(SELECT model_id FROM model_group_members WHERE group_id IN (`+sel+`))`,
		args...); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_group_members WHERE group_id IN (`+sel+`)`,
		args...); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_groups WHERE federated=1`+clause, args...); err != nil {
		return err
	}
	return tx.Commit()
}

func countPublicNamed(q txLike, name string) (int, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM models WHERE name=? AND visibility='public'`,
		name).Scan(&n)
	return n, err
}

func groupIDByName(q txLike, name string) (int64, error) {
	var id int64
	err := q.QueryRow(`SELECT id FROM model_groups WHERE name=?`, name).Scan(&id)
	return id, err
}

// ------------------------------------------------------------- catalog

// CatalogShareableModel is one shareable model row (the catalog frame's
// source). The frame carries exactly these fields.
type CatalogShareableModel struct {
	Name             string  `json:"name"`
	UpstreamModel    string  `json:"upstream_model"`
	PriceIn          int64   `json:"price_in"`
	PriceOut         int64   `json:"price_out"`
	PriceCacheRead   int64   `json:"price_cache_read"`
	PriceCacheWrite  int64   `json:"price_cache_write"`
	ContextLength    int64   `json:"context_length"`
	BenchTokPerS     float64 `json:"bench_tok_per_s"`
	ConcurrencyLimit int64   `json:"concurrency_limit"`
	BusyThresholdPct int64   `json:"busy_threshold_pct"`
	BackendID        int64   `json:"backend_id"`
}

const catalogModelCols = `m.name, m.upstream_model, m.price_in, m.price_out,
	m.price_cache_read, m.price_cache_write, m.context_length, m.bench_tok_per_s,
	m.concurrency_limit, m.busy_threshold_pct, m.backend_id`

func scanCatalogModel(row interface{ Scan(...any) error }) (CatalogShareableModel, error) {
	var c CatalogShareableModel
	err := row.Scan(&c.Name, &c.UpstreamModel, &c.PriceIn, &c.PriceOut, &c.PriceCacheRead,
		&c.PriceCacheWrite, &c.ContextLength, &c.BenchTokPerS, &c.ConcurrencyLimit,
		&c.BusyThresholdPct, &c.BackendID)
	return c, err
}

// ListSpokeShareable lists what a spoke shares with the hub: fed_visibility
// 'all' only. 'allowlist' deliberately shares nobody on a spoke: the
// allowlist references THIS instance's peers table, and a spoke has no peer
// rows (peers are the hub's registry) — an allowlist model on a spoke is a
// mis-set switch, not a hidden share.
func ListSpokeShareable(db *sql.DB) ([]CatalogShareableModel, error) {
	rows, err := db.Query(`SELECT ` + catalogModelCols + ` FROM models m
		JOIN backends b ON b.id=m.backend_id
		WHERE m.fed_visibility='all' AND m.enabled=1 AND b.enabled=1
			AND NOT (` + syntheticWhereB + `)
		ORDER BY m.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogShareableModel{}
	for rows.Next() {
		c, err := scanCatalogModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListHubShareable lists the hub's own models visible to one peer: 'all'
// plus this peer's allowlist rows. Internal (group-member) rows count: the
// group carries them to the spoke, and the spoke maps members by name.
// Synthetic backends are excluded — the hub relays spoke catalogs from
// memory, never re-materializes them into its own push.
func ListHubShareable(db *sql.DB, peerID int64) ([]CatalogShareableModel, error) {
	rows, err := db.Query(`SELECT `+catalogModelCols+` FROM models m
		JOIN backends b ON b.id=m.backend_id
		WHERE m.enabled=1 AND b.enabled=1 AND NOT (`+syntheticWhereB+`) AND (
			m.fed_visibility='all' OR (m.fed_visibility='allowlist' AND
				EXISTS (SELECT 1 FROM fed_allowlist fa WHERE fa.model_id=m.id AND fa.peer_id=?)))
		ORDER BY m.name`, peerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogShareableModel{}
	for rows.Next() {
		c, err := scanCatalogModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CatalogGroup is one enabled group with its members as model NAMES (the
// hub's federated-group push; the spoke maps names to its own rows).
type CatalogGroup struct {
	Name    string
	Members []FedGroupMember
}

// ListHubGroups lists enabled groups with eligible members, members joined
// with their backend and fed_visibility so the caller can filter by peer
// visibility in memory (local rows need SharedWithPeer semantics;
// materialized rows are visible to everyone but their origin). Federated
// groups are excluded: this instance is a hub, any federated rows are
// stale leftovers.
func ListHubGroups(db *sql.DB) ([]CatalogGroup, error) {
	rows, err := db.Query(`SELECT g.name, m.name, gm.weight, m.id, b.base_url, m.fed_visibility
		FROM model_groups g
		JOIN model_group_members gm ON gm.group_id=g.id
		JOIN models m ON m.id=gm.model_id
		JOIN backends b ON b.id=m.backend_id
		WHERE g.enabled=1 AND m.enabled=1 AND b.enabled=1 AND g.federated=0
		ORDER BY g.name, gm.position, gm.model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogGroup{}
	idx := map[string]int{}
	for rows.Next() {
		var gname, mname, burl, vis string
		var w float64
		var modelID int64
		if err := rows.Scan(&gname, &mname, &w, &modelID, &burl, &vis); err != nil {
			return nil, err
		}
		i, ok := idx[gname]
		if !ok {
			idx[gname] = len(out)
			out = append(out, CatalogGroup{Name: gname})
			i = idx[gname]
		}
		out[i].Members = append(out[i].Members, FedGroupMember{
			ModelName: mname, Weight: w, ModelID: modelID,
			BaseURL: burl, FedVisibility: vis,
		})
	}
	return out, rows.Err()
}

// AllowlistModelIDs lists the model ids on peerID's allowlist in one query
// (the in-memory group-visibility filter's only DB need besides
// ListHubGroups).
func AllowlistModelIDs(db *sql.DB, peerID int64) (map[int64]bool, error) {
	rows, err := db.Query(`SELECT model_id FROM fed_allowlist WHERE peer_id=?`, peerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ModelIsFederated reports whether a model row lives on a synthetic backend
// (the proxy's serving gates key on the backend scheme; this is the
// row-level check, kept for the API layer's sharing views).
func ModelIsFederated(db *sql.DB, modelID int64) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.id=? AND (`+syntheticWhereB+`)`, modelID).Scan(&n)
	return n > 0, err
}
