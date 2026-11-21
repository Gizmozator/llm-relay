-- 0009: measured prompt-processing (prefill) speed from the benchmark.
ALTER TABLE models ADD COLUMN bench_pp_tok_per_s REAL NOT NULL DEFAULT 0;
