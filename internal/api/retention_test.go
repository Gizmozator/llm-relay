// API-level tests for the usage-retention setting (plan
// 2026-11-25-usage-retention-v1): the settings pair carries retention_days,
// validates it, and applies a lowered window immediately (bounded purge).
package api_test

import (
	"net/http"
	"testing"
	"time"

	"llm-relay/internal/store"
)

type settingsJSON struct {
	Currency      string `json:"currency"`
	RetentionDays int    `json:"retention_days"`
}

func TestSettingsRetentionRoundTrip(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)

	var got settingsJSON
	if code := getJSON(t, e, "/api/admin/settings", tok, &got); code != 200 {
		t.Fatalf("GET settings = %d", code)
	}
	if got.Currency != "EUR" || got.RetentionDays != 0 {
		t.Fatalf("default settings = %+v; want EUR/0", got)
	}

	// PUT with retention: both values echo back and stick.
	if resp := e.do(t, "PUT", "/api/admin/settings", `{"currency":"usd","retention_days":90}`, tok); resp.StatusCode != 200 {
		t.Fatalf("PUT settings = %d", resp.StatusCode)
	}
	if code := getJSON(t, e, "/api/admin/settings", tok, &got); code != 200 {
		t.Fatalf("GET settings = %d", code)
	}
	if got.Currency != "USD" || got.RetentionDays != 90 {
		t.Fatalf("settings = %+v; want USD/90", got)
	}

	// PUT without retention_days leaves the window alone (old clients).
	if resp := e.do(t, "PUT", "/api/admin/settings", `{"currency":"eur"}`, tok); resp.StatusCode != 200 {
		t.Fatalf("PUT currency-only = %d", resp.StatusCode)
	}
	if code := getJSON(t, e, "/api/admin/settings", tok, &got); code != 200 {
		t.Fatalf("GET settings = %d", code)
	}
	if got.RetentionDays != 90 {
		t.Fatalf("retention = %d; currency-only PUT must not change it", got.RetentionDays)
	}
}

func TestSettingsRetentionValidation(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)

	for _, body := range []string{
		`{"currency":"EUR","retention_days":10}`,   // below the month floor
		`{"currency":"EUR","retention_days":-1}`,   // negative
		`{"currency":"EUR","retention_days":4000}`, // above the cap
	} {
		if resp := e.do(t, "PUT", "/api/admin/settings", body, tok); resp.StatusCode != 400 {
			t.Fatalf("PUT %s = %d; want 400", body, resp.StatusCode)
		}
	}
	// The bounds themselves pass: 0 (forever), 30 and 3650.
	for _, body := range []string{
		`{"currency":"EUR","retention_days":0}`,
		`{"currency":"EUR","retention_days":30}`,
		`{"currency":"EUR","retention_days":3650}`,
	} {
		if resp := e.do(t, "PUT", "/api/admin/settings", body, tok); resp.StatusCode != 200 {
			t.Fatalf("PUT %s = %d; want 200", body, resp.StatusCode)
		}
	}
}

// A PUT that sets a window purges old rows in the same request (bounded):
// the operator lowered retention and expects the history to be gone now.
func TestSettingsPutPurgesOldRows(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	tok := login(t, e)

	old := time.Now().UTC().AddDate(0, 0, -100).Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	for _, ts := range []string{old, old, fresh} {
		ev := &store.UsageEvent{TS: ts, Endpoint: "chat", Prompt: 1}
		if err := store.InsertUsage(e.db, ev); err != nil {
			t.Fatal(err)
		}
	}

	if resp := e.do(t, "PUT", "/api/admin/settings", `{"currency":"EUR","retention_days":30}`, tok); resp.StatusCode != 200 {
		t.Fatalf("PUT settings = %d", resp.StatusCode)
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows left after 30-day retention PUT; want only the fresh row", n)
	}

	// Forever again: nothing more is deleted by later passes.
	if resp := e.do(t, "PUT", "/api/admin/settings", `{"currency":"EUR","retention_days":0}`, tok); resp.StatusCode != 200 {
		t.Fatalf("PUT forever = %d", resp.StatusCode)
	}
	if n, _, err := store.PurgeByRetention(e.db, 0); err != nil || n != 0 {
		t.Fatalf("forever purge = %d, %v; want 0, nil", n, err)
	}
}
