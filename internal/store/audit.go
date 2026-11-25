// Append-only audit log (plan 2026-11-25-audit-log-v1): who changed what,
// when. The actor is a username SNAPSHOT taken at write time — renaming or
// deleting an account can never rewrite history. This file is the table's
// whole surface: insert and query. There is deliberately no update or
// delete function anywhere in the codebase.
package store

import (
	"database/sql"
	"strings"
	"time"
)

// AuditSystem is the actor for machine-channel events (no human behind
// them: a spoke connected, a remote backend dropped).
const AuditSystem = "system"

// AuditEntry is one audit row.
type AuditEntry struct {
	ID     int64  `json:"id"`
	TS     string `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// InsertAudit appends one entry, stamped now (UTC). Callers must treat a
// failure as non-fatal to the mutation it records — but should log it.
func InsertAudit(db *sql.DB, actor, action, target, detail string) error {
	_, err := db.Exec(`INSERT INTO audit_log(ts, actor, action, target, detail)
		VALUES(?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339), actor, action, target, detail)
	return err
}

// AuditFilter narrows a query. Category matches the "<category>." prefix
// of the action; Actor is an exact actor name; Q is a case-insensitive
// substring of actor, action, target or detail. Zero values = no filter.
type AuditFilter struct {
	Category string
	Actor    string
	Q        string
	Limit    int
	Offset   int
}

// QueryAudit returns the newest-first page plus the total row count behind
// the filter (for paging). Limit defaults to 50, capped at 500.
func QueryAudit(db *sql.DB, f AuditFilter) ([]AuditEntry, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Category != "" {
		where = append(where, "action LIKE ?")
		args = append(args, f.Category+".%")
	}
	if f.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, f.Actor)
	}
	if f.Q != "" {
		where = append(where, `(actor LIKE ? OR action LIKE ? OR target LIKE ? OR detail LIKE ?)`)
		q := "%" + f.Q + "%"
		args = append(args, q, q, q, q)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := db.Query(`SELECT id, ts, actor, action, target, detail FROM audit_log
		WHERE `+cond+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}
