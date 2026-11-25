// Usage data retention (plan 2026-11-25-usage-retention-v1). usage_log is
// the only table that grows without bound; the operator can cap it with a
// retention setting (0 = keep forever, the default: upgrading must never
// delete history silently). The purge deletes in small batches, each its
// own short transaction — one big DELETE would hold the SQLite write lock
// for seconds and starve the small connection pool (the 504 lesson from
// the fed-stats query), which is exactly what batching avoids.
package store

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// SettingUsageRetentionDays is the settings key holding the retention
// window in days: "0" (or absent) keeps rows forever.
const SettingUsageRetentionDays = "usage_retention_days"

// Retention bounds accepted from the API: 0 = forever; otherwise at least
// a month (shorter windows would eat the stats pages' own history) and at
// most ten years (a day count that big is a typo, not a policy).
const (
	RetentionForever = 0
	retentionMinDays = 30
	retentionMaxDays = 3650
)

// purgeBatch is the rows-per-DELETE batch. Small enough that each
// statement stays milliseconds even on weak hardware, large enough that a
// big backlog drains in seconds.
const purgeBatch = 1000

// ValidRetentionDays reports whether days is an accepted retention value.
func ValidRetentionDays(days int) bool {
	return days == RetentionForever || (days >= retentionMinDays && days <= retentionMaxDays)
}

// RetentionDays reads the configured retention. Unset or unparseable
// values read as forever: a junk setting must never cause deletion.
func RetentionDays(db *sql.DB) (int, error) {
	v, err := GetSetting(db, SettingUsageRetentionDays)
	if errors.Is(err, ErrNotFound) {
		return RetentionForever, nil
	}
	if err != nil {
		return 0, err
	}
	d, perr := strconv.Atoi(v)
	if perr != nil || d < 0 {
		return RetentionForever, nil
	}
	return d, nil
}

// SetRetentionDays stores the retention setting (validated).
func SetRetentionDays(db *sql.DB, days int) error {
	if !ValidRetentionDays(days) {
		return ErrInvalid
	}
	return SetSetting(db, SettingUsageRetentionDays, strconv.Itoa(days))
}

// RetentionCutoff returns the exclusive upper bound for deletion ("":
// nothing may be deleted) — rows strictly older than now-minus-days are
// purged, so a row stamped exactly at the cutoff survives. days <= 0
// means forever. ts is UTC RFC3339 text, so the comparison is
// lexicographic and exact.
func RetentionCutoff(now time.Time, days int) string {
	if days <= RetentionForever {
		return ""
	}
	return now.UTC().AddDate(0, 0, -days).Format(time.RFC3339)
}

// PurgeOldUsage deletes usage rows with ts < cutoff in batches of
// purgeBatch rows, each DELETE its own transaction. maxBatches caps the
// work per call (0 = drain everything). It reports the rows deleted and
// whether the cutoff still matches rows (more == the next pass has work
// left). An empty cutoff deletes nothing.
func PurgeOldUsage(db *sql.DB, cutoff string, maxBatches int) (deleted int64, more bool, err error) {
	if cutoff == "" {
		return 0, false, nil
	}
	for batch := 0; maxBatches == 0 || batch < maxBatches; batch++ {
		res, err := db.Exec(`DELETE FROM usage_log WHERE id IN
			(SELECT id FROM usage_log WHERE ts < ? LIMIT ?)`, cutoff, purgeBatch)
		if err != nil {
			return deleted, true, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, true, err
		}
		deleted += n
		if n < purgeBatch {
			return deleted, false, nil
		}
	}
	// Batch budget spent on a full batch: there may well be more.
	return deleted, true, nil
}

// PurgeByRetention reads the retention setting and purges accordingly;
// the whole enforcement point, shared by the daily loop and the settings
// PUT. Returns rows deleted (0 when retention is forever).
func PurgeByRetention(db *sql.DB, maxBatches int) (int64, bool, error) {
	days, err := RetentionDays(db)
	if err != nil {
		return 0, false, err
	}
	return PurgeOldUsage(db, RetentionCutoff(time.Now(), days), maxBatches)
}
