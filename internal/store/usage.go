// Usage recording and aggregation queries.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// UsageEvent is one row of the usage_log.
type UsageEvent struct {
	TS            string
	UserID        int64
	KeyID         int64
	ClientIP      string
	SessionID     string
	Endpoint      string
	ModelID       int64
	ModelRequ     string
	ModelUpstream string
	Prompt        int64
	Completion    int64
	Cached        int64
	CacheWrite    int64
	CostIn        int64
	CostOut       int64
	CostCache     int64
	CostWrite     int64
	CostTotal     int64
	Streamed      bool
	Estimated     bool
	Status        int
	LatencyMS     int64
	Error         string
	// Federation attribution (migration 0013): "" = plain local traffic;
	// "served"/"consumed" from this relay's point of view. FedPeerID is the
	// counterparty peer (nil = none: a spoke's served row has no local peer
	// row for the hub). FedUser is the hub-stamped hub-side identity name a
	// serving spoke records (spoke-local usernames never leave the spoke).
	FedDirection string
	FedPeerID    *int64
	FedUser      string
}

func InsertUsage(db *sql.DB, e *UsageEvent) error {
	_, err := db.Exec(`INSERT INTO usage_log(ts, user_id, api_key_id, client_ip, session_id, endpoint,
			model_id, model_requested, model_upstream,
			prompt_tokens, completion_tokens, cached_tokens, cache_write_tokens,
			cost_in, cost_out, cost_cache, cost_write, cost_total,
			streamed, usage_estimated, status_code, latency_ms, error,
			fed_direction, fed_peer_id, fed_user)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TS, e.UserID, e.KeyID, e.ClientIP, nullable(e.SessionID), e.Endpoint,
		e.ModelID, e.ModelRequ, e.ModelUpstream,
		e.Prompt, e.Completion, e.Cached, e.CacheWrite,
		e.CostIn, e.CostOut, e.CostCache, e.CostWrite, e.CostTotal,
		b2i(e.Streamed), b2i(e.Estimated), e.Status, e.LatencyMS, nullable(e.Error),
		e.FedDirection, e.FedPeerID, e.FedUser)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UsageFilter scopes an aggregation query. Zero values mean "no filter" —
// except the user scope, which must be stated: UserID == 0 alone is the
// forgotten-scope footgun, so unscoped queries must set AllUsers.
type UsageFilter struct {
	// AllUsers opts into a cross-user query. Required whenever UserID is 0;
	// the aggregation functions refuse a zero filter otherwise.
	AllUsers bool
	UserID   int64 // 0 = unscoped (requires AllUsers)
	KeyID    int64
	Model    string // exact model_requested match, "" = any
	From     string // YYYY-MM-DD inclusive, "" = open
	To       string // YYYY-MM-DD inclusive, "" = open
	Since    string // RFC3339 UTC inclusive, "" = open (live-window queries)
	// TZOffset shifts day bucketing and the From/To bounds to a viewer's
	// clock: minutes east of UTC (120 = UTC+2, -60 = UTC-1). 0 — the
	// default — keeps every query byte-identical to plain UTC, so callers
	// that do not set it are unaffected. A fixed offset for the whole
	// range: no DST history (callers pass the viewer's offset of *now*).
	TZOffset int
}

// scopedErr refuses a zero-value user scope: UserID == 0 without AllUsers
// means the caller forgot to scope, and silently summing every user's rows
// is exactly the mistake this guards.
func (f *UsageFilter) scopedErr() error {
	if f.UserID == 0 && !f.AllUsers {
		return errors.New("usage filter: zero UserID without AllUsers (unscoped query refused)")
	}
	return nil
}

func (f *UsageFilter) where() (string, []any) {
	conds := []string{}
	args := []any{}
	if f.Since != "" {
		// ts is stored as UTC RFC3339 text, so lexicographic >= is exact.
		conds = append(conds, `u.ts >= ?`)
		args = append(args, f.Since)
	}
	if f.UserID != 0 {
		conds = append(conds, `u.user_id = ?`)
		args = append(args, f.UserID)
	}
	if f.KeyID != 0 {
		conds = append(conds, `u.api_key_id = ?`)
		args = append(args, f.KeyID)
	}
	if f.Model != "" {
		conds = append(conds, `u.model_requested = ?`)
		args = append(args, f.Model)
	}
	if f.From != "" {
		conds = append(conds, `u.ts >= ?`)
		args = append(args, f.dayBound(f.From, 0))
	}
	if f.To != "" {
		if f.TZOffset == 0 {
			conds = append(conds, `u.ts < datetime(?, '+1 day')`)
			args = append(args, f.To+"T00:00:00Z")
		} else {
			// The To-day's local end (= To+1 local midnight) as a UTC
			// instant, computed in Go; the bound stays exclusive.
			conds = append(conds, `u.ts < ?`)
			args = append(args, f.dayBound(f.To, 1))
		}
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + join(conds, " AND "), args
}

// dayBound returns the UTC RFC3339 instant of local midnight on the given
// YYYY-MM-DD date (plus `days` days) at the filter's TZOffset: local
// midnight = UTC midnight minus the offset. With TZOffset == 0 it is the
// plain UTC bound, byte-identical to the pre-offset form. A malformed date
// falls back to that plain bound (the API validates the format before the
// store sees it; this keeps where() total and the fallback conservative).
func (f *UsageFilter) dayBound(date string, days int) string {
	if f.TZOffset == 0 {
		return date + "T00:00:00Z"
	}
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date + "T00:00:00Z"
	}
	return t.UTC().AddDate(0, 0, days).Add(-time.Duration(f.TZOffset) * time.Minute).Format(time.RFC3339)
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// UsageGroup is one aggregated bucket. Key/Label are empty for the totals row.
type UsageGroup struct {
	Key        string `json:"key,omitempty"`
	Label      string `json:"label,omitempty"`
	Requests   int64  `json:"requests"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Cached     int64  `json:"cached_tokens"`
	CacheWrite int64  `json:"cache_write_tokens"`
	CostMicro  int64  `json:"cost_micro"`
}

var groupExprs = map[string]string{
	"day":  `substr(u.ts, 1, 10)`,
	"user": `COALESCE(us.username, '(deleted user)')`,
	// A row with no local key (api_key_id 0/NULL — the synthetic federation
	// key) never had a key to delete: it is federation/spoke traffic and
	// buckets under the federation marker. Only a row whose local key row is
	// gone prints "(deleted key)". The federation marker names the hosting
	// peer ONLY in the admin all-users view (keyGroupExprAdmin); a personal
	// route must not learn another user's peer name, so it uses the bare
	// marker (keyGroupExprScoped). SummarizeUsage picks per scope.
	"key":      keyGroupExprAdmin,
	"model":    `COALESCE(u.model_requested, '(unknown)')`,
	"ip":       `COALESCE(u.client_ip, '(unknown)')`,
	"endpoint": `u.endpoint`,
	"session":  `COALESCE(substr(u.session_id, 1, 12), '(none)')`,
}

// The by-key label has two shapes. Both keep a real key's name and reserve
// "(deleted key)" for a row whose local key row is gone; they differ only in
// the federation marker. The admin view names the hosting peer (it is
// operator data); a personal scope must not, because a transit row's
// fed_peer_id names a peer the caller may not own — the same leak the detail
// route closes by zeroing fed_peer.
const (
	keyGroupExprAdmin = `CASE WHEN k.name IS NOT NULL THEN k.name
			WHEN u.api_key_id IS NULL OR u.api_key_id = 0
				THEN COALESCE('federation · ' || p.name, 'federation')
			ELSE '(deleted key)' END`
	keyGroupExprScoped = `CASE WHEN k.name IS NOT NULL THEN k.name
			WHEN u.api_key_id IS NULL OR u.api_key_id = 0
				THEN 'federation'
			ELSE '(deleted key)' END`
)

// ValidGroupBy reports whether groupBy is a supported bucket dimension.
// Handlers validate with it so a bad value is a 400, not a store error.
func ValidGroupBy(groupBy string) bool {
	_, ok := groupExprs[groupBy]
	return ok
}

// SummarizeUsage returns totals plus one bucket per group_by dimension.
func SummarizeUsage(db *sql.DB, f *UsageFilter, groupBy string) (UsageGroup, []UsageGroup, error) {
	if err := f.scopedErr(); err != nil {
		return UsageGroup{}, nil, err
	}
	expr, ok := groupExprs[groupBy]
	if !ok {
		return UsageGroup{}, nil, fmt.Errorf("bad group_by %q", groupBy)
	}
	// The by-key label names the hosting peer only in the admin all-users
	// view; a personal scope gets the bare federation marker so it cannot
	// learn another user's peer name (mirrors the detail route zeroing
	// fed_peer). See keyGroupExpr{Admin,Scoped}.
	if groupBy == "key" && !f.AllUsers {
		expr = keyGroupExprScoped
	}
	where, args := f.where()

	// Day buckets split at the viewer's local midnight when an offset is
	// set: shift each ts by the offset before taking the date, with the
	// modifier as a bound parameter (never interpolated into the SQL). The
	// SELECT-expr arg is prepended before the where() args to match the
	// placeholder order.
	exprArgs := []any{}
	if groupBy == "day" && f.TZOffset != 0 {
		expr = `strftime('%Y-%m-%d', u.ts, ?)`
		exprArgs = append(exprArgs, fmt.Sprintf("%+d minutes", f.TZOffset))
	}

	totals := UsageGroup{}
	err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(cost_total),0)
		FROM usage_log u`+where, args...).
		Scan(&totals.Requests, &totals.Prompt, &totals.Completion, &totals.Cached, &totals.CacheWrite, &totals.CostMicro)
	if err != nil {
		return totals, nil, err
	}

	rows, err := db.Query(`SELECT `+expr+` AS gk, COUNT(*), COALESCE(SUM(prompt_tokens),0),
			COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(cost_total),0)
		FROM usage_log u
		LEFT JOIN api_keys k ON k.id = u.api_key_id
		LEFT JOIN users us ON us.id = u.user_id
		LEFT JOIN peers p ON p.id = u.fed_peer_id`+where+
		` GROUP BY gk ORDER BY gk`, append(exprArgs, args...)...)
	if err != nil {
		return totals, nil, err
	}
	defer rows.Close()
	groups := []UsageGroup{}
	for rows.Next() {
		var g UsageGroup
		if err := rows.Scan(&g.Key, &g.Requests, &g.Prompt, &g.Completion, &g.Cached, &g.CacheWrite, &g.CostMicro); err != nil {
			return totals, groups, err
		}
		g.Label = g.Key
		groups = append(groups, g)
	}
	return totals, groups, rows.Err()
}

// LiveBucket is one minute of the live dashboard window. Cost is
// deliberately absent: the dashboard answers "is the server busy", not
// "who spent what" (owner decision — per-user cost lives on the Usage page).
type LiveBucket struct {
	Minute     string `json:"minute"`
	Requests   int64  `json:"requests"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Cached     int64  `json:"cached_tokens"`
	CacheWrite int64  `json:"cache_write_tokens"`
}

