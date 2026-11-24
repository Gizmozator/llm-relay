package store_test

import (
	"testing"

	"llm-relay/internal/db"
	"llm-relay/internal/store"
)

func TestRemoteTLSSingleRowLifecycle(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/tls.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}

	if _, err := store.RemoteTLSGet(d); err != store.ErrNotFound {
		t.Fatalf("empty table = %v, want ErrNotFound", err)
	}

	put := func(fp, sans string) {
		if err := store.RemoteTLSPut(d, &store.RemoteTLS{
			CertPEM: "CERT-" + fp, KeyPEM: "KEY-" + fp, Fingerprint: fp,
			SANs: sans, NotAfter: "2036-01-01T00:00:00Z", CreatedAt: "2026-01-01T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("sha256:aa", "relay.example.fi")
	got, err := store.RemoteTLSGet(d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != "sha256:aa" || got.SANs != "relay.example.fi" || got.CertPEM != "CERT-sha256:aa" {
		t.Fatalf("round-trip = %+v", got)
	}

	// Regenerate replaces the single row (never a second one).
	put("sha256:bb", "10.0.0.5")
	got, err = store.RemoteTLSGet(d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != "sha256:bb" {
		t.Fatalf("after replace = %+v", got)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM remote_tls`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("row count = %d (err %v), want 1", n, err)
	}

	if err := store.RemoteTLSDelete(d); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RemoteTLSGet(d); err != store.ErrNotFound {
		t.Fatalf("after delete = %v, want ErrNotFound", err)
	}
	// Delete on an empty table is not an error.
	if err := store.RemoteTLSDelete(d); err != nil {
		t.Fatal(err)
	}
}
