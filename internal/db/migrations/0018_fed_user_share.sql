-- 0018: per-user share allowlist + "owner only" mode (ticket #21, plan
-- 2026-11-25-share-per-user-v1).
--
-- Two schema changes:
--   1. fed_allowlist rows name USERS, not peers: a spoke may use a model
--      when its OWNER user is on the list ("share this model with these
--      accounts' relays" — it survives peer renames and re-adds, and one
--      user with several boxes shares once). Existing peer rows migrate to
--      the peer's owner; rows of ownerless peers are dropped — there is no
--      account to attribute them to.
--   2. fed_visibility gains 'owner': the model is shared only with its
--      backend's fairness owner (ticket #12) — that account's relays may
--      pull it, nobody else. The column CHECK must be widened, which
--      SQLite can only do by rebuilding models.
--
-- The runner executes each file in ONE transaction with foreign keys on
-- (DSN pragma), and PRAGMA defer_foreign_keys is a documented no-op inside
-- a transaction when FK enforcement is already on — so the rebuild drops
-- the two leaf children (fed_allowlist, model_group_members) after saving
-- their rows to TEMP tables, rebuilds models, then recreates the children
-- and reinserts. TEMP tables live on the connection, which is the whole
-- migration (the same trick 0010 uses for the AUTOINCREMENT high-water).

-- Save the children. The allowlist rows map to users here (DISTINCT: one
-- owner with several peers shares once).
CREATE TEMP TABLE mgm_saved AS SELECT * FROM model_group_members;
CREATE TEMP TABLE fa_saved AS
  SELECT DISTINCT fa.model_id AS model_id, p.owner_user_id AS user_id
  FROM fed_allowlist fa JOIN peers p ON p.id = fa.peer_id
  WHERE p.owner_user_id IS NOT NULL;

DROP TABLE fed_allowlist;
DROP TABLE model_group_members;

-- models rebuild: byte-identical to the live schema except the widened
-- fed_visibility CHECK. usage_log has no FK to models (0010's note still
-- holds), so only the two children above had to move.
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
  fed_visibility     TEXT NOT NULL DEFAULT 'off'
    CHECK (fed_visibility IN ('off','all','allowlist','owner')),
  UNIQUE(backend_id, upstream_model),
  UNIQUE(backend_id, name)
);

INSERT INTO models_new (id, backend_id, upstream_model, name, visibility, enabled,
  price_in, price_out, price_cache_read, price_cache_write, context_length,
  concurrency_limit, bench_tok_per_s, bench_at, busy_threshold_pct,
  bench_pp_tok_per_s, fed_visibility)
SELECT id, backend_id, upstream_model, name, visibility, enabled,
  price_in, price_out, price_cache_read, price_cache_write, context_length,
  concurrency_limit, bench_tok_per_s, bench_at, busy_threshold_pct,
  bench_pp_tok_per_s, fed_visibility
FROM models;

-- Save the AUTOINCREMENT high-water before the rebuild (0010's rationale:
-- DROP deletes the sqlite_sequence row; orphaned usage_log rows have no FK
-- and must never see a reused id).
CREATE TEMP TABLE models_seq_saved AS
  SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='models'), 0) AS seq;

DROP TABLE models;

ALTER TABLE models_new RENAME TO models;

INSERT INTO sqlite_sequence(name, seq)
  SELECT 'models', seq FROM models_seq_saved
  WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='models');
UPDATE sqlite_sequence AS s
  SET seq = (SELECT MAX(sv.seq, s.seq) FROM models_seq_saved AS sv)
  WHERE s.name='models';
DROP TABLE temp.models_seq_saved;

CREATE INDEX idx_models_name ON models(name);

-- Recreate the children (original definitions) and reinsert the saved rows.
CREATE TABLE model_group_members (
  group_id INTEGER NOT NULL REFERENCES model_groups(id),
  model_id INTEGER NOT NULL UNIQUE REFERENCES models(id),
  weight   REAL NOT NULL DEFAULT 1,
  position INTEGER NOT NULL DEFAULT 0,
  prev_visibility TEXT NOT NULL DEFAULT 'public'
);
CREATE INDEX idx_model_group_members_group ON model_group_members(group_id);
INSERT INTO model_group_members SELECT * FROM mgm_saved;
DROP TABLE temp.mgm_saved;

-- The new allowlist: user ids, cascading on user delete (a deleted user's
-- shares die with the account).
CREATE TABLE fed_allowlist (
  model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (model_id, user_id)
);
INSERT INTO fed_allowlist (model_id, user_id)
  SELECT model_id, user_id FROM fa_saved;
DROP TABLE temp.fa_saved;
