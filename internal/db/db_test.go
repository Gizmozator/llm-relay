package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenChmodsSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db mode = %o, want 600", fi.Mode().Perm())
	}
}

// applyUpTo runs the embedded migrations up to (and including) `last`,
// recording them in schema_version exactly like Migrate does — this builds a
// database at a pre-0010 schema so the rebuild path is exercised.
func applyUpTo(t *testing.T, d *sql.DB, last string) {
	t.Helper()
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name() > last {
			continue
		}
		content, err := migrations.ReadFile("migrations/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(string(content)); err != nil {
			t.Fatalf("apply %s: %v", f.Name(), err)
		}
		if _, err := d.Exec(`INSERT INTO schema_version(version, applied_at) VALUES(?, 'test')`, f.Name()); err != nil {
			t.Fatal(err)
		}
	}
}

// 0010 rebuilds models in place (drop name UNIQUE, add visibility +
// UNIQUE(backend_id,name)). Existing rows, ids, the AUTOINCREMENT sequence and
// every column value must survive, and the relaxed uniqueness must hold.
func TestMigrateRebuildsModelsPreservingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	applyUpTo(t, d, "0009_bench_pp.sql")

	if _, err := d.Exec(`INSERT INTO backends(id,name,base_url,created_at)
		VALUES(1,'a','http://a','t'),(2,'b','http://b','t')`); err != nil {
		t.Fatal(err)
	}
	// Old schema: name is globally UNIQUE, so the rows need distinct names.
	if _, err := d.Exec(`INSERT INTO models(id,backend_id,upstream_model,name,enabled,
			price_in,price_out,price_cache_read,price_cache_write,context_length,
			concurrency_limit,bench_tok_per_s,bench_at,busy_threshold_pct,bench_pp_tok_per_s)
		VALUES(7,1,'up-a','alpha',1,11,12,13,14,4096,3,9.5,'2026-01-01T00:00:00Z',80,100.5),
		      (9,2,'up-b','beta',0,0,0,0,0,0,0,0,NULL,0,0),
		      (11,1,'up-c','gamma-old',1,0,0,0,0,0,0,0,NULL,0,0)`); err != nil {
		t.Fatal(err)
	}
	// Delete the top-id row BEFORE migrating: the rebuild must still carry
	// the AUTOINCREMENT high-water mark (11), not just max(surviving id)=9,
	// or new inserts reuse ids 10/11 and orphaned usage_log rows re-attach
	// to the wrong model.
	if _, err := d.Exec(`DELETE FROM models WHERE id=11`); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}

	var id, backendID, enabled, priceIn, ctxLen, thr int64
	var name, vis, benchAt string
	var bench, benchPP float64
	err = d.QueryRow(`SELECT id, backend_id, name, visibility, enabled, price_in, context_length,
			bench_tok_per_s, bench_pp_tok_per_s, COALESCE(bench_at,''), busy_threshold_pct
		FROM models WHERE id=7`).
		Scan(&id, &backendID, &name, &vis, &enabled, &priceIn, &ctxLen, &bench, &benchPP, &benchAt, &thr)
	if err != nil {
		t.Fatal(err)
	}
	if name != "alpha" || vis != "public" || enabled != 1 || priceIn != 11 || ctxLen != 4096 ||
		bench != 9.5 || benchPP != 100.5 || benchAt != "2026-01-01T00:00:00Z" || thr != 80 {
		t.Errorf("rebuilt row 7 wrong: name=%q vis=%q en=%d price=%d ctx=%d tg=%v pp=%v at=%q thr=%d",
			name, vis, enabled, priceIn, ctxLen, bench, benchPP, benchAt, thr)
	}
	var disabled int
	if err := d.QueryRow(`SELECT enabled FROM models WHERE id=9`).Scan(&disabled); err != nil || disabled != 0 {
		t.Errorf("row 9 survived disabled: err=%v enabled=%d", err, disabled)
	}

	// The global name UNIQUE is gone: same name on another backend is fine...
	if _, err := d.Exec(`INSERT INTO models(backend_id,upstream_model,name) VALUES(2,'up-a','alpha')`); err != nil {
		t.Fatalf("same name on second backend rejected: %v", err)
	}
	// ...but same name on the same backend is still rejected.
	if _, err := d.Exec(`INSERT INTO models(backend_id,upstream_model,name) VALUES(1,'up-c','alpha')`); err == nil {
		t.Fatal("same name on same backend accepted, want UNIQUE failure")
	}
	// visibility is constrained to the two known values.
	if _, err := d.Exec(`INSERT INTO models(backend_id,upstream_model,name,visibility) VALUES(2,'up-x','x','bogus')`); err == nil {
		t.Fatal("bogus visibility accepted, want CHECK failure")
	}
	// AUTOINCREMENT continued past the pre-migration high-water mark (11),
	// not just past max(surviving id) (9): the sqlite_sequence row must
	// survive the rebuild even though row 11 was deleted beforehand.
	res, err := d.Exec(`INSERT INTO models(backend_id,upstream_model,name) VALUES(2,'up-d','gamma')`)
	if err != nil {
		t.Fatal(err)
	}
	if newID, _ := res.LastInsertId(); newID <= 11 {
		t.Errorf("new model id = %d, want > 11 (pre-migration high-water mark)", newID)
	}

	// The group tables exist and accept rows.
	if _, err := d.Exec(`INSERT INTO model_groups(name) VALUES('pool')`); err != nil {
		t.Fatalf("model_groups unusable: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO model_group_members(group_id,model_id,weight,position)
		VALUES((SELECT id FROM model_groups WHERE name='pool'),9,2.5,1)`); err != nil {
		t.Fatalf("model_group_members unusable: %v", err)
	}
	// A member row must reference real rows (FKs are live).
	if _, err := d.Exec(`INSERT INTO model_group_members(group_id,model_id) VALUES(1,9999)`); err == nil {
		t.Fatal("member with unknown model_id accepted, want FK failure")
	}

	// Re-running Migrate stays a no-op (0010 is recorded).
	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}
}

// TestWriteTxSerializeUnderConcurrentCommits pins the _txlock=immediate DSN
// setting: guard-read-then-write transactions must never leak
// SQLITE_BUSY_SNAPSHOT when another connection commits in between (with the
// default deferred mode the read takes a snapshot and any interleaved commit
// fails the later write immediately — busy_timeout does not wait it out).
// Two goroutines run short read-then-insert transactions with disjoint keys:
// no row ever conflicts, so any error here is lock machinery, not data.
func TestWriteTxSerializeUnderConcurrentCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contend.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}
	const rounds = 25
	errCh := make(chan error, 2)
	for g := 0; g < 2; g++ {
		go func(g int) {
			for i := 0; i < rounds; i++ {
				tx, err := d.Begin()
				if err != nil {
					errCh <- err
					return
				}
				var n int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&n); err != nil {
					tx.Rollback()
					errCh <- err
					return
				}
				if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)`,
					fmt.Sprintf("c%d-%d", g, i), "x"); err != nil {
					tx.Rollback()
					errCh <- err
					return
				}
				if err := tx.Commit(); err != nil {
					tx.Rollback()
					errCh <- err
					return
				}
			}
			errCh <- nil
		}(g)
	}
	for g := 0; g < 2; g++ {
		if err := <-errCh; err != nil {
			t.Fatalf("contended write tx: %v", err)
		}
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM settings WHERE key LIKE 'c%-%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2*rounds {
		t.Errorf("settings rows = %d, want %d", n, 2*rounds)
	}
}
