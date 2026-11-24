// Store-level tests for the live-window aggregation, using a fixed clock so
// bucket boundaries and labels are exact.
package store_test

import (
	"testing"
	"time"

	"llm-relay/internal/db"
	"llm-relay/internal/store"
)

func TestLiveBucketsFixedClock(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/live.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	seed := func(ts string, prompt, completion, cost int64) {
		err := store.InsertUsage(d, &store.UsageEvent{
			TS: ts, Endpoint: "chat", Prompt: prompt, Completion: completion,
			CostTotal: cost, Status: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Window anchor 14:03:30, 5 minutes → buckets 13:59 … 14:03.
	now := time.Date(2026, 11, 21, 14, 3, 30, 0, time.UTC)
	seed("2026-11-21T13:58:59Z", 1, 1, 1)    // before the window
	seed("2026-11-21T13:59:00Z", 10, 5, 100) // first bucket, boundary-inclusive
	seed("2026-11-21T13:59:30Z", 1, 1, 10)   // same bucket, must aggregate
	seed("2026-11-21T14:03:59Z", 7, 2, 50)   // last bucket
	seed("2026-11-21T14:04:00Z", 9, 9, 9)    // future minute, outside labels

	buckets, err := store.LiveBuckets(d, &store.UsageFilter{AllUsers: true}, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 5 {
		t.Fatalf("buckets = %d, want 5", len(buckets))
	}
	wantLabels := []string{
		"2026-11-21T13:59", "2026-11-21T14:00", "2026-11-21T14:01",
		"2026-11-21T14:02", "2026-11-21T14:03",
	}
	for i, want := range wantLabels {
		if buckets[i].Minute != want {
			t.Fatalf("bucket %d label = %q, want %q", i, buckets[i].Minute, want)
		}
	}
	first := buckets[0]
	if first.Requests != 2 || first.Prompt != 11 || first.Completion != 6 {
		t.Errorf("first bucket = %+v, want 2 reqs / 11 prompt / 6 completion", first)
	}
	last := buckets[4]
	if last.Requests != 1 {
		t.Errorf("last bucket = %+v, want 1 req", last)
	}
	for _, b := range buckets[1:4] {
		if b.Requests != 0 {
			t.Errorf("middle bucket should be zero-filled: %+v", b)
		}
	}
}

func TestBusyByModelFixedClock(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/busy.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	seed := func(ts string, latency int64, model string) {
		err := store.InsertUsage(d, &store.UsageEvent{
			TS: ts, Endpoint: "chat", ModelRequ: model, LatencyMS: latency, Status: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Window 13:59 … 14:03 (anchor 14:03:30, 5 minutes).
	now := time.Date(2026, 11, 21, 14, 3, 30, 0, time.UTC)
	seed("2026-11-21T14:00:30Z", 90_000, "m-a")  // spans 13:59 (60 s) + 14:00 (30 s)
	seed("2026-11-21T13:59:30Z", 120_000, "m-a") // starts before the window → clipped to 30 s
	seed("2026-11-21T14:01:00Z", 0, "m-a")       // zero latency contributes nothing
	seed("2026-11-21T14:02:30Z", 30_000, "m-b")  // fully inside 14:02

	busy, err := store.BusyByModel(d, &store.UsageFilter{AllUsers: true}, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(busy) != 2 {
		t.Fatalf("models = %d, want 2: %+v", len(busy), busy)
	}
	wantA := []int64{90_000, 30_000, 0, 0, 0}
	wantB := []int64{0, 0, 0, 30_000, 0}
	if busy[0].Model != "m-a" || busy[1].Model != "m-b" {
		t.Fatalf("models = %q, %q, want m-a, m-b", busy[0].Model, busy[1].Model)
	}
	for i := range wantA {
		if busy[0].Values[i] != wantA[i] {
			t.Errorf("m-a[%d] = %d, want %d", i, busy[0].Values[i], wantA[i])
		}
		if busy[1].Values[i] != wantB[i] {
			t.Errorf("m-b[%d] = %d, want %d", i, busy[1].Values[i], wantB[i])
		}
	}
}

func TestBusyByModelScoping(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/busy2.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	u1, err := store.CreateUser(d, "one", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := store.CreateUser(d, "two", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	insert := func(userID int64, model string) {
		err := store.InsertUsage(d, &store.UsageEvent{
			TS: now.Add(-30 * time.Second).Format(time.RFC3339), UserID: userID,
			Endpoint: "chat", ModelRequ: model, LatencyMS: 10_000, Status: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(u1.ID, "mine")
	insert(u2.ID, "theirs")

	busy, err := store.BusyByModel(d, &store.UsageFilter{UserID: u1.ID}, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(busy) != 1 || busy[0].Model != "mine" {
		t.Fatalf("scoped busy = %+v, want only mine", busy)
	}
}

func TestLiveBucketsScoping(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/live2.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	u1, err := store.CreateUser(d, "one", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := store.CreateUser(d, "two", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	insert := func(userID int64) {
		err := store.InsertUsage(d, &store.UsageEvent{
			TS: now.Add(-time.Minute).Format(time.RFC3339), UserID: userID,
			Endpoint: "chat", CostTotal: 42, Status: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(u1.ID)
	insert(u2.ID)

	buckets, err := store.LiveBuckets(d, &store.UsageFilter{UserID: u1.ID}, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	var reqs int64
	for _, b := range buckets {
		reqs += b.Requests
	}
	if reqs != 1 {
		t.Errorf("scoped requests = %d, want 1", reqs)
	}
}

// The "model" group-by aggregates by model_requested (COALESCE), so a
// model group's usage rolls up across its members automatically: rows
// served by different member rows (model_id) under one requested name
// must land in ONE bucket. This pins the Usage-page pool rollup — the
// dashboard needs no group-aware query, only the group name in
// model_requested (plan 2026-11-22, task 4).
func TestSummarizeUsageModelGroupRollup(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/rollup.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	seed := func(modelID int64, requested, upstream string, prompt, cost int64) {
		err := store.InsertUsage(d, &store.UsageEvent{
			TS: "2026-11-21T12:00:00Z", Endpoint: "chat",
			ModelID: modelID, ModelRequ: requested, ModelUpstream: upstream,
			Prompt: prompt, CostTotal: cost, Status: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Two members of group "pool" (same requested name, different member
	// rows and upstream ids) plus one standalone model row.
	seed(10, "pool", "qwen:fast", 100, 10)
	seed(11, "pool", "qwen:slow", 50, 5)
	seed(12, "solo", "other:1", 7, 7)

	totals, groups, err := store.SummarizeUsage(d, &store.UsageFilter{AllUsers: true}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 3 {
		t.Errorf("totals = %+v, want 3 requests", totals)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want pool + solo", groups)
	}
	var pool *store.UsageGroup
	for i := range groups {
		if groups[i].Key == "pool" {
			pool = &groups[i]
		}
	}
	if pool == nil || pool.Requests != 2 || pool.Prompt != 150 || pool.CostMicro != 15 {
		t.Errorf("pool group = %+v, want 2 requests / 150 prompt / 15 cost", pool)
	}

	// The detail rows keep the per-request identity: the group name as
	// requested, the member's upstream id for distinguishing members.
	total, rows, err := store.UsageDetail(d, &store.UsageFilter{AllUsers: true}, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("detail = %d/%d, want 3/3", total, len(rows))
	}
	upstreams := map[string]bool{}
	for _, r := range rows {
		if r.ModelRequ != nil && *r.ModelRequ == "pool" {
			if r.ModelUpstream == nil {
				t.Errorf("pool row %d has no model_upstream", r.ID)
			} else {
				upstreams[*r.ModelUpstream] = true
			}
		}
	}
	if !upstreams["qwen:fast"] || !upstreams["qwen:slow"] {
		t.Errorf("member upstream ids not surfaced: %v", upstreams)
	}
}

// Day buckets split at the viewer's local midnight when TZOffset is set
// (minutes east of UTC): rows near a UTC midnight land in the local day of
// the shifted clock. TZOffset == 0 must keep the plain UTC bucketing and
// bounds byte-identically (callers that do not send an offset are
// unaffected).
func TestSummarizeUsageDayLocalMidnight(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/localday.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	seed := func(ts string) {
		if err := store.InsertUsage(d, &store.UsageEvent{
			TS: ts, Endpoint: "chat", Prompt: 1, CostTotal: 1, Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("2026-11-19T23:00:00Z") // local +120: Nov 20 01:00
	seed("2026-11-20T22:30:00Z") // local +120: Nov 21 00:30
	seed("2026-11-21T00:30:00Z") // local +120: Nov 21 02:30 · local -60: Nov 20 23:30
	seed("2026-11-21T01:00:00Z") // local +120: Nov 21 03:00 · local -60: Nov 21 00:00

	byKey := func(f *store.UsageFilter) map[string]int64 {
		t.Helper()
		_, groups, err := store.SummarizeUsage(d, f, "day")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, g := range groups {
			out[g.Key] = g.Requests
		}
		return out
	}

	// Offset 0: plain UTC days — the 20th holds only the 22:30Z row.
	got := byKey(&store.UsageFilter{AllUsers: true})
	want := map[string]int64{"2026-11-19": 1, "2026-11-20": 1, "2026-11-21": 2}
	if len(got) != len(want) {
		t.Fatalf("offset 0 groups = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("offset 0 day %s = %d, want %d", k, got[k], w)
		}
	}

	// +120 (UTC+2): the 22:30Z and 00:30Z rows join the 01:00Z row on
	// local day Nov 21; the 19th's 23:00Z row becomes local Nov 20.
	got = byKey(&store.UsageFilter{AllUsers: true, TZOffset: 120})
	want = map[string]int64{"2026-11-20": 1, "2026-11-21": 3}
	if len(got) != len(want) {
		t.Fatalf("offset +120 groups = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("offset +120 day %s = %d, want %d", k, got[k], w)
		}
	}

	// -60 (UTC-1): the 00:30Z row falls back to local Nov 20; the exact
	// local-midnight row (01:00Z = 00:00 local) belongs to Nov 21.
	got = byKey(&store.UsageFilter{AllUsers: true, TZOffset: -60})
	want = map[string]int64{"2026-11-19": 1, "2026-11-20": 2, "2026-11-21": 1}
	if len(got) != len(want) {
		t.Fatalf("offset -60 groups = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("offset -60 day %s = %d, want %d", k, got[k], w)
		}
	}

	// From/To bounds follow the same clock: local day Nov 20 at +120 is
	// [Nov 19 22:00Z, Nov 20 22:00Z) — it holds the 23:00Z row but NOT
	// the 22:30Z row, which already belongs to local Nov 21. The same
	// filter at offset 0 holds exactly the opposite row.
	got = byKey(&store.UsageFilter{AllUsers: true, From: "2026-11-20", To: "2026-11-20", TZOffset: 120})
	want = map[string]int64{"2026-11-20": 1}
	if len(got) != 1 || got["2026-11-20"] != 1 {
		t.Errorf("bounded +120 groups = %v, want %v", got, want)
	}
	got = byKey(&store.UsageFilter{AllUsers: true, From: "2026-11-20", To: "2026-11-20"})
	if len(got) != 1 || got["2026-11-20"] != 1 {
		t.Errorf("bounded offset 0 groups = %v, want one Nov 20 row", got)
	}
}
