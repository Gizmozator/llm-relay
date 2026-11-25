// Store-level tests for usage retention (plan 2026-11-25-usage-retention-v1):
// the setting round-trip and validation, the cutoff math, and the batched
// purge — boundary, batching, caps and the forever no-op.
package store_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"llm-relay/internal/store"
)

// insertAt writes one minimal usage row stamped at ts.
func insertAt(t *testing.T, d *sql.DB, ts string) {
	t.Helper()
	if err := store.InsertUsage(d, &store.UsageEvent{TS: ts, Endpoint: "chat", Prompt: 1}); err != nil {
		t.Fatal(err)
	}
}

func countUsage(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetentionSettingRoundTrip(t *testing.T) {
	d := migratedDB(t)
	if got, err := store.RetentionDays(d); err != nil || got != store.RetentionForever {
		t.Fatalf("unset retention = %d, %v; want 0, nil", got, err)
	}
	if err := store.SetRetentionDays(d, 90); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.RetentionDays(d); got != 90 {
		t.Fatalf("retention = %d; want 90", got)
	}
	if err := store.SetRetentionDays(d, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.RetentionDays(d); got != 0 {
		t.Fatalf("retention = %d; want 0", got)
	}
	for _, bad := range []int{-1, 1, 29, 3651} {
		if err := store.SetRetentionDays(d, bad); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("SetRetentionDays(%d) = %v; want ErrInvalid", bad, err)
		}
	}
	// Junk in the setting must read as forever, never as a deletion window.
	if err := store.SetSetting(d, store.SettingUsageRetentionDays, "banana"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.RetentionDays(d); got != store.RetentionForever {
		t.Fatalf("junk retention = %d; want 0 (fail-safe forever)", got)
	}
}

func TestRetentionCutoff(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if got := store.RetentionCutoff(now, 0); got != "" {
		t.Fatalf("forever cutoff = %q; want empty", got)
	}
	if got := store.RetentionCutoff(now, -5); got != "" {
		t.Fatalf("negative cutoff = %q; want empty", got)
	}
	if got := store.RetentionCutoff(now, 30); got != "2026-10-26T12:00:00Z" {
		t.Fatalf("30-day cutoff = %q; want 2026-10-26T12:00:00Z", got)
	}
}

func TestPurgeOldUsageBoundary(t *testing.T) {
	d := migratedDB(t)
	cutoff := "2026-01-01T00:00:00Z"
	insertAt(t, d, "2025-12-31T23:59:59Z") // older: goes
	insertAt(t, d, "2026-01-01T00:00:00Z") // exactly at cutoff: strict < keeps it
	insertAt(t, d, "2026-01-02T00:00:00Z") // newer: stays
	deleted, more, err := store.PurgeOldUsage(d, cutoff, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || more {
		t.Fatalf("purge = %d deleted, more=%v; want 1, false", deleted, more)
	}
	if n := countUsage(t, d); n != 2 {
		t.Fatalf("%d rows left; want 2", n)
	}
	// Empty cutoff deletes nothing, whatever is in the table.
	if deleted, _, err := store.PurgeOldUsage(d, "", 0); err != nil || deleted != 0 {
		t.Fatalf("empty cutoff purge = %d, %v; want 0, nil", deleted, err)
	}
}

func TestPurgeOldUsageBatching(t *testing.T) {
	d := migratedDB(t)
	cutoff := "2026-01-01T00:00:00Z"
	const rows = 2500
	for i := 0; i < rows; i++ {
		insertAt(t, d, "2025-12-01T00:00:00Z")
	}
	insertAt(t, d, "2026-06-01T00:00:00Z") // recent: must survive the full drain

	// Capped call: two batches of 1000, backlog reported.
	deleted, more, err := store.PurgeOldUsage(d, cutoff, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2000 || !more {
		t.Fatalf("capped purge = %d, more=%v; want 2000, true", deleted, more)
	}
	// Uncapped call drains the rest and reports no more.
	deleted, more, err = store.PurgeOldUsage(d, cutoff, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 500 || more {
		t.Fatalf("drain purge = %d, more=%v; want 500, false", deleted, more)
	}
	if n := countUsage(t, d); n != 1 {
		t.Fatalf("%d rows left; want the one recent row", n)
	}
}

func TestPurgeByRetention(t *testing.T) {
	d := migratedDB(t)
	old := time.Now().UTC().AddDate(0, 0, -400).Format(time.RFC3339)
	recent := time.Now().UTC().AddDate(0, 0, -10).Format(time.RFC3339)
	insertAt(t, d, old)
	insertAt(t, d, recent)

	// Forever (default): nothing goes.
	if n, _, err := store.PurgeByRetention(d, 0); err != nil || n != 0 {
		t.Fatalf("forever purge = %d, %v; want 0, nil", n, err)
	}
	if countUsage(t, d) != 2 {
		t.Fatal("forever retention deleted rows")
	}
	// 30 days: the 400-day row goes, the 10-day row stays.
	if err := store.SetRetentionDays(d, 30); err != nil {
		t.Fatal(err)
	}
	n, _, err := store.PurgeByRetention(d, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || countUsage(t, d) != 1 {
		t.Fatalf("30-day purge = %d deleted; want exactly the old row", n)
	}
}
