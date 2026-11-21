-- 0008: store the default busy threshold explicitly (owner: new models
-- should show 80, not 0). SQLite cannot ALTER a column default, so the
-- column default stays 0 and CreateModel inserts 80 explicitly; 0 keeps
-- its meaning of "default" for anything that still holds it.
UPDATE models SET busy_threshold_pct=80 WHERE busy_threshold_pct=0;
