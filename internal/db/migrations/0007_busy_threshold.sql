-- 0007: per-model busy threshold for the agent free/busy verdict.
-- Mean busy % at or above which /v1/utilization and the MCP tool report
-- "busy"; 0 = unset (the compiled default of 80 % applies). The active-slots
-- rule (concurrency_limit) is unchanged and orthogonal.
ALTER TABLE models ADD COLUMN busy_threshold_pct INTEGER NOT NULL DEFAULT 0;
