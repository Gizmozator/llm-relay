-- 0010: model groups — a public name backed by several model rows, one per
-- backend replica, with session-affine distribution (plan 2026-11-22).
--
-- models rebuild (SQLite cannot drop a column UNIQUE in place):
--   * name uniqueness relaxes from global to UNIQUE(backend_id, name): two
--     backends may each carry a row named qwen3-coder.
--   * visibility ('public'|'internal'): public rows are listed in /v1/models;
--     internal rows (group members) stay directly requestable but hidden.
--     Membership writes flip it in the store (UpdateGroup/DeleteGroup):
--     joining a group sets internal, leaving restores public. Existing rows
--     are all public here; they flip when a group claims them.
-- No indexes or triggers referenced models before the rebuild (usage_log has
-- no FK to models), so none need recreating. idx_models_name is new: every
-- proxied request resolves a name, so name lookups must not scan.
--
-- The rebuild runs before model_group_members is created: with foreign keys
-- on, DROP TABLE models would fail while a child table references it.

CREATE TABLE models_new (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  backend_id         INTEGER NOT NULL REFERENCES backends(id),
  upstream_model     TEXT NOT NULL,
  name               TEXT NOT NULL,
  visibility         TEXT NOT NULL DEFAULT 'public' CHECK(visibility IN ('public','internal')),
  enabled            INTEGER NOT NULL DEFAULT 1,
  price_in           INTEGER NOT NULL DEFAULT 0,
  price_out          INTEGER NOT NULL DEFAULT 0,
  price_cache_read   INTEGER NOT NULL DEFAULT 0,
  price_cache_write  INTEGER NOT NULL DEFAULT 0,
  context_length     INTEGER NOT NULL DEFAULT 0,
  concurrency_limit  INTEGER NOT NULL DEFAULT 0,
  bench_tok_per_s    REAL NOT NULL DEFAULT 0,
  bench_at           TEXT,
  busy_threshold_pct INTEGER NOT NULL DEFAULT 0,
  bench_pp_tok_per_s REAL NOT NULL DEFAULT 0,
  UNIQUE(backend_id, upstream_model),
  UNIQUE(backend_id, name)
);

INSERT INTO models_new (id, backend_id, upstream_model, name, visibility, enabled,
  price_in, price_out, price_cache_read, price_cache_write, context_length,
  concurrency_limit, bench_tok_per_s, bench_at, busy_threshold_pct, bench_pp_tok_per_s)
SELECT id, backend_id, upstream_model, name, 'public', enabled,
  price_in, price_out, price_cache_read, price_cache_write, context_length,
  concurrency_limit, bench_tok_per_s, bench_at, busy_threshold_pct, bench_pp_tok_per_s
FROM models;

-- Save the AUTOINCREMENT high-water mark before the rebuild: DROP TABLE
-- deletes the sqlite_sequence row, and the explicit-id copy below only
-- restores max(surviving id). If the top-id rows were deleted before
-- migrating, new inserts would reuse old ids and orphaned usage_log rows
-- (no FK to models) would re-attach to the wrong model.
CREATE TEMP TABLE models_seq_saved AS
  SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='models'), 0) AS seq;

DROP TABLE models;

ALTER TABLE models_new RENAME TO models;

-- Restore the high-water mark: max(saved seq, seq from the copied rows).
-- The copy leaves a 'models' row only when it copied explicit ids.
INSERT INTO sqlite_sequence(name, seq)
  SELECT 'models', seq FROM models_seq_saved
  WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='models');
UPDATE sqlite_sequence AS s
  SET seq = (SELECT MAX(sv.seq, s.seq) FROM models_seq_saved AS sv)
  WHERE s.name='models';
DROP TABLE temp.models_seq_saved;

CREATE INDEX idx_models_name ON models(name);

-- A group is a public name that routes to its members; the group owns the
-- name (store refuses group names colliding with public model names).
CREATE TABLE model_groups (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  name    TEXT NOT NULL UNIQUE,
  -- Default 0: a bare insert must not advertise a group that cannot serve.
  -- The store enforces "enabling requires members" on the enable
  -- transition; an enabled group may later be emptied (members removed or
  -- their models deleted) — that transient is deliberate: an empty group
  -- is neither advertised nor resolved (fall-through to same-name models),
  -- and name ownership is re-checked whenever a call moves the group back
  -- into serving.
  enabled INTEGER NOT NULL DEFAULT 0
);

-- One group per model (model_id UNIQUE): no nesting, no multi-group members.
-- weight seeds the rendezvous-hash pick; position is the admin display order.
CREATE TABLE model_group_members (
  group_id INTEGER NOT NULL REFERENCES model_groups(id),
  model_id INTEGER NOT NULL UNIQUE REFERENCES models(id),
  weight   REAL NOT NULL DEFAULT 1,
  position INTEGER NOT NULL DEFAULT 0
);

-- model_id is indexed by its UNIQUE constraint; group_id is the hot lookup
-- (member fetches on every group resolution, DeleteGroup), same reasoning as
-- idx_models_name.
CREATE INDEX idx_model_group_members_group ON model_group_members(group_id);
