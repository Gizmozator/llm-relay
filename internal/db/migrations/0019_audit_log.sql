-- 0019: append-only audit log (plan 2026-11-25-audit-log-v1).
--
-- One row per state change or machine-channel event: who (username
-- SNAPSHOT, so a rename or deletion cannot rewrite history), when, what
-- (action = "<category>.<verb>"), the human name of the target and a
-- short detail of what changed. The application layer exposes insert and
-- read only — no code path updates or deletes these rows.
CREATE TABLE audit_log (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  ts     TEXT NOT NULL,             -- UTC RFC3339
  actor  TEXT NOT NULL,             -- username snapshot, or "system"
  action TEXT NOT NULL,             -- e.g. backend.create, fed.peer.connect
  target TEXT NOT NULL DEFAULT '',  -- human name of what changed
  detail TEXT NOT NULL DEFAULT ''   -- what changed (never secrets)
);

CREATE INDEX idx_audit_ts ON audit_log(ts DESC);
CREATE INDEX idx_audit_action ON audit_log(action);