// LiveBuckets aggregates the last `minutes` minutes into one bucket per
// minute, zero-filled so the series is continuous (the chart never has to
// guess gaps). `now` anchors the window; rows are bucketed by the minute
// prefix of their UTC RFC3339 ts.
func LiveBuckets(db *sql.DB, f *UsageFilter, now time.Time, minutes int) ([]LiveBucket, error) {
	if err := f.scopedErr(); err != nil {
		return nil, err
	}
	start := now.UTC().Truncate(time.Minute).Add(time.Duration(-minutes+1) * time.Minute)
	// Local copy: stamping Since on the caller's filter would leak this
	// query's window into a filter the caller reuses for other queries.
	f2 := *f
	f2.Since = start.Format(time.RFC3339)
	where, args := f2.where()

	rows, err := db.Query(`SELECT substr(u.ts,1,16) AS minute, COUNT(*),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_write_tokens),0)
		FROM usage_log u`+where+` GROUP BY minute ORDER BY minute`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byMinute := map[string]LiveBucket{}
	for rows.Next() {
		var b LiveBucket
		if err := rows.Scan(&b.Minute, &b.Requests, &b.Prompt, &b.Completion,
			&b.Cached, &b.CacheWrite); err != nil {
			return nil, err
		}
		byMinute[b.Minute] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]LiveBucket, 0, minutes)
	for i := 0; i < minutes; i++ {
		label := start.Add(time.Duration(i) * time.Minute).Format("2006-01-02T15:04")
		if b, ok := byMinute[label]; ok {
			out = append(out, b)
		} else {
			out = append(out, LiveBucket{Minute: label})
		}
	}
	return out, nil
}

