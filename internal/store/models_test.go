// Model creation defaults: both insert paths (single create and the
// backend-sync bulk add) must store the real busy threshold default (80),
// not the raw column default 0.
package store_test

import (
	"testing"

	"llm-relay/internal/db"
	"llm-relay/internal/store"
)

func TestModelBusyThresholdDefault(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	b, err := store.CreateBackend(d, "eng", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "single", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.BusyThresholdPct != 80 {
		t.Errorf("CreateModel threshold = %d, want 80", m.BusyThresholdPct)
	}

	created, skipped, err := store.AddModels(d, b.ID, []string{"synced-a", "synced-a", m.UpstreamModel})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || len(skipped) != 2 {
		t.Fatalf("AddModels created=%d skipped=%d, want 1/2", len(created), len(skipped))
	}
	if created[0].BusyThresholdPct != 80 {
		t.Errorf("AddModels threshold = %d, want 80", created[0].BusyThresholdPct)
	}
}

// A failed prefill pass (nil PP) must update TG and bench_at while leaving
// the previously stored PP measurement intact.
func TestUpdateModelBenchKeepsPP(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/bench.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	b, err := store.CreateBackend(d, "eng", "http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateModel(d, b.ID, "bench-pp", "")
	if err != nil {
		t.Fatal(err)
	}
	pp := 1234.5
	if err := store.UpdateModelBench(d, m.ID, 40.0, &pp, "2026-11-21T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateModelBench(d, m.ID, 42.0, nil, "2026-11-21T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	got, err := store.ModelByID(d, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BenchTokPerS != 42.0 || got.BenchPPTokPerS != 1234.5 || got.BenchAt != "2026-11-21T11:00:00Z" {
		t.Errorf("after nil-PP run: tg=%v pp=%v at=%q, want 42 / 1234.5 kept / newer bench_at",
			got.BenchTokPerS, got.BenchPPTokPerS, got.BenchAt)
	}
}
