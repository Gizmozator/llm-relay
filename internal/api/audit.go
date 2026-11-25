// Audit trail plumbing (plan 2026-11-25-audit-log-v1): the write helper
// every mutation handler calls after success, and the admin-only read
// endpoint. The store exposes insert + query only — nothing in the
// codebase updates or deletes audit rows.
package api

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
)

// actorName resolves the acting user's username from the JWT claims (a
// vanished account falls back to user#<id> — still attributable).
func (s *Server) actorName(c *auth.Claims) string {
	if c == nil {
		return store.AuditSystem
	}
	if u, err := store.UserByID(s.DB, c.UserID); err == nil {
		return u.Username
	}
	return "user#" + strconv.FormatInt(c.UserID, 10)
}

// audit appends one entry for the acting user. The actor is the account's
// username resolved NOW (a snapshot in the row): a later rename or delete
// cannot rewrite history. A failed insert never fails the mutation it
// records: it is logged loudly and the request proceeds.
func (s *Server) audit(c *auth.Claims, action, target, detail string) {
	actor := s.actorName(c)
	if err := store.InsertAudit(s.DB, actor, action, target, detail); err != nil {
		log.Printf("AUDIT: insert %s %q failed: %v", action, target, err)
	}
}

// auditSystem appends a machine-channel entry (no human behind it).
func (s *Server) auditSystem(action, target, detail string) {
	if err := store.InsertAudit(s.DB, store.AuditSystem, action, target, detail); err != nil {
		log.Printf("AUDIT: insert %s %q failed: %v", action, target, err)
	}
}

// AdminGetAudit serves GET /api/admin/audit (admin only): the newest-first
// page of the trail. Filters: category (action prefix, e.g. "backend"),
// actor (exact username), q (substring of actor/action/target/detail),
// page (1-based), page_size (default 50, max 500). Read-only by design —
// there is no write, modify or delete endpoint for audit entries.
func (s *Server) AdminGetAudit(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	q := r.URL.Query()
	page := 1
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "page must be a positive integer")
			return
		}
		page = n
	}
	size := 50
	if v := q.Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeErr(w, http.StatusBadRequest, "page_size must be 1..500")
			return
		}
		size = n
	}
	entries, total, err := store.QueryAudit(s.DB, store.AuditFilter{
		Category: q.Get("category"),
		Actor:    q.Get("actor"),
		Q:        q.Get("q"),
		Limit:    size,
		Offset:   (page - 1) * size,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries, "total": total, "page": page, "page_size": size,
	})
}