// ModelBusy is one model's per-minute busy time over the live window.
type ModelBusy struct {
	Model  string  `json:"model"`
	Values []int64 `json:"values"` // busy ms per minute, aligned with LiveBuckets
}

// BusyByModel distributes each request's wall-clock duration
// [ts − latency_ms, ts] across the minute series (clipped at both window
// ends) and aggregates busy milliseconds per model. A row can only overlap
// the window if it completed inside it, so the Since bound is sufficient.
// Values align with the same window as LiveBuckets.
func BusyByModel(db *sql.DB, f *UsageFilter, now time.Time, minutes int) ([]ModelBusy, error) {
	if err := f.scopedErr(); err != nil {
		return nil, err
	}
	start := now.UTC().Truncate(time.Minute).Add(time.Duration(-minutes+1) * time.Minute)
	// Local copy (see LiveBuckets): the caller's filter keeps its fields.
	f2 := *f
	f2.Since = start.Format(time.RFC3339)
	where, args := f2.where()

	rows, err := db.Query(`SELECT u.ts, u.latency_ms, COALESCE(u.model_requested,'(unknown)')
		FROM usage_log u`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byModel := map[string][]int64{}
	for rows.Next() {
		var tsStr, model string
		var latency int64
		if err := rows.Scan(&tsStr, &latency, &model); err != nil {
			return nil, err
		}
		if latency <= 0 {
			continue
		}
		end, perr := time.Parse(time.RFC3339, tsStr)
		if perr != nil {
			continue // malformed row: nothing sensible to place
		}
		end = end.UTC()
		begin := end.Add(-time.Duration(latency) * time.Millisecond)
		// Minute indices the request overlaps, clipped to the window.
		i0 := int(begin.Sub(start) / time.Minute)
		if i0 < 0 {
			i0 = 0
		}
		i1 := int(end.Sub(start) / time.Minute)
		if i1 > minutes-1 {
			i1 = minutes - 1
		}
		if i0 > i1 {
			continue
		}
		vals, ok := byModel[model]
		if !ok {
			vals = make([]int64, minutes)
			byModel[model] = vals
		}
		for i := i0; i <= i1; i++ {
			mStart := start.Add(time.Duration(i) * time.Minute)
			mEnd := mStart.Add(time.Minute)
			ovStart, ovEnd := begin, end
			if ovStart.Before(mStart) {
				ovStart = mStart
			}
			if ovEnd.After(mEnd) {
				ovEnd = mEnd
			}
			if d := ovEnd.Sub(ovStart); d > 0 {
				vals[i] += d.Milliseconds()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []ModelBusy{}
	for model, vals := range byModel {
		var total int64
		for _, v := range vals {
			total += v
		}
		if total == 0 {
			continue
		}
		out = append(out, ModelBusy{Model: model, Values: vals})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// UsageRow is one detailed usage record for the detail table.
type UsageRow struct {
	ID       int64  `json:"id"`
	TS       string `json:"ts"`
	Username string `json:"username"`
	KeyName  string `json:"key_name"`
	// KeyLocal reports whether the row ever referenced a LOCAL api key
	// (api_key_id non-zero). With an empty KeyName it separates the two
	// "no key name" cases: true = the key row is gone (a genuinely deleted
	// key); false = the row never had a local key — it arrived through the
	// federation and carries the synthetic federation key (api_key_id 0).
	KeyLocal bool `json:"key_local"`
	// FedPeer names the hosting peer of a federation row (nil for plain
	// local traffic): the marker label's "where it came from". Personal
	// routes zero it (peer names are wiring), like ModelUpstream.
	FedPeer   *string `json:"fed_peer"`
	ClientIP  *string `json:"client_ip"`
	SessionID *string `json:"session_id"`
	Endpoint  string  `json:"endpoint"`
	ModelRequ *string `json:"model_requested"`
	// ModelUpstream is the upstream model id the chosen member actually
	// served: for a group it names the member's engine model, telling
	// members apart when their upstream ids differ (the requested name is
	// the group's for every member).
	ModelUpstream *string `json:"model_upstream"`
	Prompt        int64   `json:"prompt_tokens"`
	Completion    int64   `json:"completion_tokens"`
	Cached        int64   `json:"cached_tokens"`
	CacheWrite    int64   `json:"cache_write_tokens"`
	CostMicro     int64   `json:"cost_micro"`
	Streamed      bool    `json:"streamed"`
	Estimated     bool    `json:"usage_estimated"`
	Status        int     `json:"status_code"`
	LatencyMS     int64   `json:"latency_ms"`
	Error         *string `json:"error"`
}

// UsageDetail returns one page of per-request rows, newest first.
func UsageDetail(db *sql.DB, f *UsageFilter, page, pageSize int) (int64, []UsageRow, error) {
	if err := f.scopedErr(); err != nil {
		return 0, nil, err
	}
	where, args := f.where()
	var total int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_log u`+where, args...).Scan(&total); err != nil {
		return 0, nil, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 50
	}
	rows, err := db.Query(`SELECT u.id, u.ts, COALESCE(us.username,''), COALESCE(k.name,''),
			(u.api_key_id IS NOT NULL AND u.api_key_id != 0), p.name,
			u.client_ip, u.session_id, u.endpoint, u.model_requested, u.model_upstream,
			u.prompt_tokens, u.completion_tokens, u.cached_tokens, u.cache_write_tokens,
			u.cost_total, u.streamed, u.usage_estimated, u.status_code, u.latency_ms, u.error
		FROM usage_log u
		LEFT JOIN users us ON us.id = u.user_id
		LEFT JOIN api_keys k ON k.id = u.api_key_id
		LEFT JOIN peers p ON p.id = u.fed_peer_id`+where+
		` ORDER BY u.id DESC LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return total, nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		var streamed, est, keyLocal int
		if err := rows.Scan(&r.ID, &r.TS, &r.Username, &r.KeyName, &keyLocal, &r.FedPeer, &r.ClientIP, &r.SessionID, &r.Endpoint,
			&r.ModelRequ, &r.ModelUpstream, &r.Prompt, &r.Completion, &r.Cached, &r.CacheWrite,
			&r.CostMicro, &streamed, &est, &r.Status, &r.LatencyMS, &r.Error); err != nil {
			return total, out, err
		}
		r.Streamed = streamed != 0
		r.Estimated = est != 0
		r.KeyLocal = keyLocal != 0
		out = append(out, r)
	}
	return total, out, rows.Err()
}

// SessionRecent reports whether a row for the session completed after
// since. Drain mode consults it as the fallback when the in-memory session
// tracker starts empty after a relay restart.
func SessionRecent(db *sql.DB, session string, since time.Time) (bool, error) {
	if session == "" {
		return false, nil
	}
	var one int
	err := db.QueryRow(`SELECT 1 FROM usage_log WHERE session_id=? AND ts>? LIMIT 1`,
		session, since.UTC().Format(time.RFC3339)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UserLastUsed is one user's most recent completed request.
type UserLastUsed struct {
	User     string `json:"user"`
	LastUsed string `json:"last_used"`
}

// LastUsedByUser returns each user's most recent usage row after since,
// newest first (admin live panel: who used the relay recently). Covered by
// idx_usage_user_ts.
func LastUsedByUser(db *sql.DB, since time.Time) ([]UserLastUsed, error) {
	rows, err := db.Query(`SELECT u.username, MAX(l.ts) FROM usage_log l
		JOIN users u ON u.id = l.user_id
		WHERE l.ts > ? AND l.user_id IS NOT NULL
		GROUP BY l.user_id, u.username ORDER BY MAX(l.ts) DESC`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserLastUsed{}
	for rows.Next() {
		var u UserLastUsed
		if err := rows.Scan(&u.User, &u.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
