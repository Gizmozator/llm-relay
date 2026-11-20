package db

import (
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
