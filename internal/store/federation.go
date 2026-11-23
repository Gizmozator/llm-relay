// Federation store layer (plan 2026-11-22-relay-federation, task 1):
// instance role + spoke config in the settings table, the peers registry
// (server tokens a spoke dials the hub with), and the per-model federation
// visibility switch with its allowlist.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"llm-relay/internal/auth"
)

// ---------------------------------------------------------------- settings

// Settings keys. federation_role is the instance mode; hub_url/hub_token is
// the spoke's dial-in config. hub_token is stored AS ENTERED: the spoke must
// present it to the hub verbatim, so it cannot be hashed at rest. That is
// plaintext-at-rest like every other secret in this DB (backend API keys) —
// the file's permissions are the protection. Peer tokens (below) are the
// hub side and follow the api_keys hashing rule instead.
const (
	SettingFedRole     = "federation_role"
	SettingFedHubURL   = "hub_url"
	SettingFedHubToken = "hub_token"

	FedRoleStandalone = "standalone"
	FedRoleHub        = "hub"
	FedRoleSpoke      = "spoke"
)

// ErrFedConfig marks a validation failure on the federation settings
// (wrapped with the reason); the API maps it to 400.
var ErrFedConfig = errors.New("invalid federation config")

// FederationSettings is the instance role plus the spoke's dial-in config.
type FederationSettings struct {
	Role     string `json:"role"`
	HubURL   string `json:"hub_url"`
	HubToken string `json:"hub_token"`
}

func GetFederationSettings(db *sql.DB) (FederationSettings, error) {
	out := FederationSettings{Role: FedRoleStandalone}
	role, err := GetSetting(db, SettingFedRole)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return out, err
	}
	if role != "" {
		out.Role = role
	}
	if v, err := GetSetting(db, SettingFedHubURL); err == nil {
		out.HubURL = v
	} else if !errors.Is(err, ErrNotFound) {
		return out, err
	}
	if v, err := GetSetting(db, SettingFedHubToken); err == nil {
		out.HubToken = v
	} else if !errors.Is(err, ErrNotFound) {
		return out, err
	}
	return out, nil
}

// PutFederationSettings validates and persists the whole settings block.
// Leaving spoke mode clears the dial-in config (a stale hub_url/token must
// not survive a role change). Live-safe semantics of the change (dropping
// the channel, reconnecting) are task 2's job — this just persists truth.
func PutFederationSettings(db *sql.DB, in FederationSettings) error {
	in.Role = strings.TrimSpace(in.Role)
	switch in.Role {
	case FedRoleStandalone, FedRoleHub, FedRoleSpoke:
	default:
		return fmt.Errorf("%w: role must be standalone, hub or spoke", ErrFedConfig)
	}
	if in.Role == FedRoleSpoke {
		in.HubURL = strings.TrimRight(strings.TrimSpace(in.HubURL), "/")
		u, err := url.Parse(in.HubURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: spoke requires an http(s) hub_url", ErrFedConfig)
		}
		if strings.TrimSpace(in.HubToken) == "" {
			return fmt.Errorf("%w: spoke requires the hub server token", ErrFedConfig)
		}
		in.HubToken = strings.TrimSpace(in.HubToken)
	} else {
		in.HubURL, in.HubToken = "", ""
	}
	// One tx: the three keys are one logical block, so a half-written state
	// (role=spoke with a stale or missing token) must never be observable.
	// SetSetting is autocommit-only, so its UPSERT is inlined here.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{
		SettingFedRole: in.Role, SettingFedHubURL: in.HubURL, SettingFedHubToken: in.HubToken,
	} {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- peers

// Peer is one federation server token: a spoke's right to dial in, owned by
// the user who created it (plan Decision 2: a peer maps to exactly one hub
// user; its traffic is that user's).
type Peer struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	OwnerUserID int64  `json:"owner_user_id"`
	OwnerName   string `json:"owner_name"`
	Enabled     bool   `json:"enabled"`
	LastSeen    string `json:"last_seen"`
	Version     string `json:"version"`
	CreatedAt   string `json:"created_at"`
	// SharedModels counts the models this peer may see: every fed_visibility
	// 'all' model plus its allowlist rows. Computed at list time so the
	// Servers page needs no per-peer round-trips.
	SharedModels int `json:"shared_models"`
	// Online is the live-channel flag the API layer stamps from the hub
	// registry (list time). False on a standalone/spoke box: peers only
	// dial a hub, so their channels never live there.
	Online bool `json:"online"`
}

