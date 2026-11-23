-- 0011: federation groundwork (plan 2026-11-22-relay-federation, task 1):
-- peer server tokens + the per-model federation sharing switch.
--
-- peers: the hub-side registry of server tokens a spoke dials in with.
-- Mirrors the api_keys pattern: only the SHA-256 hash is stored, the raw
-- token is shown once at create/rotate. No prefix column: unlike API keys
-- the token is never re-displayed after creation, so there is nothing to
-- render. No origin column either: every row task 1 writes is created on
-- this instance ('local' would be the only value any code path produces);
-- if task 2's remote-peer bookkeeping needs it, a plain ADD COLUMN adds it
-- then.
CREATE TABLE peers (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL,
  owner_user_id INTEGER NOT NULL REFERENCES users(id),
  key_hash      TEXT NOT NULL UNIQUE,
  enabled       INTEGER NOT NULL DEFAULT 1,
  -- last_seen/version are written by TouchPeer once the WS channel (task 2)
  -- reports peer liveness and instance version; empty until then.
  last_seen     TEXT NOT NULL DEFAULT '',
  version       TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL
);

-- Per-model federation visibility (plan Open questions): off (default:
-- share nothing) | all (every peer) | allowlist (fed_allowlist rows only).
-- Plain ADD COLUMN is legal here (unlike 0010's UNIQUE rebuild): SQLite
-- allows ADD COLUMN with NOT NULL + CHECK when the default satisfies the
-- check, and every existing row takes the default 'off'.
ALTER TABLE models ADD COLUMN fed_visibility TEXT NOT NULL DEFAULT 'off'
  CHECK (fed_visibility IN ('off','all','allowlist'));

-- The allowlist side of fed_visibility. Both FKs cascade so deleting a
-- peer or a model prunes the list; (model_id, peer_id) is the natural key
-- and its PK index serves the per-model and per-peer lookups.
CREATE TABLE fed_allowlist (
  model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  peer_id  INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
  PRIMARY KEY (model_id, peer_id)
);
