-- 0015: remote backends (plan 2026-11-24-remote-backend-v1): the connect
-- token a remote box dials /remote/v1/connect with.
--
-- Only backend rows whose base_url is remote://<id> carry a token; every
-- other row keeps NULL. The UNIQUE index treats NULLs as distinct (SQLite),
-- so plain and federation backends are unaffected. The raw rb- token is
-- shown once at create/rotate; only the SHA-256 hash is stored (same rule as
-- api_keys and peers).
ALTER TABLE backends ADD COLUMN remote_token_hash TEXT;
CREATE UNIQUE INDEX backends_remote_token ON backends(remote_token_hash);
