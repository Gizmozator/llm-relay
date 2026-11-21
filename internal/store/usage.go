// Usage recording and aggregation queries.
package store

import (
	"database/sql"
	"fmt"
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
}

func InsertUsage(db *sql.DB, e *UsageEvent) error {
	_, err := db.Exec(`INSERT INTO usage_log(ts, user_id, api_key_id, client_ip, session_id, endpoint,
			model_id, model_requested, model_upstream,
			prompt_tokens, completion_tokens, cached_tokens, cache_write_tokens,
			cost_in, cost_out, cost_cache, cost_write, cost_total,
			streamed, usage_estimated, status_code, latency_ms, error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TS, e.UserID, e.KeyID, e.ClientIP, nullable(e.SessionID), e.Endpoint,
		e.ModelID, e.ModelRequ, e.ModelUpstream,
		e.Prompt, e.Completion, e.Cached, e.CacheWrite,
		e.CostIn, e.CostOut, e.CostCache, e.CostWrite, e.CostTotal,
		b2i(e.Streamed), b2i(e.Estimated), e.Status, e.LatencyMS, nullable(e.Error))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UsageFilter scopes an aggregation query. Zero values mean "no filter".
type UsageFilter struct {
	UserID int64 // 0 = unscoped (caller must have set Admin or its own UserID)
	KeyID  int64
	Model  string // exact model_requested match, "" = any
	From   string // YYYY-MM-DD inclusive, "" = open
	To     string // YYYY-MM-DD inclusive, "" = open
	Since  string // RFC3339 UTC inclusive, "" = open (live-window queries)
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
		args = append(args, f.From+"T00:00:00Z")
	}
	if f.To != "" {
		conds = append(conds, `u.ts < datetime(?, '+1 day')`)
		args = append(args, f.To+"T00:00:00Z")
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + join(conds, " AND "), args
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
	"day":      `substr(u.ts, 1, 10)`,
	"user":     `COALESCE(us.username, '(deleted user)')`,
	"key":      `COALESCE(k.name, '(deleted key)')`,
	"model":    `COALESCE(u.model_requested, '(unknown)')`,
	"ip":       `COALESCE(u.client_ip, '(unknown)')`,
	"endpoint": `u.endpoint`,
	"session":  `COALESCE(substr(u.session_id, 1, 12), '(none)')`,
}

// ValidGroupBy reports whether groupBy is a supported bucket dimension.
// Handlers validate with it so a bad value is a 400, not a store error.
func ValidGroupBy(groupBy string) bool {
	_, ok := groupExprs[groupBy]
	return ok
}

// SummarizeUsage returns totals plus one bucket per group_by dimension.
func SummarizeUsage(db *sql.DB, f *UsageFilter, groupBy string) (UsageGroup, []UsageGroup, error) {
	expr, ok := groupExprs[groupBy]
	if !ok {
		return UsageGroup{}, nil, fmt.Errorf("bad group_by %q", groupBy)
	}
	where, args := f.where()

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
		LEFT JOIN users us ON us.id = u.user_id`+where+
		` GROUP BY gk ORDER BY gk`, args...)
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

// LiveBucket is one minute of the live dashboard window.
type LiveBucket struct {
	Minute     string `json:"minute"`
	Requests   int64  `json:"requests"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Cached     int64  `json:"cached_tokens"`
	CacheWrite int64  `json:"cache_write_tokens"`
	CostMicro  int64  `json:"cost_micro"`
}

// LiveBuckets aggregates the last `minutes` minutes into one bucket per
// minute, zero-filled so the series is continuous (the chart never has to
// guess gaps). `now` anchors the window; rows are bucketed by the minute
// prefix of their UTC RFC3339 ts.
func LiveBuckets(db *sql.DB, f *UsageFilter, now time.Time, minutes int) ([]LiveBucket, error) {
	start := now.UTC().Truncate(time.Minute).Add(time.Duration(-minutes+1) * time.Minute)
	f.Since = start.Format(time.RFC3339)
	where, args := f.where()

	rows, err := db.Query(`SELECT substr(u.ts,1,16) AS minute, COUNT(*),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_write_tokens),0),
			COALESCE(SUM(cost_total),0)
		FROM usage_log u`+where+` GROUP BY minute ORDER BY minute`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byMinute := map[string]LiveBucket{}
	for rows.Next() {
		var b LiveBucket
		if err := rows.Scan(&b.Minute, &b.Requests, &b.Prompt, &b.Completion,
			&b.Cached, &b.CacheWrite, &b.CostMicro); err != nil {
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

// UsageRow is one detailed usage record for the detail table.
type UsageRow struct {
	ID         int64   `json:"id"`
	TS         string  `json:"ts"`
	Username   string  `json:"username"`
	KeyName    string  `json:"key_name"`
	ClientIP   *string `json:"client_ip"`
	SessionID  *string `json:"session_id"`
	Endpoint   string  `json:"endpoint"`
	ModelRequ  *string `json:"model_requested"`
	Prompt     int64   `json:"prompt_tokens"`
	Completion int64   `json:"completion_tokens"`
	Cached     int64   `json:"cached_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	CostMicro  int64   `json:"cost_micro"`
	Streamed   bool    `json:"streamed"`
	Estimated  bool    `json:"usage_estimated"`
	Status     int     `json:"status_code"`
	LatencyMS  int64   `json:"latency_ms"`
	Error      *string `json:"error"`
}

// UsageDetail returns one page of per-request rows, newest first.
func UsageDetail(db *sql.DB, f *UsageFilter, page, pageSize int) (int64, []UsageRow, error) {
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
			u.client_ip, u.session_id, u.endpoint, u.model_requested,
			u.prompt_tokens, u.completion_tokens, u.cached_tokens, u.cache_write_tokens,
			u.cost_total, u.streamed, u.usage_estimated, u.status_code, u.latency_ms, u.error
		FROM usage_log u
		LEFT JOIN users us ON us.id = u.user_id
		LEFT JOIN api_keys k ON k.id = u.api_key_id`+where+
		` ORDER BY u.id DESC LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return total, nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		var streamed, est int
		if err := rows.Scan(&r.ID, &r.TS, &r.Username, &r.KeyName, &r.ClientIP, &r.SessionID, &r.Endpoint,
			&r.ModelRequ, &r.Prompt, &r.Completion, &r.Cached, &r.CacheWrite,
			&r.CostMicro, &streamed, &est, &r.Status, &r.LatencyMS, &r.Error); err != nil {
			return total, out, err
		}
		r.Streamed = streamed != 0
		r.Estimated = est != 0
		out = append(out, r)
	}
	return total, out, rows.Err()
}
