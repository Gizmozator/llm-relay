// Federation metering aggregation (plan 2026-11-22-relay-federation,
// task 4). The hub's usage_log is the fairness ledger: every cross-peer
// flow writes a 'consumed' row (whose account spent) and a 'served' row
// (whose capacity went out), so served-vs-consumed per user or per peer
// falls out of one GROUP BY over fed_direction. Uniform convention:
// fed_peer_id ALWAYS names the hosting peer — the peer whose hardware
// served — and is NULL when hub hardware served or the row sits on a
// spoke with no local peer row; consumed rows identify the consumer by
// user_id. Spokes keep two local views instead: what this box served to
// whom (fed_user, hub-stamped), and what local users spent on federated
// (synthetic-backend) models — that slice is identified by the backend
// scheme, not by fed columns (spoke-local consumption rows are
// deliberately unstamped). Hub-local usage never appears in any of these
// rollups; FedLocalOnlyUsers names the accounts that are usage-present but
// ledger-absent, so the UI can explain the gap instead of showing a blank
// (ticket #13).
//
// All queries take a `since` UTC RFC3339 bound (the same convention as
// UsageFilter.Since: ts is stored as UTC RFC3339 text, so lexicographic
// >= is exact).
package store

import (
	"database/sql"
	"errors"
	"sort"
)

