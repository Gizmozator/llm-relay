-- 0005: password-change timestamp for JWT invalidation.
-- Auth rejects tokens issued before this moment, so rotating a password
-- revokes previously issued sessions immediately. NULL = never changed
-- (tokens from before this migration stay valid until they expire).
ALTER TABLE users ADD COLUMN pass_changed_at TEXT;
