-- 0002: optional display name for users (shown in dashboards next to username).

ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
