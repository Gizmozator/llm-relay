-- Drain mode: when set, the backend refuses NEW sessions (graceful
-- shutdown: conversations that are currently running finish first).
ALTER TABLE backends ADD COLUMN draining INTEGER NOT NULL DEFAULT 0;
