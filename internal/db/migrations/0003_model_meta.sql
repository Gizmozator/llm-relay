-- 0003: per-model metadata for the catalog view.
-- context_length / concurrency_limit are admin-set (upstreams do not report
-- them); bench_tok_per_s / bench_at hold the last admin benchmark result.
ALTER TABLE models ADD COLUMN context_length INTEGER NOT NULL DEFAULT 0;
ALTER TABLE models ADD COLUMN concurrency_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE models ADD COLUMN bench_tok_per_s REAL NOT NULL DEFAULT 0;
ALTER TABLE models ADD COLUMN bench_at TEXT;
