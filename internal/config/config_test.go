package config

import "testing"

func TestLoadValidatesSecret(t *testing.T) {
	t.Setenv("JWT_SECRET", "short")
	if _, err := Load(); err == nil {
		t.Fatal("short JWT_SECRET must be rejected")
	}
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcde") // 31 chars
	if _, err := Load(); err == nil {
		t.Fatal("31-char JWT_SECRET must be rejected (HS256 floor is 32)")
	}
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("PORT", "8080")
	t.Setenv("ADMIN_PORT", "8081")
	if _, err := Load(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestLoadRejectsSharedAddr(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("ADMIN_ADDR", ":9000")
	if _, err := Load(); err == nil {
		t.Fatal("relay and admin on the same address must be rejected")
	}
}