// FedCounts is one aggregated bucket of token/cost totals.
type FedCounts struct {
	Requests   int64 `json:"requests"`
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
	Cached     int64 `json:"cached_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	CostMicro  int64 `json:"cost_micro"`
}

const fedCountAggs = `COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(cost_total),0)`

// addFedCounts folds one aggregated bucket into another. The rollups
// accumulate per key so a key seen in several rows (or re-touched across
// the served/consumed passes) never loses an earlier bucket to
// assignment.
func addFedCounts(dst *FedCounts, c FedCounts) {
	dst.Requests += c.Requests
	dst.Prompt += c.Prompt
	dst.Completion += c.Completion
	dst.Cached += c.Cached
	dst.CacheWrite += c.CacheWrite
	dst.CostMicro += c.CostMicro
}

// FedUserTotals is one user's served/consumed rollup (hub fairness view).
// userID 0 = every user; otherwise only that user's entry is returned.
// UserID is the aggregation key: deleted users share the '(deleted user)'
// label but stay distinct entries (consumers disambiguate by id).
type FedUserTotal struct {
	UserID   int64     `json:"user_id"`
	User     string    `json:"user"`
	Served   FedCounts `json:"served"`
	Consumed FedCounts `json:"consumed"`
}

func FedUserTotals(db *sql.DB, since string, userID int64) ([]FedUserTotal, error) {
	where := `u.fed_direction != '' AND u.ts >= ?`
	args := []any{since}
	if userID != 0 {
		where += ` AND u.user_id = ?`
		args = append(args, userID)
	}
	// Keyed by user_id, never by the display name: several deleted users
	// share the '(deleted user)' label and must not collapse into one
	// entry (their totals would overwrite each other).
	byUser := map[int64]*FedUserTotal{}
	rows, err := db.Query(`SELECT u.user_id, COALESCE(us.username,'(deleted user)'), u.fed_direction, `+fedCountAggs+`
		FROM usage_log u LEFT JOIN users us ON us.id = u.user_id
		WHERE `+where+`
		GROUP BY u.user_id, us.username, u.fed_direction`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var user, dir string
		var c FedCounts
		if err := rows.Scan(&id, &user, &dir, &c.Requests, &c.Prompt, &c.Completion,
			&c.Cached, &c.CacheWrite, &c.CostMicro); err != nil {
			return nil, err
		}
		foldFedUser(byUser, id, user, dir, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Hub-hardware serving credit (ticket #12). A federated row whose
	// fed_peer_id is NULL was served by THIS relay's own hardware — the
	// uniform convention is that fed_peer_id names the hosting peer and is
	// NULL exactly when hub hardware served (a spoke→hub-local serve leaves
	// the consumed row with no peer to credit). When the model that served
	// lives on a backend with an owner, that owner is credited with the
	// serving: the hub mirror of the peer-owner served credit, so hub
	// capacity finally appears in the per-user fairness table.
	//
	// Query-time join, not a write-time stamp: it leaves the metering path
	// (usage.go) untouched and attributes historical rows the instant an
	// owner is set, with no backfill. Backends without an owner contribute
	// nothing (the join requires owner_user_id NOT NULL), so their ledger is
	// byte-identical to before; peer-hosted rows carry fed_peer_id NOT NULL
	// and are never seen here, so spoke crediting is unchanged and a row is
	// never counted on both sides.
	ownerWhere := `u.fed_direction != '' AND u.fed_peer_id IS NULL AND b.owner_user_id IS NOT NULL AND u.ts >= ?`
	ownerArgs := []any{since}
	if userID != 0 {
		ownerWhere += ` AND b.owner_user_id = ?`
		ownerArgs = append(ownerArgs, userID)
	}
	own, err := db.Query(`SELECT b.owner_user_id, COALESCE(us.username,'(deleted user)'), `+fedCountAggs+`
		FROM usage_log u
		JOIN models m ON m.id = u.model_id
		JOIN backends b ON b.id = m.backend_id
		LEFT JOIN users us ON us.id = b.owner_user_id
		WHERE `+ownerWhere+`
		GROUP BY b.owner_user_id, us.username`, ownerArgs...)
	if err != nil {
		return nil, err
	}
	defer own.Close()
	for own.Next() {
		var id int64
		var user string
		var c FedCounts
		if err := own.Scan(&id, &user, &c.Requests, &c.Prompt, &c.Completion,
			&c.Cached, &c.CacheWrite, &c.CostMicro); err != nil {
			return nil, err
		}
		foldFedUser(byUser, id, user, "served", c)
	}
	if err := own.Err(); err != nil {
		return nil, err
	}

	out := make([]FedUserTotal, 0, len(byUser))
	for _, e := range byUser {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return out[i].User < out[j].User
		}
		return out[i].UserID < out[j].UserID
	})
	return out, nil
}

// foldFedUser adds one aggregated bucket to a user's served or consumed
// rollup, creating the entry on first sight. Shared by the per-row pass and
// the hub-owner pass so both accumulate into the same keyed entry (a user who
// both owns a serving backend and has their own fed rows keeps one combined
// entry, never two).
func foldFedUser(byUser map[int64]*FedUserTotal, id int64, user, dir string, c FedCounts) {
	e, ok := byUser[id]
	if !ok {
		e = &FedUserTotal{UserID: id, User: user}
		byUser[id] = e
	}
	if dir == "served" {
		addFedCounts(&e.Served, c)
	} else {
		addFedCounts(&e.Consumed, c)
	}
}

// FedLocalOnlyEntry is one account whose window usage is entirely
// hub-local: unstamped rows and no federation rows, so the fairness ledger
// (FedUserTotals) cannot show it. The stats endpoints carry this list so
// the UI can say "no federation activity — this account's usage is local"
// instead of leaving a blank that reads as missing data (ticket #13).
// UserID distinguishes entries whose label collapsed to '(deleted user)'.
type FedLocalOnlyEntry struct {
	UserID int64     `json:"user_id"`
	User   string    `json:"user"`
	Counts FedCounts `json:"counts"`
}

// FedLocalOnlyUsers lists the accounts with unstamped (hub-local) usage in
// the window and NO federation rows in it — exactly the set that is present
// on the Usage page but absent from the ledger. A user with BOTH local and
// fed rows is not listed (they are already in the ledger; the hint would be
// wrong), and neither is a user with no usage at all — the caller tells
// "no federation activity" apart from "no data at all" by whether this list
// or the ledger has entries. userID 0 = every user; otherwise only that
// user's entry (empty whenever they have any federation row).
func FedLocalOnlyUsers(db *sql.DB, since string, userID int64) ([]FedLocalOnlyEntry, error) {
	// One pass over the window, grouped by account, keeping accounts that
	// have local (unstamped) usage but NO federation rows. The "has local /
	// has fed" decision is made inside the GROUP BY with conditional
	// aggregation (HAVING), NOT a per-row correlated NOT EXISTS: the old
	// shape ran the subquery once per local usage row, which on a real
	// usage_log (tens of thousands of local rows) took ~30s and timed out
	// the dashboard's stats call at the reverse proxy (504), while holding a
	// connection from the 4-slot pool and stalling every other admin read.
	//
	// Grouping by COALESCE(user_id,0) keeps the NULL-safe semantics the old
	// `f.user_id IS u.user_id` gave: anonymous (NULL user_id) local rows
	// group together and are excluded together if any fed row also carries a
	// NULL user_id. Safe against collision because users.id is AUTOINCREMENT
	// (starts at 1, never 0), so the 0 bucket is exclusively the anonymous
	// group. The sums count local rows only (CASE WHEN fed_direction
	// = ''), matching the old outer WHERE fed_direction = '' filter.
	where := `u.ts >= ?`
	args := []any{since}
	if userID != 0 {
		where += ` AND u.user_id = ?`
		args = append(args, userID)
	}
	rows, err := db.Query(`SELECT COALESCE(u.user_id,0), COALESCE(us.username,'(deleted user)'),
			SUM(CASE WHEN u.fed_direction = '' THEN 1 ELSE 0 END),
			COALESCE(SUM(CASE WHEN u.fed_direction = '' THEN prompt_tokens END),0),
			COALESCE(SUM(CASE WHEN u.fed_direction = '' THEN completion_tokens END),0),
			COALESCE(SUM(CASE WHEN u.fed_direction = '' THEN cached_tokens END),0),
			COALESCE(SUM(CASE WHEN u.fed_direction = '' THEN cache_write_tokens END),0),
			COALESCE(SUM(CASE WHEN u.fed_direction = '' THEN cost_total END),0)
		FROM usage_log u LEFT JOIN users us ON us.id = u.user_id
		WHERE `+where+`
		GROUP BY COALESCE(u.user_id,0), us.username
		HAVING SUM(CASE WHEN u.fed_direction = '' THEN 1 ELSE 0 END) > 0
			AND SUM(CASE WHEN u.fed_direction != '' THEN 1 ELSE 0 END) = 0
		ORDER BY 2, 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FedLocalOnlyEntry{}
	for rows.Next() {
		var e FedLocalOnlyEntry
		if err := rows.Scan(&e.UserID, &e.User, &e.Counts.Requests, &e.Counts.Prompt,
			&e.Counts.Completion, &e.Counts.Cached, &e.Counts.CacheWrite,
			&e.Counts.CostMicro); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FedPeerTotals is one peer's served/consumed rollup (hub Servers page).
type FedPeerTotal struct {
	PeerID   int64     `json:"peer_id"`
	Peer     string    `json:"peer"`
	Served   FedCounts `json:"served"`
	Consumed FedCounts `json:"consumed"`
}

// FedPeerTotals is one peer's served/consumed rollup (hub Servers page),
// under the uniform fed_peer_id convention: a row's fed_peer_id always
// names the HOSTING peer (the peer whose hardware served), never the
// origin. The served bucket sums the rows crediting this peer's hardware
// (mirror and transit rows both stamp the host). The consumed bucket is
// owner-level: every consumed row whose user is this peer's owner — what
// that account spent, wherever it spent it (a consumed row on hub
// hardware carries fed_peer_id NULL and still lands here). Caveat: an
// owner running several peers carries the same consumed total under each
// of them — the ledger attributes spending to accounts, not to the
// identity a person happens to run.
func FedPeerTotals(db *sql.DB, since string) ([]FedPeerTotal, error) {
	byPeer := map[int64]*FedPeerTotal{}
	served, err := db.Query(`SELECT u.fed_peer_id, COALESCE(p.name,'(deleted peer)'), `+fedCountAggs+`
		FROM usage_log u LEFT JOIN peers p ON p.id = u.fed_peer_id
		WHERE u.fed_direction = 'served' AND u.fed_peer_id IS NOT NULL AND u.ts >= ?
		GROUP BY u.fed_peer_id, p.name`, since)
	if err != nil {
		return nil, err
	}
	defer served.Close()
	for served.Next() {
		var id int64
		var name string
		var c FedCounts
		if err := served.Scan(&id, &name, &c.Requests, &c.Prompt, &c.Completion,
			&c.Cached, &c.CacheWrite, &c.CostMicro); err != nil {
			return nil, err
		}
		e, ok := byPeer[id]
		if !ok {
			e = &FedPeerTotal{PeerID: id, Peer: name}
			byPeer[id] = e
		}
		addFedCounts(&e.Served, c)
	}
	if err := served.Err(); err != nil {
		return nil, err
	}
	consumed, err := db.Query(`SELECT p.id, p.name, `+fedCountAggs+`
		FROM usage_log u JOIN peers p ON p.owner_user_id = u.user_id
		WHERE u.fed_direction = 'consumed' AND u.ts >= ?
		GROUP BY p.id, p.name`, since)
	if err != nil {
		return nil, err
	}
	defer consumed.Close()
	for consumed.Next() {
		var id int64
		var name string
		var c FedCounts
		if err := consumed.Scan(&id, &name, &c.Requests, &c.Prompt, &c.Completion,
			&c.Cached, &c.CacheWrite, &c.CostMicro); err != nil {
			return nil, err
		}
		e, ok := byPeer[id]
		if !ok {
			e = &FedPeerTotal{PeerID: id, Peer: name}
			byPeer[id] = e
		}
		addFedCounts(&e.Consumed, c)
	}
	if err := consumed.Err(); err != nil {
		return nil, err
	}
	out := make([]FedPeerTotal, 0, len(byPeer))
	for _, e := range byPeer {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out, nil
}

// FedSourceTotals is one hub-stamped identity's share of what this spoke
// served ("my box served N tokens to User A this week").
type FedSourceTotals struct {
	FedUser string    `json:"fed_user"`
	Counts  FedCounts `json:"counts"`
}

// FedServedBySource is the spoke view over its served rows. fedUser "" =
// every source; otherwise only that identity's entry.
func FedServedBySource(db *sql.DB, since, fedUser string) ([]FedSourceTotals, error) {
	where := `fed_direction = 'served' AND ts >= ?`
	args := []any{since}
	if fedUser != "" {
		where += ` AND fed_user = ?`
		args = append(args, fedUser)
	}
	rows, err := db.Query(`SELECT fed_user, `+fedCountAggs+`
		FROM usage_log WHERE `+where+` GROUP BY fed_user ORDER BY fed_user`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FedSourceTotals{}
	for rows.Next() {
		var e FedSourceTotals
		if err := rows.Scan(&e.FedUser, &e.Counts.Requests, &e.Counts.Prompt, &e.Counts.Completion,
			&e.Counts.Cached, &e.Counts.CacheWrite, &e.Counts.CostMicro); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FedConsumedEntry is one local user's consumption of federated models.
// UserID distinguishes entries whose display label collapsed to
// '(deleted user)'.
type FedConsumedEntry struct {
	UserID int64     `json:"user_id"`
	User   string    `json:"user"`
	Counts FedCounts `json:"counts"`
}

// FedConsumedSlice is the spoke view of local users spending on models
// that live on synthetic backends (peer://… or hub://…). Those rows carry
// no fed columns by design (the spoke meters its own users normally);
// the backend scheme identifies the slice. userID 0 = every user.
func FedConsumedSlice(db *sql.DB, since string, userID int64) ([]FedConsumedEntry, error) {
	where := `(b.base_url LIKE 'peer://%' OR b.base_url LIKE 'hub://%') AND u.ts >= ?`
	args := []any{since}
	if userID != 0 {
		where += ` AND u.user_id = ?`
		args = append(args, userID)
	}
	rows, err := db.Query(`SELECT u.user_id, COALESCE(us.username,'(deleted user)'), `+fedCountAggs+`
		FROM usage_log u
		JOIN models m ON m.id = u.model_id
		JOIN backends b ON b.id = m.backend_id
		LEFT JOIN users us ON us.id = u.user_id
		WHERE `+where+`
		GROUP BY u.user_id, us.username ORDER BY 2, 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FedConsumedEntry{}
	for rows.Next() {
		var e FedConsumedEntry
		if err := rows.Scan(&e.UserID, &e.User, &e.Counts.Requests, &e.Counts.Prompt, &e.Counts.Completion,
			&e.Counts.Cached, &e.Counts.CacheWrite, &e.Counts.CostMicro); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FedPeerOwner resolves a peer's owner user (the identity the hub's
// mirrored served rows credit with serving).
func FedPeerOwner(db *sql.DB, peerID int64) (*User, error) {
	p, err := PeerByID(db, peerID)
	if err != nil {
		return nil, err
	}
	return UserByID(db, p.OwnerUserID)
}

// FedPeerModel resolves a name to this relay's materialized row for that
// peer (the peer://<id> backend): the hub uses it to attribute transit
// traffic to the row its own catalog carries. nil (no error) when no such
// row exists — the peer may have un-shared the model mid-flight.
func FedPeerModel(db *sql.DB, peerID int64, name string) (*Model, error) {
	row := db.QueryRow(`SELECT `+modelCols+` FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.name=? AND b.base_url=?`, name, PeerBackendURL(peerID))
	m, err := scanModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}