const peerCols = `p.id, p.name, p.owner_user_id, u.username, p.enabled,
	p.last_seen, p.version, p.created_at`

// scanPeer reads peerCols + sharedModelsExpr; extra receives any additional
// projection columns appended after them (GetPeerByKeyHash's owner flag).
func scanPeer(row interface{ Scan(...any) error }, extra ...any) (*Peer, error) {
	p := &Peer{}
	var en int
	targets := append([]any{&p.ID, &p.Name, &p.OwnerUserID, &p.OwnerName, &en,
		&p.LastSeen, &p.Version, &p.CreatedAt, &p.SharedModels}, extra...)
	if err := row.Scan(targets...); err != nil {
		return nil, err
	}
	p.Enabled = en != 0
	return p, nil
}

// sharedModelsExpr counts what a peer may see; 'all' models are global, so
// the same constant term rides every row — fine at homelab scale and it
// keeps the count honest for both visibility modes. Both terms require the
// servable filter (model AND backend enabled, cf. ListPublicModels): a
// disabled model is not shareable, so counting it would overstate the
// Servers page. Draining still counts — draining models stay servable.
const sharedModelsExpr = `(SELECT COUNT(*) FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.fed_visibility='all' AND m.enabled=1 AND b.enabled=1) +
	(SELECT COUNT(*) FROM fed_allowlist fa JOIN models m ON m.id=fa.model_id
		JOIN backends b ON b.id=m.backend_id
		WHERE fa.peer_id = p.id AND m.enabled=1 AND b.enabled=1)`

func CreatePeer(db *sql.DB, name string, ownerUserID int64) (*Peer, string, error) {
	plain, hash, err := auth.GenerateServerToken()
	if err != nil {
		return nil, "", err
	}
	res, err := db.Exec(`INSERT INTO peers(name, owner_user_id, key_hash, created_at) VALUES(?,?,?,?)`,
		name, ownerUserID, hash, now())
	if err != nil {
		return nil, "", err
	}
	id, _ := res.LastInsertId()
	p, err := PeerByID(db, id)
	if err != nil {
		return nil, "", err
	}
	// The raw token is returned exactly once (create/rotate responses);
	// everything after this call only ever sees the hash.
	return p, plain, nil
}

