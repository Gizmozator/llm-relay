-- 0001: initial schema — users, keys, backends, model catalog, usage log, settings.

CREATE TABLE users (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  username   TEXT NOT NULL UNIQUE,
  pass_hash  TEXT NOT NULL,
  role       TEXT NOT NULL CHECK(role IN ('admin','user')),
  disabled   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);

CREATE TABLE api_keys (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,
  key_hash     TEXT NOT NULL UNIQUE,
  key_prefix   TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  revoked_at   TEXT,
  last_used_at TEXT
);

CREATE TABLE backends (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  name             TEXT NOT NULL UNIQUE,
  base_url         TEXT NOT NULL,
  api_key          TEXT,
  enabled          INTEGER NOT NULL DEFAULT 1,
  last_probe_at    TEXT,
  last_probe_error TEXT,
  server_version   TEXT,
  created_at       TEXT NOT NULL
);

CREATE TABLE models (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  backend_id        INTEGER NOT NULL REFERENCES backends(id),
  upstream_model    TEXT NOT NULL,
  name              TEXT NOT NULL UNIQUE,
  enabled           INTEGER NOT NULL DEFAULT 1,
  price_in          INTEGER NOT NULL DEFAULT 0,
  price_out         INTEGER NOT NULL DEFAULT 0,
  price_cache_read  INTEGER NOT NULL DEFAULT 0,
  price_cache_write INTEGER NOT NULL DEFAULT 0,
  UNIQUE(backend_id, upstream_model)
);

CREATE TABLE usage_log (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                 TEXT NOT NULL,
  user_id            INTEGER,
  api_key_id         INTEGER,
  client_ip          TEXT,
  session_id         TEXT,
  endpoint           TEXT NOT NULL,
  model_id           INTEGER,
  model_requested    TEXT,
  model_upstream     TEXT,
  prompt_tokens      INTEGER NOT NULL DEFAULT 0,
  completion_tokens  INTEGER NOT NULL DEFAULT 0,
  cached_tokens      INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  cost_in            INTEGER NOT NULL DEFAULT 0,
  cost_out           INTEGER NOT NULL DEFAULT 0,
  cost_cache         INTEGER NOT NULL DEFAULT 0,
  cost_write         INTEGER NOT NULL DEFAULT 0,
  cost_total         INTEGER NOT NULL DEFAULT 0,
  streamed           INTEGER NOT NULL DEFAULT 0,
  usage_estimated    INTEGER NOT NULL DEFAULT 0,
  status_code        INTEGER NOT NULL DEFAULT 0,
  latency_ms         INTEGER NOT NULL DEFAULT 0,
  error              TEXT
);

CREATE INDEX idx_usage_ts      ON usage_log(ts);
CREATE INDEX idx_usage_key_ts  ON usage_log(api_key_id, ts);
CREATE INDEX idx_usage_user_ts ON usage_log(user_id, ts);
CREATE INDEX idx_usage_session ON usage_log(session_id, ts);

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

INSERT INTO settings(key, value) VALUES('currency', 'EUR');
