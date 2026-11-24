// Remote-backend store layer tests (plan 2026-11-24-remote-backend-v1):
// token lifecycle — create shows the raw token once, lookup resolves the
// hash, rotate kills the old token, and non-remote rows refuse rotation.
package store_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"llm-relay/internal/auth"
	"llm-relay/internal/db"
	"llm-relay/internal/store"
)

func TestRemoteBackendLifecycle(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/remote.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	b, token, err := store.CreateRemoteBackend(d, "gpu-box")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "rb-") {
		t.Errorf("token = %q, want rb- prefix", token)
	}
	if want := "remote://" + strconv.FormatInt(b.ID, 10); b.BaseURL != want {
		t.Errorf("base_url = %q, want %q", b.BaseURL, want)
	}
	if id, ok := store.RemoteBackendID(b.BaseURL); !ok || id != b.ID {
		t.Errorf("RemoteBackendID(%q) = %d,%v", b.BaseURL, id, ok)
	}
	if b.HasKey {
		t.Error("remote backend must not carry an upstream api_key")
	}

	// Token lookup resolves the row; a wrong hash does not.
	got, err := store.BackendByRemoteTokenHash(d, auth.HashKey(token))
	if err != nil || got.ID != b.ID {
		t.Fatalf("token lookup: %v, %v", got, err)
	}
	if _, err := store.BackendByRemoteTokenHash(d, auth.HashKey("rb-nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("wrong token: %v, want ErrNotFound", err)
	}
	// A disabled backend still resolves (the middleware answers 403
	// distinctly from an unknown token).
	off := false
	if _, err := store.UpdateBackend(d, b.ID, nil, nil, nil, &off, nil); err != nil {
		t.Fatal(err)
	}
	got, err = store.BackendByRemoteTokenHash(d, auth.HashKey(token))
	if err != nil || got.Enabled {
		t.Errorf("disabled lookup: %+v, %v", got, err)
	}
	on := true
	if _, err := store.UpdateBackend(d, b.ID, nil, nil, nil, &on, nil); err != nil {
		t.Fatal(err)
	}

	// Rotate: the new token resolves, the old one is dead.
	newToken, err := store.RotateRemoteToken(d, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newToken == token {
		t.Error("rotate returned the same token")
	}
	if _, err := store.BackendByRemoteTokenHash(d, auth.HashKey(token)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old token after rotate: %v, want ErrNotFound", err)
	}
	if _, err := store.BackendByRemoteTokenHash(d, auth.HashKey(newToken)); err != nil {
		t.Errorf("new token lookup: %v", err)
	}

	// The live-channel revalidation sweep sees the same lifecycle: old
	// hash dead, new hash live, disabled row dead.
	if store.RemoteTokenLive(d, b.ID, auth.HashKey(token)) {
		t.Error("old hash must not revalidate after rotate")
	}
	if !store.RemoteTokenLive(d, b.ID, auth.HashKey(newToken)) {
		t.Error("new hash must revalidate")
	}
	if _, err := store.UpdateBackend(d, b.ID, nil, nil, nil, &off, nil); err != nil {
		t.Fatal(err)
	}
	if store.RemoteTokenLive(d, b.ID, auth.HashKey(newToken)) {
		t.Error("disabled backend must not revalidate")
	}
	if _, err := store.UpdateBackend(d, b.ID, nil, nil, nil, &on, nil); err != nil {
		t.Fatal(err)
	}

	// Non-remote backends refuse rotation.
	plain, err := store.CreateBackend(d, "http-box", "http://127.0.0.1:9", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RotateRemoteToken(d, plain.ID); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("rotate on http backend: %v, want ErrInvalid", err)
	}

	// Name uniqueness matches plain backends (one shared namespace).
	if _, _, err := store.CreateRemoteBackend(d, "http-box"); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate name: %v, want ErrExists", err)
	}
}