func PeerByID(db *sql.DB, id int64) (*Peer, error) {
	p, err := scanPeer(db.QueryRow(`SELECT `+peerCols+`, `+sharedModelsExpr+`
		FROM peers p JOIN users u ON u.id=p.owner_user_id WHERE p.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// GetPeerByKeyHash resolves a server token for auth. Same contract as
// KeyByHash: the owner's live row is part of the lookup, so disabling a
// user revokes their fed- tokens immediately (ErrNotFound → the
// middleware's 401), exactly like disabling an account kills its API keys
// and dashboard sessions. The peer's own enabled check lives in the
// middleware so it can answer "disabled" distinctly.
func GetPeerByKeyHash(db *sql.DB, hash string) (*Peer, error) {
	var ownerDisabled int
	p, err := scanPeer(db.QueryRow(`SELECT `+peerCols+`, `+sharedModelsExpr+`, u.disabled
		FROM peers p JOIN users u ON u.id=p.owner_user_id WHERE p.key_hash=?`, hash),
		&ownerDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ownerDisabled != 0 {
		return nil, ErrNotFound
	}
	return p, nil
}

func ListPeers(db *sql.DB) ([]Peer, error) {
	return queryPeers(db, `SELECT `+peerCols+`, `+sharedModelsExpr+`
		FROM peers p JOIN users u ON u.id=p.owner_user_id ORDER BY p.name, p.id`)
}

func ListPeersByOwner(db *sql.DB, userID int64) ([]Peer, error) {
	return queryPeers(db, `SELECT `+peerCols+`, `+sharedModelsExpr+`
		FROM peers p JOIN users u ON u.id=p.owner_user_id
		WHERE p.owner_user_id=? ORDER BY p.name, p.id`, userID)
}

func queryPeers(db *sql.DB, q string, args ...any) ([]Peer, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Peer{}
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// RotatePeer replaces the token and returns the new raw token (shown once).
func RotatePeer(db *sql.DB, id int64) (string, error) {
	plain, hash, err := auth.GenerateServerToken()
	if err != nil {
		return "", err
	}
	res, err := db.Exec(`UPDATE peers SET key_hash=? WHERE id=?`, hash, id)
	if err != nil {
		return "", err
	}
	return rotateResult(res, plain)
}

func rotateResult(res sql.Result, plain string) (string, error) {
	if n, err := res.RowsAffected(); err != nil {
		return "", err
	} else if n == 0 {
		return "", ErrNotFound
	}
	return plain, nil
}

func SetPeerEnabled(db *sql.DB, id int64, enabled bool) error {
	res, err := db.Exec(`UPDATE peers SET enabled=? WHERE id=?`, b2i(enabled), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePeer drops the peer; fed_allowlist rows cascade away with it.
func DeletePeer(db *sql.DB, id int64) error {
	res, err := db.Exec(`DELETE FROM peers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PeerAuthState reports the live revocation facts for a peer id: whether
// the row exists, is enabled, and whether its owner is disabled. The hub
// sweep (plan 2026-11-22-relay-federation task 2 review) runs it on the
// Touch throttle so revocations that did not pass through the peer
// endpoints — e.g. a user disabled via the admin users API — still drop
// the live channel within one sweep interval.
func PeerAuthState(db *sql.DB, peerID int64) (exists, enabled, ownerDisabled bool, err error) {
	var en, dis int
	err = db.QueryRow(`SELECT p.enabled, u.disabled FROM peers p
		JOIN users u ON u.id=p.owner_user_id WHERE p.id=?`, peerID).Scan(&en, &dis)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false, nil
	}
	if err != nil {
		return false, false, false, err
	}
	return true, en != 0, dis != 0, nil
}

// TouchPeer records liveness bookkeeping (WS channel, task 2).
func TouchPeer(db *sql.DB, id int64, lastSeen, version string) error {
	res, err := db.Exec(`UPDATE peers SET last_seen=?, version=? WHERE id=?`, lastSeen, version, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- model sharing

// FedVisibility values: off shares nothing (default), all shares with every
// peer, allowlist shares with the fed_allowlist peers only.
const (
	FedVisOff       = "off"
	FedVisAll       = "all"
	FedVisAllowlist = "allowlist"
)

// SetModelFedVisibility writes the per-model sharing switch and, for
// 'allowlist', replaces the peer allowlist wholesale. peerIDs must be
// existing peers (the whole call is one tx, so a bad id leaves nothing
// half-written). Passing a non-allowlist visibility clears the list.
func SetModelFedVisibility(db *sql.DB, modelID int64, visibility string, peerIDs []int64) error {
	switch visibility {
	case FedVisOff, FedVisAll, FedVisAllowlist:
	default:
		return fmt.Errorf("%w: fed_visibility must be off, all or allowlist", ErrFedConfig)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE models SET fed_visibility=? WHERE id=?`, visibility, modelID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM fed_allowlist WHERE model_id=?`, modelID); err != nil {
		return err
	}
	if visibility == FedVisAllowlist {
		uniq := slices.Clone(peerIDs)
		slices.Sort(uniq)
		uniq = slices.Compact(uniq)
		for _, pid := range uniq {
			var one int
			if err := tx.QueryRow(`SELECT 1 FROM peers WHERE id=?`, pid).Scan(&one); errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: peer %d does not exist", ErrNotFound, pid)
			} else if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO fed_allowlist(model_id, peer_id) VALUES(?,?)`,
				modelID, pid); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ModelFedAllowlist returns the peer ids a model is shared with (the
// allowlist side; empty for 'off'/'all').
func ModelFedAllowlist(db *sql.DB, modelID int64) ([]int64, error) {
	rows, err := db.Query(`SELECT peer_id FROM fed_allowlist WHERE model_id=? ORDER BY peer_id`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SharedWithPeer reports whether peerID may use modelID under the sharing
// rules — the check task 2's hub side runs before serving a peer request.
func SharedWithPeer(db *sql.DB, modelID, peerID int64) (bool, error) {
	var vis string
	err := db.QueryRow(`SELECT fed_visibility FROM models WHERE id=?`, modelID).Scan(&vis)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	switch vis {
	case FedVisAll:
		return true, nil
	case FedVisAllowlist:
		var one int
		err := db.QueryRow(`SELECT 1 FROM fed_allowlist WHERE model_id=? AND peer_id=?`,
			modelID, peerID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	default:
		return false, nil
	}
}
