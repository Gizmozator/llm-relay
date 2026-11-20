-- 0004: per-user price multiplier (admin-only, default 1).
-- Applied to the computed cost at record time, like model prices: changing
-- the multiplier never rewrites existing usage rows.
ALTER TABLE users ADD COLUMN price_multiplier REAL NOT NULL DEFAULT 1;
